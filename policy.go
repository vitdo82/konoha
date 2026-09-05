package main

import (
	"fmt"
	"strings"
)

// Deterministic policy engine (§4.8/§5/§12/§14/§19). Plain Go, zero LLM calls.
// It answers "is this action permitted?" from validated agent artifacts
// (EstimateResult), repo config (LifecycleConfig) and observed facts. Issue and
// repository content sit below the authority line (§5) and are never an input
// here, so untrusted text cannot override a decision (§27).

// defaultDevelopmentMaxComplexity is the §11 executable-child ceiling when
// repos.yaml leaves developmentMaxComplexity unset.
const defaultDevelopmentMaxComplexity = 4

// defaultMaxDevReviewAttempts is the §16 per-issue development/repair attempt
// cap when repos.yaml leaves maxDevReviewAttempts unset. Counted per issue: the
// initial develop is attempt 1, each repair round adds one.
const defaultMaxDevReviewAttempts = 2

// DevelopmentMaxComplexity returns the §11 executable-complexity threshold for
// the repo, defaulting to 4 when unset. Config is the authority (§5); agents
// only report a score.
func DevelopmentMaxComplexity(lc LifecycleConfig) int {
	if lc.DevelopmentMaxComplexity >= 1 {
		return lc.DevelopmentMaxComplexity
	}
	return defaultDevelopmentMaxComplexity
}

// MaxDevelopmentAttempts returns the §16 cap on development/repair attempts per
// issue, defaulting to 2 when unset. The workflow counts attempts (initial
// develop + repairs) and must never schedule another once the cap is reached.
func MaxDevelopmentAttempts(lc LifecycleConfig) int {
	if lc.MaxDevReviewAttempts >= 1 {
		return lc.MaxDevReviewAttempts
	}
	return defaultMaxDevReviewAttempts
}

// EffectiveClosePolicy returns the §19 close policy, defaulting to risk-based.
func EffectiveClosePolicy(lc LifecycleConfig) string {
	if lc.Autonomy.ClosePolicy == "" {
		return closePolicyRiskBased
	}
	return lc.Autonomy.ClosePolicy
}

// hasLabel reports whether the issue carries any configured escalation label
// (e.g. "security", §20). Byte-exact against humanRequiredLabels.
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

// RepairLoopDecision is the §16 verdict after a REQUEST_CHANGES review: either
// another repair attempt is Allowed, or the per-issue attempt cap is reached so
// the workflow must Escalate to the §17 AI diagnosis path.
type RepairLoopDecision struct {
	Allowed  bool
	Escalate bool
	Reasons  []string
}

// defaultMaxResolutionAttempts is the §37 cap on disagreement-resolution passes:
// after this many rounds the disagreement is persistent and escalates to
// HUMAN_REQUIRED (§37 "if disagreement remains"). The design specifies exactly
// one resolution before escalation, hence the default of 1.
const defaultMaxResolutionAttempts = 1

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
// persists past the resolution-attempt cap (default 1) it escalates to
// HUMAN_REQUIRED — under NO circumstances is a close considered while
// disagreement holds (§13/§18/§44 #5). `resolutionAttempts` is the count of
// resolution passes already executed for this issue. The cap is a design
// constant (§37); if per-repo control is ever needed it becomes a
// LifecycleConfig key. ponytail: fixed constant, add config when a repo asks.
func EvaluateDisagreement(disagreement bool, resolutionAttempts int) DisagreementDecision {
	if !disagreement {
		return DisagreementDecision{}
	}
	if resolutionAttempts >= defaultMaxResolutionAttempts {
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
