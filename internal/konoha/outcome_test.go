package konoha

import (
	"encoding/json"
	"testing"
	"time"
)

var testOutcomeAt = time.Unix(0, 0).UTC()

// #84 AC1: the canonical case agent=SUCCESS, verification=FAILED, terminal
// HUMAN_REQUIRED is representable and NEVER collapsed into a single outcome.
func TestOutcomeCanonicalNotCollapsed(t *testing.T) {
	out := stampOutcome(CycleOutcome{
		WorkflowID: "r-issue-7", Issue: 7, Owner: "o", Repo: "r",
		TerminalState:       stateHumanRequired,
		HumanRequired:       true,
		AgentOutcome:        agentOutcomeSuccess,
		VerificationOutcome: verificationFailed,
		EscalationReason:    reasonAgentFailure,
	}, testOutcomeAt)

	b, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	var got CycleOutcome
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.AgentOutcome != agentOutcomeSuccess {
		t.Fatalf("agentOutcome = %q, want SUCCESS", got.AgentOutcome)
	}
	if got.VerificationOutcome != verificationFailed {
		t.Fatalf("verificationOutcome = %q, want FAILED (never collapsed)", got.VerificationOutcome)
	}
	if got.TerminalState != stateHumanRequired {
		t.Fatalf("terminalState = %q, want HUMAN_REQUIRED", got.TerminalState)
	}
	// Ordered conditions: agent → verification → terminal.
	if len(got.Conditions) != 3 {
		t.Fatalf("conditions = %d, want 3 ordered entries", len(got.Conditions))
	}
	for i, want := range []string{conditionAgent, conditionVerification, conditionTerminal} {
		if got.Conditions[i].Type != want {
			t.Fatalf("condition[%d].type = %q, want %q", i, got.Conditions[i].Type, want)
		}
	}
	if got.Conditions[0].Status != string(agentOutcomeSuccess) || got.Conditions[1].Status != string(verificationFailed) {
		t.Fatalf("condition statuses = %+v", got.Conditions)
	}
}

// #84 AC2: an agent failure is recorded with verificationOutcome=SKIPPED — never
// silently absent (§40). failedChildOutcome is the terminal the sweep records for
// a failed child/agent run.
func TestAgentFailureVerificationSkipped(t *testing.T) {
	out := stampOutcome(failedChildOutcome(IssueInput{Owner: "o", Repo: "r", IssueNum: 7}), testOutcomeAt)
	if out.AgentOutcome != agentOutcomeFailed {
		t.Fatalf("agentOutcome = %q, want FAILED", out.AgentOutcome)
	}
	if out.VerificationOutcome != verificationSkipped {
		t.Fatalf("verificationOutcome = %q, want SKIPPED (never absent)", out.VerificationOutcome)
	}
	b, _ := json.Marshal(out)
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	if raw["verificationOutcome"] != string(verificationSkipped) {
		t.Fatalf("outcome JSON missing verificationOutcome=SKIPPED: %s", b)
	}
}

// #84: a terminal outcome with no recorded agent/verification defaults both to
// SKIPPED and always carries the ordered condition list (never silently absent).
func TestStampOutcomeDefaultsAndConditions(t *testing.T) {
	out := stampOutcome(CycleOutcome{WorkflowID: "w", TerminalState: stateClosed}, testOutcomeAt)
	if out.AgentOutcome != agentOutcomeSkipped || out.VerificationOutcome != verificationSkipped {
		t.Fatalf("defaults = %q/%q, want SKIPPED/SKIPPED", out.AgentOutcome, out.VerificationOutcome)
	}
	if len(out.Conditions) != 3 || out.Conditions[2].Status != stateClosed {
		t.Fatalf("conditions = %+v, want terminal CLOSED", out.Conditions)
	}
	if !out.Conditions[0].At.Equal(testOutcomeAt) {
		t.Fatalf("condition timestamp = %v, want deterministic %v", out.Conditions[0].At, testOutcomeAt)
	}
}

// #84: a blocked agent is BLOCKED (not FAILED) and its verification is SKIPPED;
// the agent's own message lands on the agent condition.
func TestBlockedAgentProjection(t *testing.T) {
	res := DevelopResult{Blocked: true, BlockedReason: "spec contradicts repo"}
	if got := agentOutcomeOf(res); got != agentOutcomeBlocked {
		t.Fatalf("agentOutcomeOf(blocked) = %q, want BLOCKED", got)
	}
	if got := verificationOutcomeOf(res); got != verificationSkipped {
		t.Fatalf("verificationOutcomeOf(blocked) = %q, want SKIPPED", got)
	}
	out := stampOutcome(CycleOutcome{
		TerminalState: stateHumanRequired, AgentOutcome: agentOutcomeBlocked,
		AgentMessage: res.BlockedReason, EscalationReason: reasonAgentBlocked,
	}, testOutcomeAt)
	if out.Conditions[0].Message != res.BlockedReason {
		t.Fatalf("agent condition message = %q, want %q", out.Conditions[0].Message, res.BlockedReason)
	}
}

// #84: the develop/repair result maps to the verification outcome matrix.
func TestVerificationOutcomeMatrix(t *testing.T) {
	cases := []struct {
		name string
		res  DevelopResult
		want VerificationOutcome
	}{
		{"blocked", DevelopResult{Blocked: true}, verificationSkipped},
		{"skipped", DevelopResult{ValidationSkipped: true}, verificationSkipped},
		{"passed", DevelopResult{Validated: true}, verificationSuccess},
		{"failed", DevelopResult{Validated: false}, verificationFailed},
	}
	for _, c := range cases {
		if got := verificationOutcomeOf(c.res); got != c.want {
			t.Fatalf("%s: verificationOutcomeOf = %q, want %q", c.name, got, c.want)
		}
	}
}

// #84/§39: the explicit outcome counts are mirrored into the sink, while the
// existing derived gauges are untouched (AC5: no regression).
func TestOutcomeMetricsMirror(t *testing.T) {
	resetMetrics()
	defer resetMetrics()

	RecordCycleMetrics(bg(), stampOutcome(CycleOutcome{
		WorkflowID: "w1", Issue: 1, TerminalState: stateHumanRequired, HumanRequired: true,
		AgentOutcome: agentOutcomeSuccess, VerificationOutcome: verificationFailed,
		Route: stateSmall, Complexity: 3,
	}, testOutcomeAt))
	RecordCycleMetrics(bg(), stampOutcome(CycleOutcome{
		WorkflowID: "w2", Issue: 2, TerminalState: stateClosed, AutonomousClose: true,
		AgentOutcome: agentOutcomeBlocked, VerificationOutcome: verificationSkipped,
		Route: stateSmall, Complexity: 3,
	}, testOutcomeAt))

	if got := sink.get(mAgentOutcomePrefix + "success"); got != 1 {
		t.Fatalf("agent_outcome_success = %v, want 1", got)
	}
	if got := sink.get(mAgentOutcomePrefix + "blocked"); got != 1 {
		t.Fatalf("agent_outcome_blocked = %v, want 1", got)
	}
	if got := sink.get(mVerificationOutcomePrefix + "failed"); got != 1 {
		t.Fatalf("verification_outcome_failed = %v, want 1", got)
	}
	if got := sink.get(mVerificationOutcomePrefix + "skipped"); got != 1 {
		t.Fatalf("verification_outcome_skipped = %v, want 1", got)
	}
	// The canonical outcome triple must not disturb the existing gauges.
	if got := sink.get(mAutonomousRate); got != 1 {
		t.Fatalf("autonomous_completion_rate = %v, want 1 (unchanged)", got)
	}
	if got := sink.get(mDidHumanEscalationRate); got != 0.5 {
		t.Fatalf("human_escalation_rate = %v, want 0.5 (unchanged)", got)
	}
}

// #212: the terminal assembly is runtime-neutral and shared by the Temporal
// workflow and the local runtime, so outcome.json keeps one shape.
func TestAssembleTerminalDefaults(t *testing.T) {
	saved := appCfg
	appCfg = defaultAppConfig()
	t.Cleanup(func() { appCfg = saved })

	in := IssueInput{Owner: "o", Repo: "r", IssueNum: 7, Lifecycle: LifecycleConfig{DryRun: true}}
	cleanup := &CleanupResult{Attempted: true, Outcome: cleanupAbsent, Verified: true}
	out := assembleTerminal(in, CycleOutcome{WorkflowID: "r-issue-7", TerminalState: stateFailed}, terminalInputs{
		AgentOutcome: agentOutcomeFailed,
		Usage:        HarnessUsage{TokensIn: 3},
		Elapsed:      2 * time.Second,
		Cleanup:      cleanup,
	})
	if out.AgentOutcome != agentOutcomeFailed {
		t.Fatalf("agent outcome = %q, want FAILED (not collapsed)", out.AgentOutcome)
	}
	if out.Usage.TokensIn != 3 {
		t.Fatalf("usage = %+v, want 3 tokens in", out.Usage)
	}
	if out.DurationSeconds != 2 {
		t.Fatalf("duration = %v, want 2", out.DurationSeconds)
	}
	if out.Cleanup == nil || out.Cleanup.Outcome != cleanupAbsent {
		t.Fatalf("cleanup = %+v, want the supplied record", out.Cleanup)
	}

	out = prepareTerminalOutcome(in, out, testOutcomeAt)
	if out.VerificationOutcome != verificationSkipped {
		t.Fatalf("verification outcome = %q, want SKIPPED default", out.VerificationOutcome)
	}
	if !out.DryRun {
		t.Fatal("dryRun must be stamped from the input lifecycle")
	}
	if out.PolicyDigest == "" {
		t.Fatal("policy digest must be stamped")
	}
	if len(out.Conditions) != 3 {
		t.Fatalf("conditions = %+v, want agent/verification/terminal", out.Conditions)
	}
	if out.Conditions[0].Type != conditionAgent || out.Conditions[1].Type != conditionVerification || out.Conditions[2].Type != conditionTerminal {
		t.Fatalf("conditions out of order: %+v", out.Conditions)
	}
}
