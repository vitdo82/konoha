package konoha

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
)

// §34 / §41 distributed-failure property, exercised via the ghRunner seam:
//   mutation succeeds -> activity response lost -> Temporal retries
//   -> same object returned, no duplicate created.

func TestMarkersRoundtrip(t *testing.T) {
	p := ParentRef{Owner: "vitdo82", Repo: "konoha", Num: 123, Key: "02"}
	body := childIssueBody(p, "do the thing")

	got, ok := parseParentMarker(body)
	if !ok || got.Owner != p.Owner || got.Repo != p.Repo || got.Num != p.Num {
		t.Fatalf("parseParentMarker: got %+v ok=%v, want parent %s/%s#%d", got, ok, p.Owner, p.Repo, p.Num)
	}
	k, ok := parseChildMarker(body)
	if !ok || k != "02" {
		t.Fatalf("parseChildMarker: got %q ok=%v", k, ok)
	}
	if _, ok := parseParentMarker("no marker here"); ok {
		t.Fatal("parseParentMarker should fail on missing marker")
	}
}

func TestFindChildIssueSkipsPRs(t *testing.T) {
	p := ParentRef{Owner: "o", Repo: "r", Num: 1, Key: "01"}
	pr := ghItem{Number: 8, Body: childIssueBody(p, "x")}
	pr.PullRequest = json.RawMessage(`{}`)
	if n := findChildIssue([]ghItem{pr}, p); n != 0 {
		t.Fatalf("PR body matched as issue: got %d want 0", n)
	}
	is := ghItem{Number: 8, Body: childIssueBody(p, "x")}
	if n := findChildIssue([]ghItem{is}, p); n != 8 {
		t.Fatalf("issue with marker not found: got %d want 8", n)
	}
}

func TestParseIssueNumber(t *testing.T) {
	for _, in := range []string{"https://github.com/o/r/issues/42", "42\n", "https://github.com/o/r/pull/7"} {
		if n, err := parseIssueNumber(in); err != nil || n == 0 {
			t.Fatalf("parseIssueNumber(%q): %d %v", in, n, err)
		}
	}
	if _, err := parseIssueNumber("not a number"); err == nil {
		t.Fatal("parseIssueNumber should error on garbage")
	}
}

func TestEnsureIssueRetryNoDuplicate(t *testing.T) {
	ctx := context.Background()
	p := ParentRef{Owner: "vitdo82", Repo: "konoha", Num: 4, Key: "01"}
	req := IssueRequest{Owner: "vitdo82", Repo: "konoha", Title: "t", Body: "b",
		Labels: []string{"konoha:child"}, Parent: &p}

	const createdNum = 42
	creates := 0
	prev := ghRunner
	ghRunner = func(_ context.Context, args ...string) ([]byte, error) {
		switch {
		case args[0] == "api":
			if creates == 0 {
				return json.Marshal([]ghItem{})
			}
			return json.Marshal([]ghItem{{Number: createdNum, State: "open", Body: childIssueBody(p, "b")}})
		case args[0] == "issue" && args[1] == "create":
			creates++
			return []byte("https://github.com/o/r/issues/" + fmt.Sprint(createdNum)), nil
		}
		return nil, fmt.Errorf("unexpected gh call: %v", args)
	}
	defer func() { ghRunner = prev }()

	n1, err := EnsureIssue(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	n2, err := EnsureIssue(ctx, req) // response lost => retry
	if err != nil {
		t.Fatal(err)
	}
	if n1 != n2 || n2 != createdNum {
		t.Fatalf("EnsureIssue retry: got %d and %d, want %d both times", n1, n2, createdNum)
	}
	if creates != 1 {
		t.Fatalf("issue create called %d times, want 1", creates)
	}
}

// §31 reconciliation: a human already closed/labeled the issue, so the
// Ensure* activity must absorb the external state and mutate nothing.
func TestEnsureIssueStateReconciles(t *testing.T) {
	ctx := context.Background()
	in := StateRequest{Owner: "vitdo82", Repo: "konoha", Num: 4, State: "closed"}
	patches := 0
	prev := ghRunner
	ghRunner = func(_ context.Context, args ...string) ([]byte, error) {
		switch {
		case args[0] == "api" && args[1] == "--method":
			patches++
			return []byte(`{}`), nil
		case args[0] == "api":
			return json.Marshal(ghItem{Number: in.Num, State: "closed"})
		}
		return nil, fmt.Errorf("unexpected gh call: %v", args)
	}
	defer func() { ghRunner = prev }()

	if err := EnsureIssueState(ctx, in); err != nil {
		t.Fatal(err)
	}
	if patches != 0 {
		t.Fatalf("PATCH called %d times, want 0 (human already closed)", patches)
	}

	in.State = "open"
	if err := EnsureIssueState(ctx, in); err != nil {
		t.Fatal(err)
	}
	if patches != 1 {
		t.Fatalf("PATCH called %d times, want 1 (reconcile to open)", patches)
	}
}

func TestEnsureIssueLabelsReconciles(t *testing.T) {
	ctx := context.Background()
	in := LabelsRequest{Owner: "vitdo82", Repo: "konoha", Num: 4, Labels: []string{"konoha:managed"}}
	edits := 0
	prev := ghRunner
	ghRunner = func(_ context.Context, args ...string) ([]byte, error) {
		switch args[0] {
		case "api":
			return json.Marshal(ghItem{Number: in.Num, Labels: []ghLabel{{Name: "konoha:managed"}}})
		case "issue":
			edits++
			return nil, nil
		}
		return nil, fmt.Errorf("unexpected gh call: %v", args)
	}
	defer func() { ghRunner = prev }()

	if err := EnsureIssueLabels(ctx, in); err != nil {
		t.Fatal(err)
	}
	if edits != 0 {
		t.Fatalf("label edit called %d times, want 0 (already labeled)", edits)
	}

	in.Labels = []string{"konoha:managed", "konoha:child"}
	if err := EnsureIssueLabels(ctx, in); err != nil {
		t.Fatal(err)
	}
	if edits != 1 {
		t.Fatalf("label edit called %d times, want 1 (one missing)", edits)
	}
}

// §42 dry-run: every Ensure* mutation must no-op without touching GitHub.
func TestEnsureDryRunSuppressesMutations(t *testing.T) {
	ctx := context.Background()
	calls := 0
	prev := ghRunner
	ghRunner = func(_ context.Context, args ...string) ([]byte, error) {
		calls++
		return nil, fmt.Errorf("dryRun must not invoke gh: %v", args)
	}
	defer func() { ghRunner = prev }()

	if n, err := EnsureIssue(ctx, IssueRequest{Owner: "o", Repo: "r", DryRun: true}); err != nil || n != 0 {
		t.Fatalf("EnsureIssue dryRun: n=%d err=%v", n, err)
	}
	if err := EnsureIssueState(ctx, StateRequest{Owner: "o", Repo: "r", Num: 1, State: "closed", DryRun: true}); err != nil {
		t.Fatalf("EnsureIssueState dryRun: %v", err)
	}
	if err := EnsureIssueLabels(ctx, LabelsRequest{Owner: "o", Repo: "r", Num: 1, DryRun: true}); err != nil {
		t.Fatalf("EnsureIssueLabels dryRun: %v", err)
	}
	if calls != 0 {
		t.Fatalf("dryRun triggered %d gh call(s), want 0", calls)
	}
}
