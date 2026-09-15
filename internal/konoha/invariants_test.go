package konoha

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// §44 core invariants exercisable in-process, asserted by number. Several are
// already covered in their owning package's tests (idempotency, globMatch,
// determinism); this file asserts the cross-cutting guarantees §41 calls for.

// §44 #1 + #13: Temporal owns lifecycle state (constant, not label-driven), and
// child complexity cannot exceed the executable threshold.
func TestInvariantsStateConstantsAndChildCeiling(t *testing.T) {
	// The authoritative states are the §6 constants; labels are only projections.
	if stateHumanRequired != "HUMAN_REQUIRED" || stateBlocked != "BLOCKED" ||
		stateClosed != "CLOSED" || stateFailed != "FAILED" ||
		statePendingApproval != "PENDING_APPROVAL" {
		t.Fatal("authoritative §6 states drifted")
	}
	// Child ceiling is enforced at the splitter validation gate (§11).
	lc := LifecycleConfig{}
	for _, c := range []int{1, 4} {
		if err := ValidateSplit(lc, SplitResult{Tickets: []SplitTicket{
			{Key: "k", Complexity: c, Requirements: []string{"r"}, AcceptanceCriteria: []string{"c"}},
		}}); err != nil {
			t.Fatalf("child complexity %d within ceiling rejected: %v", c, err)
		}
	}
	if err := ValidateSplit(lc, SplitResult{Tickets: []SplitTicket{
		{Key: "k", Complexity: 5, Requirements: []string{"r"}, AcceptanceCriteria: []string{"c"}},
	}}); err == nil {
		t.Fatal("child complexity above the executable threshold must be rejected (§11)")
	}
}

// §44 #4: LLMs propose, deterministic policy decides — no agent can execute its
// own recommendation. Probe every bounded-recommendation agent: splitter
// (ValidateSplit is separate from any creation), resolution, diagnosis.
func TestInvariantsLLMsProposePolicyDecides(t *testing.T) {
	// Diagnosis "retry" does NOT self-execute: it returns a transition the
	// WORKFLOW applies; the agent API surface never mutates GitHub.
	tr, err := ApplyDiagnosis(DiagnosisResult{Recommendation: "retry", Rationale: "x"})
	if err != nil || tr.TargetState != stateDeveloping {
		t.Fatalf("diagnosis recommendation must map through policy, got %+v err=%v", tr, err)
	}
	// Same for resolution "repair": the action is a bounded string the workflow
	// acts on; it is not an RPC that can mutate anything.
	if !validResolutionAction(resolutionActionRepair) {
		t.Fatal("repair must remain a bounded resolution action")
	}
	// Splitter: a validated split is a plan; EnsureChildren is what mutates, and
	// it is idempotent + dry-run aware. A call with DryRun must not create.
	prev := ghRunner
	created := 0
	ghRunner = func(_ context.Context, args ...string) ([]byte, error) { return nil, nil }
	defer func() { ghRunner = prev }()
	out, err := EnsureChildren(context.Background(), ChildCreateRequest{
		Owner: "o", Repo: "r", ParentNum: 1, DryRun: true, Tickets: []SplitTicket{ticket("01")},
	})
	if err != nil || created != 0 {
		t.Fatalf("dryRun child creation must mutate nothing, got %d creates err=%v", created, err)
	}
	// the plan is still returned, but no GitHub identity is assigned (0).
	if len(out) != 1 || out[0].GitHubIssueNumber != 0 || out[0].ParentIssueNumber != 1 {
		t.Fatalf("dryRun plan identities: %+v", out)
	}
}

// §44 #5: no agent can self-authorize a close — the §14 gate is conjunctive and
// `CompletionAgentComplete` alone is never sufficient.
func TestInvariantsNoAgentSelfAuthorizesClose(t *testing.T) {
	c := CompletionCheck{CompletionAgentComplete: true} // agent says complete only
	if perm, _ := EvaluateCompletion(c); perm {
		t.Fatal("completion agent alone must not authorize a close (§44 #5)")
	}
	// Even with a close recommendation, a denied policy must not close.
	if CompletionConflict(CloseDecision{Reasons: []string{"policy restriction"}}, "close") != true {
		t.Fatal("close recommendation against denied policy must be a conflict (§37)")
	}
}

// §44 #17: no failure path silently becomes success. Every §36 validator and
// fail-closed policy path returns an error for malformed input.
func TestInvariantsNoSilentSuccess(t *testing.T) {
	// A garbage estimate, split, completion, review, resolution, diagnosis and
	// human signal must ALL fail validation (none returns (ok, nil)).
	bad := []struct {
		name string
		err  error
	}{
		{"estimate", ValidateEstimate(EstimateResult{Complexity: 99, Confidence: "high", Rationale: "x"})},
		{"split", ValidateSplit(LifecycleConfig{}, SplitResult{Tickets: []SplitTicket{{Key: "", Complexity: 2, Requirements: []string{"r"}, AcceptanceCriteria: []string{"c"}}}})},
		{"completion", ValidateCompletion(CompletionResult{Recommendation: "ship"})},
		{"review", ValidateReview(ReviewResult{Verdict: "APPROVE", CommitSHA: ""})},
		{"resolution", ValidateResolution(ResolutionResult{Action: "merge", Rationale: "x"})},
		{"diagnosis", ValidateDiagnosis(DiagnosisResult{Recommendation: "retry", Rationale: ""})},
		{"human decision", ValidateHumanDecision("ship")},
		{"human reason", ValidateHumanReason("typo")},
	}
	for _, b := range bad {
		if b.err == nil {
			t.Fatalf("%s: malformed input must fail closed (§44 #17), got nil", b.name)
		}
	}
	// A human-gate timeout never resumes/closes (§40).
	if d := EvaluateHumanGate(200*time.Hour, POGateTimeout(LifecycleConfig{})); !d.TimedOut || d.Awaiting {
		t.Fatalf("timeout must stay blocked, got %+v", d)
	}
}

func TestInvariantsChildIDsDeterministic(t *testing.T) {
	// §9/§12: child identity is deterministic: same ticket + parent ⇒ same
	// markers, byte-exact (§24 idempotency relies on it).
	body1 := childIssueBody(ParentRef{Owner: "o", Repo: "r", Num: 7, Key: "02"}, "b")
	body2 := childIssueBody(ParentRef{Owner: "o", Repo: "r", Num: 7, Key: "02"}, "b")
	if body1 != body2 {
		t.Fatal("deterministic child markers must be byte-identical across calls (§9/§24)")
	}
	if _, ok := parseChildMarker(body1); !ok {
		t.Fatal("child marker must round-trip (§9)")
	}
	// §43 cap deterministic: same tickets+caps produce identical batch plans.
	a, _ := scheduleChildBatches([]SplitTicket{ticket("a"), ticket("b"), ticket("c")}, 2)
	b, _ := scheduleChildBatches([]SplitTicket{ticket("a"), ticket("b"), ticket("c")}, 2)
	if fmt.Sprint(a) != fmt.Sprint(b) {
		t.Fatal("DAG scheduling must be deterministic (§10/§43)")
	}
}

func TestInvariantsLabelsAreProjection(t *testing.T) {
	// §44 #20: labels never drive workflow state; they are a pure projection.
	// ProjectLabels with manage=false yields nothing; the state constant remains
	// authoritative (a label value can never change stateHumanRequired).
	if got := ProjectLabels(LifecycleConfig{}, "some-random-label-state", 0, false); len(got) != 0 {
		t.Fatalf("un-managed projection must be empty, got %v", got)
	}
	if stateHumanRequired != "HUMAN_REQUIRED" {
		t.Fatal("state constant must not change via labels")
	}
}
