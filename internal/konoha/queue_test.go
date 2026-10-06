package konoha

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/client"
)

func queueActionEnabled(item QueueItem, action string) (QueueActionView, bool) {
	for _, a := range item.Actions {
		if a.Action == action {
			return a, true
		}
	}
	return QueueActionView{}, false
}

// #109 AC1: the pure projection lists awaiting-approval / blocked / failed /
// interrupted cycles (and only those), with deterministic ordering.
func TestAttentionQueueProjection(t *testing.T) {
	now := time.Now().UTC()
	cycles := []queueCycle{
		{ID: "r-issue-1", Repo: "r", Issue: 1, State: statePendingApproval, Running: true, At: now},
		{ID: "r-issue-2", Repo: "r", Issue: 2, State: stateBlocked, HasOutcome: true, At: now.Add(-time.Hour)},
		{ID: "r-issue-3", Repo: "r", Issue: 3, State: stateFailed, HasOutcome: true},
		{ID: "r-issue-4", Repo: "r", Issue: 4, State: stateHumanRequired, HasOutcome: true},
		{ID: "r-issue-5", Repo: "r", Issue: 5, Interrupted: true},
		// Not attention:
		{ID: "r-issue-6", Repo: "r", Issue: 6, State: stateClosed, HasOutcome: true},
		{ID: "r-issue-7", Repo: "r", Issue: 7, State: stateDeveloping, Running: true},
		{ID: "r-issue-8", Repo: "r", Issue: 8, State: stateCancelled, HasOutcome: true, Canceled: true},
	}
	items := attentionItems(cycles, false)
	got := map[string]string{}
	for _, it := range items {
		got[it.ID] = it.Reason
	}
	want := map[string]string{
		"r-issue-1": queueReasonAwaitingApproval,
		"r-issue-2": queueReasonBlocked,
		"r-issue-3": queueReasonFailed,
		"r-issue-4": queueReasonHumanRequired,
		"r-issue-5": queueReasonInterrupted,
	}
	if len(items) != len(want) {
		t.Fatalf("items = %d, want %d (%+v)", len(items), len(want), got)
	}
	for id, reason := range want {
		if got[id] != reason {
			t.Fatalf("item %s reason = %q, want %q", id, got[id], reason)
		}
	}
	// Newest-first ordering (r-issue-1 has the newest At).
	if items[0].ID != "r-issue-1" {
		t.Fatalf("first item = %s, want r-issue-1 (newest)", items[0].ID)
	}

	// Actions: retry on terminal states; cancel only while running; interrupted
	// retry requires confirmation; the session verbs are disabled (no provider).
	blocked := items[1]
	if a, _ := queueActionEnabled(blocked, queueActionRetry); !a.Enabled {
		t.Fatal("blocked retry must be enabled")
	}
	if a, _ := queueActionEnabled(blocked, queueActionCancel); a.Enabled {
		t.Fatal("blocked cancel must be disabled (already stopped)")
	}
	byID := map[string]QueueItem{}
	for _, it := range items {
		byID[it.ID] = it
	}
	awaiting := byID["r-issue-1"]
	if a, _ := queueActionEnabled(awaiting, queueActionCancel); !a.Enabled {
		t.Fatal("awaiting-approval cancel must be enabled (running)")
	}
	interrupted := byID["r-issue-5"]
	if !interrupted.RequiresConfirmation {
		t.Fatal("interrupted item must require confirmation")
	}
	for _, verb := range []string{queueActionAttach, queueActionPause, queueActionResume} {
		if a, ok := queueActionEnabled(interrupted, verb); !ok || a.Enabled || a.Reason == "" {
			t.Fatalf("session verb %s = %+v, want disabled with a reason", verb, a)
		}
	}
}

// §109/§36: the action set is closed; anything else fails closed.
func TestValidateQueueAction(t *testing.T) {
	for _, a := range []string{queueActionRetry, queueActionCancel, queueActionAttach, queueActionPause, queueActionResume} {
		if err := ValidateQueueAction(a); err != nil {
			t.Fatalf("ValidateQueueAction(%q) = %v", a, err)
		}
	}
	for _, a := range []string{"", "restart", "RETRY", "delete"} {
		if err := ValidateQueueAction(a); err == nil {
			t.Fatalf("ValidateQueueAction(%q) = nil, want an error", a)
		}
	}
}

// newQueueConsole builds a consoleData whose queue writer is real, with injected
// lifecycle seams. The store carries the given outcome plus an optional lineage.
func newQueueConsole(t *testing.T, o *CycleOutcome, running []CycleSummary, status consoleStatusQuery) (*consoleData, *EvidenceStore) {
	t.Helper()
	store, err := NewEvidenceStore(EvidenceConfig{Dir: t.TempDir()}, &defaultRedactor)
	if err != nil {
		t.Fatal(err)
	}
	if o != nil && o.WorkflowID != "" {
		b, _ := json.Marshal(o)
		if err := store.Write(o.WorkflowID, evidenceOutcome, string(b), false); err != nil {
			t.Fatal(err)
		}
	}
	d := &consoleData{
		store: store,
		repos: map[string]RepoConfig{"r": {Owner: "o", Repo: "r"}},
	}
	d.queue = consoleQueueWriter(d)
	if running != nil {
		d.running = func(context.Context) ([]CycleSummary, bool) { return running, true }
	}
	d.status = status
	return d, store
}

// #109 AC4: a running cycle can be canceled; the cancel is authoritative and
// audited, the action does not auto-replay (no retry/start fires), and a repeat
// is a NOOP (idempotent §24).
func TestQueueCancelStopsWithoutReplay(t *testing.T) {
	o := &CycleOutcome{WorkflowID: "r-issue-7", Owner: "o", Repo: "r", Issue: 7, TerminalState: statePendingApproval}
	d, store := newQueueConsole(t, o, nil, nil)
	// A running cycle is observed via the running lister + status query.
	d.running = func(context.Context) ([]CycleSummary, bool) {
		return []CycleSummary{{ID: "r-issue-7", Repo: "r", Issue: 7}}, true
	}
	d.status = func(context.Context, string) (StatusQuery, bool) {
		return StatusQuery{State: statePendingApproval}, true
	}
	canceled, started := 0, 0
	d.cancelWorkflow = func(context.Context, string) error { canceled++; return nil }
	d.startCycle = func(context.Context, IssueInput) error { started++; return nil }

	res, err := d.submitQueueAction(context.Background(), "r-issue-7", QueueActionRequest{Action: queueActionCancel})
	if err != nil || !res.Accepted || res.Outcome != queueOutcomeApplied {
		t.Fatalf("cancel = %+v err=%v", res, err)
	}
	if canceled != 1 || started != 0 {
		t.Fatalf("canceled=%d started=%d, want 1/0 (no auto-replay)", canceled, started)
	}

	// Once the stop is reflected (no longer running), a repeat is a NOOP — the
	// action is idempotent and never replays.
	d.running = func(context.Context) ([]CycleSummary, bool) { return nil, true }
	res, err = d.submitQueueAction(context.Background(), "r-issue-7", QueueActionRequest{Action: queueActionCancel})
	if err != nil || res.Outcome != queueOutcomeNoop {
		t.Fatalf("stopped cancel = %+v err=%v, want NOOP", res, err)
	}
	// A running cycle cannot be retried until it is stopped first.
	res, err = d.submitQueueAction(context.Background(), "r-issue-7", QueueActionRequest{Action: queueActionRetry})
	if err == nil || res.Outcome != "" {
		t.Fatalf("retry while running = %+v err=%v, want a refusal", res, err)
	}
	if started != 0 {
		t.Fatalf("retry while running must not start a run (started=%d)", started)
	}
	_ = store
}

// #109 AC3: an interrupted cycle cannot be retried without an explicit
// "process stopped" confirmation.
func TestQueueInterruptedRetryRequiresConfirmation(t *testing.T) {
	d, store := newQueueConsole(t, nil, nil, nil) // no outcome, not running → interrupted
	if err := store.Write("r-issue-7", evidenceStdout, "in-flight evidence", false); err != nil {
		t.Fatal(err)
	}
	started := 0
	d.startCycle = func(context.Context, IssueInput) error { started++; return nil }

	_, err := d.submitQueueAction(context.Background(), "r-issue-7", QueueActionRequest{Action: queueActionRetry})
	if err == nil || !strings.Contains(err.Error(), "confirmStopped") {
		t.Fatalf("retry without confirmation = %v, want a confirmStopped error", err)
	}
	if started != 0 {
		t.Fatal("interrupted retry must not start before confirmation")
	}

	res, err := d.submitQueueAction(context.Background(), "r-issue-7",
		QueueActionRequest{Action: queueActionRetry, ConfirmStopped: true})
	if err != nil || res.Outcome != queueOutcomeApplied || started != 1 {
		t.Fatalf("confirmed retry = %+v err=%v started=%d, want APPLIED/1", res, err, started)
	}
}

// #109 AC2: retry preserves prior attempts + history — it starts a new run
// (Trigger=queue) and never touches the recorded lineage/change history.
func TestQueueRetryPreservesHistory(t *testing.T) {
	o := &CycleOutcome{WorkflowID: "r-issue-7", Owner: "o", Repo: "r", Issue: 7, TerminalState: stateBlocked}
	d, store := newQueueConsole(t, o, nil, nil)
	if err := store.recordActivation("o", "r", 7, LineageActivation{
		Attempt: 1, WorkflowID: "r-issue-7", Reason: activationReasonInitial, Result: "BLOCKED",
	}, false); err != nil {
		t.Fatal(err)
	}
	if err := store.recordChange(ChangeRequest{
		Owner: "o", Repo: "r", Issue: 7, State: stateBlocked,
		Feedback: []ChangeFeedback{{Key: "k1", Kind: feedbackKindReviewComment, Body: "prior work"}},
	}); err != nil {
		t.Fatal(err)
	}
	var got IssueInput
	d.startCycle = func(_ context.Context, in IssueInput) error { got = in; return nil }

	res, err := d.submitQueueAction(context.Background(), "r-issue-7", QueueActionRequest{Action: queueActionRetry})
	if err != nil || res.Outcome != queueOutcomeApplied {
		t.Fatalf("retry = %+v err=%v", res, err)
	}
	if got.Trigger != "queue" || got.IssueNum != 7 || got.Owner != "o" {
		t.Fatalf("retry input = %+v, want a queue-triggered run for issue 7", got)
	}
	l, err := store.loadLineage("o", "r", 7, false)
	if err != nil || l.Attempts != 1 || len(l.Activations) != 1 || l.Activations[0].Attempt != 1 {
		t.Fatalf("lineage after retry = %+v err=%v, want the prior attempt preserved", l, err)
	}
	c, err := store.loadChange("r", 7)
	if err != nil || len(c.Feedback) != 1 || c.Feedback[0].Body != "prior work" {
		t.Fatalf("change after retry = %+v err=%v, want prior feedback preserved", c, err)
	}
}

// §42: under dryRun a retry/cancel is a PREVIEW with no mutation.
func TestQueueDryRunPreview(t *testing.T) {
	o := &CycleOutcome{WorkflowID: "r-issue-7", Owner: "o", Repo: "r", Issue: 7, TerminalState: stateBlocked}
	d, _ := newQueueConsole(t, o, nil, nil)
	d.repos["r"] = RepoConfig{Owner: "o", Repo: "r", Lifecycle: &LifecycleConfig{DryRun: true}}
	mutations := 0
	d.startCycle = func(context.Context, IssueInput) error { mutations++; return nil }
	d.cancelWorkflow = func(context.Context, string) error { mutations++; return nil }

	res, err := d.submitQueueAction(context.Background(), "r-issue-7", QueueActionRequest{Action: queueActionRetry})
	if err != nil || res.Outcome != queueOutcomePreview {
		t.Fatalf("dry-run retry = %+v err=%v, want PREVIEW", res, err)
	}
	if mutations != 0 {
		t.Fatal("dry-run must not mutate")
	}
}

// #109/#136/#144: attach/pause/resume have no session provider → SKIPPED, never
// a false success (§40), and no mutation.
func TestQueueSessionVerbsSkipped(t *testing.T) {
	o := &CycleOutcome{WorkflowID: "r-issue-7", Owner: "o", Repo: "r", Issue: 7, TerminalState: stateBlocked}
	d, _ := newQueueConsole(t, o, nil, nil)
	mutations := 0
	d.startCycle = func(context.Context, IssueInput) error { mutations++; return nil }
	d.cancelWorkflow = func(context.Context, string) error { mutations++; return nil }
	for _, verb := range []string{queueActionAttach, queueActionPause, queueActionResume} {
		res, err := d.submitQueueAction(context.Background(), "r-issue-7", QueueActionRequest{Action: verb})
		if err != nil || res.Outcome != queueOutcomeSkipped {
			t.Fatalf("%s = %+v err=%v, want SKIPPED", verb, res, err)
		}
	}
	if mutations != 0 {
		t.Fatal("session verbs must not mutate without a provider")
	}
}

// §55: each accepted queue action is appended to the audit sink with actor
// attribution and a step naming the action.
func TestQueueActionAudited(t *testing.T) {
	o := &CycleOutcome{WorkflowID: "r-issue-7", Owner: "o", Repo: "r", Issue: 7, TerminalState: stateBlocked}
	d, _ := newQueueConsole(t, o, nil, nil)
	sink := filepath.Join(t.TempDir(), "audit.jsonl")
	d.repos["r"] = RepoConfig{Owner: "o", Repo: "r", Lifecycle: &LifecycleConfig{Audit: AuditConfig{Enabled: true, Sink: sink}}}
	d.startCycle = func(context.Context, IssueInput) error { return nil }

	if _, err := d.submitQueueAction(context.Background(), "r-issue-7",
		QueueActionRequest{Action: queueActionRetry, Actor: "dana"}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(sink)
	if err != nil {
		t.Fatal(err)
	}
	var rec struct {
		Actor    string `json:"actor"`
		Decision string `json:"decision"`
		Step     string `json:"step"`
		Issue    int    `json:"issue"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(b))), &rec); err != nil {
		t.Fatalf("audit line: %v (%q)", err, string(b))
	}
	if rec.Actor != "dana" || rec.Decision != "allow" || rec.Step != auditStepQueueRetry || rec.Issue != 7 {
		t.Fatalf("audit record = %+v", rec)
	}
}

// #109 AC4: canceling a running cycle records the authoritative CANCELLED
// terminal (so a canceled run is never a silent, outcome-less drop).
func TestWorkflowCancelRecordsCancelledTerminal(t *testing.T) {
	resetMetrics()
	root := t.TempDir()
	saved := appCfg
	appCfg.Evidence.Dir = root
	t.Cleanup(func() { appCfg = saved })

	env := newCycleEnv(t, EstimateResult{Complexity: 3, Confidence: "high"})
	env.RegisterActivity(WriteCycleEvidence) // real outcome writer
	env.OnActivity(FetchIssueLabels, mock.Anything, mock.Anything).Return([]string{"enhancement"}, nil)
	env.OnActivity(ApplyIssueLabels, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(EnsureManagedNote, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(CleanupWorktree, mock.Anything, mock.Anything).Return(CleanupResult{Outcome: cleanupAbsent, Verified: true}, nil)
	// The developer hangs on the workflow clock so the delayed cancel wins.
	env.OnActivity(RunDeveloper, mock.Anything, mock.Anything).After(2*time.Hour).Return(DevelopResult{}, nil)
	env.SetStartWorkflowOptions(client.StartWorkflowOptions{ID: "r-issue-7"})
	env.RegisterDelayedCallback(env.CancelWorkflow, time.Hour)

	in := IssueInput{Owner: "o", Repo: "r", IssueNum: testIssue, RepoPath: t.TempDir(),
		Lifecycle: LifecycleConfig{Mode: modeSupervisedAutonomous, DevelopmentMaxComplexity: 4}}
	env.ExecuteWorkflow(IssueCycleWorkflow, in)

	b, err := os.ReadFile(filepath.Join(root, "cycles", "r-issue-7", evidenceOutcome))
	if err != nil {
		t.Fatalf("cancelled run must still record outcome.json: %v", err)
	}
	var out CycleOutcome
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.TerminalState != stateCancelled {
		t.Fatalf("cancelled terminal = %q, want CANCELLED (no silent drop)", out.TerminalState)
	}
}

// #109: the queue endpoint returns the projection and the action endpoint maps
// the guards to HTTP statuses (400 malformed / 409 needs-confirmation / 404
// unknown / 501 disabled).
func TestConsoleQueueEndpoints(t *testing.T) {
	o := &CycleOutcome{WorkflowID: "r-issue-7", Owner: "o", Repo: "r", Issue: 7, TerminalState: stateBlocked}
	d, _ := newQueueConsole(t, o, nil, nil)
	started := 0
	d.startCycle = func(context.Context, IssueInput) error { started++; return nil }
	srv, err := newConsoleServer(ConsoleConfig{Addr: "127.0.0.1:0", Token: "secret-token"}, d, "test")
	if err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(srv)
	t.Cleanup(hs.Close)

	res := getURL(t, hs.URL+"/api/v1/queue", "secret-token", "")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("queue = %d, want 200", res.StatusCode)
	}
	var q QueueView
	if err := json.NewDecoder(res.Body).Decode(&q); err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if len(q.Items) != 1 || q.Items[0].ID != "r-issue-7" || q.Items[0].Reason != queueReasonBlocked {
		t.Fatalf("queue = %+v, want the blocked cycle", q.Items)
	}

	post := func(body string) *http.Response {
		req, _ := http.NewRequest(http.MethodPost, hs.URL+"/api/v1/cycles/r-issue-7/actions", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer secret-token")
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	res = post(`{"action":"retry"}`)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("retry = %d, want 200", res.StatusCode)
	}
	res.Body.Close()
	if started != 1 {
		t.Fatalf("started = %d, want 1", started)
	}
	res = post(`{"action":"nope"}`)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad action = %d, want 400", res.StatusCode)
	}
	res.Body.Close()

	// Unknown cycle → 404.
	req, _ := http.NewRequest(http.MethodPost, hs.URL+"/api/v1/cycles/ghost-issue-9/actions", strings.NewReader(`{"action":"retry"}`))
	req.Header.Set("Authorization", "Bearer secret-token")
	res, _ = http.DefaultClient.Do(req)
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown cycle = %d, want 404", res.StatusCode)
	}
	res.Body.Close()

	// Interrupted retry without confirmation → 409.
	if err := d.store.Write("r-issue-8", evidenceStdout, "in-flight evidence", false); err != nil {
		t.Fatal(err)
	}
	d2 := &consoleData{store: d.store, repos: d.repos}
	d2.queue = consoleQueueWriter(d2)
	srv2, _ := newConsoleServer(ConsoleConfig{Addr: "127.0.0.1:0", Token: "t"}, d2, "test")
	hs2 := httptest.NewServer(srv2)
	t.Cleanup(hs2.Close)
	req, _ = http.NewRequest(http.MethodPost, hs2.URL+"/api/v1/cycles/r-issue-8/actions", strings.NewReader(`{"action":"retry"}`))
	req.Header.Set("Authorization", "Bearer t")
	res, _ = http.DefaultClient.Do(req)
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("interrupted retry = %d, want 409", res.StatusCode)
	}
	res.Body.Close()

	// Disabled queue → 501.
	disabled, _ := newConsoleServer(ConsoleConfig{Addr: "127.0.0.1:0", Token: "t"}, &consoleData{store: d.store}, "test")
	hs3 := httptest.NewServer(disabled)
	t.Cleanup(hs3.Close)
	req, _ = http.NewRequest(http.MethodPost, hs3.URL+"/api/v1/cycles/r-issue-7/actions", strings.NewReader(`{"action":"retry"}`))
	req.Header.Set("Authorization", "Bearer t")
	res, _ = http.DefaultClient.Do(req)
	if res.StatusCode != http.StatusNotImplemented {
		t.Fatalf("disabled queue = %d, want 501", res.StatusCode)
	}
	res.Body.Close()
}
