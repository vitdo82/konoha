package konoha

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"strings"
)

// Per-issue activation lineage + policy/model digest (#88). Evidence gives us
// per-cycle artifacts and outcome.json the terminal state; lineage.json adds the
// durable IDENTITY view: every develop/repair activation of one (repo, issue)
// with the harness/model/base/commit that produced it, plus recomputed
// token/cost aggregates. The digest answers "same config, different result?" by
// pinning the effective policy + resolved harness/model + task identity into
// outcome.json.
//
// ESF rule: numbers are recomputed from recorded activations, never estimated. A
// missing usage entry contributes ZERO. Lineage is best-effort observability and
// never authoritative over the §6 terminal state (§40/§44 #20).

// Activation reasons (byte-exact; a repair rework is a `repair`).
const (
	activationReasonInitial = "initial"
	activationReasonRepair  = "repair"
)

// LineageActivation is one recorded agent activation for an issue. WorkflowID is
// the idempotency key: re-recording the same activation REPLACES it, never
// appends (§24), so Temporal activity retries converge. A repair is recorded
// under a distinct WorkflowID (the cycle id suffixed with the attempt) so a
// develop + repair are two ordered activations, not one.
type LineageActivation struct {
	Attempt     int    `json:"attempt"`
	WorkflowID  string `json:"workflowId"`
	Reason      string `json:"reason"`          // initial|repair
	Stage       string `json:"stage,omitempty"` // §98: model-selectable stage (developer|repair)
	Harness     string `json:"harness,omitempty"`
	Model       string `json:"model,omitempty"`
	ModelDigest string `json:"modelDigest,omitempty"` // §98: digest of the resolved named model
	BaseSHA     string `json:"baseSha,omitempty"`
	CommitSHA   string `json:"commitSha,omitempty"`
	Result      string `json:"result,omitempty"` // AgentOutcome (SUCCESS|BLOCKED|...)
	// #97: the rework instruction carried onto this activation from a rejected
	// review (empty for the initial develop).
	ReworkNote string `json:"reworkNote,omitempty"`
	// #87 usage, recorded per activation so the totals are a recomputation.
	TokensIn  int64   `json:"tokensIn,omitempty"`
	TokensOut int64   `json:"tokensOut,omitempty"`
	CostUSD   float64 `json:"costUSD,omitempty"`
}

// Lineage is the per-(repo, issue) activation record persisted as lineage.json.
type Lineage struct {
	Repo         string              `json:"repo"`
	Issue        int                 `json:"issue"`
	Activations  []LineageActivation `json:"activations"`
	Attempts     int                 `json:"attempts"`
	TotalTokens  int64               `json:"totalTokens"`
	TotalCostUsd float64             `json:"totalCostUsd"`
}

// ActivationRequest is the input to the RecordActivation activity.
type ActivationRequest struct {
	Owner      string            `json:"owner"`
	Repo       string            `json:"repo"`
	Issue      int               `json:"issue"`
	Activation LineageActivation `json:"activation"`
	DryRun     bool              `json:"dryRun,omitempty"`
}

// upsertActivation inserts act, or REPLACES the existing activation with the same
// WorkflowID in place (preserving order). Pure and deterministic.
func upsertActivation(list []LineageActivation, act LineageActivation) []LineageActivation {
	for i := range list {
		if list[i].WorkflowID == act.WorkflowID {
			list[i] = act
			return list
		}
	}
	return append(list, act)
}

// recomputeLineage re-derives the aggregates from the recorded activations:
// Attempts = number of activations, totals = sum of the recorded usage only
// (absent/negative components contribute ZERO — never estimated).
func recomputeLineage(l *Lineage) {
	l.Attempts = len(l.Activations)
	var tokensIn, tokensOut int64
	var cost float64
	for _, a := range l.Activations {
		if a.TokensIn > 0 {
			tokensIn += a.TokensIn
		}
		if a.TokensOut > 0 {
			tokensOut += a.TokensOut
		}
		if a.CostUSD > 0 {
			cost += a.CostUSD
		}
	}
	l.TotalTokens = tokensIn + tokensOut
	l.TotalCostUsd = cost
}

// lineagePath resolves the sink-relative lineage.json path for one (owner,
// repo, issue), rejecting a non-plain-segment id (traversal guard, mirroring the
// evidence store). DryRun mirrors evidence under dryrun/ (§42: a record, not a
// mutation).
func (s *EvidenceStore) lineagePath(owner, repo string, issue int, dryRun bool) (string, error) {
	id := fmt.Sprintf("%s-%s-issue-%d", owner, repo, issue)
	if !validCycleID(id) {
		return "", fmt.Errorf("lineage id %q is not a plain path segment", id)
	}
	if dryRun {
		return path.Join("dryrun", "lineage", id+".json"), nil
	}
	return path.Join("lineage", id+".json"), nil
}

// recordActivation loads the existing lineage (if any), upserts the activation
// by WorkflowID, recomputes the aggregates, and writes it atomically. A corrupt
// existing file fails closed rather than silently discarding prior activations.
func (s *EvidenceStore) recordActivation(owner, repo string, issue int, act LineageActivation, dryRun bool) error {
	rel, err := s.lineagePath(owner, repo, issue, dryRun)
	if err != nil {
		return err
	}
	var l Lineage
	b, err := s.sink.Read(rel)
	switch {
	case err == nil:
		if err := json.Unmarshal(b, &l); err != nil {
			return fmt.Errorf("lineage %s: %w", rel, err)
		}
	case os.IsNotExist(err):
		// fresh record
	default:
		return err
	}
	l.Repo = owner + "/" + repo
	l.Issue = issue
	l.Activations = upsertActivation(l.Activations, act)
	recomputeLineage(&l)
	out, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return err
	}
	return s.sink.Write(rel, []byte(s.redactor.Redact(string(out))))
}

// RecordActivation is the lineage I/O activity (#88): an idempotent, atomic
// upsert of one activation under evidence.dir. Best-effort at the workflow; a
// write failure never changes the §6 terminal state. Never call
// activity.GetLogger here: unit tests use a non-Temporal context (it panics).
func RecordActivation(_ context.Context, in ActivationRequest) error {
	store, err := NewEvidenceStore(appCfg.Evidence, &defaultRedactor)
	if err != nil {
		return err
	}
	return store.recordActivation(in.Owner, in.Repo, in.Issue, in.Activation, in.DryRun)
}

// resolvedHarnessModel resolves the effective harness name and its concrete
// model for a repo's lifecycle (repo selection → process default → built-in, and
// the harness's own model resolution). Pure given the process-fixed appCfg; on a
// resolution error it reports the raw configured model so the digest still
// tracks config edits.
func resolvedHarnessModel(lc LifecycleConfig) (harness, model string) {
	harness = effectiveHarnessName(lc.Harness)
	h, err := harnessRegistryFor().Lookup(harness)
	if err != nil {
		return harness, strings.TrimSpace(lc.Model)
	}
	m, err := h.ResolveModel(lc.Model)
	if err != nil {
		return harness, strings.TrimSpace(lc.Model)
	}
	return harness, m
}

// digestPayload is the canonical digest input; json.Marshal is canonical (struct
// field order fixed, map keys sorted) so the hash is stable across processes.
type digestPayload struct {
	Policy   LifecycleConfig `json:"policy"`
	Harness  string          `json:"harness"`
	Model    string          `json:"model"`
	TaskHash string          `json:"taskHash"`
}

// taskHash is the deterministic identity of the work item (the issue), so the
// digest distinguishes issues that share a policy.
func taskHash(owner, repo string, issue int) string {
	b, _ := json.Marshal(struct {
		Owner string `json:"owner"`
		Repo  string `json:"repo"`
		Issue int    `json:"issue"`
	}{owner, repo, issue})
	return sha256Hex(b)
}

// policyDigest is the SHA-256 of the effective LifecycleConfig + resolved
// harness/model + task hash, canonical-JSON encoded. Identical inputs yield an
// identical digest across processes; any policy/model/task change flips it.
func policyDigest(lc LifecycleConfig, task string) string {
	harness, model := resolvedHarnessModel(lc)
	b, _ := json.Marshal(digestPayload{Policy: lc, Harness: harness, Model: model, TaskHash: task})
	return sha256Hex(b)
}

// sha256Hex renders a SHA-256 digest as lowercase hex.
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
