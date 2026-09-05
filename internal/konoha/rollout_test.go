package konoha

import (
	"context"
	"testing"
)

// §42: the default is conservative — an unconfigured repo is treated as shadow
// (never above its declared phase).
func TestRolloutConservativeDefaults(t *testing.T) {
	if got := rolloutPhase(LifecycleConfig{}); got != phaseShadow {
		t.Fatalf("empty config phase = %q, want shadow", got)
	}
	if canDevelop(LifecycleConfig{}) {
		t.Fatal("shadow default must not develop")
	}
	// review-only mode is also shadow (analysis runs, nothing mutates).
	if got := rolloutPhase(LifecycleConfig{Mode: modeReviewOnly}); got != phaseShadow {
		t.Fatalf("review-only phase = %q, want shadow", got)
	}
}

// §42 phases: dryRun+supervised → shadow; 1-2 ceiling autonomous → phase-2;
// 1-4 ceiling risk-based → phase-3; defaults conserve.
func TestRolloutPhaseMapping(t *testing.T) {
	if got := rolloutPhase(LifecycleConfig{Mode: modeSupervisedAutonomous, DryRun: true}); got != phaseShadow {
		t.Fatalf("dryRun supervised = %q, want shadow (Phase 1)", got)
	}
	ph2 := LifecycleConfig{Mode: modeSupervisedAutonomous, DevelopmentMaxComplexity: 2,
		Autonomy: Autonomy{ClosePolicy: closePolicyRiskBased}}
	if got := rolloutPhase(ph2); got != phaseSmall {
		t.Fatalf("1-2 ceiling = %q, want phase-2", got)
	}
	ph3 := LifecycleConfig{Mode: modeSupervisedAutonomous, DevelopmentMaxComplexity: 4,
		Autonomy: Autonomy{ClosePolicy: closePolicyRiskBased}}
	if got := rolloutPhase(ph3); got != phaseMedium {
		t.Fatalf("1-4 risk-based = %q, want phase-3", got)
	}
}

// §42/§11: the effective developable ceiling is the min of the declared ceiling
// and the phase ceiling — a shadow repo develops nothing, a phase-2 repo caps at
// 2, and phase-3 caps at the declared 4.
func TestEffectiveDevelopmentMax(t *testing.T) {
	cases := []struct {
		name string
		lc   LifecycleConfig
		want int
	}{
		{"shadow empty", LifecycleConfig{}, 0},
		{"shadow dryRun", LifecycleConfig{Mode: modeSupervisedAutonomous, DryRun: true, DevelopmentMaxComplexity: 4}, 0},
		{"phase2 declared 2", LifecycleConfig{Mode: modeSupervisedAutonomous, DevelopmentMaxComplexity: 2,
			Autonomy: Autonomy{ClosePolicy: closePolicyRiskBased}}, 2},
		{"phase3 declared 3", LifecycleConfig{Mode: modeSupervisedAutonomous, DevelopmentMaxComplexity: 3,
			Autonomy: Autonomy{ClosePolicy: closePolicyRiskBased}}, 3},
		{"phase3 declared 4", LifecycleConfig{Mode: modeSupervisedAutonomous, DevelopmentMaxComplexity: 4,
			Autonomy: Autonomy{ClosePolicy: closePolicyRiskBased}}, 4},
	}
	for _, tc := range cases {
		if got := effectiveDevelopmentMax(tc.lc); got != tc.want {
			t.Errorf("%s: effectiveDevelopmentMax = %d want %d", tc.name, got, tc.want)
		}
	}
}

// §42 Phase 4: decomposition is enabled only past shadow with a ≥4 ceiling;
// shadow and phase-2 (ceiling 2) repos never spawn children.
func TestPhaseCanDecompose(t *testing.T) {
	shadow := LifecycleConfig{Mode: modeSupervisedAutonomous, DryRun: true, DevelopmentMaxComplexity: 4}
	if phaseCanDecompose(shadow) {
		t.Fatal("shadow must not decompose")
	}
	ph2 := LifecycleConfig{Mode: modeSupervisedAutonomous, DevelopmentMaxComplexity: 2}
	if phaseCanDecompose(ph2) {
		t.Fatal("phase-2 (ceiling 2) must not decompose (<4)")
	}
}

// §42 acceptance: shadow mode runs the whole pipeline with ZERO GitHub
// mutations — every mutating activity honors dryRun. Enumerate each one and
// prove the ghRunner (the single mutation seam) is never invoked.
func TestShadowModeZeroMutations(t *testing.T) {
	ctx := context.Background()
	prev := ghRunner
	mutations := 0
	ghRunner = func(c context.Context, args ...string) ([]byte, error) {
		mutations++
		return []byte(`[]`), nil
	}
	defer func() { ghRunner = prev }()

	// Ensure* family (reconcile.go).
	if _, err := EnsureIssue(ctx, IssueRequest{Owner: "o", Repo: "r", DryRun: true}); err != nil {
		t.Fatal(err)
	}
	if err := EnsureIssueState(ctx, StateRequest{Owner: "o", Repo: "r", Num: 1, State: "closed", DryRun: true}); err != nil {
		t.Fatal(err)
	}
	if err := EnsureIssueLabels(ctx, LabelsRequest{Owner: "o", Repo: "r", Num: 1, DryRun: true}); err != nil {
		t.Fatal(err)
	}
	// children (idempotent create).
	if _, err := EnsureChildren(ctx, ChildCreateRequest{Owner: "o", Repo: "r", ParentNum: 1, DryRun: true,
		Tickets: []SplitTicket{ticket("01")}}); err != nil {
		t.Fatal(err)
	}
	// labels projection.
	if err := EnsureRepoLabels(ctx, LabelsTheirsRequest{Owner: "o", Repo: "r", DryRun: true}); err != nil {
		t.Fatal(err)
	}
	if err := ApplyIssueLabels(ctx, LabelsRequest{Owner: "o", Repo: "r", Num: 1, DryRun: true}); err != nil {
		t.Fatal(err)
	}

	if mutations != 0 {
		t.Fatalf("shadow mode made %d gh mutations, want 0 (§42)", mutations)
	}
}
