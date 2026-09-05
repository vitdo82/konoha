package main

import (
	"testing"

	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/testsuite"
)

// --- Workflow tests (§41) via the Temporal testsuite -------------------------
//
// The agent activities (AnalyzeIssue/EstimateIssue) shell out to `opencode`, so
// they are mocked with testify Anything matchers. RecordCycleMetrics runs for
// real (replay-safe activity) and mutates the §39 sink, so the tests assert the
// terminal state via the "state" query handler and that the sink recorded the
// right route.

func runCycle(t *testing.T, est EstimateResult) *testsuite.TestWorkflowEnvironment {
	t.Helper()
	env := (&testsuite.WorkflowTestSuite{}).NewTestWorkflowEnvironment()

	analysis := IssueAnalysis{
		Title: "t", Body: "b", Summary: "s", Spec: specFallback,
		Requirements: []string{"r1"}, AcceptanceCriteria: []string{"Given c When d Then e"},
	}
	env.RegisterActivity(RecordCycleMetrics)
	env.OnActivity(AnalyzeIssue, mock.Anything, mock.Anything).Return(analysis, nil)
	env.OnActivity(EstimateIssue, mock.Anything, mock.Anything, mock.Anything).Return(est, nil)

	// Phase-3 repo (supervised-autonomous, ceiling 4, risk-based) so routing is
	// meaningful — an empty config would be the conservative shadow default.
	in := IssueInput{Owner: "o", Repo: "r", IssueNum: 7, RepoPath: t.TempDir(),
		Lifecycle: LifecycleConfig{Mode: modeSupervisedAutonomous, DevelopmentMaxComplexity: 4}}
	env.ExecuteWorkflow(IssueCycleWorkflow, in)
	if env.IsWorkflowCompleted() {
		if err := env.GetWorkflowError(); err != nil {
			t.Fatalf("workflow error: %v", err)
		}
	}
	return env
}

// terminalState queries the §6 "state" handler.
func terminalState(t *testing.T, env *testsuite.TestWorkflowEnvironment) string {
	t.Helper()
	query, err := env.QueryWorkflow("state")
	if err != nil {
		t.Fatalf("query state: %v", err)
	}
	var s string
	if err := query.Get(&s); err != nil {
		t.Fatalf("query decode: %v", err)
	}
	return s
}

// §41 autonomous-small path: complexity within threshold → SMALL, and the §39
// metrics activity (executed for real) records the SMALL route.
func TestWorkflowSmallRouting(t *testing.T) {
	resetMetrics()
	est := EstimateResult{Complexity: 3, Confidence: "high"}
	env := runCycle(t, est)
	if got := terminalState(t, env); got != stateSmall {
		t.Fatalf("small route terminal state = %q, want SMALL", got)
	}
	if v := sink.get(mSplitSmall); v != 1 {
		t.Fatalf("issue_route_small = %v, want 1", v)
	}
}

// §41 autonomous-large path: complexity above threshold → LARGE, record LARGE.
func TestWorkflowLargeRouting(t *testing.T) {
	resetMetrics()
	est := EstimateResult{Complexity: 7, Confidence: "medium"}
	env := runCycle(t, est)
	if got := terminalState(t, env); got != stateLarge {
		t.Fatalf("large route terminal state = %q, want LARGE", got)
	}
	if v := sink.get(mSplitLarge); v != 1 {
		t.Fatalf("issue_route_large = %v, want 1", v)
	}
}

// §41 review-retry-cap boundary (§16): attempts under the cap allow a repair;
// at/over the cap the bounded loop escalates to §17 AI diagnosis, never a
// silent pass and never an unbounded loop.
func TestReviewRetryCapBoundary(t *testing.T) {
	// default cap 2: 0 and 1 completed attempts → repair still allowed.
	for _, done := range []int{0, 1} {
		if !EvaluateRepairLoop(LifecycleConfig{}, done).Allowed {
			t.Fatalf("cap 2: %d completed must still allow a repair", done)
		}
	}
	// 2 completed → escalate (allowed=false, escalate=true).
	d := EvaluateRepairLoop(LifecycleConfig{}, 2)
	if d.Allowed || !d.Escalate {
		t.Fatalf("cap 2: 2 completed attempts must escalate, got %+v", d)
	}
	// config cap override: 3 allows the 3rd attempt, 4 escalates.
	lc3 := LifecycleConfig{MaxDevReviewAttempts: 3}
	if !EvaluateRepairLoop(lc3, 2).Allowed || EvaluateRepairLoop(lc3, 3).Allowed {
		t.Fatal("config cap 3 boundary violated (3rd attempt allowed, 4th escalates)")
	}
}
