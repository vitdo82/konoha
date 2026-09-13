package konoha

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/testsuite"
)

// --- Workflow tests (§41) via the Temporal testsuite -------------------------
//
// Agent activities (AnalyzeIssue/EstimateIssue/RunDeveloper/RunReviewer/
// RunRepair) shell out to `opencode`/`gh`, so they are mocked with testify
// Anything matchers (one per argument). RecordCycleMetrics runs for real; the
// tests assert the terminal §6 state via the "state" query handler.

const testIssue = 7

// mockCycleEnv returns an env with the analysis/estimate mocks wired and
// RecordCycleMetrics registered, primed for the given estimate.
func mockCycleEnv(t *testing.T, est EstimateResult) *testsuite.TestWorkflowEnvironment {
	t.Helper()
	env := (&testsuite.WorkflowTestSuite{}).NewTestWorkflowEnvironment()
	env.RegisterActivity(RecordCycleMetrics)
	env.RegisterWorkflow(PostMergeWorkflow) // §50: registered for release-enabled tests

	analysis := IssueAnalysis{
		Title: "t", Body: "b", Summary: "s", Spec: specFallback,
		Requirements: []string{"r1"}, AcceptanceCriteria: []string{"Given c When d Then e"},
	}
	env.OnActivity(AnalyzeIssue, mock.Anything, mock.Anything).Return(analysis, nil)
	env.OnActivity(EstimateIssue, mock.Anything, mock.Anything, mock.Anything).Return(est, nil)
	return env
}

// smallPathHooks counts the side-effecting calls a test cares about.
type smallPathHooks struct {
	merges, closes int
}

// mockSmallPath wires the dev→publish→review activities for a clean APPROVE
// with CI green and a successful merge.
func mockSmallPath(env *testsuite.TestWorkflowEnvironment) *smallPathHooks {
	return mockSmallPathWith(env, CIStatus{Green: true}, nil, MergeResult{Merged: true}, nil)
}

// mockSmallPathWith wires the SMALL path but lets a test force the §49 CI gate
// verdict/error and the merge outcome (mergeErr != nil simulates a merge
// failure).
func mockSmallPathWith(env *testsuite.TestWorkflowEnvironment, ci CIStatus, ciErr error, merged MergeResult, mergeErr error) *smallPathHooks {
	h := &smallPathHooks{}
	env.OnActivity(FetchIssueLabels, mock.Anything, mock.Anything).Return([]string{"enhancement"}, nil)
	env.OnActivity(RunDeveloper, mock.Anything, mock.Anything).Return(DevelopResult{
		WorktreeDir: "/tmp/wt", CommitSHA: "devsha", ChangedFiles: []string{"pkg/worker.go"},
		Summary: "change", Validated: true,
	}, nil)
	env.OnActivity(PublishDeveloperChange, mock.Anything, mock.Anything).Return(PublishResult{
		Branch: "konoha/issue-7", PRNumber: 101, PRURL: "https://x/pr/101", Validated: true,
	}, nil)
	env.OnActivity(FetchPRHead, mock.Anything, mock.Anything).Return("headsha", nil)
	env.OnActivity(FetchPRDiff, mock.Anything, mock.Anything).Return("+1 line", nil)
	env.OnActivity(RunReviewer, mock.Anything, mock.Anything).Return(ReviewResult{
		Verdict: "APPROVE", CommitSHA: "headsha", PRNumber: 101,
	}, nil)
	// §49: CI gate verdict, then the merge. A non-green CI must never merge.
	env.OnActivity(WaitForCI, mock.Anything, mock.Anything).Return(ci, ciErr)
	env.OnActivity(MergePullRequest, mock.Anything, mock.Anything).Return(func(_ context.Context, in MergeRequest) (MergeResult, error) {
		h.merges++
		return merged, mergeErr
	})
	env.OnActivity(EnsureIssueState, mock.Anything, mock.Anything).Return(func(_ context.Context, s StateRequest) error {
		if s.State == "closed" {
			h.closes++
		}
		return nil
	})
	return h
}

// runAndAssert executes the workflow and asserts it completed without error.
func runAndAssert(t *testing.T, env *testsuite.TestWorkflowEnvironment) {
	t.Helper()
	in := IssueInput{Owner: "o", Repo: "r", IssueNum: testIssue, RepoPath: t.TempDir(),
		Lifecycle: LifecycleConfig{Mode: modeSupervisedAutonomous, DevelopmentMaxComplexity: 4}}
	env.ExecuteWorkflow(IssueCycleWorkflow, in)
	if env.IsWorkflowCompleted() {
		if err := env.GetWorkflowError(); err != nil {
			t.Fatalf("workflow error: %v", err)
		}
	}
}

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

// §46/§41: SMALL happy path — dev → publish → APPROVE → merge → close, exactly
// one PR and exactly one merge.
func TestWorkflowSmallAutonomousClose(t *testing.T) {
	resetMetrics()
	env := mockCycleEnv(t, EstimateResult{Complexity: 3, Confidence: "high"})
	h := mockSmallPath(env)
	runAndAssert(t, env)

	if got := terminalState(t, env); got != stateClosed {
		t.Fatalf("small autonomous path terminal = %q, want CLOSED", got)
	}
	if v := sink.get(mCyclesCompletedAuton); v != 1 {
		t.Fatalf("autonomous completes = %v, want 1", v)
	}
	// §49: the issue closes only after exactly one merge.
	if h.merges != 1 || h.closes != 1 {
		t.Fatalf("merge/close counts = %d/%d, want 1/1", h.merges, h.closes)
	}
}

// §49/§41: CI not green → PR is NEVER merged and the issue is NOT closed; the
// workflow escalates to HUMAN_REQUIRED (no silent success).
func TestWorkflowCIFailureEscalates(t *testing.T) {
	resetMetrics()
	env := mockCycleEnv(t, EstimateResult{Complexity: 3, Confidence: "high"})
	h := mockSmallPathWith(env, CIStatus{Failed: true, Detail: "1 check failed"}, nil, MergeResult{}, nil)
	runAndAssert(t, env)

	if got := terminalState(t, env); got != stateHumanRequired {
		t.Fatalf("CI-failure terminal = %q, want HUMAN_REQUIRED", got)
	}
	if h.merges != 0 || h.closes != 0 {
		t.Fatalf("CI failure must not merge or close (merges=%d closes=%d)", h.merges, h.closes)
	}
	if v := sink.get(mHumanEscalations); v < 1 {
		t.Fatalf("CI failure must count as a human escalation, got %v", v)
	}
}

// §49/§40: an exhausted CI activity (persistent error) also escalates instead of
// failing the workflow — never a silent success.
func TestWorkflowCIActivityErrorEscalates(t *testing.T) {
	resetMetrics()
	env := mockCycleEnv(t, EstimateResult{Complexity: 3, Confidence: "high"})
	h := mockSmallPathWith(env, CIStatus{}, errors.New("gh unavailable"), MergeResult{}, nil)
	runAndAssert(t, env)

	if got := terminalState(t, env); got != stateHumanRequired {
		t.Fatalf("CI activity error terminal = %q, want HUMAN_REQUIRED", got)
	}
	if h.merges != 0 || h.closes != 0 {
		t.Fatalf("CI activity error must not merge or close (merges=%d closes=%d)", h.merges, h.closes)
	}
}

// §49/§41: merge error after a green CI → issue stays open, escalates.
func TestWorkflowMergeFailureEscalates(t *testing.T) {
	resetMetrics()
	env := mockCycleEnv(t, EstimateResult{Complexity: 3, Confidence: "high"})
	h := mockSmallPathWith(env, CIStatus{Green: true}, nil, MergeResult{}, errors.New("branch protection"))
	runAndAssert(t, env)

	if got := terminalState(t, env); got != stateHumanRequired {
		t.Fatalf("merge-failure terminal = %q, want HUMAN_REQUIRED", got)
	}
	if h.closes != 0 {
		t.Fatalf("failed merge must never close the issue (closes=%d)", h.closes)
	}
}

// §46/§41: one REQUEST_CHANGES then APPROVE → repair round once, close.
func TestWorkflowSmallRepairLoop(t *testing.T) {
	resetMetrics()
	env := mockCycleEnv(t, EstimateResult{Complexity: 3, Confidence: "high"})
	env.OnActivity(FetchIssueLabels, mock.Anything, mock.Anything).Return([]string{"bug"}, nil)
	env.OnActivity(RunDeveloper, mock.Anything, mock.Anything).Return(DevelopResult{
		WorktreeDir: "/tmp/wt", CommitSHA: "devsha", ChangedFiles: []string{"pkg/worker.go"}, Summary: "c", Validated: true,
	}, nil)

	// first publish
	first := true
	env.OnActivity(PublishDeveloperChange, mock.Anything, mock.Anything).Return(func(ctx context.Context, in PublishInput) (PublishResult, error) {
		_ = ctx
		if first {
			first = false
		}
		return PublishResult{Branch: "konoha/issue-7", PRNumber: 101, Validated: true}, nil
	})
	env.OnActivity(FetchPRHead, mock.Anything, mock.Anything).Return("headsha", nil)
	env.OnActivity(FetchPRDiff, mock.Anything, mock.Anything).Return("+1 line", nil)
	// first review → REQUEST_CHANGES, then APPROVE
	calls := 0
	env.OnActivity(RunReviewer, mock.Anything, mock.Anything).Return(func(ctx context.Context, r ReviewRequest) (ReviewResult, error) {
		_ = ctx
		calls++
		if calls == 1 {
			return ReviewResult{Verdict: "REQUEST_CHANGES", CommitSHA: "headsha", PRNumber: 101,
				Blocking: []string{"auth gap"}}, nil
		}
		return ReviewResult{Verdict: "APPROVE", CommitSHA: "headsha", PRNumber: 101}, nil
	})
	// repair runs once
	env.OnActivity(RunRepair, mock.Anything, mock.Anything).Return(DevelopResult{
		WorktreeDir: "/tmp/wt2", CommitSHA: "repairsha", ChangedFiles: []string{"pkg/worker.go"}, Summary: "fixed", Validated: true,
	}, nil)
	env.OnActivity(WaitForCI, mock.Anything, mock.Anything).Return(CIStatus{Green: true}, nil)
	env.OnActivity(MergePullRequest, mock.Anything, mock.Anything).Return(MergeResult{Merged: true}, nil)
	env.OnActivity(EnsureIssueState, mock.Anything, mock.Anything).Return(nil)

	runAndAssert(t, env)
	if got := terminalState(t, env); got != stateClosed {
		t.Fatalf("repair-loop path terminal = %q, want CLOSED", got)
	}
	if calls != 2 {
		t.Fatalf("reviewer called %d times, want 2", calls)
	}
}

// §46/§41/§16: REQUEST_CHANGES past the cap → BLOCKED, never close.
func TestWorkflowSmallRepairCapBlocked(t *testing.T) {
	resetMetrics()
	env := mockCycleEnv(t, EstimateResult{Complexity: 3, Confidence: "high"})
	env.OnActivity(FetchIssueLabels, mock.Anything, mock.Anything).Return([]string{"bug"}, nil)
	env.OnActivity(RunDeveloper, mock.Anything, mock.Anything).Return(DevelopResult{
		WorktreeDir: "/tmp/wt", CommitSHA: "devsha", ChangedFiles: []string{"pkg/worker.go"}, Summary: "c", Validated: true,
	}, nil)
	// always fails review, cap 2 (default) → attempt 2 escalates.
	env.OnActivity(PublishDeveloperChange, mock.Anything, mock.Anything).Return(PublishResult{Branch: "b", PRNumber: 101, Validated: true}, nil)
	env.OnActivity(FetchPRHead, mock.Anything, mock.Anything).Return("headsha", nil)
	env.OnActivity(FetchPRDiff, mock.Anything, mock.Anything).Return("+1 line", nil)
	env.OnActivity(RunReviewer, mock.Anything, mock.Anything).Return(ReviewResult{
		Verdict: "REQUEST_CHANGES", CommitSHA: "headsha", PRNumber: 101, Blocking: []string{"nope"},
	}, nil)
	env.OnActivity(RunRepair, mock.Anything, mock.Anything).Return(DevelopResult{
		WorktreeDir: "/tmp/wt2", CommitSHA: "repairsha", ChangedFiles: []string{"pkg/worker.go"}, Summary: "f", Validated: true,
	}, nil)
	// EnsureIssueState(CLOSED) must NEVER run.
	closed := 0
	env.OnActivity(EnsureIssueState, mock.Anything, mock.Anything).Return(func(ctx context.Context, s StateRequest) error {
		_ = ctx
		if s.State == "closed" {
			closed++
		}
		return nil
	})

	runAndAssert(t, env)
	if got := terminalState(t, env); got != stateBlocked {
		t.Fatalf("cap path terminal = %q, want BLOCKED", got)
	}
	if closed != 0 {
		t.Fatalf("EnsureIssueState(closed) called %d times; cap must never close (§40)", closed)
	}
}

// §46/§20: dev diff touching a restricted path → HUMAN_REQUIRED, no publish.
func TestWorkflowSmallRiskGateHumanRequired(t *testing.T) {
	resetMetrics()
	lc := LifecycleConfig{Mode: modeSupervisedAutonomous, DevelopmentMaxComplexity: 4,
		Autonomy: Autonomy{HumanRequiredPaths: []string{"infra/**"}}}
	env := mockCycleEnv(t, EstimateResult{Complexity: 3, Confidence: "high"})
	env.OnActivity(AnalyzeIssue, mock.Anything, mock.Anything).Return(IssueAnalysis{
		Title: "t", Body: "b", Summary: "s", Requirements: []string{"r1"}, AcceptanceCriteria: []string{"c"}, Spec: specFallback,
	}, nil)
	env.OnActivity(EstimateIssue, mock.Anything, mock.Anything, mock.Anything).Return(EstimateResult{Complexity: 3, Confidence: "high"}, nil)
	env.OnActivity(FetchIssueLabels, mock.Anything, mock.Anything).Return([]string{"enhancement"}, nil)
	env.OnActivity(RunDeveloper, mock.Anything, mock.Anything).Return(DevelopResult{
		WorktreeDir: "/tmp/wt", CommitSHA: "devsha", ChangedFiles: []string{"infra/prod.yaml"}, Summary: "c", Validated: true,
	}, nil)
	published := 0
	env.OnActivity(PublishDeveloperChange, mock.Anything, mock.Anything).Return(func(ctx context.Context, in PublishInput) (PublishResult, error) {
		_ = ctx
		published++
		return PublishResult{}, nil
	})

	in := IssueInput{Owner: "o", Repo: "r", IssueNum: testIssue, RepoPath: t.TempDir(), Lifecycle: lc}
	env.ExecuteWorkflow(IssueCycleWorkflow, in)
	if env.IsWorkflowCompleted() {
		if err := env.GetWorkflowError(); err != nil {
			t.Fatalf("workflow error: %v", err)
		}
	}
	if got := terminalState(t, env); got != stateHumanRequired {
		t.Fatalf("risk-gate terminal = %q, want HUMAN_REQUIRED", got)
	}
	if published != 0 {
		t.Fatalf("publish called %d times; §20 must suppress publish before mutation", published)
	}
}

// §8/§19: LARGE route splits → creates child issues → each child runs the real
// SMALL IssueCycleWorkflow (mocked activities) → all complete → parent CLOSED.
func TestWorkflowLargeSplitChildrenClose(t *testing.T) {
	resetMetrics()
	env := (&testsuite.WorkflowTestSuite{}).NewTestWorkflowEnvironment()
	env.RegisterActivity(RecordCycleMetrics)
	analysis := IssueAnalysis{Title: "t", Body: "b", Summary: "s", Spec: specFallback,
		Requirements: []string{"r1"}, AcceptanceCriteria: []string{"Given c When d Then e"}}
	env.OnActivity(AnalyzeIssue, mock.Anything, mock.Anything).Return(analysis, nil)
	// parent (issue testIssue) is LARGE; children (any other issue #) are SMALL.
	env.OnActivity(EstimateIssue, mock.Anything, mock.Anything, mock.Anything).Return(
		func(_ context.Context, in IssueInput, a IssueAnalysis) (EstimateResult, error) {
			if in.IssueNum == testIssue {
				return EstimateResult{Complexity: 7, Confidence: "medium"}, nil
			}
			return EstimateResult{Complexity: 2, Confidence: "high"}, nil
		})
	env.OnActivity(RunSplit, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(SplitResult{
		Tickets: []SplitTicket{{Key: "01", Title: "child-a", Complexity: 2, Requirements: []string{"r"}, AcceptanceCriteria: []string{"c"}}},
	}, nil)
	env.OnActivity(EnsureChildren, mock.Anything, mock.Anything).Return([]ChildIssue{
		{Key: "01", ParentIssueNumber: testIssue, GitHubIssueNumber: 101, Complexity: 2, Title: "child-a"},
	}, nil)
	env.OnActivity(EnsureIssueState, mock.Anything, mock.Anything).Return(nil)
	// Child issue #101 runs the real SMALL path: dev → publish → APPROVE → close.
	mockSmallPath(env) // activity mocks are global (mock.Anything), reused by child

	runAndAssert(t, env)
	if got := terminalState(t, env); got != stateClosed {
		t.Fatalf("large route terminal = %q, want CLOSED", got)
	}
	if v := sink.get(mSplitLarge); v != 1 {
		t.Fatalf("issue_route_large = %v, want 1", int(v))
	}
}

// §8/§40: a child failure fails the parent into BLOCKED (no silent skip).
func TestWorkflowLargeChildFailureBlocksParent(t *testing.T) {
	resetMetrics()
	env := (&testsuite.WorkflowTestSuite{}).NewTestWorkflowEnvironment()
	env.RegisterActivity(RecordCycleMetrics)
	analysis := IssueAnalysis{Title: "t", Body: "b", Summary: "s", Spec: specFallback,
		Requirements: []string{"r1"}, AcceptanceCriteria: []string{"Given c When d Then e"}}
	env.OnActivity(AnalyzeIssue, mock.Anything, mock.Anything).Return(analysis, nil)
	env.OnActivity(EstimateIssue, mock.Anything, mock.Anything, mock.Anything).Return(
		func(_ context.Context, in IssueInput, a IssueAnalysis) (EstimateResult, error) {
			if in.IssueNum == testIssue {
				return EstimateResult{Complexity: 7, Confidence: "medium"}, nil
			}
			return EstimateResult{Complexity: 2, Confidence: "high"}, nil
		})
	env.OnActivity(RunSplit, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(SplitResult{
		Tickets: []SplitTicket{{Key: "01", Title: "child-a", Complexity: 2, Requirements: []string{"r"}, AcceptanceCriteria: []string{"c"}}},
	}, nil)
	env.OnActivity(EnsureChildren, mock.Anything, mock.Anything).Return([]ChildIssue{
		{Key: "01", ParentIssueNumber: testIssue, GitHubIssueNumber: 101, Complexity: 2, Title: "child-a"},
	}, nil)
	env.OnActivity(EnsureIssueState, mock.Anything, mock.Anything).Return(nil)
	// Child runs the real SMALL path but its RunDeveloper fails → child fails →
	// parent must BLOCK (never silently continue).
	env.OnActivity(FetchIssueLabels, mock.Anything, mock.Anything).Return([]string{"enhancement"}, nil)
	env.OnActivity(RunDeveloper, mock.Anything, mock.Anything).Return(DevelopResult{}, errors.New("dev failed"))
	env.OnActivity(FetchPRHead, mock.Anything, mock.Anything).Return("headsha", nil)
	env.OnActivity(FetchPRDiff, mock.Anything, mock.Anything).Return("+1", nil)
	env.OnActivity(RunReviewer, mock.Anything, mock.Anything).Return(ReviewResult{Verdict: "APPROVE", CommitSHA: "headsha", PRNumber: 101}, nil)
	env.OnActivity(PublishDeveloperChange, mock.Anything, mock.Anything).Return(PublishResult{PRNumber: 101}, nil)

	runAndAssert(t, env)
	if got := terminalState(t, env); got != stateBlocked {
		t.Fatalf("child failure must block parent, got %q", got)
	}
}

// §50/§49: after a successful merge+close, an enabled repo fires PostMergeWorkflow
// exactly once (and the release failure can never change the terminal state).
func TestWorkflowSmallPostMergeRelease(t *testing.T) {
	resetMetrics()
	env := mockCycleEnv(t, EstimateResult{Complexity: 3, Confidence: "high"})
	mockSmallPath(env)
	releases := 0
	env.OnActivity(RunRelease, mock.Anything, mock.Anything).Return(
		func(context.Context, ReleaseRequest) (ReleaseResult, error) {
			releases++
			return ReleaseResult{Tag: "v1.0.1", Bump: bumpPatch}, nil
		})

	in := IssueInput{Owner: "o", Repo: "r", IssueNum: testIssue, RepoPath: t.TempDir(),
		Lifecycle: LifecycleConfig{Mode: modeSupervisedAutonomous, DevelopmentMaxComplexity: 4,
			Release: ReleaseConfig{Enabled: true}}}
	env.ExecuteWorkflow(IssueCycleWorkflow, in)
	if env.IsWorkflowCompleted() {
		if err := env.GetWorkflowError(); err != nil {
			t.Fatalf("workflow error: %v", err)
		}
	}
	if got := terminalState(t, env); got != stateClosed {
		t.Fatalf("release path terminal = %q, want CLOSED", got)
	}
	if releases != 1 {
		t.Fatalf("release activity ran %d times, want 1", releases)
	}
}
