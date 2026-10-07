package konoha

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func exportTime(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse time %q: %v", s, err)
	}
	return ts
}

// seedExport writes a small evidence store: repo o/r has issue 7 (develop +
// repair, terminal CLOSED) and issue 3 (develop only, HUMAN_REQUIRED); repo
// o/other has issue 9 (develop only). Used across the export tests.
func seedExport(t *testing.T) string {
	t.Helper()
	root := withEvidenceDir(t)
	ctx := context.Background()

	act := func(owner, repo string, issue int, a LineageActivation) {
		if err := RecordActivation(ctx, ActivationRequest{Owner: owner, Repo: repo, Issue: issue, Activation: a}); err != nil {
			t.Fatalf("RecordActivation: %v", err)
		}
	}
	act("o", "r", 7, LineageActivation{
		Attempt: 1, WorkflowID: "r-issue-7", Reason: activationReasonInitial,
		Harness: "opencode", Model: "m1", Result: "SUCCESS",
		TokensIn: 10, TokensOut: 5, CostUSD: 0.25,
	})
	act("o", "r", 7, LineageActivation{
		Attempt: 2, WorkflowID: "r-issue-7-repair-2", Reason: activationReasonRepair,
		Harness: "opencode", Model: "m1", Result: "SUCCESS",
		TokensIn: 1, TokensOut: 1, CostUSD: 0.5,
	})
	act("o", "r", 3, LineageActivation{
		Attempt: 1, WorkflowID: "r-issue-3", Reason: activationReasonInitial,
		Harness: "opencode", Model: "m1", Result: "SUCCESS",
	})
	act("o", "other", 9, LineageActivation{
		Attempt: 1, WorkflowID: "other-issue-9", Reason: activationReasonInitial,
		Harness: "generic", Model: "m2", Result: "BLOCKED",
	})

	outcome := func(id, state string, at time.Time, digest string) {
		o := CycleOutcome{
			WorkflowID: id, Owner: "o", Issue: 0, TerminalState: state,
			AgentOutcome: agentOutcomeSuccess, VerificationOutcome: verificationSuccess,
			PolicyDigest: digest,
			Conditions:   []Condition{{Type: conditionTerminal, Status: state, At: at}},
		}
		if err := WriteCycleEvidence(ctx, EvidenceRequest{
			CycleID: id, Outcome: evidenceJSON(o),
		}); err != nil {
			t.Fatalf("WriteCycleEvidence: %v", err)
		}
	}
	outcome("r-issue-7", stateClosed, exportTime(t, "2026-03-01T00:00:00Z"), strings.Repeat("a", 64))
	outcome("r-issue-3", stateHumanRequired, exportTime(t, "2026-01-01T00:00:00Z"), strings.Repeat("b", 64))
	outcome("other-issue-9", stateBlocked, exportTime(t, "2026-04-01T00:00:00Z"), strings.Repeat("c", 64))
	return root
}

func exportRecords(t *testing.T, args ...string) []map[string]any {
	t.Helper()
	var buf bytes.Buffer
	if err := runExport(args, &buf); err != nil {
		t.Fatalf("runExport(%v): %v", args, err)
	}
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("record line is not JSON: %q: %v", line, err)
		}
		out = append(out, rec)
	}
	return out
}

// #125/AC: one record per attempt, deterministic (repo, issue, attempt) order,
// each carrying the defined fields + schema version, and the filters narrow it.
func TestExportRecordsPerAttemptOrdered(t *testing.T) {
	seedExport(t)

	recs := exportRecords(t, "--format", "jsonl")
	if len(recs) != 4 {
		t.Fatalf("want one record per attempt (4), got %d: %+v", len(recs), recs)
	}
	for _, r := range recs {
		if r["schemaVersion"] != float64(exportSchemaVersion) {
			t.Fatalf("schemaVersion = %v, want %d", r["schemaVersion"], exportSchemaVersion)
		}
		if _, ok := r["prompt"]; ok {
			t.Fatalf("prompts must be excluded by default: %+v", r)
		}
	}

	// Deterministic (repo, issue, attempt) order.
	key := func(r map[string]any) string {
		return strings.Join([]string{
			r["repo"].(string),
			itoa(r["issue"]),
			itoa(r["attempt"]),
		}, "|")
	}
	want := []string{"o/other|9|1", "o/r|3|1", "o/r|7|1", "o/r|7|2"}
	for i, w := range want {
		if got := key(recs[i]); got != w {
			t.Fatalf("record %d = %s, want %s (order must be deterministic)", i, got, w)
		}
	}

	first := recs[2]
	if first["reason"] != "initial" || first["harness"] != "opencode" || first["model"] != "m1" {
		t.Fatalf("attempt-1 provenance wrong: %+v", first)
	}
	if first["agentOutcome"] != "SUCCESS" || first["verificationOutcome"] != "SUCCESS" || first["terminalState"] != stateClosed {
		t.Fatalf("attempt-1 outcomes wrong: %+v", first)
	}
	if itoa(first["tokensIn"]) != "10" || itoa(first["tokensOut"]) != "5" || first["costUsd"] != 0.25 {
		t.Fatalf("attempt-1 usage wrong: %+v", first)
	}
	if first["policyDigest"] != strings.Repeat("a", 64) {
		t.Fatalf("attempt-1 policyDigest wrong: %+v", first)
	}
	if recs[3]["reason"] != "repair" || recs[3]["attempt"] != float64(2) {
		t.Fatalf("attempt-2 record wrong: %+v", recs[3])
	}

	if got := exportRecords(t, "--repo", "r"); len(got) != 3 {
		t.Fatalf("--repo r want 3 records, got %d", len(got))
	}
	if got := exportRecords(t, "--repo", "o/other"); len(got) != 1 {
		t.Fatalf("--repo o/other want 1 record, got %d", len(got))
	}
	if got := exportRecords(t, "--since", "2026-02-15T00:00:00Z"); len(got) != 3 {
		t.Fatalf("--since want 3 records, got %d", len(got))
	}
	if err := runExport([]string{"--format", "xml"}, &bytes.Buffer{}); err == nil {
		t.Fatal("unknown --format must fail closed")
	}
}

// #125/AC: records are redacted, prompts are excluded unless opted in, and the
// opt-in prompt is redacted and size-capped.
func TestExportPromptOptInRedactedAndCapped(t *testing.T) {
	withEvidenceDir(t)
	ctx := context.Background()
	secret := "ghp_" + strings.Repeat("A", 20)
	prompt := "please review " + secret + " " + strings.Repeat("x", exportPromptSizeCap+500)
	if err := WriteCycleEvidence(ctx, EvidenceRequest{CycleID: "r-issue-7", Prompt: prompt}); err != nil {
		t.Fatal(err)
	}
	if err := RecordActivation(ctx, ActivationRequest{
		Owner: "o", Repo: "r", Issue: 7,
		Activation: LineageActivation{Attempt: 1, WorkflowID: "w", Reason: activationReasonInitial},
	}); err != nil {
		t.Fatal(err)
	}

	base := exportRecords(t, "--format", "jsonl")
	if len(base) != 1 {
		t.Fatalf("want 1 record, got %d", len(base))
	}
	if _, ok := base[0]["prompt"]; ok {
		t.Fatal("prompt must be excluded without --include-prompts")
	}

	withPrompt := exportRecords(t, "--format", "jsonl", "--include-prompts")
	got, _ := withPrompt[0]["prompt"].(string)
	if got == "" {
		t.Fatal("--include-prompts must include the prompt")
	}
	if strings.Contains(got, secret) {
		t.Fatalf("prompt must be redacted, got %q", got)
	}
	if !strings.Contains(got, redacted) {
		t.Fatalf("prompt must carry the redaction marker, got %q", got)
	}
	if len(got) > exportPromptSizeCap {
		t.Fatalf("prompt must be capped at %d bytes, got %d", exportPromptSizeCap, len(got))
	}
	if withPrompt[0]["promptTruncated"] != true {
		t.Fatalf("a capped prompt must set promptTruncated: %+v", withPrompt[0])
	}
}

func itoa(v any) string {
	return fmt.Sprintf("%v", v)
}
