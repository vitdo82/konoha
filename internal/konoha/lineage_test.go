package konoha

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/mock"
)

// readLineage loads lineage.json for one (owner, repo, issue) from a root.
func readLineage(t *testing.T, root, owner, repo string, issue int) Lineage {
	t.Helper()
	p := filepath.Join(root, "lineage", lineageFileName(owner, repo, issue))
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	var l Lineage
	if err := json.Unmarshal(b, &l); err != nil {
		t.Fatalf("lineage must decode: %v", err)
	}
	return l
}

func lineageFileName(owner, repo string, issue int) string {
	return fmt.Sprintf("%s-%s-issue-%d.json", owner, repo, issue)
}

// #88/§24: re-recording the same activation (same WorkflowID) REPLACES, never
// appends; a distinct WorkflowID appends in order. Aggregates sum recorded usage
// only — a missing entry contributes zero.
func TestRecordActivationIdempotentByWorkflowID(t *testing.T) {
	root := withEvidenceDir(t)
	ctx := context.Background()
	initial := LineageActivation{
		Attempt: 1, WorkflowID: "w", Reason: activationReasonInitial,
		Harness: "opencode", Model: "m", BaseSHA: "base", CommitSHA: "c1", Result: "SUCCESS",
		TokensIn: 10, TokensOut: 5, CostUSD: 0.25,
	}
	for i := 0; i < 3; i++ {
		if err := RecordActivation(ctx, ActivationRequest{Owner: "o", Repo: "r", Issue: 7, Activation: initial}); err != nil {
			t.Fatalf("RecordActivation: %v", err)
		}
	}
	l := readLineage(t, root, "o", "r", 7)
	if len(l.Activations) != 1 {
		t.Fatalf("duplicate calls must not append: got %d activations", len(l.Activations))
	}

	// A repair (distinct WorkflowID, no reported usage) appends and contributes 0.
	repair := LineageActivation{
		Attempt: 2, WorkflowID: "w-repair-2", Reason: activationReasonRepair,
		Harness: "opencode", Model: "m", BaseSHA: "base", CommitSHA: "c2", Result: "SUCCESS",
	}
	if err := RecordActivation(ctx, ActivationRequest{Owner: "o", Repo: "r", Issue: 7, Activation: repair}); err != nil {
		t.Fatalf("RecordActivation: %v", err)
	}
	l = readLineage(t, root, "o", "r", 7)
	if len(l.Activations) != 2 {
		t.Fatalf("want 2 activations, got %d", len(l.Activations))
	}
	if l.Activations[0].Reason != activationReasonInitial || l.Activations[1].Reason != activationReasonRepair {
		t.Fatalf("activations must preserve order: %+v", l.Activations)
	}
	if l.Activations[0].WorkflowID == l.Activations[1].WorkflowID {
		t.Fatalf("develop and repair must have distinct workflowIds: %+v", l.Activations)
	}
	if l.Attempts != 2 {
		t.Fatalf("attempts = %d, want 2", l.Attempts)
	}
	if l.TotalTokens != 15 || l.TotalCostUsd != 0.25 {
		t.Fatalf("aggregates = %d tokens / %v usd, want 15 / 0.25 (missing usage contributes zero)", l.TotalTokens, l.TotalCostUsd)
	}
	if l.Repo != "o/r" || l.Issue != 7 {
		t.Fatalf("identity = %s#%d, want o/r#7", l.Repo, l.Issue)
	}
}

// #88: the lineage id is a single path segment; a hostile owner/repo cannot
// escape the evidence root.
func TestRecordActivationRejectsTraversal(t *testing.T) {
	withEvidenceDir(t)
	err := RecordActivation(context.Background(), ActivationRequest{
		Owner: "../evil", Repo: "r", Issue: 7,
		Activation: LineageActivation{WorkflowID: "w"},
	})
	if err == nil {
		t.Fatal("a non-plain-segment lineage id must be rejected")
	}
}

// #88/§42: DryRun namespaces lineage under dryrun/ and writes nothing outside it.
func TestRecordActivationDryRunNamespaces(t *testing.T) {
	root := withEvidenceDir(t)
	if err := RecordActivation(context.Background(), ActivationRequest{
		Owner: "o", Repo: "r", Issue: 7, DryRun: true,
		Activation: LineageActivation{WorkflowID: "w", Reason: activationReasonInitial},
	}); err != nil {
		t.Fatalf("RecordActivation: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "dryrun", "lineage", lineageFileName("o", "r", 7))); err != nil {
		t.Fatalf("dry-run lineage missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "lineage", lineageFileName("o", "r", 7))); err == nil {
		t.Fatal("dry-run must not write the non-dryrun lineage path")
	}
}

// #88: the digest is stable across calls for identical input and changes when
// any policy/model/task field changes.
func TestPolicyDigestStableAndSensitive(t *testing.T) {
	base := LifecycleConfig{
		Mode: modeSupervisedAutonomous, Model: "provider/model", Harness: "opencode",
		DevelopmentMaxComplexity: 4,
		Autonomy:                 Autonomy{ClosePolicy: closePolicyRiskBased},
	}
	task := taskHash("o", "r", 7)
	d := policyDigest(base, task)
	if d == "" || d != policyDigest(base, task) {
		t.Fatalf("digest must be stable, got %q", d)
	}

	policy := base
	policy.DevelopmentMaxComplexity = 2
	if policyDigest(policy, task) == d {
		t.Fatal("a policy change must change the digest")
	}
	model := base
	model.Model = "provider/other"
	if policyDigest(model, task) == d {
		t.Fatal("a model change must change the digest")
	}
	harness := base
	harness.Harness = "generic"
	if policyDigest(harness, task) == d {
		t.Fatal("a harness change must change the digest")
	}
	if policyDigest(base, taskHash("o", "r", 8)) == d {
		t.Fatal("a task change must change the digest")
	}
}

// #88: canonical encoding — identical map content in a different insertion order
// yields the same digest.
func TestPolicyDigestCanonicalMapOrder(t *testing.T) {
	a := LifecycleConfig{Alerting: AlertingConfig{Thresholds: map[string]float64{"a": 1, "b": 2, "c": 3}}}
	b := LifecycleConfig{Alerting: AlertingConfig{Thresholds: map[string]float64{"c": 3, "a": 1, "b": 2}}}
	task := taskHash("o", "r", 7)
	if policyDigest(a, task) != policyDigest(b, task) {
		t.Fatal("digest must be canonical over map key order")
	}
}

// #88/AC: a develop + repair produces two ordered activations, and the terminal
// outcome.json carries the policy digest.
func TestWorkflowLineageDevelopThenRepair(t *testing.T) {
	resetMetrics()
	root := withEvidenceDir(t)
	env := mockCycleEnv(t, EstimateResult{Complexity: 3, Confidence: "high"})
	env.RegisterActivity(RecordActivation)   // real lineage writer
	env.RegisterActivity(WriteCycleEvidence) // real outcome writer

	env.OnActivity(FetchIssueLabels, mock.Anything, mock.Anything).Return([]string{"bug"}, nil)
	env.OnActivity(RunDeveloper, mock.Anything, mock.Anything).Return(DevelopResult{
		WorktreeDir: "/tmp/wt", BaseSHA: "base", CommitSHA: "devsha", Summary: "c", Validated: true,
		Usage: HarnessUsage{TokensIn: 10, TokensOut: 5, CostUSD: 0.25},
	}, nil)
	env.OnActivity(PublishDeveloperChange, mock.Anything, mock.Anything).Return(PublishResult{
		Branch: "konoha/issue-7", PRNumber: 101, Validated: true,
	}, nil)
	env.OnActivity(FetchPRHead, mock.Anything, mock.Anything).Return("headsha", nil)
	env.OnActivity(FetchPRDiff, mock.Anything, mock.Anything).Return("+1 line", nil)
	calls := 0
	env.OnActivity(RunReviewer, mock.Anything, mock.Anything).Return(func(_ context.Context, _ ReviewRequest) (ReviewResult, error) {
		calls++
		if calls == 1 {
			return ReviewResult{Verdict: "REQUEST_CHANGES", CommitSHA: "headsha", PRNumber: 101, Blocking: []string{"gap"}}, nil
		}
		return ReviewResult{Verdict: "APPROVE", CommitSHA: "headsha", PRNumber: 101}, nil
	})
	env.OnActivity(RunRepair, mock.Anything, mock.Anything).Return(DevelopResult{
		WorktreeDir: "/tmp/wt2", BaseSHA: "base", CommitSHA: "repairsha", Summary: "fixed", Validated: true,
		Usage: HarnessUsage{TokensIn: 1, TokensOut: 1, CostUSD: 0.5},
	}, nil)
	env.OnActivity(WaitForCI, mock.Anything, mock.Anything).Return(CIStatus{Green: true}, nil)
	env.OnActivity(MergePullRequest, mock.Anything, mock.Anything).Return(MergeResult{Merged: true}, nil)
	env.OnActivity(EnsureIssueState, mock.Anything, mock.Anything).Return(nil)

	runAndAssert(t, env)

	l := readLineage(t, root, "o", "r", testIssue)
	if len(l.Activations) != 2 {
		t.Fatalf("develop + repair must produce 2 activations, got %d", len(l.Activations))
	}
	if l.Activations[0].Reason != activationReasonInitial || l.Activations[1].Reason != activationReasonRepair {
		t.Fatalf("activations out of order: %+v", l.Activations)
	}
	if l.Activations[0].WorkflowID == l.Activations[1].WorkflowID {
		t.Fatalf("activation workflowIds must be distinct: %+v", l.Activations)
	}
	if l.Attempts != 2 || l.TotalTokens != 17 || l.TotalCostUsd != 0.75 {
		t.Fatalf("lineage aggregates = attempts %d, tokens %d, usd %v; want 2/17/0.75", l.Attempts, l.TotalTokens, l.TotalCostUsd)
	}

	// The outcome.json carries the #88 digest.
	matches, _ := filepath.Glob(filepath.Join(root, "cycles", "*", evidenceOutcome))
	if len(matches) != 1 {
		t.Fatalf("expected one outcome.json, got %v", matches)
	}
	b, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	var out CycleOutcome
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("outcome.json must decode: %v", err)
	}
	if len(out.PolicyDigest) != 64 {
		t.Fatalf("policyDigest = %q, want a 64-char SHA-256 hex", out.PolicyDigest)
	}
}
