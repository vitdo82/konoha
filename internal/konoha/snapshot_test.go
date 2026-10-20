package konoha

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// #141/AC1: the release checkpoint reads the issue's state + labels in one
// read-only call, so the workflow can stop before publishing or merging.
// FetchIssueLabels is left untouched: widening a registered activity's result
// type would break replay of in-flight executions.
func TestFetchIssueSnapshot(t *testing.T) {
	prev := ghRunner
	ghRunner = func(_ context.Context, args ...string) ([]byte, error) {
		if len(args) >= 2 && args[1] == "repos/o/r/issues/7" {
			return json.Marshal(ghItem{
				Number: 7,
				State:  "closed",
				Labels: []ghLabel{{Name: labelManaged}, {Name: "duplicate"}},
			})
		}
		return nil, fmt.Errorf("unexpected gh call: %v", args)
	}
	defer func() { ghRunner = prev }()

	snap, err := FetchIssueSnapshot(context.Background(), IssueInput{Owner: "o", Repo: "r", IssueNum: 7})
	if err != nil {
		t.Fatal(err)
	}
	if snap.State != "closed" {
		t.Fatalf("State = %q, want closed", snap.State)
	}
	if len(snap.Labels) != 2 || snap.Labels[1] != "duplicate" {
		t.Fatalf("Labels = %v", snap.Labels)
	}
	if !releasedClaim(snap.State, snap.Labels, ReconcileConfig{}) {
		t.Fatal("a closed issue must release the claim")
	}
}

// §36/§40: a payload without a state is unparseable evidence, so the activity
// fails closed rather than letting the checkpoint read "" as "still open".
func TestFetchIssueSnapshotRejectsMissingState(t *testing.T) {
	prev := ghRunner
	ghRunner = func(_ context.Context, args ...string) ([]byte, error) {
		return json.Marshal(ghItem{Number: 7})
	}
	defer func() { ghRunner = prev }()

	if _, err := FetchIssueSnapshot(context.Background(), IssueInput{Owner: "o", Repo: "r", IssueNum: 7}); err == nil {
		t.Fatal("an empty issue state must fail closed")
	}
}

// #141: a cancelled child is a decided stop, not a failure — the sweep must not
// escalate it into a BLOCKED record on an issue a human already closed.
func TestChildCanceled(t *testing.T) {
	if childCanceled(nil) {
		t.Fatal("nil is not a cancellation")
	}
	if !childCanceled(workflow.ErrCanceled) {
		t.Fatal("workflow.ErrCanceled must be recognized")
	}
	if !childCanceled(temporal.NewCanceledError("stopped")) {
		t.Fatal("a Temporal CanceledError must be recognized")
	}
	if childCanceled(errors.New("dev failed")) {
		t.Fatal("an ordinary failure is not a cancellation")
	}
}
