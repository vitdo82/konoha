package konoha

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"sort"
	"strings"
	"time"
)

// Change / work-item lifecycle (#97). ESF's durable Change has a lifecycle
// (OPEN/IN_REVIEW/DONE/ABANDONED), carries a rework note across revisions, and is
// queryable. Konoha already records per-issue activations (#88 lineage) and the
// terminal §6 state (#83 outcome.json); this file adds the derived change view: a
// durable per-(repo, issue) record whose STATUS is derived from the §6 state, a
// redacted rework note carried forward, and the full ordered feedback thread
// (rework notes + review comments).
//
// The record is recomputed from recorded evidence/manifests, never estimated
// (§88 rule), best-effort, and never authoritative over the §6 terminal state
// (§40/§44 #20). It is idempotent by (repo, issue) and by feedback key (§24).

// Change lifecycle statuses (byte-exact; mirror ESF's Change.status).
const (
	changeStatusOpen      = "OPEN"
	changeStatusInReview  = "IN_REVIEW"
	changeStatusDone      = "DONE"
	changeStatusAbandoned = "ABANDONED"
)

// Feedback thread entry kinds.
const (
	feedbackKindReworkNote    = "rework_note"
	feedbackKindReviewComment = "review_comment"
)

// ChangeFeedback is one redacted entry in the ordered review/rework thread.
// Key is the idempotency key: re-recording the same entry REPLACES it, never
// appends (§24), so an activity retry converges. Order is preserved.
type ChangeFeedback struct {
	Key    string    `json:"key"`
	Kind   string    `json:"kind"`
	Author string    `json:"author,omitempty"`
	Body   string    `json:"body"`
	At     time.Time `json:"at"`
}

// Change is the per-(repo, issue) work-item record persisted as change.json.
type Change struct {
	Repo       string           `json:"repo"`
	Issue      int              `json:"issue"`
	Status     string           `json:"status"`
	WorkflowID string           `json:"workflowId,omitempty"`
	ReworkNote string           `json:"reworkNote,omitempty"`
	Feedback   []ChangeFeedback `json:"feedback"`
	UpdatedAt  time.Time        `json:"updatedAt"`
}

// ChangeRequest is the input to the RecordChange activity. State is the §6 state
// observed (terminal or in-flight, e.g. PENDING_APPROVAL); the change status is
// derived from it, never set directly.
type ChangeRequest struct {
	Owner      string           `json:"owner"`
	Repo       string           `json:"repo"`
	Issue      int              `json:"issue"`
	WorkflowID string           `json:"workflowId,omitempty"`
	State      string           `json:"state,omitempty"`
	ReworkNote string           `json:"reworkNote,omitempty"`
	Feedback   []ChangeFeedback `json:"feedback,omitempty"`
	DryRun     bool             `json:"dryRun,omitempty"`
}

// deriveChangeStatus maps the §6 state to the change lifecycle status: CLOSED →
// DONE, CANCELLED → ABANDONED, a human-decision wait (PENDING_APPROVAL /
// HUMAN_REQUIRED) → IN_REVIEW, everything else → OPEN. Pure.
func deriveChangeStatus(state string) string {
	switch state {
	case stateClosed:
		return changeStatusDone
	case stateCancelled:
		return changeStatusAbandoned
	case statePendingApproval, stateHumanRequired:
		return changeStatusInReview
	default:
		return changeStatusOpen
	}
}

// upsertFeedback inserts entries, REPLACING the existing entry with the same Key
// in place (order preserved) — idempotent by feedback key (§24).
func upsertFeedback(list []ChangeFeedback, in []ChangeFeedback) []ChangeFeedback {
	for _, f := range in {
		replaced := false
		for i := range list {
			if list[i].Key == f.Key {
				list[i] = f
				replaced = true
				break
			}
		}
		if !replaced {
			list = append(list, f)
		}
	}
	return list
}

// reviewFeedback projects one revision's review findings into ordered, keyed
// feedback entries: blocking comments then nits (idempotent by workflow +
// revision + kind + index, §24). Empty findings contribute nothing.
func reviewFeedback(workflowID string, revision int, rev ReviewResult) []ChangeFeedback {
	var out []ChangeFeedback
	add := func(kind string, i int, body string) {
		out = append(out, ChangeFeedback{
			Key:  fmt.Sprintf("%s/review/%d/%s/%d", workflowID, revision, kind, i),
			Kind: feedbackKindReviewComment, Author: "reviewer", Body: body,
		})
	}
	for i, b := range rev.Blocking {
		add("blocking", i, b)
	}
	for i, n := range rev.Nits {
		add("nit", i, n)
	}
	return out
}

// reworkInstruction is the human/agent-facing rework note carried onto the
// change and the next activation from a rejected review.
func reworkInstruction(rev ReviewResult) string {
	return strings.Join(rev.Blocking, "\n")
}

// changePath resolves the sink-relative change.json path for one repo+issue,
// rejecting a non-plain-segment id (traversal guard, mirroring the evidence
// store). DryRun mirrors evidence under dryrun/ (§42: a record, not a mutation).
func (s *EvidenceStore) changePath(repo string, issue int, dryRun bool) (string, error) {
	id := fmt.Sprintf("%s-issue-%d", repo, issue)
	if !validCycleID(id) {
		return "", fmt.Errorf("change id %q is not a plain path segment", id)
	}
	if dryRun {
		return path.Join("dryrun", "changes", id+".json"), nil
	}
	return path.Join("changes", id+".json"), nil
}

// loadLineage reads lineage.json for one (owner, repo, issue); a missing file is
// not an error (an issue may have a change record but no recorded activations).
func (s *EvidenceStore) loadLineage(owner, repo string, issue int, dryRun bool) (Lineage, error) {
	rel, err := s.lineagePath(owner, repo, issue, dryRun)
	if err != nil {
		return Lineage{}, err
	}
	b, err := s.sink.Read(rel)
	if os.IsNotExist(err) {
		return Lineage{}, nil
	}
	if err != nil {
		return Lineage{}, err
	}
	var l Lineage
	if err := json.Unmarshal(b, &l); err != nil {
		return Lineage{}, err
	}
	return l, nil
}

// recordChange loads the existing change (if any), derives the status, carries
// the rework note, merges the feedback thread by key, and writes it atomically
// redacted. A corrupt existing file fails closed rather than discarding prior
// feedback.
func (s *EvidenceStore) recordChange(in ChangeRequest) error {
	rel, err := s.changePath(in.Repo, in.Issue, in.DryRun)
	if err != nil {
		return err
	}
	var c Change
	b, err := s.sink.Read(rel)
	switch {
	case err == nil:
		if err := json.Unmarshal(b, &c); err != nil {
			return fmt.Errorf("change %s: %w", rel, err)
		}
	case os.IsNotExist(err):
		// fresh record
	default:
		return err
	}
	c.Repo = in.Owner + "/" + in.Repo
	c.Issue = in.Issue
	if in.WorkflowID != "" {
		c.WorkflowID = in.WorkflowID
	}
	if in.State != "" {
		c.Status = deriveChangeStatus(in.State)
	}
	if c.Status == "" {
		c.Status = changeStatusOpen
	}
	if strings.TrimSpace(in.ReworkNote) != "" {
		c.ReworkNote = in.ReworkNote
	}
	c.Feedback = upsertFeedback(c.Feedback, in.Feedback)
	c.UpdatedAt = time.Now().UTC()
	out, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return s.sink.Write(rel, []byte(s.redactor.Redact(string(out))))
}

// loadChange reads the recorded change for one (repo, issue). A missing file is
// an error (the CLI reports "no such change" rather than printing an empty one).
func (s *EvidenceStore) loadChange(repo string, issue int) (Change, error) {
	rel, err := s.changePath(repo, issue, false)
	if err != nil {
		return Change{}, err
	}
	b, err := s.sink.Read(rel)
	if err != nil {
		return Change{}, err
	}
	var c Change
	if err := json.Unmarshal(b, &c); err != nil {
		return Change{}, err
	}
	return c, nil
}

// listChanges reads every recorded (non-dryrun) change record. A missing
// changes/ prefix is an empty list, never an error.
func (s *EvidenceStore) listChanges() ([]Change, error) {
	paths, err := s.sink.List("changes")
	if err != nil {
		return nil, err
	}
	var out []Change
	for _, p := range paths {
		if !strings.HasSuffix(p, ".json") {
			continue
		}
		b, err := s.sink.Read(p)
		if err != nil {
			return nil, err
		}
		var c Change
		if err := json.Unmarshal(b, &c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

// RecordChange is the change-lifecycle I/O activity (#97): an idempotent,
// atomic upsert of one (repo, issue)'s change record under evidence.dir.
// Best-effort at the workflow; a write failure never changes the §6 terminal
// state. Never call activity.GetLogger here: unit tests use a non-Temporal
// context (it panics).
func RecordChange(_ context.Context, in ChangeRequest) error {
	store, err := NewEvidenceStore(appCfg.Evidence, &defaultRedactor)
	if err != nil {
		return err
	}
	return store.recordChange(in)
}

// ChangeView is the pure projection of a change joined with its recomputed
// lineage (activations + totals) — what `prbot describe change` prints.
type ChangeView struct {
	Repo         string              `json:"repo"`
	Issue        int                 `json:"issue"`
	Status       string              `json:"status"`
	WorkflowID   string              `json:"workflowId,omitempty"`
	ReworkNote   string              `json:"reworkNote,omitempty"`
	Feedback     []ChangeFeedback    `json:"feedback"`
	Activations  []LineageActivation `json:"activations"`
	Attempts     int                 `json:"attempts"`
	TotalTokens  int64               `json:"totalTokens"`
	TotalCostUsd float64             `json:"totalCostUsd"`
	UpdatedAt    time.Time           `json:"updatedAt,omitempty"`
}

// buildChangeView joins a change record with its lineage; pure and deterministic
// (totals are the recomputed lineage aggregates, never estimated).
func buildChangeView(c Change, l Lineage) ChangeView {
	return ChangeView{
		Repo: c.Repo, Issue: c.Issue, Status: c.Status, WorkflowID: c.WorkflowID,
		ReworkNote:  c.ReworkNote,
		Feedback:    append([]ChangeFeedback{}, c.Feedback...),
		Activations: append([]LineageActivation{}, l.Activations...),
		Attempts:    l.Attempts, TotalTokens: l.TotalTokens, TotalCostUsd: l.TotalCostUsd,
		UpdatedAt: c.UpdatedAt,
	}
}

// sortChangesNewestFirst orders changes by UpdatedAt descending, ties broken by
// the higher issue number (stable).
func sortChangesNewestFirst(list []Change) {
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].UpdatedAt.Equal(list[j].UpdatedAt) {
			return list[i].Issue > list[j].Issue
		}
		return list[i].UpdatedAt.After(list[j].UpdatedAt)
	})
}

// changeViewJSON is the pure projection from a change view to printable JSON
// (used by `prbot describe change`, unit-tested without a Temporal server).
func changeViewJSON(v ChangeView) (string, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// changesJSON is the pure projection from a change list to printable JSON (used
// by `prbot get changes`, unit-tested without a Temporal server).
func changesJSON(list []Change) (string, error) {
	if list == nil {
		list = []Change{}
	}
	b, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b), nil
}
