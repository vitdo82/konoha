package konoha

import (
	"fmt"
	"strings"
	"time"
)

// Deterministic policy engine (§4.8/§5/§12/§14/§19). Plain Go, zero LLM calls.
// It answers "is this action permitted?" from validated agent artifacts
// (EstimateResult), repo config (LifecycleConfig) and observed facts. Issue and
// repository content sit below the authority line (§5) and are never an input
// here, so untrusted text cannot override a decision (§27).

// DevelopmentMaxComplexity returns the §11 executable-complexity threshold for
// the repo, defaulting to the configured `defaults.lifecycle` value (4) when
// unset (#48). Config is the authority (§5); agents only report a score.
func DevelopmentMaxComplexity(lc LifecycleConfig) int {
	if lc.DevelopmentMaxComplexity >= 1 {
		return lc.DevelopmentMaxComplexity
	}
	return appCfg.Defaults.Lifecycle.DevelopmentMaxComplexity
}

// MaxDevelopmentAttempts returns the §16 cap on development/repair attempts per
// issue, defaulting to the configured value (2) when unset. The workflow counts
// attempts (initial develop + repairs) and must never schedule another once the
// cap is reached.
func MaxDevelopmentAttempts(lc LifecycleConfig) int {
	if lc.MaxDevReviewAttempts >= 1 {
		return lc.MaxDevReviewAttempts
	}
	return appCfg.Defaults.Lifecycle.MaxDevReviewAttempts
}

// EffectiveClosePolicy returns the §19 close policy, defaulting to risk-based.
func EffectiveClosePolicy(lc LifecycleConfig) string {
	if lc.Autonomy.ClosePolicy == "" {
		return closePolicyRiskBased
	}
	return lc.Autonomy.ClosePolicy
}

// §49 merge strategies (whitelist). The policy engine validates config and
// resolves per-repo defaults; the merge activity re-validates (fail-closed).
const (
	mergeStrategySquash = "squash"
	mergeStrategyRebase = "rebase"
	mergeStrategyMerge  = "merge"
)

// ValidMergeStrategy reports whether s is a supported §49 merge strategy.
func ValidMergeStrategy(s string) bool {
	switch s {
	case mergeStrategySquash, mergeStrategyRebase, mergeStrategyMerge:
		return true
	}
	return false
}

// MergeStrategy resolves the per-repo merge strategy: the repo's declared
// strategy, else the configured default, else squash. Config validation
// guarantees a declared value is in the whitelist; this stays fail-closed for a
// direct struct.
func MergeStrategy(lc LifecycleConfig) string {
	if ValidMergeStrategy(lc.Merge.Strategy) {
		return lc.Merge.Strategy
	}
	if ValidMergeStrategy(appCfg.Defaults.Lifecycle.Merge.Strategy) {
		return appCfg.Defaults.Lifecycle.Merge.Strategy
	}
	return mergeStrategySquash
}

// RequireCI resolves the §49 CI status gate. An explicit per-repo value wins;
// otherwise the configured default; otherwise true (fail-closed: gate the merge
// on green CI unless a repo explicitly opts out).
func RequireCI(lc LifecycleConfig) bool {
	if lc.Merge.RequireCI != nil {
		return *lc.Merge.RequireCI
	}
	if d := appCfg.Defaults.Lifecycle.Merge.RequireCI; d != nil {
		return *d
	}
	return true
}

// CIMaxWait resolves the §49 bounded CI poll window.
func CIMaxWait(lc LifecycleConfig) time.Duration {
	if d, err := time.ParseDuration(lc.Merge.CIMaxWait); err == nil && d > 0 {
		return d
	}
	if d, err := time.ParseDuration(appCfg.Defaults.Lifecycle.Merge.CIMaxWait); err == nil && d > 0 {
		return d
	}
	return 10 * time.Minute
}

// validationSpec resolves the §26 validation profile: a repo that declares
// either structured gates or a legacy command wins; otherwise the process
// default (defaults.lifecycle) applies.
func validationSpec(lc LifecycleConfig) (gates []ValidationGate, cmd string) {
	if len(lc.Validation) > 0 || strings.TrimSpace(lc.ValidationCmd) != "" {
		return lc.Validation, lc.ValidationCmd
	}
	d := appCfg.Defaults.Lifecycle
	return d.Validation, d.ValidationCmd
}

// ValidationGates resolves the §26 structured gate list (§26).
func ValidationGates(lc LifecycleConfig) []ValidationGate {
	g, _ := validationSpec(lc)
	return g
}

// ValidationCommand resolves the §26 legacy shell-string fallback.
func ValidationCommand(lc LifecycleConfig) string {
	_, c := validationSpec(lc)
	return c
}

// builtinHumanRequiredLabels are §20 risk labels Konoha always escalates on,
// independent of repo config: a §52 security/dependency finding must never
// auto-merge, even under closePolicy: autonomous. They extend (never replace)
// the configured humanRequiredLabels.
func builtinHumanRequiredLabels() []string {
	return []string{labelSecurity, labelDependency}
}

// hasEscalationLabel reports whether the issue carries any escalation label —
// the repo's configured humanRequiredLabels plus the built-in §52 security/
// dependency labels. Byte-exact, so a non-konoha "security" label is not
// conflated with `konoha:security`.
func hasEscalationLabel(labels, wanted []string) bool {
	have := make(map[string]bool, len(labels))
	for _, l := range labels {
		have[l] = true
	}
	for _, w := range wanted {
		if have[w] {
			return true
		}
	}
	for _, w := range builtinHumanRequiredLabels() {
		if have[w] {
			return true
		}
	}
	return false
}

// restrictedPathMatch reports whether any path matches any humanRequiredPaths
// glob (§20). Empty patterns or paths mean no restriction observed.
func restrictedPathMatch(patterns, paths []string) bool {
	for _, p := range paths {
		for _, pat := range patterns {
			if globMatch(pat, p) {
				return true
			}
		}
	}
	return false
}

// globMatch is a deterministic `**`/`*`/`?` matcher for slash-separated path
// globs. `**` consumes zero-or-more segments so "infra/**" matches "infra",
// "infra/db/x.go", etc.; `*`/`?` match within a single segment. No regexp.
func globMatch(pattern, path string) bool {
	pp, sp := strings.Split(pattern, "/"), strings.Split(path, "/")
	var m func(pi, si int) bool
	m = func(pi, si int) bool {
		if pi == len(pp) {
			return si == len(sp)
		}
		if pp[pi] == "**" {
			for s := si; s <= len(sp); s++ {
				if m(pi+1, s) {
					return true
				}
			}
			return false
		}
		if si == len(sp) {
			return false
		}
		if !segMatch(pp[pi], sp[si]) {
			return false
		}
		return m(pi+1, si+1)
	}
	return m(0, 0)
}

// segMatch matches one glob segment against one path segment (`*`, `?`).
func segMatch(pat, seg string) bool {
	var m func(pi, si int) bool
	m = func(pi, si int) bool {
		if pi == len(pat) {
			return si == len(seg)
		}
		switch pat[pi] {
		case '*':
			for s := si; s <= len(seg); s++ {
				if m(pi+1, s) {
					return true
				}
			}
			return false
		case '?':
			return si < len(seg) && m(pi+1, si+1)
		default:
			return si < len(seg) && pat[pi] == seg[si] && m(pi+1, si+1)
		}
	}
	return m(0, 0)
}

// AutonomyDecision is the §12 verdict: may this issue proceed autonomously?
// Reasons lists every failing condition (empty when autonomous).
type AutonomyDecision struct {
	Autonomous bool
	Reasons    []string
}

// RiskDecision is the §20 risk-based escalation verdict. Escalate is true when
// the change is high-risk (a diff path matches humanRequiredPaths or the issue
// carries a humanRequiredLabels label): such work is blocked from autonomous
// publication and, under the risk-based close policy, can only be closed by a
// human decision. Reasons name which trigger fired.
type RiskDecision struct {
	Escalate bool
	Reasons  []string
}

// DetectHumanEscalation applies the §20 risk gate. paths are the repo-relative
// files the work touches (e.g. DevelopResult.ChangedFiles); labels are the
// issue's labels. Deterministic and config-driven (§33): unlisted paths/labels
// leave ordinary work autonomous (fail-open by absence, the §12 contract). This
// is the same §20 signal EvaluateAutonomy ingests, exposed inline so the
// workflow can gate BEFORE publication/close rather than inferring from a
// downstream decision.
func DetectHumanEscalation(lc LifecycleConfig, paths, labels []string) RiskDecision {
	var reasons []string
	if restrictedPathMatch(lc.Autonomy.HumanRequiredPaths, paths) {
		reasons = append(reasons, "touches restricted path")
	}
	if hasEscalationLabel(labels, lc.Autonomy.HumanRequiredLabels) {
		reasons = append(reasons, "human-required label")
	}
	return RiskDecision{Escalate: len(reasons) > 0, Reasons: reasons}
}

// EvaluateAutonomy applies the §12 predicate to a validated EstimateResult plus
// repo config:
//
//	complexity <= DevelopmentMaxComplexity
//	AND confidence != low
//	AND no touched path matches humanRequiredPaths
//	AND no human-required escalation label present
//
// Deterministic and fail-closed: unset config resolves to documented defaults
// (threshold 4, close policy risk-based), an out-of-contract estimate (complexity
// 0, unknown confidence) is never autonomous, and only the structured fields
// (complexity/confidence) influence the decision — Rationale/Risks/Assumptions
// freeform text is ignored (§5/§27: agent prose cannot override policy). `paths`
// are repo-relative files the work would touch; `labels` are the issue's labels.
// Empty means no risk observed (fail-open by absence, which is the §12 contract).
func EvaluateAutonomy(lc LifecycleConfig, e EstimateResult, paths, labels []string) AutonomyDecision {
	var reasons []string
	if e.Complexity < 1 {
		reasons = append(reasons, "complexity below rubric")
	} else if e.Complexity > DevelopmentMaxComplexity(lc) {
		reasons = append(reasons, "complexity above threshold")
	}
	if e.Confidence != confidenceLow && e.Confidence != confidenceMedium && e.Confidence != confidenceHigh {
		reasons = append(reasons, "confidence outside controlled set")
	} else if e.Confidence == confidenceLow {
		reasons = append(reasons, "confidence low")
	}
	if restrictedPathMatch(lc.Autonomy.HumanRequiredPaths, paths) {
		reasons = append(reasons, "touches restricted path")
	}
	if hasEscalationLabel(labels, lc.Autonomy.HumanRequiredLabels) {
		reasons = append(reasons, "human-required label")
	}
	return AutonomyDecision{Autonomous: len(reasons) == 0, Reasons: reasons}
}

// CompletionCheck is the §14 completion gate. Every condition must pass before
// EnsureIssueState(CLOSED). CompletionAgentComplete is the independent
// completion-agent verdict, NOT a system instruction, and on its own can never
// authorize a close: the gate is conjunctive over all conditions (§44 #5), so
// an agent cannot force a close its own evaluation permits.
type CompletionCheck struct {
	ImplementationExists     bool // implementation exists
	PREnabled                bool // PR exists
	LatestCommitReviewed     bool // latest commit reviewed (not a stale review)
	ReviewApproved           bool // review = APPROVED
	TestsPass                bool // tests pass
	CompletionAgentComplete  bool // completion agent = COMPLETE
	NoBlockingOpenQuestions  bool // no blocking open questions
	NoPolicyRestriction      bool // no close-policy restriction
	RequiredChildrenComplete bool // all required child issues complete
	AgentDisagreement        bool // reviewer/completion disagree (§13/§18)
}

// EvaluateCompletion applies the §14 gate: all listed conditions must hold
// before EnsureIssueState(CLOSED). Failing Reasons name the missing condition.
// AgentDisagreement short-circuits to not-permitted regardless of the other
// bits: an issue where agents disagree routes to resolution and is never
// auto-closed (§13/§18/§44 #5).
func EvaluateCompletion(c CompletionCheck) (permitted bool, reasons []string) {
	check := func(ok bool, name string) {
		if !ok {
			reasons = append(reasons, name)
		}
	}
	check(c.ImplementationExists, "implementation exists")
	check(c.PREnabled, "PR exists")
	check(c.LatestCommitReviewed, "latest commit reviewed")
	check(c.ReviewApproved, "review not approved")
	check(c.TestsPass, "tests not passing")
	check(c.CompletionAgentComplete, "completion agent not COMPLETE")
	check(c.NoBlockingOpenQuestions, "blocking open questions remain")
	check(c.NoPolicyRestriction, "policy restriction")
	check(c.RequiredChildrenComplete, "required child issues incomplete")
	check(!c.AgentDisagreement, "agent disagreement")
	return len(reasons) == 0, reasons
}

// CloseDecision is the §19 verdict on automatic closure.
type CloseDecision struct {
	Close      bool
	Escalation bool // requires human intervention; never auto-closed
	Reasons    []string
}

// EvaluateClosePolicy resolves the §19 close policy on top of a §14 completion
// gate. Modes:
//
//	autonomous     -> automatic close whenever the gate passes.
//	human-required -> always escalates; only humans close.
//	risk-based     -> default; automatic close unless the change is high-risk
//	                  (restricted paths / escalation labels per §20), which
//	                  escalates even when the gate passes.
//
// A failing gate is never closed under any policy, and agent disagreement (via
// EvaluateCompletion) always blocks (§44 #5). `paths`/`labels` are the same
// §20 risk signals EvaluateAutonomy ingests.
func EvaluateClosePolicy(lc LifecycleConfig, c CompletionCheck, paths, labels []string) CloseDecision {
	permitted, reasons := EvaluateCompletion(c)
	if !permitted {
		return CloseDecision{Reasons: reasons}
	}
	// §52: the built-in security/dependency labels always require a human, even
	// under closePolicy: autonomous — a known-CVE change is never auto-closed.
	if hasEscalationLabel(labels, nil) {
		return CloseDecision{Escalation: true, Reasons: []string{"built-in security/dependency label"}}
	}
	switch EffectiveClosePolicy(lc) {
	case closePolicyHumanRequired:
		return CloseDecision{Escalation: true, Reasons: []string{"closePolicy human-required"}}
	case closePolicyAutonomous:
		return CloseDecision{Close: true}
	default: // closePolicyRiskBased
		if restrictedPathMatch(lc.Autonomy.HumanRequiredPaths, paths) || hasEscalationLabel(labels, lc.Autonomy.HumanRequiredLabels) {
			return CloseDecision{Escalation: true, Reasons: []string{"high-risk change"}}
		}
		return CloseDecision{Close: true}
	}
}

// releasedClaim reports whether an issue has released its run's claim (#141):
// it is no longer open, or a human applied one of the configured release labels
// (default duplicate/wontfix/invalid). Both signals are explicit, so a missing
// label or an empty state never stops a healthy run — the caller fails closed on
// a fetch error instead of guessing (§40). Case-sensitive: GitHub renders a
// label as it was created, so an exact match is the strict reading.
func releasedClaim(state string, labels []string, cfg ReconcileConfig) bool {
	if state != "" && !strings.EqualFold(state, "open") {
		return true
	}
	release := cfg.ReleasedLabels
	if len(release) == 0 {
		release = defaultReleasedLabels
	}
	for _, l := range labels {
		for _, r := range release {
			if l == r {
				return true
			}
		}
	}
	return false
}

// RepairLoopDecision is the §16 verdict after a REQUEST_CHANGES review: either
// another repair attempt is Allowed, or the per-issue attempt cap is reached so
// the workflow must Escalate to the §17 AI diagnosis path.
type RepairLoopDecision struct {
	Allowed  bool
	Escalate bool
	Reasons  []string
}

// DisagreementDecision is the §37/§18 verdict on a reviewer/completion
// disagreement. Resolve routes to the Resolution Agent; Human escalates to
// HUMAN_REQUIRED (persistent disagreement, fix #13). An issue where this fires
// is never auto-closed: the workflow consults this BEFORE the §14 close gate.
type DisagreementDecision struct {
	Disagree bool
	Resolve  bool // run the Resolution Agent to pick a bounded next action
	Human    bool // persistent disagreement → HUMAN_REQUIRED
	Reasons  []string
}

// EvaluateDisagreement applies the §37 rule: when the reviewer and completion
// agents disagree, the issue must route to Resolution, and if disagreement
// persists past the resolution-attempt cap (config `defaults.lifecycle.
// maxResolutionAttempts`, default 1) it escalates to HUMAN_REQUIRED — under NO
// circumstances is a close considered while disagreement holds (§13/§18/§44
// #5). `resolutionAttempts` is the count of resolution passes already executed
// for this issue.
func EvaluateDisagreement(disagreement bool, resolutionAttempts int) DisagreementDecision {
	if !disagreement {
		return DisagreementDecision{}
	}
	capCount := appCfg.Defaults.Lifecycle.MaxResolutionAttempts
	if capCount < 1 {
		capCount = 1
	}
	if resolutionAttempts >= capCount {
		return DisagreementDecision{
			Disagree: true,
			Human:    true,
			Reasons:  []string{"disagreement persists after 1 resolution attempt (§37) → HUMAN_REQUIRED"},
		}
	}
	return DisagreementDecision{
		Disagree: true,
		Resolve:  true,
		Reasons:  []string{"reviewer/completion disagree (§37): route to Resolution Agent"},
	}
}

// EvaluateRepairLoop answers the §4.8 policy question "can another development
// attempt occur?" (§16). attempts is the count of development/repair attempts
// already completed per issue (initial develop = 1, each repair round +1). While
// under MaxDevelopmentAttempts another repair may run; at/over the cap the loop
// must stop and route to AI escalation (§17) — never a silent pass (a REQUEST_
// CHANGES verdict is never treated as success, §40).
func EvaluateRepairLoop(lc LifecycleConfig, attempts int) RepairLoopDecision {
	if attempts < MaxDevelopmentAttempts(lc) {
		return RepairLoopDecision{Allowed: true}
	}
	return RepairLoopDecision{
		Escalate: true,
		Reasons:  []string{fmt.Sprintf("maxDevReviewAttempts %d reached (§16) → AI escalation (§17)", MaxDevelopmentAttempts(lc))},
	}
}

// POGateTimeout resolves the per-repo human-response timeout, falling back to
// the configured `defaults.lifecycle.poGateTimeout` (default 168h) (#48). Config
// validation guarantees a configured repos.yaml poGateTimeout parses; a direct
// struct with an unparseable string falls back to the default.
func POGateTimeout(lc LifecycleConfig) time.Duration {
	if lc.POGateTimeout != "" {
		if d, err := time.ParseDuration(lc.POGateTimeout); err == nil {
			return d
		}
	}
	return appCfg.Defaults.Lifecycle.poTimeout()
}

// HumanGateDecision is the §18/§40 wait verdict while the workflow is in
// HUMAN_REQUIRED. There is INTENTIONALLY no "auto-proceed on timeout" path:
// awaited or timed-out, the workflow stays put and never silently succeeds.
type HumanGateDecision struct {
	Awaiting bool
	TimedOut bool
}

// EvaluateHumanGate decides what "waiting on a human" looks like. Before the
// poGateTimeout window the workflow keeps Awaiting; once elapsed it is TimedOut
// and must remain BLOCKED (§40) — a timeout never resumes, closes, or otherwise
// turns into success.
func EvaluateHumanGate(elapsed, timeout time.Duration) HumanGateDecision {
	if elapsed >= timeout {
		return HumanGateDecision{TimedOut: true}
	}
	return HumanGateDecision{Awaiting: true}
}

// HumanDecisionTransition is the deterministic workflow response to a §18 human
// decision. Terminal actions map to a lifecycle state; Resume signals the normal
// lifecycle path continues. The policy engine decides — the human signal never
// bypasses it (§5/§44 #4).
type HumanDecisionTransition struct {
	TargetState string
	Resume      bool
}

// ApplyHumanDecision maps each bounded §18 human decision to its workflow
// response: block/cancel/close are terminal (→ BLOCKED/CANCELLED/CLOSED);
// resume/reestimate/repartition/retry/approve re-enter the normal lifecycle;
// deny escalates to HUMAN_REQUIRED (§55: the human refused the pre-gate, so a
// high-assurance repo stops and never silently proceeds). Malformed decisions
// fail closed (error, §36).
func ApplyHumanDecision(d HumanDecision) (HumanDecisionTransition, error) {
	if err := ValidateHumanDecision(d.Action); err != nil {
		return HumanDecisionTransition{}, err
	}
	switch d.Action {
	case humanDecisionBlock:
		return HumanDecisionTransition{TargetState: stateBlocked}, nil
	case humanDecisionCancel:
		return HumanDecisionTransition{TargetState: stateCancelled}, nil
	case humanDecisionClose:
		return HumanDecisionTransition{TargetState: stateClosed}, nil
	case humanDecisionDeny:
		return HumanDecisionTransition{TargetState: stateHumanRequired}, nil
	default: // resume, reestimate, repartition, retry, approve
		return HumanDecisionTransition{Resume: true}, nil
	}
}

// DiagnosisTransition is the deterministic response to a §17 diagnosis
// recommendation. TargetState names the state to enter; Human means the
// workflow must route to HUMAN_REQUIRED (§40).
type DiagnosisTransition struct {
	TargetState string
	Human       bool
}

// ApplyDiagnosis maps the bounded §17 diagnosis recommendations to their
// transitions: retry → re-enter development, repartition → re-enter splitting,
// block → alert a human (HUMAN_REQUIRED). The agent never applies its own
// recommendation (§17/§44 #4); this deterministic policy decides. A malformed
// recommendation (validation failed, §36) fails closed and maps to Human —
// "diagnosis cannot resolve → HUMAN_REQUIRED, no silent success" (§40).
func ApplyDiagnosis(d DiagnosisResult) (DiagnosisTransition, error) {
	if err := ValidateDiagnosis(d); err != nil {
		return DiagnosisTransition{Human: true}, fmt.Errorf("diagnosis invalid → HUMAN_REQUIRED (§40): %w", err)
	}
	switch d.Recommendation {
	case diagRetry:
		return DiagnosisTransition{TargetState: stateDeveloping}, nil
	case diagRepartition:
		return DiagnosisTransition{TargetState: stateSplitting}, nil
	case diagBlock:
		return DiagnosisTransition{Human: true, TargetState: stateHumanRequired}, nil
	}
	return DiagnosisTransition{Human: true}, nil // unreachable; validator guarantees membership
}

// defaultReleasedLabels are the human-applied labels that release a claim when
// lifecycle.reconcile.releasedLabels is unset (#141).
var defaultReleasedLabels = []string{"duplicate", "wontfix", "invalid"}
