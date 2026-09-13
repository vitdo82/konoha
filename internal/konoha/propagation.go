package konoha

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"go.temporal.io/sdk/workflow"
)

// §54 cross-repo change propagation. When a change in repo A is merged, a
// downstream repo B that consumes A may need its own work item. A per-repo
// dependency map declares the downstream repos and the file globs that trigger
// them; after a merge, Konoha detects the touched downstreams and files one
// idempotent cross-repo issue in each. The filed issue carries no
// `konoha:managed` label, so repo B's OWN sweep/webhook drives it through repo
// B's lifecycle — Konoha never starts that cycle here, so B's phase/autonomy/
// §20 gates apply unchanged and there is no double-start (§32/§52).
//
// Detection is read-only; filing is bounded by the configured fan-out cap (§43
// style) and honors DryRun (§42). All I/O lives in DetectAffectedDownstreams /
// EnsureCrossRepoIssue; the workflow stage is deterministic.

// PropagationConfig is the per-repo §54 policy (repos.yaml `lifecycle.
// propagation`). Downstream maps a downstream repo name to the trigger that
// makes a merged change affect it. A key may be a bare repo name (same owner as
// the upstream) or `owner/repo`.
type PropagationConfig struct {
	Enabled     bool                         `yaml:"enabled" json:"enabled,omitempty"`
	MaxPerMerge int                          `yaml:"maxPerMerge" json:"maxPerMerge,omitempty"` // fan-out cap per merge
	Downstream  map[string]PropagationTarget `yaml:"downstream" json:"downstream,omitempty"`
}

// PropagationTarget is one downstream entry: a file glob over the merged change
// and the change-kind tag carried onto the filed issue.
type PropagationTarget struct {
	Trigger string `yaml:"trigger" json:"trigger,omitempty"` // changed-file glob (globMatch syntax)
	Type    string `yaml:"type" json:"type,omitempty"`       // change kind (e.g. breaking, api, schema)
}

const defaultPropagationMaxPerMerge = 3

// PropagationEnabled reports whether the repo opted into §54 propagation.
// Absent/false is a no-op everywhere.
func PropagationEnabled(lc LifecycleConfig) bool { return lc.Propagation.Enabled }

// maxPerMerge resolves the §43-style fan-out cap; 0/unset means the default.
func (pc PropagationConfig) maxPerMerge() int {
	if pc.MaxPerMerge < 1 {
		return defaultPropagationMaxPerMerge
	}
	return pc.MaxPerMerge
}

// validate gates the §54 propagation block: an enabled map needs at least one
// downstream, every entry needs a trigger and a type, and the cap is
// non-negative. Unknown keys fail at decode (KnownFields).
func (pc PropagationConfig) validate(prefix string) error {
	if pc.MaxPerMerge < 0 {
		return fmt.Errorf("%s.maxPerMerge: %d must be >= 0", prefix, pc.MaxPerMerge)
	}
	if pc.Enabled && len(pc.Downstream) == 0 {
		return fmt.Errorf("%s.downstream: at least one downstream is required when propagation.enabled", prefix)
	}
	names := make([]string, 0, len(pc.Downstream))
	for n := range pc.Downstream {
		names = append(names, n)
	}
	sort.Strings(names) // deterministic error for a bad map
	for _, n := range names {
		t := pc.Downstream[n]
		if strings.TrimSpace(n) == "" {
			return fmt.Errorf("%s.downstream: repo name required", prefix)
		}
		if strings.TrimSpace(t.Trigger) == "" {
			return fmt.Errorf("%s.downstream.%s.trigger: required", prefix, n)
		}
		if strings.TrimSpace(t.Type) == "" {
			return fmt.Errorf("%s.downstream.%s.type: required", prefix, n)
		}
	}
	return nil
}

// AffectedDownstream is one matched propagation target.
type AffectedDownstream struct {
	Repo    string `json:"repo"`    // downstream repo key (bare name or owner/repo)
	Type    string `json:"type"`    // change-kind tag
	Trigger string `json:"trigger"` // the glob that matched
}

// MatchAffectedDownstreams is the pure §54 dependency-map matcher: a downstream
// is affected when at least one changed file matches its trigger glob. Targets
// are evaluated in sorted repo-name order and the result is capped at max
// (<=0 applies the default), so the fan-out is deterministic and bounded.
func MatchAffectedDownstreams(pc PropagationConfig, changedFiles []string, max int) []AffectedDownstream {
	if max < 1 {
		max = pc.maxPerMerge()
	}
	names := make([]string, 0, len(pc.Downstream))
	for n := range pc.Downstream {
		names = append(names, n)
	}
	sort.Strings(names)
	var out []AffectedDownstream
	for _, n := range names {
		if len(out) >= max {
			break
		}
		t := pc.Downstream[n]
		for _, f := range changedFiles {
			if globMatch(t.Trigger, f) {
				out = append(out, AffectedDownstream{Repo: n, Type: t.Type, Trigger: t.Trigger})
				break
			}
		}
	}
	return out
}

// PropagationDetectRequest parameterizes DetectAffectedDownstreams.
type PropagationDetectRequest struct {
	Owner  string            `json:"owner"`
	Repo   string            `json:"repo"`
	PrNum  int               `json:"pr,omitempty"`
	Config PropagationConfig `json:"config,omitempty"`
}

// DetectAffectedDownstreams matches the merged PR's changed files against the
// configured dependency map. It is read-only: the changed-file list comes from
// `gh pr diff --name-only`. Disabled config or a missing PR (PrNum<=0, e.g. a
// dry-run shadow where no PR was created) yields no affected downstreams. A gh
// failure is returned, never silently treated as "nothing affected".
func DetectAffectedDownstreams(ctx context.Context, in PropagationDetectRequest) ([]AffectedDownstream, error) {
	if !in.Config.Enabled || in.PrNum <= 0 {
		return nil, nil
	}
	out, err := ghRunner(ctx, "pr", "diff", strconv.Itoa(in.PrNum),
		"-R", in.Owner+"/"+in.Repo, "--name-only")
	if err != nil {
		return nil, fmt.Errorf("propagation diff %s/%s#%d: %s: %w",
			in.Owner, in.Repo, in.PrNum, strings.TrimSpace(string(out)), err)
	}
	var files []string
	for _, l := range strings.Split(string(out), "\n") {
		if f := strings.TrimSpace(l); f != "" {
			files = append(files, f)
		}
	}
	return MatchAffectedDownstreams(in.Config, files, in.Config.maxPerMerge()), nil
}

// crossMarkerOpen is the §24 idempotency marker for a cross-repo issue:
// `<!-- konoha-cross: <owner>/<repo>#<sha> -->`, matched byte-exactly.
const crossMarkerOpen = "<!-- konoha-cross: "

func crossMarker(upstreamOwner, upstreamRepo, sha string) string {
	return crossMarkerOpen + upstreamOwner + "/" + upstreamRepo + "#" + sha + " -->"
}

// CrossRepoIssueRequest targets one downstream's idempotent cross-repo issue.
type CrossRepoIssueRequest struct {
	UpstreamOwner string `json:"upstreamOwner"`
	UpstreamRepo  string `json:"upstreamRepo"`
	PrNum         int    `json:"pr,omitempty"`
	Sha           string `json:"sha"`
	Owner         string `json:"owner,omitempty"` // downstream owner (defaults to upstream owner)
	Repo          string `json:"repo"`            // downstream repo
	Type          string `json:"type,omitempty"`
	Trigger       string `json:"trigger,omitempty"`
	Summary       string `json:"summary,omitempty"`
	Label         string `json:"label,omitempty"` // base label (default konoha:cross-repo)
	DryRun        bool   `json:"dryRun,omitempty"`
}

// CrossRepoIssueResult reports the filed (or matched) issue. Number is 0 under DryRun.
type CrossRepoIssueResult struct {
	Number  int    `json:"number,omitempty"`
	Created bool   `json:"created,omitempty"`
	Marker  string `json:"marker,omitempty"`
}

// EnsureCrossRepoIssue files one issue in a downstream repo, idempotent under
// retry (§24): it scans the downstream repo's issues for the deterministic
// marker first and returns the existing number instead of creating a duplicate.
// The issue carries the required `konoha:cross-repo` label AND NO
// `konoha:managed` label, so the downstream sweep/webhook starts its lifecycle
// with the downstream repo's own policy (never started here, no double-start).
func EnsureCrossRepoIssue(ctx context.Context, in CrossRepoIssueRequest) (CrossRepoIssueResult, error) {
	if in.DryRun {
		return CrossRepoIssueResult{}, nil // §42: shadow mode mutates nothing
	}
	if strings.TrimSpace(in.Sha) == "" {
		return CrossRepoIssueResult{}, fmt.Errorf("EnsureCrossRepoIssue: sha required for the idempotent marker")
	}
	if strings.TrimSpace(in.UpstreamOwner) == "" || strings.TrimSpace(in.UpstreamRepo) == "" || strings.TrimSpace(in.Repo) == "" {
		return CrossRepoIssueResult{}, fmt.Errorf("EnsureCrossRepoIssue: upstream owner/repo and downstream repo are required")
	}
	owner := strings.TrimSpace(in.Owner)
	if owner == "" {
		owner = in.UpstreamOwner
	}
	marker := crossMarker(in.UpstreamOwner, in.UpstreamRepo, in.Sha)
	items, err := listIssues(ctx, owner, in.Repo)
	if err != nil {
		return CrossRepoIssueResult{}, err
	}
	if n := findMarkerIssue(items, marker); n != 0 {
		return CrossRepoIssueResult{Number: n, Marker: marker}, nil
	}
	label := strings.TrimSpace(in.Label)
	if label == "" {
		label = labelCrossRepo
	}
	args := []string{"issue", "create", "-R", owner + "/" + in.Repo,
		"--title", crossRepoIssueTitle(in), "--body", crossRepoIssueBody(in, marker)}
	for _, l := range dedupeLabels([]string{label}) {
		args = append(args, "--label", l)
	}
	out, err := ghRunner(ctx, args...)
	if err != nil {
		return CrossRepoIssueResult{}, fmt.Errorf("gh issue create (cross-repo): %s: %w", strings.TrimSpace(string(out)), err)
	}
	n, err := parseIssueNumber(string(out))
	if err != nil {
		return CrossRepoIssueResult{}, err
	}
	return CrossRepoIssueResult{Number: n, Created: true, Marker: marker}, nil
}

// crossRepoIssueTitle renders a deterministic title per downstream change kind.
func crossRepoIssueTitle(in CrossRepoIssueRequest) string {
	kind := strings.TrimSpace(in.Type)
	if kind == "" {
		kind = "change"
	}
	return fmt.Sprintf("[cross-repo] %s from %s/%s", kind, in.UpstreamOwner, in.UpstreamRepo)
}

// crossRepoIssueBody renders the issue body with the idempotency marker first.
func crossRepoIssueBody(in CrossRepoIssueRequest, marker string) string {
	var b strings.Builder
	b.WriteString(marker + "\n\n")
	fmt.Fprintf(&b, "A merged change in `%s/%s` may affect this repo.\n\n", in.UpstreamOwner, in.UpstreamRepo)
	if in.PrNum > 0 {
		fmt.Fprintf(&b, "- Upstream PR: `%s/%s#%d`\n", in.UpstreamOwner, in.UpstreamRepo, in.PrNum)
	}
	if in.Sha != "" {
		fmt.Fprintf(&b, "- Upstream commit: `%s`\n", in.Sha)
	}
	if in.Type != "" {
		fmt.Fprintf(&b, "- Change type: `%s`\n", in.Type)
	}
	if in.Trigger != "" {
		fmt.Fprintf(&b, "- Matched trigger: `%s`\n", in.Trigger)
	}
	if strings.TrimSpace(in.Summary) != "" {
		fmt.Fprintf(&b, "\n%s\n", strings.TrimSpace(in.Summary))
	}
	b.WriteString("\nFiled by Konoha cross-repo propagation (§54). This issue runs through this repo's normal lifecycle; ")
	b.WriteString("this repo's own config and gates decide autonomy.\n")
	return b.String()
}

// runPropagation is the §54 post-merge stage: detect the configured downstream
// repos the merged change touches and file one idempotent cross-repo issue in
// each. Disabled/absent config is a no-op; detection is read-only. DryRun keeps
// the analysis but suppresses every mutation (label create + issue create).
func runPropagation(ctx workflow.Context, in PostMergeInput) error {
	if !PropagationEnabled(in.Lifecycle) {
		return nil
	}
	var affected []AffectedDownstream
	if err := workflow.ExecuteActivity(ctx, DetectAffectedDownstreams, PropagationDetectRequest{
		Owner: in.Owner, Repo: in.Repo, PrNum: in.PrNum, Config: in.Lifecycle.Propagation,
	}).Get(ctx, &affected); err != nil {
		return err
	}
	workflow.GetLogger(ctx).Info("cross-repo propagation detected",
		"repo", in.Owner+"/"+in.Repo, "affected", len(affected))

	for _, a := range affected {
		owner, repo := downstreamOwnerRepo(in.Owner, a.Repo)
		// §21: the required label must exist in the downstream repo before create.
		if err := workflow.ExecuteActivity(ctx, EnsureRepoLabels, LabelsTheirsRequest{
			Owner: owner, Repo: repo, Labels: []string{labelCrossRepo}, DryRun: in.Lifecycle.DryRun,
		}).Get(ctx, nil); err != nil {
			return err
		}
		var res CrossRepoIssueResult
		if err := workflow.ExecuteActivity(ctx, EnsureCrossRepoIssue, CrossRepoIssueRequest{
			UpstreamOwner: in.Owner, UpstreamRepo: in.Repo, PrNum: in.PrNum, Sha: in.Sha,
			Owner: owner, Repo: repo, Type: a.Type, Trigger: a.Trigger,
			Label: labelCrossRepo, DryRun: in.Lifecycle.DryRun,
		}).Get(ctx, &res); err != nil {
			return err
		}
		workflow.GetLogger(ctx).Info("cross-repo issue",
			"downstream", owner+"/"+repo, "issue", res.Number, "created", res.Created)
	}
	return nil
}

// downstreamOwnerRepo resolves a dependency-map key ("repo" = same owner as the
// upstream, "owner/repo" = explicit) into an owner/repo pair.
func downstreamOwnerRepo(upstreamOwner, key string) (string, string) {
	if i := strings.IndexByte(key, '/'); i > 0 {
		return key[:i], key[i+1:]
	}
	return upstreamOwner, key
}
