package konoha

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// §50 post-merge release + changelog automation. After a PR merges, Konoha
// derives a semver bump from the conventional-commit messages since the last
// tag, renders CHANGELOG.md, and publishes a GitHub Release + tag via the App.
//
// Idempotency (§24): the next tag is computed from the existing tags and an
// existing tag is a no-op, so a Temporal retry (or a duplicate PostMergeWorkflow)
// never creates two releases. Release is opt-in per repo
// (`lifecycle.release.enabled`); disabled/absent → no-op everywhere.
//
// All I/O lives in RunRelease (activity). PostMergeWorkflow is deterministic:
// it only calls the activity.

// ReleaseConfig is the per-repo §50 release policy (repos.yaml `lifecycle.release`).
type ReleaseConfig struct {
	Enabled       bool   `yaml:"enabled" json:"enabled,omitempty"`
	ChangelogPath string `yaml:"changelogPath" json:"changelogPath,omitempty"`
	VersionPrefix string `yaml:"versionPrefix" json:"versionPrefix,omitempty"`
}

const (
	defaultChangelogPath = "CHANGELOG.md"
	defaultVersionPrefix = "v"
)

// changelogPath returns the configured changelog path or the default.
func (rc ReleaseConfig) changelogPath() string {
	if p := strings.TrimSpace(rc.ChangelogPath); p != "" {
		return p
	}
	return defaultChangelogPath
}

// versionPrefix returns the configured tag prefix or the default ("v").
func (rc ReleaseConfig) versionPrefix() string {
	if rc.VersionPrefix == "" {
		return defaultVersionPrefix
	}
	return rc.VersionPrefix
}

// ReleaseEnabled reports whether the repo opted into §50 release automation.
// Absent/false is a no-op everywhere (config-driven, fail-closed).
func ReleaseEnabled(lc LifecycleConfig) bool { return lc.Release.Enabled }

// §50 bump levels, ordered major > minor > patch.
const (
	bumpMajor = "major"
	bumpMinor = "minor"
	bumpPatch = "patch"
)

// ConventionalCommitBump classifies one commit message per the conventional
// commmits spec: a `!` in the type/scope header or a `BREAKING CHANGE`/
// `BREAKING-CHANGE` footer → major; `feat` → minor; `fix`/`perf` → patch;
// anything unmatched (including non-conventional text) → patch.
func ConventionalCommitBump(msg string) string {
	header := msg
	if i := strings.IndexByte(msg, '\n'); i >= 0 {
		header = msg[:i]
	}
	if strings.Contains(msg, "BREAKING CHANGE") || strings.Contains(msg, "BREAKING-CHANGE") {
		return bumpMajor
	}
	typ, breaking := ccType(header)
	if breaking {
		return bumpMajor
	}
	switch typ {
	case "feat":
		return bumpMinor
	default:
		// fix, perf, chore, docs, ... and no-match all map to patch.
		return bumpPatch
	}
}

// ccType extracts the conventional-commit type from a header line and whether
// the header is marked breaking (`type!:` / `type(scope)!:`).
func ccType(header string) (typ string, breaking bool) {
	header = strings.TrimSpace(header)
	colon := strings.IndexByte(header, ':')
	if colon < 0 {
		return "", false
	}
	subject := header[:colon]
	if strings.HasSuffix(subject, "!") {
		breaking = true
		subject = strings.TrimSuffix(subject, "!")
	}
	if i := strings.IndexByte(subject, '('); i >= 0 {
		subject = subject[:i]
	}
	return strings.ToLower(strings.TrimSpace(subject)), breaking
}

// DetermineBump returns the highest bump across the given commit messages.
// No messages (or none matching) → patch, per §50.
func DetermineBump(messages []string) string {
	bump := bumpPatch
	for _, m := range messages {
		switch ConventionalCommitBump(m) {
		case bumpMajor:
			return bumpMajor
		case bumpMinor:
			bump = bumpMinor
		}
	}
	return bump
}

// ParseVersion parses a tag like v1.2.3 (prefix-stripped) into its semver
// components. Tags without a full X.Y.Z are rejected (ok=false) so unrelated
// tags sharing the prefix are ignored, not misread.
func ParseVersion(tag, prefix string) (major, minor, patch int, ok bool) {
	s := tag
	if prefix != "" && strings.HasPrefix(s, prefix) {
		s = strings.TrimPrefix(s, prefix)
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return 0, 0, 0, false
	}
	nums := make([]int, 3)
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return 0, 0, 0, false
		}
		nums[i] = n
	}
	return nums[0], nums[1], nums[2], true
}

// NextVersion bumps latest by bump and returns the prefixed tag. An empty latest
// is treated as 0.0.0. An unknown bump level fails closed (never a silent tag).
func NextVersion(latest, bump, prefix string) (string, error) {
	major, minor, patch := 0, 0, 0
	if latest != "" {
		var ok bool
		major, minor, patch, ok = ParseVersion(latest, prefix)
		if !ok {
			return "", fmt.Errorf("NextVersion: latest tag %q is not semver (prefix %q)", latest, prefix)
		}
	}
	switch bump {
	case bumpMajor:
		major, minor, patch = major+1, 0, 0
	case bumpMinor:
		minor, patch = minor+1, 0
	case bumpPatch:
		patch++
	default:
		return "", fmt.Errorf("NextVersion: unknown bump %q", bump)
	}
	return fmt.Sprintf("%s%d.%d.%d", prefix, major, minor, patch), nil
}

// MergedPR is one merged pull request in a changelog entry.
type MergedPR struct {
	Number   int    `json:"number"`
	Title    string `json:"title"`
	URL      string `json:"url,omitempty"`
	MergedAt string `json:"mergedAt,omitempty"`
}

// RenderChangelog renders one release's markdown section listing the merged PRs
// since the previous tag (the §50 acceptance criterion).
func RenderChangelog(version string, prs []MergedPR, date string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## %s - %s\n\n", version, date)
	if len(prs) == 0 {
		b.WriteString("_No changes._\n")
		return b.String()
	}
	for _, pr := range prs {
		title := strings.TrimSpace(pr.Title)
		if pr.Number > 0 {
			fmt.Fprintf(&b, "- #%d %s\n", pr.Number, title)
		} else {
			fmt.Fprintf(&b, "- %s\n", title)
		}
	}
	return b.String()
}

// ReleaseRequest parameterizes RunRelease. Config comes from the repo's
// lifecycle.release block (threaded through the workflow input).
type ReleaseRequest struct {
	Owner      string        `json:"owner"`
	Repo       string        `json:"repo"`
	PrNum      int           `json:"pr,omitempty"`
	BaseBranch string        `json:"baseBranch,omitempty"` // "" = repo default
	Config     ReleaseConfig `json:"config,omitempty"`
	DryRun     bool          `json:"dryRun,omitempty"`
}

// ReleaseResult reports what RunRelease did. Skipped is a first-class outcome:
// release disabled, dry-run, no commits since the last tag, or the next tag
// already exists (idempotent no-op).
type ReleaseResult struct {
	Skipped     bool       `json:"skipped,omitempty"`
	Reason      string     `json:"reason,omitempty"`
	Tag         string     `json:"tag,omitempty"`
	Version     string     `json:"version,omitempty"`
	Bump        string     `json:"bump,omitempty"`
	PreviousTag string     `json:"previousTag,omitempty"`
	Changelog   string     `json:"changelog,omitempty"`
	ReleaseURL  string     `json:"releaseURL,omitempty"`
	PRs         []MergedPR `json:"prs,omitempty"`
}

// RunRelease is the §50 release activity: it is idempotent (an existing next tag
// is a no-op), never mutates under DryRun/disabled, and commits CHANGELOG.md via
// the GitHub Contents API (no local repo checkout needed) before creating the
// tag + release. Failing to create the release returns an error (no silent
// success, §40).
func RunRelease(ctx context.Context, in ReleaseRequest) (ReleaseResult, error) {
	if !in.Config.Enabled {
		return ReleaseResult{Skipped: true, Reason: "release disabled"}, nil
	}
	if in.DryRun {
		return ReleaseResult{Skipped: true, Reason: "dryRun"}, nil
	}
	tok, err := appToken(ctx)
	if err != nil {
		return ReleaseResult{}, err
	}
	repo := in.Owner + "/" + in.Repo
	base := in.BaseBranch
	if base == "" {
		if base, err = repoDefaultBranch(ctx, tok, repo); err != nil {
			return ReleaseResult{}, err
		}
	}
	prefix := in.Config.versionPrefix()

	tags, err := repoListTags(ctx, tok, repo)
	if err != nil {
		return ReleaseResult{}, err
	}
	latest := latestTag(tags, prefix)
	since := ""
	if latest != "" {
		since = commitDate(ctx, tok, repo, latest)
	}

	messages, err := commitMessages(ctx, tok, repo, base, since)
	if err != nil {
		return ReleaseResult{}, err
	}
	if len(messages) == 0 {
		// No unreleased commits → no spurious patch release on every run.
		return ReleaseResult{Skipped: true, Reason: "no commits since last tag", PreviousTag: latest}, nil
	}

	bump := DetermineBump(messages)
	tag, err := NextVersion(latest, bump, prefix)
	if err != nil {
		return ReleaseResult{}, err
	}
	if tagIn(tags, tag) {
		return ReleaseResult{Skipped: true, Reason: "tag " + tag + " already exists", Tag: tag, PreviousTag: latest}, nil
	}

	prs, err := mergedPRsSince(ctx, tok, repo, base, since)
	if err != nil {
		return ReleaseResult{}, err
	}
	changelog := RenderChangelog(tag, prs, time.Now().UTC().Format("2006-01-02"))

	// Commit the changelog first (idempotent upsert), then create the release so
	// a retry after a lost response re-uses the same tag + changelog (§24).
	if err := upsertChangelog(ctx, tok, repo, base, in.Config.changelogPath(), tag, changelog); err != nil {
		return ReleaseResult{}, err
	}
	url, err := createRelease(ctx, tok, repo, tag, base, changelog)
	if err != nil {
		return ReleaseResult{}, err
	}
	return ReleaseResult{
		Tag: tag, Version: strings.TrimPrefix(tag, prefix), Bump: bump,
		PreviousTag: latest, Changelog: changelog, ReleaseURL: url, PRs: prs,
	}, nil
}

// PostMergeInput identifies a merged PR whose release should be published.
type PostMergeInput struct {
	Owner      string          `json:"owner"`
	Repo       string          `json:"repo"`
	RepoPath   string          `json:"repoPath"`
	PrNum      int             `json:"pr,omitempty"`
	BaseBranch string          `json:"baseBranch,omitempty"`
	Lifecycle  LifecycleConfig `json:"lifecycle,omitempty"`
}

// PostMergeWorkflow runs the §50 release after a merge lands. It is started from
// the merge-complete path in IssueCycleWorkflow and is safe to re-run (the tag
// check makes it idempotent); a cron/manual invocation is a valid backstop.
// Deterministic: all I/O lives in the RunRelease activity.
func PostMergeWorkflow(ctx workflow.Context, in PostMergeInput) error {
	if !ReleaseEnabled(in.Lifecycle) {
		return nil
	}
	ao := workflow.ActivityOptions{
		StartToCloseTimeout: appCfg.Worker.startToClose(),
		RetryPolicy: &temporal.RetryPolicy{
			MaximumAttempts: int32(appCfg.Worker.RetryMaxAttempts),
		},
	}
	ctx = workflow.WithActivityOptions(ctx, ao)

	var res ReleaseResult
	if err := workflow.ExecuteActivity(ctx, RunRelease, ReleaseRequest{
		Owner: in.Owner, Repo: in.Repo, PrNum: in.PrNum, BaseBranch: in.BaseBranch,
		Config: in.Lifecycle.Release, DryRun: in.Lifecycle.DryRun,
	}).Get(ctx, &res); err != nil {
		return err
	}
	workflow.GetLogger(ctx).Info("post-merge release",
		"repo", in.Owner+"/"+in.Repo, "skipped", res.Skipped, "tag", res.Tag, "reason", res.Reason)
	return nil
}

// repoDefaultBranch resolves the repo's default branch via the App token.
func repoDefaultBranch(ctx context.Context, tok, repo string) (string, error) {
	b, err := ghTokenRunner(ctx, tok, "api", "repos/"+repo)
	if err != nil {
		return "", fmt.Errorf("default branch %s: %s: %w", repo, strings.TrimSpace(string(b)), err)
	}
	var doc struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return "", fmt.Errorf("parse repo %s: %w", repo, err)
	}
	if doc.DefaultBranch == "" {
		return "main", nil
	}
	return doc.DefaultBranch, nil
}

// repoListTags returns up to 100 tag names (oldest→newest as GitHub returns
// them). 100 is the page size; a repo with more tags would need pagination.
func repoListTags(ctx context.Context, tok, repo string) ([]string, error) {
	b, err := ghTokenRunner(ctx, tok, "api", "repos/"+repo+"/tags?per_page=100")
	if err != nil {
		return nil, fmt.Errorf("list tags %s: %s: %w", repo, strings.TrimSpace(string(b)), err)
	}
	var rows []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(b, &rows); err != nil {
		return nil, fmt.Errorf("parse tags %s: %w", repo, err)
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Name)
	}
	return out, nil
}

// latestTag returns the highest semver tag carrying prefix, or "" when none.
func latestTag(tags []string, prefix string) string {
	best := ""
	bmaj, bmin, bpat := 0, 0, 0
	for _, t := range tags {
		maj, min, pat, ok := ParseVersion(t, prefix)
		if !ok {
			continue
		}
		if best == "" || maj > bmaj || (maj == bmaj && min > bmin) ||
			(maj == bmaj && min == bmin && pat > bpat) {
			best, bmaj, bmin, bpat = t, maj, min, pat
		}
	}
	return best
}

// tagIn reports whether tags already contains tag (byte-exact).
func tagIn(tags []string, tag string) bool {
	for _, t := range tags {
		if t == tag {
			return true
		}
	}
	return false
}

// commitDate returns the committer date (RFC3339) of a tag/ref, or "" when it
// cannot be resolved (the caller then treats it as "no lower bound").
func commitDate(ctx context.Context, tok, repo, ref string) string {
	b, err := ghTokenRunner(ctx, tok, "api", "repos/"+repo+"/commits/"+ref)
	if err != nil {
		return ""
	}
	var doc struct {
		Commit struct {
			Committer struct {
				Date string `json:"date"`
			} `json:"committer"`
		} `json:"commit"`
	}
	if json.Unmarshal(b, &doc) != nil {
		return ""
	}
	return doc.Commit.Committer.Date
}

// commitMessages lists the commit messages on base since `since` (RFC3339; ""
// = no lower bound). Bounded to 100 commits.
func commitMessages(ctx context.Context, tok, repo, base, since string) ([]string, error) {
	endpoint := "repos/" + repo + "/commits?sha=" + base + "&per_page=100"
	if since != "" {
		endpoint += "&since=" + since
	}
	b, err := ghTokenRunner(ctx, tok, "api", endpoint)
	if err != nil {
		return nil, fmt.Errorf("list commits %s: %s: %w", repo, strings.TrimSpace(string(b)), err)
	}
	var rows []struct {
		Commit struct {
			Message string `json:"message"`
		} `json:"commit"`
	}
	if err := json.Unmarshal(b, &rows); err != nil {
		return nil, fmt.Errorf("parse commits %s: %w", repo, err)
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		if m := strings.TrimSpace(r.Commit.Message); m != "" {
			out = append(out, m)
		}
	}
	return out, nil
}

// mergedPRsSince lists PRs merged into base at/after `since` ("" = all returned,
// bounded to 100). Newest first, as GitHub returns them.
func mergedPRsSince(ctx context.Context, tok, repo, base, since string) ([]MergedPR, error) {
	b, err := ghTokenRunner(ctx, tok, "pr", "list", "-R", repo,
		"--state", "merged", "--base", base, "--limit", "100",
		"--json", "number,title,url,mergedAt")
	if err != nil {
		return nil, fmt.Errorf("list merged PRs %s: %s: %w", repo, strings.TrimSpace(string(b)), err)
	}
	var rows []MergedPR
	if err := json.Unmarshal(b, &rows); err != nil {
		return nil, fmt.Errorf("parse merged PRs %s: %w", repo, err)
	}
	if since == "" {
		return rows, nil
	}
	cutoff, err := time.Parse(time.RFC3339, since)
	if err != nil {
		return rows, nil // unparseable bound → include all rather than silently drop
	}
	var out []MergedPR
	for _, pr := range rows {
		t, err := time.Parse(time.RFC3339, pr.MergedAt)
		if err != nil || t.After(cutoff) {
			out = append(out, pr)
		}
	}
	return out, nil
}

// upsertChangelog prepends the new entry to path on base via the Contents API.
// It reads the existing file (sha + content) first so the update is a normal
// commit and a retry converges on the same result (§24).
func upsertChangelog(ctx context.Context, tok, repo, base, path, tag, entry string) error {
	sha, prev := "", ""
	if b, err := ghTokenRunner(ctx, tok, "api", "repos/"+repo+"/contents/"+path); err == nil {
		var doc struct {
			Sha      string `json:"sha"`
			Content  string `json:"content"`
			Encoding string `json:"encoding"`
		}
		if json.Unmarshal(b, &doc) == nil {
			sha = doc.Sha
			if doc.Encoding == "" || doc.Encoding == "base64" {
				if dec, e := base64.StdEncoding.DecodeString(strings.ReplaceAll(doc.Content, "\n", "")); e == nil {
					prev = string(dec)
				}
			}
		}
	}
	content := entry
	if strings.TrimSpace(prev) != "" {
		content = entry + "\n" + strings.TrimLeft(prev, "\n")
	}
	args := []string{"api", "-X", "PUT", "repos/" + repo + "/contents/" + path,
		"-f", "message=chore(release): " + tag,
		"-f", "content=" + base64.StdEncoding.EncodeToString([]byte(content)),
		"-f", "branch=" + base,
	}
	if sha != "" {
		args = append(args, "-f", "sha="+sha)
	}
	if out, err := ghTokenRunner(ctx, tok, args...); err != nil {
		return fmt.Errorf("commit %s: %s: %w", path, strings.TrimSpace(string(out)), err)
	}
	return nil
}

// createRelease creates the tag + GitHub Release at base and returns its URL.
// `gh release create` also creates the tag when absent; RunRelease's tag check
// guarantees an existing tag never reaches here (idempotent).
func createRelease(ctx context.Context, tok, repo, tag, target, notes string) (string, error) {
	out, err := ghTokenRunner(ctx, tok, "release", "create", tag, "-R", repo,
		"--title", tag, "--notes", notes, "--target", target)
	if err != nil {
		return "", fmt.Errorf("gh release create %s: %s: %w", tag, strings.TrimSpace(string(out)), err)
	}
	return strings.TrimSpace(string(out)), nil
}
