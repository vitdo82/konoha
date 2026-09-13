package konoha

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// §49 PR merge + GitHub CI status gate. The workflow ALWAYS runs this after a
// commit-bound approval, before the issue may close:
//
//	APPROVED → WaitForCI (if requireCI) → MergePullRequest → EnsureIssueState(CLOSED)
//
// A failing/never-green CI gate or a merge error is never a silent success: the
// workflow escalates to HUMAN_REQUIRED and the issue is NOT closed, so a closed
// issue always means its PR actually merged.
//
// Idempotency (§24): MergePullRequest probes the current PR state first and
// re-reads after a failed merge, so a lost response never double-merges.

// ghTokenRunner runs a `gh` command with the App installation token in its env
// (§23). Only merge/PR-state reads use it; package-level so tests simulate
// GitHub without network (see reconcile.go's ghRunner for the read-only seam).
var ghTokenRunner = func(ctx context.Context, tok string, args ...string) ([]byte, error) {
	c := exec.CommandContext(ctx, "gh", args...)
	c.Env = gitEnv(tok)
	return c.CombinedOutput()
}

// MergeRequest asks for the reviewed PR to be merged (§49). ExpectedSHA, when
// set, is passed to gh as --match-head-commit so a head that moved after the
// review (§15) is refused instead of merged.
type MergeRequest struct {
	Owner       string `json:"owner"`
	Repo        string `json:"repo"`
	PrNum       int    `json:"pr"`
	Strategy    string `json:"strategy"`              // squash|rebase|merge
	ExpectedSHA string `json:"expectedSha,omitempty"` // reviewed commit (§15)
	DryRun      bool   `json:"dryRun,omitempty"`
}

// MergeResult reports the merge outcome. Skipped is true only under DryRun (§42).
type MergeResult struct {
	Merged  bool   `json:"merged"`
	Skipped bool   `json:"skipped,omitempty"`
	Detail  string `json:"detail,omitempty"`
}

// MergePullRequest merges an approved PR exactly once using the App token. The
// strategy is config-driven (§49) and re-validated here (fail-closed).
func MergePullRequest(ctx context.Context, in MergeRequest) (MergeResult, error) {
	if in.DryRun {
		return MergeResult{Skipped: true, Detail: "dryRun"}, nil
	}
	if !ValidMergeStrategy(in.Strategy) {
		return MergeResult{}, fmt.Errorf("MergePullRequest: invalid strategy %q (want %q|%q|%q)",
			in.Strategy, mergeStrategySquash, mergeStrategyRebase, mergeStrategyMerge)
	}
	tok, err := appToken(ctx)
	if err != nil {
		return MergeResult{}, err
	}
	repo := in.Owner + "/" + in.Repo

	// §24 probe: already merged → no-op (retry/overlap safe, never double-merge).
	state, err := prState(ctx, tok, repo, in.PrNum)
	if err != nil {
		return MergeResult{}, err
	}
	if state == "MERGED" {
		return MergeResult{Merged: true, Detail: "already merged"}, nil
	}
	if state == "CLOSED" {
		return MergeResult{}, fmt.Errorf("MergePullRequest: %s#%d is closed without a merge", repo, in.PrNum)
	}

	args := []string{"pr", "merge", strconv.Itoa(in.PrNum), "-R", repo}
	switch in.Strategy {
	case mergeStrategySquash:
		args = append(args, "--squash")
	case mergeStrategyRebase:
		args = append(args, "--rebase")
	case mergeStrategyMerge:
		args = append(args, "--merge")
	}
	if in.ExpectedSHA != "" {
		args = append(args, "--match-head-commit", in.ExpectedSHA)
	}
	out, err := ghTokenRunner(ctx, tok, args...)
	if err != nil {
		// The merge may have landed but the response was lost (§24): re-read
		// before reporting failure so a retry never double-merges.
		if s, e2 := prState(ctx, tok, repo, in.PrNum); e2 == nil && s == "MERGED" {
			return MergeResult{Merged: true, Detail: "merged (confirmed on retry)"}, nil
		}
		return MergeResult{}, fmt.Errorf("gh pr merge %s#%d: %s: %w", repo, in.PrNum, strings.TrimSpace(string(out)), err)
	}
	return MergeResult{Merged: true}, nil
}

// prState reads the PR's GitHub state (OPEN|MERGED|CLOSED).
func prState(ctx context.Context, tok, repo string, num int) (string, error) {
	out, err := ghTokenRunner(ctx, tok, "pr", "view", strconv.Itoa(num), "-R", repo, "--json", "state")
	if err != nil {
		return "", fmt.Errorf("gh pr view %s#%d: %s: %w", repo, num, strings.TrimSpace(string(out)), err)
	}
	var v struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal(out, &v); err != nil {
		return "", fmt.Errorf("parse PR state %s#%d: %w", repo, num, err)
	}
	return strings.ToUpper(strings.TrimSpace(v.State)), nil
}

// CIRequest parameterizes the §49 CI status gate. RequireCI false (repo opted
// out) makes WaitForCI a no-op; MaxWait bounds the poll (Go duration).
type CIRequest struct {
	Owner     string `json:"owner"`
	Repo      string `json:"repo"`
	PrNum     int    `json:"pr"`
	RequireCI bool   `json:"requireCI"`
	MaxWait   string `json:"maxWait,omitempty"`
}

// CIStatus is the gate verdict. Green is never true unless every check passed.
type CIStatus struct {
	Green   bool   `json:"green"`
	Failed  bool   `json:"failed,omitempty"`  // at least one check failed/cancelled/unknown
	Pending bool   `json:"pending,omitempty"` // checks still running / never reported
	Skipped bool   `json:"skipped,omitempty"` // requireCI disabled
	Detail  string `json:"detail,omitempty"`
}

// ciPollInterval is how often WaitForCI re-runs `gh pr checks`.
const ciPollInterval = 5 * time.Second

// WaitForCI polls `gh pr checks` until every check passes or one fails, bounded
// by MaxWait. A timeout (still pending), a fail, or an unclassifiable check all
// return Green=false — the workflow escalates, never a silent success.
func WaitForCI(ctx context.Context, in CIRequest) (CIStatus, error) {
	if !in.RequireCI {
		return CIStatus{Green: true, Skipped: true, Detail: "requireCI disabled"}, nil
	}
	maxWait := 10 * time.Minute
	if d, err := time.ParseDuration(in.MaxWait); err == nil && d > 0 {
		maxWait = d
	}
	deadline := time.Now().Add(maxWait)
	for {
		st, err := checkCIOnce(ctx, in)
		if err != nil {
			return CIStatus{}, err
		}
		if st.Green || st.Failed {
			return st, nil
		}
		rem := time.Until(deadline)
		if rem <= 0 {
			st.Pending = true
			st.Detail = "timed out after " + maxWait.String() + ": " + st.Detail
			return st, nil
		}
		if rem > ciPollInterval {
			rem = ciPollInterval
		}
		select {
		case <-ctx.Done():
			return CIStatus{}, ctx.Err()
		case <-time.After(rem):
		}
	}
}

// checkCIOnce runs one `gh pr checks` poll and classifies the result. gh exits
// non-zero when checks are pending/failed but still prints the JSON body, so a
// command error with an empty body is the only retryable failure.
func checkCIOnce(ctx context.Context, in CIRequest) (CIStatus, error) {
	out, err := ghRunner(ctx, "pr", "checks", strconv.Itoa(in.PrNum),
		"-R", in.Owner+"/"+in.Repo, "--json", "bucket,name")
	if err != nil && strings.TrimSpace(string(out)) == "" {
		return CIStatus{}, fmt.Errorf("gh pr checks: %w", err)
	}
	var rows []struct {
		Bucket string `json:"bucket"`
		Name   string `json:"name"`
	}
	if err := json.Unmarshal(out, &rows); err != nil {
		return CIStatus{}, fmt.Errorf("parse gh pr checks: %w", err)
	}
	var ok, failed, pending, unknown int
	for _, r := range rows {
		switch strings.ToLower(strings.TrimSpace(r.Bucket)) {
		case "pass", "passing", "success", "skipping":
			ok++
		case "fail", "failing", "failure", "cancel", "cancelled", "error":
			failed++
		case "pending", "queued", "in_progress", "expected":
			pending++
		default:
			unknown++ // fail-closed: an unclassifiable check is not a pass
		}
	}
	switch {
	case failed > 0:
		return CIStatus{Failed: true, Detail: fmt.Sprintf("%d check(s) failed", failed)}, nil
	case unknown > 0:
		return CIStatus{Failed: true, Detail: fmt.Sprintf("%d check(s) with unknown status", unknown)}, nil
	case pending > 0 || ok == 0:
		return CIStatus{Pending: true, Detail: "checks pending or not yet reported"}, nil
	default:
		return CIStatus{Green: true, Detail: fmt.Sprintf("%d check(s) passed", ok)}, nil
	}
}
