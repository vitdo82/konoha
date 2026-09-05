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
