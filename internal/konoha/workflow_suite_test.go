package konoha

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/testsuite"
)

// --- Workflow tests (§41) via the Temporal testsuite -------------------------
//
// Agent activities (AnalyzeIssue/EstimateIssue/RunDeveloper/RunReviewer/
// RunRepair) shell out to `opencode`/`gh`, so they are mocked with testify
// Anything matchers (one per argument). RecordCycleMetrics runs for real; the
// tests assert the terminal §6 state via the "state" query handler.

const testIssue = 7

// newCycleEnv returns an env with the analysis/estimate mocks wired and
// RecordCycleMetrics registered, primed for the given estimate. It does NOT
// stub the #93 cleanup activity: callers either call mockCleanup or register
// the real CleanupWorktree activity (to assert a real teardown).
func newCycleEnv(t *testing.T, est EstimateResult) *testsuite.TestWorkflowEnvironment {
	t.Helper()
	env := (&testsuite.WorkflowTestSuite{}).NewTestWorkflowEnvironment()
	env.RegisterActivity(RecordCycleMetrics)
	env.RegisterActivity(RecordAuditEvent)        // §55: real; no-op when audit disabled
	env.RegisterWorkflow(PostMergeWorkflow)       // §50: registered for release-enabled tests
	env.RegisterWorkflow(FailureFeedbackWorkflow) // §53: registered for alerting-enabled tests

	analysis := IssueAnalysis{
		Title: "t", Body: "b", Summary: "s", Spec: specFallback,
		Requirements: []string{"r1"}, AcceptanceCriteria: []string{"Given c When d Then e"},
	}
	env.OnActivity(AnalyzeIssue, mock.Anything, mock.Anything).Return(analysis, nil)
	env.OnActivity(EstimateIssue, mock.Anything, mock.Anything, mock.Anything).Return(est, nil)
	return env
}

// mockCycleEnv is newCycleEnv plus the #93 teardown stub.
func mockCycleEnv(t *testing.T, est EstimateResult) *testsuite.TestWorkflowEnvironment {
	env := newCycleEnv(t, est)
	mockCleanup(env)
	mockOpenClaim(env)
	return env
}

// mockOpenClaim stubs the #141 claim-release checkpoint with a live claim, so a
// test that is not about reconciliation proceeds through publish and merge.
func mockOpenClaim(env *testsuite.TestWorkflowEnvironment) {
	env.OnActivity(FetchIssueSnapshot, mock.Anything, mock.Anything).Return(
		IssueSnapshot{State: "open", Labels: []string{labelManaged}}, nil)
}

// mockCleanup stubs the #93 teardown activity (nothing to remove in tests that
// mock the agent activities); tests that assert real teardown register the real
// activity instead.
func mockCleanup(env *testsuite.TestWorkflowEnvironment) {
	env.OnActivity(CleanupWorktree, mock.Anything, mock.Anything).Return(
		CleanupResult{Outcome: cleanupAbsent, Verified: true}, nil)
}

// createClaimedWorktree materializes the deterministic worktree dir + owner
// claim for a develop request, so a test can exercise the workflow's real
// teardown (the mocked agent activity does not run the real one).
func createClaimedWorktree(in DevelopRequest) {
	dir := worktreeDir(filepath.Clean(in.RepoPath), in.Repo, in.IssueNum)
	_ = os.MkdirAll(dir, 0o700)
	_ = writeOwner(dir, worktreeOwner{WorkflowID: in.WorkflowID, BaseSHA: "testbase"})
}

// smallPathHooks counts the side-effecting calls a test cares about.
type smallPathHooks struct {
	merges, closes int
}

// mockSmallPath wires the dev→publish→review activities for a clean APPROVE
// with CI green and a successful merge.
func mockSmallPath(env *testsuite.TestWorkflowEnvironment) *smallPathHooks {
	return mockSmallPathWith(env, CIStatus{Green: true}, nil, MergeResult{Merged: true}, nil)
}

// mockSmallPathWith wires the SMALL path but lets a test force the §49 CI gate
// verdict/error and the merge outcome (mergeErr != nil simulates a merge
// failure).
func mockSmallPathWith(env *testsuite.TestWorkflowEnvironment, ci CIStatus, ciErr error, merged MergeResult, mergeErr error) *smallPathHooks {
	h := &smallPathHooks{}
	mockOpenClaim(env) // #141: the SMALL path runs the claim checkpoint before publish/merge
	env.OnActivity(FetchIssueLabels, mock.Anything, mock.Anything).Return([]string{"enhancement"}, nil)
	env.OnActivity(RunDeveloper, mock.Anything, mock.Anything).Return(func(_ context.Context, in DevelopRequest) (DevelopResult, error) {
		createClaimedWorktree(in) // #93: the real activity would leave this for workflow teardown
		return DevelopResult{
			WorktreeDir: worktreeDir(filepath.Clean(in.RepoPath), in.Repo, in.IssueNum),
			CommitSHA:   "devsha", ChangedFiles: []string{"pkg/worker.go"},
			Summary: "change", Validated: true,
			Evidence: &AgentEvidence{Prompt: "dev prompt", Stdout: "dev stdout", Stderr: "dev stderr"},
		}, nil
	})
	env.OnActivity(PublishDeveloperChange, mock.Anything, mock.Anything).Return(PublishResult{
		Branch: "konoha/issue-7", PRNumber: 101, PRURL: "https://x/pr/101", Validated: true,
	}, nil)
	env.OnActivity(FetchPRHead, mock.Anything, mock.Anything).Return("headsha", nil)
	env.OnActivity(FetchPRDiff, mock.Anything, mock.Anything).Return("+1 line", nil)
	env.OnActivity(RunReviewer, mock.Anything, mock.Anything).Return(ReviewResult{
		Verdict: "APPROVE", CommitSHA: "headsha", PRNumber: 101,
		Evidence: &AgentEvidence{Prompt: "review prompt", Stdout: "review stdout"},
	}, nil)
	// §49: CI gate verdict, then the merge. A non-green CI must never merge.
	env.OnActivity(WaitForCI, mock.Anything, mock.Anything).Return(ci, ciErr)
	env.OnActivity(MergePullRequest, mock.Anything, mock.Anything).Return(func(_ context.Context, in MergeRequest) (MergeResult, error) {
		h.merges++
		return merged, mergeErr
	})
	env.OnActivity(EnsureIssueState, mock.Anything, mock.Anything).Return(func(_ context.Context, s StateRequest) error {
		if s.State == "closed" {
			h.closes++
		}
		return nil
	})
	return h
}

// runAndAssert executes the workflow and asserts it completed without error.
func runAndAssert(t *testing.T, env *testsuite.TestWorkflowEnvironment) {
	t.Helper()
	in := IssueInput{Owner: "o", Repo: "r", IssueNum: testIssue, RepoPath: t.TempDir(),
		Lifecycle: LifecycleConfig{Mode: modeSupervisedAutonomous, DevelopmentMaxComplexity: 4}}
	env.ExecuteWorkflow(IssueCycleWorkflow, in)
	if env.IsWorkflowCompleted() {
		if err := env.GetWorkflowError(); err != nil {
			t.Fatalf("workflow error: %v", err)
		}
	}
}

func terminalState(t *testing.T, env *testsuite.TestWorkflowEnvironment) string {
	t.Helper()
	query, err := env.QueryWorkflow("state")
	if err != nil {
		t.Fatalf("query state: %v", err)
	}
	var s string
	if err := query.Get(&s); err != nil {
		t.Fatalf("query decode: %v", err)
	}
	return s
}

// §46/§41: SMALL happy path — dev → publish → APPROVE → merge → close, exactly
// one PR and exactly one merge.
func TestWorkflowSmallAutonomousClose(t *testing.T) {
	resetMetrics()
	env := mockCycleEnv(t, EstimateResult{Complexity: 3, Confidence: "high"})
	h := mockSmallPath(env)
	runAndAssert(t, env)

	if got := terminalState(t, env); got != stateClosed {
		t.Fatalf("small autonomous path terminal = %q, want CLOSED", got)
	}
	if v := sink.get(mCyclesCompletedAuton); v != 1 {
		t.Fatalf("autonomous completes = %v, want 1", v)
	}
	// §49: the issue closes only after exactly one merge.
	if h.merges != 1 || h.closes != 1 {
		t.Fatalf("merge/close counts = %d/%d, want 1/1", h.merges, h.closes)
	}
}

// #83/§41: one cycle lands the durable artifact set on disk (prompt, stdout,
// diff, outcome at minimum) for the mocked agent activities.
func TestWorkflowEvidenceArtifacts(t *testing.T) {
	resetMetrics()
	root := t.TempDir()
	saved := appCfg
	appCfg.Evidence.Dir = root
	t.Cleanup(func() { appCfg = saved })

	env := mockCycleEnv(t, EstimateResult{Complexity: 3, Confidence: "high"})
	mockSmallPath(env)
	env.RegisterActivity(WriteCycleEvidence) // real writer
	runAndAssert(t, env)

	if got := terminalState(t, env); got != stateClosed {
		t.Fatalf("terminal = %q, want CLOSED", got)
	}
	matches, err := filepath.Glob(filepath.Join(root, "cycles", "*", evidencePrompt))
	if err != nil || len(matches) != 1 {
		t.Fatalf("expected exactly one cycle evidence dir, got %v (err=%v)", matches, err)
	}
	dir := filepath.Dir(matches[0])
	for _, name := range []string{evidencePrompt, evidenceStdout, evidenceDiff, evidenceOutcome} {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || strings.TrimSpace(string(b)) == "" {
			t.Fatalf("artifact %s must exist and be non-empty (err=%v)", name, err)
		}
	}
}

// #83/§40/§41: an evidence-write failure never changes the authoritative
// terminal state — the cycle still closes.
func TestWorkflowEvidenceWriteFailureNonFatal(t *testing.T) {
	resetMetrics()
	env := mockCycleEnv(t, EstimateResult{Complexity: 3, Confidence: "high"})
	mockSmallPath(env)
	env.OnActivity(WriteCycleEvidence, mock.Anything, mock.Anything).Return(errors.New("disk full"))
	runAndAssert(t, env)

	if got := terminalState(t, env); got != stateClosed {
		t.Fatalf("evidence failure changed terminal = %q, want CLOSED", got)
	}
}

// #84/AC4: outcome.json's terminalState byte-equals the authoritative §6 state
// the workflow records (queried), and the explicit agent/verification outcomes
// + ordered conditions are present.
func TestWorkflowOutcomeEvidenceTerminalMatchesState(t *testing.T) {
	resetMetrics()
	root := t.TempDir()
	saved := appCfg
	appCfg.Evidence.Dir = root
	t.Cleanup(func() { appCfg = saved })

	env := mockCycleEnv(t, EstimateResult{Complexity: 3, Confidence: "high"})
	mockSmallPath(env)
	env.RegisterActivity(WriteCycleEvidence)
	runAndAssert(t, env)

	want := terminalState(t, env)
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
		t.Fatalf("outcome.json must decode: %v", err)
	}
	if out.TerminalState != want {
		t.Fatalf("outcome.json terminalState = %q, want the §6 state %q", out.TerminalState, want)
	}
	if out.AgentOutcome != agentOutcomeSuccess || out.VerificationOutcome != verificationSuccess {
		t.Fatalf("outcome = %+v, want agent SUCCESS / verification SUCCESS", out)
	}
	if len(out.Conditions) != 3 || out.Conditions[2].Status != want {
		t.Fatalf("conditions = %+v, want ordered terminal condition %q", out.Conditions, want)
	}
}

// #84/AC3: an agent that reports BLOCKED routes to the human path — it never
// publishes, never enters the repair loop, and never closes (§40).
func TestWorkflowBlockedAgentRoutesHumanRequired(t *testing.T) {
	resetMetrics()
	env := mockCycleEnv(t, EstimateResult{Complexity: 3, Confidence: "high"})
	env.OnActivity(FetchIssueLabels, mock.Anything, mock.Anything).Return([]string{"enhancement"}, nil)
	env.OnActivity(RunDeveloper, mock.Anything, mock.Anything).Return(DevelopResult{
		WorktreeDir: "/tmp/wt", Blocked: true, BlockedReason: "spec contradicts repo",
		Evidence: &AgentEvidence{Prompt: "p", Stdout: "s"},
	}, nil)
	published, repaired, closed := 0, 0, 0
	env.OnActivity(PublishDeveloperChange, mock.Anything, mock.Anything).Return(
		func(context.Context, PublishInput) (PublishResult, error) { published++; return PublishResult{}, nil })
	env.OnActivity(RunRepair, mock.Anything, mock.Anything).Return(
		func(context.Context, RepairRequest) (DevelopResult, error) { repaired++; return DevelopResult{}, nil })
	env.OnActivity(EnsureIssueState, mock.Anything, mock.Anything).Return(
		func(context.Context, StateRequest) error { closed++; return nil })

	runAndAssert(t, env)
	if got := terminalState(t, env); got != stateHumanRequired {
		t.Fatalf("blocked agent terminal = %q, want HUMAN_REQUIRED", got)
	}
	if published != 0 || repaired != 0 || closed != 0 {
		t.Fatalf("blocked agent must not publish/repair/close (pub=%d repair=%d close=%d)", published, repaired, closed)
	}
	if v := sink.get(mAgentOutcomePrefix + "blocked"); v != 1 {
		t.Fatalf("agent_outcome_blocked = %v, want 1", v)
	}
}

// §22/§41: the ownership transition posts the claim note exactly once, and a
// label-projection failure never aborts the cycle (§40: no silent success, but
// also no fail-open on an unrelated best-effort error).
func TestWorkflowManagedClaimNoteOnceAndLabelFailureNonFatal(t *testing.T) {
	resetMetrics()
	env := mockCycleEnv(t, EstimateResult{Complexity: 3, Confidence: "high"})
	h := mockSmallPath(env)

	var claimed int
	var got ManagedNoteRequest
	env.OnActivity(EnsureManagedNote, mock.Anything, mock.Anything).Return(func(_ context.Context, in ManagedNoteRequest) error {
		claimed++
		got = in
		return nil
	})
	env.OnActivity(ApplyIssueLabels, mock.Anything, mock.Anything).Return(errors.New("label API down"))

	runAndAssert(t, env)

	if got := terminalState(t, env); got != stateClosed {
		t.Fatalf("terminal = %q, want CLOSED despite label failure", got)
	}
	if claimed != 1 {
		t.Fatalf("claim activity invocations = %d, want exactly 1", claimed)
	}
	if got.Route != stateSmall || got.Complexity != 3 || got.Model != defaultAgentModel || got.IssueNum != testIssue {
		t.Fatalf("claim request = %+v, want SMALL/3/%s/issue %d", got, defaultAgentModel, testIssue)
	}
	if h.merges != 1 {
		t.Fatalf("merges = %d, want 1 (label failure must not abort the cycle)", h.merges)
	}
}

// §49/§41: CI not green → PR is NEVER merged and the issue is NOT closed; the
// workflow escalates to HUMAN_REQUIRED (no silent success).
func TestWorkflowCIFailureEscalates(t *testing.T) {
	resetMetrics()
	env := mockCycleEnv(t, EstimateResult{Complexity: 3, Confidence: "high"})
	h := mockSmallPathWith(env, CIStatus{Failed: true, Detail: "1 check failed"}, nil, MergeResult{}, nil)
	runAndAssert(t, env)

	if got := terminalState(t, env); got != stateHumanRequired {
		t.Fatalf("CI-failure terminal = %q, want HUMAN_REQUIRED", got)
	}
	if h.merges != 0 || h.closes != 0 {
		t.Fatalf("CI failure must not merge or close (merges=%d closes=%d)", h.merges, h.closes)
	}
	if v := sink.get(mHumanEscalations); v < 1 {
		t.Fatalf("CI failure must count as a human escalation, got %v", v)
	}
}

// §49/§40: an exhausted CI activity (persistent error) also escalates instead of
// failing the workflow — never a silent success.
func TestWorkflowCIActivityErrorEscalates(t *testing.T) {
	resetMetrics()
	env := mockCycleEnv(t, EstimateResult{Complexity: 3, Confidence: "high"})
	h := mockSmallPathWith(env, CIStatus{}, errors.New("gh unavailable"), MergeResult{}, nil)
	runAndAssert(t, env)

	if got := terminalState(t, env); got != stateHumanRequired {
		t.Fatalf("CI activity error terminal = %q, want HUMAN_REQUIRED", got)
	}
	if h.merges != 0 || h.closes != 0 {
		t.Fatalf("CI activity error must not merge or close (merges=%d closes=%d)", h.merges, h.closes)
	}
}

// §49/§41: merge error after a green CI → issue stays open, escalates.
func TestWorkflowMergeFailureEscalates(t *testing.T) {
	resetMetrics()
	env := mockCycleEnv(t, EstimateResult{Complexity: 3, Confidence: "high"})
	h := mockSmallPathWith(env, CIStatus{Green: true}, nil, MergeResult{}, errors.New("branch protection"))
	runAndAssert(t, env)

	if got := terminalState(t, env); got != stateHumanRequired {
		t.Fatalf("merge-failure terminal = %q, want HUMAN_REQUIRED", got)
	}
	if h.closes != 0 {
		t.Fatalf("failed merge must never close the issue (closes=%d)", h.closes)
	}
}

// §46/§41: one REQUEST_CHANGES then APPROVE → repair round once, close.
func TestWorkflowSmallRepairLoop(t *testing.T) {
	resetMetrics()
	env := mockCycleEnv(t, EstimateResult{Complexity: 3, Confidence: "high"})
	env.OnActivity(FetchIssueLabels, mock.Anything, mock.Anything).Return([]string{"bug"}, nil)
	env.OnActivity(RunDeveloper, mock.Anything, mock.Anything).Return(DevelopResult{
		WorktreeDir: "/tmp/wt", CommitSHA: "devsha", ChangedFiles: []string{"pkg/worker.go"}, Summary: "c", Validated: true,
	}, nil)

	// first publish
	first := true
	env.OnActivity(PublishDeveloperChange, mock.Anything, mock.Anything).Return(func(ctx context.Context, in PublishInput) (PublishResult, error) {
		_ = ctx
		if first {
			first = false
		}
		return PublishResult{Branch: "konoha/issue-7", PRNumber: 101, Validated: true}, nil
	})
	env.OnActivity(FetchPRHead, mock.Anything, mock.Anything).Return("headsha", nil)
	env.OnActivity(FetchPRDiff, mock.Anything, mock.Anything).Return("+1 line", nil)
	// first review → REQUEST_CHANGES, then APPROVE
	calls := 0
	env.OnActivity(RunReviewer, mock.Anything, mock.Anything).Return(func(ctx context.Context, r ReviewRequest) (ReviewResult, error) {
		_ = ctx
		calls++
		if calls == 1 {
			return ReviewResult{Verdict: "REQUEST_CHANGES", CommitSHA: "headsha", PRNumber: 101,
				Blocking: []string{"auth gap"}}, nil
		}
		return ReviewResult{Verdict: "APPROVE", CommitSHA: "headsha", PRNumber: 101}, nil
	})
	// repair runs once
	env.OnActivity(RunRepair, mock.Anything, mock.Anything).Return(DevelopResult{
		WorktreeDir: "/tmp/wt2", CommitSHA: "repairsha", ChangedFiles: []string{"pkg/worker.go"}, Summary: "fixed", Validated: true,
	}, nil)
	env.OnActivity(WaitForCI, mock.Anything, mock.Anything).Return(CIStatus{Green: true}, nil)
	env.OnActivity(MergePullRequest, mock.Anything, mock.Anything).Return(MergeResult{Merged: true}, nil)
	env.OnActivity(EnsureIssueState, mock.Anything, mock.Anything).Return(nil)

	runAndAssert(t, env)
	if got := terminalState(t, env); got != stateClosed {
		t.Fatalf("repair-loop path terminal = %q, want CLOSED", got)
	}
	if calls != 2 {
		t.Fatalf("reviewer called %d times, want 2", calls)
	}
}

// §46/§41/§16: REQUEST_CHANGES past the cap → BLOCKED, never close.
func TestWorkflowSmallRepairCapBlocked(t *testing.T) {
	resetMetrics()
	env := mockCycleEnv(t, EstimateResult{Complexity: 3, Confidence: "high"})
	env.OnActivity(FetchIssueLabels, mock.Anything, mock.Anything).Return([]string{"bug"}, nil)
	env.OnActivity(RunDeveloper, mock.Anything, mock.Anything).Return(DevelopResult{
		WorktreeDir: "/tmp/wt", CommitSHA: "devsha", ChangedFiles: []string{"pkg/worker.go"}, Summary: "c", Validated: true,
	}, nil)
	// always fails review, cap 2 (default) → attempt 2 escalates.
	env.OnActivity(PublishDeveloperChange, mock.Anything, mock.Anything).Return(PublishResult{Branch: "b", PRNumber: 101, Validated: true}, nil)
	env.OnActivity(FetchPRHead, mock.Anything, mock.Anything).Return("headsha", nil)
	env.OnActivity(FetchPRDiff, mock.Anything, mock.Anything).Return("+1 line", nil)
	env.OnActivity(RunReviewer, mock.Anything, mock.Anything).Return(ReviewResult{
		Verdict: "REQUEST_CHANGES", CommitSHA: "headsha", PRNumber: 101, Blocking: []string{"nope"},
	}, nil)
	env.OnActivity(RunRepair, mock.Anything, mock.Anything).Return(DevelopResult{
		WorktreeDir: "/tmp/wt2", CommitSHA: "repairsha", ChangedFiles: []string{"pkg/worker.go"}, Summary: "f", Validated: true,
	}, nil)
	// EnsureIssueState(CLOSED) must NEVER run.
	closed := 0
	env.OnActivity(EnsureIssueState, mock.Anything, mock.Anything).Return(func(ctx context.Context, s StateRequest) error {
		_ = ctx
		if s.State == "closed" {
			closed++
		}
		return nil
	})

	runAndAssert(t, env)
	if got := terminalState(t, env); got != stateBlocked {
		t.Fatalf("cap path terminal = %q, want BLOCKED", got)
	}
	if closed != 0 {
		t.Fatalf("EnsureIssueState(closed) called %d times; cap must never close (§40)", closed)
	}
}

// §46/§20: dev diff touching a restricted path → HUMAN_REQUIRED, no publish.
func TestWorkflowSmallRiskGateHumanRequired(t *testing.T) {
	resetMetrics()
	lc := LifecycleConfig{Mode: modeSupervisedAutonomous, DevelopmentMaxComplexity: 4,
		Autonomy: Autonomy{HumanRequiredPaths: []string{"infra/**"}}}
	env := mockCycleEnv(t, EstimateResult{Complexity: 3, Confidence: "high"})
	env.OnActivity(AnalyzeIssue, mock.Anything, mock.Anything).Return(IssueAnalysis{
		Title: "t", Body: "b", Summary: "s", Requirements: []string{"r1"}, AcceptanceCriteria: []string{"c"}, Spec: specFallback,
	}, nil)
	env.OnActivity(EstimateIssue, mock.Anything, mock.Anything, mock.Anything).Return(EstimateResult{Complexity: 3, Confidence: "high"}, nil)
	env.OnActivity(FetchIssueLabels, mock.Anything, mock.Anything).Return([]string{"enhancement"}, nil)
	env.OnActivity(RunDeveloper, mock.Anything, mock.Anything).Return(DevelopResult{
		WorktreeDir: "/tmp/wt", CommitSHA: "devsha", ChangedFiles: []string{"infra/prod.yaml"}, Summary: "c", Validated: true,
	}, nil)
	published := 0
	env.OnActivity(PublishDeveloperChange, mock.Anything, mock.Anything).Return(func(ctx context.Context, in PublishInput) (PublishResult, error) {
		_ = ctx
		published++
		return PublishResult{}, nil
	})

	in := IssueInput{Owner: "o", Repo: "r", IssueNum: testIssue, RepoPath: t.TempDir(), Lifecycle: lc}
	env.ExecuteWorkflow(IssueCycleWorkflow, in)
	if env.IsWorkflowCompleted() {
		if err := env.GetWorkflowError(); err != nil {
			t.Fatalf("workflow error: %v", err)
		}
	}
	if got := terminalState(t, env); got != stateHumanRequired {
		t.Fatalf("risk-gate terminal = %q, want HUMAN_REQUIRED", got)
	}
	if published != 0 {
		t.Fatalf("publish called %d times; §20 must suppress publish before mutation", published)
	}
}

// §8/§19: LARGE route splits → creates child issues → each child runs the real
// SMALL IssueCycleWorkflow (mocked activities) → all complete → parent CLOSED.
func TestWorkflowLargeSplitChildrenClose(t *testing.T) {
	resetMetrics()
	env := (&testsuite.WorkflowTestSuite{}).NewTestWorkflowEnvironment()
	env.RegisterActivity(RecordCycleMetrics)
	mockCleanup(env)
	analysis := IssueAnalysis{Title: "t", Body: "b", Summary: "s", Spec: specFallback,
		Requirements: []string{"r1"}, AcceptanceCriteria: []string{"Given c When d Then e"}}
	env.OnActivity(AnalyzeIssue, mock.Anything, mock.Anything).Return(analysis, nil)
	// parent (issue testIssue) is LARGE; children (any other issue #) are SMALL.
	env.OnActivity(EstimateIssue, mock.Anything, mock.Anything, mock.Anything).Return(
		func(_ context.Context, in IssueInput, a IssueAnalysis) (EstimateResult, error) {
			if in.IssueNum == testIssue {
				return EstimateResult{Complexity: 7, Confidence: "medium"}, nil
			}
			return EstimateResult{Complexity: 2, Confidence: "high"}, nil
		})
	env.OnActivity(RunSplit, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(SplitResult{
		Tickets: []SplitTicket{{Key: "01", Title: "child-a", Complexity: 2, Requirements: []string{"r"}, AcceptanceCriteria: []string{"c"}}},
	}, nil)
	env.OnActivity(EnsureChildren, mock.Anything, mock.Anything).Return([]ChildIssue{
		{Key: "01", ParentIssueNumber: testIssue, GitHubIssueNumber: 101, Complexity: 2, Title: "child-a"},
	}, nil)
	env.OnActivity(EnsureIssueState, mock.Anything, mock.Anything).Return(nil)
	// Child issue #101 runs the real SMALL path: dev → publish → APPROVE → close.
	mockSmallPath(env) // activity mocks are global (mock.Anything), reused by child

	runAndAssert(t, env)
	if got := terminalState(t, env); got != stateClosed {
		t.Fatalf("large route terminal = %q, want CLOSED", got)
	}
	if v := sink.get(mSplitLarge); v != 1 {
		t.Fatalf("issue_route_large = %v, want 1", int(v))
	}
}

// §8/§40: a child failure fails the parent into BLOCKED (no silent skip).
func TestWorkflowLargeChildFailureBlocksParent(t *testing.T) {
	resetMetrics()
	env := (&testsuite.WorkflowTestSuite{}).NewTestWorkflowEnvironment()
	env.RegisterActivity(RecordCycleMetrics)
	mockCleanup(env)
	analysis := IssueAnalysis{Title: "t", Body: "b", Summary: "s", Spec: specFallback,
		Requirements: []string{"r1"}, AcceptanceCriteria: []string{"Given c When d Then e"}}
	env.OnActivity(AnalyzeIssue, mock.Anything, mock.Anything).Return(analysis, nil)
	env.OnActivity(EstimateIssue, mock.Anything, mock.Anything, mock.Anything).Return(
		func(_ context.Context, in IssueInput, a IssueAnalysis) (EstimateResult, error) {
			if in.IssueNum == testIssue {
				return EstimateResult{Complexity: 7, Confidence: "medium"}, nil
			}
			return EstimateResult{Complexity: 2, Confidence: "high"}, nil
		})
	env.OnActivity(RunSplit, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(SplitResult{
		Tickets: []SplitTicket{{Key: "01", Title: "child-a", Complexity: 2, Requirements: []string{"r"}, AcceptanceCriteria: []string{"c"}}},
	}, nil)
	env.OnActivity(EnsureChildren, mock.Anything, mock.Anything).Return([]ChildIssue{
		{Key: "01", ParentIssueNumber: testIssue, GitHubIssueNumber: 101, Complexity: 2, Title: "child-a"},
	}, nil)
	env.OnActivity(EnsureIssueState, mock.Anything, mock.Anything).Return(nil)
	// Child runs the real SMALL path but its RunDeveloper fails → child fails →
	// parent must BLOCK (never silently continue).
	env.OnActivity(FetchIssueLabels, mock.Anything, mock.Anything).Return([]string{"enhancement"}, nil)
	env.OnActivity(RunDeveloper, mock.Anything, mock.Anything).Return(DevelopResult{}, errors.New("dev failed"))
	env.OnActivity(FetchPRHead, mock.Anything, mock.Anything).Return("headsha", nil)
	env.OnActivity(FetchPRDiff, mock.Anything, mock.Anything).Return("+1", nil)
	env.OnActivity(RunReviewer, mock.Anything, mock.Anything).Return(ReviewResult{Verdict: "APPROVE", CommitSHA: "headsha", PRNumber: 101}, nil)
	env.OnActivity(PublishDeveloperChange, mock.Anything, mock.Anything).Return(PublishResult{PRNumber: 101}, nil)

	runAndAssert(t, env)
	if got := terminalState(t, env); got != stateBlocked {
		t.Fatalf("child failure must block parent, got %q", got)
	}
}

// §50/§49: after a successful merge+close, an enabled repo fires PostMergeWorkflow
// exactly once (and the release failure can never change the terminal state).
func TestWorkflowSmallPostMergeRelease(t *testing.T) {
	resetMetrics()
	env := mockCycleEnv(t, EstimateResult{Complexity: 3, Confidence: "high"})
	mockSmallPath(env)
	releases := 0
	env.OnActivity(RunRelease, mock.Anything, mock.Anything).Return(
		func(context.Context, ReleaseRequest) (ReleaseResult, error) {
			releases++
			return ReleaseResult{Tag: "v1.0.1", Bump: bumpPatch}, nil
		})

	in := IssueInput{Owner: "o", Repo: "r", IssueNum: testIssue, RepoPath: t.TempDir(),
		Lifecycle: LifecycleConfig{Mode: modeSupervisedAutonomous, DevelopmentMaxComplexity: 4,
			Release: ReleaseConfig{Enabled: true}}}
	env.ExecuteWorkflow(IssueCycleWorkflow, in)
	if env.IsWorkflowCompleted() {
		if err := env.GetWorkflowError(); err != nil {
			t.Fatalf("workflow error: %v", err)
		}
	}
	if got := terminalState(t, env); got != stateClosed {
		t.Fatalf("release path terminal = %q, want CLOSED", got)
	}
	if releases != 1 {
		t.Fatalf("release activity ran %d times, want 1", releases)
	}
}

// §55/§41: with the publish step gated, the pipeline proceeds only AFTER an
// approval decision is read; the gate runs and the cycle still closes.
func TestWorkflowApprovalGateGrants(t *testing.T) {
	resetMetrics()
	env := mockCycleEnv(t, EstimateResult{Complexity: 3, Confidence: "high"})
	mockSmallPath(env)
	gates := 0
	env.OnActivity(AwaitApproval, mock.Anything, mock.Anything).Return(
		func(context.Context, ApprovalRequest) (ApprovalResult, error) {
			gates++
			return ApprovalResult{Granted: true, Action: humanDecisionApprove}, nil
		})

	in := IssueInput{Owner: "o", Repo: "r", IssueNum: testIssue, RepoPath: t.TempDir(),
		Lifecycle: LifecycleConfig{Mode: modeSupervisedAutonomous, DevelopmentMaxComplexity: 4,
			Approval: ApprovalConfig{PreGate: true, RequiredSteps: []string{approvalStepPublish}}}}
	env.ExecuteWorkflow(IssueCycleWorkflow, in)
	if env.IsWorkflowCompleted() {
		if err := env.GetWorkflowError(); err != nil {
			t.Fatalf("workflow error: %v", err)
		}
	}
	if got := terminalState(t, env); got != stateClosed {
		t.Fatalf("approved gate terminal = %q, want CLOSED", got)
	}
	if gates != 1 {
		t.Fatalf("approval gate ran %d times, want 1", gates)
	}
}

// §55/§40: a denied approval NEVER publishes — it escalates to HUMAN_REQUIRED.
func TestWorkflowApprovalGateDenied(t *testing.T) {
	resetMetrics()
	env := mockCycleEnv(t, EstimateResult{Complexity: 3, Confidence: "high"})
	env.OnActivity(FetchIssueLabels, mock.Anything, mock.Anything).Return([]string{"enhancement"}, nil)
	env.OnActivity(RunDeveloper, mock.Anything, mock.Anything).Return(DevelopResult{
		WorktreeDir: "/tmp/wt", CommitSHA: "devsha", ChangedFiles: []string{"pkg/worker.go"}, Validated: true,
	}, nil)
	published := 0
	env.OnActivity(PublishDeveloperChange, mock.Anything, mock.Anything).Return(
		func(context.Context, PublishInput) (PublishResult, error) {
			published++
			return PublishResult{Branch: "b", PRNumber: 101, Validated: true}, nil
		})
	env.OnActivity(AwaitApproval, mock.Anything, mock.Anything).Return(
		ApprovalResult{Denied: true, Action: humanDecisionDeny}, nil)

	in := IssueInput{Owner: "o", Repo: "r", IssueNum: testIssue, RepoPath: t.TempDir(),
		Lifecycle: LifecycleConfig{Mode: modeSupervisedAutonomous, DevelopmentMaxComplexity: 4,
			Approval: ApprovalConfig{PreGate: true}}}
	env.ExecuteWorkflow(IssueCycleWorkflow, in)
	if env.IsWorkflowCompleted() {
		if err := env.GetWorkflowError(); err != nil {
			t.Fatalf("workflow error: %v", err)
		}
	}
	if got := terminalState(t, env); got != stateHumanRequired {
		t.Fatalf("denied gate terminal = %q, want HUMAN_REQUIRED", got)
	}
	if published != 0 {
		t.Fatalf("denied approval must not publish (published=%d)", published)
	}
}

// §108/§55: a request-changes decision at the publish gate runs a bounded repair
// carrying the human feedback and then re-gates; an approval on the re-gate lets
// the pipeline proceed. The gate is consulted twice and nothing is silently
// approved.
func TestWorkflowApprovalGateRequestChangesReworksThenGrants(t *testing.T) {
	resetMetrics()
	env := mockCycleEnv(t, EstimateResult{Complexity: 3, Confidence: "high"})
	mockSmallPath(env)
	gates, repairs := 0, 0
	var repairReq RepairRequest
	env.OnActivity(AwaitApproval, mock.Anything, mock.Anything).Return(
		func(context.Context, ApprovalRequest) (ApprovalResult, error) {
			gates++
			if gates == 1 {
				return ApprovalResult{Rework: true, Action: humanDecisionRequestChanges,
					Feedback: "add a regression test\n", Attempts: 1,
					Comments: []DiffComment{{File: "pkg/worker.go", Line: 12, Side: "new", SHA: "devsha", Body: "guard the nil case"}}}, nil
			}
			return ApprovalResult{Granted: true, Action: humanDecisionApprove, Attempts: 2}, nil
		})
	env.OnActivity(RunRepair, mock.Anything, mock.Anything).Return(
		func(_ context.Context, in RepairRequest) (DevelopResult, error) {
			repairs++
			repairReq = in
			return DevelopResult{CommitSHA: "sha2", ChangedFiles: []string{"pkg/worker.go"},
				Validated: true, Evidence: &AgentEvidence{Prompt: "repair", Stdout: "ok"}}, nil
		})

	in := IssueInput{Owner: "o", Repo: "r", IssueNum: testIssue, RepoPath: t.TempDir(),
		Lifecycle: LifecycleConfig{Mode: modeSupervisedAutonomous, DevelopmentMaxComplexity: 4,
			Approval: ApprovalConfig{PreGate: true, RequiredSteps: []string{approvalStepPublish}}}}
	env.ExecuteWorkflow(IssueCycleWorkflow, in)
	if env.IsWorkflowCompleted() {
		if err := env.GetWorkflowError(); err != nil {
			t.Fatalf("workflow error: %v", err)
		}
	}
	if got := terminalState(t, env); got != stateClosed {
		t.Fatalf("rework-then-approve terminal = %q, want CLOSED", got)
	}
	if gates != 2 || repairs != 1 {
		t.Fatalf("gates/repairs = %d/%d, want 2/1", gates, repairs)
	}
	// The human feedback AND the diff-anchored comment are delivered to the repair
	// agent as ordered, deterministic BLOCKING findings (#113).
	wantBlocking := []string{"add a regression test", "pkg/worker.go:12 (new): guard the nil case"}
	if !reflect.DeepEqual(repairReq.Review.Blocking, wantBlocking) {
		t.Fatalf("repair blocking = %#v, want %#v", repairReq.Review.Blocking, wantBlocking)
	}
	if repairReq.PreviousCommit != "devsha" {
		t.Fatalf("repair must build on the reviewed commit, got %q", repairReq.PreviousCommit)
	}
}

// §55/§40: a gate timeout stays BLOCKED and never publishes (no silent success).
func TestWorkflowApprovalGateTimeoutBlocks(t *testing.T) {
	resetMetrics()
	env := mockCycleEnv(t, EstimateResult{Complexity: 3, Confidence: "high"})
	env.OnActivity(FetchIssueLabels, mock.Anything, mock.Anything).Return([]string{"enhancement"}, nil)
	env.OnActivity(RunDeveloper, mock.Anything, mock.Anything).Return(DevelopResult{
		WorktreeDir: "/tmp/wt", CommitSHA: "devsha", ChangedFiles: []string{"pkg/worker.go"}, Validated: true,
	}, nil)
	published := 0
	env.OnActivity(PublishDeveloperChange, mock.Anything, mock.Anything).Return(
		func(context.Context, PublishInput) (PublishResult, error) {
			published++
			return PublishResult{}, nil
		})
	env.OnActivity(AwaitApproval, mock.Anything, mock.Anything).Return(
		ApprovalResult{TimedOut: true}, nil)

	in := IssueInput{Owner: "o", Repo: "r", IssueNum: testIssue, RepoPath: t.TempDir(),
		Lifecycle: LifecycleConfig{Mode: modeSupervisedAutonomous, DevelopmentMaxComplexity: 4,
			Approval: ApprovalConfig{PreGate: true, RequiredSteps: []string{approvalStepPublish}}}}
	env.ExecuteWorkflow(IssueCycleWorkflow, in)
	if env.IsWorkflowCompleted() {
		if err := env.GetWorkflowError(); err != nil {
			t.Fatalf("workflow error: %v", err)
		}
	}
	if got := terminalState(t, env); got != stateBlocked {
		t.Fatalf("timed-out gate terminal = %q, want BLOCKED", got)
	}
	if published != 0 {
		t.Fatalf("timed-out approval must not publish (published=%d)", published)
	}
}

// --- §40/§44 #1/§53: sweep child-failure escalation --------------------------

// sweepLifecycle returns the per-repo policy for the sweep tests (alerting and
// dry-run toggled).
func sweepLifecycle(alerting, dryRun bool) LifecycleConfig {
	lc := LifecycleConfig{Mode: modeSupervisedAutonomous, DevelopmentMaxComplexity: 4, DryRun: dryRun}
	if alerting {
		lc.Alerting = AlertingConfig{Enabled: true, Thresholds: map[string]float64{mAutonomousRate: 0.7}}
	}
	return lc
}

// sweepScope is the schedule input for IssueAllWorkflow.
func sweepScope(lc LifecycleConfig) RepoScope {
	return RepoScope{Owner: "o", Repo: "r", RepoPath: "/tmp/repo", Lifecycle: lc}
}

// newSweepEnv wires an IssueAllWorkflow env whose ListUnmanagedIssues returns
// the given issue numbers, with the child cycle + failure-feedback workflows
// registered.
func newSweepEnv(t *testing.T, lc LifecycleConfig, issues []int) *testsuite.TestWorkflowEnvironment {
	t.Helper()
	env := (&testsuite.WorkflowTestSuite{}).NewTestWorkflowEnvironment()
	env.RegisterWorkflow(IssueCycleWorkflow)
	env.RegisterWorkflow(FailureFeedbackWorkflow)
	env.RegisterActivity(RecordCycleMetrics)
	mockCleanup(env)
	var pending []IssueInput
	for _, n := range issues {
		pending = append(pending, IssueInput{
			Owner: "o", Repo: "r", IssueNum: n, RepoPath: t.TempDir(), Lifecycle: lc,
		})
	}
	env.OnActivity(ListUnmanagedIssues, mock.Anything, mock.Anything).Return(pending, nil)
	return env
}

const sweepAnalysisTitle = "t"

// §40/§44 #1 + §53/§41: a failing sweep child is surfaced, not silently skipped
// — the affected issue is projected human-required/blocked (via ApplyIssueLabels)
// and exactly one failure issue is filed for the run, keyed by the stable child
// workflow ID. The sweep itself still returns (a child failure is not a sweep
// failure).
func TestWorkflowSweepChildFailureEscalates(t *testing.T) {
	resetMetrics()
	lc := sweepLifecycle(true, false)
	env := newSweepEnv(t, lc, []int{testIssue})
	env.OnActivity(AnalyzeIssue, mock.Anything, mock.Anything).Return(IssueAnalysis{}, errors.New("model returned no JSON"))

	projected := 0
	env.OnActivity(ApplyIssueLabels, mock.Anything, mock.Anything).Return(
		func(_ context.Context, in LabelsRequest) error {
			projected++
			if !containsString(in.Labels, labelHumanRequired) || !containsString(in.Labels, labelBlocked) {
				t.Errorf("failed child projection = %v, want human-required+blocked", in.Labels)
			}
			return nil
		})
	filed := 0
	var outcome CycleOutcome
	env.OnActivity(EnsureFailureIssue, mock.Anything, mock.Anything).Return(
		func(_ context.Context, in FailureFeedbackRequest) (FailureIssueResult, error) {
			filed++
			outcome = in.Outcome
			return FailureIssueResult{Number: 55, Created: true}, nil
		})

	env.ExecuteWorkflow(IssueAllWorkflow, sweepScope(lc))
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("a child failure must not fail the sweep: %v", err)
	}
	if projected != 1 {
		t.Fatalf("human-required/blocked projection issued %d times, want 1", projected)
	}
	if filed != 1 || outcome.WorkflowID != "r-issue-7" || outcome.TerminalState != stateBlocked {
		t.Fatalf("failure feedback = filed %d outcome %+v, want 1/r-issue-7/BLOCKED", filed, outcome)
	}
	// §39: the failed run is recorded (parity with the LARGE path's recordTerminal).
	if v := sink.get(mCyclesBlocked); v != 1 {
		t.Fatalf("blocked cycles = %v, want 1", v)
	}
	if v := sink.get(mHumanEscalations); v != 1 {
		t.Fatalf("human escalations = %v, want 1", v)
	}
}

// §40/§41: one child failing must not abort the batch — a sibling still runs the
// full small path and closes exactly once, while the failed issue escalates.
func TestWorkflowSweepChildFailureKeepsBatch(t *testing.T) {
	resetMetrics()
	lc := sweepLifecycle(false, false)
	env := newSweepEnv(t, lc, []int{testIssue, testIssue + 1})
	env.OnActivity(AnalyzeIssue, mock.Anything, mock.Anything).Return(
		func(_ context.Context, in IssueInput) (IssueAnalysis, error) {
			if in.IssueNum == testIssue {
				return IssueAnalysis{}, errors.New("model returned no JSON")
			}
			return IssueAnalysis{
				Title: sweepAnalysisTitle, Body: "b", Summary: "s", Spec: specFallback,
				Requirements: []string{"r1"}, AcceptanceCriteria: []string{"Given c When d Then e"},
			}, nil
		})
	env.OnActivity(EstimateIssue, mock.Anything, mock.Anything, mock.Anything).Return(
		EstimateResult{Complexity: 3, Confidence: "high"}, nil)
	env.OnActivity(ApplyIssueLabels, mock.Anything, mock.Anything).Return(nil)
	h := mockSmallPathWith(env, CIStatus{Green: true}, nil, MergeResult{Merged: true}, nil)

	env.ExecuteWorkflow(IssueAllWorkflow, sweepScope(lc))
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("sweep must complete: %v", err)
	}
	if h.merges != 1 || h.closes != 1 {
		t.Fatalf("sibling must still close (merges=%d closes=%d), want 1/1", h.merges, h.closes)
	}
}

// §42/§41: under DryRun a child failure mutates nothing — no label write and no
// failure issue — but the run still completes (the failure is only logged), and
// evidence lands under the dryrun/ namespace.
func TestWorkflowSweepChildFailureDryRun(t *testing.T) {
	resetMetrics()
	root := t.TempDir()
	saved := appCfg
	appCfg.Evidence.Dir = root
	t.Cleanup(func() { appCfg = saved })

	lc := sweepLifecycle(true, true)
	env := newSweepEnv(t, lc, []int{testIssue})
	env.OnActivity(AnalyzeIssue, mock.Anything, mock.Anything).Return(IssueAnalysis{}, errors.New("no JSON"))
	// Real activities: all must short-circuit on DryRun before touching GitHub.
	env.RegisterActivity(ApplyIssueLabels)
	env.RegisterActivity(EnsureFailureIssue)
	env.RegisterActivity(WriteCycleEvidence)
	prev := ghRunner
	ghRunner = func(context.Context, ...string) ([]byte, error) {
		t.Fatal("DryRun must not mutate GitHub")
		return nil, nil
	}
	t.Cleanup(func() { ghRunner = prev })

	env.ExecuteWorkflow(IssueAllWorkflow, sweepScope(lc))
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("dry-run sweep must complete: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "dryrun", "cycles", "r-issue-7", evidenceOutcome)); err != nil {
		t.Fatalf("dry-run evidence missing under dryrun/: %v", err)
	}
}

// §53/§24/§41: two sweeps over the same failed run file exactly one failure
// issue — the real idempotent EnsureFailureIssue matches the byte-exact marker
// on the second pass.
func TestWorkflowSweepFailureFeedbackIdempotent(t *testing.T) {
	resetMetrics()
	prev := ghRunner
	var calls []string
	filed := false
	ghRunner = func(_ context.Context, args ...string) ([]byte, error) {
		line := strings.Join(args, " ")
		calls = append(calls, line)
		switch {
		case strings.Contains(line, "issues?state=all"):
			if filed {
				return json.Marshal([]ghItem{{Number: 42, Body: failureMarker("r-issue-7"), State: "open"}})
			}
			return json.Marshal([]ghItem{})
		case strings.HasPrefix(line, "issue create"):
			filed = true
			return []byte("https://github.com/o/r/issues/42"), nil
		default:
			return nil, fmt.Errorf("unexpected gh call: %s", line)
		}
	}
	t.Cleanup(func() { ghRunner = prev })

	lc := sweepLifecycle(true, false)
	for i := 0; i < 2; i++ {
		env := newSweepEnv(t, lc, []int{testIssue})
		env.RegisterActivity(EnsureFailureIssue)
		env.OnActivity(AnalyzeIssue, mock.Anything, mock.Anything).Return(IssueAnalysis{}, errors.New("no JSON"))
		env.OnActivity(ApplyIssueLabels, mock.Anything, mock.Anything).Return(nil)
		env.ExecuteWorkflow(IssueAllWorkflow, sweepScope(lc))
		if err := env.GetWorkflowError(); err != nil {
			t.Fatalf("sweep %d error: %v", i, err)
		}
	}
	if n := len(createdIssueCalls(calls)); n != 1 {
		t.Fatalf("two sweeps created %d failure issues, want exactly 1 (§24)", n)
	}
}

// §55/§51: a denied deploy approval never deploys and reopens the issue to
// HUMAN_REQUIRED (the post-merge human surface); it is never a silent success.
func TestWorkflowDeployApprovalGateDenied(t *testing.T) {
	resetMetrics()
	env := (&testsuite.WorkflowTestSuite{}).NewTestWorkflowEnvironment()
	env.RegisterWorkflow(PostMergeWorkflow)
	env.RegisterActivity(RecordAuditEvent)
	env.OnActivity(AwaitApproval, mock.Anything, mock.Anything).Return(
		ApprovalResult{Denied: true, Action: humanDecisionDeny}, nil)
	deployed := 0
	env.OnActivity(DeployRelease, mock.Anything, mock.Anything).Return(
		func(context.Context, DeployRequest) (DeployResult, error) {
			deployed++
			return DeployResult{Deployed: true}, nil
		})
	reopened := 0
	env.OnActivity(EnsureIssueState, mock.Anything, mock.Anything).Return(
		func(context.Context, StateRequest) error {
			reopened++
			return nil
		})
	env.OnActivity(ApplyIssueLabels, mock.Anything, mock.Anything).Return(nil)

	in := PostMergeInput{Owner: "o", Repo: "r", PrNum: 101, IssueNum: testIssue, RepoPath: t.TempDir(),
		Lifecycle: LifecycleConfig{Deploy: DeployConfig{Enabled: true, Command: "true"},
			Approval: ApprovalConfig{PreGate: true, RequiredSteps: []string{approvalStepDeploy}}}}
	env.ExecuteWorkflow(PostMergeWorkflow, in)
	if !env.IsWorkflowCompleted() {
		t.Fatal("workflow did not complete")
	}
	if err := env.GetWorkflowError(); err == nil {
		t.Fatal("denied deploy gate must surface an error, never a silent success")
	}
	if deployed != 0 {
		t.Fatalf("denied deploy gate must not deploy (deployed=%d)", deployed)
	}
	if reopened == 0 {
		t.Fatal("denied deploy gate must reopen the issue to HUMAN_REQUIRED")
	}
}
