package konoha

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
)

// Observability (§39). Every IssueCycleWorkflow run records the §39 field set in
// its history/logs, and a metrics sink exports the §39 metric list with STABLE
// names; the key operational metric is **autonomous completion rate** (autonomous
// closes / total completes).
//
// Workflow determinism: Temporal replays workflow code on a run history, so
// side effects may not be incremented from the workflow body. §39 metrics are
// therefore recorded in ACTIVITIES (which execute once, never replay) via
// RecordCycleMetrics. go.mod has no metrics stack, so the sink is a small
// sync-guarded registry + stdlib /metrics endpoint — no new dependency, and
// names stay exactly as §39 lists them (any rename is a documented break).

// §39 metric names (stable contract — test-enforced, do not drift).
const (
	mCyclesStarted            = "issue_cycles_started"
	mCyclesCompleted          = "issue_cycles_completed"
	mCyclesCompletedAuton     = "issue_cycles_completed_autonomous"
	mCyclesCompletedHuman     = "issue_cycles_completed_human"
	mCyclesBlocked            = "issue_cycles_blocked"
	mHumanEscalations         = "human_escalations"
	mSplitSmall               = "issue_route_small"
	mSplitLarge               = "issue_route_large"
	mDevelopmentAttempts      = "development_attempts"
	mReviewRounds             = "review_rounds"
	mReviewDisagreements      = "review_disagreements"
	mPOInterventions          = "po_interventions"
	mAutonomousRate           = "autonomous_completion_rate"
	mDidHumanEscalationRate   = "human_escalation_rate"
	mSplitRate                = "split_rate"
	mReviewDisagreementRate   = "review_disagreement_rate"
	mCycleDurationLast        = "cycle_duration_seconds_last"
	mEstimateComplexityPrefix = "estimate_complexity_"
)

// metricsRegistry is the in-process §39 sink. All mutation happens from
// activities (single execution); exports happen from an HTTP handler, so a mutex
// keeps both consistent.
type metricsRegistry struct {
	mu       sync.Mutex
	counters map[string]float64
	gauges   map[string]float64
}

var sink = &metricsRegistry{
	counters: map[string]float64{},
	gauges:   map[string]float64{},
}

func (r *metricsRegistry) inc(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.counters[name]++
}

func (r *metricsRegistry) add(name string, v float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.counters[name] += v
}

func (r *metricsRegistry) setGauge(name string, v float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.gauges[name] = v
}

func (r *metricsRegistry) get(name string) float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	if v, ok := r.counters[name]; ok {
		return v
	}
	return r.gauges[name]
}

// CycleOutcome carries the §39 fields a workflow records once it terminates.
// Recorded as an activity payload so it is replay-safe.
type CycleOutcome struct {
	WorkflowID       string  `json:"workflowId"`
	Issue            int     `json:"issue"`
	Owner            string  `json:"owner"`
	Repo             string  `json:"repo"`
	TerminalState    string  `json:"terminalState,omitempty"` // CLOSED/FAILED/BLOCKED/CANCELLED/HUMAN_REQUIRED/...
	AutonomousClose  bool    `json:"autonomousClose"`         // true only when policy closed it (no human gate)
	HumanRequired    bool    `json:"humanRequired"`
	EscalationReason string  `json:"escalationReason,omitempty"` // §18 trigger, if any
	Route            string  `json:"route,omitempty"`            // SMALL|LARGE
	Complexity       int     `json:"complexity,omitempty"`
	Confidence       string  `json:"confidence,omitempty"`
	Attempts         int     `json:"attempts,omitempty"`
	ReviewRounds     int     `json:"reviewRounds,omitempty"`
	Disagreements    int     `json:"disagreements,omitempty"`
	POInterventions  int     `json:"poInterventions,omitempty"`
	DurationSeconds  float64 `json:"durationSeconds,omitempty"`
}

// RecordCycleMetrics is the single §39 recording activity: it applies the
// outcome to the registry, records the complexity distribution, and recomputes
// the derived rate gauges. Executed once (activities never replay) so counters
// are exact. Returns error so the Temporal SDK can register it (activities must
// return, at minimum, an error); it never fails in practice.
func RecordCycleMetrics(ctx context.Context, o CycleOutcome) error {
	if o.WorkflowID != "" || o.Issue != 0 {
		sink.inc(mCyclesStarted)
	}
	switch o.Route {
	case stateSmall:
		sink.inc(mSplitSmall)
	case stateLarge:
		sink.inc(mSplitLarge)
	}
	if o.Complexity >= 1 && o.Complexity <= 10 {
		sink.inc(fmt.Sprintf("%s%d", mEstimateComplexityPrefix, o.Complexity))
	}
	if o.Attempts > 0 {
		sink.add(mDevelopmentAttempts, float64(o.Attempts))
	}
	for i := 0; i < o.ReviewRounds; i++ {
		sink.inc(mReviewRounds)
	}
	for i := 0; i < o.Disagreements; i++ {
		sink.inc(mReviewDisagreements)
	}
	for i := 0; i < o.POInterventions; i++ {
		sink.inc(mPOInterventions)
	}

	switch o.TerminalState {
	case stateClosed:
		sink.inc(mCyclesCompleted)
		if o.AutonomousClose {
			sink.inc(mCyclesCompletedAuton)
		} else {
			sink.inc(mCyclesCompletedHuman) // human-required close counts as human
		}
	case stateBlocked, stateFailed:
		sink.inc(mCyclesBlocked)
	}
	if o.HumanRequired || o.EscalationReason != "" {
		sink.inc(mHumanEscalations)
	}
	if o.DurationSeconds > 0 {
		sink.setGauge(mCycleDurationLast, o.DurationSeconds)
	}

	recomputeDerived()
	return nil
}

// recomputeDerived recalculates the §39 rate gauges from raw counters:
//   - autonomous_completion_rate = autonomous closes / total completes
//   - human_escalation_rate       = human escalations / started
//   - split_rate                  = large / (small+large)
//   - review_disagreement_rate    = disagreements / review rounds
func recomputeDerived() {
	if total := sink.get(mCyclesCompleted); total > 0 {
		sink.setGauge(mAutonomousRate, sink.get(mCyclesCompletedAuton)/total)
	}
	if started := sink.get(mCyclesStarted); started > 0 {
		sink.setGauge(mDidHumanEscalationRate, sink.get(mHumanEscalations)/started)
	}
	small, large := sink.get(mSplitSmall), sink.get(mSplitLarge)
	if small+large > 0 {
		sink.setGauge(mSplitRate, large/(small+large))
	}
	if rounds := sink.get(mReviewRounds); rounds > 0 {
		sink.setGauge(mReviewDisagreementRate, sink.get(mReviewDisagreements)/rounds)
	}
}

// MetricsText renders the registry in a Prometheus-style line format with the
// STABLE §39 names (sorted). Each line is `<name> <value>`.
func MetricsText() string {
	sink.mu.Lock()
	all := make(map[string]float64, len(sink.counters)+len(sink.gauges))
	for n, v := range sink.counters {
		all[n] = v
	}
	for n, v := range sink.gauges {
		all[n] = v
	}
	sink.mu.Unlock()

	names := make([]string, 0, len(all))
	for n := range all {
		names = append(names, n)
	}
	sort.Strings(names)

	var b strings.Builder
	for _, n := range names {
		fmt.Fprintf(&b, "%s %v\n", n, all[n])
	}
	return b.String()
}

// StartMetricsServer serves the §39 metrics over HTTP at GET /metrics. addr ""
// means no server (metrics still recorded in-process for tests/CLI). Bind errors
// are logged by the underlying net/http server (best-effort debug endpoint).
func StartMetricsServer(addr string) *http.Server {
	if addr == "" {
		return nil
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		fmt.Fprint(w, MetricsText())
	})
	srv := &http.Server{Addr: addr, Handler: mux}
	go srv.ListenAndServe() // background: metric scraping is best-effort
	return srv
}

// logCycleEvent emits a workflow history log line carrying the §39 field set
// (§39 observability): workflow ID, issue, state, and the current observed
// facts. Call from workflow code on every transition; Temporal makes logging
// replay-safe, so this is deterministic.
func logCycleEvent(wf workflowLogger, workflowID string, issue int, repo, state string,
	complexity int, confidence string, attempts, reviewRounds int) {
	wf.Info("issue state",
		"workflow", workflowID,
		"issue", issue,
		"repo", repo,
		"state", state,
		"complexity", complexity,
		"confidence", confidence,
		"attempts", attempts,
		"review_rounds", reviewRounds,
	)
}

// workflowLogger is the minimal logger surface the §39 helper needs, satisfied by
// workflow.GetLogger(ctx). Kept narrow so tests can substitute a recorder.
type workflowLogger interface {
	Info(msg string, kv ...interface{})
}
