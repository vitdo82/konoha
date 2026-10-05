package konoha

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
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

// evidenceDigest returns a sorted (relative-path, perms, sha256) fingerprint of
// every file under root, so two trees can be compared byte-for-byte.
func evidenceDigest(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		out = append(out, fmt.Sprintf("%s|%o|%x", filepath.ToSlash(rel), info.Mode().Perm(), sum))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

// #100: the local sink's output is byte-identical to the #83 layout — same
// names, same perms (0600), same bytes — for every artifact including the empty
// ensureSet fillers.
func TestEvidenceSinkLocalByteIdentical(t *testing.T) {
	root := withEvidenceDir(t)
	req := EvidenceRequest{
		CycleID: "ducklambda-issue-7",
		Prompt:  "the prompt", Stdout: "the stdout", Stderr: "the stderr",
		Result: `{"verdict":"APPROVE"}`, Diff: "diff --git a/x b/x",
		Validation: "go test ./... ok", Outcome: `{"terminalState":"CLOSED"}`,
		Provisioning: `[{"capability":"binary","outcome":"APPLIED"}]`,
	}
	if err := WriteCycleEvidence(context.Background(), req); err != nil {
		t.Fatalf("WriteCycleEvidence: %v", err)
	}

	// Reproduce the #83 layout independently with the raw atomic writer.
	ref := t.TempDir()
	base := filepath.Join(ref, "cycles", req.CycleID)
	content := map[string]string{
		evidencePrompt: req.Prompt, evidenceStdout: req.Stdout, evidenceStderr: req.Stderr,
		evidenceResult: req.Result, evidenceDiff: req.Diff, evidenceValidation: req.Validation,
		evidenceOutcome: req.Outcome, evidenceProvisioning: req.Provisioning,
	}
	for _, name := range evidenceArtifactNames {
		if err := writeFileAtomic(filepath.Join(base, name), []byte(content[name])); err != nil {
			t.Fatal(err)
		}
	}
	if got, want := evidenceDigest(t, root), evidenceDigest(t, ref); !reflect.DeepEqual(got, want) {
		t.Fatalf("local sink output differs from the #83 layout:\n got=%v\nwant=%v", got, want)
	}
}

// #100: an unknown evidence.sink fails at load; empty and "local" both resolve
// to the local-directory sink (the documented S3/MinIO seam is not implemented).
func TestEvidenceSinkUnknownBackendFailsLoad(t *testing.T) {
	for _, sink := range []string{"s3", "minio", "bogus"} {
		p := writeAppConfig(t, "evidence:\n  dir: .konoha/evidence\n  sink: "+sink+"\n")
		if _, err := loadAppConfig(p); err == nil {
			t.Fatalf("evidence.sink %q must fail at load", sink)
		}
	}
	for _, content := range []string{
		"evidence:\n  dir: .konoha/evidence\n",
		"evidence:\n  dir: .konoha/evidence\n  sink: local\n",
	} {
		cfg, err := loadAppConfig(writeAppConfig(t, content))
		if err != nil {
			t.Fatalf("local/empty sink must load: %v", err)
		}
		if cfg.Evidence.sinkName() != evidenceSinkLocal {
			t.Fatalf("sinkName = %q, want %q", cfg.Evidence.sinkName(), evidenceSinkLocal)
		}
	}
	if _, err := NewEvidenceStore(EvidenceConfig{Dir: t.TempDir(), Sink: "s3"}, nil); err == nil {
		t.Fatal("constructor must reject an unknown sink (defense in depth)")
	}
}
