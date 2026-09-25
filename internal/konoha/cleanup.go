package konoha

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// #93: disconnect-safe worktree teardown + durable cleanup record.
//
// The developer/repair activities already remove their worktree on the normal
// path (deferred, so a failed attempt cleans up too). This file adds the
// STRUCTURAL teardown the workflow owns: an idempotent, owner-verified
// CleanupWorktree activity the workflow schedules on every exit path (including
// cancellation, via workflow.NewDisconnectedContext) and an orphan sweep for the
// crash case where no cleanup ever ran. Cleanup is best-effort: a failure is
// recorded on outcome.json but never changes the authoritative §6 terminal
// state (§40/§44 #20).

// Cleanup outcome values (a closed set; the `cleanup.outcome` field).
const (
	cleanupRemoved = "removed" // owned worktree existed → removed + verified gone
	cleanupAbsent  = "absent"  // nothing to remove; verified gone
	cleanupSkipped = "skipped" // foreign/unclaimed dir, or the debug bypass
	cleanupFailed  = "failed"  // removal attempted but verification still shows it
)

// defaultOrphanWorktreeTTL is how long a konoha-owned worktree must be idle
// before the startup sweep will consider it orphaned.
const defaultOrphanWorktreeTTL = "24h"

// cleanupBackoffBase is the first retry delay; it doubles per attempt. Small so
// a stuck removal fails fast but transient locks get a few chances.
const cleanupBackoffBase = 200 * time.Millisecond

// CleanupResult is the durable teardown record persisted in outcome.json (#93).
// Attempted is true only when this call actually tried a removal (false for a
// foreign dir or the debug bypass). Verified is the independent post-condition:
// `git worktree list` no longer names dir AND it is gone from disk. Remaining is
// the count of konoha-owned worktrees still present under the repo's .konoha
// dir after the attempt — a leftover is reported, never hidden.
type CleanupResult struct {
	Attempted bool   `json:"attempted"`
	Outcome   string `json:"outcome"`
	Verified  bool   `json:"verified"`
	Remaining int    `json:"remaining"`
	Error     string `json:"error,omitempty"`
}

// CleanupRequest identifies the worktree teardown target. Dir is optional: when
// empty it is derived from RepoPath+Repo+IssueNum (the deterministic path).
type CleanupRequest struct {
	RepoPath   string `json:"repoPath"`
	Dir        string `json:"dir,omitempty"`
	Repo       string `json:"repo,omitempty"`
	IssueNum   int    `json:"issue,omitempty"`
	WorkflowID string `json:"workflowId"`
}

// cleanupDisabled is the #93 test/debug escape hatch. It mirrors the existing
// KONOHA_TOKEN_FIXED convention: honored only when the operator explicitly sets
// the env var, and never set in a real deploy (the variable simply is not
// present there). It disables BOTH the workflow teardown and the orphan sweep.
func cleanupDisabled() bool {
	return os.Getenv("KONOHA_DISABLE_WORKTREE_CLEANUP") == "1"
}

// resolveCleanupDir returns the explicit dir or the deterministic worktree path.
func resolveCleanupDir(in CleanupRequest) (string, error) {
	if strings.TrimSpace(in.Dir) != "" {
		return filepath.Clean(in.Dir), nil
	}
	if strings.TrimSpace(in.Repo) == "" || in.IssueNum == 0 {
		return "", fmt.Errorf("CleanupRequest needs dir or repo+issue")
	}
	return worktreeDir(filepath.Clean(in.RepoPath), in.Repo, in.IssueNum), nil
}

// CleanupWorktree is the idempotent, owner-verified teardown activity (#93). A
// directory without a konoha claim, or claimed by a different workflow, is NEVER
// removed — it returns skipped. Removal is retried (configurable attempts, with
// backoff) because it is idempotent, then independently verified via
// `git worktree list` + an on-disk stat. It always returns a result (never a
// non-nil error for a handled failure) so the workflow can persist a durable
// record; the §6 terminal state is never touched. Never call activity.GetLogger
// here: unit tests use a non-Temporal context (it panics).
func CleanupWorktree(ctx context.Context, in CleanupRequest) (CleanupResult, error) {
	if cleanupDisabled() {
		return CleanupResult{Outcome: cleanupSkipped}, nil
	}
	repoPath := filepath.Clean(in.RepoPath)
	dir, err := resolveCleanupDir(in)
	if err != nil {
		return CleanupResult{Outcome: cleanupFailed, Error: err.Error()}, nil
	}

	owner, ok := readOwner(dir)
	if !ok {
		// No claim → not ours (foreign), or already gone.
		if _, statErr := os.Stat(dir); os.IsNotExist(statErr) {
			return CleanupResult{
				Outcome:   cleanupAbsent,
				Verified:  verifyWorktreeGone(ctx, repoPath, dir),
				Remaining: len(konohaOwnedWorktrees(repoPath)),
			}, nil
		}
		return CleanupResult{Outcome: cleanupSkipped, Remaining: len(konohaOwnedWorktrees(repoPath))}, nil
	}
	if owner.WorkflowID != in.WorkflowID {
		return CleanupResult{Outcome: cleanupSkipped, Remaining: len(konohaOwnedWorktrees(repoPath))}, nil
	}

	res := CleanupResult{Attempted: true}
	attempts := appCfg.Worker.cleanupAttempts()
	var lastErr error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			time.Sleep(cleanupBackoffBase << (i - 1))
		}
		rmErr := removeWorktree(ctx, repoPath, dir)
		res.Verified = verifyWorktreeGone(ctx, repoPath, dir)
		if rmErr == nil && res.Verified {
			res.Outcome = cleanupRemoved
			_ = os.Remove(ownerPath(dir))
			res.Remaining = len(konohaOwnedWorktrees(repoPath))
			return res, nil
		}
		if rmErr != nil {
			lastErr = rmErr
		} else {
			lastErr = fmt.Errorf("worktree %s still present after removal", dir)
		}
	}
	res.Outcome = cleanupFailed
	if lastErr != nil {
		res.Error = lastErr.Error()
	}
	res.Remaining = len(konohaOwnedWorktrees(repoPath))
	return res, nil
}

// verifyWorktreeGone is the independent post-condition: dir is absent from
// `git worktree list` on the main repo AND gone from disk.
func verifyWorktreeGone(ctx context.Context, repoPath, dir string) bool {
	target := filepath.Clean(dir)
	for _, w := range gitWorktreeList(ctx, repoPath) {
		if filepath.Clean(w) == target {
			return false
		}
	}
	if _, err := os.Stat(dir); err == nil {
		return false
	}
	return true
}

// gitWorktreeList parses `git worktree list --porcelain` for registered worktree
// paths (nil/empty on any error, e.g. a non-repo path).
func gitWorktreeList(ctx context.Context, repoPath string) []string {
	out, err := execGit(ctx, repoPath, "worktree", "list", "--porcelain")
	if err != nil {
		return nil
	}
	var dirs []string
	for _, line := range strings.Split(out, "\n") {
		if p, ok := strings.CutPrefix(line, "worktree "); ok {
			if p = strings.TrimSpace(p); p != "" {
				dirs = append(dirs, p)
			}
		}
	}
	return dirs
}

// konohaOwnedWorktrees returns the konoha-owned worktree dirs under repoPath's
// .konoha parent: directories carrying a valid `*.owner` claim marker. Sorted
// for deterministic counting/logging.
func konohaOwnedWorktrees(repoPath string) []string {
	base := filepath.Join(filepath.Dir(filepath.Clean(repoPath)), ".konoha")
	entries, err := os.ReadDir(base)
	if err != nil {
		return nil
	}
	var dirs []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(base, e.Name())
		if _, ok := readOwner(dir); ok {
			dirs = append(dirs, dir)
		}
	}
	sort.Strings(dirs)
	return dirs
}

// OrphanSweepRequest identifies one repo to sweep for stranded worktrees (#93).
// TTL overrides worker.orphanWorktreeTTL when set.
type OrphanSweepRequest struct {
	RepoPath string `json:"repoPath"`
	TTL      string `json:"ttl,omitempty"`
}

// OrphanSweepResult records what the sweep did. Remaining is the post-sweep
// owned count; Errors lists per-worktree removal failures.
type OrphanSweepResult struct {
	Scanned   int      `json:"scanned"`
	Swept     int      `json:"swept"`
	Remaining int      `json:"remaining"`
	Errors    []string `json:"errors,omitempty"`
}

// workflowAlive is the #93 test seam used by the orphan sweep to decide whether
// a claim's owning workflow still exists. It is nil by default: the sweep then
// never removes anything (fail-safe — an unverifiable claim is treated as live,
// never swept). Main() sets it from the Temporal client.
var workflowAlive func(ctx context.Context, workflowID string) bool

// SweepOrphanWorktrees removes konoha-owned worktrees whose owning workflow no
// longer exists and whose age exceeds the TTL (#93). A non-expired worktree or a
// foreign/unclaimed directory is never touched. With workflowAlive unset the
// sweep is a no-op (cannot prove the owner is gone). Never call
// activity.GetLogger here (non-Temporal ctx in unit tests).
func SweepOrphanWorktrees(ctx context.Context, in OrphanSweepRequest) (OrphanSweepResult, error) {
	var res OrphanSweepResult
	if cleanupDisabled() {
		return res, nil
	}
	repoPath := filepath.Clean(in.RepoPath)
	ttl := appCfg.Worker.orphanTTL()
	if s := strings.TrimSpace(in.TTL); s != "" {
		if d, err := time.ParseDuration(s); err == nil && d > 0 {
			ttl = d
		}
	}
	if workflowAlive == nil {
		res.Remaining = len(konohaOwnedWorktrees(repoPath))
		return res, nil
	}
	for _, dir := range konohaOwnedWorktrees(repoPath) {
		res.Scanned++
		owner, ok := readOwner(dir)
		if !ok {
			continue
		}
		age, err := ownerAge(dir)
		if err != nil || age <= ttl {
			continue // unknown/young → never sweep
		}
		if workflowAlive(ctx, owner.WorkflowID) {
			continue // owner still running → not an orphan
		}
		if err := removeWorktree(ctx, repoPath, dir); err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", dir, err))
			continue
		}
		_ = os.Remove(ownerPath(dir))
		res.Swept++
	}
	res.Remaining = len(konohaOwnedWorktrees(repoPath))
	return res, nil
}

// ownerAge is how long ago a worktree's claim marker was last written.
func ownerAge(dir string) (time.Duration, error) {
	fi, err := os.Stat(ownerPath(dir))
	if err != nil {
		return 0, err
	}
	return time.Since(fi.ModTime()), nil
}

// sweepOrphanWorktreesOnStart runs the #93 orphan sweep once at worker start for
// every configured repo. Best-effort: a missing/corrupt repos.yaml or a sweep
// error is logged, never fatal (a crashed worker's worktrees are a cleanup
// concern, not a startup gate).
func sweepOrphanWorktreesOnStart() {
	if cleanupDisabled() {
		return
	}
	repos, err := LoadReposConfig(defaultReposPath())
	if err != nil {
		log.Printf("orphan worktree sweep skipped: repos.yaml: %v", err)
		return
	}
	for _, r := range repos {
		res, err := SweepOrphanWorktrees(context.Background(), OrphanSweepRequest{RepoPath: r.RepoPath})
		if err != nil {
			log.Printf("orphan worktree sweep %s failed: %v", r.Repo, err)
			continue
		}
		if res.Scanned > 0 || res.Swept > 0 {
			log.Printf("orphan worktree sweep %s: scanned=%d swept=%d remaining=%d", r.Repo, res.Scanned, res.Swept, res.Remaining)
		}
	}
}
