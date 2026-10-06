package konoha

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"go.temporal.io/sdk/activity"
)

// PublishInput describes a change the developer agent prepared locally. All
// GitHub mutations happen here with the App installation token only (§23, §25).
// CommitSHA is optional: when set, the local branch ref is created from that
// commit in RepoPath's object DB (the dev/repair worktree's commit object lives
// there because worktrees share the main repo object DB), so publication can run
// from the main repo after the worktree is cleaned up.
type PublishInput struct {
	Owner          string           `json:"owner"`
	Repo           string           `json:"repo"`
	RepoPath       string           `json:"repoPath"`   // repo whose object DB holds the commit
	CommitSHA      string           `json:"commitSha"`  // optional local commit to publish (§25)
	Branch         string           `json:"branch"`     // branch to push and open a PR for
	BaseBranch     string           `json:"baseBranch"` // PR base (default: the repo default)
	Title          string           `json:"title"`
	Body           string           `json:"body"`
	ValidationCmd  string           `json:"validationCmd"`        // repo validation command (§26); "" = none
	Validation     []ValidationGate `json:"validation,omitempty"` // §26 structured argv gates; mutually exclusive with ValidationCmd
	CycleID        string           `json:"cycleId,omitempty"`    // evidence store scope for per-gate output (§26)
	Limits         *LimitsConfig    `json:"limits,omitempty"`     // #102 host resource ceilings for gate processes (nil = global)
	AllowOnFailure bool             `json:"allowOnFailure"`       // explicit policy permission to publish despite failing validation (§26)
	DryRun         bool             `json:"dryRun,omitempty"`
}

// PublishResult reports what PublishDeveloperChange did. ValidationOutput is
// always carried back even when the publish fails, so the §26 failure is never
// silently dropped.
type PublishResult struct {
	Branch            string `json:"branch"`
	PRNumber          int    `json:"prNumber"`
	PRURL             string `json:"prUrl"`
	Validated         bool   `json:"validated"`
	ValidationSkipped bool   `json:"validationSkipped"`
	ValidationOutput  string `json:"validationOutput"`
}

// validateRepo runs the repo's validation in the worktree. When structured
// gates are configured, every gate runs in order (no early exit), each gate's
// stdout/stderr is persisted to evidence, and the aggregate passes iff every
// mandatory gate exited 0; a gate that cannot start records ERROR and fails
// closed (§40). Absent gates, the legacy shell-string ValidationCmd runs as
// before. Empty/absent both means "nothing required", reported as skipped
// (Validated stays false so callers can distinguish "not checked" from
// "checked+passed").
func validateRepo(in PublishInput) (ok, skipped bool, output string, err error) {
	if len(in.Validation) > 0 {
		if strings.TrimSpace(in.ValidationCmd) != "" {
			return false, false, "", fmt.Errorf("validation and validationCmd are mutually exclusive (ambiguous)")
		}
		results := runGates(in)
		return gatesPassed(results), false, gateOutput(results), nil
	}
	cmd := strings.TrimSpace(in.ValidationCmd)
	if cmd == "" {
		return false, true, "", nil
	}
	// #102: best-effort host ceilings on the legacy shell-string path too.
	c, _ := enforceCommand(context.Background(), limitsForOverride(in.Limits), "sh", []string{"-c", cmd}, in.RepoPath, cmdEnv(in.RepoPath))
	out, runErr := c.CombinedOutput()
	return runErr == nil, false, string(out), nil
}

// verificationDrift is the #94 post-verification evidence: whether a §26 gate
// dirtied the worktree after the agent's commit. Evidence only — it never
// changes the delivered commit SHA or any verdict (§40).
type verificationDrift struct {
	Mutated bool
	Patch   string
}

// validateRepoWithDrift wraps validateRepo with #94 drift detection: snapshot
// `git status --porcelain` before the gates, re-check after, and when the gates
// left the worktree dirty capture the post-verification patch under the cycle's
// `verification/post-verification.patch` (redacted, best-effort). The delivered
// commit is untouched: a validation gate that rewrites a tracked file is
// recorded as drift, never folded into the deliverable.
func validateRepoWithDrift(in PublishInput) (ok, skipped bool, output string, drift verificationDrift, err error) {
	pre, _ := execGit(context.Background(), in.RepoPath, "status", "--porcelain")
	ok, skipped, output, err = validateRepo(in)
	// Only a run that actually executed gates can drift; a skipped profile (or
	// the publish path's already-validated no-op) has nothing to check — and
	// must not flag the main checkout's pre-existing state.
	if skipped {
		return ok, skipped, output, drift, err
	}
	drift = detectVerificationDrift(in.RepoPath, pre)
	if drift.Mutated {
		log.Printf("konoha: verification drift in %s (cycle %s): a validation gate left uncommitted changes (evidence only)",
			in.RepoPath, in.CycleID)
		persistPostVerificationPatch(in.CycleID, drift.Patch, in.DryRun)
	}
	return ok, skipped, output, drift, err
}

// detectVerificationDrift re-reads `git status --porcelain` after validation and,
// when non-empty, captures the diff since HEAD. pre is the pre-validation status
// (kept in the evidence for context; the developer path asserts a clean tree
// before validation, so any post-validation dirt is gate-induced).
func detectVerificationDrift(repoPath, pre string) verificationDrift {
	ctx := context.Background()
	post, err := execGit(ctx, repoPath, "status", "--porcelain")
	if err != nil || strings.TrimSpace(post) == "" {
		return verificationDrift{}
	}
	patch, _ := execGit(ctx, repoPath, "diff", "HEAD")
	body := "before:\n" + strings.TrimRight(pre, "\n") + "\nafter:\n" + strings.TrimSpace(post)
	if d := strings.TrimSpace(patch); d != "" {
		body += "\n\n" + d
	}
	return verificationDrift{Mutated: true, Patch: body}
}

// persistPostVerificationPatch writes the drift patch under the cycle's
// verification/ evidence (best-effort, redacted). A cycle without an ID (direct
// CLI run) or no patch skips persistence.
func persistPostVerificationPatch(cycleID, patch string, dryRun bool) {
	if cycleID == "" || strings.TrimSpace(patch) == "" {
		return
	}
	store, err := NewEvidenceStore(appCfg.Evidence, &defaultRedactor)
	if err != nil {
		return
	}
	_ = store.WriteVerification(cycleID, "post-verification", "patch", patch, dryRun)
}

// gateRun is one gate's observed result.
type gateRun struct {
	ID        string
	Mandatory bool
	Started   bool
	ExitCode  int
	Stdout    string
	Stderr    string
	Err       string
}

// status is the gate disposition recorded in the aggregate output: PASSED,
// FAILED (non-zero exit), or ERROR (could not start / timed out).
func (r gateRun) status() string {
	if r.Err != "" {
		return "ERROR"
	}
	if r.ExitCode == 0 {
		return "PASSED"
	}
	return "FAILED"
}

// runGates runs every gate in order without short-circuiting, persisting each
// gate's streams to the evidence store (best-effort). Gates get the scrubbed
// agent env (§23) and a bounded per-gate timeout.
func runGates(in PublishInput) []gateRun {
	results := make([]gateRun, 0, len(in.Validation))
	for _, g := range in.Validation {
		r := runGate(in, g)
		persistGateEvidence(in.CycleID, r, in.DryRun)
		results = append(results, r)
	}
	return results
}

// gatesPassed is the §26 aggregate: every mandatory gate exited 0; any gate
// that could not start (ERROR) fails closed regardless of mandatory (§40).
func gatesPassed(results []gateRun) bool {
	for _, r := range results {
		if r.Err != "" {
			return false
		}
		if r.Mandatory && r.ExitCode != 0 {
			return false
		}
	}
	return true
}

// runGate executes one structured-argv gate. argv is used verbatim (never a
// shell), dir is relative to repoPath and must not escape it. #102 host ceilings
// are applied best-effort to the gate process.
func runGate(in PublishInput, g ValidationGate) gateRun {
	r := gateRun{ID: g.ID, Mandatory: g.isMandatory()}
	if len(g.Argv) == 0 {
		r.Err = "empty argv"
		return r
	}
	repoPath := in.RepoPath
	dir := repoPath
	if d := strings.TrimSpace(g.Dir); d != "" {
		dir = filepath.Join(repoPath, d)
		if rel, err := filepath.Rel(repoPath, dir); err != nil || rel == ".." ||
			strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			r.Err = fmt.Sprintf("dir %q escapes the checkout", g.Dir)
			return r
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), g.timeout())
	defer cancel()
	c, _ := enforceCommand(ctx, limitsForOverride(in.Limits), g.Argv[0], g.Argv[1:], dir, cmdEnv(dir))
	stdout, stderr, err := runCapture(c)
	r.Stdout, r.Stderr = stdout, stderr
	if err == nil {
		r.Started = true
		return r
	}
	if ctx.Err() == context.DeadlineExceeded {
		r.Started = true // it started, then the bounded window killed it
		r.Err = "timeout after " + g.timeout().String()
		return r
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		r.Started = true
		r.ExitCode = ee.ExitCode()
		return r
	}
	r.Err = err.Error() // could not start (not found, bad dir, permission, ...)
	return r
}

// gateOutput renders the per-gate aggregate recorded in PublishResult.
func gateOutput(results []gateRun) string {
	var b strings.Builder
	for _, r := range results {
		fmt.Fprintf(&b, "=== gate %s (%s) ===\n", r.ID, r.status())
		if r.Err != "" {
			fmt.Fprintf(&b, "%s\n", r.Err)
		}
		if s := strings.TrimRight(r.Stdout, "\n"); s != "" {
			b.WriteString(s)
			b.WriteByte('\n')
		}
		if s := strings.TrimRight(r.Stderr, "\n"); s != "" {
			b.WriteString(s)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// persistGateEvidence writes one gate's streams under the cycle's verification
// dir, redacted. Best-effort (§40): an evidence failure never changes the gate
// verdict, and a cycle without an ID (direct CLI run) skips persistence.
func persistGateEvidence(cycleID string, r gateRun, dryRun bool) {
	if cycleID == "" {
		return
	}
	store, err := NewEvidenceStore(appCfg.Evidence, &defaultRedactor)
	if err != nil {
		return
	}
	_ = store.WriteVerification(cycleID, r.ID, "stdout", r.Stdout, dryRun)
	_ = store.WriteVerification(cycleID, r.ID, "stderr", r.Stderr, dryRun)
}

// gitEnv returns the process env enriched with a GitHub App installation token
// for a single `git` invocation. Only `gh`/`git` here; agents never see it.
func gitEnv(tok string) []string {
	return append(os.Environ(), "GH_TOKEN="+tok)
}

// pushBranch force-pushes the worktree's current HEAD to refs/heads/branch
// over https using the App installation token as basic-auth username
// (x-access-token). Force is safe: the branch is bot-owned and the worker is
// the only writer for it. Output is scrubbed of the token before any error
// bubbles up.
func pushBranch(ctx context.Context, tok string, in PublishInput) error {
	RegisterSecrets(tok)                 // §23: the exact installation token never reaches an error
	base := os.Getenv("KONOHA_GIT_BASE") // e2e override: local bare remote supports hermetic tier-1
	url := ""
	if base == "" {
		url = fmt.Sprintf("https://x-access-token:%s@github.com/%s/%s.git", tok, in.Owner, in.Repo)
	} else {
		url = fmt.Sprintf("%s/%s/%s.git", base, in.Owner, in.Repo)
	}
	// Push the local commit (or HEAD) to the target branch. Pushing a specific
	// SHA works from any checkout whose object DB contains it — in particular the
	// main repo, whose object DB the dev/repair worktree commits live in (§25).
	src := "HEAD"
	if in.CommitSHA != "" {
		src = in.CommitSHA
	}
	stdout, err := cmdWithEnv(ctx, gitEnv(tok), in.RepoPath,
		"git", "push", url, src+":refs/heads/"+in.Branch, "--force")
	if err != nil {
		return fmt.Errorf("git push %s: %s", in.Branch, strings.TrimSpace(Redact(stdout)))
	}
	activity.GetLogger(ctx).Info("pushed branch", "repo", in.Owner+"/"+in.Repo, "branch", in.Branch)
	return nil
}

// cmdWithEnv runs cmd in dir with combined output captured and returned.
func cmdWithEnv(ctx context.Context, env []string, dir string, name string, args ...string) (string, error) {
	c := exec.CommandContext(ctx, name, args...)
	c.Env = env
	c.Dir = dir
	out, err := c.CombinedOutput()
	return string(out), err
}

// validateGate runs repo validation and applies the §26 policy: validation must
// pass (or be explicitly skipped) before a PR may be published, unless the
// workflow grants AllowOnFailure. The failing output is always returned so the
// failure is recorded, never silently ignored.
func validateGate(in PublishInput) (PublishResult, error) {
	res := PublishResult{Branch: in.Branch}
	// #94: the wrapper records verification drift as evidence (never a verdict).
	ok, skipped, output, _, err := validateRepoWithDrift(in)
	if err != nil {
		return res, fmt.Errorf("run validation %q: %w", in.ValidationCmd, err)
	}
	res.Validated = ok
	res.ValidationSkipped = skipped
	res.ValidationOutput = output
	if !ok && !skipped && !in.AllowOnFailure {
		return res, fmt.Errorf("validation failed for %s: %s (§26, set AllowOnFailure to override)",
			in.Branch, summary(output))
	}
	return res, nil
}

// PublishDeveloperChange turns a locally committed change into a PR using the
// GitHub App token only: run validation (§26) → push branch → idempotent
// EnsurePullRequest.
func PublishDeveloperChange(ctx context.Context, in PublishInput) (PublishResult, error) {
	res, err := validateGate(in)
	if err != nil {
		activity.GetLogger(ctx).Error("validation failed, PR not published (§26)",
			"branch", in.Branch, "output", res.ValidationOutput)
		return res, err
	}
	if in.DryRun {
		activity.GetLogger(ctx).Info("dryRun: publish suppressed", "repo", in.Owner+"/"+in.Repo, "branch", in.Branch)
		return res, nil
	}

	tok, err := appToken(ctx)
	if err != nil {
		return res, err
	}

	if err := pushBranch(ctx, tok, in); err != nil {
		return res, err
	}

	// Record a policy-override validation failure on the PR itself (§26: must
	// be recorded, never silently ignored).
	body := in.Body
	if !res.Validated && !res.ValidationSkipped {
		body = "**konoha: validation failed; published with policy override (§26).**\n\n```\n" +
			summary(res.ValidationOutput) + "\n```\n\n" + body
	}

	pr, purl, err := EnsurePullRequest(ctx, in, body)
	if err != nil {
		activity.GetLogger(ctx).Error("ensure PR failed", "branch", in.Branch, "err", err)
		return res, err
	}
	res.PRNumber = pr
	res.PRURL = purl
	activity.GetLogger(ctx).Info("published PR", "repo", in.Owner+"/"+in.Repo,
		"branch", in.Branch, "pr", pr, "validated", res.Validated)
	return res, nil
}

// EnsurePullRequest idempotently creates or updates the PR for the head branch
// (§24): it looks up an existing open PR for the branch first (via gh with the
// App token) and edits it, only creating when none exists. Reuses the same
// activity for both paths so Temporal retries/duplicate sweeps converge.
func EnsurePullRequest(ctx context.Context, in PublishInput, body string) (int, string, error) {
	tok, err := appToken(ctx)
	if err != nil {
		return 0, "", err
	}
	repo := in.Owner + "/" + in.Repo

	// find existing open PR for this head branch (empty jq output = none)
	find := "gh pr list -R " + repo + " --head " + in.Branch +
		" --state open --json number,url --jq '.[0] | \"\\(.number) \\(.url)\"'"
	out, err := cmdWithEnv(ctx, gitEnv(tok), in.RepoPath, "sh", "-c", find)
	if err != nil {
		return 0, "", err
	}
	if nr := strings.TrimSpace(out); nr != "" {
		parts := strings.SplitN(nr, " ", 2)
		n := 0
		fmt.Sscan(parts[0], &n)
		edit := exec.CommandContext(ctx, "gh", "pr", "edit", fmt.Sprint(n), "-R", repo,
			"--title", in.Title, "--body", body)
		edit.Env = gitEnv(tok)
		if o, e := edit.CombinedOutput(); e != nil {
			// §24 robustness: the listed PR may have been force-pushed away /
			// closed between find and edit (or was stale from a prior failed
			// attempt). Treat "not found" as "no PR" and fall through to create
			// instead of failing the whole publish.
			if strings.Contains(strings.ToLower(string(o)), "no pull requests found") ||
				strings.Contains(strings.ToLower(string(o)), "not found") {
				activity.GetLogger(ctx).Info("listed PR gone, falling back to create",
					"repo", repo, "pr", n, "err", strings.TrimSpace(string(o)))
			} else {
				return 0, "", fmt.Errorf("gh pr edit: %s", strings.TrimSpace(string(o)))
			}
		} else {
			activity.GetLogger(ctx).Info("updated PR", "repo", repo, "pr", n)
			return n, parts[1], nil
		}
	}

	create := exec.CommandContext(ctx, "gh", "pr", "create", "-R", repo,
		"--title", in.Title, "--body", body, "--head", in.Branch)
	if in.BaseBranch != "" {
		create.Args = append(create.Args, "--base", in.BaseBranch)
	}
	create.Env = gitEnv(tok)
	var purl string
	if b, e := create.CombinedOutput(); e != nil {
		return 0, "", fmt.Errorf("gh pr create: %s", strings.TrimSpace(string(b)))
	} else {
		purl = strings.TrimSpace(string(b))
	}
	n := 0
	if i := strings.LastIndex(purl, "/pull/"); i >= 0 {
		fmt.Sscan(purl[i+6:], &n)
	}
	if n == 0 {
		return 0, "", fmt.Errorf("gh pr create: cannot parse PR number from %q", purl)
	}
	activity.GetLogger(ctx).Info("created PR", "repo", repo, "pr", n)
	return n, purl, nil
}

// summary trims command output to the first 12 non-empty lines so a failing
// validation is recorded without plastering a full build log onto a PR/error.
func summary(s string) string {
	var lines []string
	for _, l := range strings.Split(s, "\n") {
		if len(lines) == 12 {
			break
		}
		if t := strings.TrimSpace(l); t != "" {
			lines = append(lines, t)
		}
	}
	return strings.Join(lines, "\n")
}
