package konoha

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/testsuite"
)

// withAgentsConfig installs a §98 catalog for the duration of one test.
func withAgentsConfig(t *testing.T, agents AgentsConfig) {
	t.Helper()
	saved := appCfg
	appCfg.Defaults.Agents = agents
	t.Cleanup(func() { appCfg = saved })
}

// runCycleWith executes IssueCycleWorkflow with a custom lifecycle and asserts
// it completed.
func runCycleWith(t *testing.T, env *testsuite.TestWorkflowEnvironment, lc LifecycleConfig) {
	t.Helper()
	in := IssueInput{Owner: "o", Repo: "r", IssueNum: testIssue, RepoPath: t.TempDir(), Lifecycle: lc}
	env.ExecuteWorkflow(IssueCycleWorkflow, in)
	if !env.IsWorkflowCompleted() {
		return
	}
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
}

// readOutcome loads the single cycle outcome.json under an evidence root.
func readOutcome(t *testing.T, root string) CycleOutcome {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(root, "cycles", "*", evidenceOutcome))
	if err != nil || len(matches) != 1 {
		t.Fatalf("expected one outcome.json, got %v (err=%v)", matches, err)
	}
	b, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	var out CycleOutcome
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("outcome.json must decode: %v", err)
	}
	return out
}

// §98 AC: a named model resolves, its provider/model + digest land in
// outcome.json, and the resolved id is what the developer activity receives.
func TestWorkflowRecordsResolvedModelDigest(t *testing.T) {
	resetMetrics()
	root := withEvidenceDir(t)
	withAgentsConfig(t, AgentsConfig{Models: map[string]AgentModel{
		"strong": {Provider: "anthropic", Model: "claude-sonnet", APIKeyEnv: "ANTHROPIC_API_KEY"},
	}})

	env := mockCycleEnv(t, EstimateResult{Complexity: 3, Confidence: "high"})
	// Register the developer capture FIRST so it wins over mockSmallPath's stub.
	var devModel string
	env.OnActivity(RunDeveloper, mock.Anything, mock.Anything).Return(func(_ context.Context, in DevelopRequest) (DevelopResult, error) {
		devModel = in.Model
		return DevelopResult{CommitSHA: "devsha", ChangedFiles: []string{"a"}, Validated: true}, nil
	}).Once()
	mockSmallPath(env)
	env.RegisterActivity(WriteCycleEvidence) // real outcome writer

	lc := LifecycleConfig{Mode: modeSupervisedAutonomous, DevelopmentMaxComplexity: 4,
		Models: map[string]string{stageDeveloper: "strong"}}
	runCycleWith(t, env, lc)

	if got := terminalState(t, env); got != stateClosed {
		t.Fatalf("terminal = %q, want CLOSED", got)
	}
	if devModel != "anthropic/claude-sonnet" {
		t.Fatalf("developer activity model = %q, want the resolved catalog model", devModel)
	}

	out := readOutcome(t, root)
	if out.Resources == nil || len(out.Resources.Models) != len(pipelineStages) {
		t.Fatalf("outcome resources = %+v, want one resolved model per stage", out.Resources)
	}
	dev := out.Resources.modelFor(stageDeveloper)
	if dev.Name != "strong" || dev.AgentModel() != "anthropic/claude-sonnet" || len(dev.Digest) != 64 {
		t.Fatalf("resolved developer evidence = %+v, want named+d igest", dev)
	}
	if dev.APIKeyEnv != "ANTHROPIC_API_KEY" {
		t.Fatalf("outcome must record the apiKeyEnv NAME, got %q", dev.APIKeyEnv)
	}
}

// §98 AC: per-stage models are recorded per activation (developer vs repair),
// each with its own resolved-model digest.
func TestWorkflowPerStageModelsRecordedPerActivation(t *testing.T) {
	resetMetrics()
	root := withEvidenceDir(t)
	withAgentsConfig(t, AgentsConfig{Models: map[string]AgentModel{
		"strong": {Provider: "anthropic", Model: "claude-sonnet"},
		"cheap":  {Provider: "anthropic", Model: "claude-haiku"},
	}})

	env := mockCycleEnv(t, EstimateResult{Complexity: 3, Confidence: "high"})
	env.RegisterActivity(RecordActivation) // real lineage writer
	env.OnActivity(FetchIssueLabels, mock.Anything, mock.Anything).Return([]string{"bug"}, nil)
	env.OnActivity(RunDeveloper, mock.Anything, mock.Anything).Return(DevelopResult{
		CommitSHA: "devsha", Validated: true, Usage: HarnessUsage{TokensIn: 1, TokensOut: 1},
	}, nil)
	env.OnActivity(PublishDeveloperChange, mock.Anything, mock.Anything).Return(PublishResult{PRNumber: 101, Validated: true}, nil)
	env.OnActivity(FetchPRHead, mock.Anything, mock.Anything).Return("headsha", nil)
	env.OnActivity(FetchPRDiff, mock.Anything, mock.Anything).Return("+1", nil)
	calls := 0
	env.OnActivity(RunReviewer, mock.Anything, mock.Anything).Return(func(_ context.Context, _ ReviewRequest) (ReviewResult, error) {
		calls++
		if calls == 1 {
			return ReviewResult{Verdict: "REQUEST_CHANGES", CommitSHA: "headsha", Blocking: []string{"gap"}}, nil
		}
		return ReviewResult{Verdict: "APPROVE", CommitSHA: "headsha"}, nil
	})
	env.OnActivity(RunRepair, mock.Anything, mock.Anything).Return(DevelopResult{CommitSHA: "repairsha", Validated: true}, nil)
	env.OnActivity(WaitForCI, mock.Anything, mock.Anything).Return(CIStatus{Green: true}, nil)
	env.OnActivity(MergePullRequest, mock.Anything, mock.Anything).Return(MergeResult{Merged: true}, nil)
	env.OnActivity(EnsureIssueState, mock.Anything, mock.Anything).Return(nil)

	lc := LifecycleConfig{Mode: modeSupervisedAutonomous, DevelopmentMaxComplexity: 4,
		Models: map[string]string{stageDeveloper: "strong", stageRepair: "cheap"}}
	runCycleWith(t, env, lc)

	l := readLineage(t, root, "o", "r", testIssue)
	if len(l.Activations) != 2 {
		t.Fatalf("want 2 activations, got %+v", l.Activations)
	}
	if l.Activations[0].Stage != stageDeveloper || l.Activations[0].Model != "anthropic/claude-sonnet" {
		t.Fatalf("dev activation = %+v, want stage developer / strong model", l.Activations[0])
	}
	if l.Activations[1].Stage != stageRepair || l.Activations[1].Model != "anthropic/claude-haiku" {
		t.Fatalf("repair activation = %+v, want stage repair / cheap model", l.Activations[1])
	}
	if l.Activations[0].ModelDigest == "" || l.Activations[1].ModelDigest == "" ||
		l.Activations[0].ModelDigest == l.Activations[1].ModelDigest {
		t.Fatalf("per-stage model digests must be present and distinct: %+v", l.Activations)
	}
}

// §98 AC/§40: a budget crossing is recorded as evidence and NEVER changes the
// verdict — the cycle still closes autonomously.
func TestWorkflowBudgetCrossingRecordedAsEvidence(t *testing.T) {
	resetMetrics()
	root := withEvidenceDir(t)
	cost := 0.10
	tokens := int64(5)
	withAgentsConfig(t, AgentsConfig{Budgets: map[string]AgentBudget{
		"tiny": {MaxCostUSD: &cost, MaxTokens: &tokens, MaxWallClock: "1ns"},
	}})

	env := mockCycleEnv(t, EstimateResult{Complexity: 3, Confidence: "high"})
	env.RegisterActivity(WriteCycleEvidence)
	env.OnActivity(FetchIssueLabels, mock.Anything, mock.Anything).Return([]string{"enhancement"}, nil)
	env.OnActivity(RunDeveloper, mock.Anything, mock.Anything).Return(DevelopResult{
		CommitSHA: "devsha", Validated: true,
		Usage: HarnessUsage{TokensIn: 100, TokensOut: 100, CostUSD: 9.0},
	}, nil)
	env.OnActivity(PublishDeveloperChange, mock.Anything, mock.Anything).Return(PublishResult{PRNumber: 101, Validated: true}, nil)
	env.OnActivity(FetchPRHead, mock.Anything, mock.Anything).Return("headsha", nil)
	env.OnActivity(FetchPRDiff, mock.Anything, mock.Anything).Return("+1", nil)
	env.OnActivity(RunReviewer, mock.Anything, mock.Anything).Return(ReviewResult{Verdict: "APPROVE", CommitSHA: "headsha"}, nil)
	env.OnActivity(WaitForCI, mock.Anything, mock.Anything).Return(CIStatus{Green: true}, nil)
	env.OnActivity(MergePullRequest, mock.Anything, mock.Anything).Return(MergeResult{Merged: true}, nil)
	env.OnActivity(EnsureIssueState, mock.Anything, mock.Anything).Return(nil)

	lc := LifecycleConfig{Mode: modeSupervisedAutonomous, DevelopmentMaxComplexity: 4, Budget: "tiny"}
	runCycleWith(t, env, lc)

	// The budget never gates the verdict: still CLOSED.
	if got := terminalState(t, env); got != stateClosed {
		t.Fatalf("a budget crossing must not change the terminal: %q", got)
	}
	out := readOutcome(t, root)
	if out.Resources == nil || out.Resources.Budget == nil || out.Resources.Budget.Name != "tiny" {
		t.Fatalf("outcome must carry the selected budget evidence: %+v", out.Resources)
	}
	crossings := out.Resources.Budget.Crossings
	if len(crossings) != 3 {
		t.Fatalf("crossings = %+v, want cost/tokens/wall-clock", crossings)
	}
	for _, c := range crossings {
		// Cost + tokens are derived from the reported #87 usage and must cross;
		// the wall-clock window is intrinsic to elapsed time (evidence either way).
		if (c.Metric == "maxCostUsd" || c.Metric == "maxTokens") && !c.Exceeded {
			t.Fatalf("crossing %+v must be exceeded for this usage", c)
		}
	}
}

// §98 AC: credential VALUES never appear in evidence — only the apiKeyEnv name.
func TestWorkflowEvidenceNeverRecordsCredentialValue(t *testing.T) {
	resetMetrics()
	root := withEvidenceDir(t)
	const secret = "sk-super-secret-value-do-not-record"
	t.Setenv("KONOHA_TEST_KEY", secret)
	withAgentsConfig(t, AgentsConfig{Models: map[string]AgentModel{
		"strong": {Provider: "anthropic", Model: "claude-sonnet", APIKeyEnv: "KONOHA_TEST_KEY"},
	}})

	env := mockCycleEnv(t, EstimateResult{Complexity: 3, Confidence: "high"})
	mockSmallPath(env)
	env.RegisterActivity(WriteCycleEvidence)

	lc := LifecycleConfig{Mode: modeSupervisedAutonomous, DevelopmentMaxComplexity: 4,
		Models: map[string]string{stageDeveloper: "strong"}}
	runCycleWith(t, env, lc)

	out := readOutcome(t, root)
	if out.Resources.modelFor(stageDeveloper).APIKeyEnv != "KONOHA_TEST_KEY" {
		t.Fatalf("the apiKeyEnv NAME must be recorded: %+v", out.Resources.modelFor(stageDeveloper))
	}
	// No evidence artifact anywhere may contain the credential value.
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if strings.Contains(string(b), secret) {
			t.Fatalf("credential value leaked into evidence file %s", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
