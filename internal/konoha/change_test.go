package konoha

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/mock"
)

// readChange loads change.json for one (repo, issue) from a root.
func readChange(t *testing.T, root, repo string, issue int) Change {
	t.Helper()
	p := filepath.Join(root, "changes", fmt.Sprintf("%s-issue-%d.json", repo, issue))
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	var c Change
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatalf("change must decode: %v", err)
	}
	return c
}

// #97/AC: the change status is derived from the §6 state — CLOSED→DONE,
// CANCELLED→ABANDONED, a human-decision wait→IN_REVIEW, else OPEN.
func TestDeriveChangeStatus(t *testing.T) {
	cases := map[string]string{
		stateClosed:          changeStatusDone,
		stateCancelled:       changeStatusAbandoned,
		statePendingApproval: changeStatusInReview,
		stateHumanRequired:   changeStatusInReview,
		stateDeveloping:      changeStatusOpen,
		stateBlocked:         changeStatusOpen,
		stateFailed:          changeStatusOpen,
		"":                   changeStatusOpen,
	}
	for state, want := range cases {
		if got := deriveChangeStatus(state); got != want {
			t.Fatalf("deriveChangeStatus(%q) = %q, want %q", state, got, want)
		}
	}
}

// #97/AC/§24: feedback entries upsert by Key (no duplicate revisions), order is
// preserved across revisions, and the rework note is carried forward.
func TestRecordChangeThreadIdempotent(t *testing.T) {
	root := withEvidenceDir(t)
	ctx := context.Background()

	rev1 := reviewFeedback("wf", 1, ReviewResult{
		Verdict: "REQUEST_CHANGES", Blocking: []string{"b1", "b2"}, Nits: []string{"n1"},
	})
	for i := 0; i < 3; i++ { // retry must not duplicate
		if err := RecordChange(ctx, ChangeRequest{
			Owner: "o", Repo: "r", Issue: 7, WorkflowID: "wf",
			Feedback: rev1, ReworkNote: "b1\nb2",
		}); err != nil {
			t.Fatalf("RecordChange: %v", err)
		}
	}
	c := readChange(t, root, "r", 7)
	if len(c.Feedback) != 3 {
		t.Fatalf("want 3 feedback entries after retries, got %d: %+v", len(c.Feedback), c.Feedback)
	}
	if c.ReworkNote != "b1\nb2" {
		t.Fatalf("rework note = %q, want carried b1\\nb2", c.ReworkNote)
	}

	// A second revision appends, preserving the ordered thread.
	rev2 := reviewFeedback("wf", 2, ReviewResult{Verdict: "REQUEST_CHANGES", Blocking: []string{"still"}})
	if err := RecordChange(ctx, ChangeRequest{
		Owner: "o", Repo: "r", Issue: 7, WorkflowID: "wf",
		Feedback: rev2, ReworkNote: "still",
	}); err != nil {
		t.Fatalf("RecordChange: %v", err)
	}
	c = readChange(t, root, "r", 7)
	if len(c.Feedback) != 4 {
		t.Fatalf("want 4 feedback entries across 2 revisions, got %d: %+v", len(c.Feedback), c.Feedback)
	}
	if c.Feedback[0].Key != rev1[0].Key || c.Feedback[3].Key != rev2[0].Key {
		t.Fatalf("thread order not preserved: %+v", c.Feedback)
	}
	if c.ReworkNote != "still" {
		t.Fatalf("rework note = %q, want latest 'still'", c.ReworkNote)
	}
}

// #97/AC: a terminal state derives the change status; recording is idempotent by
// (repo, issue).
func TestRecordChangeStatusDerived(t *testing.T) {
	root := withEvidenceDir(t)
	ctx := context.Background()
	base := ChangeRequest{Owner: "o", Repo: "r", Issue: 7, WorkflowID: "wf", State: stateClosed}
	for i := 0; i < 2; i++ {
		if err := RecordChange(ctx, base); err != nil {
			t.Fatalf("RecordChange: %v", err)
		}
	}
	c := readChange(t, root, "r", 7)
	if c.Status != changeStatusDone {
		t.Fatalf("status = %q, want DONE", c.Status)
	}
	if c.Repo != "o/r" || c.Issue != 7 || c.WorkflowID != "wf" {
		t.Fatalf("identity = %+v", c)
	}
}

// #97/§23: a registered secret in a rework note or feedback body is redacted.
func TestRecordChangeRedacts(t *testing.T) {
	root := withEvidenceDir(t)
	secret := "registered-change-secret-99"
	RegisterSecrets(secret)
	err := RecordChange(context.Background(), ChangeRequest{
		Owner: "o", Repo: "r", Issue: 7, WorkflowID: "wf", ReworkNote: "please fix " + secret,
		Feedback: []ChangeFeedback{{Key: "k", Kind: feedbackKindReworkNote, Body: secret}},
	})
	if err != nil {
		t.Fatalf("RecordChange: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(root, "changes", "r-issue-7.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), secret) {
		t.Fatalf("secret leaked into change record: %s", b)
	}
	if !strings.Contains(string(b), redacted) {
		t.Fatalf("expected %s in change record, got %s", redacted, b)
	}
}

// #97/§42: DryRun namespaces the change record under dryrun/ and writes nothing
// outside it.
func TestRecordChangeDryRunNamespaces(t *testing.T) {
	root := withEvidenceDir(t)
	if err := RecordChange(context.Background(), ChangeRequest{
		Owner: "o", Repo: "r", Issue: 7, WorkflowID: "wf", State: stateClosed, DryRun: true,
	}); err != nil {
		t.Fatalf("RecordChange: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "dryrun", "changes", "r-issue-7.json")); err != nil {
		t.Fatalf("dry-run change missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "changes", "r-issue-7.json")); err == nil {
		t.Fatal("dry-run must not write the non-dryrun change path")
	}
}

// #97/AC: the describe projection joins change + lineage with recomputed totals,
// and is pure (no Temporal).
func TestBuildChangeView(t *testing.T) {
	c := Change{
		Repo: "o/r", Issue: 7, Status: changeStatusDone, WorkflowID: "wf", ReworkNote: "fix",
		Feedback: []ChangeFeedback{{Key: "k", Kind: feedbackKindReworkNote, Body: "fix"}},
	}
	l := Lineage{
		Repo: "o/r", Issue: 7,
		Activations: []LineageActivation{{Attempt: 1, WorkflowID: "wf"}, {Attempt: 2, WorkflowID: "wf-repair-2"}},
		Attempts:    2, TotalTokens: 42, TotalCostUsd: 1.5,
	}
	v := buildChangeView(c, l)
	if v.Status != changeStatusDone || v.ReworkNote != "fix" || v.Attempts != 2 ||
		v.TotalTokens != 42 || v.TotalCostUsd != 1.5 || len(v.Activations) != 2 {
		t.Fatalf("view = %+v", v)
	}
	out, err := changeViewJSON(v)
	if err != nil {
		t.Fatalf("changeViewJSON: %v", err)
	}
	var back ChangeView
	if err := json.Unmarshal([]byte(out), &back); err != nil {
		t.Fatalf("view JSON must decode: %v", err)
	}
	if back.Repo != "o/r" || back.Attempts != 2 {
		t.Fatalf("round-tripped view = %+v", back)
	}
}

// #97: get changes lists newest first (pure sort).
func TestSortChangesNewestFirst(t *testing.T) {
	list := []Change{{Issue: 1}, {Issue: 3}, {Issue: 2}}
	sortChangesNewestFirst(list)
	if list[0].Issue != 3 || list[1].Issue != 2 || list[2].Issue != 1 {
		t.Fatalf("order = %+v, want 3,2,1", list)
	}
	out, err := changesJSON(nil)
	if err != nil || strings.TrimSpace(out) != "[]" {
		t.Fatalf("empty list must print []: %q err=%v", out, err)
	}
}

// #97/AC: a closed cycle records a DONE change; a rejected review records the
// full ordered feedback thread + a rework note, and the repair activation
// carries the note.
func TestWorkflowChangeLifecycleDevelopRepair(t *testing.T) {
	resetMetrics()
	root := withEvidenceDir(t)
	env := mockCycleEnv(t, EstimateResult{Complexity: 3, Confidence: "high"})
	env.RegisterActivity(RecordChange)       // real change writer
	env.RegisterActivity(RecordActivation)   // real lineage writer
	env.RegisterActivity(WriteCycleEvidence) // real outcome writer

	env.OnActivity(FetchIssueLabels, mock.Anything, mock.Anything).Return([]string{"bug"}, nil)
	env.OnActivity(RunDeveloper, mock.Anything, mock.Anything).Return(DevelopResult{
		WorktreeDir: "/tmp/wt", BaseSHA: "base", CommitSHA: "devsha", Summary: "c", Validated: true,
		Usage: HarnessUsage{TokensIn: 10, TokensOut: 5, CostUSD: 0.25},
	}, nil)
	env.OnActivity(PublishDeveloperChange, mock.Anything, mock.Anything).Return(PublishResult{
		Branch: "konoha/issue-7", PRNumber: 101, Validated: true,
	}, nil)
	env.OnActivity(FetchPRHead, mock.Anything, mock.Anything).Return("headsha", nil)
	env.OnActivity(FetchPRDiff, mock.Anything, mock.Anything).Return("+1 line", nil)
	calls := 0
	env.OnActivity(RunReviewer, mock.Anything, mock.Anything).Return(func(_ context.Context, _ ReviewRequest) (ReviewResult, error) {
		calls++
		if calls == 1 {
			return ReviewResult{Verdict: "REQUEST_CHANGES", CommitSHA: "headsha", PRNumber: 101,
				Blocking: []string{"gap"}, Nits: []string{"style"}}, nil
		}
		return ReviewResult{Verdict: "APPROVE", CommitSHA: "headsha", PRNumber: 101}, nil
	})
	env.OnActivity(RunRepair, mock.Anything, mock.Anything).Return(DevelopResult{
		WorktreeDir: "/tmp/wt2", BaseSHA: "base", CommitSHA: "repairsha", Summary: "fixed", Validated: true,
		Usage: HarnessUsage{TokensIn: 1, TokensOut: 1, CostUSD: 0.5},
	}, nil)
	env.OnActivity(WaitForCI, mock.Anything, mock.Anything).Return(CIStatus{Green: true}, nil)
	env.OnActivity(MergePullRequest, mock.Anything, mock.Anything).Return(MergeResult{Merged: true}, nil)
	env.OnActivity(EnsureIssueState, mock.Anything, mock.Anything).Return(nil)

	runAndAssert(t, env)

	c := readChange(t, root, "r", testIssue)
	if c.Status != changeStatusDone {
		t.Fatalf("change status = %q, want DONE after a closed cycle", c.Status)
	}
	if c.ReworkNote != "gap" {
		t.Fatalf("rework note = %q, want the rejected review's blocking 'gap'", c.ReworkNote)
	}
	// rev1 blocking+nit, rev2 (approve, no findings) → 2 entries, ordered.
	if len(c.Feedback) != 2 || c.Feedback[0].Body != "gap" || c.Feedback[1].Body != "style" {
		t.Fatalf("feedback thread = %+v, want ordered [gap, style]", c.Feedback)
	}

	l := readLineage(t, root, "o", "r", testIssue)
	if len(l.Activations) != 2 || l.Activations[1].ReworkNote != "gap" {
		t.Fatalf("repair activation must carry the rework note: %+v", l.Activations)
	}

	// The pure describe projection joins both records.
	view := buildChangeView(c, l)
	if view.Status != changeStatusDone || view.Attempts != 2 || view.TotalTokens != 17 {
		t.Fatalf("view = %+v, want DONE / 2 attempts / 17 tokens", view)
	}
}
