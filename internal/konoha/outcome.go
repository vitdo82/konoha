package konoha

import "time"

// Explicit three-outcome evidence record (#84, milestone v4.0-evidence). The
// terminal §6 state stays authoritative (§44 #20); these outcomes are
// descriptive evidence that make "which step failed" answerable from
// outcome.json instead of Temporal history.
//
// The canonical case the record must never collapse:
//
//	agent_result        = SUCCESS
//	verification_result = FAILED
//	terminal_state      = HUMAN_REQUIRED
//
// A BLOCKED agent is an agent-REPORTED "cannot proceed / needs input" outcome,
// distinct from a process failure (non-zero exit): the workflow routes it to the
// human path, never the repair loop (§18/§40).

// AgentOutcome is the outcome of one agent (develop/repair) execution.
type AgentOutcome string

const (
	agentOutcomeSuccess  AgentOutcome = "SUCCESS"
	agentOutcomeFailed   AgentOutcome = "FAILED"
	agentOutcomeTimedOut AgentOutcome = "TIMED_OUT"
	agentOutcomeSkipped  AgentOutcome = "SKIPPED"
	agentOutcomeBlocked  AgentOutcome = "BLOCKED"
)

// VerificationOutcome is the outcome of the §26 verification run for an attempt.
type VerificationOutcome string

const (
	verificationSuccess VerificationOutcome = "SUCCESS"
	verificationFailed  VerificationOutcome = "FAILED"
	verificationSkipped VerificationOutcome = "SKIPPED"
	verificationError   VerificationOutcome = "ERROR"
)

// Condition types for the ordered per-step condition list in outcome.json.
const (
	conditionAgent        = "agent"
	conditionVerification = "verification"
	conditionTerminal     = "terminal"
)

// Condition is one ordered per-step condition: what was checked, its status,
// and (when relevant) the reason/message that produced it.
type Condition struct {
	Type    string    `json:"type"`
	Status  string    `json:"status"`
	Reason  string    `json:"reason,omitempty"`
	Message string    `json:"message,omitempty"`
	At      time.Time `json:"at"`
}

// agentOutcomeOf projects a returned develop/repair result to its agent outcome.
// A blocked report is an agent outcome, not a failure: the agent ran to
// completion and reported it cannot proceed (a process failure instead surfaces
// as a returned error with no DevelopResult).
func agentOutcomeOf(res DevelopResult) AgentOutcome {
	if res.Blocked {
		return agentOutcomeBlocked
	}
	return agentOutcomeSuccess
}

// verificationOutcomeOf projects the §26 validation result. A blocked agent (or
// one that never ran validation) is SKIPPED, never silently absent (§40).
func verificationOutcomeOf(res DevelopResult) VerificationOutcome {
	if res.Blocked || res.ValidationSkipped {
		return verificationSkipped
	}
	if res.Validated {
		return verificationSuccess
	}
	return verificationFailed
}

// stampOutcome fills the explicit three-outcome evidence on a terminal outcome:
// a missing agent/verification outcome defaults to SKIPPED (never silently
// absent, §40) and the ordered agent → verification → terminal condition list is
// attached. Pure except for the caller-supplied deterministic timestamp, so it
// is safe to call from workflow code.
func stampOutcome(out CycleOutcome, now time.Time) CycleOutcome {
	if out.AgentOutcome == "" {
		out.AgentOutcome = agentOutcomeSkipped
	}
	if out.VerificationOutcome == "" {
		out.VerificationOutcome = verificationSkipped
	}
	if len(out.Conditions) == 0 {
		out.Conditions = []Condition{
			{Type: conditionAgent, Status: string(out.AgentOutcome), Reason: out.EscalationReason, Message: out.AgentMessage, At: now},
			{Type: conditionVerification, Status: string(out.VerificationOutcome), At: now},
			{Type: conditionTerminal, Status: out.TerminalState, Reason: out.EscalationReason, At: now},
		}
	}
	return out
}
