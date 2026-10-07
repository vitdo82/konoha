package konoha

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// Queryable evidence export for evals (#125). The evidence store (#83) is
// organized for audit, one cycle at a time; this projects the SAME recorded data
// — #88 lineage activations joined with the cycle's #84 outcome + #87 usage +
// #88 policy digest — into one stable, versioned JSONL record per attempt, so
// history becomes data for analytics/evals. No new storage: the export only
// reads the existing store; every record is redacted (#82) and raw prompts stay
// out unless explicitly opted in.

const exportSchemaVersion = 1

// exportPromptSizeCap bounds an opted-in prompt (bytes) so a large raw prompt
// can never make an eval record unbounded.
const exportPromptSizeCap = 16 * 1024

// ExportRecord is the stable, versioned per-attempt export schema (one JSONL
// line). The field set is the eval contract: adding/removing a field is a
// schema-version bump.
type ExportRecord struct {
	SchemaVersion       int         `json:"schemaVersion"`
	Repo                string      `json:"repo"`
	Issue               int         `json:"issue"`
	Attempt             int         `json:"attempt"`
	Reason              string      `json:"reason,omitempty"` // initial|repair
	Harness             string      `json:"harness,omitempty"`
	Model               string      `json:"model,omitempty"`
	AgentOutcome        string      `json:"agentOutcome,omitempty"`
	VerificationOutcome string      `json:"verificationOutcome,omitempty"`
	TerminalState       string      `json:"terminalState,omitempty"`
	Conditions          []Condition `json:"conditions,omitempty"`
	TokensIn            int64       `json:"tokensIn,omitempty"`
	TokensOut           int64       `json:"tokensOut,omitempty"`
	CostUsd             float64     `json:"costUsd,omitempty"`
	PolicyDigest        string      `json:"policyDigest,omitempty"`
	// At is the cycle's latest condition timestamp (derived; zero when no
	// outcome is recorded). It is the `--since` filter key, not a per-attempt
	// clock.
	At              time.Time `json:"at,omitempty"`
	Prompt          string    `json:"prompt,omitempty"`          // only with --include-prompts (redacted)
	PromptTruncated bool      `json:"promptTruncated,omitempty"` // prompt hit the size cap
}

// ExportOptions are the `prbot export` filters. A zero value emits every
// attempt, newest fields intact.
type ExportOptions struct {
	Repo           string
	Since          time.Time
	IncludePrompts bool
}

// matchesRepo reports whether a record's "owner/repo" passes the --repo filter.
// The filter matches the bare repo name or the full "owner/repo".
func (o ExportOptions) matchesRepo(repo string) bool {
	if o.Repo == "" {
		return true
	}
	return repo == o.Repo || strings.HasSuffix(repo, "/"+o.Repo)
}

// listLineage reads every recorded #88 lineage file and returns them in a
// deterministic (repo, issue) order. A missing lineage/ prefix is empty.
func (s *EvidenceStore) listLineage() ([]Lineage, error) {
	paths, err := s.sink.List("lineage")
	if err != nil {
		return nil, err
	}
	var out []Lineage
	for _, p := range paths {
		if !strings.HasSuffix(p, ".json") {
			continue
		}
		b, err := s.sink.Read(p)
		if err != nil {
			return nil, err
		}
		var l Lineage
		if err := json.Unmarshal(b, &l); err != nil {
			return nil, fmt.Errorf("lineage %s: %w", p, err)
		}
		if l.Repo == "" || l.Issue <= 0 {
			continue
		}
		out = append(out, l)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Repo != out[j].Repo {
			return out[i].Repo < out[j].Repo
		}
		return out[i].Issue < out[j].Issue
	})
	return out, nil
}

// exportRecords joins each recorded activation (one per attempt) with its
// issue's cycle outcome, applies the filters, and returns records in
// deterministic (repo, issue, attempt) order. All values come from the existing
// redacted store.
func (s *EvidenceStore) exportRecords(opts ExportOptions) ([]ExportRecord, error) {
	lineages, err := s.listLineage()
	if err != nil {
		return nil, err
	}
	var records []ExportRecord
	for _, l := range lineages {
		repoName := l.Repo
		if _, r, ok := strings.Cut(l.Repo, "/"); ok {
			repoName = r
		}
		id := fmt.Sprintf("%s-issue-%d", repoName, l.Issue)
		hasID := validCycleID(id)

		out, hasOutcome := CycleOutcome{}, false
		if hasID {
			if o, ok, err := s.loadOutcomeOptional(id); err != nil {
				return nil, err
			} else {
				out, hasOutcome = o, ok
			}
		}
		at := time.Time{}
		if hasOutcome {
			at = latestConditionTime(out.Conditions)
		}
		if !opts.matchesRepo(l.Repo) || (!opts.Since.IsZero() && at.Before(opts.Since)) {
			continue
		}
		prompt, truncated := "", false
		if opts.IncludePrompts && hasID {
			raw, err := s.loadArtifact(id, evidencePrompt)
			if err != nil {
				return nil, err
			}
			prompt, truncated = capExportPrompt(s.redactor.Redact(raw))
		}
		for _, act := range l.Activations {
			rec := ExportRecord{
				SchemaVersion: exportSchemaVersion,
				Repo:          l.Repo, Issue: l.Issue,
				Attempt: act.Attempt, Reason: act.Reason,
				Harness: act.Harness, Model: act.Model,
				AgentOutcome: act.Result,
				TokensIn:     act.TokensIn, TokensOut: act.TokensOut, CostUsd: act.CostUSD,
				At:     at,
				Prompt: prompt, PromptTruncated: truncated,
			}
			if hasOutcome {
				rec.VerificationOutcome = string(out.VerificationOutcome)
				rec.TerminalState = out.TerminalState
				rec.Conditions = out.Conditions
				rec.PolicyDigest = out.PolicyDigest
				if rec.AgentOutcome == "" {
					rec.AgentOutcome = string(out.AgentOutcome)
				}
			}
			records = append(records, rec)
		}
	}
	sortExportRecords(records)
	return records, nil
}

// capExportPrompt truncates an opted-in prompt to the size cap, reporting
// whether it was cut so a consumer can tell a complete prompt from a capped one.
func capExportPrompt(p string) (string, bool) {
	if len(p) <= exportPromptSizeCap {
		return p, false
	}
	return p[:exportPromptSizeCap], true
}

// sortExportRecords orders records deterministically by (repo, issue, attempt);
// stable so an attempt tie keeps its recorded activation order.
func sortExportRecords(recs []ExportRecord) {
	sort.SliceStable(recs, func(i, j int) bool {
		if recs[i].Repo != recs[j].Repo {
			return recs[i].Repo < recs[j].Repo
		}
		if recs[i].Issue != recs[j].Issue {
			return recs[i].Issue < recs[j].Issue
		}
		return recs[i].Attempt < recs[j].Attempt
	})
}

// runExport is `prbot export [--format jsonl] [--repo NAME] [--since T]
// [--include-prompts]`: it prints one JSONL record per attempt from the
// evidence store (no Temporal). Reads only #83/#87/#88 data — no new storage.
func runExport(args []string, out io.Writer) error {
	opts := ExportOptions{}
	format := "jsonl"
	for i := 0; i < len(args); i++ {
		a := args[i]
		value := func() (string, error) {
			if i+1 >= len(args) {
				return "", fmt.Errorf("%s requires a value", a)
			}
			i++
			return args[i], nil
		}
		switch {
		case a == "--format":
			v, err := value()
			if err != nil {
				return err
			}
			format = v
		case strings.HasPrefix(a, "--format="):
			format = strings.TrimPrefix(a, "--format=")
		case a == "--repo":
			v, err := value()
			if err != nil {
				return err
			}
			opts.Repo = v
		case strings.HasPrefix(a, "--repo="):
			opts.Repo = strings.TrimPrefix(a, "--repo=")
		case a == "--since":
			v, err := value()
			if err != nil {
				return err
			}
			if opts.Since, err = parseExportSince(v); err != nil {
				return err
			}
		case strings.HasPrefix(a, "--since="):
			t, err := parseExportSince(strings.TrimPrefix(a, "--since="))
			if err != nil {
				return err
			}
			opts.Since = t
		case a == "--include-prompts" || a == "--prompts":
			opts.IncludePrompts = true
		default:
			return fmt.Errorf("unknown export flag %q", a)
		}
	}
	if format != "jsonl" {
		return fmt.Errorf("unsupported --format %q (supported: jsonl)", format)
	}
	store, err := NewEvidenceStore(appCfg.Evidence, &defaultRedactor)
	if err != nil {
		return err
	}
	records, err := store.exportRecords(opts)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(out)
	for _, r := range records {
		if err := enc.Encode(r); err != nil {
			return err
		}
	}
	return nil
}

// parseExportSince accepts an RFC3339 timestamp or a YYYY-MM-DD date.
func parseExportSince(s string) (time.Time, error) {
	for _, layout := range []string{time.RFC3339, "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid --since %q (use RFC3339 or YYYY-MM-DD)", s)
}
