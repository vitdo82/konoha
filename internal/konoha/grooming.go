package konoha

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// §56 backlog grooming pre-stage. The lifecycle normally *reacts* to issues; a
// schedule-driven BacklogGroomWorkflow makes it *produce* a stream of ready
// tickets instead. It selects raw backlog items (config `grooming.selector`),
// runs the Issue Analyst + Estimator in DRY (analysis only, §42) to prove the
// item is actionable, and files one fully-specified ready issue per groomed
// item — idempotent by a deterministic marker (§24). An unactionable item is
// left in the backlog with an idempotent note, never silently closed (§40): the
// human keeps ownership of the source board.
//
// The ready issue carries no `konoha:managed` label, so the existing sweep /
// `opened` webhook drives it through the normal IssueCycleWorkflow — grooming
// never starts that cycle itself (§32/§52 pattern).
//
// All I/O lives in ListBacklogItems / EnsureReadyIssue / RecordGroomNote;
// BacklogGroomWorkflow is deterministic. Disabled/absent config is a no-op.

// GroomingConfig is the per-repo §56 policy (repos.yaml `lifecycle.grooming`).
type GroomingConfig struct {
	Enabled    bool   `yaml:"enabled" json:"enabled,omitempty"`
	Selector   string `yaml:"selector" json:"selector,omitempty"`     // label selecting raw items
	Schedule   string `yaml:"schedule" json:"schedule,omitempty"`     // per-repo cadence (Go duration)
	RepoLabel  string `yaml:"repoLabel" json:"repoLabel,omitempty"`   // optional extra label on the ready issue
	ReadyLabel string `yaml:"readyLabel" json:"readyLabel,omitempty"` // label marking a ready issue
}

const (
	defaultGroomingSchedule = "24h"
	defaultGroomingSelector = "backlog"
)

// validate gates the §56 grooming block: a configured schedule is a positive
// duration. Unknown keys fail at decode.
func (gc GroomingConfig) validate(prefix string) error {
	if gc.Schedule != "" {
		if d, err := time.ParseDuration(gc.Schedule); err != nil || d <= 0 {
			return fmt.Errorf("%s.schedule: %q is not a positive duration", prefix, gc.Schedule)
		}
	}
	return nil
}

// GroomingEnabled reports whether the repo opted into §56 backlog grooming.
func GroomingEnabled(lc LifecycleConfig) bool { return lc.Grooming.Enabled }

// GroomingSelector resolves the label selecting raw backlog items.
func GroomingSelector(lc LifecycleConfig) string {
	if s := strings.TrimSpace(lc.Grooming.Selector); s != "" {
		return s
	}
	return defaultGroomingSelector
}

// GroomingReadyLabel resolves the label applied to a created ready issue.
func GroomingReadyLabel(lc LifecycleConfig) string {
	if l := strings.TrimSpace(lc.Grooming.ReadyLabel); l != "" {
		return l
	}
	return labelReady
}

// GroomingRepoLabel resolves the optional extra provenance label on a ready issue.
func GroomingRepoLabel(lc LifecycleConfig) string {
	return strings.TrimSpace(lc.Grooming.RepoLabel)
}

// GroomingSchedule resolves the per-repo groom cadence (Go duration string).
func GroomingSchedule(lc LifecycleConfig) string {
	if s := strings.TrimSpace(lc.Grooming.Schedule); s != "" {
		return s
	}
	return defaultGroomingSchedule
}

// §24 idempotency markers. groomed marks a created ready issue (its source);
// rejected marks the idempotent note left on an unactionable raw item.
const (
	groomMarkerOpen       = "<!-- konoha-groomed: "
	groomRejectMarkerOpen = "<!-- konoha-groom-rejected: "
)

// groomIdentity is the deterministic marker key for a raw item: a content hash
// of the source issue. `sha` is stable across runs (same item text → same
// marker) and changes when the raw item is edited, so a re-groomed item yields
// a fresh marker instead of colliding with its stale ready issue.
func groomIdentity(owner, repo string, num int, title, body string) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%s/%s#%d\x00%s\x00%s", owner, repo, num, title, body)))
	return hex.EncodeToString(h[:])[:16]
}

func groomMarker(id string) string { return groomMarkerOpen + id + " -->" }
func groomRejectMarker(id string) string {
	return groomRejectMarkerOpen + id + " -->"
}

// RawBacklogItem is one selectable raw backlog issue (read-only §23).
type RawBacklogItem struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	Body   string `json:"body"`
}

// GroomListRequest parameterizes ListBacklogItems.
type GroomListRequest struct {
	Owner    string `json:"owner"`
	Repo     string `json:"repo"`
	Selector string `json:"selector"`
}

// ListBacklogItems reads the open issues carrying the selector label. A missing
// selector lists all open issues; a read error fails closed (never "no items").
func ListBacklogItems(ctx context.Context, in GroomListRequest) ([]RawBacklogItem, error) {
	args := []string{"issue", "list", "-R", in.Owner + "/" + in.Repo,
		"--state", "open", "--limit", "100", "--json", "number,title,body"}
	if s := strings.TrimSpace(in.Selector); s != "" {
		args = append(args, "--label", s)
	}
	out, err := ghRunner(ctx, args...)
	if err != nil {
		return nil, fmt.Errorf("list backlog issues %s/%s: %s: %w", in.Owner, in.Repo, strings.TrimSpace(string(out)), err)
	}
	var items []RawBacklogItem
	if err := json.Unmarshal(out, &items); err != nil {
		return nil, fmt.Errorf("parse backlog issues %s/%s: %w", in.Owner, in.Repo, err)
	}
	return items, nil
}

// GroomReadyRequest files one ready issue from a groomed raw item.
type GroomReadyRequest struct {
	Owner      string         `json:"owner"`
	Repo       string         `json:"repo"`
	Source     RawBacklogItem `json:"source"`
	Analysis   IssueAnalysis  `json:"analysis"`
	Estimate   EstimateResult `json:"estimate"`
	ReadyLabel string         `json:"readyLabel,omitempty"`
	RepoLabel  string         `json:"repoLabel,omitempty"`
	DryRun     bool           `json:"dryRun,omitempty"`
}

// GroomReadyResult reports the filed (or matched) ready issue. Number is 0 under
// DryRun.
type GroomReadyResult struct {
	Number  int    `json:"number,omitempty"`
	Created bool   `json:"created,omitempty"`
	Marker  string `json:"marker,omitempty"`
}

// EnsureReadyIssue creates a fully-specified ready issue, idempotent under retry
// (§24): it scans repo issues for the deterministic marker first and returns the
// existing number instead of creating a duplicate. The ready issue carries no
// `konoha:managed` label, so the normal sweep consumes it.
func EnsureReadyIssue(ctx context.Context, in GroomReadyRequest) (GroomReadyResult, error) {
	if in.DryRun {
		return GroomReadyResult{}, nil // §42: shadow mode mutates nothing
	}
	id := groomIdentity(in.Owner, in.Repo, in.Source.Number, in.Source.Title, in.Source.Body)
	marker := groomMarker(id)
	items, err := listIssues(ctx, in.Owner, in.Repo)
	if err != nil {
		return GroomReadyResult{}, err
	}
	if n := findMarkerIssue(items, marker); n != 0 {
		return GroomReadyResult{Number: n, Marker: marker}, nil
	}
	args := []string{"issue", "create", "-R", in.Owner + "/" + in.Repo,
		"--title", groomIssueTitle(in), "--body", groomIssueBody(in, marker)}
	for _, l := range dedupeLabels([]string{in.ReadyLabel, in.RepoLabel}) {
		args = append(args, "--label", l)
	}
	out, err := ghRunner(ctx, args...)
	if err != nil {
		return GroomReadyResult{}, fmt.Errorf("gh issue create (groomed): %s: %w", strings.TrimSpace(string(out)), err)
	}
	n, err := parseIssueNumber(string(out))
	if err != nil {
		return GroomReadyResult{}, err
	}
	return GroomReadyResult{Number: n, Created: true, Marker: marker}, nil
}

// groomIssueTitle uses the analyst's title (which echoes the source), falling
// back to the raw item's title.
func groomIssueTitle(in GroomReadyRequest) string {
	if t := strings.TrimSpace(in.Analysis.Title); t != "" {
		return t
	}
	return in.Source.Title
}

// groomIssueBody renders the machine-read analysis as a ready-ticket body with
// the idempotency marker first (§24).
func groomIssueBody(in GroomReadyRequest, marker string) string {
	var b strings.Builder
	b.WriteString(marker + "\n\n")
	fmt.Fprintf(&b, "Groomed from #%d by Konoha backlog grooming (§56). Raw item left untouched; the source board owns it.\n", in.Source.Number)

	if s := strings.TrimSpace(in.Analysis.Summary); s != "" {
		fmt.Fprintf(&b, "\n## Summary\n%s\n", s)
	}
	writeList(&b, "Requirements", in.Analysis.Requirements)
	writeList(&b, "Acceptance criteria", in.Analysis.AcceptanceCriteria)
	writeList(&b, "Constraints", in.Analysis.Constraints)
	writeList(&b, "Assumptions", in.Analysis.Assumptions)
	writeList(&b, "Open questions", in.Analysis.OpenQuestions)
	writeList(&b, "Risks", in.Analysis.Risks)

	if spec := strings.TrimSpace(in.Analysis.Spec); spec != "" {
		fmt.Fprintf(&b, "\n## Spec\n%s\n", spec)
	}
	fmt.Fprintf(&b, "\n## Estimate\ncomplexity %d/10, confidence %s\n", in.Estimate.Complexity, in.Estimate.Confidence)
	return b.String()
}

func writeList(b *strings.Builder, heading string, items []string) {
	if len(items) == 0 {
		return
	}
	fmt.Fprintf(b, "\n## %s\n", heading)
	for _, it := range items {
		fmt.Fprintf(b, "- %s\n", it)
	}
}

// GroomNoteRequest leaves one idempotent note on an unactionable raw item.
type GroomNoteRequest struct {
	Owner        string `json:"owner"`
	Repo         string `json:"repo"`
	SourceNumber int    `json:"sourceNumber"`
	SourceTitle  string `json:"sourceTitle"`
	SourceBody   string `json:"sourceBody"`
	Reason       string `json:"reason,omitempty"`
	DryRun       bool   `json:"dryRun,omitempty"`
}

// RecordGroomNote comments on the raw source explaining why it was not groomed,
// idempotent by the reject marker (a retry or re-run never spams the issue). The
// raw item is never closed or edited — the human keeps ownership (§56/§40).
func RecordGroomNote(ctx context.Context, in GroomNoteRequest) error {
	if in.DryRun {
		return nil // §42
	}
	id := groomIdentity(in.Owner, in.Repo, in.SourceNumber, in.SourceTitle, in.SourceBody)
	marker := groomRejectMarker(id)
	out, err := ghRunner(ctx, "issue", "view", fmt.Sprint(in.SourceNumber),
		"-R", in.Owner+"/"+in.Repo, "--json", "comments")
	if err != nil {
		return fmt.Errorf("gh issue view (groom note): %s: %w", strings.TrimSpace(string(out)), err)
	}
	var doc struct {
		Comments []struct {
			Body string `json:"body"`
		} `json:"comments"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		return fmt.Errorf("parse issue comments: %w", err)
	}
	for _, c := range doc.Comments {
		if strings.Contains(c.Body, marker) {
			return nil // already noted
		}
	}
	body := marker + "\n\n"
	if r := strings.TrimSpace(in.Reason); r != "" {
		body += r + "\n"
	}
	body += "\nKonoha backlog grooming (§56) could not derive a validated analysis; the item is left in the backlog for a human. It is never auto-closed.\n"
	if out, err := ghRunner(ctx, "issue", "comment", fmt.Sprint(in.SourceNumber),
		"-R", in.Owner+"/"+in.Repo, "--body", body); err != nil {
		return fmt.Errorf("gh issue comment (groom note): %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// BacklogGroomWorkflow is the schedule-driven §56 entrypoint: analyze each raw
// backlog item in DRY and file one idempotent ready issue per actionable item,
// noting (never closing) the rest. Disabled config is a no-op.
func BacklogGroomWorkflow(ctx workflow.Context, scope RepoScope) error {
	lc := scope.Lifecycle
	if !GroomingEnabled(lc) {
		return nil
	}
	ao := workflow.ActivityOptions{
		StartToCloseTimeout: appCfg.Worker.startToClose(),
		RetryPolicy: &temporal.RetryPolicy{
			MaximumAttempts: int32(appCfg.Worker.RetryMaxAttempts),
		},
	}
	ctx = workflow.WithActivityOptions(ctx, ao)

	var items []RawBacklogItem
	if err := workflow.ExecuteActivity(ctx, ListBacklogItems, GroomListRequest{
		Owner: scope.Owner, Repo: scope.Repo, Selector: GroomingSelector(lc),
	}).Get(ctx, &items); err != nil {
		return err
	}
	workflow.GetLogger(ctx).Info("backlog grooming",
		"repo", scope.Owner+"/"+scope.Repo, "items", len(items))

	// §21: the labels we are about to apply must exist (idempotent create).
	if err := workflow.ExecuteActivity(ctx, EnsureRepoLabels, LabelsTheirsRequest{
		Owner: scope.Owner, Repo: scope.Repo,
		Labels: dedupeLabels([]string{GroomingReadyLabel(lc), GroomingRepoLabel(lc)}),
		DryRun: lc.DryRun,
	}).Get(ctx, nil); err != nil {
		return err
	}

	outcome := GroomOutcome{}
	for _, it := range items {
		in := IssueInput{
			Owner: scope.Owner, Repo: scope.Repo, IssueNum: it.Number,
			RepoPath: scope.RepoPath, Lifecycle: lc,
		}
		var a IssueAnalysis
		if err := workflow.ExecuteActivity(ctx, AnalyzeIssue, in).Get(ctx, &a); err != nil {
			outcome.Rejected++
			if e := noteUngroomable(ctx, scope, it, "The Issue Analyst could not produce a validated analysis."); e != nil {
				return e
			}
			continue
		}
		var est EstimateResult
		if err := workflow.ExecuteActivity(ctx, EstimateIssue, in, a).Get(ctx, &est); err != nil {
			outcome.Rejected++
			if e := noteUngroomable(ctx, scope, it, "The Estimator could not produce a validated estimate."); e != nil {
				return e
			}
			continue
		}
		var res GroomReadyResult
		if err := workflow.ExecuteActivity(ctx, EnsureReadyIssue, GroomReadyRequest{
			Owner: scope.Owner, Repo: scope.Repo, Source: it, Analysis: a, Estimate: est,
			ReadyLabel: GroomingReadyLabel(lc), RepoLabel: GroomingRepoLabel(lc), DryRun: lc.DryRun,
		}).Get(ctx, &res); err != nil {
			return err
		}
		outcome.Groomed++
		workflow.GetLogger(ctx).Info("groomed backlog item",
			"source", it.Number, "ready", res.Number, "created", res.Created)
	}
	return workflow.ExecuteActivity(ctx, RecordGroomMetrics, outcome).Get(ctx, nil)
}

// noteUngroomable records the idempotent note on a raw item (and honors DryRun).
func noteUngroomable(ctx workflow.Context, scope RepoScope, it RawBacklogItem, reason string) error {
	return workflow.ExecuteActivity(ctx, RecordGroomNote, GroomNoteRequest{
		Owner: scope.Owner, Repo: scope.Repo, SourceNumber: it.Number,
		SourceTitle: it.Title, SourceBody: it.Body, Reason: reason, DryRun: scope.Lifecycle.DryRun,
	}).Get(ctx, nil)
}
