package konoha

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"go.temporal.io/sdk/client"
)

// Operator-facing CLI surface (#99): a `doctor` preflight plus read-only
// inspection over the durable evidence store. `doctor` validates config,
// Temporal, `gh`, the harness executable and the evidence dir, exiting non-zero
// with a specific message per failure mode. `logs`/`describe` read the evidence
// store and need no Temporal server; only `status` (#89) queries the workflow.

// doctorLookPath / doctorRunCommand / doctorTemporalHealth / doctorWritable are
// the external seams the doctor checks run through, so tests drive every failure
// mode deterministically (mirrors the ghRunner/ghTokenRunner pattern).
var (
	doctorLookPath   = exec.LookPath
	doctorRunCommand = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return exec.CommandContext(ctx, name, args...).CombinedOutput()
	}
	doctorTemporalHealth = func(ctx context.Context, addr string) error {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		c, err := client.Dial(client.Options{HostPort: addr})
		if err != nil {
			return fmt.Errorf("temporal %s unreachable: %w", addr, err)
		}
		defer c.Close()
		if _, err := c.CheckHealth(ctx, &client.CheckHealthRequest{}); err != nil {
			return fmt.Errorf("temporal %s unreachable: %w", addr, err)
		}
		return nil
	}
	doctorWritable = writableDir
)

// doctorCheck is one named preflight check; a non-nil err is a specific failure.
type doctorCheck struct {
	name string
	err  error
}

// runDoctorChecks runs every preflight check in order. It never stops at the
// first failure (an operator sees the whole picture); the caller decides the exit
// code. Pure inputs (paths/addr) keep it testable without a running worker.
func runDoctorChecks(ctx context.Context, appPath, reposPath, addr string) []doctorCheck {
	cfg, appErr := loadAppConfig(appPath)
	checks := []doctorCheck{{"konoha.yaml", appErr}}

	repos, repoErr := loadReposConfigWithResources(reposPath)
	checks = append(checks, doctorCheck{"repos.yaml", repoErr})

	if appErr == nil {
		checks = append(checks, doctorCheck{"harness", doctorHarnessCheck(cfg, repos)})
	}

	dir := defaultEvidenceDir
	if appErr == nil {
		dir = cfg.Evidence.Dir
	}
	checks = append(checks, doctorCheck{"evidence dir", doctorWritable(dir)})

	checks = append(checks, doctorCheck{"gh", ghDoctorCheck(ctx)})
	checks = append(checks, doctorCheck{"temporal", doctorTemporalHealth(ctx, addr)})
	return checks
}

// runDoctor is the `prbot doctor` entrypoint: it prints every check and returns a
// non-nil error when any failed, so Main exits non-zero with a specific message.
func runDoctor() error {
	addr := os.Getenv("TEMPORAL_ADDRESS")
	if addr == "" {
		addr = "127.0.0.1:7233"
	}
	checks := runDoctorChecks(context.Background(), defaultAppConfigPath(), defaultReposPath(), addr)
	var failed []string
	for _, c := range checks {
		if c.err != nil {
			fmt.Fprintf(os.Stderr, "doctor: %s: %v\n", c.name, c.err)
			failed = append(failed, c.name)
			continue
		}
		fmt.Printf("doctor: %s: ok\n", c.name)
	}
	if len(failed) > 0 {
		return fmt.Errorf("doctor: %d check(s) failed: %s", len(failed), strings.Join(failed, ", "))
	}
	fmt.Println("doctor: all checks passed")
	return nil
}

// doctorHarnessCheck verifies every harness a repo (or the default) selects has
// an executable that resolves on PATH. A name with no executable configured
// (e.g. an unset generic harness) fails too.
func doctorHarnessCheck(cfg AppConfig, repos []RepoConfig) error {
	reg, _ := newHarnessRegistry(opencodeHarness{}, genericHarness{cfg: cfg.Harness})
	def := strings.ToLower(strings.TrimSpace(cfg.Defaults.Lifecycle.Harness))
	if def == "" {
		def = defaultHarnessName
	}
	names := map[string]bool{def: true}
	for _, r := range repos {
		if r.Lifecycle == nil {
			continue
		}
		if n := strings.ToLower(strings.TrimSpace(r.Lifecycle.Harness)); n != "" {
			names[n] = true
		}
	}
	ordered := make([]string, 0, len(names))
	for n := range names {
		ordered = append(ordered, n)
	}
	sort.Strings(ordered)
	for _, name := range ordered {
		h, err := reg.Lookup(name)
		if err != nil {
			return err
		}
		exe := strings.TrimSpace(h.Executable())
		if exe == "" {
			return fmt.Errorf("harness %q has no executable configured", name)
		}
		if _, err := doctorLookPath(exe); err != nil {
			return fmt.Errorf("harness %q executable %q not found on PATH", name, exe)
		}
	}
	return nil
}

// ghDoctorCheck reports a missing `gh` binary and an unauthenticated `gh`
// distinctly, so the failure message names the actual fix.
func ghDoctorCheck(ctx context.Context) error {
	if _, err := doctorLookPath("gh"); err != nil {
		return fmt.Errorf("gh CLI not found on PATH")
	}
	out, err := doctorRunCommand(ctx, "gh", "auth", "status")
	if err != nil {
		detail := strings.TrimSpace(string(out))
		if detail == "" {
			detail = err.Error()
		}
		return fmt.Errorf("gh CLI not authenticated: %s", detail)
	}
	return nil
}

// writableDir proves the evidence dir is usable: it creates the directory if
// needed and round-trips a temp file. Any failure is a specific message.
func writableDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("evidence dir %s not writable: %w", dir, err)
	}
	f, err := os.CreateTemp(dir, ".doctor-*")
	if err != nil {
		return fmt.Errorf("evidence dir %s not writable: %w", dir, err)
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
	return nil
}

// repoIssueArgs parses the `<repo> <issue>` tail shared by logs/describe; a
// missing or invalid argument yields the command's usage message.
func repoIssueArgs(args []string, cmd string) (string, int, error) {
	if len(args) < 2 {
		return "", 0, fmt.Errorf("usage: prbot %s <repo> <issue>", cmd)
	}
	var n int
	fmt.Sscan(args[1], &n)
	if strings.TrimSpace(args[0]) == "" || n <= 0 {
		return "", 0, fmt.Errorf("usage: prbot %s <repo> <issue>", cmd)
	}
	return args[0], n, nil
}

// runLogs is `prbot logs <repo> <issue>`: it prints the cycle's agent
// stdout/stderr and validation artifacts from the evidence store (no Temporal).
func runLogs(args []string, out io.Writer) error {
	repo, n, err := repoIssueArgs(args, "logs")
	if err != nil {
		return err
	}
	id := fmt.Sprintf("%s-issue-%d", repo, n)
	store := NewEvidenceStore(appCfg.Evidence.Dir, &defaultRedactor)
	printed := false
	for _, name := range []string{evidenceStdout, evidenceStderr, evidenceValidation} {
		content, err := store.loadArtifact(id, name)
		if err != nil {
			return err
		}
		if strings.TrimSpace(content) == "" {
			continue
		}
		fmt.Fprintf(out, "===== %s =====\n%s", name, content)
		if !strings.HasSuffix(content, "\n") {
			fmt.Fprintln(out)
		}
		printed = true
	}
	if !printed {
		return fmt.Errorf("no evidence for %s under %s", id, appCfg.Evidence.Dir)
	}
	return nil
}

// runDescribeOutcome is `prbot describe <repo> <issue>`: a human-readable
// summary of the cycle's outcome.json (state, outcomes, conditions, usage),
// read from the evidence store with no Temporal server.
func runDescribeOutcome(args []string, out io.Writer) error {
	repo, n, err := repoIssueArgs(args, "describe")
	if err != nil {
		return err
	}
	id := fmt.Sprintf("%s-issue-%d", repo, n)
	store := NewEvidenceStore(appCfg.Evidence.Dir, &defaultRedactor)
	o, err := store.loadOutcome(id)
	if err != nil {
		return err
	}
	_, err = io.WriteString(out, summarizeOutcome(o))
	return err
}

// loadArtifact reads one artifact for a cycle; a missing file is an empty string
// (a phase may not have produced it), never an error.
func (s *EvidenceStore) loadArtifact(id, name string) (string, error) {
	dir, err := s.dirFor(id, false)
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(filepath.Join(dir, name))
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// loadOutcome reads + parses outcome.json for one cycle. A missing record is a
// specific "no evidence" error (an empty outcome would be misleading).
func (s *EvidenceStore) loadOutcome(id string) (CycleOutcome, error) {
	dir, err := s.dirFor(id, false)
	if err != nil {
		return CycleOutcome{}, err
	}
	b, err := os.ReadFile(filepath.Join(dir, evidenceOutcome))
	if os.IsNotExist(err) {
		return CycleOutcome{}, fmt.Errorf("no evidence for %s under %s", id, s.root)
	}
	if err != nil {
		return CycleOutcome{}, err
	}
	var o CycleOutcome
	if err := json.Unmarshal(b, &o); err != nil {
		return CycleOutcome{}, fmt.Errorf("outcome %s: %w", id, err)
	}
	return o, nil
}

// summarizeOutcome renders the operator-facing view of one cycle outcome: the
// terminal state (authoritative, §44 #20), the descriptive agent/verification
// outcomes, route/attempt facts, usage, and the ordered conditions. Pure.
func summarizeOutcome(o CycleOutcome) string {
	var b strings.Builder
	model := o.WorkflowID
	if model == "" {
		model = fmt.Sprintf("%s/%s#%d", o.Owner, o.Repo, o.Issue)
	}
	fmt.Fprintf(&b, "cycle: %s\n", model)
	fmt.Fprintf(&b, "state: %s\n", orNone(o.TerminalState))
	fmt.Fprintf(&b, "agent: %s\n", orNone(string(o.AgentOutcome)))
	fmt.Fprintf(&b, "verification: %s\n", orNone(string(o.VerificationOutcome)))
	if o.Route != "" || o.Complexity > 0 {
		fmt.Fprintf(&b, "route: %s complexity: %d confidence: %s\n",
			orNone(o.Route), o.Complexity, orNone(o.Confidence))
	}
	fmt.Fprintf(&b, "attempts: %d reviewRounds: %d disagreements: %d\n",
		o.Attempts, o.ReviewRounds, o.Disagreements)
	if o.DurationSeconds > 0 {
		fmt.Fprintf(&b, "duration: %.1fs\n", o.DurationSeconds)
	}
	if !o.Usage.IsZero() {
		fmt.Fprintf(&b, "usage: tokens_in=%d tokens_out=%d cost_usd=%.4f\n",
			o.Usage.TokensIn, o.Usage.TokensOut, o.Usage.CostUSD)
	}
	if o.EscalationReason != "" {
		fmt.Fprintf(&b, "escalation: %s\n", o.EscalationReason)
	}
	if len(o.Conditions) > 0 {
		b.WriteString("conditions:\n")
		for _, c := range o.Conditions {
			fmt.Fprintf(&b, "  - %s %s", c.Type, c.Status)
			if c.Reason != "" {
				fmt.Fprintf(&b, " reason=%s", c.Reason)
			}
			if c.Message != "" {
				fmt.Fprintf(&b, " message=%q", c.Message)
			}
			b.WriteString("\n")
		}
	}
	return b.String()
}

// orNone renders an empty field as "-" so the summary lines are always present.
func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}
