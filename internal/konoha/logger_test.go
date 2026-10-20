package konoha

import (
	"context"
	"testing"
)

// #212: the local runtime calls activities in-process with a non-Temporal
// context. activity.GetLogger panics there; the helper must fall back instead.
func TestActivityLoggerWorksOutsideActivity(t *testing.T) {
	activityLogger(context.Background()).Info("outside", "k", "v")

	// A real activity path that logs before its dryRun early return: proves the
	// in-process call no longer panics.
	if err := SubmitReview(context.Background(), PrInput{Lifecycle: LifecycleConfig{DryRun: true}}, ReviewResult{}); err != nil {
		t.Fatalf("dryRun SubmitReview: %v", err)
	}
}
