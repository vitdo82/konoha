package konoha

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// localFakes wires the local runtime's activity seam without gh/opencode/PATH.
// Tests override individual fields for the scenario they exercise.
type localFakes struct {
	acts     localActivities
	evidence []EvidenceRequest
	metrics  []CycleOutcome

	analyses, estimates, develops, repairs, publishes, reviews int
	merges, closes, cleanups, repairsPrevEmpty                 int
}

func newLocalFakes() *localFakes {
	f := &localFakes{}
	f.acts = localActivities{
		analyze: func(context.Context, IssueInput) (IssueAnalysis, error) {
			f.analyses++
			return IssueAnalysis{
				Title: "t", Body: "b", Summary: "s", Spec: specFallback,
				Requirements: []string{"r1"}, AcceptanceCriteria: []string{"Given c When d Then e"},
			}, nil
		},
		estimate: func(context.Context, IssueInput, IssueAnalysis) (EstimateResult, error) {
			f.estimates++
			return EstimateResult{Complexity: 3, Confidence: "high"}, nil
		},
		ensureManagedNote: func(context.Context, ManagedNoteRequest) error { return nil },
		applyLabels:       func(context.Context, LabelsRequest) error { return nil },
		fetchSnapshot: func(context.Context, IssueInput) (IssueSnapshot, error) {
			return IssueSnapshot{State: "open", Labels: []string{labelManaged}}, nil
		},
		fetchLabels: func(context.Context, IssueInput) ([]string, error) {
			return []string{"enhancement"}, nil
		},
		developer: func(_ context.Context, in DevelopRequest) (DevelopResult, error) {
			f.develops++
			return DevelopResult{
				WorktreeDir: worktreeDir(in.RepoPath, in.Repo, in.IssueNum),
				CommitSHA:   "devsha", BaseSHA: "basesha",
				ChangedFiles: []string{"pkg/worker.go"}, Summary: "change", Validated: true,
				Evidence: &AgentEvidence{Prompt: "dev prompt", Stdout: "dev stdout", Stderr: "dev stderr"},
			}, nil
		},
		repair: func(_ context.Context, in RepairRequest) (DevelopResult, error) {
			f.repairs++
			return DevelopResult{
				CommitSHA: "repairsha", BaseSHA: in.PreviousCommit,
				ChangedFiles: []string{"pkg/worker.go"}, Summary: "repaired", Validated: true,
				Evidence: &AgentEvidence{Prompt: "repair prompt", Stdout: "repair stdout"},
			}, nil
		},
		publish: func(context.Context, PublishInput) (PublishResult, error) {
			f.publishes++
			return PublishResult{Branch: "konoha/issue-7", PRNumber: 101, Validated: true}, nil
		},
		prHead: func(context.Context, PrInput) (string, error) { return "headsha", nil },
		prDiff: func(context.Context, PrInput) (string, error) { return "+1 line", nil },
		reviewer: func(context.Context, ReviewRequest) (ReviewResult, error) {
			f.reviews++
			return ReviewResult{Verdict: "APPROVE", CommitSHA: "headsha", PRNumber: 101,
				Evidence: &AgentEvidence{Prompt: "review prompt", Stdout: "review stdout"}}, nil
		},
		prDescription: func(context.Context, PRDescriptionRequest) (PRDescriptionResult, error) {
			return PRDescriptionResult{}, nil
		},
		waitCI: func(context.Context, CIRequest) (CIStatus, error) { return CIStatus{Green: true}, nil },
		merge: func(context.Context, MergeRequest) (MergeResult, error) {
			f.merges++
			return MergeResult{Merged: true}, nil
		},
		ensureIssueState: func(_ context.Context, in StateRequest) error {
			if in.State == "closed" {
				f.closes++
			}
			return nil
		},
		awaitApproval: func(context.Context, ApprovalRequest) (ApprovalResult, error) {
			return ApprovalResult{Granted: true}, nil
		},
		recordAudit: func(context.Context, AuditRequest) error { return nil },
		cleanup: func(context.Context, CleanupRequest) (CleanupResult, error) {
			f.cleanups++
			return CleanupResult{Outcome: cleanupAbsent, Verified: true}, nil
		},
		recordActivation: func(context.Context, ActivationRequest) error { return nil },
		recordChange:     func(context.Context, ChangeRequest) error { return nil },
		recordMetrics: func(_ context.Context, o CycleOutcome) error {
			f.metrics = append(f.metrics, o)
			return nil
		},
	}
	f.acts.writeEvidence = func(_ context.Context, req EvidenceRequest) error {
		f.evidence = append(f.evidence, req)
		return nil
	}
	return f
}

// localOutcome decodes the last outcome.json payload the runtime wrote.
func (f *localFakes) localOutcome(t *testing.T) CycleOutcome {
	t.Helper()
	for i := len(f.evidence) - 1; i >= 0; i-- {
		if f.evidence[i].Outcome == "" {
			continue
		}
		var out CycleOutcome
		if err := json.Unmarshal([]byte(f.evidence[i].Outcome), &out); err != nil {
			t.Fatalf("decode outcome: %v", err)
		}
		return out
	}
	t.Fatal("no outcome evidence written")
	return CycleOutcome{}
}

func localTestLifecycle() LifecycleConfig {
	return LifecycleConfig{
		Mode:                     modeSupervisedAutonomous,
		DevelopmentMaxComplexity: 4,
		Autonomy:                 Autonomy{ClosePolicy: closePolicyRiskBased},
	}
}

func withLocalAppCfg(t *testing.T) {
	t.Helper()
	saved := appCfg
	appCfg = defaultAppConfig()
	appCfg.Evidence.Dir = t.TempDir()
	savedWait := localRetryWait
	localRetryWait = func(context.Context, time.Duration) error { return nil }
	t.Cleanup(func() { appCfg = saved; localRetryWait = savedWait })
}

// #212: the local runtime runs the SMALL path in-process and records the same
// terminal evidence as the workflow.
func TestLocalRuntimeSmallHappyPath(t *testing.T) {
	withLocalAppCfg(t)
	f := newLocalFakes()
	rt := newLocalRuntime(appCfg, f.acts)
	in := IssueInput{Owner: "o", Repo: "r", IssueNum: 7, RepoPath: t.TempDir(), Trigger: "cli", Lifecycle: localTestLifecycle()}

	if err := rt.runCycle(context.Background(), in); err != nil {
		t.Fatalf("runCycle: %v", err)
	}
	out := f.localOutcome(t)
	if out.TerminalState != stateClosed || !out.AutonomousClose {
		t.Fatalf("terminal = %q autonomous=%v, want CLOSED/true", out.TerminalState, out.AutonomousClose)
	}
	if out.Attempts != 1 || out.ReviewRounds != 1 {
		t.Fatalf("attempts/reviewRounds = %d/%d, want 1/1", out.Attempts, out.ReviewRounds)
	}
	if out.PolicyDigest == "" || out.AgentOutcome != agentOutcomeSuccess || out.VerificationOutcome != verificationSuccess {
		t.Fatalf("outcome evidence incomplete: %+v", out)
	}
	if out.Cleanup == nil || out.Cleanup.Outcome != cleanupAbsent {
		t.Fatalf("cleanup = %+v, want absent", out.Cleanup)
	}
	if f.develops != 1 || f.publishes != 1 || f.reviews != 1 || f.merges != 1 || f.closes != 1 {
		t.Fatalf("calls dev/pub/rev/merge/close = %d/%d/%d/%d/%d, want 1 each",
			f.develops, f.publishes, f.reviews, f.merges, f.closes)
	}
	if f.repairs != 0 {
		t.Fatalf("repairs = %d, want 0", f.repairs)
	}
	if f.cleanups != 1 {
		t.Fatalf("cleanups = %d, want 1", f.cleanups)
	}
	if len(f.metrics) != 1 || f.metrics[0].TerminalState != stateClosed {
		t.Fatalf("metrics = %+v, want one CLOSED", f.metrics)
	}
}

// #212: in-process calls apply the same retry classes as Temporal: infrastructure
// retries, an agent run never replays, verification retries three times.
func TestLocalCallRetryClasses(t *testing.T) {
	withLocalAppCfg(t)
	f := newLocalFakes()
	rt := newLocalRuntime(appCfg, f.acts)

	attempts := 0
	err := rt.call(context.Background(), retryInfrastructure, func(context.Context) error {
		attempts++
		if attempts < 3 {
			return context.DeadlineExceeded
		}
		return nil
	})
	if err != nil || attempts != 3 {
		t.Fatalf("infrastructure attempts = %d err = %v, want 3/nil", attempts, err)
	}

	attempts = 0
	if err := rt.call(context.Background(), retryAgent, func(context.Context) error {
		attempts++
		return context.DeadlineExceeded
	}); err == nil || attempts != 1 {
		t.Fatalf("agent attempts = %d err = %v, want 1/error", attempts, err)
	}

	attempts = 0
	if err := rt.call(context.Background(), retryDeterministic, func(context.Context) error {
		attempts++
		return context.DeadlineExceeded
	}); err == nil || attempts != 1 {
		t.Fatalf("deterministic attempts = %d err = %v, want 1/error", attempts, err)
	}

	attempts = 0
	if err := rt.call(context.Background(), retryVerification, func(context.Context) error {
		attempts++
		return context.DeadlineExceeded
	}); err == nil || attempts != 3 {
		t.Fatalf("verification attempts = %d err = %v, want 3/error", attempts, err)
	}
}

// runLocalCycle executes one cycle against the fakes and returns its outcome.
func runLocalCycle(t *testing.T, f *localFakes, lc LifecycleConfig) (CycleOutcome, error) {
	t.Helper()
	rt := newLocalRuntime(appCfg, f.acts)
	in := IssueInput{Owner: "o", Repo: "r", IssueNum: 7, RepoPath: t.TempDir(), Trigger: "cli", Lifecycle: lc}
	err := rt.runCycle(context.Background(), in)
	if err != nil {
		return CycleOutcome{}, err
	}
	return f.localOutcome(t), nil
}

// #212: LARGE ends at NOT_WIRED in local mode; no split/child/developer call.
func TestLocalRouteLargeNotWired(t *testing.T) {
	withLocalAppCfg(t)
	f := newLocalFakes()
	f.acts.estimate = func(context.Context, IssueInput, IssueAnalysis) (EstimateResult, error) {
		return EstimateResult{Complexity: 5, Confidence: "high"}, nil
	}
	out, err := runLocalCycle(t, f, localTestLifecycle())
	if err != nil {
		t.Fatal(err)
	}
	if out.TerminalState != stateNotWired {
		t.Fatalf("terminal = %q, want NOT_WIRED", out.TerminalState)
	}
	if f.develops != 0 || f.publishes != 0 {
		t.Fatalf("LARGE must not develop/publish: dev=%d pub=%d", f.develops, f.publishes)
	}
}

// #212: the bounded repair loop caps at maxDevReviewAttempts and blocks, never
// treating REQUEST_CHANGES as success.
func TestLocalRepairLoopCapBlocks(t *testing.T) {
	withLocalAppCfg(t)
	f := newLocalFakes()
	f.acts.reviewer = func(context.Context, ReviewRequest) (ReviewResult, error) {
		f.reviews++
		return ReviewResult{Verdict: "REQUEST_CHANGES", Blocking: []string{"fix it"}, CommitSHA: "headsha"}, nil
	}
	lc := localTestLifecycle()
	lc.MaxDevReviewAttempts = 2
	out, err := runLocalCycle(t, f, lc)
	if err != nil {
		t.Fatal(err)
	}
	if out.TerminalState != stateBlocked {
		t.Fatalf("terminal = %q, want BLOCKED", out.TerminalState)
	}
	if f.repairs != 1 || f.reviews != 2 || f.publishes != 2 || f.closes != 0 {
		t.Fatalf("repair/reviews/publishes/closes = %d/%d/%d/%d, want 1/2/2/0",
			f.repairs, f.reviews, f.publishes, f.closes)
	}
}

// #212: the §20 risk gate escalates before any GitHub mutation.
func TestLocalRiskGateEscalates(t *testing.T) {
	withLocalAppCfg(t)
	f := newLocalFakes()
	f.acts.developer = func(_ context.Context, in DevelopRequest) (DevelopResult, error) {
		f.develops++
		return DevelopResult{CommitSHA: "devsha", ChangedFiles: []string{"infra/main.tf"}, Validated: true}, nil
	}
	lc := localTestLifecycle()
	lc.Autonomy.HumanRequiredPaths = []string{"infra/**"}
	out, err := runLocalCycle(t, f, lc)
	if err != nil {
		t.Fatal(err)
	}
	if out.TerminalState != stateHumanRequired || out.EscalationReason != reasonPolicyViolation {
		t.Fatalf("terminal/reason = %q/%q, want HUMAN_REQUIRED/%s", out.TerminalState, out.EscalationReason, reasonPolicyViolation)
	}
	if f.publishes != 0 {
		t.Fatalf("risk gate must suppress publish, got %d", f.publishes)
	}
}

// #212: a §55 gate that never receives a decision blocks and writes evidence.
func TestLocalApprovalTimeoutBlocks(t *testing.T) {
	withLocalAppCfg(t)
	f := newLocalFakes()
	f.acts.awaitApproval = func(context.Context, ApprovalRequest) (ApprovalResult, error) {
		return ApprovalResult{TimedOut: true}, nil
	}
	lc := localTestLifecycle()
	lc.Approval = ApprovalConfig{PreGate: true, RequiredSteps: []string{approvalStepPublish}}
	out, err := runLocalCycle(t, f, lc)
	if err != nil {
		t.Fatal(err)
	}
	if out.TerminalState != stateBlocked {
		t.Fatalf("terminal = %q, want BLOCKED", out.TerminalState)
	}
	if f.publishes != 0 {
		t.Fatalf("timed-out gate must not publish, got %d", f.publishes)
	}
	if len(f.evidence) == 0 {
		t.Fatal("a blocked gate must write terminal evidence")
	}
}

// #212: a denied §55 gate routes to HUMAN_REQUIRED and never mutates.
func TestLocalApprovalDenyHumanRequired(t *testing.T) {
	withLocalAppCfg(t)
	f := newLocalFakes()
	f.acts.awaitApproval = func(context.Context, ApprovalRequest) (ApprovalResult, error) {
		return ApprovalResult{Denied: true, Detail: "no"}, nil
	}
	lc := localTestLifecycle()
	lc.Approval = ApprovalConfig{PreGate: true, RequiredSteps: []string{approvalStepPublish}}
	out, err := runLocalCycle(t, f, lc)
	if err != nil {
		t.Fatal(err)
	}
	if out.TerminalState != stateHumanRequired || out.EscalationReason != reasonApprovalDenied {
		t.Fatalf("terminal/reason = %q/%q", out.TerminalState, out.EscalationReason)
	}
	if f.publishes != 0 {
		t.Fatalf("denied gate must not publish, got %d", f.publishes)
	}
}

// #212: a request-changes decision at the publish gate repairs with the human
// feedback and re-gates, mirroring the workflow.
func TestLocalApprovalReworkRepairs(t *testing.T) {
	withLocalAppCfg(t)
	f := newLocalFakes()
	calls := 0
	f.acts.awaitApproval = func(context.Context, ApprovalRequest) (ApprovalResult, error) {
		calls++
		if calls == 1 {
			return ApprovalResult{Rework: true, Feedback: "tighten the input check"}, nil
		}
		return ApprovalResult{Granted: true}, nil
	}
	lc := localTestLifecycle()
	lc.Approval = ApprovalConfig{PreGate: true, RequiredSteps: []string{approvalStepPublish}}
	out, err := runLocalCycle(t, f, lc)
	if err != nil {
		t.Fatal(err)
	}
	if out.TerminalState != stateClosed {
		t.Fatalf("terminal = %q, want CLOSED", out.TerminalState)
	}
	if f.repairs != 1 || f.publishes != 1 || calls != 2 {
		t.Fatalf("repairs/publishes/gate calls = %d/%d/%d, want 1/1/2", f.repairs, f.publishes, calls)
	}
}

// #212: a non-green CI gate escalates and never merges/closes (§49).
func TestLocalCIUnhealthyEscalates(t *testing.T) {
	withLocalAppCfg(t)
	f := newLocalFakes()
	f.acts.waitCI = func(context.Context, CIRequest) (CIStatus, error) { return CIStatus{Green: false, Failed: true}, nil }
	out, err := runLocalCycle(t, f, localTestLifecycle())
	if err != nil {
		t.Fatal(err)
	}
	if out.TerminalState != stateHumanRequired || out.EscalationReason != reasonCIFailed {
		t.Fatalf("terminal/reason = %q/%q", out.TerminalState, out.EscalationReason)
	}
	if f.merges != 0 || f.closes != 0 {
		t.Fatalf("red CI must not merge/close: %d/%d", f.merges, f.closes)
	}
}

// #212: a merge failure escalates; the issue never closes without a merge.
func TestLocalMergeFailureEscalates(t *testing.T) {
	withLocalAppCfg(t)
	f := newLocalFakes()
	f.acts.merge = func(context.Context, MergeRequest) (MergeResult, error) {
		return MergeResult{}, context.DeadlineExceeded
	}
	out, err := runLocalCycle(t, f, localTestLifecycle())
	if err != nil {
		t.Fatal(err)
	}
	if out.TerminalState != stateHumanRequired || out.EscalationReason != reasonMergeFailed {
		t.Fatalf("terminal/reason = %q/%q", out.TerminalState, out.EscalationReason)
	}
	if f.closes != 0 {
		t.Fatalf("merge failure must not close, got %d", f.closes)
	}
}

// #212: a human-released claim cancels the run before any mutation (#141).
func TestLocalClaimReleasedCancels(t *testing.T) {
	withLocalAppCfg(t)
	f := newLocalFakes()
	f.acts.fetchSnapshot = func(context.Context, IssueInput) (IssueSnapshot, error) {
		return IssueSnapshot{State: "closed"}, nil
	}
	out, err := runLocalCycle(t, f, localTestLifecycle())
	if err != nil {
		t.Fatal(err)
	}
	if out.TerminalState != stateCancelled || out.CancelReason != reasonClaimReleased {
		t.Fatalf("terminal/cancel = %q/%q", out.TerminalState, out.CancelReason)
	}
	if f.publishes != 0 {
		t.Fatalf("released claim must not publish, got %d", f.publishes)
	}
}

// #212: an agent-reported block routes to the human path, never the repair loop.
func TestLocalBlockedDeveloperHumanRequired(t *testing.T) {
	withLocalAppCfg(t)
	f := newLocalFakes()
	f.acts.developer = func(context.Context, DevelopRequest) (DevelopResult, error) {
		return DevelopResult{Blocked: true, BlockedReason: "needs input"}, nil
	}
	out, err := runLocalCycle(t, f, localTestLifecycle())
	if err != nil {
		t.Fatal(err)
	}
	if out.TerminalState != stateHumanRequired || out.EscalationReason != reasonAgentBlocked || out.AgentOutcome != agentOutcomeBlocked {
		t.Fatalf("terminal/reason/agent = %q/%q/%q", out.TerminalState, out.EscalationReason, out.AgentOutcome)
	}
	if f.repairs != 0 || f.publishes != 0 {
		t.Fatalf("blocked agent must not repair/publish: %d/%d", f.repairs, f.publishes)
	}
}

// #212: the opt-in AI PR description feeds the published body (evidence only).
func TestLocalPRDescriptionUsed(t *testing.T) {
	withLocalAppCfg(t)
	f := newLocalFakes()
	f.acts.prDescription = func(context.Context, PRDescriptionRequest) (PRDescriptionResult, error) {
		return PRDescriptionResult{Body: "ai body", Generated: true}, nil
	}
	var published PublishInput
	f.acts.publish = func(_ context.Context, in PublishInput) (PublishResult, error) {
		f.publishes++
		published = in
		return PublishResult{PRNumber: 101}, nil
	}
	lc := localTestLifecycle()
	lc.Publish.AIDescription = true
	if _, err := runLocalCycle(t, f, lc); err != nil {
		t.Fatal(err)
	}
	if published.Body != "ai body" {
		t.Fatalf("published body = %q, want the generated description", published.Body)
	}
}

// #212: a hung activity is bounded by the worker window, like Temporal's
// activity timeout, instead of blocking the sequential loop forever.
func TestLocalCallTimeout(t *testing.T) {
	withLocalAppCfg(t)
	appCfg.Worker.DevTimeout = "20ms"
	rt := newLocalRuntime(appCfg, newLocalFakes().acts)
	start := time.Now()
	err := rt.call(context.Background(), retryAgent, func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	})
	if err == nil {
		t.Fatal("a hung agent call must time out")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("timeout took %v, want the 20ms window", time.Since(start))
	}
}

// #212: an activity error mid-path still tears down the worktree.
func TestLocalDeveloperErrorCleansUp(t *testing.T) {
	withLocalAppCfg(t)
	f := newLocalFakes()
	f.acts.developer = func(context.Context, DevelopRequest) (DevelopResult, error) {
		return DevelopResult{}, context.DeadlineExceeded
	}
	rt := newLocalRuntime(appCfg, f.acts)
	in := IssueInput{Owner: "o", Repo: "r", IssueNum: 7, RepoPath: t.TempDir(), Lifecycle: localTestLifecycle()}
	if err := rt.runCycle(context.Background(), in); err == nil {
		t.Fatal("developer error must propagate")
	}
	if f.cleanups != 1 {
		t.Fatalf("cleanups = %d, want 1 after a failed cycle", f.cleanups)
	}
}
