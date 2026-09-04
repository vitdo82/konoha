package main

import (
	"fmt"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// batchRuns splits n items into consecutive runs of at most max. §43: a sweep
// never has more than `max` child workflows in flight at once. max<1 → 1
// (serialized).
func batchRuns(n, max int) [][2]int {
	if max < 1 {
		max = 1
	}
	var out [][2]int
	for i := 0; i < n; i += max {
		j := i + max
		if j > n {
			j = n
		}
		out = append(out, [2]int{i, j})
	}
	return out
}

// PrReviewWorkflow: fetch diff -> opencode review -> post verdict back to PR.
func PrReviewWorkflow(ctx workflow.Context, in PrInput) error {
	ao := workflow.ActivityOptions{
		StartToCloseTimeout: 15 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			MaximumAttempts: 3,
		},
	}
	ctx = workflow.WithActivityOptions(ctx, ao)
	info := workflow.GetInfo(ctx)

	var diff string
	if err := workflow.ExecuteActivity(ctx, FetchDiff, in).Get(ctx, &diff); err != nil {
		return err
	}

	var result ReviewResult
	if err := workflow.ExecuteActivity(ctx, ReviewWithOpencode, in, diff).Get(ctx, &result); err != nil {
		return err
	}

	if err := workflow.ExecuteActivity(ctx, SubmitReview, in, result).Get(ctx, nil); err != nil {
		return err
	}

	workflow.GetLogger(ctx).Info("review done",
		"workflow", info.WorkflowExecution.ID, "verdict", result.Verdict)
	return nil
}

// ReviewAllWorkflow is cron-driven (Temporal Schedule): lists unreviewed open
// PRs in the given repo and reviews each as a child workflow with a stable,
// repo-specific ID (<repo>-review-<pr>). Reused across repos by creating one
// schedule per repo with a different RepoScope input. Best-effort per PR: a
// failure in one doesn't abort the rest; stable child ID prevents double review.
func ReviewAllWorkflow(ctx workflow.Context, scope RepoScope) error {
	ao := workflow.ActivityOptions{
		StartToCloseTimeout: 10 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			MaximumAttempts: 2,
		},
	}
	ctx = workflow.WithActivityOptions(ctx, ao)

	var pending []PrInput
	if err := workflow.ExecuteActivity(ctx, ListUnreviewedPRs, scope).Get(ctx, &pending); err != nil {
		return err
	}
	workflow.GetLogger(ctx).Info("ReviewAll sweep", "repo", scope.Owner+"/"+scope.Repo, "pendingPRs", len(pending))

	for _, in := range pending {
		cwo := workflow.ChildWorkflowOptions{
			WorkflowID: fmt.Sprintf("%s-review-%d", scope.Repo, in.PrNum), // repo-specific, stable
		}
		cctx := workflow.WithChildOptions(ctx, cwo)
		if err := workflow.ExecuteChildWorkflow(cctx, PrReviewWorkflow, in).Get(cctx, nil); err != nil {
			workflow.GetLogger(ctx).Error("child review failed, skipping", "pr", in.PrNum, "err", err)
			continue
		}
		workflow.GetLogger(ctx).Info("reviewed", "pr", in.PrNum)
	}
	return nil
}

// Issue lifecycle states (§6). Workflow history is the authoritative lifecycle
// record: each transition is logged and readable via the "state" query handler.
const (
	stateDiscovered = "DISCOVERED"
	stateAnalyzing  = "ANALYZING"
	stateEstimating = "ESTIMATING"
	stateSmall      = "SMALL"
	stateLarge      = "LARGE"
)

// IssueCycleWorkflow drives one issue through the lifecycle state machine
// (§6). Deterministic: all I/O lives in activities. Foundation scope:
// DISCOVERED → ANALYZING → ESTIMATING → SMALL|LARGE. The states after the
// routing branch (DEVELOPING/SPLITTING onward, ending in CLOSED/FAILED/
// CANCELLED/HUMAN_REQUIRED) are wired in by subsequent issues — until then the
// workflow ends cleanly at the branch decision. Since label-based idempotency
// is also a later issue, the next sweep may simply run the cycle again; the
// stable <repo>-issue-<n> ID guarantees only one run is active at a time.
func IssueCycleWorkflow(ctx workflow.Context, in IssueInput) error {
	ao := workflow.ActivityOptions{
		StartToCloseTimeout: 15 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			MaximumAttempts: 3,
		},
	}
	ctx = workflow.WithActivityOptions(ctx, ao)
	info := workflow.GetInfo(ctx)

	state := stateDiscovered
	if err := workflow.SetQueryHandler(ctx, "state", func() (string, error) { return state, nil }); err != nil {
		workflow.GetLogger(ctx).Error("set state query handler failed", "err", err)
	}
	transition := func(s string) {
		state = s
		workflow.GetLogger(ctx).Info("issue state",
			"workflow", info.WorkflowExecution.ID, "issue", in.IssueNum, "state", state)
	}

	transition(stateAnalyzing)
	var a IssueAnalysis
	if err := workflow.ExecuteActivity(ctx, AnalyzeIssue, in).Get(ctx, &a); err != nil {
		return err
	}

	transition(stateEstimating)
	var est EstimateResult
	if err := workflow.ExecuteActivity(ctx, EstimateIssue, in, a).Get(ctx, &est); err != nil {
		return err
	}

	// The estimator only reports; the deterministic policy routes (§4.2). Record
	// complexity + confidence in history (§38): confidence is data, never proof
	// that bypasses the routing below.
	workflow.GetLogger(ctx).Info("estimate recorded",
		"workflow", info.WorkflowExecution.ID, "issue", in.IssueNum,
		"complexity", est.Complexity, "confidence", est.Confidence)

	// Deterministic policy (§4.8): the autonomy §12 predicate decides whether
	// this issue can run autonomously. Touched paths/labels are unknown at this
	// stage, so only complexity/confidence gate it; restricted-path escalation
	// applies once the work touches files (§20 wiring lands with DEVELOPMENT).
	aut := EvaluateAutonomy(in.Lifecycle, est, nil, nil)
	workflow.GetLogger(ctx).Info("autonomy decision",
		"workflow", info.WorkflowExecution.ID, "issue", in.IssueNum,
		"autonomous", aut.Autonomous, "reasons", aut.Reasons)

	// deterministic routing policy (§11): complexity within the executable
	// threshold → SMALL, else LARGE. Threshold comes from config (§33), not a
	// hardcoded constant, so repo policy is the authority (§5).
	if est.Complexity >= 1 && est.Complexity <= DevelopmentMaxComplexity(in.Lifecycle) {
		transition(stateSmall)
	} else {
		transition(stateLarge)
	}
	return nil
}

// IssueAllWorkflow is the issue-sweep analog of ReviewAllWorkflow (§32): lists
// open issues missing the konoha:managed label and starts an
// IssueCycleWorkflow per issue as a child with a stable, repo-specific ID
// (<repo>-issue-<n>). Stable IDs make duplicate sweep starts harmless: a
// second start of an already-running issue fails with
// WorkflowExecutionAlreadyStarted and is skipped.
func IssueAllWorkflow(ctx workflow.Context, scope RepoScope) error {
	ao := workflow.ActivityOptions{
		StartToCloseTimeout: 10 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			MaximumAttempts: 2,
		},
	}
	ctx = workflow.WithActivityOptions(ctx, ao)

	var pending []IssueInput
	if err := workflow.ExecuteActivity(ctx, ListUnmanagedIssues, scope).Get(ctx, &pending); err != nil {
		return err
	}
	workflow.GetLogger(ctx).Info("IssueAll sweep", "repo", scope.Owner+"/"+scope.Repo, "pendingIssues", len(pending))

	// §43: never more than `maxConcurrentIssueCycles` child cycles in flight per
	// repo. Stable child IDs keep duplicate sweeps harmless either way.
	type run struct {
		in     IssueInput
		cctx   workflow.Context
		future workflow.ChildWorkflowFuture
	}
	for _, b := range batchRuns(len(pending), scope.Lifecycle.ConcurrentIssueCycles()) {
		var batch []run
		for _, in := range pending[b[0]:b[1]] {
			cwo := workflow.ChildWorkflowOptions{
				WorkflowID: fmt.Sprintf("%s-issue-%d", scope.Repo, in.IssueNum), // repo-specific, stable
			}
			cctx := workflow.WithChildOptions(ctx, cwo)
			batch = append(batch, run{in: in, cctx: cctx, future: workflow.ExecuteChildWorkflow(cctx, IssueCycleWorkflow, in)})
		}
		for _, x := range batch {
			if err := x.future.Get(x.cctx, nil); err != nil {
				workflow.GetLogger(ctx).Error("child cycle failed, skipping", "issue", x.in.IssueNum, "err", err)
				continue
			}
			workflow.GetLogger(ctx).Info("cycle done", "issue", x.in.IssueNum)
		}
	}
	return nil
}
