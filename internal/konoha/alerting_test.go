package konoha

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/testsuite"
)

// §53: threshold math — autonomous_completion_rate alerts BELOW its bound, the
// other rates alert ABOVE, equality is not a violation, and onRates narrows the
// evaluated set.
func TestEvaluateRateAlertsThresholds(t *testing.T) {
	thr := map[string]float64{
		mAutonomousRate:         0.7,
		mDidHumanEscalationRate: 0.3,
	}
	values := map[string]float64{
		mAutonomousRate:         0.6, // below 0.7 → alert
		mDidHumanEscalationRate: 0.5, // above 0.3 → alert
		mReviewDisagreementRate: 0.9, // not configured → ignored
	}
	got := EvaluateRateAlerts(thr, nil, values, "w1")
	if len(got) != 2 {
		t.Fatalf("got %d alerts, want 2: %+v", len(got), got)
	}
	// sorted by metric name: autonomous_completion_rate < human_escalation_rate
	if got[0].Metric != mAutonomousRate || got[0].Direction != "below" || got[0].Value != 0.6 {
		t.Fatalf("autonomous alert = %+v", got[0])
	}
	if got[1].Metric != mDidHumanEscalationRate || got[1].Direction != "above" {
		t.Fatalf("escalation alert = %+v", got[1])
	}

	// Equality is not a violation (strict comparisons).
	if a := EvaluateRateAlerts(map[string]float64{mAutonomousRate: 0.6}, nil,
		map[string]float64{mAutonomousRate: 0.6}, "w"); len(a) != 0 {
		t.Fatalf("equal value must not alert: %+v", a)
	}

	// onRates narrows evaluation even when another threshold is violated.
	only := EvaluateRateAlerts(thr, []string{mDidHumanEscalationRate}, values, "w")
	if len(only) != 1 || only[0].Metric != mDidHumanEscalationRate {
		t.Fatalf("onRates filter = %+v", only)
	}
}

// §53/§24: emission is exactly once per (repo, metric, window) — a retry or a
// duplicate run in the same window is deduped, a new window alerts again.
func TestEvaluateAlertsIdempotentPerWindow(t *testing.T) {
	resetMetrics()
	resetEmittedAlerts()
	defer func() { resetMetrics(); resetEmittedAlerts() }()

	sink.setGauge(mAutonomousRate, 0.5) // < 0.7
	req := AlertRequest{
		Owner: "o", Repo: "r", Window: "2026-01-01T00:00:00Z",
		Thresholds: map[string]float64{mAutonomousRate: 0.7},
	}

	first, err := EvaluateAlerts(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Emitted) != 1 || first.Emitted[0].Metric != mAutonomousRate {
		t.Fatalf("first evaluation = %+v, want one emitted alert", first)
	}

	// Same window → deduped, nothing emitted.
	second, err := EvaluateAlerts(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Emitted) != 0 || len(second.Deduped) != 1 {
		t.Fatalf("retry in the same window = %+v, want deduped", second)
	}

	// A new window alerts again (per-window, not forever).
	req.Window = "2026-01-02T00:00:00Z"
	third, err := EvaluateAlerts(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if len(third.Emitted) != 1 {
		t.Fatalf("new window = %+v, want one alert", third)
	}

	// A different repo is independent.
	req.Owner, req.Repo = "o2", "r2"
	fourth, err := EvaluateAlerts(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if len(fourth.Emitted) != 1 {
		t.Fatalf("other repo = %+v, want one alert", fourth)
	}
}

// §53: dry-run alerts log but never POST to the webhook.
func TestEvaluateAlertsDryRunNoHTTP(t *testing.T) {
	resetMetrics()
	resetEmittedAlerts()
	defer func() { resetMetrics(); resetEmittedAlerts() }()

	sink.setGauge(mAutonomousRate, 0.1)
	res, err := EvaluateAlerts(context.Background(), AlertRequest{
		Owner: "o", Repo: "r", Window: "w",
		Notify:     "http://127.0.0.1:0/never",
		Thresholds: map[string]float64{mAutonomousRate: 0.7},
		DryRun:     true,
	})
	if err != nil {
		t.Fatalf("dry-run must not attempt the webhook: %v", err)
	}
	if len(res.Emitted) != 1 {
		t.Fatalf("dry-run emitted = %+v, want one", res)
	}
}

// §53/§24: one bug issue per run — a second run with the marker present never
// creates a duplicate.
func TestEnsureFailureIssueIdempotent(t *testing.T) {
	in := FailureFeedbackRequest{
		Owner: "o", Repo: "r",
		Outcome: CycleOutcome{WorkflowID: "r-issue-7", Issue: 7, TerminalState: stateBlocked, Attempts: 2},
	}
	calls := depFakeGh(t, nil, 42)

	first, err := EnsureFailureIssue(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Created || first.Number != 42 {
		t.Fatalf("first run = %+v, want created #42", first)
	}
	if n := len(createdIssueCalls(*calls)); n != 1 {
		t.Fatalf("first run must create exactly one issue, got %d", n)
	}

	// Second run: the marker already exists → matched, no create.
	*calls = nil
	prev := ghRunner
	ghRunner = func(_ context.Context, args ...string) ([]byte, error) {
		line := strings.Join(args, " ")
		*calls = append(*calls, line)
		if strings.Contains(line, "issues?state=all") {
			return json.Marshal([]ghItem{{Number: 42, Body: "x\n" + failureMarker("r-issue-7") + "\ny", State: "open"}})
		}
		return nil, fmt.Errorf("must not create: %s", line)
	}
	t.Cleanup(func() { ghRunner = prev })

	second, err := EnsureFailureIssue(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if second.Created || second.Number != 42 {
		t.Fatalf("second run = %+v, want matched #42 (no create)", second)
	}
}

// §42: dry-run files nothing; §40: a missing run id fails closed.
func TestEnsureFailureIssueGuards(t *testing.T) {
	prev := ghRunner
	ghRunner = func(context.Context, ...string) ([]byte, error) {
		t.Fatal("guarded paths must not call gh")
		return nil, nil
	}
	t.Cleanup(func() { ghRunner = prev })

	if res, err := EnsureFailureIssue(context.Background(), FailureFeedbackRequest{
		Owner: "o", Repo: "r", DryRun: true,
		Outcome: CycleOutcome{WorkflowID: "w", TerminalState: stateBlocked},
	}); err != nil || res.Number != 0 || res.Created {
		t.Fatalf("dry-run = %+v err=%v, want no-op", res, err)
	}
	if _, err := EnsureFailureIssue(context.Background(), FailureFeedbackRequest{
		Owner: "o", Repo: "r", Outcome: CycleOutcome{TerminalState: stateBlocked},
	}); err == nil {
		t.Fatal("missing workflowId must fail closed")
	}
}

// §53: only an enabled + thresholded block passes validation; unknown metric
// names / bad notify / bad schedule fail fast.
func TestAlertingConfigValidation(t *testing.T) {
	good := `
repos:
  - repo: good
    owner: o
    repoPath: /x
    lifecycle:
      alerting:
        enabled: true
        notify: https://hooks.example/abc
        schedule: 12h
        onRates: [autonomous_completion_rate]
        thresholds:
          autonomous_completion_rate: 0.7
          human_escalation_rate: 0.3
`
	if _, err := LoadReposConfig(writeRepos(t, good)); err != nil {
		t.Fatalf("valid alerting config rejected: %v", err)
	}

	bad := map[string]string{
		"unknown threshold metric": `
repos: [{repo: r, owner: o, repoPath: /x, lifecycle: {alerting: {enabled: true, thresholds: {typo_metric: 1}}}}]`,
		"enabled without thresholds": `
repos: [{repo: r, owner: o, repoPath: /x, lifecycle: {alerting: {enabled: true}}}]`,
		"bad notify scheme": `
repos: [{repo: r, owner: o, repoPath: /x, lifecycle: {alerting: {notify: "ftp://x", thresholds: {split_rate: 0.5}}}}]`,
		"bad schedule": `
repos: [{repo: r, owner: o, repoPath: /x, lifecycle: {alerting: {schedule: nope, thresholds: {split_rate: 0.5}}}}]`,
		"unknown onRates": `
repos: [{repo: r, owner: o, repoPath: /x, lifecycle: {alerting: {onRates: [bogus], thresholds: {split_rate: 0.5}}}}]`,
		"rate over 1": `
repos: [{repo: r, owner: o, repoPath: /x, lifecycle: {alerting: {enabled: true, thresholds: {autonomous_completion_rate: 1.5}}}}]`,
	}
	for name, body := range bad {
		if _, err := LoadReposConfig(writeRepos(t, body)); err == nil {
			t.Fatalf("%s: invalid config must fail fast", name)
		}
	}
}

// §53: only alerting.enabled repos emit an alert schedule line.
func TestAlertsScheduleInputs(t *testing.T) {
	p := writeRepos(t, `
repos:
  - repo: on
    owner: o
    repoPath: /x
    lifecycle:
      alerting:
        enabled: true
        schedule: 6h
        thresholds: {autonomous_completion_rate: 0.7}
  - repo: off
    owner: o
    repoPath: /x
`)
	lines, err := alertsScheduleInputs(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 {
		t.Fatalf("got %d alert schedule lines, want 1", len(lines))
	}
	parts := strings.SplitN(lines[0], "\t", 3)
	if len(parts) != 3 || parts[0] != "on" || parts[1] != "6h" {
		t.Fatalf("unexpected alert schedule line: %q", lines[0])
	}
	var scope RepoScope
	if err := json.Unmarshal([]byte(parts[2]), &scope); err != nil {
		t.Fatal(err)
	}
	if !AlertingEnabled(scope.Lifecycle) {
		t.Fatalf("unexpected scope: %+v", scope)
	}

	off := writeRepos(t, `repos: [{repo: off, owner: o, repoPath: /x}]`)
	if lines, err := alertsScheduleInputs(off); err != nil || len(lines) != 0 {
		t.Fatalf("disabled repo must emit no alert schedule: %v err=%v", lines, err)
	}
}

// §53: disabled alerting never evaluates; an enabled workflow passes the
// resolved thresholds + a deterministic window to the activity.
func TestAlertWorkflowWiring(t *testing.T) {
	evaluated := 0
	var captured AlertRequest
	env := (&testsuite.WorkflowTestSuite{}).NewTestWorkflowEnvironment()
	env.OnActivity(EvaluateAlerts, mock.Anything, mock.Anything).Return(func(_ context.Context, in AlertRequest) (AlertResult, error) {
		evaluated++
		captured = in
		return AlertResult{}, nil
	})

	// Disabled → no activity call.
	env.ExecuteWorkflow(AlertWorkflow, RepoScope{Owner: "o", Repo: "off"})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	if evaluated != 0 {
		t.Fatalf("disabled alerting evaluated %d times, want 0", evaluated)
	}

	// Enabled → one evaluation carrying the config + a window.
	env2 := (&testsuite.WorkflowTestSuite{}).NewTestWorkflowEnvironment()
	env2.OnActivity(EvaluateAlerts, mock.Anything, mock.Anything).Return(func(_ context.Context, in AlertRequest) (AlertResult, error) {
		captured = in
		return AlertResult{}, nil
	})
	in := RepoScope{Owner: "o", Repo: "on", Lifecycle: LifecycleConfig{Alerting: AlertingConfig{
		Enabled: true, Notify: "https://hooks.example/x", Schedule: "1h",
		Thresholds: map[string]float64{mAutonomousRate: 0.7},
	}}}
	env2.ExecuteWorkflow(AlertWorkflow, in)
	if err := env2.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	if captured.Owner != "o" || captured.Repo != "on" || captured.Window == "" ||
		captured.Notify != "https://hooks.example/x" || captured.Thresholds[mAutonomousRate] != 0.7 {
		t.Fatalf("unexpected activity request: %+v", captured)
	}
}

// §53: FailureFeedbackWorkflow's only I/O is the idempotent EnsureFailureIssue.
func TestFailureFeedbackWorkflowFiles(t *testing.T) {
	filed := 0
	env := (&testsuite.WorkflowTestSuite{}).NewTestWorkflowEnvironment()
	env.OnActivity(EnsureFailureIssue, mock.Anything, mock.Anything).Return(func(_ context.Context, in FailureFeedbackRequest) (FailureIssueResult, error) {
		filed++
		return FailureIssueResult{Number: 9, Created: true}, nil
	})
	env.ExecuteWorkflow(FailureFeedbackWorkflow, FailureFeedbackRequest{
		Owner: "o", Repo: "r",
		Outcome: CycleOutcome{WorkflowID: "r-issue-7", Issue: 7, TerminalState: stateBlocked},
	})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	if filed != 1 {
		t.Fatalf("EnsureFailureIssue filed %d times, want 1", filed)
	}
}

// §53/§40: a BLOCKED run with alerting enabled files exactly one bug issue; the
// terminal state stays BLOCKED (alerting never resolves it).
func TestWorkflowBlockedFilesFeedbackWhenEnabled(t *testing.T) {
	resetMetrics()
	resetEmittedAlerts()
	env := mockCycleEnv(t, EstimateResult{Complexity: 3, Confidence: "high"})
	env.OnActivity(FetchIssueLabels, mock.Anything, mock.Anything).Return([]string{"bug"}, nil)
	env.OnActivity(RunDeveloper, mock.Anything, mock.Anything).Return(DevelopResult{
		WorktreeDir: "/tmp/wt", CommitSHA: "devsha", ChangedFiles: []string{"pkg/worker.go"}, Summary: "c", Validated: true,
	}, nil)
	env.OnActivity(PublishDeveloperChange, mock.Anything, mock.Anything).Return(PublishResult{Branch: "b", PRNumber: 101, Validated: true}, nil)
	env.OnActivity(FetchPRHead, mock.Anything, mock.Anything).Return("headsha", nil)
	env.OnActivity(FetchPRDiff, mock.Anything, mock.Anything).Return("+1 line", nil)
	env.OnActivity(RunReviewer, mock.Anything, mock.Anything).Return(ReviewResult{
		Verdict: "REQUEST_CHANGES", CommitSHA: "headsha", PRNumber: 101, Blocking: []string{"nope"},
	}, nil)
	env.OnActivity(RunRepair, mock.Anything, mock.Anything).Return(DevelopResult{
		WorktreeDir: "/tmp/wt2", CommitSHA: "repairsha", ChangedFiles: []string{"pkg/worker.go"}, Summary: "f", Validated: true,
	}, nil)
	env.OnActivity(EnsureIssueState, mock.Anything, mock.Anything).Return(nil)
	filed := 0
	env.OnActivity(EnsureFailureIssue, mock.Anything, mock.Anything).Return(func(_ context.Context, in FailureFeedbackRequest) (FailureIssueResult, error) {
		filed++
		if in.Outcome.WorkflowID == "" || in.Outcome.TerminalState != stateBlocked {
			t.Errorf("feedback request missing run facts: %+v", in.Outcome)
		}
		return FailureIssueResult{Number: 55, Created: true}, nil
	})

	in := IssueInput{Owner: "o", Repo: "r", IssueNum: testIssue, RepoPath: t.TempDir(),
		Lifecycle: LifecycleConfig{
			Mode: modeSupervisedAutonomous, DevelopmentMaxComplexity: 4,
			Alerting: AlertingConfig{Enabled: true, Thresholds: map[string]float64{mAutonomousRate: 0.7}},
		}}
	env.ExecuteWorkflow(IssueCycleWorkflow, in)
	if env.IsWorkflowCompleted() {
		if err := env.GetWorkflowError(); err != nil {
			t.Fatalf("workflow error: %v", err)
		}
	}
	if got := terminalState(t, env); got != stateBlocked {
		t.Fatalf("terminal = %q, want BLOCKED (alerting must not resolve the run)", got)
	}
	if filed != 1 {
		t.Fatalf("EnsureFailureIssue filed %d times, want exactly 1", filed)
	}
}
