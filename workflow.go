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