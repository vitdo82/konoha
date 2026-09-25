package konoha

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/client"
)

// mkOwnedWorktree registers a real git worktree for repoPath and writes the
// matching konoha owner claim (workflowID). Returns the worktree dir.
func mkOwnedWorktree(t *testing.T, ctx context.Context, repoPath, repoName string, issue int, workflowID string) string {
	t.Helper()
	dir := worktreeDir(repoPath, repoName, issue)
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		t.Fatal(err)
	}
	mustRun(t, ctx, repoPath, "git", "worktree", "add", "--detach", dir, "HEAD")
	if err := writeOwner(dir, worktreeOwner{WorkflowID: workflowID, BaseSHA: "HEAD"}); err != nil {
		t.Fatal(err)
	}
	return dir
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// #93/AC6: an owned worktree is removed, independently verified, and the claim
// is cleared — Remaining drops to zero.
func TestCleanupWorktreeRemovesOwnedAndVerifies(t *testing.T) {
	ctx := context.Background()
	repo, _ := initGitRepo(t)
	dir := mkOwnedWorktree(t, ctx, repo, "r", 7, "wf-7")

	res, err := CleanupWorktree(ctx, CleanupRequest{RepoPath: repo, Repo: "r", IssueNum: 7, WorkflowID: "wf-7"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Attempted || res.Outcome != cleanupRemoved || !res.Verified || res.Remaining != 0 || res.Error != "" {
		t.Fatalf("cleanup result = %+v, want attempted/removed/verified/0", res)
	}
	if exists(dir) {
		t.Fatal("worktree dir still present after cleanup")
	}
	if exists(ownerPath(dir)) {
		t.Fatal("owner claim still present after cleanup")
	}
}

// #93/AC5: a directory with no owner claim (foreign) is never removed.
func TestCleanupWorktreeNeverRemovesUnclaimed(t *testing.T) {
	ctx := context.Background()
	repo, _ := initGitRepo(t)
	dir := filepath.Join(filepath.Dir(repo), ".konoha", "r-issue-7")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	res, err := CleanupWorktree(ctx, CleanupRequest{RepoPath: repo, Repo: "r", IssueNum: 7, WorkflowID: "wf-7"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Attempted || res.Outcome != cleanupSkipped {
		t.Fatalf("cleanup result = %+v, want skipped/not-attempted", res)
	}
	if !exists(dir) {
		t.Fatal("foreign unclaimed dir must never be removed")
	}
}

// #93: a worktree claimed by a DIFFERENT workflow is never removed.
func TestCleanupWorktreeNeverRemovesOtherWorkflow(t *testing.T) {
	ctx := context.Background()
	repo, _ := initGitRepo(t)
	dir := mkOwnedWorktree(t, ctx, repo, "r", 7, "wf-other")

	res, err := CleanupWorktree(ctx, CleanupRequest{RepoPath: repo, Repo: "r", IssueNum: 7, WorkflowID: "wf-7"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Attempted || res.Outcome != cleanupSkipped {
		t.Fatalf("cleanup result = %+v, want skipped", res)
	}
	if !exists(dir) {
		t.Fatal("worktree owned by another workflow must never be removed")
	}
	if res.Remaining != 1 {
		t.Fatalf("remaining = %d, want 1 (the untouched leftover is reported)", res.Remaining)
	}
}

// #93/AC4: a removal that cannot complete is recorded with an error and a
// non-zero Remaining (never silent), and the dir is left for a retry/sweep.
func TestCleanupWorktreeFailureRecorded(t *testing.T) {
	ctx := context.Background()
	repo, _ := initGitRepo(t)
	dir := mkOwnedWorktree(t, ctx, repo, "r", 7, "wf-7")
	base := filepath.Dir(dir) // the .konoha dir; read-only blocks unlink
	if err := os.Chmod(base, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(base, 0o700) })

	saved := appCfg
	appCfg.Worker.CleanupMaxAttempts = 1 // keep the failing-retry loop fast
	t.Cleanup(func() { appCfg = saved })

	res, err := CleanupWorktree(ctx, CleanupRequest{RepoPath: repo, Repo: "r", IssueNum: 7, WorkflowID: "wf-7"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Attempted || res.Outcome != cleanupFailed || res.Error == "" {
		t.Fatalf("cleanup = %+v, want attempted/failed with a recorded error", res)
	}
	if res.Remaining < 1 {
		t.Fatalf("remaining = %d, want the leftover reported", res.Remaining)
	}
	if !exists(dir) {
		t.Fatal("a failed cleanup must leave the dir for a retry/sweep")
	}
}

// #93: nothing to remove → absent + verified, never an error.
func TestCleanupWorktreeAbsent(t *testing.T) {
	ctx := context.Background()
	repo, _ := initGitRepo(t)
	res, err := CleanupWorktree(ctx, CleanupRequest{RepoPath: repo, Repo: "r", IssueNum: 7, WorkflowID: "wf-7"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Attempted || res.Outcome != cleanupAbsent || !res.Verified || res.Remaining != 0 {
		t.Fatalf("cleanup result = %+v, want absent/verified", res)
	}
}

// #93: the debug bypass disables cleanup and is honored only when explicitly set.
func TestCleanupWorktreeDebugBypass(t *testing.T) {
	ctx := context.Background()
	repo, _ := initGitRepo(t)
	dir := mkOwnedWorktree(t, ctx, repo, "r", 7, "wf-7")

	if cleanupDisabled() {
		t.Fatal("cleanup must be enabled by default (bypass is opt-in)")
	}
	t.Setenv("KONOHA_DISABLE_WORKTREE_CLEANUP", "1")
	if !cleanupDisabled() {
		t.Fatal("bypass env var must disable cleanup")
	}
	res, err := CleanupWorktree(ctx, CleanupRequest{RepoPath: repo, Repo: "r", IssueNum: 7, WorkflowID: "wf-7"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Attempted || res.Outcome != cleanupSkipped {
		t.Fatalf("bypassed cleanup = %+v, want skipped", res)
	}
	if !exists(dir) {
		t.Fatal("bypass must leave the worktree untouched")
	}
}

// #93: the orphan sweep removes only owned worktrees that are BOTH expired and
// whose owning workflow is gone; young, live, and foreign ones survive.
func TestSweepOrphanWorktrees(t *testing.T) {
	ctx := context.Background()
	repo, _ := initGitRepo(t)
	dead := mkOwnedWorktree(t, ctx, repo, "dead", 1, "wf-dead")
	alive := mkOwnedWorktree(t, ctx, repo, "alive", 2, "wf-alive")
	young := mkOwnedWorktree(t, ctx, repo, "young", 3, "wf-young")
	foreign := filepath.Join(filepath.Dir(repo), ".konoha", "foreign-issue-4")
	if err := os.MkdirAll(foreign, 0o700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	for _, d := range []string{dead, alive} { // expired; young keeps its fresh mtime
		if err := os.Chtimes(ownerPath(d), old, old); err != nil {
			t.Fatal(err)
		}
	}

	prev := workflowAlive
	workflowAlive = func(_ context.Context, id string) bool { return id == "wf-alive" }
	t.Cleanup(func() { workflowAlive = prev })

	res, err := SweepOrphanWorktrees(ctx, OrphanSweepRequest{RepoPath: repo, TTL: "24h"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Scanned != 3 || res.Swept != 1 || res.Remaining != 2 {
		t.Fatalf("sweep result = %+v, want scanned=3 swept=1 remaining=2", res)
	}
	if exists(dead) {
		t.Fatal("expired dead-owner worktree must be swept")
	}
	for _, keep := range []string{alive, young, foreign} {
		if !exists(keep) {
			t.Fatalf("worktree %s must not be swept", keep)
		}
	}
}

// #93: without a way to prove the owner is gone, the sweep never removes.
func TestSweepOrphanWorktreesUnverifiableNoSweep(t *testing.T) {
	ctx := context.Background()
	repo, _ := initGitRepo(t)
	dir := mkOwnedWorktree(t, ctx, repo, "r", 1, "wf-1")
	old := time.Now().Add(-72 * time.Hour)
	if err := os.Chtimes(ownerPath(dir), old, old); err != nil {
		t.Fatal(err)
	}
	prev := workflowAlive
	workflowAlive = nil
	t.Cleanup(func() { workflowAlive = prev })

	res, err := SweepOrphanWorktrees(ctx, OrphanSweepRequest{RepoPath: repo, TTL: "1h"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Swept != 0 || !exists(dir) {
		t.Fatalf("unverifiable sweep = %+v; worktree must survive", res)
	}
}

// #93/§42: the startup sweep is also disabled by the debug bypass.
func TestSweepOrphanWorktreesBypass(t *testing.T) {
	ctx := context.Background()
	repo, _ := initGitRepo(t)
	dir := mkOwnedWorktree(t, ctx, repo, "r", 1, "wf-1")
	old := time.Now().Add(-72 * time.Hour)
	if err := os.Chtimes(ownerPath(dir), old, old); err != nil {
		t.Fatal(err)
	}
	prev := workflowAlive
	workflowAlive = func(context.Context, string) bool { return false }
	t.Cleanup(func() { workflowAlive = prev })
	t.Setenv("KONOHA_DISABLE_WORKTREE_CLEANUP", "1")

	res, err := SweepOrphanWorktrees(ctx, OrphanSweepRequest{RepoPath: repo, TTL: "1h"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Scanned != 0 || !exists(dir) {
		t.Fatalf("bypassed sweep = %+v; worktree must survive", res)
	}
}

// #93/AC2/AC3: on a normal terminal the workflow really removes its owned
// worktree (independent framing: the dir is gone) and records the cleanup
// section in outcome.json, while the §6 terminal state is unchanged.
func TestWorkflowCleanupRecordedInOutcome(t *testing.T) {
	resetMetrics()
	root := t.TempDir()
	saved := appCfg
	appCfg.Evidence.Dir = root
	t.Cleanup(func() { appCfg = saved })

	env := newCycleEnv(t, EstimateResult{Complexity: 3, Confidence: "high"})
	mockSmallPath(env)
	env.RegisterActivity(CleanupWorktree) // real teardown
	env.RegisterActivity(WriteCycleEvidence)

	repoPath := t.TempDir()
	in := IssueInput{Owner: "o", Repo: "r", IssueNum: testIssue, RepoPath: repoPath,
		Lifecycle: LifecycleConfig{Mode: modeSupervisedAutonomous, DevelopmentMaxComplexity: 4}}
	env.ExecuteWorkflow(IssueCycleWorkflow, in)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	if got := terminalState(t, env); got != stateClosed {
		t.Fatalf("terminal = %q, want CLOSED (cleanup must not change state)", got)
	}
	if dir := worktreeDir(repoPath, "r", testIssue); exists(dir) {
		t.Fatalf("worktree %s should have been removed by workflow teardown", dir)
	}

	matches, err := filepath.Glob(filepath.Join(root, "cycles", "*", evidenceOutcome))
	if err != nil || len(matches) != 1 {
		t.Fatalf("expected exactly one outcome.json, got %v (err=%v)", matches, err)
	}
	b, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	var out CycleOutcome
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.Cleanup == nil {
		t.Fatal("outcome.json must carry the cleanup record (§93)")
	}
	if !out.Cleanup.Attempted || out.Cleanup.Outcome != cleanupRemoved || !out.Cleanup.Verified || out.Cleanup.Remaining != 0 {
		t.Fatalf("cleanup record = %+v, want attempted/removed/verified/0", out.Cleanup)
	}
	if out.TerminalState != stateClosed {
		t.Fatalf("outcome terminal = %q, want CLOSED", out.TerminalState)
	}
}

// #93/AC1: a cancelled IssueCycleWorkflow still truly removes its owned
// worktree — the deferred teardown runs on a DISCONNECTED context (a
// cancellation backstop) with the real, owner-verified activity.
func TestWorkflowCancellationStillCleansWorktree(t *testing.T) {
	resetMetrics()
	ctx := context.Background()
	repoPath, _ := initGitRepo(t)
	const wfID = "cancelled-run-7"
	dir := mkOwnedWorktree(t, ctx, repoPath, "r", testIssue, wfID)

	env := newCycleEnv(t, EstimateResult{Complexity: 3, Confidence: "high"})
	env.OnActivity(ApplyIssueLabels, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(EnsureManagedNote, mock.Anything, mock.Anything).Return(nil)
	// The developer hangs on the workflow clock so the delayed cancel wins.
	env.OnActivity(RunDeveloper, mock.Anything, mock.Anything).After(2*time.Hour).Return(DevelopResult{}, nil)
	env.RegisterActivity(CleanupWorktree) // real teardown
	env.SetStartWorkflowOptions(client.StartWorkflowOptions{ID: wfID})
	env.RegisterDelayedCallback(env.CancelWorkflow, time.Hour)

	in := IssueInput{Owner: "o", Repo: "r", IssueNum: testIssue, RepoPath: repoPath,
		Lifecycle: LifecycleConfig{Mode: modeSupervisedAutonomous, DevelopmentMaxComplexity: 4}}
	env.ExecuteWorkflow(IssueCycleWorkflow, in)

	if exists(dir) {
		t.Fatalf("cancelled workflow left its owned worktree %s behind (§93)", dir)
	}
}
