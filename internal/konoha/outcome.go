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

// terminalInputs carries the per-run facts the terminal assembler fills into a
// CycleOutcome. Shared by the Temporal workflow and the local runtime (#212).
type terminalInputs struct {
	AgentOutcome        AgentOutcome
	VerificationOutcome VerificationOutcome
	Usage               HarnessUsage
	Limits              *LimitEvidence
	VerificationMutated bool
	EvidenceError       string
	Cleanup             *CleanupResult
	Elapsed             time.Duration
	Resources           *ResourceEvidence
}

// assembleTerminal fills the runtime-neutral terminal evidence on an outcome:
// defaulted agent/verification outcomes, usage, drift, limits, advisory egress,
// duration, resolved resources/budget crossings, and the cleanup record. Pure
// given its inputs (appCfg is fixed at bootstrap), so both runtimes record the
// same outcome.json shape.
func assembleTerminal(in IssueInput, out CycleOutcome, ti terminalInputs) CycleOutcome {
	if out.AgentOutcome == "" {
		out.AgentOutcome = ti.AgentOutcome
	}
	if out.VerificationOutcome == "" {
		out.VerificationOutcome = ti.VerificationOutcome
	}
	if out.Usage.IsZero() {
		out.Usage = ti.Usage
	}
	// #94: carry the latest attempt's drift evidence (never state-changing).
	out.VerificationMutated = out.VerificationMutated || ti.VerificationMutated
	if out.EvidenceError == "" {
		out.EvidenceError = ti.EvidenceError
	}
	// #102: record the applied ceilings + enforcement status. A result that
	// observed a crossed ceiling wins; otherwise the effective configured limits
	// are recorded (evidence only, never state-changing).
	if out.Limits == nil {
		if ti.Limits != nil {
			out.Limits = ti.Limits
		} else {
			out.Limits = limitsEvidence(EffectiveLimits(in.Lifecycle))
		}
	}
	// #103: record the advisory egress policy (or its absence) as evidence.
	if out.Egress == nil {
		out.Egress = egressEvidence(appCfg.Egress)
	}
	// §98: record the resolved catalog + budget crossings as evidence. A crossing
	// NEVER rewrites the verdict/state or shortens a timeout (§40).
	if out.DurationSeconds == 0 {
		out.DurationSeconds = ti.Elapsed.Seconds()
	}
	if ti.Resources != nil {
		res := *ti.Resources
		if res.Budget != nil {
			b := *res.Budget
			b.Crossings = budgetCrossings(res.Budget, out.Usage, ti.Elapsed)
			res.Budget = &b
		}
		out.Resources = &res
	}
	out.Cleanup = ti.Cleanup
	return out
}

// prepareTerminalOutcome applies the runtime-neutral record fields on every
// terminal: explicit outcomes + ordered conditions, the §42 dryRun marker, the
// #88 policy digest, the #104 scope, #117 profiles/tooling, and the #122 URL
// policy + secret NAMES (never values).
func prepareTerminalOutcome(in IssueInput, out CycleOutcome, now time.Time) CycleOutcome {
	// #84: fill the explicit three-outcome evidence (defaults to SKIPPED, never
	// silently absent §40) and stamp the ordered conditions deterministically.
	out = stampOutcome(out, now)
	out.DryRun = in.Lifecycle.DryRun // §42: evidence paths resolve from the record, not live config
	// #88: pin the effective policy + resolved harness/model + task identity.
	if out.PolicyDigest == "" {
		out.PolicyDigest = policyDigest(in.Lifecycle, taskHash(in.Owner, in.Repo, in.IssueNum))
	}
	// #104: record the load-resolved tenancy scope alongside the policy.
	if out.Scope == "" {
		out.Scope = in.Lifecycle.Scope
	}
	// #117: record the load-resolved agent profiles (stage → name) + digest.
	stampProfiles(&out, in.Lifecycle)
	// #122: record the URL policy and referenced secret NAMES (never values).
	if out.URLPolicy == nil {
		out.URLPolicy = urlPolicyEvidence(EffectiveURLPolicy(in.Lifecycle))
	}
	if len(out.SecretNames) == 0 {
		out.SecretNames = OutboundSecretNames(in.Lifecycle)
	}
	return out
}
