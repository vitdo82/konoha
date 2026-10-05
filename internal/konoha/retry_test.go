package konoha

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"
)

func agentTimeoutError() error {
	return temporal.NewHeartbeatTimeoutError()
}

func isAgentTimeoutError(err error) bool { return temporal.IsTimeoutError(err) }

// §95/AC1: the deterministic class is MaximumAttempts=1 — and the testsuite
// actually re-executes a failed activity only when the policy allows it, so a
// call-count of 1 proves the call-site configuration, not just the constant.
var detProbeCalls int

func detProbeActivity(context.Context) error {
	detProbeCalls++
	return errors.New("transient")
}

func detProbeWorkflow(ctx workflow.Context) error {
	ctx = workflow.WithActivityOptions(ctx, activityOptionsFor(retryDeterministic, time.Minute))
	return workflow.ExecuteActivity(ctx, detProbeActivity).Get(ctx, nil)
}

func TestDeterministicActivityMaximumAttemptsOne(t *testing.T) {
	if got := retryPolicyFor(retryDeterministic).MaximumAttempts; got != 1 {
		t.Fatalf("deterministic MaximumAttempts = %d, want 1", got)
	}
	detProbeCalls = 0
	env := (&testsuite.WorkflowTestSuite{}).NewTestWorkflowEnvironment()
	env.RegisterWorkflow(detProbeWorkflow)
	env.RegisterActivity(detProbeActivity)
	env.ExecuteWorkflow(detProbeWorkflow)
	if err := env.GetWorkflowError(); err == nil {
		t.Fatal("a deterministic activity error must surface, not be swallowed")
	}
	if detProbeCalls != 1 {
		t.Fatalf("deterministic activity ran %d times, want exactly 1 (MaximumAttempts=1)", detProbeCalls)
	}
}

// §95: the four classes carry the spec'd attempt counts (infra with backoff).
func TestRetryPolicyClasses(t *testing.T) {
	if got := retryPolicyFor(retryInfrastructure); got.MaximumAttempts != 5 ||
		got.InitialInterval <= 0 || got.BackoffCoefficient <= 1 || got.MaximumInterval <= 0 {
		t.Fatalf("infrastructure policy = %+v, want 5 attempts with exponential backoff", got)
	}
	if got := retryPolicyFor(retryAgent).MaximumAttempts; got != 1 {
		t.Fatalf("agent MaximumAttempts = %d, want 1 (a transport-loss retry must not replay an agent)", got)
	}
	if got := retryPolicyFor(retryVerification).MaximumAttempts; got != 3 {
		t.Fatalf("verification MaximumAttempts = %d, want 3", got)
	}
}

// probeActivity reports the Temporal attempt number it is running as, failing
// the first attempt so the testsuite drives a real retry.
func probeActivity(ctx context.Context) (int, error) {
	a := activityAttempt(ctx)
	if a < 2 {
		return a, errors.New("transient")
	}
	return a, nil
}

func probeWorkflow(ctx workflow.Context, max int32) (int, error) {
	ao := activityOptionsFor(retryAgent, time.Minute)
	ao.RetryPolicy.MaximumAttempts = max // test-only: allow one retry
	ctx = workflow.WithActivityOptions(ctx, ao)
	var got int
	err := workflow.ExecuteActivity(ctx, probeActivity).Get(ctx, &got)
	return got, err
}

// §95/AC2: after a retried invocation the recorded attempt number is the second
// Temporal attempt, proving the live attempt is read (not a constant 1).
func TestAgentAttemptProvenanceOnRetry(t *testing.T) {
	env := (&testsuite.WorkflowTestSuite{}).NewTestWorkflowEnvironment()
	env.RegisterWorkflow(probeWorkflow)
	env.RegisterActivity(probeActivity)
	env.ExecuteWorkflow(probeWorkflow, int32(2))
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	var got int
	if err := env.GetWorkflowResult(&got); err != nil {
		t.Fatalf("workflow result: %v", err)
	}
	if got != 2 {
		t.Fatalf("recorded attempt = %d, want 2", got)
	}
}

// §95: outside a Temporal activity context the attempt defaults to 1, so unit
// tests calling the agent activities directly never panic.
func TestAgentAttemptOutsideTemporal(t *testing.T) {
	if got := activityAttempt(context.Background()); got != 1 {
		t.Fatalf("non-Temporal attempt = %d, want 1", got)
	}
}

// §95/AC2: the attempt number lands in the agent result's evidence JSON.
func TestAgentAttemptInResultEvidence(t *testing.T) {
	js := evidenceResultJSON(DevelopResult{CommitSHA: "abc", Attempt: 2})
	if !strings.Contains(js, `"attempt":2`) {
		t.Fatalf("result evidence = %s, want the attempt number recorded", js)
	}
}

// §95/AC3: agent activities carry a HeartbeatTimeout; the other classes do not.
func TestAgentActivityHeartbeatConfigured(t *testing.T) {
	ao := activityOptionsFor(retryAgent, 45*time.Minute)
	if ao.HeartbeatTimeout <= 0 {
		t.Fatal("agent activities must set a HeartbeatTimeout")
	}
	if ao.HeartbeatTimeout > ao.StartToCloseTimeout {
		t.Fatalf("HeartbeatTimeout %v must not exceed StartToCloseTimeout %v", ao.HeartbeatTimeout, ao.StartToCloseTimeout)
	}
	if got := activityOptionsFor(retryInfrastructure, time.Minute).HeartbeatTimeout; got != 0 {
		t.Fatalf("infrastructure HeartbeatTimeout = %v, want none", got)
	}
}

// §95/AC3: a stalled agent surfaces as a timeout error — the workflow must fail
// (never report a silent success) and never reach CLOSED.
func TestAgentHeartbeatTimeoutSurfaces(t *testing.T) {
	resetMetrics()
	env := mockCycleEnv(t, EstimateResult{Complexity: 3, Confidence: "high"})
	env.OnActivity(RunDeveloper, mock.Anything, mock.Anything).Return(
		DevelopResult{}, agentTimeoutError())
	env.ExecuteWorkflow(IssueCycleWorkflow, IssueInput{
		Owner: "o", Repo: "r", IssueNum: testIssue, RepoPath: t.TempDir(),
		Lifecycle: LifecycleConfig{Mode: modeSupervisedAutonomous, DevelopmentMaxComplexity: 4},
	})
	err := env.GetWorkflowError()
	if err == nil {
		t.Fatal("a stalled agent must surface as an error, not a silent success")
	}
	if !isAgentTimeoutError(err) {
		t.Fatalf("workflow error = %v, want a timeout error", err)
	}
}
