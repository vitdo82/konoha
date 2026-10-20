package konoha

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/testsuite"
)

// #141/AC1: a cycle whose issue was closed by a human stops before publish —
// no PR is opened and the terminal state is CANCELLED, never success.
func TestWorkflowClaimReleasedStopsBeforePublish(t *testing.T) {
	resetMetrics()
	env := newCycleEnv(t, EstimateResult{Complexity: 3, Confidence: "high"})
	mockCleanup(env)
	env.OnActivity(FetchIssueSnapshot, mock.Anything, mock.Anything).Return(
		IssueSnapshot{State: "closed"}, nil)
	env.OnActivity(RunDeveloper, mock.Anything, mock.Anything).Return(DevelopResult{
		WorktreeDir: "/tmp/wt", BaseSHA: "base", CommitSHA: "devsha", Summary: "c", Validated: true,
	}, nil)

	runAndAssert(t, env)

	if state := terminalState(t, env); state != stateCancelled {
		t.Fatalf("terminal state = %q, want %q", state, stateCancelled)
	}
	env.AssertNotCalled(t, "PublishDeveloperChange", mock.Anything, mock.Anything)
}

// #141/AC1: an issue closed while the cycle is in review stops before the merge
// mutation — the already-opened PR is left for a human, never merged.
func TestWorkflowClaimReleasedStopsBeforeMerge(t *testing.T) {
	resetMetrics()
	env := newCycleEnv(t, EstimateResult{Complexity: 3, Confidence: "high"})
	mockCleanup(env)
	calls := 0
	env.OnActivity(FetchIssueSnapshot, mock.Anything, mock.Anything).Return(
		func(_ context.Context, _ IssueInput) (IssueSnapshot, error) {
			calls++
			// 1 = pre-publish, 2 = post-approval-gate, 3 = pre-merge.
			if calls < 3 {
				return IssueSnapshot{State: "open", Labels: []string{labelManaged}}, nil
			}
			return IssueSnapshot{State: "closed"}, nil
		})
	env.OnActivity(RunDeveloper, mock.Anything, mock.Anything).Return(DevelopResult{
		WorktreeDir: "/tmp/wt", BaseSHA: "base", CommitSHA: "devsha", Summary: "c", Validated: true,
	}, nil)
	env.OnActivity(PublishDeveloperChange, mock.Anything, mock.Anything).Return(PublishResult{
		Branch: "konoha/issue-7", PRNumber: 101, Validated: true,
	}, nil)
	env.OnActivity(FetchPRHead, mock.Anything, mock.Anything).Return("headsha", nil)
	env.OnActivity(FetchIssueLabels, mock.Anything, mock.Anything).Return([]string{labelManaged}, nil)
	env.OnActivity(FetchPRDiff, mock.Anything, mock.Anything).Return("+1 line", nil)
	env.OnActivity(RunReviewer, mock.Anything, mock.Anything).Return(
		ReviewResult{Verdict: "APPROVE", CommitSHA: "headsha", PRNumber: 101}, nil)
	env.OnActivity(WaitForCI, mock.Anything, mock.Anything).Return(CIStatus{Green: true}, nil)

	runAndAssert(t, env)

	if state := terminalState(t, env); state != stateCancelled {
		t.Fatalf("terminal state = %q, want %q", state, stateCancelled)
	}
	env.AssertNotCalled(t, "MergePullRequest", mock.Anything, mock.Anything)
}

// #141/AC1: releasing via a label (not just a close) stops the run at the same
// checkpoint — the predicate is wired, not only unit-tested.
func TestWorkflowClaimReleaseLabelStopsBeforePublish(t *testing.T) {
	resetMetrics()
	env := newCycleEnv(t, EstimateResult{Complexity: 3, Confidence: "high"})
	mockCleanup(env)
	env.OnActivity(FetchIssueSnapshot, mock.Anything, mock.Anything).Return(
		IssueSnapshot{State: "open", Labels: []string{labelManaged, "duplicate"}}, nil)
	env.OnActivity(RunDeveloper, mock.Anything, mock.Anything).Return(DevelopResult{
		WorktreeDir: "/tmp/wt", BaseSHA: "base", CommitSHA: "devsha", Summary: "c", Validated: true,
	}, nil)

	runAndAssert(t, env)

	if state := terminalState(t, env); state != stateCancelled {
		t.Fatalf("terminal state = %q, want %q", state, stateCancelled)
	}
	env.AssertNotCalled(t, "PublishDeveloperChange", mock.Anything, mock.Anything)
}

// §40: an unreadable claim is never read as "still open". The cycle fails rather
// than proceeding to a mutation it cannot justify.
func TestWorkflowClaimFetchErrorFailsClosed(t *testing.T) {
	resetMetrics()
	env := newCycleEnv(t, EstimateResult{Complexity: 3, Confidence: "high"})
	mockCleanup(env)
	env.OnActivity(FetchIssueSnapshot, mock.Anything, mock.Anything).Return(
		IssueSnapshot{}, errors.New("gh api timeout"))
	env.OnActivity(RunDeveloper, mock.Anything, mock.Anything).Return(DevelopResult{
		WorktreeDir: "/tmp/wt", BaseSHA: "base", CommitSHA: "devsha", Summary: "c", Validated: true,
	}, nil)

	in := IssueInput{Owner: "o", Repo: "r", IssueNum: testIssue, RepoPath: t.TempDir(),
		Lifecycle: LifecycleConfig{Mode: modeSupervisedAutonomous, DevelopmentMaxComplexity: 4}}
	env.ExecuteWorkflow(IssueCycleWorkflow, in)

	if env.GetWorkflowError() == nil {
		t.Fatal("an unreadable claim must fail the cycle, not proceed")
	}
	env.AssertNotCalled(t, "PublishDeveloperChange", mock.Anything, mock.Anything)
}

// #141/AC1: the LARGE path must not file child issues for a released claim.
func TestWorkflowLargeClaimReleasedSkipsChildren(t *testing.T) {
	resetMetrics()
	env := (&testsuite.WorkflowTestSuite{}).NewTestWorkflowEnvironment()
	env.RegisterActivity(RecordCycleMetrics)
	mockCleanup(env)
	env.OnActivity(AnalyzeIssue, mock.Anything, mock.Anything).Return(IssueAnalysis{
		Title: "t", Body: "b", Summary: "s", Spec: specFallback,
		Requirements: []string{"r1"}, AcceptanceCriteria: []string{"Given c When d Then e"},
	}, nil)
	env.OnActivity(EstimateIssue, mock.Anything, mock.Anything, mock.Anything).Return(
		EstimateResult{Complexity: 7, Confidence: "medium"}, nil)
	env.OnActivity(RunSplit, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(SplitResult{
		Tickets: []SplitTicket{{Key: "01", Title: "child-a", Complexity: 2, Requirements: []string{"r"}, AcceptanceCriteria: []string{"c"}}},
	}, nil)
	env.OnActivity(FetchIssueSnapshot, mock.Anything, mock.Anything).Return(
		IssueSnapshot{State: "closed"}, nil)

	runAndAssert(t, env)

	if state := terminalState(t, env); state != stateCancelled {
		t.Fatalf("terminal state = %q, want %q", state, stateCancelled)
	}
	env.AssertNotCalled(t, "EnsureChildren", mock.Anything, mock.Anything)
}
