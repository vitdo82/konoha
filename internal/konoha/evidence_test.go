package konoha

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withEvidenceDir points the resolved config at a temp evidence root and
// restores it after the test.
func withEvidenceDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	saved := appCfg
	appCfg.Evidence.Dir = dir
	t.Cleanup(func() { appCfg = saved })
	return dir
}

func evidencePath(root, id, name string, dryRun bool) string {
	base := filepath.Join(root, "cycles", id)
	if dryRun {
		base = filepath.Join(root, "dryrun", "cycles", id)
	}
	return filepath.Join(base, name)
}

// #83: one cycle lands the documented full artifact set, with the asserted
// artifacts non-empty and owner-only perms.
func TestWriteCycleEvidenceFullArtifactSet(t *testing.T) {
	root := withEvidenceDir(t)
	err := WriteCycleEvidence(context.Background(), EvidenceRequest{
		CycleID:    "ducklambda-issue-7",
		Prompt:     "the prompt",
		Stdout:     "the agent stdout",
		Stderr:     "the agent stderr",
		Result:     `{"verdict":"APPROVE"}`,
		Diff:       "diff --git a/x b/x",
		Validation: "go test ./... ok",
		Outcome:    `{"terminalState":"CLOSED"}`,
	})
	if err != nil {
		t.Fatalf("WriteCycleEvidence: %v", err)
	}
	for _, name := range evidenceArtifactNames {
		if _, err := os.Stat(evidencePath(root, "ducklambda-issue-7", name, false)); err != nil {
			t.Fatalf("artifact %s missing: %v", name, err)
		}
	}
	for _, name := range []string{evidencePrompt, evidenceStdout, evidenceDiff, evidenceOutcome} {
		b, err := os.ReadFile(evidencePath(root, "ducklambda-issue-7", name, false))
		if err != nil || strings.TrimSpace(string(b)) == "" {
			t.Fatalf("artifact %s must be non-empty (err=%v)", name, err)
		}
	}
	info, err := os.Stat(evidencePath(root, "ducklambda-issue-7", evidencePrompt, false))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("evidence perms = %o, want 600 (owner-only)", perm)
	}
}

// #83: cycle IDs that are not plain path segments are rejected; nothing escapes
// the evidence root.
func TestEvidenceRejectsTraversal(t *testing.T) {
	root := withEvidenceDir(t)
	for _, id := range []string{"", ".", "..", "../escape", "a/b", `a\b`, "/abs", "x/../../y"} {
		err := WriteCycleEvidence(context.Background(), EvidenceRequest{CycleID: id, Outcome: "x"})
		if err == nil {
			t.Fatalf("cycle id %q must be rejected", id)
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(root), "escaped")); err == nil {
		t.Fatal("traversal must not write outside the evidence root")
	}
}

// #83/§23: a registered secret echoed by the agent is masked in agent.stdout.
func TestEvidenceRedactsRegisteredSecret(t *testing.T) {
	root := withEvidenceDir(t)
	secret := "registered-evidence-value-42"
	RegisterSecrets(secret)

	err := WriteCycleEvidence(context.Background(), EvidenceRequest{
		CycleID: "ducklambda-issue-7",
		Stdout:  "agent echoed " + secret + " and bearer abcdefghijklmnop1234",
	})
	if err != nil {
		t.Fatalf("WriteCycleEvidence: %v", err)
	}
	b, err := os.ReadFile(evidencePath(root, "ducklambda-issue-7", evidenceStdout, false))
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	if strings.Contains(got, secret) || strings.Contains(got, "abcdefghijklmnop1234") {
		t.Fatalf("secret leaked into evidence: %q", got)
	}
	if !strings.Contains(got, redacted) {
		t.Fatalf("expected %s in evidence, got %q", redacted, got)
	}
}

// §42/#83: DryRun namespaces evidence under dryrun/ (it is not a mutation).
func TestWriteCycleEvidenceDryRunNamespaces(t *testing.T) {
	root := withEvidenceDir(t)
	if err := WriteCycleEvidence(context.Background(), EvidenceRequest{
		CycleID: "ducklambda-issue-7", DryRun: true, Outcome: `{"terminalState":"BLOCKED"}`,
	}); err != nil {
		t.Fatalf("WriteCycleEvidence: %v", err)
	}
	if _, err := os.Stat(evidencePath(root, "ducklambda-issue-7", evidenceOutcome, true)); err != nil {
		t.Fatalf("dry-run evidence missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "cycles", "ducklambda-issue-7")); err == nil {
		t.Fatal("dry-run must not write the non-dryrun path")
	}
}
