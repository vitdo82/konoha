package konoha

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Durable per-cycle evidence store (#83). Temporal history ages out, so this is
// the on-disk, append-only, redacted proof of what an agent was asked, what it
// emitted, the diff, the validation result, and the terminal outcome. Evidence
// is BEST-EFFORT: a write failure is recorded on the CycleOutcome and logged,
// but never changes the authoritative §6 terminal state (§40). It is never
// authoritative over Temporal state (§44 #20).

// Artifact file names (byte-exact contract; tests and README reference them).
const (
	evidencePrompt       = "prompt.txt"
	evidenceStdout       = "agent.stdout"
	evidenceStderr       = "agent.stderr"
	evidenceResult       = "result.json"
	evidenceDiff         = "diff.patch"
	evidenceValidation   = "validation.txt"
	evidenceOutcome      = "outcome.json"
	evidenceProvisioning = "provisioning.json" // #96 optional capability outcomes
)

// evidenceArtifactNames is the full set every cycle directory carries (ensured
// to exist even when a phase produced no content for one of them).
var evidenceArtifactNames = []string{
	evidencePrompt, evidenceStdout, evidenceStderr,
	evidenceResult, evidenceDiff, evidenceValidation, evidenceOutcome,
	evidenceProvisioning,
}

// AgentEvidence is the raw artifact set an agent activity captures for the
// evidence store. Redaction happens at write time, never here.
type AgentEvidence struct {
	Prompt string `json:"prompt,omitempty"`
	Stdout string `json:"stdout,omitempty"`
	Stderr string `json:"stderr,omitempty"`
	// Provisioning is the #96 optional-capability outcome set (binary/catalog/
	// version/mcp), each APPLIED or SKIPPED (never a false success, §40).
	Provisioning []CapabilityOutcome `json:"provisioning,omitempty"`
}

// evidenceOf dereferences an optional AgentEvidence for a nil-safe read.
func evidenceOf(ev *AgentEvidence) AgentEvidence {
	if ev == nil {
		return AgentEvidence{}
	}
	return *ev
}

// EvidenceStore writes one cycle's artifacts under root/cycles/<id>/ (or
// root/dryrun/cycles/<id>/ under §42 DryRun).
type EvidenceStore struct {
	root     string
	redactor *Redactor
}

// NewEvidenceStore roots a store at dir; a nil redactor uses the process-wide
// redactor so registered operator secrets are masked.
func NewEvidenceStore(dir string, r *Redactor) *EvidenceStore {
	if strings.TrimSpace(dir) == "" {
		dir = defaultEvidenceDir
	}
	if r == nil {
		r = &defaultRedactor
	}
	return &EvidenceStore{root: filepath.Clean(dir), redactor: r}
}

// validCycleID reports whether id is a single plain path segment (path
// traversal guard, #83): non-empty, not "."/"..", no separators, no NUL.
func validCycleID(id string) bool {
	if id == "" || id == "." || id == ".." || strings.ContainsRune(id, 0) {
		return false
	}
	if strings.ContainsAny(id, `/\`) || id != filepath.Base(id) {
		return false
	}
	return true
}

// dirFor resolves the on-disk directory for a cycle, rejecting traversal.
func (s *EvidenceStore) dirFor(id string, dryRun bool) (string, error) {
	if !validCycleID(id) {
		return "", fmt.Errorf("evidence cycle id %q is not a plain path segment", id)
	}
	dir := filepath.Join(s.root, "cycles", id)
	if dryRun {
		dir = filepath.Join(s.root, "dryrun", "cycles", id)
	}
	// Defense in depth: the cleaned path must stay under the root.
	rel, err := filepath.Rel(s.root, dir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("evidence path %q escapes root %q", dir, s.root)
	}
	return dir, nil
}

// Write redacts and atomically writes one artifact. Empty content is skipped so
// a later phase never erases an earlier artifact.
func (s *EvidenceStore) Write(id, name, content string, dryRun bool) error {
	if content == "" {
		return nil
	}
	dir, err := s.dirFor(id, dryRun)
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(dir, name), []byte(s.redactor.Redact(content)))
}

// WriteVerification redacts and writes one §26 gate stream under the cycle's
// verification/ subdir as <gateID>.<stream> (stream = stdout|stderr). The gate
// id must be a plain path segment, so a hostile config can never escape the
// cycle directory (the config loader already enforces this; defense in depth).
func (s *EvidenceStore) WriteVerification(id, gateID, stream, content string, dryRun bool) error {
	if content == "" {
		return nil
	}
	if !validCycleID(gateID) {
		return fmt.Errorf("gate id %q is not a plain path segment", gateID)
	}
	dir, err := s.dirFor(id, dryRun)
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(dir, "verification", gateID+"."+stream),
		[]byte(s.redactor.Redact(content)))
}

// ensureSet creates the full artifact set (empty files where absent) so one
// cycle always lands the documented layout, without truncating existing data.
func (s *EvidenceStore) ensureSet(id string, dryRun bool) error {
	dir, err := s.dirFor(id, dryRun)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	for _, name := range evidenceArtifactNames {
		p := filepath.Join(dir, name)
		if _, err := os.Stat(p); err == nil {
			continue
		}
		f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		f.Close()
	}
	return nil
}

// writeFileAtomic writes data via a temp file + rename, owner-only perms.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // no-op once renamed
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// EvidenceRequest carries the artifacts available at one point in a cycle.
// Empty fields are skipped, so each phase contributes what it has and a later
// phase never erases an earlier artifact.
type EvidenceRequest struct {
	CycleID      string `json:"cycleId"`
	DryRun       bool   `json:"dryRun,omitempty"`
	Prompt       string `json:"prompt,omitempty"`
	Stdout       string `json:"stdout,omitempty"`
	Stderr       string `json:"stderr,omitempty"`
	Result       string `json:"result,omitempty"`
	Diff         string `json:"diff,omitempty"`
	Validation   string `json:"validation,omitempty"`
	Outcome      string `json:"outcome,omitempty"`
	Provisioning string `json:"provisioning,omitempty"` // #96 capability outcomes JSON
}

// WriteCycleEvidence is the single evidence I/O activity (#83): it writes the
// provided artifacts redacted + atomically under evidence.dir. It returns an
// error so the caller can record it on the outcome, but the workflow treats it
// as non-fatal (§40). Never call activity.GetLogger here: unit tests use a
// non-Temporal context (it panics).
func WriteCycleEvidence(_ context.Context, in EvidenceRequest) error {
	store := NewEvidenceStore(appCfg.Evidence.Dir, &defaultRedactor)
	for _, a := range []struct{ name, content string }{
		{evidencePrompt, in.Prompt},
		{evidenceStdout, in.Stdout},
		{evidenceStderr, in.Stderr},
		{evidenceResult, in.Result},
		{evidenceDiff, in.Diff},
		{evidenceValidation, in.Validation},
		{evidenceOutcome, in.Outcome},
		{evidenceProvisioning, in.Provisioning},
	} {
		if a.content == "" {
			continue
		}
		if err := store.Write(in.CycleID, a.name, a.content, in.DryRun); err != nil {
			return err
		}
	}
	return store.ensureSet(in.CycleID, in.DryRun)
}

// runCapture runs cmd capturing stdout and stderr separately (so evidence can
// distinguish them) while callers that parse agent output still get the
// combined stream.
func runCapture(cmd *exec.Cmd) (stdout, stderr string, err error) {
	// A harness in stdin_file mode opens the prompt file for the child's stdin;
	// close it once the run finishes (never os.Stdin).
	if f, ok := cmd.Stdin.(*os.File); ok && f != os.Stdin {
		defer f.Close()
	}
	var ob, eb bytes.Buffer
	cmd.Stdout = &ob
	cmd.Stderr = &eb
	err = cmd.Run()
	return ob.String(), eb.String(), err
}

// evidenceJSON renders a result/outcome value as JSON for the evidence store.
// Pure and deterministic (json.Marshal orders struct fields by definition), so
// it is safe to call from workflow code.
func evidenceJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

// evidenceResultJSON renders an agent result without its raw Evidence (the
// streams already land in their own artifacts, so result.json stays the clean
// validated result and the Temporal payload is not duplicated).
func evidenceResultJSON(v any) string {
	switch r := v.(type) {
	case DevelopResult:
		r.Evidence = nil
		return evidenceJSON(r)
	case ReviewResult:
		r.Evidence = nil
		return evidenceJSON(r)
	default:
		return evidenceJSON(v)
	}
}

// writeAgentEvidence ships one agent result (prompt/streams + result.json +
// validation) to the evidence store via the caller's write closure. Shared by
// the develop/repair evidence points.
func writeAgentEvidence(write func(EvidenceRequest), ev *AgentEvidence, result any, validation string) {
	e := evidenceOf(ev)
	provisioning := ""
	if len(e.Provisioning) > 0 {
		provisioning = evidenceJSON(e.Provisioning)
	}
	write(EvidenceRequest{
		Prompt: e.Prompt, Stdout: e.Stdout, Stderr: e.Stderr,
		Result: evidenceResultJSON(result), Validation: validation,
		Provisioning: provisioning,
	})
}
