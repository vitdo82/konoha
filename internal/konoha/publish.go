package konoha

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"go.temporal.io/sdk/activity"
)

// PublishInput describes a change the developer agent prepared locally. All
// GitHub mutations happen here with the App installation token only (§23, §25).
type PublishInput struct {
	Owner          string `json:"owner"`
	Repo           string `json:"repo"`
	RepoPath       string `json:"repoPath"`   // worktree holding the local commits
	Branch         string `json:"branch"`     // branch to push and open a PR for
	BaseBranch     string `json:"baseBranch"` // PR base (default: the repo default)
	Title          string `json:"title"`
	Body           string `json:"body"`
	ValidationCmd  string `json:"validationCmd"`  // repo validation command (§26); "" = none
	AllowOnFailure bool   `json:"allowOnFailure"` // explicit policy permission to publish despite failing validation (§26)
	DryRun         bool   `json:"dryRun,omitempty"`
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

// validateRepo runs the repo's validation command in the worktree. A
// configured command that exits non-zero means validation failed. Empty
// ValidationCmd means "nothing required", which reports as skipped (Validated
// stays false so callers can distinguish "not checked" from "checked+passed").
func validateRepo(in PublishInput) (ok, skipped bool, output string, err error) {
	cmd := strings.TrimSpace(in.ValidationCmd)
	if cmd == "" {
		return false, true, "", nil
	}
	c := exec.Command("sh", "-c", cmd)
	c.Dir = in.RepoPath
	out, runErr := c.CombinedOutput()
	return runErr == nil, false, string(out), nil
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
	url := fmt.Sprintf("https://x-access-token:%s@github.com/%s/%s.git", tok, in.Owner, in.Repo)
	stdout, err := cmdWithEnv(ctx, gitEnv(tok), in.RepoPath,
		"git", "push", url, "HEAD:refs/heads/"+in.Branch, "--force")
	if err != nil {
		scrubbed := strings.ReplaceAll(stdout, tok, "***")
		return fmt.Errorf("git push %s: %s", in.Branch, strings.TrimSpace(scrubbed))
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
	ok, skipped, output, err := validateRepo(in)
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
			return 0, "", fmt.Errorf("gh pr edit: %s", strings.TrimSpace(string(o)))
		}
		activity.GetLogger(ctx).Info("updated PR", "repo", repo, "pr", n)
		return n, parts[1], nil
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
