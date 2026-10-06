package konoha

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// #110 AC: the SSE stream delivers condition transitions (and the terminal end)
// for a simulated cycle; ids are monotonic, the wire is text/event-stream.
func TestStreamConditionTransitions(t *testing.T) {
	withFastPoll(t)
	fake := &fakeStreamSource{snaps: []streamSnapshot{
		{State: stateAnalyzing, Conditions: []Condition{{Type: conditionAgent, Status: "step1"}}},
		{State: stateDeveloping, Conditions: []Condition{{Type: conditionAgent, Status: "step1"}, {Type: conditionAgent, Status: "step2"}}},
		{State: stateClosed, Conditions: []Condition{{Type: conditionAgent, Status: "step1"}, {Type: conditionAgent, Status: "step2"}, {Type: conditionTerminal, Status: "step3"}}, Done: true},
	}}
	_, hs := newStreamConsole(t, fake.fn)

	res := getURL(t, hs.URL+"/api/v1/cycles/repo-issue-1/stream", "secret-token", "")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("stream status = %d, want 200", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}
	defer res.Body.Close()
	evs := readStream(res.Body)

	assertCondition(t, waitType(t, evs, streamEventCondition), "step1")
	fake.advance()
	assertCondition(t, waitType(t, evs, streamEventCondition), "step2")
	fake.advance()
	assertCondition(t, waitType(t, evs, streamEventCondition), "step3")
	if end := waitType(t, evs, streamEventEnd); end.ID == 0 {
		t.Fatal("end event must carry an id")
	}
}

// #110 AC: watch mode tails a running environment's command/output activity,
// bounded by streamOutputCap, and ends with the cycle.
func TestStreamWatchTailsOutput(t *testing.T) {
	withFastPoll(t)
	fake := &fakeStreamSource{snaps: []streamSnapshot{
		{State: stateDeveloping, Output: "hello", OutputSize: 5},
		{State: stateClosed, Output: "hello world", OutputSize: 11, Done: true},
	}}
	_, hs := newStreamConsole(t, fake.fn)

	res := getURL(t, hs.URL+"/api/v1/cycles/repo-issue-1/stream?watch=1", "secret-token", "")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("stream status = %d, want 200", res.StatusCode)
	}
	defer res.Body.Close()
	evs := readStream(res.Body)
	fake.advance()
	var out strings.Builder
	deadline := time.After(3 * time.Second)
	for {
		select {
		case ev, ok := <-evs:
			if !ok {
				if got := out.String(); got != "hello world" {
					t.Fatalf("output tail = %q, want %q", got, "hello world")
				}
				return
			}
			if ev.Type == streamEventOutput {
				var d struct{ Chunk string }
				if err := json.Unmarshal([]byte(ev.Data), &d); err != nil {
					t.Fatal(err)
				}
				if len(d.Chunk) > streamOutputCap {
					t.Fatalf("output chunk %d bytes exceeds cap", len(d.Chunk))
				}
				out.WriteString(d.Chunk)
			}
		case <-deadline:
			t.Fatalf("stream did not end; output so far %q", out.String())
		}
	}
}

// #110 AC: a watch-off subscriber never receives output events.
func TestStreamWithoutWatchOmitsOutput(t *testing.T) {
	withFastPoll(t)
	fake := &fakeStreamSource{snaps: []streamSnapshot{
		{State: stateDeveloping, Output: "secret output", OutputSize: 13},
		{State: stateClosed, Output: "secret output", OutputSize: 13, Done: true},
	}}
	_, hs := newStreamConsole(t, fake.fn)
	res := getURL(t, hs.URL+"/api/v1/cycles/repo-issue-1/stream", "secret-token", "")
	defer res.Body.Close()
	evs := readStream(res.Body)
	fake.advance()
	for ev := range evs {
		if ev.Type == streamEventOutput {
			t.Fatal("output event leaked to a condition-only subscriber")
		}
	}
}

// #110 AC: a reconnect (Last-Event-ID) replays only events after the cursor —
// never a duplicate.
func TestStreamResumeNoDuplicates(t *testing.T) {
	h := newStreamHub()
	c := &streamCycle{id: "c", subs: map[*streamSub]struct{}{}}
	h.cycles["c"] = c
	h.observe(c, streamSnapshot{State: "A", Conditions: []Condition{{Status: "a"}}})
	h.observe(c, streamSnapshot{State: "A", Conditions: []Condition{{Status: "a"}, {Status: "b"}}})

	done := func(context.Context, string, bool) (streamSnapshot, bool) {
		return streamSnapshot{Done: true}, true
	}
	sub, replay, ok := h.subscribe(context.Background(), "c", done, 2, false)
	if !ok {
		t.Fatal("subscribe = false, want true")
	}
	defer h.unsubscribe("c", sub)
	if len(replay) == 0 {
		t.Fatal("resume after id 2 replayed nothing")
	}
	seen := map[int64]bool{}
	for _, ev := range replay {
		if ev.ID <= 2 {
			t.Fatalf("replayed id %d at/behind the cursor", ev.ID)
		}
		if seen[ev.ID] {
			t.Fatalf("duplicate replayed id %d", ev.ID)
		}
		seen[ev.ID] = true
	}
}

// #110 AC: memory is bounded for a large output stream (the buffer never holds
// more than the cap, however much output arrives).
func TestStreamOutputBoundedMemory(t *testing.T) {
	h := newStreamHub()
	c := &streamCycle{id: "c", subs: map[*streamSub]struct{}{}, wantOut: true}
	h.cycles["c"] = c
	chunk := strings.Repeat("x", 4096)
	for i := 1; i <= 2000; i++ {
		h.observe(c, streamSnapshot{Output: chunk, OutputSize: int64(i * len(chunk))})
	}
	if c.outBytes > streamOutputCap+len(chunk) {
		t.Fatalf("buffered output %d bytes exceeds cap %d", c.outBytes, streamOutputCap)
	}
	if len(c.events) > streamBufferEvents+1 {
		t.Fatalf("buffered events %d exceeds cap %d", len(c.events), streamBufferEvents)
	}
	if c.nextID != 2000 {
		t.Fatalf("emitted %d output events, want 2000", c.nextID)
	}
}

// #110 AC: the endpoint requires the same bearer auth as the rest of the API.
func TestStreamBearerAuth(t *testing.T) {
	_, hs := newStreamConsole(t, func(context.Context, string, bool) (streamSnapshot, bool) {
		return streamSnapshot{Done: true}, true
	})
	res := getURL(t, hs.URL+"/api/v1/cycles/repo-issue-1/stream", "", "")
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated stream = %d, want 401", res.StatusCode)
	}
	res.Body.Close()
}

// #110: an unknown cycle id is a 404, a malformed id a 400.
func TestStreamUnknownAndMalformed(t *testing.T) {
	_, hs := newStreamConsole(t, func(context.Context, string, bool) (streamSnapshot, bool) {
		return streamSnapshot{}, false
	})
	res := getURL(t, hs.URL+"/api/v1/cycles/repo-issue-1/stream", "secret-token", "")
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown cycle stream = %d, want 404", res.StatusCode)
	}
	res.Body.Close()
	res = getURL(t, hs.URL+"/api/v1/cycles/bad%20id/stream", "secret-token", "")
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed cycle stream = %d, want 400", res.StatusCode)
	}
	res.Body.Close()
}

// #110: the default source streams a finished cycle's recorded conditions from
// the evidence store and closes.
func TestStreamFinishedCycleFromEvidence(t *testing.T) {
	withFastPoll(t)
	out := CycleOutcome{
		WorkflowID:    "repo-issue-1",
		TerminalState: stateClosed,
		Conditions:    []Condition{{Type: conditionTerminal, Status: stateClosed}},
	}
	_, _, hs := newTestConsole(t, out)
	res := getURL(t, hs.URL+"/api/v1/cycles/repo-issue-1/stream", "secret-token", "")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	defer res.Body.Close()
	evs := readStream(res.Body)
	assertCondition(t, waitType(t, evs, streamEventCondition), stateClosed)
	waitType(t, evs, streamEventEnd)
}

// --- helpers ---

type fakeStreamSource struct {
	mu    sync.Mutex
	snaps []streamSnapshot
	step  int
}

func (f *fakeStreamSource) fn(_ context.Context, _ string, want bool) (streamSnapshot, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.snaps) == 0 {
		return streamSnapshot{}, false
	}
	i := f.step
	if i >= len(f.snaps) {
		i = len(f.snaps) - 1
	}
	s := f.snaps[i]
	if !want {
		s.Output = ""
	}
	return s, true
}

func (f *fakeStreamSource) advance() {
	f.mu.Lock()
	if f.step < len(f.snaps)-1 {
		f.step++
	}
	f.mu.Unlock()
}

func newStreamConsole(t *testing.T, src consoleStreamSource) (*ConsoleServer, *httptest.Server) {
	t.Helper()
	store, err := NewEvidenceStore(EvidenceConfig{Dir: t.TempDir()}, &defaultRedactor)
	if err != nil {
		t.Fatalf("evidence store: %v", err)
	}
	srv, err := newConsoleServer(ConsoleConfig{Addr: "127.0.0.1:0", Token: "secret-token"},
		&consoleData{store: store, stream: src}, "test-1")
	if err != nil {
		t.Fatalf("new console: %v", err)
	}
	hs := httptest.NewServer(srv)
	t.Cleanup(hs.Close)
	return srv, hs
}

func withFastPoll(t *testing.T) {
	t.Helper()
	old := streamPollInterval
	streamPollInterval = time.Millisecond
	t.Cleanup(func() { streamPollInterval = old })
}

type sseEvent struct {
	ID   int64
	Type string
	Data string
}

func readStream(body io.Reader) <-chan sseEvent {
	ch := make(chan sseEvent, 256)
	go func() {
		defer close(ch)
		sc := bufio.NewScanner(body)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		var cur sseEvent
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "":
				if cur.Type != "" {
					ch <- cur
					cur = sseEvent{}
				}
			case strings.HasPrefix(line, "id: "):
				cur.ID, _ = strconv.ParseInt(strings.TrimPrefix(line, "id: "), 10, 64)
			case strings.HasPrefix(line, "event: "):
				cur.Type = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				d := strings.TrimPrefix(line, "data: ")
				if cur.Data != "" {
					cur.Data += "\n"
				}
				cur.Data += d
			}
		}
	}()
	return ch
}

func waitType(t *testing.T, ch <-chan sseEvent, typ string) sseEvent {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatalf("stream closed before a %q event", typ)
			}
			if ev.Type == typ {
				return ev
			}
		case <-deadline:
			t.Fatalf("timed out waiting for a %q event", typ)
		}
	}
}

func assertCondition(t *testing.T, ev sseEvent, want string) {
	t.Helper()
	var c Condition
	if err := json.Unmarshal([]byte(ev.Data), &c); err != nil {
		t.Fatalf("condition data %q: %v", ev.Data, err)
	}
	if c.Status != want {
		t.Fatalf("condition status = %q, want %q", c.Status, want)
	}
}
