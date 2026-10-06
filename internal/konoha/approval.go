package konoha

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// §55 pre-execution approval gate + audit. High-assurance repos can require a
// human sign-off BEFORE the pipeline performs a mutating step (publish / merge /
// deploy). When gated, the workflow enters PENDING_APPROVAL, then reads the
// existing §18 decision channel (the byte-exact `<!-- konoha-decision: ... -->`
// marker, so the parser and validation are reused, never forked): `approve`
// proceeds, `deny` routes to HUMAN_REQUIRED, and a decision that never arrives
// blocks at the poGateTimeout (§40 — a timeout is never a silent proceed). This
// is an OPT-IN pre-gate distinct from the post-hoc HUMAN_REQUIRED exception
// path; with `approval.preGate` off (the default) the pipeline is unchanged.
//
// Every gated decision is written to the append-only audit sink (append-only
// JSONL file) by RecordAuditEvent. Temporal history remains the authoritative
// lifecycle record (§44 #1); the audit file is the durable, exportable surface.

// §55 gated steps (closed set; a typo in repos.yaml fails at load).
const (
	approvalStepPublish = "publish"
	approvalStepMerge   = "merge"
	approvalStepDeploy  = "deploy"
)

var approvalSteps = []string{approvalStepPublish, approvalStepMerge, approvalStepDeploy}

// ValidApprovalStep reports whether step is a supported gated step.
func ValidApprovalStep(step string) bool {
	switch step {
	case approvalStepPublish, approvalStepMerge, approvalStepDeploy:
		return true
	}
	return false
}

// ApprovalConfig is the per-repo §55 pre-execution approval policy (repos.yaml
// `lifecycle.approval`).
type ApprovalConfig struct {
	PreGate       bool     `yaml:"preGate" json:"preGate,omitempty"`
	RequiredSteps []string `yaml:"requiredSteps" json:"requiredSteps,omitempty"`
}

// AuditConfig is the per-repo §55 audit policy (repos.yaml `lifecycle.audit`).
// Sink is an append-only file path; the activity appends one JSON line per
// event. Enabled without a sink fails at load, and enabling the pre-gate
// without an enabled audit also fails at load (a gated decision that is not
// recorded breaks the §55 guarantee).
type AuditConfig struct {
	Enabled bool   `yaml:"enabled" json:"enabled,omitempty"`
	Sink    string `yaml:"sink" json:"sink,omitempty"`
}

// GatedSteps returns the effective gated steps: the configured subset, or every
// step when preGate is on with an empty list (fail-closed default: an enabled
// gate must gate something). NONE when preGate is off — the default, which
// leaves the pipeline fully autonomous.
func GatedSteps(lc LifecycleConfig) []string {
	if !lc.Approval.PreGate {
		return nil
	}
	if len(lc.Approval.RequiredSteps) == 0 {
		return append([]string(nil), approvalSteps...)
	}
	return lc.Approval.RequiredSteps
}

// ApprovalRequired reports whether the given mutating step must wait for a
// human approval decision (§55).
func ApprovalRequired(lc LifecycleConfig, step string) bool {
	return containsString(GatedSteps(lc), step)
}

// validate gates the §55 approval block: every listed step is in the closed set
// (a typo must fail `./scripts/apply.sh`, not silently gate nothing).
func (ac ApprovalConfig) validate(prefix string) error {
	for _, s := range ac.RequiredSteps {
		if !ValidApprovalStep(s) {
			return fmt.Errorf("%s.requiredSteps: %q not one of %q, %q, %q",
				prefix, s, approvalStepPublish, approvalStepMerge, approvalStepDeploy)
		}
	}
	return nil
}

// validate gates the §55 audit block: an enabled audit needs a sink file, and
// the path must be a plain file path.
func (ac AuditConfig) validate(prefix string) error {
	if !ac.Enabled {
		return nil
	}
	if strings.TrimSpace(ac.Sink) == "" {
		return fmt.Errorf("%s.sink: required when audit.enabled (append-only log file path)", prefix)
	}
	if strings.ContainsRune(ac.Sink, '\x00') {
		return fmt.Errorf("%s.sink: %q contains a NUL byte", prefix, ac.Sink)
	}
	return nil
}

// ApprovalRequest parameterizes AwaitApproval. Timeout is a Go duration string;
// empty falls back to the resolved poGateTimeout.
type ApprovalRequest struct {
	Owner   string `json:"owner"`
	Repo    string `json:"repo"`
	Num     int    `json:"num"`
	Step    string `json:"step"`
	Timeout string `json:"timeout,omitempty"`
	DryRun  bool   `json:"dryRun,omitempty"`
}

// ApprovalResult is the §55 gate verdict. Exactly one of Granted/Denied/Rework/
// TimedOut is set (Skipped covers DryRun, where there is no mutation to gate).
// Action is the raw human decision that produced the verdict, for the audit
// record; Feedback/Comments carry a request-changes decision (§108) back to the
// workflow so it can re-enter the working path with the human's instructions.
type ApprovalResult struct {
	Granted  bool          `json:"granted,omitempty"`
	Denied   bool          `json:"denied,omitempty"`
	Rework   bool          `json:"rework,omitempty"`
	TimedOut bool          `json:"timedOut,omitempty"`
	Skipped  bool          `json:"skipped,omitempty"`
	Action   string        `json:"action,omitempty"`
	Detail   string        `json:"detail,omitempty"`
	Feedback string        `json:"feedback,omitempty"`
	Comments []DiffComment `json:"comments,omitempty"`
	Attempts int           `json:"attempts,omitempty"`
}

// approvalVerdict is the pure gate policy's output.
type approvalVerdict int

const (
	approvalPending approvalVerdict = iota
	approvalGranted
	approvalDenied
	approvalRework
)

// EvaluateApproval maps a read §18 human decision to the gate verdict. Only
// `approve` grants (the `resume` action is the older "continue" synonym and also
// grants); `request-changes` returns the work to the working path (§108); every
// other decision — including `deny`, `block`, `cancel`, `close` — denies, so a
// valid-but-inapplicable signal routes to HUMAN_REQUIRED rather than silently
// proceeding. A malformed marker never reaches here: it already fails closed in
// ReadHumanDecision (§36).
func EvaluateApproval(d HumanDecision) approvalVerdict {
	if !d.Present {
		return approvalPending
	}
	switch d.Action {
	case humanDecisionApprove, humanDecisionResume:
		return approvalGranted
	case humanDecisionRequestChanges:
		return approvalRework
	default:
		return approvalDenied
	}
}

// approvalPollInterval is the gap between decision-channel reads. A package var
// so unit tests can shrink it (like the ghRunner seam) without waiting minutes.
var approvalPollInterval = time.Minute

// AwaitApproval is the §55 gate activity: it polls the issue's decision channel
// until an approval/denial appears or the bounded timeout elapses. It reuses
// ReadHumanDecision verbatim (same marker, same validation). A timeout returns
// TimedOut (no error): the caller must block, never proceed (§40). Read-only
// until the decision exists; it never mutates GitHub.
//
// ponytail: one long-poll activity bounded by poGateTimeout (mirrors WaitForCI);
// it does not heartbeat, so a worker restart mid-wait retries after
// StartToClose — add heartbeats if multi-day waits become common.
func AwaitApproval(ctx context.Context, in ApprovalRequest) (ApprovalResult, error) {
	if in.DryRun {
		// §42 shadow mode mutates nothing, so there is nothing to gate.
		return ApprovalResult{Skipped: true, Detail: "dryRun"}, nil
	}
	if err := ValidateApprovalStep(in.Step); err != nil {
		return ApprovalResult{}, err
	}
	timeout := POGateTimeout(LifecycleConfig{POGateTimeout: in.Timeout})
	deadline := time.Now().Add(timeout)
	attempts := 0
	for {
		attempts++
		d, err := ReadHumanDecision(ctx, HumanDecisionInput{Owner: in.Owner, Repo: in.Repo, Num: in.Num})
		if err != nil {
			return ApprovalResult{}, err // malformed signal → fail closed (§36)
		}
		switch EvaluateApproval(d) {
		case approvalGranted:
			return ApprovalResult{Granted: true, Action: d.Action, Attempts: attempts}, nil
		case approvalRework:
			return ApprovalResult{Rework: true, Action: d.Action,
				Detail: d.Feedback, Feedback: d.Feedback, Comments: d.Comments, Attempts: attempts}, nil
		case approvalDenied:
			return ApprovalResult{Denied: true, Action: d.Action,
				Detail: "decision " + d.Action + " is not an approval signal", Attempts: attempts}, nil
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return ApprovalResult{TimedOut: true,
				Detail: "no approval decision within " + timeout.String(), Attempts: attempts}, nil
		}
		if remaining > approvalPollInterval {
			remaining = approvalPollInterval
		}
		select {
		case <-ctx.Done():
			return ApprovalResult{}, ctx.Err()
		case <-time.After(remaining):
		}
	}
}

// ValidateApprovalStep fails closed on an unknown gated step (§36): an
// un-validated step could otherwise never be gated or silently mistyped.
func ValidateApprovalStep(step string) error {
	if !ValidApprovalStep(step) {
		return fmt.Errorf("approval step %q not in the §55 set {publish, merge, deploy}", step)
	}
	return nil
}

// AuditEvent is one append-only §55 audit record: which step, who acted, and the
// decision. Owner/Repo/Issue identify the subject; Time is stamped by the
// activity (I/O), never the workflow.
type AuditEvent struct {
	Owner    string `json:"owner,omitempty"`
	Repo     string `json:"repo"`
	Issue    int    `json:"issue,omitempty"`
	Step     string `json:"step"`
	Actor    string `json:"actor"`    // human | policy
	Decision string `json:"decision"` // allow | deny | timeout
	Detail   string `json:"detail,omitempty"`
}

// AuditRequest pairs an event with the repo's audit policy.
type AuditRequest struct {
	Event  AuditEvent  `json:"event"`
	Config AuditConfig `json:"config"`
}

// RecordAuditEvent appends one JSON line to the configured sink. Disabled config
// is a no-op. A write failure (or an enabled config with no sink) returns an
// error so the workflow fails closed — an un-recorded gated decision breaks the
// §55 guarantee, so it is never a silent success. Deliberately calls no logger:
// activity.GetLogger panics on the non-Temporal contexts unit tests use.
func RecordAuditEvent(ctx context.Context, in AuditRequest) error {
	if !in.Config.Enabled {
		return nil
	}
	rec := struct {
		Time string `json:"time"`
		AuditEvent
	}{Time: time.Now().UTC().Format(time.RFC3339Nano), AuditEvent: in.Event}
	line, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("audit marshal: %w", err)
	}
	sink := strings.TrimSpace(in.Config.Sink)
	if sink == "" {
		// Config validation forbids this, but a direct struct must still fail
		// closed: an enabled audit with nowhere to record is not a silent pass.
		return fmt.Errorf("audit.enabled but sink is empty")
	}
	// ponytail: O_APPEND line write, at-least-once under activity retry; add a
	// dedupe key if the consumer ever needs exactly-once audit lines.
	f, err := os.OpenFile(sink, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("audit sink %s: %w", sink, err)
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("audit write %s: %w", sink, err)
	}
	return nil
}
