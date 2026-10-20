package konoha

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
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

// EvidenceSink is the pluggable, domain-free storage backend for cycle
// evidence (#100): it reads/writes/lists opaque bytes at a store-root-relative
// slash path. Redaction and path semantics belong to the EvidenceStore; a sink
// only moves bytes, so an S3/MinIO implementation slots in without touching the
// store. That remote sink is the documented DEFERRED seam — not implemented
// here (YAGNI until retention/durability requires it); the local-directory sink
// is the default and must stay byte-identical to the #83 layout.
type EvidenceSink interface {
	// Write stores data at relPath, creating parents as needed.
	Write(relPath string, data []byte) error
	// Read returns the bytes at relPath, or an error satisfying
	// os.IsNotExist when the object is absent.
	Read(relPath string) ([]byte, error)
	// List returns the store-root-relative slash paths of every object under
	// prefix (sorted). A missing prefix is an empty result, never an error.
	List(prefix string) ([]string, error)
}

// localDirSink is the default EvidenceSink: a root directory on the local
// filesystem. Writes are atomic (temp + rename) and owner-only (0600), exactly
// the #83 contract.
type localDirSink struct {
	root string
}

// newEvidenceSink resolves the configured backend into a sink. Only the
// local-directory sink exists (#100); unknown names fail closed. Load-time
// config validation already rejects them, so this is defense in depth.
func newEvidenceSink(cfg EvidenceConfig) (EvidenceSink, error) {
	switch cfg.sinkName() {
	case evidenceSinkLocal:
		return &localDirSink{root: filepath.Clean(cfg.Dir)}, nil
	default:
		return nil, fmt.Errorf("evidence.sink: unknown backend %q (supported: %s)", cfg.Sink, evidenceSinkLocal)
	}
}

// abs maps a store-root-relative slash path to a local path, rejecting any path
// that escapes the root (traversal defense in depth).
func (s *localDirSink) abs(rel string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(rel))
	if rel == "" || clean == "." || filepath.IsAbs(clean) ||
		clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("evidence path %q escapes sink root", rel)
	}
	return filepath.Join(s.root, clean), nil
}

func (s *localDirSink) Write(relPath string, data []byte) error {
	p, err := s.abs(relPath)
	if err != nil {
		return err
	}
	return writeFileAtomic(p, data)
}

func (s *localDirSink) Read(relPath string) ([]byte, error) {
	p, err := s.abs(relPath)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(p)
}

func (s *localDirSink) List(prefix string) ([]string, error) {
	dir, err := s.abs(prefix)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, nil
	}
	var out []string
	err = filepath.WalkDir(dir, func(p string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(s.root, p)
		if rerr != nil {
			return rerr
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

// EvidenceStore writes one cycle's artifacts under root/cycles/<id>/ (or
// root/dryrun/cycles/<id>/ under §42 DryRun). All raw I/O goes through the
// EvidenceSink seam (#100); the store owns redaction, path validation, and the
// artifact layout.
type EvidenceStore struct {
	sink     EvidenceSink
	redactor *Redactor
	root     string // local root, for operator-facing error messages only
}

// NewEvidenceStore builds a store for the resolved evidence config; a nil
// redactor uses the process-wide redactor so registered operator secrets are
// masked.
func NewEvidenceStore(cfg EvidenceConfig, r *Redactor) (*EvidenceStore, error) {
	dir := strings.TrimSpace(cfg.Dir)
	if dir == "" {
		dir = defaultEvidenceDir
	}
	cfg.Dir = dir
	if r == nil {
		r = &defaultRedactor
	}
	sink, err := newEvidenceSink(cfg)
	if err != nil {
		return nil, err
	}
	return &EvidenceStore{sink: sink, redactor: r, root: filepath.Clean(dir)}, nil
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

// relDirFor resolves the sink-relative directory for a cycle, rejecting
// traversal (slash path so it is backend-neutral).
func (s *EvidenceStore) relDirFor(id string, dryRun bool) (string, error) {
	if !validCycleID(id) {
		return "", fmt.Errorf("evidence cycle id %q is not a plain path segment", id)
	}
	if dryRun {
		return path.Join("dryrun", "cycles", id), nil
	}
	return path.Join("cycles", id), nil
}

// Write redacts and atomically writes one artifact. Empty content is skipped so
// a later phase never erases an earlier artifact.
// dirFor resolves a cycle's store-relative directory: the §42 dryrun/ namespace
// when that is where the run wrote, else the normal one. Resolution is by what
// exists on disk, never by live config, so a later dryRun flip cannot repoint a
// finished cycle (#141).
func (s *EvidenceStore) dirFor(id string) (string, bool, error) {
	rel, err := s.relDirFor(id, false)
	if err != nil {
		return "", false, err
	}
	if paths, lerr := s.sink.List(rel); lerr == nil && len(paths) > 0 {
		return rel, false, nil
	}
	dry, err := s.relDirFor(id, true)
	if err != nil {
		return "", false, err
	}
	return dry, true, nil
}

func (s *EvidenceStore) Write(id, name, content string, dryRun bool) error {
	if content == "" {
		return nil
	}
	rel, err := s.relDirFor(id, dryRun)
	if err != nil {
		return err
	}
	return s.sink.Write(path.Join(rel, name), []byte(s.redactor.Redact(content)))
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
	rel, err := s.relDirFor(id, dryRun)
	if err != nil {
		return err
	}
	return s.sink.Write(path.Join(rel, "verification", gateID+"."+stream),
		[]byte(s.redactor.Redact(content)))
}

// ensureSet creates the full artifact set (empty files where absent) so one
// cycle always lands the documented layout, without truncating existing data.
func (s *EvidenceStore) ensureSet(id string, dryRun bool) error {
	rel, err := s.relDirFor(id, dryRun)
	if err != nil {
		return err
	}
	for _, name := range evidenceArtifactNames {
		p := path.Join(rel, name)
		if _, err := s.sink.Read(p); err == nil {
			continue
		} else if !os.IsNotExist(err) {
			return err
		}
		if err := s.sink.Write(p, nil); err != nil {
			return err
		}
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
	store, err := NewEvidenceStore(appCfg.Evidence, &defaultRedactor)
	if err != nil {
		return err
	}
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
