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
	t.Cleanup(func() { appCfg = saved })
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
	savedWait := localRetryWait
	localRetryWait = func(context.Context, time.Duration) error { return nil }
	t.Cleanup(func() { localRetryWait = savedWait })
	f := newLocalFakes()
	rt := newLocalRuntime(appCfg, f.acts)

	attempts := 0
	err := rt.call(context.Background(), retryInfrastructure, func() error {
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
	if err := rt.call(context.Background(), retryAgent, func() error {
		attempts++
		return context.DeadlineExceeded
	}); err == nil || attempts != 1 {
		t.Fatalf("agent attempts = %d err = %v, want 1/error", attempts, err)
	}

	attempts = 0
	if err := rt.call(context.Background(), retryDeterministic, func() error {
		attempts++
		return context.DeadlineExceeded
	}); err == nil || attempts != 1 {
		t.Fatalf("deterministic attempts = %d err = %v, want 1/error", attempts, err)
	}

	attempts = 0
	if err := rt.call(context.Background(), retryVerification, func() error {
		attempts++
		return context.DeadlineExceeded
	}); err == nil || attempts != 3 {
		t.Fatalf("verification attempts = %d err = %v, want 3/error", attempts, err)
	}
}
