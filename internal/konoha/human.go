package konoha

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Human intervention (§18/§19/§40): humans are exception handlers, not routine
// gates. When automation cannot safely continue the workflow enters
// HUMAN_REQUIRED with a recorded §18 reason; the human responds through a
// bounded decision channel; the deterministic policy (never the agent) applies
// the transition. The poGateTimeout never silently becomes success (§40) — see
// policy.go EvaluateHumanGate. Labels here are a projection; Temporal workflow
// state is authoritative (§44 #20).

// §18 human-required reasons. Validated bounded set: the workflow records one
// of these as the trigger that escalated the issue.
const (
	reasonRequirementsAmbiguous    = "requirements_ambiguous"
	reasonSecuritySensitive        = "security_sensitive"
	reasonArchitectureChange       = "architecture_change"
	reasonRepeatedReviewFailure    = "repeated_review_failure"
	reasonInsufficientTestCoverage = "insufficient_test_coverage"
	reasonExternalDependency       = "external_dependency"
	reasonPolicyViolation          = "policy_violation"
	reasonAgentFailure             = "agent_failure"
	reasonConflictingRequirements  = "conflicting_requirements"
	reasonCIFailed                 = "ci_failed"       // §49: CI gate not green before merge
	reasonMergeFailed              = "merge_failed"    // §49: PR merge failed after approval
	reasonDeployFailed             = "deploy_failed"   // §51: post-deploy verification failed
	reasonApprovalDenied           = "approval_denied" // §55: human denied the pre-execution approval gate
	reasonAgentBlocked             = "agent_blocked"   // #84: agent reported it cannot proceed / needs input
	reasonClaimReleased            = "claim_released"  // #141: issue closed or release-labeled mid-run
)

func validHumanReason(r string) bool {
	switch r {
	case reasonRequirementsAmbiguous, reasonSecuritySensitive, reasonArchitectureChange,
		reasonRepeatedReviewFailure, reasonInsufficientTestCoverage, reasonExternalDependency,
		reasonPolicyViolation, reasonAgentFailure, reasonConflictingRequirements,
		reasonCIFailed, reasonMergeFailed, reasonDeployFailed, reasonApprovalDenied,
		reasonAgentBlocked:
		return true
	}
	return false
}

// ValidateHumanReason gates the §18 trigger record (§36): only the controlled
// reasons may be recorded as escalating an issue.
func ValidateHumanReason(r string) error {
	if !validHumanReason(r) {
		return fmt.Errorf("reason %q not in the §18 human-required set", r)
	}
	return nil
}

// §18 human decision actions: the bounded response channel. The deterministic
// policy (HumanDecisionTransition) maps each; anything else is rejected (§36).
// `approve`/`deny` are the §55 pre-execution approval-gate signals; they reuse
// the same decision marker (and therefore the same parser + validation) rather
// than inventing a new channel.
const (
	humanDecisionResume      = "resume"
	humanDecisionReestimate  = "reestimate"
	humanDecisionRepartition = "repartition"
	humanDecisionRetry       = "retry"
	humanDecisionBlock       = "block"
	humanDecisionCancel      = "cancel"
	humanDecisionClose       = "close"
	humanDecisionApprove     = "approve" // §55: grant the pre-execution approval gate
	humanDecisionDeny        = "deny"    // §55: refuse the pre-execution approval gate
	// humanDecisionRequestChanges (§108) rejects the current gate and sends the
	// work back for a bounded rework. Declared in decisions.go; membership here.
)

func validHumanDecision(a string) bool {
	switch a {
	case humanDecisionResume, humanDecisionReestimate, humanDecisionRepartition,
		humanDecisionRetry, humanDecisionBlock, humanDecisionCancel, humanDecisionClose,
		humanDecisionApprove, humanDecisionDeny, humanDecisionRequestChanges:
		return true
	}
	return false
}

// ValidateHumanDecision deterministically gates a human signal (§36): a
// malformed decision can never drive a transition.
func ValidateHumanDecision(a string) error {
	if !validHumanDecision(a) {
		return fmt.Errorf("decision %q not in the §18 set {resume, reestimate, repartition, retry, block, cancel, close, approve, deny, request-changes}", a)
	}
	return nil
}

// §21/§22 label projections for the human-required / blocked states.
const (
	labelHumanRequired = "konoha:human-required"
	labelBlocked       = "konoha:blocked"
)

// HumanRequiredLabels is the §22 projection while an issue waits on a human.
func HumanRequiredLabels() []string { return []string{labelHumanRequired} }

// BlockedLabels is the §22 projection for a blocked issue (also human-required).
func BlockedLabels() []string { return []string{labelHumanRequired, labelBlocked} }

// HumanDecision is a validated human response (§18). Present is false when no
// decision signal exists yet; the workflow keeps waiting. Feedback/Comments
// carry the #108 request-changes payload (empty for the other actions).
type HumanDecision struct {
	Action   string        `json:"action"`
	Reason   string        `json:"reason,omitempty"` // optional note recorded with the signal (§39)
	Feedback string        `json:"feedback,omitempty"`
	Comments []DiffComment `json:"comments,omitempty"`
	Present  bool          `json:"present"`
}

// decisionMarkerOpen is the deterministic HTML-comment marker carrying the human
// decision in an issue body or comment (§24): `<!-- konoha-decision: <action> -->`.
// Reuses the byte-exact parseMarker machinery from reconcile.go, so a decision
// written by a human or API is reliably found across retries.
const decisionMarkerOpen = "<!-- konoha-decision: "

func humanDecisionMarker(action string) string {
	return decisionMarkerOpen + action + " -->"
}

func parseHumanDecisionMarker(body string) (string, bool) {
	return parseMarker(body, decisionMarkerOpen)
}

// HumanDecisionInput targets the decision channel of one issue.
type HumanDecisionInput struct {
	Owner string `json:"owner"`
	Repo  string `json:"repo"`
	Num   int    `json:"num"`
}

// ReadHumanDecision fetches the issue body and comments and returns the most
// recent valid `<!-- konoha-decision: <action> -->` marker as a validated
// HumanDecision (§18 decision channel: issue comment or manual body edit). A
// found-but-malformed marker fails closed (error, §36) so it can never drive a
// transition; no marker yields Present=false so the workflow keeps waiting.
// Read-only: never mutates GitHub (§23). Uses ghRunner (not activity logger),
// like the other reconcile activities, so unit tests can simulate GitHub.
func ReadHumanDecision(ctx context.Context, in HumanDecisionInput) (HumanDecision, error) {
	it, err := getIssue(ctx, in.Owner, in.Repo, in.Num)
	if err != nil {
		return HumanDecision{}, err
	}
	bodyAction, bodyOK := parseHumanDecisionMarker(it.Body)

	// Newest comment first: a decision in a comment is newer than the body and
	// wins, matching "latest signal wins". Otherwise fall back to the body.
	target := fmt.Sprintf("repos/%s/%s/issues/%d/comments", in.Owner, in.Repo, in.Num)
	out, err := ghRunner(ctx, "api", target, "--paginate")
	if err != nil {
		return HumanDecision{}, fmt.Errorf("list comments %s: %s: %w", target, strings.TrimSpace(string(out)), err)
	}
	var comments []struct {
		Body string `json:"body"`
	}
	if err := json.Unmarshal(out, &comments); err != nil {
		return HumanDecision{}, fmt.Errorf("parse comments %s: %w", target, err)
	}
	for i := len(comments) - 1; i >= 0; i-- {
		if a, ok := parseHumanDecisionMarker(comments[i].Body); ok {
			return humanDecisionFromBody(a, comments[i].Body)
		}
	}
	if bodyOK {
		return humanDecisionFromBody(bodyAction, it.Body)
	}
	return HumanDecision{}, nil
}

// humanDecisionFromBody validates the action and attaches the #108 payload
// (feedback + diff-anchored comments) when present.
func humanDecisionFromBody(action, body string) (HumanDecision, error) {
	if err := ValidateHumanDecision(action); err != nil {
		return HumanDecision{}, err
	}
	d := HumanDecision{Action: action, Present: true}
	if p, ok := parseDecisionPayload(body); ok {
		d.Feedback = p.Feedback
		d.Comments = p.Comments
	}
	return d, nil
}
