package konoha

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// #110 live progress streaming (SSE). A read-only server-sent-events endpoint
// that surfaces a cycle's condition transitions (and, in watch mode, a bounded
// tail of its agent command/output activity) live, so the console can render
// progress between §6 transitions without polling. It is NOT a second source of
// truth: the events are projections of the durable status/evidence records, and
// the §6 workflow state stays authoritative (§44 #20).
//
// Wire model: each event carries a per-cycle, monotonically increasing integer
// `id`; the browser resumes with `Last-Event-ID` and the hub replays the buffered
// events after that cursor, so a reconnect never duplicates an event. Buffered
// output is capped (streamOutputCap) so an unbounded agent log can never grow
// console memory.

// SSE event types emitted by the stream.
const (
	streamEventState     = "state"     // a §6 state transition
	streamEventCondition = "condition" // a new ordered condition (#84)
	streamEventOutput    = "output"    // a bounded agent command/output tail chunk
	streamEventEnd       = "end"       // the cycle is terminal; the stream closes
	streamEventReset     = "reset"     // the client lagged; reconnect and resume
)

// Streaming bounds and cadence. The buffer is bounded by both an event count and
// a byte cap on output, so memory is O(cap) regardless of stream length.
const (
	streamHeartbeat    = 15 * time.Second
	streamBufferEvents = 512
	streamOutputCap    = 64 * 1024
	streamSubBuffer    = 64
	streamIdleTicks    = 12 // producer exits after this many empty-subs polls
)

// streamPollInterval is a var (not const) so tests can drive the producer fast.
var streamPollInterval = 500 * time.Millisecond

// StreamEvent is one SSE event. ID is monotonic per cycle; Data is the JSON
// `data:` payload.
type StreamEvent struct {
	ID   int64           `json:"id"`
	Type string          `json:"type"`
	Data json.RawMessage `json:"data,omitempty"`
}

// streamSnapshot is the bounded observable state of one cycle at a point in
// time. Output is a bounded TAIL of the full stream (at most streamOutputCap
// bytes); OutputSize is the total bytes ever produced, so the hub can emit only
// the delta without buffering the whole log.
type streamSnapshot struct {
	State      string
	Conditions []Condition
	Output     string
	OutputSize int64
	Done       bool
}

// consoleStreamSource resolves one cycle's bounded snapshot. ok=false means the
// cycle is unknown (→ 404). wantOutput lets the console skip the output read for
// a condition-only subscriber. This is the seam: production reads the durable
// status/evidence records; tests inject a simulated environment. It is invoked
// only from the hub's producer goroutine, never from workflow code.
type consoleStreamSource func(ctx context.Context, id string, wantOutput bool) (streamSnapshot, bool)

// streamHub is the process-local fan-out for SSE subscriptions. It is created
// per console server; a standalone console without the worker simply never sees
// live output beyond what the evidence store already holds.
type streamHub struct {
	mu     sync.Mutex
	cycles map[string]*streamCycle
}

func newStreamHub() *streamHub { return &streamHub{cycles: map[string]*streamCycle{}} }

// streamCycle is one cycle's replay buffer + subscriber set. run is true while a
// producer goroutine is active; the buffer is bounded by streamOutputCap bytes
// of output (conditions/state/end are small and workflow-bounded).
type streamCycle struct {
	id       string
	nextID   int64
	events   []StreamEvent
	outBytes int
	subs     map[*streamSub]struct{}
	condN    int
	state    string
	outEmit  int64 // total output bytes already emitted as events
	wantOut  bool
	done     bool
	run      bool
	idle     int
}

// streamSub is one open SSE connection. lagged marks a subscriber that could not
// keep up; its handler emits a reset and lets the client reconnect.
type streamSub struct {
	ch     chan StreamEvent
	watch  bool
	lagged atomic.Bool
}

// subscribe registers a subscriber and replays buffered events after lastID
// (0 = all). ok=false = unknown cycle. It starts the producer on first use.
func (h *streamHub) subscribe(ctx context.Context, id string, src consoleStreamSource, lastID int64, watch bool) (sub *streamSub, replay []StreamEvent, ok bool) {
	if _, exists := src(ctx, id, false); !exists {
		return nil, nil, false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	c := h.cycles[id]
	if c == nil {
		c = &streamCycle{id: id, subs: map[*streamSub]struct{}{}}
		h.cycles[id] = c
	}
	sub = &streamSub{ch: make(chan StreamEvent, streamSubBuffer), watch: watch}
	c.subs[sub] = struct{}{}
	c.idle = 0
	if watch {
		c.wantOut = true
	}
	for _, ev := range c.events {
		if ev.ID > lastID {
			replay = append(replay, ev)
		}
	}
	if !c.run && !c.done {
		c.run = true
		go h.produce(c, src)
	}
	return sub, replay, true
}

// unsubscribe removes a subscriber; with no subscribers left on a finished cycle
// the entry (and its buffer) is dropped.
func (h *streamHub) unsubscribe(id string, sub *streamSub) {
	h.mu.Lock()
	defer h.mu.Unlock()
	c := h.cycles[id]
	if c == nil {
		return
	}
	delete(c.subs, sub)
	if sub.watch && !c.anyWatchLocked() {
		c.wantOut = false
	}
	if len(c.subs) == 0 && c.done {
		delete(h.cycles, id)
	}
}

func (c *streamCycle) anyWatchLocked() bool {
	for s := range c.subs {
		if s.watch {
			return true
		}
	}
	return false
}

// produce polls the source and publishes deltas until the cycle is terminal or
// it has been idle (no subscribers) long enough to reclaim. The goroutine is
// per actively-streamed cycle; a running cycle with no watchers is reclaimed
// after streamIdleTicks polls.
func (h *streamHub) produce(c *streamCycle, src consoleStreamSource) {
	tick := time.NewTicker(streamPollInterval)
	defer tick.Stop()
	for {
		h.mu.Lock()
		if c.done {
			c.run = false
			if len(c.subs) == 0 {
				delete(h.cycles, c.id)
			}
			h.mu.Unlock()
			return
		}
		if len(c.subs) == 0 {
			c.idle++
			if c.idle >= streamIdleTicks {
				c.run = false
				delete(h.cycles, c.id)
				h.mu.Unlock()
				return
			}
		} else {
			c.idle = 0
		}
		want := c.wantOut
		h.mu.Unlock()

		snap, ok := src(context.Background(), c.id, want)
		if !ok {
			h.finish(c, "")
			return
		}
		h.observe(c, snap)
		<-tick.C
	}
}

// observe publishes the deltas between the last snapshot and snap: a state
// transition, each appended condition, new output bytes (watch mode), and the
// terminal end event.
func (h *streamHub) observe(c *streamCycle, snap streamSnapshot) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if snap.State != c.state {
		c.state = snap.State
		h.emitLocked(c, streamEventState, map[string]string{"state": snap.State, "step": statusStep(snap.State)})
	}
	for ; c.condN < len(snap.Conditions); c.condN++ {
		h.emitLocked(c, streamEventCondition, snap.Conditions[c.condN])
	}
	if c.wantOut && snap.Output != "" {
		h.emitOutputLocked(c, snap)
	}
	if snap.Done && !c.done {
		c.done = true
		h.emitLocked(c, streamEventEnd, map[string]any{"state": snap.State, "done": true})
	}
}

// finish publishes the terminal end event for a cycle the source can no longer
// resolve (it was dropped mid-stream).
func (h *streamHub) finish(c *streamCycle, state string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if c.done {
		return
	}
	c.done = true
	h.emitLocked(c, streamEventEnd, map[string]any{"state": state, "done": true})
}

// emitOutputLocked emits the output bytes produced since the last emission. The
// source returns only a bounded tail plus the total size, so a huge log is
// streamed incrementally without ever being buffered whole.
func (h *streamHub) emitOutputLocked(c *streamCycle, snap streamSnapshot) {
	pending := snap.OutputSize - c.outEmit
	if pending <= 0 {
		return
	}
	if pending > int64(len(snap.Output)) {
		pending = int64(len(snap.Output)) // older bytes were trimmed by the source bound
	}
	chunk := snap.Output[int64(len(snap.Output))-pending:]
	if chunk != "" {
		h.emitLocked(c, streamEventOutput, map[string]string{"chunk": chunk})
	}
	c.outEmit = snap.OutputSize
}

// emitLocked assigns the next id, buffers the event (trimming oldest output past
// the cap), and fans it out non-blockingly; a full subscriber is marked lagged.
func (h *streamHub) emitLocked(c *streamCycle, typ string, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	c.nextID++
	ev := StreamEvent{ID: c.nextID, Type: typ, Data: data}
	c.events = append(c.events, ev)
	if typ == streamEventOutput {
		c.outBytes += len(data)
	}
	for c.outBytes > streamOutputCap || len(c.events) > streamBufferEvents {
		idx := -1
		for i := range c.events {
			if c.events[i].Type == streamEventOutput {
				idx = i
				break
			}
		}
		if idx < 0 {
			c.outBytes = 0
			break
		}
		c.outBytes -= len(c.events[idx].Data)
		c.events = append(c.events[:idx], c.events[idx+1:]...)
	}
	for sub := range c.subs {
		select {
		case sub.ch <- ev:
		default:
			sub.lagged.Store(true)
		}
	}
}

// handleCycleStream is the SSE endpoint (GET /api/v1/cycles/{id}/stream).
// Condition/state events are always streamed; ?watch=1 adds the bounded
// agent command/output tail. Last-Event-ID (or ?lastEventId=) resumes without
// duplicates. Malformed id → 400, unknown → 404, streaming disabled → 501.
func (s *ConsoleServer) handleCycleStream(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validConsoleCycleID(id) {
		writeConsoleError(w, http.StatusBadRequest, "malformed cycle id")
		return
	}
	if s.data == nil || s.stream == nil || s.hub == nil {
		writeConsoleError(w, http.StatusNotImplemented, "streaming disabled")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeConsoleError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	watch := isTruthy(r.URL.Query().Get("watch"))
	lastID := parseLastEventID(r)
	sub, replay, ok := s.hub.subscribe(r.Context(), id, s.stream, lastID, watch)
	if !ok {
		writeConsoleError(w, http.StatusNotFound, "unknown cycle")
		return
	}
	defer s.hub.unsubscribe(id, sub)

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	write := func(ev StreamEvent) bool {
		if ev.Type == streamEventOutput && !watch {
			return true
		}
		writeStreamEvent(w, ev)
		flusher.Flush()
		return ev.Type != streamEventEnd
	}
	for _, ev := range replay {
		if !write(ev) {
			return
		}
	}
	hb := time.NewTicker(streamHeartbeat)
	defer hb.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case ev := <-sub.ch:
			if !write(ev) {
				return
			}
			if sub.lagged.Load() {
				writeStreamEvent(w, StreamEvent{Type: streamEventReset, Data: json.RawMessage(`{"reason":"lagged"}`)})
				flusher.Flush()
				return
			}
		case <-hb.C:
			if sub.lagged.Load() {
				writeStreamEvent(w, StreamEvent{Type: streamEventReset, Data: json.RawMessage(`{"reason":"lagged"}`)})
				flusher.Flush()
				return
			}
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}

// writeStreamEvent renders one SSE event.
func writeStreamEvent(w http.ResponseWriter, ev StreamEvent) {
	if ev.ID > 0 {
		fmt.Fprintf(w, "id: %d\n", ev.ID)
	}
	fmt.Fprintf(w, "event: %s\n", ev.Type)
	for _, line := range strings.Split(string(ev.Data), "\n") {
		fmt.Fprintf(w, "data: %s\n", line)
	}
	fmt.Fprint(w, "\n")
}

// parseLastEventID reads the SSE resume cursor: the Last-Event-ID header, else
// the lastEventId query param. A malformed/absent cursor is 0 (full replay).
func parseLastEventID(r *http.Request) int64 {
	raw := strings.TrimSpace(r.Header.Get("Last-Event-ID"))
	if raw == "" {
		raw = strings.TrimSpace(r.URL.Query().Get("lastEventId"))
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

func isTruthy(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// streamDoneState reports whether a §6 state is terminal, so the stream closes
// (a PENDING_APPROVAL / HUMAN_REQUIRED wait is terminal for the workflow; a gate
// wait is not).
func streamDoneState(state string) bool {
	switch state {
	case stateClosed, stateFailed, stateCancelled, stateNotWired, stateHumanRequired, stateBlocked:
		return true
	}
	return false
}

// streamSource resolves the console's default stream snapshot from the durable
// evidence store (finished cycles) or the #89 status query (running cycles) plus
// a bounded tail of the recorded agent stdout. ok=false = unknown cycle.
func (d *consoleData) streamSource() consoleStreamSource {
	if d == nil || d.store == nil {
		return nil
	}
	return func(ctx context.Context, id string, wantOutput bool) (streamSnapshot, bool) {
		if o, ok, err := d.store.loadOutcomeOptional(id); err == nil && ok {
			out, size := d.streamOutput(id, wantOutput)
			return streamSnapshot{State: o.TerminalState, Conditions: o.Conditions, Output: out, OutputSize: size, Done: true}, true
		}
		if d.status != nil {
			if st, ok := d.status(ctx, id); ok {
				out, size := d.streamOutput(id, wantOutput)
				return streamSnapshot{State: st.State, Conditions: st.Conditions, Output: out, OutputSize: size,
					Done: streamDoneState(st.State)}, true
			}
		}
		// Known-but-interrupted (no outcome, no live status): terminal with
		// whatever was recorded, never a stream that hangs forever.
		if d.store.cycleExists(id) {
			return streamSnapshot{Done: true}, true
		}
		return streamSnapshot{}, false
	}
}

// streamOutput returns a bounded tail of the recorded agent stdout plus the
// total size. It is best-effort: no artifact → ("", 0).
//
// ponytail: the artifact is read whole (the sink is opaque) then sliced to the
// bound; upgrade to a seek-based tail reader if agent logs grow large, and feed
// a redaction-aware live tee here once a live environment stream lands (#114).
func (d *consoleData) streamOutput(id string, want bool) (string, int64) {
	if !want {
		return "", 0
	}
	full, ok := d.store.readArtifact(id, evidenceStdout)
	if !ok {
		return "", 0
	}
	tail := full
	if len(tail) > streamOutputCap {
		tail = tail[len(tail)-streamOutputCap:]
	}
	return tail, int64(len(full))
}
