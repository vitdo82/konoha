package konoha

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/testsuite"
)

// §56: the marker key is a stable content hash — same item text → same id, an
// edit to the raw item → a different id (so a re-groom does not collide).
func TestGroomIdentityStableAndContentBound(t *testing.T) {
	a := groomIdentity("o", "r", 1, "title", "body")
	if b := groomIdentity("o", "r", 1, "title", "body"); a != b {
		t.Fatalf("identity not stable: %q != %q", a, b)
	}
	if c := groomIdentity("o", "r", 1, "title", "edited"); c == a {
		t.Fatalf("edited body must change identity, got %q", c)
	}
	if d := groomIdentity("o", "r", 2, "title", "body"); d == a {
		t.Fatalf("different source number must change identity, got %q", d)
	}
}

// groomFakeGh simulates the issues-list + issue-create calls of EnsureReadyIssue.
func groomFakeGh(t *testing.T, existing []ghItem, created int) *[]string {
	t.Helper()
	prev := ghRunner
	var calls []string
	ghRunner = func(_ context.Context, args ...string) ([]byte, error) {
		line := strings.Join(args, " ")
		calls = append(calls, line)
		switch {
		case strings.Contains(line, "issues?state=all"):
			return json.Marshal(existing)
		case strings.HasPrefix(line, "issue create"):
			return []byte(fmt.Sprintf("https://github.com/o/r/issues/%d", created)), nil
		default:
			return nil, fmt.Errorf("unexpected gh call: %s", line)
		}
	}
	t.Cleanup(func() { ghRunner = prev })
	return &calls
}

func groomReadyReq() GroomReadyRequest {
	return GroomReadyRequest{
		Owner: "o", Repo: "r",
		Source:     RawBacklogItem{Number: 1, Title: "Add foo", Body: "raw body"},
		Analysis:   IssueAnalysis{Title: "Add foo", Summary: "s", Requirements: []string{"do foo"}, AcceptanceCriteria: []string{"foo works end to end"}, Spec: specFallback},
		Estimate:   EstimateResult{Complexity: 3, Confidence: "high"},
		ReadyLabel: labelReady,
	}
}

// §56/§24: a second run finds the marker and never creates a duplicate ready
// issue; the first run creates exactly one carrying the marker.
func TestEnsureReadyIssueIdempotent(t *testing.T) {
	req := groomReadyReq()
	calls := groomFakeGh(t, nil, 42)

	first, err := EnsureReadyIssue(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Created || first.Number != 42 {
		t.Fatalf("first run = %+v, want created #42", first)
	}
	created := strings.Join(createdIssueCalls(*calls), " ")
	if !strings.Contains(created, "konoha-groomed: ") || !strings.Contains(created, labelReady) {
		t.Fatalf("create call missing marker/label: %s", created)
	}
	if n := len(createdIssueCalls(*calls)); n != 1 {
		t.Fatalf("first run must create exactly one issue, got %d", n)
	}

	// existing ready issue carrying the marker → matched, no create
	id := groomIdentity("o", "r", req.Source.Number, req.Source.Title, req.Source.Body)
	*calls = nil
	existing := []ghItem{{Number: 42, Body: "intro\n" + groomMarker(id) + "\nrest", State: "open"}}
	prev := ghRunner
	ghRunner = func(_ context.Context, args ...string) ([]byte, error) {
		line := strings.Join(args, " ")
		*calls = append(*calls, line)
		if strings.Contains(line, "issues?state=all") {
			return json.Marshal(existing)
		}
		return nil, fmt.Errorf("must not create: %s", line)
	}
	t.Cleanup(func() { ghRunner = prev })

	second, err := EnsureReadyIssue(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if second.Created || second.Number != 42 {
		t.Fatalf("second run = %+v, want matched #42 (no create)", second)
	}
}

// §42: dry-run files nothing.
func TestEnsureReadyIssueDryRunNoop(t *testing.T) {
	prev := ghRunner
	ghRunner = func(context.Context, ...string) ([]byte, error) {
		t.Fatal("dry-run must not call gh")
		return nil, nil
	}
	t.Cleanup(func() { ghRunner = prev })

	res, err := EnsureReadyIssue(context.Background(), GroomReadyRequest{DryRun: true})
	if err != nil || res.Number != 0 || res.Created {
		t.Fatalf("dry-run = %+v err=%v, want no-op", res, err)
	}
}

// groomNoteFakeGh simulates the issue-view + issue-comment calls of RecordGroomNote.
func groomNoteFakeGh(t *testing.T, comments string) *[]string {
	t.Helper()
	prev := ghRunner
	var calls []string
	ghRunner = func(_ context.Context, args ...string) ([]byte, error) {
		line := strings.Join(args, " ")
		calls = append(calls, line)
		switch {
		case strings.Contains(line, "issue view"):
			return json.Marshal(map[string]any{"comments": []map[string]string{{"body": comments}}})
		case strings.HasPrefix(line, "issue comment"):
			return []byte("{}"), nil
		default:
			return nil, fmt.Errorf("unexpected gh call: %s", line)
		}
	}
	t.Cleanup(func() { ghRunner = prev })
	return &calls
}

// §56/§24: the reject note is idempotent (marker-guarded) and never closes or
// edits the raw item — only a comment is added.
func TestRecordGroomNoteIdempotent(t *testing.T) {
	req := GroomNoteRequest{Owner: "o", Repo: "r", SourceNumber: 5, SourceTitle: "raw", SourceBody: "b", Reason: "unactionable"}

	calls := groomNoteFakeGh(t, "human chatter")
	if err := RecordGroomNote(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if n := len(*calls); n != 2 { // view + comment
		t.Fatalf("want view+comment, got %v", *calls)
	}
	joined := strings.Join(*calls, "\n")
	if !strings.Contains(joined, "issue comment") || !strings.Contains(joined, "konoha-groom-rejected: ") {
		t.Fatalf("missing idempotent note comment: %s", joined)
	}
	if strings.Contains(joined, "issue close") || strings.Contains(joined, "issue edit") {
		t.Fatalf("raw item must never be closed/edited: %s", joined)
	}

	// marker already present → no second comment
	id := groomIdentity("o", "r", 5, "raw", "b")
	calls = groomNoteFakeGh(t, groomRejectMarker(id))
	if err := RecordGroomNote(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(*calls, "\n"), "issue comment") {
		t.Fatalf("existing note must not be re-posted: %v", *calls)
	}
}

// §42: dry-run leaves no note.
func TestRecordGroomNoteDryRunNoop(t *testing.T) {
	prev := ghRunner
	ghRunner = func(context.Context, ...string) ([]byte, error) {
		t.Fatal("dry-run must not call gh")
		return nil, nil
	}
	t.Cleanup(func() { ghRunner = prev })

	if err := RecordGroomNote(context.Background(), GroomNoteRequest{DryRun: true}); err != nil {
		t.Fatal(err)
	}
}

// §56: ListBacklogItems passes the selector label and parses the issue list.
func TestListBacklogItems(t *testing.T) {
	prev := ghRunner
	var got string
	ghRunner = func(_ context.Context, args ...string) ([]byte, error) {
		got = strings.Join(args, " ")
		return json.Marshal([]RawBacklogItem{{Number: 3, Title: "t", Body: "b"}})
	}
	t.Cleanup(func() { ghRunner = prev })

	items, err := ListBacklogItems(context.Background(), GroomListRequest{Owner: "o", Repo: "r", Selector: "backlog"})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Number != 3 {
		t.Fatalf("unexpected items: %+v", items)
	}
	if !strings.Contains(got, "--label backlog") {
		t.Fatalf("selector label not passed: %s", got)
	}
}

// §56/§56-AC: disabled grooming never lists.
func TestBacklogGroomWorkflowDisabledNoop(t *testing.T) {
	listed := 0
	env := (&testsuite.WorkflowTestSuite{}).NewTestWorkflowEnvironment()
	env.OnActivity(ListBacklogItems, mock.Anything, mock.Anything).Return(func(context.Context, GroomListRequest) ([]RawBacklogItem, error) {
		listed++
		return nil, nil
	})
	env.ExecuteWorkflow(BacklogGroomWorkflow, RepoScope{Owner: "o", Repo: "r"})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	if listed != 0 {
		t.Fatalf("disabled grooming listed %d times, want 0", listed)
	}
}

// §56: actionable items become ready issues; an unactionable item gets a note,
// never a ready issue (and never a failure of the whole run).
func TestBacklogGroomWorkflowGroomsAndRejects(t *testing.T) {
	var readyFor []int
	var noted []int
	var outcome GroomOutcome

	env := (&testsuite.WorkflowTestSuite{}).NewTestWorkflowEnvironment()
	env.OnActivity(ListBacklogItems, mock.Anything, mock.Anything).Return([]RawBacklogItem{
		{Number: 1, Title: "ok", Body: "b"},
		{Number: 2, Title: "bad", Body: "b"},
	}, nil)
	env.OnActivity(EnsureRepoLabels, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(AnalyzeIssue, mock.Anything, mock.Anything).Return(func(_ context.Context, in IssueInput) (IssueAnalysis, error) {
		if in.IssueNum == 2 {
			return IssueAnalysis{}, fmt.Errorf("analysis invalid")
		}
		return IssueAnalysis{Title: "ok", Summary: "s", Requirements: []string{"r"}, AcceptanceCriteria: []string{"a b c"}, Spec: specFallback}, nil
	})
	env.OnActivity(EstimateIssue, mock.Anything, mock.Anything, mock.Anything).Return(EstimateResult{Complexity: 3, Confidence: "high"}, nil)
	env.OnActivity(EnsureReadyIssue, mock.Anything, mock.Anything).Return(func(_ context.Context, in GroomReadyRequest) (GroomReadyResult, error) {
		readyFor = append(readyFor, in.Source.Number)
		return GroomReadyResult{Number: 10, Created: true}, nil
	})
	env.OnActivity(RecordGroomNote, mock.Anything, mock.Anything).Return(func(_ context.Context, in GroomNoteRequest) error {
		noted = append(noted, in.SourceNumber)
		return nil
	})
	env.OnActivity(RecordGroomMetrics, mock.Anything, mock.Anything).Return(func(_ context.Context, o GroomOutcome) error {
		outcome = o
		return nil
	})

	in := RepoScope{Owner: "o", Repo: "r", Lifecycle: LifecycleConfig{Grooming: GroomingConfig{Enabled: true}}}
	env.ExecuteWorkflow(BacklogGroomWorkflow, in)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	if len(readyFor) != 1 || readyFor[0] != 1 {
		t.Fatalf("ready issues = %v, want only source #1", readyFor)
	}
	if len(noted) != 1 || noted[0] != 2 {
		t.Fatalf("notes = %v, want only source #2", noted)
	}
	if outcome.Groomed != 1 || outcome.Rejected != 1 {
		t.Fatalf("outcome = %+v, want groomed=1 rejected=1", outcome)
	}
}

// §56/§42: DryRun still analyses but the mutating activities receive DryRun=true
// and no ready issue is created.
func TestBacklogGroomWorkflowDryRun(t *testing.T) {
	var readyDryRun, noteDryRun *bool
	env := (&testsuite.WorkflowTestSuite{}).NewTestWorkflowEnvironment()
	env.OnActivity(ListBacklogItems, mock.Anything, mock.Anything).Return([]RawBacklogItem{
		{Number: 1, Title: "ok", Body: "b"},
		{Number: 2, Title: "bad", Body: "b"},
	}, nil)
	env.OnActivity(EnsureRepoLabels, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(AnalyzeIssue, mock.Anything, mock.Anything).Return(func(_ context.Context, in IssueInput) (IssueAnalysis, error) {
		if in.IssueNum == 2 {
			return IssueAnalysis{}, fmt.Errorf("analysis invalid")
		}
		return IssueAnalysis{Title: "ok", Requirements: []string{"r"}, AcceptanceCriteria: []string{"a b c"}, Spec: specFallback}, nil
	})
	env.OnActivity(EstimateIssue, mock.Anything, mock.Anything, mock.Anything).Return(EstimateResult{Complexity: 3, Confidence: "high"}, nil)
	env.OnActivity(EnsureReadyIssue, mock.Anything, mock.Anything).Return(func(_ context.Context, in GroomReadyRequest) (GroomReadyResult, error) {
		b := in.DryRun
		readyDryRun = &b
		return GroomReadyResult{}, nil
	})
	env.OnActivity(RecordGroomNote, mock.Anything, mock.Anything).Return(func(_ context.Context, in GroomNoteRequest) error {
		b := in.DryRun
		noteDryRun = &b
		return nil
	})
	env.OnActivity(RecordGroomMetrics, mock.Anything, mock.Anything).Return(nil)

	in := RepoScope{Owner: "o", Repo: "r", Lifecycle: LifecycleConfig{
		DryRun:   true,
		Grooming: GroomingConfig{Enabled: true},
	}}
	env.ExecuteWorkflow(BacklogGroomWorkflow, in)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	if readyDryRun == nil || !*readyDryRun {
		t.Fatalf("EnsureReadyIssue must receive DryRun=true, got %v", readyDryRun)
	}
	if noteDryRun == nil || !*noteDryRun {
		t.Fatalf("RecordGroomNote must receive DryRun=true, got %v", noteDryRun)
	}
}

// §56: the reject-rate gauge is rejected / processed and the counters accumulate.
func TestGroomMetrics(t *testing.T) {
	resetMetrics()
	defer resetMetrics()

	if err := RecordGroomMetrics(bg(), GroomOutcome{Groomed: 3, Rejected: 1}); err != nil {
		t.Fatal(err)
	}
	if got := sink.get(mGroomed); got != 3 {
		t.Fatalf("groomed_count = %v, want 3", got)
	}
	if got := sink.get(mGroomRejected); got != 1 {
		t.Fatalf("groom_rejected_count = %v, want 1", got)
	}
	if got := sink.get(mGroomRejectRate); got != 0.25 {
		t.Fatalf("groom_reject_rate = %v, want 0.25", got)
	}
	if !strings.Contains(MetricsText(), mGroomRejectRate) {
		t.Fatalf("stable metric %q missing from export", mGroomRejectRate)
	}
}
