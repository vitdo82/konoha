package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func ticket(key string, deps ...string) SplitTicket {
	return SplitTicket{
		Key: key, Title: "child-" + key, Complexity: 2,
		Dependencies:       deps,
		Requirements:       []string{"r"},
		AcceptanceCriteria: []string{"c"},
	}
}

// §10: dependencies produce ordered, dependency-respecting batches; which is
// alone does not constrain the order of its independence.
func TestScheduleChildBatchesOrdering(t *testing.T) {
	tickets := []SplitTicket{
		ticket("c1"),
		ticket("c2", "c1"), // depends on c1
		ticket("c3", "c2"), // depends on c2
	}
	batches, err := scheduleChildBatches(tickets, 10)
	if err != nil {
		t.Fatal(err)
	}
	// linear chain: [c1] [c2] [c3]
	want := [][]string{{"c1"}, {"c2"}, {"c3"}}
	if fmt.Sprint(batches) != fmt.Sprint(want) {
		t.Fatalf("chain batches = %v, want %v", batches, want)
	}

	// diamond: a and b independent, both before c.
	tickets = []SplitTicket{ticket("c", "a", "b"), ticket("a"), ticket("b")}
	batches, err = scheduleChildBatches(tickets, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(batches) != 2 {
		t.Fatalf("diamond should have 2 levels, got %v", batches)
	}
	if len(batches[0]) != 2 || batches[1][0] != "c" {
		t.Fatalf("diamond batch[0] must be {a,b} and batch[1] {c}, got %v", batches)
	}
}

// §43: a level wider than the cap is broken into cap-sized batches; the whole
// batch list respects dependencies either way.
func TestScheduleChildBatchesCap(t *testing.T) {
	tickets := []SplitTicket{ticket("a"), ticket("b"), ticket("c"), ticket("d"), ticket("e")}
	batches, err := scheduleChildBatches(tickets, 2)
	if err != nil {
		t.Fatal(err)
	}
	want := [][]string{{"a", "b"}, {"c", "d"}, {"e"}}
	if fmt.Sprint(batches) != fmt.Sprint(want) {
		t.Fatalf("cap-2 batches = %v, want %v", batches, want)
	}

	// every batch must be ≤ cap.
	for _, b := range batches {
		if len(b) > 2 {
			t.Fatalf("batch %v exceeds cap", b)
		}
	}
	// deterministic ordering: same input → same output.
	again, _ := scheduleChildBatches(tickets, 2)
	if fmt.Sprint(batches) != fmt.Sprint(again) {
		t.Fatal("scheduling must be deterministic")
	}
}

// §10/§36: a dependency cycle or a dependency on an unknown key fails closed —
// the workflow must not blindly fan out an invalid DAG, and never falls into
// an infinite loop.
func TestScheduleChildBatchesFailClosed(t *testing.T) {
	cycle := []SplitTicket{ticket("a", "b"), ticket("b", "a")}
	if _, err := scheduleChildBatches(cycle, 2); err == nil {
		t.Fatal("dependency cycle must be rejected")
	}
	selfDep := []SplitTicket{ticket("a", "a")}
	if _, err := scheduleChildBatches(selfDep, 2); err == nil {
		t.Fatal("self-dependency must be rejected (cycle)")
	}
	missing := []SplitTicket{ticket("a", "ghost")}
	if _, err := scheduleChildBatches(missing, 2); err == nil {
		t.Fatal("dependency on unknown key must be rejected")
	}
	dups := []SplitTicket{ticket("a"), ticket("a")}
	if _, err := scheduleChildBatches(dups, 2); err == nil {
		t.Fatal("duplicate keys must be rejected by the scheduler")
	}
}

// §9/§24: EnsureChildren creates one GitHub issue per ticket with the
// deterministic parent+child markers and managed+child labels, and is
// idempotent under retry (markers found → no duplicate create).
func TestEnsureChildrenIdempotent(t *testing.T) {
	ctx := context.Background()
	tickets := []SplitTicket{ticket("01"), ticket("02", "01")}
	prev := ghRunner
	creates := 0
	existing := []ghItem{}
	ghRunner = func(_ context.Context, args ...string) ([]byte, error) {
		if args[0] == "api" {
			return json.Marshal(existing)
		}
		if args[0] == "issue" && args[1] == "create" {
			key := ""
			for i, a := range args {
				if a == "--title" && i+1 < len(args) {
					key = strings.TrimPrefix(args[i+1], "child-")
				}
			}
			num := 100 + len(existing) + 1
			body := childIssueBody(ParentRef{Owner: "o", Repo: "r", Num: 7, Key: key}, "b")
			existing = append(existing, ghItem{Number: num, State: "open", Body: body})
			creates++
			return []byte(fmt.Sprintf("https://github.com/o/r/issues/%d", num)), nil
		}
		return nil, fmt.Errorf("unexpected gh call: %v", args)
	}
	defer func() { ghRunner = prev }()

	out, err := EnsureChildren(ctx, ChildCreateRequest{Owner: "o", Repo: "r", ParentNum: 7, Tickets: tickets})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 || creates != 2 {
		t.Fatalf("first pass: %d children created=%d, want 2 children/2 creates", len(out), creates)
	}
	if out[0].ParentIssueNumber != 7 || out[0].Key != "01" || out[0].GitHubIssueNumber == 0 {
		t.Fatalf("child identity wrong: %+v", out[0])
	}

	// retry (duplicate sweep): no new creates, same identities returned.
	out2, err := EnsureChildren(ctx, ChildCreateRequest{Owner: "o", Repo: "r", ParentNum: 7, Tickets: tickets})
	if err != nil {
		t.Fatal(err)
	}
	if creates != 2 {
		t.Fatalf("second pass created %d more, want 0 (idempotent)", creates-2)
	}
	for i := range out {
		if out2[i].GitHubIssueNumber != out[i].GitHubIssueNumber || out2[i].Key != out[i].Key {
			t.Fatalf("retry identity mismatch: %+v vs %+v", out[i], out2[i])
		}
	}
}

// §8/§21: child issues carry konoha:managed + konoha:child.
func TestEnsureChildrenAddsLabels(t *testing.T) {
	ctx := context.Background()
	prev := ghRunner
	var labelArgs []string
	ghRunner = func(_ context.Context, args ...string) ([]byte, error) {
		if args[0] == "api" {
			return json.Marshal([]ghItem{})
		}
		if args[0] == "issue" && args[1] == "create" {
			labelArgs = append(labelArgs, args...)
			return []byte("https://github.com/o/r/issues/5"), nil
		}
		return nil, fmt.Errorf("unexpected gh call: %v", args)
	}
	defer func() { ghRunner = prev }()

	if _, err := EnsureChildren(ctx, ChildCreateRequest{
		Owner: "o", Repo: "r", ParentNum: 1, Tickets: []SplitTicket{ticket("01")},
	}); err != nil {
		t.Fatal(err)
	}
	var labels []string
	for i, a := range labelArgs {
		if a == "--label" && i+1 < len(labelArgs) {
			labels = append(labels, labelArgs[i+1])
		}
	}
	if !containsString(labels, labelManaged) || !containsString(labels, labelChild) {
		t.Fatalf("child must be created with %s + %s, got labels %v", labelManaged, labelChild, labels)
	}
	if len(labels) != 2 {
		t.Fatalf("expected exactly managed+child, got %v", labels)
	}
}

// §9: the child body carries the deterministic markers and the ticket's spec.
func TestChildTicketBody(t *testing.T) {
	p := ParentRef{Owner: "o", Repo: "r", Num: 7, Key: "02"}
	tk := ticket("02")
	body := childIssueBody(p, childTicketBody(tk))
	if _, ok := parseParentMarker(body); !ok {
		t.Fatal("child body must carry the parent marker (§9)")
	}
	if k, ok := parseChildMarker(body); !ok || k != "02" {
		t.Fatalf("child marker wrong: %q ok=%v", k, ok)
	}
	if !strings.Contains(body, "## Requirements") || !strings.Contains(body, "## Acceptance criteria") {
		t.Fatal("child body must carry the ticket's spec (requirements + acceptance criteria)")
	}
}
