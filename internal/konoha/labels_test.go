package konoha

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// §22/§44 #20: ProjectLabels is a pure function of (state, fact) → labels;
// Temporal state drives it. Exactly one complexity label appears, and transient
// labels appear only while their state is active.
func TestProjectLabels(t *testing.T) {
	lc := LifecycleConfig{}

	// small issue after estimate: managed + complexity-3, no transient.
	got := ProjectLabels(lc, stateSmall, 3, true)
	if !equalStrings(got, []string{labelManaged, "konoha:complexity-3"}) {
		t.Fatalf("SMALL(3) projection = %v", got)
	}

	// review: managed + complexity-3 + review (transient).
	got = ProjectLabels(lc, stateReviewing, 3, true)
	if !equalStrings(got, []string{labelManaged, "konoha:complexity-3", labelReview}) {
		t.Fatalf("REVIEWING projection = %v", got)
	}

	// changes-requested: managed + complexity-3 + changes-requested; review dropped.
	got = ProjectLabels(lc, stateChangesRequested, 3, true)
	if equalStrings(got, []string{labelManaged, "konoha:complexity-3", labelReview}) {
		t.Fatal("CHANGES_REQUESTED must not retain the stale review transient")
	}
	if !equalStrings(got, []string{labelManaged, "konoha:complexity-3", labelChangesRequested}) {
		t.Fatalf("CHANGES_REQUESTED projection = %v", got)
	}

	// blocked: managed + complexity-3 + human-required + blocked.
	got = ProjectLabels(lc, stateBlocked, 3, true)
	if !equalStrings(got, []string{labelManaged, "konoha:complexity-3", labelHumanRequired, labelBlocked}) {
		t.Fatalf("BLOCKED projection = %v", got)
	}

	// human-required (not blocked): human-required without blocked.
	got = ProjectLabels(lc, stateHumanRequired, 3, true)
	if containsString(got, labelBlocked) {
		t.Fatalf("HUMAN_REQUIRED must not carry the blocked label: %v", got)
	}

	// large: complexity>4 adds estimate-large but keeps exactly one complexity.
	got = ProjectLabels(lc, stateLarge, 7, true)
	if !equalStrings(got, []string{labelManaged, "konoha:complexity-7", labelEstimateLarge}) {
		t.Fatalf("LARGE(7) projection = %v", got)
	}
	// large with a low complexity (routing said large) has no estimate-large.
	got = ProjectLabels(lc, stateLarge, 3, true)
	if containsString(got, labelEstimateLarge) {
		t.Fatalf("LARGE(3) must not carry estimate-large: %v", got)
	}

	// terminal states.
	if !equalStrings(ProjectLabels(lc, stateFailed, 0, true), []string{labelManaged, labelFailed}) {
		t.Fatalf("FAILED projection = %v", ProjectLabels(lc, stateFailed, 0, true))
	}
	if !equalStrings(ProjectLabels(lc, stateCancelled, 0, true), []string{labelManaged, labelCancelled}) {
		t.Fatalf("CANCELLED projection = %v", ProjectLabels(lc, stateCancelled, 0, true))
	}

	// manage=false → no labels (un-managed discovery).
	if got := ProjectLabels(lc, stateDiscovered, 0, false); len(got) != 0 {
		t.Fatalf("manage=false must project nothing, got %v", got)
	}
}

// §22: exactly one complexity label is guaranteed by the projection itself and
// by the reconciler dropping old complexity labels.
func TestProjectLabelsSingleComplexity(t *testing.T) {
	for _, c := range []int{1, 4, 5, 10} {
		got := ProjectLabels(LifecycleConfig{}, stateSmall, c, true)
		n := 0
		for _, l := range got {
			if strings.HasPrefix(l, "konoha:complexity-") {
				n++
			}
		}
		if n != 1 {
			t.Fatalf("complexity %d: expected exactly one complexity label, got %v", c, got)
		}
	}
}

// §21: all labels are konoha:-namespaced and the complexity scale is 1-10.
func TestLabelNamingAndScale(t *testing.T) {
	for _, l := range projectableLabels() {
		if !strings.HasPrefix(l, "konoha:") {
			t.Fatalf("label %q must be namespaced konoha:*", l)
		}
	}
	if len(complexityLabels) != 10 {
		t.Fatalf("complexity scale must be 1-10, got %d", len(complexityLabels))
	}
	if complexityLabel(0) != "" || complexityLabel(11) != "" {
		t.Fatal("complexityLabel out-of-range must be empty")
	}
}

// §24/§31: ApplyIssueLabels diffs against the current labels — it adds missing
// desired, drops stale transient projectable labels, and leaves human labels.
func TestApplyIssueLabelsReconciles(t *testing.T) {
	ctx := context.Background()
	prev := ghRunner
	var ran struct{ add, remove string }
	ghRunner = func(_ context.Context, args ...string) ([]byte, error) {
		if len(args) >= 2 && args[1] == "repos/o/r/issues/7" {
			// issue currently has: a stale review transient, an old complexity,
			// and a human-added "security" label.
			return json.Marshal(ghItem{Number: 7, Labels: []ghLabel{
				{Name: labelReview}, {Name: "konoha:complexity-5"}, {Name: "security"},
			}})
		}
		if args[0] == "issue" && args[1] == "edit" {
			ran.add, ran.remove = "", ""
			for i, a := range args {
				if a == "--add-label" {
					ran.add += args[i+1] + ","
				}
				if a == "--remove-label" {
					ran.remove += args[i+1] + ","
				}
			}
			return []byte(`{}`), nil
		}
		return nil, fmt.Errorf("unexpected gh call: %v", args)
	}
	defer func() { ghRunner = prev }()

	// move to new state: complexity-3, no review transient.
	want := ProjectLabels(LifecycleConfig{}, stateChangesRequested, 3, true)
	if err := ApplyIssueLabels(ctx, LabelsRequest{Owner: "o", Repo: "r", Num: 7, Labels: want}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ran.add, labelChangesRequested) || !strings.Contains(ran.add, "konoha:complexity-3") {
		t.Fatalf("expected adds, got add=%q", ran.add)
	}
	if !strings.Contains(ran.remove, labelReview) || !strings.Contains(ran.remove, "konoha:complexity-5") {
		t.Fatalf("expected stale transient + old complexity removed, got remove=%q", ran.remove)
	}
	if strings.Contains(ran.remove, "security") {
		t.Fatal("human-added security label must NOT be removed")
	}
}

// Idempotent under retry: when current labels already equal the desired set and
// no projectable stale label exists, no mutation is issued.
func TestApplyIssueLabelsNoopWhenIdentical(t *testing.T) {
	ctx := context.Background()
	prev := ghRunner
	called := false
	ghRunner = func(_ context.Context, args ...string) ([]byte, error) {
		if len(args) >= 2 && args[1] == "repos/o/r/issues/9" {
			return json.Marshal(ghItem{Number: 9, Labels: []ghLabel{
				{Name: labelManaged}, {Name: "konoha:complexity-2"},
			}})
		}
		called = true
		return nil, fmt.Errorf("unexpected mutation: %v", args)
	}
	defer func() { ghRunner = prev }()

	want := ProjectLabels(LifecycleConfig{}, stateSmall, 2, true)
	if err := ApplyIssueLabels(ctx, LabelsRequest{Owner: "o", Repo: "r", Num: 9, Labels: want}); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("no mutation should be issued when labels already match")
	}
}

// §22 + §24: EnsureRepoLabels creates only the missing labels from the §21 set.
func TestEnsureRepoLabelsCreatesMissing(t *testing.T) {
	ctx := context.Background()
	prev := ghRunner
	var created []string
	ghRunner = func(_ context.Context, args ...string) ([]byte, error) {
		if args[0] == "label" && args[1] == "list" {
			return json.Marshal([]struct{ Name string }{{Name: labelManaged}, {Name: labelReview}})
		}
		if args[0] == "label" && args[1] == "create" {
			created = append(created, args[2])
			return []byte(`{}`), nil
		}
		return nil, fmt.Errorf("unexpected gh call: %v", args)
	}
	defer func() { ghRunner = prev }()

	if err := EnsureRepoLabels(ctx, LabelsTheirsRequest{Owner: "o", Repo: "r", Labels: projectableLabels()}); err != nil {
		t.Fatal(err)
	}
	for _, have := range []string{labelManaged, labelReview} {
		if containsString(created, have) {
			t.Fatalf("existing label %q must not be recreated", have)
		}
	}
	want := len(projectableLabels()) - 2
	if len(created) != want {
		t.Fatalf("created %d labels, want %d (all except the two existing)", len(created), want)
	}
}

// --- §22/§47: the konoha:managed ownership claim comment --------------------

type managedNoteCalls struct {
	lines  []string
	bodies []string
}

// managedNoteFakeGh simulates the issue-view + issue-comment calls of
// EnsureManagedNote, with the given existing comment body.
func managedNoteFakeGh(t *testing.T, comments string) *managedNoteCalls {
	t.Helper()
	prev := ghRunner
	c := &managedNoteCalls{}
	ghRunner = func(_ context.Context, args ...string) ([]byte, error) {
		line := strings.Join(args, " ")
		c.lines = append(c.lines, line)
		switch {
		case strings.Contains(line, "issue view"):
			return json.Marshal(map[string]any{"comments": []map[string]string{{"body": comments}}})
		case strings.HasPrefix(line, "issue comment"):
			for i, a := range args {
				if a == "--body" && i+1 < len(args) {
					c.bodies = append(c.bodies, args[i+1])
				}
			}
			return []byte("{}"), nil
		default:
			return nil, fmt.Errorf("unexpected gh call: %s", line)
		}
	}
	t.Cleanup(func() { ghRunner = prev })
	return c
}

func managedNoteReq() ManagedNoteRequest {
	return ManagedNoteRequest{
		Owner: "o", Repo: "r", IssueNum: 22,
		WorkflowID: "ducklambda-issue-22", Trigger: "sweep",
		Route: stateSmall, Complexity: 2, Model: defaultAgentModel,
	}
}

// §22: claiming an unmanaged issue posts one well-structured Markdown comment
// whose first line is the deterministic marker. Assert on structure, not just
// substrings.
func TestEnsureManagedNotePostsStructuredClaim(t *testing.T) {
	req := managedNoteReq()
	calls := managedNoteFakeGh(t, "human chatter")
	if err := EnsureManagedNote(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if len(calls.lines) != 2 || len(calls.bodies) != 1 {
		t.Fatalf("want view+comment, got lines=%v bodies=%v", calls.lines, calls.bodies)
	}
	body := calls.bodies[0]
	lines := strings.Split(body, "\n")
	if lines[0] != managedMarker(req.WorkflowID) {
		t.Fatalf("first line = %q, want marker %q", lines[0], managedMarker(req.WorkflowID))
	}
	if n := strings.Count(body, "### Konoha took ownership of this issue"); n != 1 {
		t.Fatalf("heading must appear exactly once, got %d:\n%s", n, body)
	}
	for _, want := range []string{
		"**Repo:**", "**Workflow / run id:**", "**Route / estimated complexity:**",
		"**Agent model:**", "**How to intervene:**",
		"`o/r`", "`ducklambda-issue-22`", "`opencode-go/deepseek-v4.1-flash`",
		"`sweep`", "`SMALL`", "`konoha:complexity-2`", "konoha-decision",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q:\n%s", want, body)
		}
	}
}

// §22/§47: hostile run-id/model/trigger values are sanitized so they cannot
// break Markdown or inject content; the marker stays a single well-formed line.
func TestEnsureManagedNoteHostileInputSafe(t *testing.T) {
	req := managedNoteReq()
	req.WorkflowID = "r-issue-22`\n### PWNED <script>"
	req.Model = "model`\ninject <b>"
	req.Trigger = "we`bhook\n<x>"

	calls := managedNoteFakeGh(t, "")
	if err := EnsureManagedNote(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if len(calls.bodies) != 1 {
		t.Fatalf("want exactly one comment, got %v", calls.bodies)
	}
	body := calls.bodies[0]
	if strings.Contains(body, "<script>") || strings.Contains(body, "<b>") || strings.Contains(body, "<x>") {
		t.Fatalf("hostile angle-bracket payload leaked into the comment:\n%s", body)
	}
	if strings.Contains(body, "we`bhook") || strings.Contains(body, "model`") {
		t.Fatalf("raw backtick survived sanitization:\n%s", body)
	}
	for _, l := range strings.Split(body, "\n") {
		if strings.HasPrefix(l, "### PWNED") {
			t.Fatalf("injected heading survived (newline must collapse):\n%s", body)
		}
	}
	if strings.Count(body, "### Konoha took ownership of this issue") != 1 {
		t.Fatalf("injected heading changed structure:\n%s", body)
	}
	marker := managedMarker(req.WorkflowID)
	if strings.Contains(marker, "\n") || strings.TrimSpace(strings.Split(body, "\n")[0]) != marker {
		t.Fatalf("marker not a single well-formed line: %q", marker)
	}
}

// §22/§24: re-projecting labels / re-running the cycle finds the marker and
// never posts a second claim comment.
func TestEnsureManagedNoteIdempotent(t *testing.T) {
	req := managedNoteReq()
	calls := managedNoteFakeGh(t, managedMarker(req.WorkflowID))
	if err := EnsureManagedNote(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if len(calls.bodies) != 0 {
		t.Fatalf("existing claim note must not be re-posted: %v", calls.bodies)
	}
}

// §42: dry-run performs no gh call and posts nothing.
func TestEnsureManagedNoteDryRunNoop(t *testing.T) {
	prev := ghRunner
	ghRunner = func(context.Context, ...string) ([]byte, error) {
		t.Fatal("dry-run must not call gh")
		return nil, nil
	}
	t.Cleanup(func() { ghRunner = prev })

	req := managedNoteReq()
	req.DryRun = true
	if err := EnsureManagedNote(context.Background(), req); err != nil {
		t.Fatal(err)
	}
}

// §22: same facts → same body (marker dedupe reliability); an unknown route
// omits the complexity bullet rather than rendering a dangling value.
func TestManagedNoteBodyDeterministic(t *testing.T) {
	if a, b := managedNoteBody(managedNoteReq()), managedNoteBody(managedNoteReq()); a != b {
		t.Fatalf("body not deterministic:\n%q\n%q", a, b)
	}
	unknown := managedNoteReq()
	unknown.Route, unknown.Complexity = "", 0
	if strings.Contains(managedNoteBody(unknown), "Route / estimated complexity") {
		t.Fatalf("unknown route must omit the complexity bullet:\n%s", managedNoteBody(unknown))
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
