package konoha

import (
	"encoding/json"
	"strings"
	"time"
)

// Deterministic "status" query (#89): a read-only snapshot of a long-lived
// cycle so a watcher sees progress without reading Temporal history. Additive to
// the unchanged "state" query handler. The snapshot is assembled entirely in
// workflow code (never an activity), so replay reconstructs it identically.

// stateReviewed is the PR-review terminal state (IssueCycleWorkflow uses the §6
// lifecycle states; PR review is its own shorter pipeline).
const stateReviewed = "REVIEWED"

// StatusQuery is the snapshot returned by the "status" query handler:
// {state, currentStep, conditions, agentOutcome, verificationOutcome}.
type StatusQuery struct {
	State               string              `json:"state"`
	CurrentStep         string              `json:"currentStep,omitempty"`
	Conditions          []Condition         `json:"conditions"`
	AgentOutcome        AgentOutcome        `json:"agentOutcome,omitempty"`
	VerificationOutcome VerificationOutcome `json:"verificationOutcome,omitempty"`
}

// statusStep maps a §6 state to the coarse step a watcher sees in flight. The
// state stays the authoritative §6 value (§44 #20); the step is the normalized
// operation the state represents.
func statusStep(state string) string {
	switch state {
	case stateDiscovered:
		return "discover"
	case stateAnalyzing:
		return "analyze"
	case stateEstimating:
		return "estimate"
	case stateSmall, stateLarge:
		return "route"
	case stateDeveloping:
		return "develop"
	case stateChangesRequested:
		return "repair"
	case stateSplitting:
		return "split"
	case stateReviewing:
		return "review"
	case stateMerging:
		return "merge"
	case statePendingApproval:
		return "approval"
	case stateClosed:
		return "close"
	default:
		return strings.ToLower(state)
	}
}

// statusTracker accumulates the status snapshot inside workflow code. Every
// method is pure (the timestamp is caller-supplied and deterministic) and never
// does I/O or mutates anything outside the tracker, so replay is identical.
type statusTracker struct {
	q StatusQuery
}

func newStatusTracker(state string) *statusTracker {
	t := &statusTracker{}
	t.setState(state)
	return t
}

// setState updates the §6 state and derives its coarse step.
func (t *statusTracker) setState(state string) {
	t.q.State = state
	t.q.CurrentStep = statusStep(state)
}

// setStep overrides the coarse step for a finer-grained activity within a state.
func (t *statusTracker) setStep(step string) { t.q.CurrentStep = step }

// recordAgent appends one agent step condition (one per develop/repair attempt).
func (t *statusTracker) recordAgent(out AgentOutcome, reason, msg string, at time.Time) {
	t.q.AgentOutcome = out
	t.q.Conditions = append(t.q.Conditions, Condition{
		Type: conditionAgent, Status: string(out), Reason: reason, Message: msg, At: at,
	})
}

// recordVerification appends one verification step condition.
func (t *statusTracker) recordVerification(out VerificationOutcome, at time.Time) {
	t.q.VerificationOutcome = out
	t.q.Conditions = append(t.q.Conditions, Condition{
		Type: conditionVerification, Status: string(out), At: at,
	})
}

// recordTerminal sets the terminal state and appends the terminal condition.
func (t *statusTracker) recordTerminal(state, reason string, at time.Time) {
	t.setState(state)
	t.q.Conditions = append(t.q.Conditions, Condition{
		Type: conditionTerminal, Status: state, Reason: reason, At: at,
	})
}

// snapshot returns a copy so the query result never aliases workflow state. An
// empty condition list is normalized to [] (never a null JSON array).
func (t *statusTracker) snapshot() StatusQuery {
	s := t.q
	s.Conditions = append([]Condition{}, t.q.Conditions...)
	return s
}

// agentConditionReason is the condition reason for a develop/repair result: a
// blocked report carries the §18 agent-blocked reason, a normal run has none.
func agentConditionReason(res DevelopResult) string {
	if res.Blocked {
		return reasonAgentBlocked
	}
	return ""
}

// statusJSON is the pure projection from a status snapshot to printable JSON
// (used by `prbot status` and unit-tested without a Temporal server).
func statusJSON(st StatusQuery) (string, error) {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b), nil
}
