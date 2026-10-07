package konoha

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Machine-provider abstraction (#121). Omnara-style execution fabric: a Provider
// creates/reattaches/destroys Machines, and a Machine runs one structured command
// at a time. This is the FOUNDATION the #92 evaluation called for, built only
// after #92 returned "do not adopt a sandbox now" (docs/security.md →
// "Sandboxed execution evaluation"). It is NOT wired into the agent path yet:
// today the local provider reproduces the existing host/worktree behavior
// (#102 ceilings, #103 advisory egress, §23 scrubbed env) and stands as the
// default, and no sandbox provider is added until a revisit trigger fires.
//
// Ownership rule (#121/#142): the WORKFLOW owns infrastructure and is the only
// code that may hold a Provider (Create/Reattach/Destroy/List/Ping). An agent
// only operates inside the Machine it was given and never creates or destroys
// one. The adapter boundary is structural: this file references no GitHub
// runner, no policy gate, no label projection and no reconciliation, so an
// adapter physically cannot bypass claim authority, workflow policy, review
// gates or tracker reconciliation — it can only Execute/Read/Write inside its
// own worktree. An adapter that claims a capability it does not honor fails
// closed at negotiation (a missing capability is SKIPPED, never a false success,
// §40).

// defaultMachineProvider is the built-in provider today. Local host execution is
// the only provider until #92's revisit triggers fire (docs/non-goals.md → A1).
const defaultMachineProvider = "local"

// MachineCapability is one optional capability a provider advertises (#121).
// Closed set: an unknown value is never negotiated.
type MachineCapability string

const (
	machineCapSnapshot MachineCapability = "snapshot"
	machineCapClone    MachineCapability = "clone"
	machineCapSuspend  MachineCapability = "suspend"
	machineCapPreview  MachineCapability = "preview"
)

// machineCapabilities is the closed set of capabilities that can be required.
var machineCapabilities = []MachineCapability{
	machineCapSnapshot, machineCapClone, machineCapSuspend, machineCapPreview,
}

func knownMachineCapability(c MachineCapability) bool {
	for _, k := range machineCapabilities {
		if c == k {
			return true
		}
	}
	return false
}

// Command is one execution request. Argv is the ONLY command form — there is
// deliberately no shell-string field, so no caller can smuggle `sh -c` through
// the interface. Dir/Env default to the Machine's worktree and scrubbed env.
type Command struct {
	Argv      []string
	Dir       string
	Env       []string
	StdinPath string
	Timeout   time.Duration
}

// CommandResult is the outcome of one Execute. ExitCode is the child's code (0
// on success); Limits carries the #102 evidence (nil when no ceiling is set).
type CommandResult struct {
	ExitCode int
	Stdout   string
	Stderr   string
	Limits   *LimitEvidence
}

// Machine is the agent-facing handle: it can run one structured command and
// read/write files inside its own worktree. It can destroy itself, but it can
// never create or reattach another machine (that is the Provider's job).
type Machine interface {
	// ID is the stable, deterministic identity of the machine (the worktree dir).
	ID() string
	Execute(ctx context.Context, cmd Command) (CommandResult, error)
	ReadFile(ctx context.Context, path string) ([]byte, error)
	WriteFile(ctx context.Context, path string, data []byte) error
	Destroy(ctx context.Context) error
}

// Provider is the workflow-only infrastructure handle. An agent never receives
// a Provider: the workflow creates, reattaches to and tears down machines.
type Provider interface {
	Name() string
	// Capabilities advertises the optional capabilities this provider honors.
	// An empty set is honest: nothing is implied by omission.
	Capabilities() []MachineCapability
	Create(ctx context.Context, spec MachineSpec) (Machine, error)
	Reattach(ctx context.Context, spec MachineSpec) (Machine, error)
	Destroy(ctx context.Context, spec MachineSpec) error
	List(ctx context.Context) ([]MachineInfo, error)
	Ping(ctx context.Context) error
}

// MachineSpec identifies one machine. WorkflowID is the ownership claim (the
// same §4.4 claim the worktree carries); RepoPath is the main checkout; Dir is
// the deterministic owned worktree; BaseSHA is the commit to branch from.
type MachineSpec struct {
	WorkflowID string        `json:"workflowId"`
	RepoPath   string        `json:"repoPath"`
	Dir        string        `json:"dir"`
	BaseSHA    string        `json:"baseSha,omitempty"`
	Limits     *LimitsConfig `json:"limits,omitempty"`
}

// MachineInfo is the non-secret identity of a created machine (List output).
type MachineInfo struct {
	ID         string `json:"id"`
	Provider   string `json:"provider"`
	Dir        string `json:"dir"`
	WorkflowID string `json:"workflowId"`
	BaseSHA    string `json:"baseSha,omitempty"`
}

// machineNegotiation is the outcome of capability negotiation. Outcomes is one
// record per requested capability: APPLIED when advertised, else SKIPPED with
// the provider named (never a false success, §40).
type machineNegotiation struct {
	Provider   string
	Advertised []MachineCapability
	Outcomes   []CapabilityOutcome
}

// supports reports whether the provider advertised c.
func (n machineNegotiation) supports(c MachineCapability) bool {
	for _, a := range n.Advertised {
		if a == c {
			return true
		}
	}
	return false
}

// negotiateCapabilities asks p whether it honors each requested capability. A
// capability the provider does not advertise is recorded SKIPPED — the same
// status vocabulary as the #96 harness capabilities, so negotiation never reads
// as success for an unimplemented capability.
func negotiateCapabilities(p Provider, requested []MachineCapability) machineNegotiation {
	adv := p.Capabilities()
	n := machineNegotiation{Provider: p.Name(), Advertised: append([]MachineCapability(nil), adv...)}
	for _, c := range requested {
		if n.supports(c) {
			n.Outcomes = append(n.Outcomes, CapabilityOutcome{Capability: string(c), Status: capabilityApplied, Detail: p.Name()})
			continue
		}
		n.Outcomes = append(n.Outcomes, CapabilityOutcome{
			Capability: string(c),
			Status:     capabilitySkipped,
			Detail:     fmt.Sprintf("provider %q does not advertise capability %q; not applied", p.Name(), c),
		})
	}
	return n
}

// requireCapabilities fails closed when p does not honor any required
// capability: an adapter may not proceed as if it did (§40, §142). Unknown
// capability names are rejected too — the set is closed.
func requireCapabilities(p Provider, required []MachineCapability) error {
	for _, c := range required {
		if !knownMachineCapability(c) {
			return fmt.Errorf("machine provider %q: unknown capability %q", p.Name(), c)
		}
	}
	var missing []string
	for _, c := range required {
		if !negotiateCapabilities(p, []MachineCapability{c}).supports(c) {
			missing = append(missing, string(c))
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("machine provider %q lacks required capability(ies): %s", p.Name(), strings.Join(missing, ", "))
	}
	return nil
}

// machineRegistry is the name-keyed set of built providers (#121). Selection is
// by NAME, never by a caller-supplied executable (mirrors #85/#118). Only
// `local` exists today.
type machineRegistry struct {
	byName map[string]Provider
	def    Provider
}

// newMachineRegistry builds the process registry with the local provider as the
// default.
func newMachineRegistry() *machineRegistry {
	local := newLocalProvider()
	return &machineRegistry{
		byName: map[string]Provider{defaultMachineProvider: local},
		def:    local,
	}
}

// Default returns the default provider (`local`).
func (r *machineRegistry) Default() Provider { return r.def }

// Lookup resolves a provider by name; unknown fails closed.
func (r *machineRegistry) Lookup(name string) (Provider, error) {
	n := strings.ToLower(strings.TrimSpace(name))
	if n == "" {
		return r.def, nil
	}
	p, ok := r.byName[n]
	if !ok {
		return nil, fmt.Errorf("unknown machine provider %q (known: %s)", name, strings.Join(r.Names(), ", "))
	}
	return p, nil
}

// Names returns the sorted provider names.
func (r *machineRegistry) Names() []string {
	out := make([]string, 0, len(r.byName))
	for n := range r.byName {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// localProvider is the default provider: it reproduces today's host/worktree
// behavior exactly. Create materializes an owned git worktree (§4.4 claim),
// Execute runs a structured argv child on the host under the #102 ceilings and
// #103 advisory egress with the §23 scrubbed env, and Destroy removes only a
// worktree whose claim matches the requesting workflow.
type localProvider struct {
	mu       sync.Mutex
	machines map[string]*localMachine
}

func newLocalProvider() *localProvider {
	return &localProvider{machines: make(map[string]*localMachine)}
}

func (p *localProvider) Name() string { return defaultMachineProvider }

// Capabilities is deliberately empty: host worktrees have no provider-level
// snapshot/clone/suspend/preview. Advertising nothing is the honest answer, and
// any requested capability negotiates to SKIPPED (never a false success).
func (p *localProvider) Capabilities() []MachineCapability { return nil }

// Create ensures the deterministic owned worktree exists (claiming it for the
// workflow) and returns a Machine bound to it. A foreign or unclaimed directory
// is refused untouched by the shared §4.4 claim check.
func (p *localProvider) Create(ctx context.Context, spec MachineSpec) (Machine, error) {
	if err := validateMachineSpec(spec); err != nil {
		return nil, err
	}
	dir := filepath.Clean(spec.Dir)
	owner := worktreeOwner{WorkflowID: spec.WorkflowID, BaseSHA: spec.BaseSHA}
	if err := ensureWorktree(ctx, filepath.Clean(spec.RepoPath), dir, owner); err != nil {
		return nil, err
	}
	m := &localMachine{spec: spec, dir: dir, limits: machineLimits(spec)}
	p.mu.Lock()
	p.machines[dir] = m
	p.mu.Unlock()
	return m, nil
}

// Reattach reconnects to an existing owned worktree, verifying the claim matches
// the requesting workflow. A missing/unclaimed/foreign directory fails closed.
func (p *localProvider) Reattach(ctx context.Context, spec MachineSpec) (Machine, error) {
	if err := validateMachineSpec(spec); err != nil {
		return nil, err
	}
	dir := filepath.Clean(spec.Dir)
	owner, ok := readOwner(dir)
	if !ok {
		return nil, fmt.Errorf("cannot reattach %s: no konoha claim marker", dir)
	}
	if owner.WorkflowID != spec.WorkflowID {
		return nil, fmt.Errorf("cannot reattach %s: claimed by workflow %q, not %q", dir, owner.WorkflowID, spec.WorkflowID)
	}
	m := &localMachine{spec: spec, dir: dir, limits: machineLimits(spec)}
	p.mu.Lock()
	p.machines[dir] = m
	p.mu.Unlock()
	return m, nil
}

// Destroy removes a machine's worktree, honoring the §4.4 claim: only a dir
// whose owner matches spec.WorkflowID is touched.
func (p *localProvider) Destroy(ctx context.Context, spec MachineSpec) error {
	if err := validateMachineSpec(spec); err != nil {
		return err
	}
	dir := filepath.Clean(spec.Dir)
	cleanupWorktree(ctx, filepath.Clean(spec.RepoPath), dir, spec.WorkflowID)
	p.mu.Lock()
	delete(p.machines, dir)
	p.mu.Unlock()
	return nil
}

// List returns the machines this provider created, sorted by id (deterministic).
func (p *localProvider) List(ctx context.Context) ([]MachineInfo, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]MachineInfo, 0, len(p.machines))
	for _, m := range p.machines {
		out = append(out, m.info())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Ping reports host reachability. The local provider runs on the host itself, so
// it is reachable by definition; a nil error never implies isolation.
func (p *localProvider) Ping(ctx context.Context) error { return nil }

// localMachine is a host worktree machine (the #121 Machine for localProvider).
type localMachine struct {
	spec   MachineSpec
	dir    string
	limits LimitsConfig
}

func (m *localMachine) ID() string { return m.dir }

func (m *localMachine) info() MachineInfo {
	return MachineInfo{ID: m.dir, Provider: defaultMachineProvider, Dir: m.dir, WorkflowID: m.spec.WorkflowID, BaseSHA: m.spec.BaseSHA}
}

// Execute runs one structured command on the host in the machine's worktree.
// There is no shell: cmd.Argv is exec'd directly. The child env is the scrubbed
// §23 env (GH/cloud credentials removed, PWD pinned) plus the advisory #103
// egress proxy; a caller-supplied env can never re-introduce a stripped
// credential. #102 ceilings are applied best-effort (no-op off Linux).
func (m *localMachine) Execute(ctx context.Context, cmd Command) (CommandResult, error) {
	if len(cmd.Argv) == 0 || strings.TrimSpace(cmd.Argv[0]) == "" {
		return CommandResult{}, fmt.Errorf("machine Execute: argv is required (structured argv, never a shell string)")
	}
	runCtx := ctx
	if cmd.Timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, cmd.Timeout)
		defer cancel()
	}
	dir := filepath.Clean(m.dir)
	if strings.TrimSpace(cmd.Dir) != "" {
		dir = filepath.Clean(cmd.Dir)
	}
	env := machineEnv(dir, cmd.Env)
	child, _ := enforceCommand(runCtx, m.limits, cmd.Argv[0], cmd.Argv[1:], dir, env)
	if strings.TrimSpace(cmd.StdinPath) != "" {
		f, err := os.Open(cmd.StdinPath)
		if err != nil {
			return CommandResult{}, fmt.Errorf("machine Execute: open stdin: %w", err)
		}
		defer f.Close()
		child.Stdin = f
	}
	stdout, stderr, ev, err := runCaptureWithLimits(child, m.limits)
	res := CommandResult{Stdout: stdout, Stderr: stderr, Limits: ev}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			res.ExitCode = ee.ExitCode()
			return res, nil
		}
		return res, err
	}
	return res, nil
}

// ReadFile reads a file inside the machine's worktree. An absolute path or a
// `..` escape is rejected (the agent operates inside its machine only).
func (m *localMachine) ReadFile(ctx context.Context, path string) ([]byte, error) {
	full, err := m.resolve(path)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(full)
}

// WriteFile writes a file inside the machine's worktree (0600, traversal
// rejected). It is the agent's only write surface; it cannot reach the host.
func (m *localMachine) WriteFile(ctx context.Context, path string, data []byte) error {
	full, err := m.resolve(path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		return err
	}
	return os.WriteFile(full, data, 0o600)
}

// Destroy tears down this machine's own worktree, honoring the ownership claim.
func (m *localMachine) Destroy(ctx context.Context) error {
	cleanupWorktree(ctx, filepath.Clean(m.spec.RepoPath), m.dir, m.spec.WorkflowID)
	return nil
}

// resolve maps a worktree-relative path to an absolute one, rejecting absolute
// paths, NUL, empty paths and any `..` escape.
func (m *localMachine) resolve(path string) (string, error) {
	p := strings.TrimSpace(path)
	if p == "" || strings.ContainsRune(p, 0) {
		return "", fmt.Errorf("machine path %q is empty or contains NUL", path)
	}
	if filepath.IsAbs(p) {
		return "", fmt.Errorf("machine path %q must be worktree-relative", path)
	}
	full := filepath.Clean(filepath.Join(m.dir, p))
	if full != m.dir && !strings.HasPrefix(full, m.dir+string(filepath.Separator)) {
		return "", fmt.Errorf("machine path %q escapes the worktree", path)
	}
	return full, nil
}

// validateMachineSpec rejects an unusable spec at the provider boundary: a
// machine must be owned (WorkflowID), have a main checkout (RepoPath) and a dir.
func validateMachineSpec(spec MachineSpec) error {
	if strings.TrimSpace(spec.WorkflowID) == "" {
		return fmt.Errorf("machine spec: workflowId is required (ownership claim, §4.4)")
	}
	if strings.TrimSpace(spec.RepoPath) == "" {
		return fmt.Errorf("machine spec: repoPath is required")
	}
	if strings.TrimSpace(spec.Dir) == "" {
		return fmt.Errorf("machine spec: dir is required")
	}
	return nil
}

// machineLimits resolves the ceilings a machine's children run under: the
// per-spec override when set, else the global `limits:` default (#102).
func machineLimits(spec MachineSpec) LimitsConfig { return limitsForOverride(spec.Limits) }

// machineEnv builds the child env for a machine command: the scrubbed §23 env
// with PWD pinned to dir (cmdEnv), overlay extras, then the advisory #103 egress
// proxy. A credential-shaped extra is dropped, so an adapter or agent cannot
// re-add a GitHub/cloud credential the pipeline stripped.
func machineEnv(dir string, extra []string) []string {
	env := cmdEnv(dir)
	for _, kv := range extra {
		i := strings.IndexByte(kv, '=')
		if i <= 0 {
			continue
		}
		k := kv[:i]
		if isGitHubCredentialEnv(k) || isCloudCredentialEnv(k) {
			continue
		}
		env = setEnv(env, k, kv[i+1:])
	}
	return egressEnv(env, appCfg.Egress)
}
