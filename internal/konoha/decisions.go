package konoha

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// #108 console human decisions: approve / request-changes / deny.
//
// The console is NOT a second source of truth. A decision is written to the
// EXISTING §18 decision channel as the byte-exact `<!-- konoha-decision: <action> -->`
// marker (posted as one issue comment), so ReadHumanDecision/AwaitApproval pick
// it up unchanged. Request-changes additionally carries a size-bounded feedback
// body + diff-anchored comments in a base64 JSON payload marker, and records a
// #97 rework activation + change feedback + the cycle's comments.json evidence —
// all derived records, never authoritative over the §6 state (§44 #20).
//
// Authority rules (§108): only the CURRENT gate (the live attempt + reviewed
// SHA) may be actioned; a superseded attempt/SHA is stale and refused. A
// decision is idempotent per gate: the first submission wins and a repeated
// submit is a no-op, never a second decision.

const (
	// humanDecisionRequestChanges is the §18 action that rejects the current
	// gate and sends the work back for a bounded rework (Machinist model).
	humanDecisionRequestChanges = "request-changes"

	// maxDecisionFeedback bounds the request-changes feedback body in bytes.
	maxDecisionFeedback = 8 * 1024
	// maxDecisionComments bounds the diff-anchored comments one submission may
	// carry, so a payload can never balloon the evidence/audit record.
	maxDecisionComments = 200
	// maxDecisionBody bounds the whole HTTP decision request body.
	maxDecisionBody = 256 * 1024

	// decisionIDMarkerOpen and decisionPayloadMarkerOpen are byte-exact
	// HTML-comment markers embedded in the decision comment. The payload is
	// base64 so arbitrary feedback text (including `--`) cannot break the
	// comment.
	decisionIDMarkerOpen      = "<!-- konoha-decision-id: "
	decisionPayloadMarkerOpen = "<!-- konoha-decision-payload: "

	// auditStepConsoleDecision is the audit "step" recorded for a console
	// decision (the §55 audit event is otherwise step-scoped to publish/merge/
	// deploy; a console decision is a decision on one of those gates).
	auditStepConsoleDecision = "console-decision"
)

// DecisionRequest is one console decision submission. Attempt and SHA bind the
// decision to the exact gate the human saw; a superseded attempt/SHA is stale.
type DecisionRequest struct {
	Owner    string        `json:"owner,omitempty"`
	Repo     string        `json:"repo,omitempty"`
	Issue    int           `json:"issue,omitempty"`
	CycleID  string        `json:"cycleId,omitempty"`
	Action   string        `json:"action"`
	Feedback string        `json:"feedback,omitempty"`
	Comments []DiffComment `json:"comments,omitempty"`
	Attempt  int           `json:"attempt,omitempty"`
	SHA      string        `json:"sha,omitempty"`
	Actor    string        `json:"actor,omitempty"`
	DryRun   bool          `json:"dryRun,omitempty"`
}

// DecisionResult reports the outcome of a submit. Duplicate=true means the same
// gate had already been actioned, so the submit was a no-op (§24).
type DecisionResult struct {
	Accepted  bool   `json:"accepted"`
	Duplicate bool   `json:"duplicate,omitempty"`
	Action    string `json:"action,omitempty"`
	Attempt   int    `json:"attempt,omitempty"`
	SHA       string `json:"sha,omitempty"`
}

// decisionIdentity is the deterministic per-gate idempotency key: one decision
// may be recorded per (cycle, attempt, reviewed SHA). It excludes the action, so
// a second submission for the same gate is a no-op rather than a second decision.
func decisionIdentity(in DecisionRequest) string {
	return fmt.Sprintf("%s/%d/%s", in.CycleID, in.Attempt, in.SHA)
}

func decisionIDMarker(in DecisionRequest) string {
	return decisionIDMarkerOpen + decisionIdentity(in) + " -->"
}

// ValidateDecisionRequest deterministically gates one console decision (§36):
// action membership, size bounds, and the request-changes invariants. Owner/
// Issue/caller-filled fields are validated by the caller (the console snapshot).
func ValidateDecisionRequest(in DecisionRequest) error {
	if strings.TrimSpace(in.CycleID) == "" {
		return fmt.Errorf("decision: cycleId required")
	}
	switch in.Action {
	case humanDecisionApprove, humanDecisionDeny:
		if strings.TrimSpace(in.Feedback) != "" || len(in.Comments) > 0 {
			return fmt.Errorf("%s carries no feedback/comments", in.Action)
		}
	case humanDecisionRequestChanges:
		fb := strings.TrimSpace(in.Feedback)
		if fb == "" {
			return fmt.Errorf("request-changes requires non-empty feedback")
		}
		if len(in.Feedback) > maxDecisionFeedback {
			return fmt.Errorf("request-changes feedback %d bytes exceeds the %d-byte cap", len(in.Feedback), maxDecisionFeedback)
		}
		if len(in.Comments) > maxDecisionComments {
			return fmt.Errorf("request-changes carries %d comments, over the %d cap", len(in.Comments), maxDecisionComments)
		}
		for _, c := range in.Comments {
			if err := validateDiffComment(c, in.SHA); err != nil {
				return err
			}
		}
	case humanDecisionResume:
		// #120: a clarification form maps onto the existing §18 `resume` action;
		// it may carry the typed answer as feedback, but no anchored comments.
		if len(in.Comments) > 0 {
			return fmt.Errorf("resume carries no anchored comments")
		}
		if len(in.Feedback) > maxDecisionFeedback {
			return fmt.Errorf("resume feedback %d bytes exceeds the %d-byte cap", len(in.Feedback), maxDecisionFeedback)
		}
	default:
		return fmt.Errorf("decision action %q not in the §108 set {approve, request-changes, deny, resume}", in.Action)
	}
	if in.Attempt <= 0 {
		return fmt.Errorf("decision: attempt required (the gate identity)")
	}
	if strings.TrimSpace(in.SHA) == "" {
		return fmt.Errorf("decision: reviewed sha required (the gate identity)")
	}
	return nil
}

// validCommentSide is the closed set of diff sides a comment may anchor to: the
// new file (`new`), the old file (`old`), or unspecified. Anything else fails
// closed (§36) so an unanchorable side can never be stored.
func validCommentSide(side string) bool {
	switch side {
	case "", "new", "old":
		return true
	}
	return false
}

// validateDiffComment checks one diff-anchored comment against the gate's
// reviewed SHA: the comment must be anchored to (sha, file, line, side), and a
// comment anchored to any other (superseded) SHA is stale and can never be
// re-submitted (§108/#113).
func validateDiffComment(c DiffComment, reviewedSHA string) error {
	body := strings.TrimSpace(c.Body)
	if body == "" {
		return fmt.Errorf("comment requires a non-empty body")
	}
	if len(c.Body) > maxDecisionFeedback {
		return fmt.Errorf("comment body %d bytes exceeds the %d-byte cap", len(c.Body), maxDecisionFeedback)
	}
	if strings.TrimSpace(c.File) == "" {
		return fmt.Errorf("comment requires a file anchor")
	}
	if c.Line < 0 {
		return fmt.Errorf("comment line %d must be non-negative", c.Line)
	}
	if !validCommentSide(c.Side) {
		return fmt.Errorf("comment side %q not in the diff-side set {\"\", new, old}", c.Side)
	}
	if strings.TrimSpace(c.SHA) == "" || c.SHA != reviewedSHA {
		return fmt.Errorf("comment anchored to %q is stale: the current gate is bound to %q", c.SHA, reviewedSHA)
	}
	return nil
}

// diffCommentID derives the stable, content-addressed id for one anchored
// comment: the same (gate, anchor, body) submitted twice maps to the SAME id, so
// carrying comments forward or replaying a submission is idempotent and never
// duplicates (§24). Two byte-identical comments at one anchor are the same
// comment; a distinct body yields a distinct id.
func diffCommentID(in DecisionRequest, c DiffComment) string {
	h := sha256.Sum256([]byte(strings.Join([]string{
		in.CycleID, strconv.Itoa(in.Attempt), c.SHA,
		strings.TrimSpace(c.File), strconv.Itoa(c.Line), c.Side, strings.TrimSpace(c.Body),
	}, "\x00")))
	return fmt.Sprintf("%s/%d/%s/%s", in.CycleID, in.Attempt, c.SHA, hex.EncodeToString(h[:8]))
}

// sortCommentsForDelivery orders comments deterministically for delivery/display:
// by file, then line, then side, preserving submission order for ties (§113).
// Pure.
func sortCommentsForDelivery(comments []DiffComment) []DiffComment {
	out := append([]DiffComment(nil), comments...)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.File != b.File {
			return a.File < b.File
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		return a.Side < b.Side
	})
	return out
}

// anchoredCommentLine renders one comment in the deterministic, parseable
// `file:line (side): body` form the repair agent consumes (§35). Pure.
func anchoredCommentLine(c DiffComment) string {
	anchor := fmt.Sprintf("%s:%d", strings.TrimSpace(c.File), c.Line)
	if s := strings.TrimSpace(c.Side); s != "" {
		anchor += " (" + s + ")"
	}
	return anchor + ": " + strings.TrimSpace(c.Body)
}

// anchoredCommentLines renders a bundle of anchored comments in deterministic
// order for the repair/rework input; entries missing a file/body are dropped
// (they would render nothing parseable). Pure, unit-tested without Temporal.
func anchoredCommentLines(comments []DiffComment) []string {
	var out []string
	for _, c := range sortCommentsForDelivery(comments) {
		if strings.TrimSpace(c.File) == "" || strings.TrimSpace(c.Body) == "" {
			continue
		}
		out = append(out, anchoredCommentLine(c))
	}
	return out
}

// reworkBlocking projects a §108 request-changes rework into the ordered BLOCKING
// findings the repair agent consumes: the human's freeform feedback lines first,
// then the diff-anchored comments in deterministic `file:line (side): body` form.
// Pure and unit-tested without a browser or Temporal.
func reworkBlocking(feedback string, comments []DiffComment) []string {
	out := feedbackLines(feedback)
	return append(out, anchoredCommentLines(comments)...)
}

// mergeComments appends incoming comments to the existing list, REPLACING any
// entry with the same stable id in place (order preserved) — idempotent under
// retry and carry-forward (§24). Existing (earlier-revision) comments are kept,
// never dropped, so the full thread is retained across revisions.
func mergeComments(existing, incoming []DiffComment) []DiffComment {
	out := append([]DiffComment(nil), existing...)
	for _, c := range incoming {
		replaced := false
		for i := range out {
			if out[i].ID == c.ID {
				out[i] = c
				replaced = true
				break
			}
		}
		if !replaced {
			out = append(out, c)
		}
	}
	return out
}

// decisionPayload is the machine-readable body of a decision comment (base64
// encoded in the comment so it survives GitHub rendering byte-exactly).
type decisionPayload struct {
	Feedback string        `json:"feedback,omitempty"`
	Comments []DiffComment `json:"comments,omitempty"`
}

// humanDecisionCommentBody renders the one comment that carries the decision:
// the byte-exact §18 marker, the per-gate idempotency marker, and the base64
// payload, followed by the human-readable feedback.
func humanDecisionCommentBody(in DecisionRequest) string {
	var b strings.Builder
	b.WriteString(humanDecisionMarker(in.Action))
	b.WriteString("\n")
	b.WriteString(decisionIDMarker(in))
	b.WriteString("\n")
	payload, _ := json.Marshal(decisionPayload{Feedback: in.Feedback, Comments: in.Comments})
	b.WriteString(decisionPayloadMarkerOpen)
	b.WriteString(base64.StdEncoding.EncodeToString(payload))
	b.WriteString(" -->\n")
	if fb := strings.TrimSpace(in.Feedback); fb != "" {
		b.WriteString("\n")
		b.WriteString(fb)
		b.WriteString("\n")
	}
	return b.String()
}

// parseDecisionPayload decodes the base64 payload marker from a decision body.
// Absent or malformed → ok=false (the action marker still drives the decision).
func parseDecisionPayload(body string) (decisionPayload, bool) {
	enc, ok := parseMarker(body, decisionPayloadMarkerOpen)
	if !ok {
		return decisionPayload{}, false
	}
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		return decisionPayload{}, false
	}
	var p decisionPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return decisionPayload{}, false
	}
	return p, true
}

// SubmitHumanDecision writes one decision to the §18 channel as an issue comment,
// idempotent per gate: it searches the issue's comments for the decision-id
// marker before creating, so a repeated submit (or an activity retry) never
// posts a second decision (§24). The audit line is appended by the existing
// §55 RecordAuditEvent. Never call activity.GetLogger here (non-Temporal ctx).
func SubmitHumanDecision(ctx context.Context, audit AuditConfig, in DecisionRequest) (DecisionResult, error) {
	if in.DryRun {
		return DecisionResult{Accepted: true, Action: in.Action, Attempt: in.Attempt, SHA: in.SHA}, nil
	}
	if err := ValidateDecisionRequest(in); err != nil {
		return DecisionResult{}, err
	}
	id := decisionIDMarker(in)
	out, err := ghRunner(ctx, "issue", "view", strconv.Itoa(in.Issue),
		"-R", in.Owner+"/"+in.Repo, "--json", "comments")
	if err != nil {
		return DecisionResult{}, fmt.Errorf("gh issue view (decision idempotency): %s: %w", strings.TrimSpace(string(out)), err)
	}
	var doc struct {
		Comments []struct {
			Body string `json:"body"`
		} `json:"comments"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		return DecisionResult{}, fmt.Errorf("parse issue comments: %w", err)
	}
	for _, c := range doc.Comments {
		if strings.Contains(c.Body, id) {
			return DecisionResult{Accepted: true, Duplicate: true, Action: in.Action, Attempt: in.Attempt, SHA: in.SHA}, nil
		}
	}
	body := humanDecisionCommentBody(in)
	if out, err := ghRunner(ctx, "issue", "comment", strconv.Itoa(in.Issue),
		"-R", in.Owner+"/"+in.Repo, "--body", body); err != nil {
		return DecisionResult{}, fmt.Errorf("gh issue comment (decision): %s: %w", strings.TrimSpace(string(out)), err)
	}
	if err := RecordAuditEvent(ctx, AuditRequest{Config: audit, Event: AuditEvent{
		Owner: in.Owner, Repo: in.Repo, Issue: in.Issue, Step: auditStepConsoleDecision,
		Actor: in.Actor, Decision: auditDecisionForAction(in.Action), Detail: strings.TrimSpace(in.Feedback),
	}}); err != nil {
		return DecisionResult{}, err // §55 fail-closed: an unrecorded decision is surfaced
	}
	return DecisionResult{Accepted: true, Action: in.Action, Attempt: in.Attempt, SHA: in.SHA}, nil
}

// auditDecisionForAction maps a console action to the §55 audit decision field.
func auditDecisionForAction(a string) string {
	switch a {
	case humanDecisionApprove:
		return "allow"
	case humanDecisionDeny:
		return "deny"
	case humanDecisionRequestChanges:
		return "request_changes"
	}
	return a
}

// WriteConsoleDecision is the console's decision writer: it writes the §18
// channel (SubmitHumanDecision) and, for request-changes, records the derived
// #97 evidence (a rework activation carrying the feedback, the change feedback
// thread, and the cycle's diff-anchored comments). A duplicate submission
// records no new evidence.
func WriteConsoleDecision(ctx context.Context, store *EvidenceStore, audit AuditConfig, in DecisionRequest) (DecisionResult, error) {
	res, err := SubmitHumanDecision(ctx, audit, in)
	if err != nil {
		return DecisionResult{}, err
	}
	if res.Duplicate {
		return res, nil
	}
	if err := applyDecisionEvidence(store, in); err != nil {
		return DecisionResult{}, err
	}
	return res, nil
}

// applyDecisionEvidence records the derived records for a request-changes
// decision: the rework activation (#97, reason=repair, carrying the feedback),
// the change feedback thread + rework note, and the diff-anchored comments as
// the cycle's comments.json (rendered on the next review). Best-effort callers
// treat a failure as fail-closed (the decision was posted; the evidence error is
// surfaced, never silently dropped).
func applyDecisionEvidence(store *EvidenceStore, in DecisionRequest) error {
	if store == nil || in.Action != humanDecisionRequestChanges || in.DryRun {
		return nil
	}
	// Enrich the submitted comments with their cycle/attempt/SHA identity and a
	// stable content-addressed id BEFORE recording, so both the change thread and
	// comments.json agree on one identity.
	now := time.Now().UTC()
	comments := make([]DiffComment, 0, len(in.Comments))
	for _, c := range in.Comments {
		c.ID = diffCommentID(in, c)
		c.CycleID = in.CycleID
		c.Attempt = in.Attempt
		if c.Author == "" {
			c.Author = in.Actor
		}
		if c.CreatedAt.IsZero() {
			c.CreatedAt = now
		}
		comments = append(comments, c)
	}

	// #97: a distinct rework activation (attempt+1) so develop + rework are two
	// ordered activations. Idempotent by WorkflowID.
	act := LineageActivation{
		Attempt: in.Attempt + 1,
		// The workflow id suffix keeps the rework distinct from the workflow's
		// own repair activation (workflowId-repair-N).
		WorkflowID: in.CycleID + "-human-rework-" + strconv.Itoa(in.Attempt),
		Reason:     activationReasonRepair,
		Stage:      stageRepair,
		Result:     "HUMAN_REWORK",
		ReworkNote: in.Feedback,
	}
	if err := store.recordActivation(in.Owner, in.Repo, in.Issue, act, false); err != nil {
		return fmt.Errorf("decision rework activation: %w", err)
	}
	// #97: carry the feedback onto the change record + ordered feedback thread
	// (status returns to OPEN: the working path). Each anchored comment is its own
	// entry, keyed by the stable comment id, so the full thread is retained across
	// revisions and a replay replaces in place (§24).
	key := fmt.Sprintf("%s/human-rework/%d", in.CycleID, in.Attempt)
	feedback := []ChangeFeedback{{
		Key: key, Kind: feedbackKindReworkNote, Author: in.Actor, Body: in.Feedback, At: now,
	}}
	for _, c := range comments {
		feedback = append(feedback, ChangeFeedback{
			Key:    in.CycleID + "/review-comment/" + c.ID,
			Kind:   feedbackKindReviewComment,
			Author: c.Author,
			Body:   anchoredCommentLine(c),
			At:     c.CreatedAt,
		})
	}
	if err := store.recordChange(ChangeRequest{
		Owner: in.Owner, Repo: in.Repo, Issue: in.Issue, WorkflowID: in.CycleID,
		State:      stateChangesRequested,
		ReworkNote: in.Feedback,
		Feedback:   feedback,
	}); err != nil {
		return fmt.Errorf("decision change feedback: %w", err)
	}
	// #108/#113: persist the diff-anchored comments so the next attempt's diff
	// view shows them, MERGING with any carried-over comments by stable id (so the
	// thread is retained across revisions and never duplicated). Bodies are
	// redacted by the evidence store Write (§82).
	if len(comments) > 0 {
		merged := mergeComments(store.readComments(in.CycleID), comments)
		b, err := json.Marshal(merged)
		if err != nil {
			return err
		}
		if err := store.Write(in.CycleID, evidenceComments, string(b), false); err != nil {
			return fmt.Errorf("decision comments evidence: %w", err)
		}
	}
	return nil
}

// readComments reads the optional recorded comments.json for one cycle,
// best-effort: absent/malformed yields none (never an error to the caller).
func (s *EvidenceStore) readComments(id string) []DiffComment {
	raw, ok := s.readArtifact(id, evidenceComments)
	if !ok || strings.TrimSpace(raw) == "" {
		return nil
	}
	var cs []DiffComment
	if err := json.Unmarshal([]byte(raw), &cs); err != nil {
		return nil
	}
	return cs
}

// feedbackLines splits a request-changes feedback body into bounded, non-empty
// blocking lines for the repair agent. An empty body yields no lines (fail-safe
// callers substitute a default message).
func feedbackLines(feedback string) []string {
	var out []string
	for _, l := range strings.Split(feedback, "\n") {
		if t := strings.TrimSpace(l); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// GateSnapshot is the current gate identity for one cycle: the owner, whether a
// gate is open (PENDING_APPROVAL), the live attempt number, and the reviewed SHA.
type GateSnapshot struct {
	Owner   string
	Repo    string
	Issue   int
	Open    bool
	Attempt int
	SHA     string
}

// currentAttemptSHA picks the highest-attempt activation's attempt + commit SHA
// (the live gate identity). Pure; zero when there are no activations.
func currentAttemptSHA(activations []LineageActivation) (int, string) {
	attempt, sha := 0, ""
	for _, a := range activations {
		if a.Attempt >= attempt {
			attempt, sha = a.Attempt, a.CommitSHA
		}
	}
	return attempt, sha
}
