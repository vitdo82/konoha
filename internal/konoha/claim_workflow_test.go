package konoha

import (
	"context"
	"testing"

	"github.com/stretchr/testify/mock"
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
			if calls == 1 {
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
