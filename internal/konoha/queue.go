package konoha

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"
)

// §109 needs-attention queue + recovery actions (Machinist recovery model).
//
// The queue aggregates every cycle waiting on a human: awaiting approval,
// blocked, failed, and interrupted. Recovery rules are strict:
//   - retry preserves prior attempts + history and is a NEW attempt, never a
//     silent replay of external effects;
//   - cancel reflects the stopped state before a retry is allowed;
//   - an interrupted cycle (its worker/lease was lost) is NEVER auto-queued for
//     retry — the operator must explicitly confirm the old process stopped;
//   - every state-changing action goes through the authoritative channel
//     (Temporal lifecycle for retry/cancel; the §18 channel for decisions) and
//     is audited (#55) and idempotent (§24).
//
// This file is the pure projection + action policy; the console layer resolves
// the live cycle facts and supplies the Temporal seams. attach/pause/resume are
// session verbs owned by #136/#144; with no session provider they are reported
// SKIPPED (never a false success, §40) rather than silently pretended.

// Why a cycle is in the needs-attention queue.
const (
	queueReasonAwaitingApproval = "awaiting_approval"
	queueReasonBlocked          = "blocked"
	queueReasonFailed           = "failed"
	queueReasonHumanRequired    = "human_required"
	queueReasonInterrupted      = "interrupted"
)

// Normalized Temporal execution statuses (the authoritative "is it running /
// stopped" fact). Kept separate from the §6 lifecycle states.
const (
	execStatusRunning    = "RUNNING"
	execStatusCompleted  = "COMPLETED"
	execStatusCanceled   = "CANCELED"
	execStatusFailed     = "FAILED"
	execStatusTimedOut   = "TIMED_OUT"
	execStatusTerminated = "TERMINATED"
)

// Queue actions. retry/cancel are owned here; attach/pause/resume are the
// session verbs (#136/#144) surfaced (disabled) in the same queue.
const (
	queueActionRetry  = "retry"
	queueActionCancel = "cancel"
	queueActionAttach = "attach"
	queueActionPause  = "pause"
	queueActionResume = "resume"
)

// Queue action outcomes. APPLIED = performed; SKIPPED = not supported (never a
// false success §40); PREVIEW = a §42 dry-run, no mutation; NOOP = idempotent
// repeat (e.g. cancel of an already-stopped cycle).
const (
	queueOutcomeApplied = "APPLIED"
	queueOutcomeSkipped = "SKIPPED"
	queueOutcomePreview = "PREVIEW"
	queueOutcomeNoop    = "NOOP"
)

// §55 audit steps for the queue's authoritative actions.
const (
	auditStepQueueRetry  = "queue-retry"
	auditStepQueueCancel = "queue-cancel"
)

// QueueActionView is one action offered for a queue item, with an explicit
// enabled flag + reason so the UI can render disabled/preview state (§42).
type QueueActionView struct {
	Action  string `json:"action"`
	Enabled bool   `json:"enabled"`
	Reason  string `json:"reason,omitempty"`
}

// QueueItem is one needs-attention entry. RequiresConfirmation is the
// interrupted gate: retry is refused until the operator confirms the old
// process stopped.
type QueueItem struct {
	ID                   string            `json:"id"`
	Owner                string            `json:"owner,omitempty"`
	Repo                 string            `json:"repo,omitempty"`
	Issue                int               `json:"issue,omitempty"`
	State                string            `json:"state"`
	Reason               string            `json:"reason"`
	Running              bool              `json:"running,omitempty"`
	Interrupted          bool              `json:"interrupted,omitempty"`
	Attempt              int               `json:"attempt,omitempty"`
	RequiresConfirmation bool              `json:"requiresConfirmation,omitempty"`
	DryRun               bool              `json:"dryRun,omitempty"`
	Evidence             string            `json:"evidence,omitempty"` // #141: store-relative evidence dir (empty = none recorded yet)
	Actions              []QueueActionView `json:"actions"`
	At                   time.Time         `json:"at,omitempty"`
}

// QueueView is the GET /api/v1/queue payload.
type QueueView struct {
	Items []QueueItem `json:"items"`
}

// QueueActionRequest is one mutating queue action submission. The server fills
// Owner/Repo/Issue/CycleID from the resolved item; the browser supplies only the
// action (and the confirmation / dry-run flags).
type QueueActionRequest struct {
	CycleID        string `json:"cycleId,omitempty"`
	Owner          string `json:"owner,omitempty"`
	Repo           string `json:"repo,omitempty"`
	Issue          int    `json:"issue,omitempty"`
	Attempt        int    `json:"attempt,omitempty"`
	Action         string `json:"action"`
	ConfirmStopped bool   `json:"confirmStopped,omitempty"`
	Actor          string `json:"actor,omitempty"`
	DryRun         bool   `json:"dryRun,omitempty"`
}

// QueueActionResult reports the outcome of one action.
type QueueActionResult struct {
	Accepted bool   `json:"accepted"`
	Outcome  string `json:"outcome"`
	Action   string `json:"action,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

// queueCycle is the neutral per-cycle input to the pure projection. The console
// resolves it from the evidence store + Temporal; the pure policy never does I/O.
type queueCycle struct {
	ID             string
	Owner          string
	Repo           string
	Issue          int
	State          string
	HasOutcome     bool
	Running        bool
	Interrupted    bool
	Canceled       bool
	Attempt        int
	DryRun         bool
	RecordedDryRun bool // #141: the run's own §42 flag (evidence namespace)
	At             time.Time
}

// ValidateQueueAction is the fail-closed §36 membership gate for a queue action.
func ValidateQueueAction(a string) error {
	switch a {
	case queueActionRetry, queueActionCancel, queueActionAttach, queueActionPause, queueActionResume:
		return nil
	}
	return fmt.Errorf("queue action %q not in the set {retry, cancel, attach, pause, resume}", a)
}

// isSessionVerb reports whether an action is a session lifecycle verb
// (#136/#144) rather than a retry/cancel recovery action.
func isSessionVerb(a string) bool {
	switch a {
	case queueActionAttach, queueActionPause, queueActionResume:
		return true
	}
	return false
}

// attentionReason classifies why a cycle needs a human, or ok=false when it does
// not need attention (running normally, closed, canceled, or already stopped).
// Pure.
func attentionReason(c queueCycle) (string, bool) {
	if c.Canceled {
		return "", false
	}
	if c.Interrupted {
		return queueReasonInterrupted, true
	}
	if c.Running {
		switch c.State {
		case statePendingApproval:
			return queueReasonAwaitingApproval, true
		case stateHumanRequired:
			// A human-required state observed while still running (e.g. a gate
			// held open) is attention; the terminal form is handled below.
			return queueReasonHumanRequired, true
		}
		return "", false
	}
	if c.HasOutcome {
		switch c.State {
		case stateBlocked:
			return queueReasonBlocked, true
		case stateFailed:
			return queueReasonFailed, true
		case stateHumanRequired:
			return queueReasonHumanRequired, true
		}
		return "", false
	}
	// No recorded outcome and not running: the worker/lease was lost. Never
	// auto-queued for retry — surfaced explicitly as interrupted.
	return queueReasonInterrupted, true
}

// queueActionsFor lists the actions offered for a reason, with enabled/reason
// flags. retry applies to terminal/interrupted cycles; cancel to a running one;
// the session verbs are disabled without a provider (#136/#144).
func queueActionsFor(reason string, running, sessionAvailable bool) []QueueActionView {
	retryable := reason == queueReasonBlocked || reason == queueReasonFailed ||
		reason == queueReasonHumanRequired || reason == queueReasonInterrupted
	out := []QueueActionView{
		{Action: queueActionRetry, Enabled: retryable && !running},
		{Action: queueActionCancel, Enabled: running},
	}
	for _, a := range []string{queueActionAttach, queueActionPause, queueActionResume} {
		v := QueueActionView{Action: a, Enabled: sessionAvailable}
		if !sessionAvailable {
			v.Reason = "no live session provider (#136/#144)"
		}
		out = append(out, v)
	}
	return out
}

// cycleEvidencePointer is the store-relative evidence directory for a cycle
// (#141), emitted only when an outcome was recorded — the projection never
// guesses a path for a cycle that may not have written anything. Honors the
// §42 dry-run namespace.
func cycleEvidencePointer(c queueCycle) string {
	if !c.HasOutcome {
		return ""
	}
	if c.RecordedDryRun {
		return "dryrun/cycles/" + c.ID
	}
	return "cycles/" + c.ID
}

// queueItemFromCycle projects one classified cycle to a queue item. Pure.
func queueItemFromCycle(c queueCycle, sessionAvailable bool) QueueItem {
	reason, _ := attentionReason(c)
	return QueueItem{
		ID: c.ID, Owner: c.Owner, Repo: c.Repo, Issue: c.Issue,
		State: c.State, Reason: reason, Running: c.Running, Interrupted: c.Interrupted,
		Attempt: c.Attempt, RequiresConfirmation: reason == queueReasonInterrupted,
		DryRun: c.DryRun, Evidence: cycleEvidencePointer(c), Actions: queueActionsFor(reason, c.Running, sessionAvailable), At: c.At,
	}
}

// attentionItems is the pure projection: cycles needing a human, newest first
// (by the latest condition timestamp, ties broken by descending id) so the
// ordering is deterministic.
func attentionItems(cycles []queueCycle, sessionAvailable bool) []QueueItem {
	out := make([]QueueItem, 0, len(cycles))
	for _, c := range cycles {
		if _, ok := attentionReason(c); !ok {
			continue
		}
		out = append(out, queueItemFromCycle(c, sessionAvailable))
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].At.Equal(out[j].At) {
			return out[i].At.After(out[j].At)
		}
		return out[i].ID > out[j].ID
	})
	return out
}

// queueCycleFor resolves one cycle's live queue facts: the terminal outcome
// (if any), whether Temporal still reports it running (and whether the status
// query answers — a running cycle whose worker is gone is interrupted), and
// whether it was canceled. Best-effort; a status/probe miss is an interrupted
// signal, never a fabricated success.
func (d *consoleData) queueCycleFor(ctx context.Context, id string) (queueCycle, bool, error) {
	repo, issue, ok := parseCycleID(id)
	if !ok {
		return queueCycle{}, false, nil
	}
	c := queueCycle{ID: id, Repo: repo, Issue: issue}
	if d.store != nil {
		if o, has, err := d.store.loadOutcomeOptional(id); err != nil {
			return queueCycle{}, false, err
		} else if has {
			c.HasOutcome = true
			c.State = o.TerminalState
			// #141: the evidence namespace comes from the RUN's record, so flipping
			// dryRun later never repoints a finished cycle at a missing directory.
			c.RecordedDryRun = o.DryRun
			if o.Owner != "" {
				c.Owner = o.Owner
			}
			if o.Repo != "" {
				c.Repo = o.Repo
			}
			if o.Issue != 0 {
				c.Issue = o.Issue
			}
			c.At = latestConditionTime(o.Conditions)
		}
	}
	if d.running != nil {
		if rs, listed := d.running(ctx); listed {
			for _, r := range rs {
				if r.ID != id {
					continue
				}
				c.Running = true
				if r.Repo != "" {
					c.Repo = r.Repo
				}
				if r.Issue != 0 {
					c.Issue = r.Issue
				}
				break
			}
		}
	}
	switch {
	case c.Running:
		// A running cycle the status query cannot answer has lost its worker/
		// lease: explicitly interrupted, never silently treated as healthy.
		if d.status == nil {
			c.Interrupted = true
			c.Running = false
		} else if st, ok := d.status(ctx, id); ok {
			c.State = st.State
			c.At = latestConditionTime(st.Conditions)
		} else {
			c.Interrupted = true
			c.Running = false
		}
	case !c.HasOutcome:
		// No recorded outcome and not in the running list: consult the
		// authoritative execution status (canceled → stopped; running → a list
		// miss). A cycle with no status and no evidence is unknown, not
		// interrupted — never fabricate an interrupted cycle out of a typo.
		s, probed := "", false
		if d.execStatus != nil {
			s, probed = d.execStatus(ctx, id)
		}
		switch {
		case probed && s == execStatusCanceled:
			c.Canceled = true
		case probed && s == execStatusRunning:
			c.Running = true
			answered := false
			if d.status != nil {
				if st, ok := d.status(ctx, id); ok {
					c.State = st.State
					c.At = latestConditionTime(st.Conditions)
					answered = true
				}
			}
			if !answered {
				// Temporal reports running but no worker answers the status
				// query: the worker/lease was lost.
				c.Running = false
				c.Interrupted = true
			}
		case d.store != nil && d.store.cycleExists(id):
			c.Interrupted = true
		default:
			return queueCycle{}, false, nil
		}
	}
	c.DryRun = d.repoDryRun(c.Repo) // action gating uses today's policy
	if d.store != nil && c.Owner != "" {
		if l, err := d.store.loadLineage(c.Owner, c.Repo, c.Issue, false); err == nil {
			c.Attempt, _ = currentAttemptSHA(l.Activations)
		}
	}
	return c, true, nil
}

// repoDryRun reports the repo's §42 dry-run policy (resolved by name at load).
func (d *consoleData) repoDryRun(repo string) bool {
	if rc, ok := d.repos[repo]; ok && rc.Lifecycle != nil {
		return rc.Lifecycle.DryRun
	}
	return false
}

// listQueue builds the needs-attention queue from the evidence store + the
// (best-effort) Temporal running/status/execution probes.
func (d *consoleData) listQueue(ctx context.Context) (QueueView, error) {
	if d.store == nil {
		return QueueView{Items: []QueueItem{}}, nil
	}
	ids, err := d.store.listCycleIDs()
	if err != nil {
		return QueueView{}, err
	}
	// A running cycle may not have written evidence yet; include the running ids
	// so an early awaiting-approval/blocked cycle is never missed.
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		seen[id] = true
	}
	if d.running != nil {
		if rs, ok := d.running(ctx); ok {
			for _, r := range rs {
				if r.ID != "" && !seen[r.ID] {
					seen[r.ID] = true
					ids = append(ids, r.ID)
				}
			}
		}
	}
	cycles := make([]queueCycle, 0, len(ids))
	for _, id := range ids {
		// ponytail: one running/status probe per evidence cycle (O(n) probes);
		// cache the running set once if the console ever holds thousands.
		c, ok, err := d.queueCycleFor(ctx, id)
		if err != nil {
			return QueueView{}, err
		}
		if ok {
			cycles = append(cycles, c)
		}
	}
	return QueueView{Items: attentionItems(cycles, false)}, nil
}

// submitQueueAction resolves the live item, enforces the recovery guards, and
// delegates the mutation to the authoritative writer. A repeated/terminal action
// is a NOOP, never a second mutation (§24); attach/pause/resume report SKIPPED
// without a session provider (§40); a §42 dry-run is a PREVIEW with no mutation.
func (d *consoleData) submitQueueAction(ctx context.Context, id string, in QueueActionRequest) (QueueActionResult, error) {
	if d.queue == nil {
		return QueueActionResult{}, errQueueDisabled
	}
	c, ok, err := d.queueCycleFor(ctx, id)
	if err != nil {
		return QueueActionResult{}, err
	}
	if !ok {
		return QueueActionResult{}, errUnknownCycle
	}
	if err := ValidateQueueAction(in.Action); err != nil {
		return QueueActionResult{}, fmt.Errorf("%w: %v", errBadQueueAction, err)
	}
	// Session verbs (#136/#144) have no provider wired here: report SKIPPED,
	// never a false success, and never mutate.
	if isSessionVerb(in.Action) {
		return QueueActionResult{Accepted: true, Outcome: queueOutcomeSkipped, Action: in.Action,
			Reason: "no live session provider (#136/#144)"}, nil
	}
	// §42: a dry run previews the action and mutates nothing — checked before the
	// guards so a dry-run submission of an interrupted retry is a PREVIEW, never a
	// mutation or an error.
	if in.DryRun || c.DryRun {
		return QueueActionResult{Accepted: true, Outcome: queueOutcomePreview, Action: in.Action,
			Reason: "dry run: no mutation"}, nil
	}
	if in.Action == queueActionCancel {
		// Idempotent: a cycle that is not running is already stopped → NOOP.
		if !c.Running {
			return QueueActionResult{Accepted: true, Outcome: queueOutcomeNoop, Action: in.Action,
				Reason: "cycle is not running"}, nil
		}
	} else { // retry
		if _, needsAttention := attentionReason(c); !needsAttention {
			return QueueActionResult{}, fmt.Errorf("%w: cycle %s does not need attention", errBadQueueAction, id)
		}
		if c.Running {
			return QueueActionResult{}, fmt.Errorf("%w: cycle is running; cancel it before retrying", errBadQueueAction)
		}
		// An interrupted cycle is never auto-retried: the operator must confirm
		// the old process stopped before a new run starts (§24 no silent replay).
		if c.Interrupted && !in.ConfirmStopped {
			return QueueActionResult{}, fmt.Errorf("%w: interrupted cycle requires confirmStopped before retry", errNeedsConfirmation)
		}
	}
	in.CycleID, in.Owner, in.Repo, in.Issue, in.Attempt = id, c.Owner, c.Repo, c.Issue, c.Attempt
	if strings.TrimSpace(in.Actor) == "" {
		in.Actor = "console"
	}
	return d.queue(ctx, in, queueItemFromCycle(c, false))
}

// consoleQueueWriter is the real authoritative writer: cancel goes through the
// Temporal lifecycle (the workflow's disconnect-safe cleanup still runs), retry
// starts a NEW run under the same deterministic ID (preserving prior runs in
// Temporal history) after best-effort stopping a still-registered interrupted
// execution (no silent replay). Both are §55-audited; a retry does not touch
// the recorded change/lineage history (prior attempts survive).
func consoleQueueWriter(d *consoleData) consoleQueueAction {
	return func(ctx context.Context, in QueueActionRequest, item QueueItem) (QueueActionResult, error) {
		var audit AuditConfig
		if rc, ok := d.repos[in.Repo]; ok && rc.Lifecycle != nil {
			audit = rc.Lifecycle.Audit
		}
		switch in.Action {
		case queueActionCancel:
			if d.cancelWorkflow == nil {
				return QueueActionResult{}, errQueueDisabled
			}
			if err := d.cancelWorkflow(ctx, in.CycleID); err != nil {
				return QueueActionResult{}, fmt.Errorf("cancel %s: %w", in.CycleID, err)
			}
		case queueActionRetry:
			if d.startCycle == nil {
				return QueueActionResult{}, errQueueDisabled
			}
			if item.Interrupted && d.cancelWorkflow != nil {
				_ = d.cancelWorkflow(ctx, in.CycleID)
			}
			if err := d.startCycle(ctx, d.retryInput(in)); err != nil {
				return QueueActionResult{}, fmt.Errorf("retry %s: %w", in.CycleID, err)
			}
		default:
			return QueueActionResult{Accepted: true, Outcome: queueOutcomeSkipped, Action: in.Action,
				Reason: "no live session provider (#136/#144)"}, nil
		}
		if err := RecordAuditEvent(ctx, AuditRequest{Config: audit, Event: AuditEvent{
			Owner: in.Owner, Repo: in.Repo, Issue: in.Issue,
			Step: queueAuditStep(in.Action), Actor: in.Actor, Decision: "allow",
		}}); err != nil {
			return QueueActionResult{}, err
		}
		return QueueActionResult{Accepted: true, Outcome: queueOutcomeApplied, Action: in.Action}, nil
	}
}

// retryInput rebuilds the IssueCycleWorkflow input from repos.yaml (owner,
// repo, repo path, lifecycle), so a retried run inherits the same policy.
func (d *consoleData) retryInput(in QueueActionRequest) IssueInput {
	scope := RepoScope{Owner: in.Owner, Repo: in.Repo}
	if rc, ok := d.repos[in.Repo]; ok {
		scope.Owner, scope.Repo, scope.RepoPath = rc.Owner, rc.Repo, rc.RepoPath
		if rc.Lifecycle != nil {
			scope.Lifecycle = *rc.Lifecycle
		}
	}
	return IssueInput{
		Owner: scope.Owner, Repo: scope.Repo, IssueNum: in.Issue,
		RepoPath: scope.RepoPath, Trigger: "queue", Lifecycle: scope.Lifecycle,
	}
}

// queueAuditStep maps an action to its §55 audit step.
func queueAuditStep(a string) string {
	if a == queueActionCancel {
		return auditStepQueueCancel
	}
	return auditStepQueueRetry
}

// temporalCancelWorkflow is the authoritative cancel seam (empty run id targets
// the current open run).
func temporalCancelWorkflow(c client.Client) func(context.Context, string) error {
	return func(ctx context.Context, id string) error { return c.CancelWorkflow(ctx, id, "") }
}

// temporalStartCycle starts a fresh run under the deterministic §32 ID. The
// reuse policy allows a new run after a closed one and the conflict policy
// converges on an already-running one, so a repeated retry never double-starts
// (§24).
func temporalStartCycle(c client.Client) func(context.Context, IssueInput) error {
	return func(ctx context.Context, in IssueInput) error {
		opts := client.StartWorkflowOptions{
			ID:                       fmt.Sprintf("%s-issue-%d", in.Repo, in.IssueNum),
			TaskQueue:                appCfg.Worker.TaskQueue,
			WorkflowIDReusePolicy:    enums.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE,
			WorkflowIDConflictPolicy: enums.WORKFLOW_ID_CONFLICT_POLICY_USE_EXISTING,
		}
		_, err := c.ExecuteWorkflow(ctx, opts, "IssueCycleWorkflow", in)
		return err
	}
}

// temporalExecStatus normalizes a workflow's Temporal execution status, so the
// console can tell "canceled" (intentional stop) from "interrupted" (worker/lease
// lost). ok=false when the execution is unknown / the probe fails.
func temporalExecStatus(c client.Client) func(context.Context, string) (string, bool) {
	return func(ctx context.Context, id string) (string, bool) {
		resp, err := c.DescribeWorkflowExecution(ctx, id, "")
		if err != nil {
			return "", false
		}
		switch resp.GetWorkflowExecutionInfo().GetStatus() {
		case enums.WORKFLOW_EXECUTION_STATUS_RUNNING:
			return execStatusRunning, true
		case enums.WORKFLOW_EXECUTION_STATUS_COMPLETED:
			return execStatusCompleted, true
		case enums.WORKFLOW_EXECUTION_STATUS_CANCELED:
			return execStatusCanceled, true
		case enums.WORKFLOW_EXECUTION_STATUS_FAILED:
			return execStatusFailed, true
		case enums.WORKFLOW_EXECUTION_STATUS_TIMED_OUT:
			return execStatusTimedOut, true
		case enums.WORKFLOW_EXECUTION_STATUS_TERMINATED:
			return execStatusTerminated, true
		}
		return "", false
	}
}
