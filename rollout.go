package main

// Rollout policy (§42): staged autonomy per repo, config-driven, no code edits.
//
//	Phase 1 shadow : analyze/estimate/split for quality, ZERO GitHub mutation
//	                (mode supervised-autonomous + dryRun).
//	Phase 2        : complexity 1-2 autonomous, risk-based close.
//	Phase 3        : complexity 1-4 autonomous, risk-based close (the default).
//	Phase 4        : decomposition (children) enabled.
//	Phase 5        : tuning via observed §39 metrics (no gate change here).
//
// A repo never exceeds its declared phase: the effective autonomy ceiling is the
// MIN of what the config declares. Conservative defaults mean an unset repo runs
// review-only (phase 1 shadow with all mutations suppressed).

// §42 phase identifiers, ordered from most conservative to most permissive.
const (
	phaseShadow = "shadow"  // Phase 1: dry-run whole pipeline, zero mutations
	phaseSmall  = "phase-2" // Phase 2: complexity 1-2 autonomous
	phaseMedium = "phase-3" // Phase 3: complexity 1-4 autonomous + risk-based close
)

// rolloutPhase derives the repo's effective §42 phase from its config. It never
// advances the repo beyond what the config declares, and defaults conservatively
// (review-only mode → shadow; unset → shadow). The phase label follows the
// declared executable ceiling: the ceiling IS the declared phase, so a repo
// declaring developmentMaxComplexity: 2 runs Phase 2, and 4 runs Phase 3.
func rolloutPhase(lc LifecycleConfig) string {
	if lc.Mode != modeSupervisedAutonomous || lc.DryRun || lc.Mode == modeReviewOnly {
		return phaseShadow // Phase 1 (analysis runs, nothing mutates)
	}
	switch {
	case EffectiveClosePolicy(lc) != closePolicyRiskBased:
		return phaseShadow // conservative: non-risk-based → keep shadow
	case DevelopmentMaxComplexity(lc) <= 2:
		return phaseSmall // Phase 2
	default:
		return phaseMedium // Phase 3 (ceiling 3-4, risk-based)
	}
}

// autonomyComplexityCeiling returns the §42 phase-gated executable-complexity
// ceiling: phase shadow → 0 (nothing develops); phase-2 → 2; phase-3 → 4. The
// repo's DevelopmentMaxComplexity may be lower, and routing uses the min.
func autonomyComplexityCeiling(lc LifecycleConfig) int {
	switch rolloutPhase(lc) {
	case phaseShadow:
		return 0
	case phaseSmall:
		return 2
	default:
		return 4
	}
}

// canDevelop reports whether the repo's declared phase permits development at
// all (Phase 1 shadow analyzes/estimates/splits but never mutates).
func canDevelop(lc LifecycleConfig) bool {
	return rolloutPhase(lc) != phaseShadow && !lc.DryRun
}

// phaseCanDecompose reports whether Phase 4 (child fan-out) is enabled. A repo
// reaches decomposition once it is past shadow mode AND declares an executable
// ceiling ≥ 4, so complexity-5+ issues must split into children (§11/§8). Under
// shadow or a 1-2 ceiling, large issues must not spawn children.
func phaseCanDecompose(lc LifecycleConfig) bool {
	return canDevelop(lc) && DevelopmentMaxComplexity(lc) >= 4
}

// effectiveDevelopmentMax is the developable ceiling for routing (§11): the min
// of the repo's declared DevelopmentMaxComplexity and the rollout phase ceiling.
// Phase 1 (shadow) yields 0, so nothing is DEVELOPING in shadow mode.
func effectiveDevelopmentMax(lc LifecycleConfig) int {
	declared := DevelopmentMaxComplexity(lc)
	ceiling := autonomyComplexityCeiling(lc)
	if ceiling < declared {
		return ceiling
	}
	return declared
}
