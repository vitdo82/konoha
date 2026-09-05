package konoha

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// GitHub label projection (§21/§22/§44 #20). Labels are a human-readable
// PROJECTION of the authoritative Temporal workflow state; stale transient
// labels are dropped on transition and exactly one complexity label is kept.
// All label mutations reconcile external edits (§31) and are idempotent under
// retry (§24).

// §21 label set, namespaced konoha:*. The projection code treats every label
// below as "projectable": any of them found on an issue but NOT in the desired
// set is removed (stale transient / old complexity), and the rest are left
// alone (human-added labels survive).
const (
	labelManaged          = "konoha:managed"
	labelFailed           = "konoha:failed"
	labelCancelled        = "konoha:cancelled"
	labelEstimatePending  = "konoha:estimate-pending"
	labelEstimateLarge    = "konoha:estimate-large"
	labelReview           = "konoha:review"
	labelChangesRequested = "konoha:changes-requested"
	labelNeedsSpec        = "konoha:needs-spec"
	labelChild            = "konoha:child"
)

// ComplexityLabels is the §21 complexity scale (only one present per issue,
// §22). complexityLabel returns the label for a rubric score, "" out of range.
var complexityLabels = [10]string{
	"konoha:complexity-1", "konoha:complexity-2", "konoha:complexity-3",
	"konoha:complexity-4", "konoha:complexity-5", "konoha:complexity-6",
	"konoha:complexity-7", "konoha:complexity-8", "konoha:complexity-9",
	"konoha:complexity-10",
}

func complexityLabel(complexity int) string {
	if complexity < 1 || complexity > len(complexityLabels) {
		return ""
	}
	return complexityLabels[complexity-1]
}

// projectableLabels returns the full §21 projectable set: lifecycle + pipeline +
// hierarchy + all complexity labels. Used to decide what to remove.
func projectableLabels() []string {
	out := []string{
		labelManaged, labelBlocked, labelFailed, labelCancelled, labelHumanRequired,
		labelEstimatePending, labelEstimateLarge, labelReview, labelChangesRequested,
		labelNeedsSpec, labelChild,
	}
	out = append(out, complexityLabels[:]...)
	return out
}

// ProjectLabels is the PURE state→label projection (§21/§22/§44 #20): it maps a
// workflow state plus observed facts (estimated complexity) to the label set the
// issue should carry. Deterministic, zero I/O. Exactly one complexity label
// appears (when complexity is known); transient labels appear only while their
// state is active, so the reconciler drops them once the state moves on.
// `manage` toggles the managed prefix (false leaves the caller free of the base
// label, e.g. for un-managed discovery).
func ProjectLabels(lc LifecycleConfig, state string, complexity int, manage bool) []string {
	if !manage {
		return nil
	}
	out := []string{labelManaged}
	switch state {
	case stateAnalyzing:
		out = append(out, labelEstimatePending)
	case stateEstimating:
		out = append(out, labelEstimatePending)
	case stateSmall, stateDeveloping:
		out = append(out, complexityLabel(complexity))
	case stateLarge, stateSplitting:
		out = append(out, complexityLabel(complexity))
		if complexity > DevelopmentMaxComplexity(lc) {
			out = append(out, labelEstimateLarge)
		}
	case stateReviewing:
		out = append(out, complexityLabel(complexity))
		out = append(out, labelReview)
	case stateChangesRequested:
		out = append(out, complexityLabel(complexity))
		out = append(out, labelChangesRequested)
	case stateHumanRequired, stateBlocked:
		out = append(out, complexityLabel(complexity))
		out = append(out, labelHumanRequired)
		if state == stateBlocked {
			out = append(out, labelBlocked)
		}
	case stateFailed:
		out = append(out, labelFailed)
	case stateCancelled:
		out = append(out, labelCancelled)
	}
	return dedupeLabels(out)
}

func dedupeLabels(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, l := range in {
		if l != "" && !seen[l] {
			seen[l] = true
			out = append(out, l)
		}
	}
	return out
}

// LabelsTheirsRequest declares the repo label set to create if missing (§21).
type LabelsTheirsRequest struct {
	Owner  string   `json:"owner"`
	Repo   string   `json:"repo"`
	Labels []string `json:"labels"`
	DryRun bool     `json:"dryRun,omitempty"`
}

// EnsureRepoLabels idempotently creates the configured labels (full §21 set)
// via the App: list existing, create only the missing ones. Retries re-list and
// no-op (§24). Uses ghRunner so tests can simulate GitHub.
func EnsureRepoLabels(ctx context.Context, in LabelsTheirsRequest) error {
	if in.DryRun {
		return nil
	}
	repo := in.Owner + "/" + in.Repo
	out, err := ghRunner(ctx, "label", "list", "-R", repo, "--json", "name")
	if err != nil {
		return fmt.Errorf("gh label list %s: %s: %w", repo, strings.TrimSpace(string(out)), err)
	}
	var existing []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(out, &existing); err != nil {
		return fmt.Errorf("parse labels %s: %w", repo, err)
	}
	have := map[string]bool{}
	for _, l := range existing {
		have[l.Name] = true
	}
	for _, l := range in.Labels {
		if have[l] {
			continue
		}
		if _, err := ghRunner(ctx, "label", "create", "-R", repo, l, "--color", "bfd4f2"); err != nil {
			return fmt.Errorf("gh label create %s %s: %s: %w", repo, l, strings.TrimSpace(string(out)), err)
		}
	}
	return nil
}

// ApplyIssueLabels reconciles an issue's labels to the desired projection
// (§21/§22/§24/§31): add missing, remove any projectable label NOT desired
// (drops stale transients like konoha:review after review, and old complexity
// labels so exactly one remains), and leave non-konoha human labels alone.
// Idempotent under retry: each attempt re-reads current labels and diffs.
func ApplyIssueLabels(ctx context.Context, in LabelsRequest) error {
	if in.DryRun {
		return nil
	}
	it, err := getIssue(ctx, in.Owner, in.Repo, in.Num)
	if err != nil {
		return err
	}
	have := map[string]bool{}
	var allHave []string
	for _, l := range it.Labels {
		have[l.Name] = true
		allHave = append(allHave, l.Name)
	}
	desired := map[string]bool{}
	for _, l := range in.Labels {
		desired[l] = true
	}

	var add, remove []string
	projectable := projectableLabels()
	for l := range desired {
		if !have[l] {
			add = append(add, l)
		}
	}
	for _, l := range allHave {
		// Remove projectable labels that are no longer desired (stale transients,
		// old complexity, estimate-large after the parent went back small...).
		if !desired[l] && containsString(projectable, l) {
			remove = append(remove, l)
		}
	}
	if len(add) == 0 && len(remove) == 0 {
		return nil
	}
	args := []string{"issue", "edit", fmt.Sprint(in.Num), "-R", in.Owner + "/" + in.Repo}
	for _, l := range add {
		args = append(args, "--add-label", l)
	}
	for _, l := range remove {
		args = append(args, "--remove-label", l)
	}
	if _, err := ghRunner(ctx, args...); err != nil {
		return fmt.Errorf("ApplyIssueLabels: %s/%s#%d (%s/+%v/-%v): %w",
			in.Owner, in.Repo, in.Num, in.Labels, add, remove, err)
	}
	return nil
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
