package main

import (
	"fmt"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

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

	// deterministic routing policy (§11): complexity 1-4 → SMALL, else LARGE
	if est.Complexity >= 1 && est.Complexity <= 4 {
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

	for _, in := range pending {
		cwo := workflow.ChildWorkflowOptions{
			WorkflowID: fmt.Sprintf("%s-issue-%d", scope.Repo, in.IssueNum), // repo-specific, stable
		}
		cctx := workflow.WithChildOptions(ctx, cwo)
		if err := workflow.ExecuteChildWorkflow(cctx, IssueCycleWorkflow, in).Get(cctx, nil); err != nil {
			workflow.GetLogger(ctx).Error("child cycle failed, skipping", "issue", in.IssueNum, "err", err)
			continue
		}
		workflow.GetLogger(ctx).Info("cycle done", "issue", in.IssueNum)
	}
	return nil
}
