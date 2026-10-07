package konoha

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// Name-based agent harness registry (#85). A repo selects a harness by NAME
// (lifecycle.harness, default opencode); config never names an executable path.
// The harness owns its executable, argv, env allowlist and prompt transport.
// Agents still receive no GH_* credential (§23) regardless of harness.

// Prompt transport modes a harness can declare.
const (
	promptModeArg       = "arg"
	promptModeStdin     = "stdin"
	promptModeStdinFile = "stdin_file"
)

// Optional harness capability statuses (#96). A capability the selected harness
// does not implement/configure is recorded SKIPPED — never a false success
// (§40). FAILED fails the run closed.
const (
	capabilityApplied = "APPLIED"
	capabilitySkipped = "SKIPPED"
	capabilityFailed  = "FAILED"
)

// CapabilityOutcome is the recorded result of one optional harness capability
// (#96), surfaced in the cycle's provisioning.json evidence.
type CapabilityOutcome struct {
	Capability string `json:"capability"`
	Status     string `json:"status"`
	Detail     string `json:"detail,omitempty"`
}

// Optional harness capabilities (#96), detected by type assertion. Every path
// (binary/catalog) is operator configuration (konoha.yaml); a caller can never
// supply a path. A harness that does not implement an interface — or declares
// it disabled via capabilityDeclarer — records SKIPPED.
type Provisioner interface {
	// Provision stages the operator-supplied standalone binary into the agent's
	// PATH and verifies its sha256 before use. A mismatch fails closed. Returns
	// the staged absolute executable path.
	Provision(ctx context.Context, dir string) (executable, detail string, err error)
}

type CatalogCacher interface {
	// StageCatalog stages the model catalog so resolution is offline,
	// returning the staged path.
	StageCatalog(ctx context.Context, dir string) (catalogPath string, err error)
}

type Versioner interface {
	// Version captures "<executable> --version" best-effort ("" on any failure).
	Version(ctx context.Context, dir string) string
}

type MCPConfigurer interface {
	// WriteMCPConfig writes the resolved #119 MCP server set (registry specs, in
	// name order) into the harness's expected location before the agent runs. An
	// empty set falls back to the #96 static operator config, so both paths are
	// supported.
	WriteMCPConfig(ctx context.Context, dir string, servers []ResolvedMCPServer) error
}

// capabilityDeclarer lets a harness declare which optional capabilities its
// current operator configuration enables. A harness without it (opencode) has
// none, so every optional capability is SKIPPED.
type capabilityDeclarer interface {
	capabilities() map[string]bool
}

// defaultHarnessName is the built-in harness used when a repo leaves
// lifecycle.harness unset.
const defaultHarnessName = "opencode"

// isGitHubCredentialEnv reports whether key is a GitHub credential. These are
// stripped from every agent AND validation child unconditionally (§23/§90): no
// allowlist can re-enable them.
func isGitHubCredentialEnv(key string) bool {
	return strings.HasPrefix(key, "GH_") || key == "GITHUB_TOKEN"
}

// cloudCredentialPrefixes are the provider families stripped by default. A
// generic harness's passEnv may deliberately allowlist one (the operator opts
// in explicitly); the opencode harness and all validation gates never do.
var cloudCredentialPrefixes = []string{"AWS_", "GOOGLE_", "AZURE_"}

// cloudCredentialNames are the provider/registry secrets stripped by default.
var cloudCredentialNames = map[string]bool{
	"OPENAI_API_KEY":    true,
	"ANTHROPIC_API_KEY": true,
	"NPM_TOKEN":         true,
}

// isCloudCredentialEnv reports whether key is a common cloud/provider
// credential (§90(a)).
func isCloudCredentialEnv(key string) bool {
	if cloudCredentialNames[key] {
		return true
	}
	for _, p := range cloudCredentialPrefixes {
		if strings.HasPrefix(key, p) {
			return true
		}
	}
	return false
}

// builtinHarnessNames is the closed set of resolvable harness names. An unknown
// name fails at config load (apply.sh / prbot config).
var builtinHarnessNames = []string{"generic", "opencode"}

// knownHarnessName reports whether name is a built-in harness.
func knownHarnessName(name string) bool {
	n := strings.ToLower(strings.TrimSpace(name))
	for _, b := range builtinHarnessNames {
		if n == b {
			return true
		}
	}
	return false
}

// Harness is one named agent execution strategy.
type Harness interface {
	Name() string
	Executable() string
	Args(prompt, model string) []string
	Env(dir string) []string
	PromptMode() string // promptModeArg | promptModeStdin
	ResolveModel(model string) (string, error)
}

// harnessRegistry is a name-keyed harness set.
type harnessRegistry struct {
	byName map[string]Harness
}

// newHarnessRegistry builds a registry, rejecting empty and duplicate
// (lowercased) names.
func newHarnessRegistry(hs ...Harness) (*harnessRegistry, error) {
	r := &harnessRegistry{byName: make(map[string]Harness, len(hs))}
	for _, h := range hs {
		n := strings.ToLower(strings.TrimSpace(h.Name()))
		if n == "" {
			return nil, fmt.Errorf("harness: empty name")
		}
		if _, dup := r.byName[n]; dup {
			return nil, fmt.Errorf("harness: duplicate name %q", n)
		}
		r.byName[n] = h
	}
	return r, nil
}

// Lookup resolves a harness by name (lowercased; empty falls back to the
// built-in default). An unknown name is an error.
func (r *harnessRegistry) Lookup(name string) (Harness, error) {
	n := strings.ToLower(strings.TrimSpace(name))
	if n == "" {
		n = defaultHarnessName
	}
	h, ok := r.byName[n]
	if !ok {
		return nil, fmt.Errorf("unknown harness %q (known: %s)", name, strings.Join(r.Names(), ", "))
	}
	return h, nil
}

// Names returns the sorted harness names (stable output for prbot doctor / the
// console).
func (r *harnessRegistry) Names() []string {
	out := make([]string, 0, len(r.byName))
	for n := range r.byName {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Availability reports, per harness name, whether its executable resolves on
// PATH. Best-effort discovery: a missing executable is reported, never fatal.
func (r *harnessRegistry) Availability() map[string]bool {
	out := make(map[string]bool, len(r.byName))
	for n, h := range r.byName {
		if strings.TrimSpace(h.Executable()) == "" {
			out[n] = false
			continue
		}
		_, err := exec.LookPath(h.Executable())
		out[n] = err == nil
	}
	return out
}

// harnessRegistryFor builds the process registry from the resolved app config
// (the generic harness's operator-declared executable/args live there).
func harnessRegistryFor() *harnessRegistry {
	r, _ := newHarnessRegistry(opencodeHarness{}, genericHarness{cfg: appCfg.Harness})
	return r
}

// effectiveHarnessName resolves the repo selection, then the process default,
// then the built-in default.
func effectiveHarnessName(repoName string) string {
	if n := strings.ToLower(strings.TrimSpace(repoName)); n != "" {
		return n
	}
	if n := strings.ToLower(strings.TrimSpace(appCfg.Defaults.Lifecycle.Harness)); n != "" {
		return n
	}
	return defaultHarnessName
}

// provisionedAgent is the result of preparing one agent run: the optional
// capability outcomes (for evidence) plus the resolved executable (staged
// pinned binary or the harness default) and staged catalog path.
type provisionedAgent struct {
	Outcomes    []CapabilityOutcome
	Executable  string
	CatalogPath string
}

// prepareAgent applies the harness's optional capabilities (#96) in a fixed
// order. A capability the harness does not implement/configure records SKIPPED;
// a provisioning failure (e.g. sha256 mismatch) fails closed with the FAILED
// outcome already recorded. The #119 resolved MCP server set is handed to an
// MCPConfigurer; an empty set keeps the #96 static config path.
func prepareAgent(ctx context.Context, h Harness, dir string, servers []ResolvedMCPServer) (provisionedAgent, error) {
	pa := provisionedAgent{}
	enabled := map[string]bool{}
	if cd, ok := h.(capabilityDeclarer); ok {
		enabled = cd.capabilities()
	}

	if p, ok := h.(Provisioner); ok && enabled["provision"] {
		exe, detail, err := p.Provision(ctx, dir)
		if err != nil {
			pa.Outcomes = append(pa.Outcomes, CapabilityOutcome{Capability: "provision", Status: capabilityFailed, Detail: err.Error()})
			return pa, fmt.Errorf("harness provision: %w", err)
		}
		pa.Executable = exe
		pa.Outcomes = append(pa.Outcomes, CapabilityOutcome{Capability: "provision", Status: capabilityApplied, Detail: detail})
	} else {
		pa.Outcomes = append(pa.Outcomes, CapabilityOutcome{Capability: "provision", Status: capabilitySkipped, Detail: "not configured"})
	}

	if c, ok := h.(CatalogCacher); ok && enabled["catalog"] {
		path, err := c.StageCatalog(ctx, dir)
		if err != nil {
			pa.Outcomes = append(pa.Outcomes, CapabilityOutcome{Capability: "catalog", Status: capabilityFailed, Detail: err.Error()})
			return pa, fmt.Errorf("harness catalog: %w", err)
		}
		pa.CatalogPath = path
		pa.Outcomes = append(pa.Outcomes, CapabilityOutcome{Capability: "catalog", Status: capabilityApplied, Detail: filepath.Base(path)})
	} else {
		pa.Outcomes = append(pa.Outcomes, CapabilityOutcome{Capability: "catalog", Status: capabilitySkipped, Detail: "not configured"})
	}

	if v, ok := h.(Versioner); ok && enabled["version"] {
		ver := v.Version(ctx, dir)
		detail := ver
		if detail == "" {
			detail = "unavailable"
		}
		pa.Outcomes = append(pa.Outcomes, CapabilityOutcome{Capability: "version", Status: capabilityApplied, Detail: detail})
	} else {
		pa.Outcomes = append(pa.Outcomes, CapabilityOutcome{Capability: "version", Status: capabilitySkipped, Detail: "not configured"})
	}

	if m, ok := h.(MCPConfigurer); ok && enabled["mcp"] {
		if err := m.WriteMCPConfig(ctx, dir, servers); err != nil {
			pa.Outcomes = append(pa.Outcomes, CapabilityOutcome{Capability: "mcp", Status: capabilityFailed, Detail: err.Error()})
			return pa, fmt.Errorf("harness mcp: %w", err)
		}
		detail := "static config"
		if len(servers) > 0 {
			detail = fmt.Sprintf("%d resolved server(s)", len(servers))
		}
		pa.Outcomes = append(pa.Outcomes, CapabilityOutcome{Capability: "mcp", Status: capabilityApplied, Detail: detail})
	} else {
		pa.Outcomes = append(pa.Outcomes, CapabilityOutcome{Capability: "mcp", Status: capabilitySkipped, Detail: "not configured"})
	}

	return pa, nil
}

// agentCommand builds the child process for one agent run: the repo-selected
// harness (name, never a path) plus an explicitly resolved model. A malformed
// model is rejected rather than adopted silently. Optional capabilities run
// first; their outcomes are returned for evidence. #102 host resource ceilings
// are applied best-effort to the child (no-op off Linux). #119: the resolved
// tool/MCP policy is enforced here — a non-empty deny list fails closed (the
// harness cannot remove the tool) and MCP server names resolve against the
// process registry, unknown failing closed.
func agentCommand(ctx context.Context, prompt, model, harnessName, dir string, limits LimitsConfig, tooling AgentTooling) (*exec.Cmd, []CapabilityOutcome, error) {
	h, err := harnessRegistryFor().Lookup(effectiveHarnessName(harnessName))
	if err != nil {
		return nil, nil, err
	}
	if len(tooling.Deny) > 0 {
		return nil, nil, fmt.Errorf("tool deny list %v cannot be enforced by harness %q: fail closed (§40)", tooling.Deny, h.Name())
	}
	servers, err := resolveMCPServerSpecs(tooling.MCP, appCfg.MCPServers)
	if err != nil {
		return nil, nil, err
	}
	pa, err := prepareAgent(ctx, h, dir, servers)
	if err != nil {
		return nil, pa.Outcomes, err
	}
	rm, err := resolveHarnessModel(h, model, pa.CatalogPath)
	if err != nil {
		return nil, pa.Outcomes, err
	}
	exe := pa.Executable
	if strings.TrimSpace(exe) == "" {
		exe = h.Executable()
	}
	// §23: the scrubbed env never carries a GitHub write credential; #102 wraps
	// the child without changing that env. #103 appends the advisory egress proxy
	// (bypassable, never a boundary) to the scrubbed env for every harness.
	env := egressEnv(h.Env(dir), appCfg.Egress)
	cmd, _ := enforceCommand(ctx, limits, exe, h.Args(prompt, rm), dir, env)
	if pa.Executable != "" {
		cmd.Env = prependPath(cmd.Env, filepath.Dir(pa.Executable))
	}
	switch h.PromptMode() {
	case promptModeStdin:
		cmd.Stdin = strings.NewReader(prompt)
	case promptModeStdinFile:
		f, err := promptFileStdin(ctx, dir, prompt)
		if err != nil {
			return nil, pa.Outcomes, err
		}
		cmd.Stdin = f
	}
	return cmd, pa.Outcomes, nil
}

// resolveHarnessModel selects the model through the staged catalog when one was
// provisioned (deterministic, offline), else delegates to the harness.
func resolveHarnessModel(h Harness, model, catalogPath string) (string, error) {
	if strings.TrimSpace(catalogPath) == "" {
		return h.ResolveModel(model)
	}
	data, err := os.ReadFile(catalogPath)
	if err != nil {
		return "", fmt.Errorf("read staged model catalog: %w", err)
	}
	return resolveModelFromCatalog(data, model)
}

// resolveModelFromCatalog selects the model from a staged catalog so selection
// is deterministic and never touches the network. A requested model must be
// present; an empty request picks the lexicographically-first entry.
func resolveModelFromCatalog(catalog []byte, requested string) (string, error) {
	var raw []string
	if err := json.Unmarshal(catalog, &raw); err != nil {
		return "", fmt.Errorf("staged model catalog is not a JSON array of model ids: %w", err)
	}
	seen := map[string]bool{}
	var models []string
	for _, m := range raw {
		m = strings.TrimSpace(m)
		if m == "" || seen[m] {
			continue
		}
		if _, err := validateAgentModel(m); err != nil {
			return "", fmt.Errorf("staged model catalog entry %q: %w", m, err)
		}
		seen[m] = true
		models = append(models, m)
	}
	if len(models) == 0 {
		return "", fmt.Errorf("staged model catalog is empty")
	}
	sort.Strings(models)
	req := strings.TrimSpace(requested)
	if req == "" {
		return models[0], nil
	}
	for _, m := range models {
		if m == req {
			return m, nil
		}
	}
	return "", fmt.Errorf("model %q not in staged catalog (offline; available: %s)", req, strings.Join(models, ", "))
}

// stageRoot resolves the konoha-managed staging directory inside the agent's
// git admin dir, so staged files (pinned binary/catalog/prompt) are usable but
// can never dirty `git status` or be committed.
func stageRoot(ctx context.Context, dir string) (string, error) {
	out, err := execGit(ctx, dir, "rev-parse", "--git-path", "konoha-harness")
	if err != nil {
		return "", fmt.Errorf("resolve staging dir in %s: %w", dir, err)
	}
	p := strings.TrimSpace(out)
	if p == "" {
		return "", fmt.Errorf("empty staging path in %s", dir)
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(dir, p)
	}
	return p, nil
}

// promptFileStdin writes the prompt to a konoha-managed file outside the tracked
// tree (so it can never dirty status or be committed) and returns it opened for
// the child's stdin. A fixed name means repeated runs overwrite, never accumulate.
func promptFileStdin(ctx context.Context, dir, prompt string) (*os.File, error) {
	root, err := stageRoot(ctx, dir)
	if err != nil {
		return nil, err
	}
	p := filepath.Join(root, "prompt.txt")
	if err := writeFileAtomic(p, []byte(prompt)); err != nil {
		return nil, err
	}
	return os.Open(p)
}

// captureVersion runs "<executable> --version" best-effort and returns the
// trimmed output ("" on any failure — version capture never fails a run).
func captureVersion(ctx context.Context, dir string, env []string, executable string) string {
	if strings.TrimSpace(executable) == "" {
		return ""
	}
	cmd := exec.CommandContext(ctx, executable, "--version")
	cmd.Dir = dir
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// addGitExclude makes rel (worktree-relative, slash-separated) invisible to
// `git status` for the agent's repo, so a capability-written config file can
// never dirty the tree or be committed. Idempotent.
func addGitExclude(ctx context.Context, dir, rel string) error {
	out, err := execGit(ctx, dir, "rev-parse", "--git-path", "info/exclude")
	if err != nil {
		return err
	}
	p := strings.TrimSpace(out)
	if p == "" {
		return nil
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(dir, p)
	}
	if cur, err := os.ReadFile(p); err == nil {
		for _, l := range strings.Split(string(cur), "\n") {
			if strings.TrimSpace(l) == rel {
				return nil
			}
		}
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintf(f, "\n# konoha harness staging (#96)\n%s\n", rel)
	return err
}

// opencodeHarness preserves today's opencode behavior byte-for-byte.
type opencodeHarness struct{}

func (opencodeHarness) Name() string       { return "opencode" }
func (opencodeHarness) Executable() string { return "opencode" }
func (opencodeHarness) PromptMode() string { return promptModeArg }

func (opencodeHarness) Args(prompt, model string) []string { return agentArgs(prompt, model) }
func (opencodeHarness) Env(dir string) []string            { return cmdEnv(dir) }

func (opencodeHarness) ResolveModel(model string) (string, error) {
	if strings.TrimSpace(model) == "" {
		return defaultAgentModel, nil
	}
	return validateAgentModel(model)
}

// genericHarness runs an operator-declared executable with fixed argv, model
// flag substitution and an explicit env allowlist.
type genericHarness struct {
	cfg GenericHarnessConfig
}

func (genericHarness) Name() string         { return "generic" }
func (g genericHarness) Executable() string { return g.cfg.Executable }

func (g genericHarness) PromptMode() string {
	if m := strings.ToLower(strings.TrimSpace(g.cfg.PromptMode)); m == promptModeStdin || m == promptModeStdinFile {
		return m
	}
	if g.cfg.PromptOnStdin {
		return promptModeStdin
	}
	return promptModeArg
}

func (g genericHarness) Args(prompt, model string) []string {
	out := make([]string, 0, len(g.cfg.Args)+len(g.modelArgs(model))+1)
	hasPrompt := false
	for _, a := range g.cfg.Args {
		switch a {
		case "{{model_args}}":
			out = append(out, g.modelArgs(model)...)
		case "{{prompt}}":
			out = append(out, prompt)
			hasPrompt = true
		default:
			out = append(out, strings.ReplaceAll(a, "{{prompt}}", prompt))
		}
	}
	if g.PromptMode() == promptModeArg && !hasPrompt {
		out = append(out, prompt)
	}
	return out
}

// modelArgs expands {{model_args}}: the model flag + resolved model, or nothing
// when no model applies.
func (g genericHarness) modelArgs(model string) []string {
	if strings.TrimSpace(model) == "" {
		return nil
	}
	return []string{g.cfg.ModelFlag, model}
}

// Env forwards only passEnv-listed variables (plus PWD). Every GitHub
// credential is stripped regardless (§23) — an operator cannot whitelist one.
// Cloud/provider credentials (§90(a)) are absent unless explicitly allowlisted,
// so opting in is a deliberate operator decision.
func (g genericHarness) Env(dir string) []string {
	allow := make(map[string]bool, len(g.cfg.PassEnv))
	for _, k := range g.cfg.PassEnv {
		allow[k] = true
	}
	out := []string{"PWD=" + dir}
	for _, kv := range os.Environ() {
		i := strings.IndexByte(kv, '=')
		if i <= 0 {
			continue
		}
		k := kv[:i]
		if k == "PWD" || isGitHubCredentialEnv(k) || !allow[k] {
			continue
		}
		out = append(out, kv)
	}
	return out
}

func (g genericHarness) ResolveModel(model string) (string, error) {
	if strings.TrimSpace(model) == "" {
		return "", nil
	}
	m, err := validateAgentModel(model)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(g.cfg.ModelFlag) == "" {
		return "", fmt.Errorf("harness generic cannot express model %q: modelFlag is unset", model)
	}
	return m, nil
}

// capabilities declares which optional capabilities this operator
// configuration enables; anything else records SKIPPED (#96).
func (g genericHarness) capabilities() map[string]bool {
	return map[string]bool{
		"provision": strings.TrimSpace(g.cfg.Binary) != "",
		"catalog":   strings.TrimSpace(g.cfg.CatalogCache) != "",
		"version":   strings.TrimSpace(g.cfg.Executable) != "",
		"mcp":       g.cfg.MCP != nil,
	}
}

// Provision stages the operator-supplied standalone binary into the agent's
// git-admin staging dir and verifies binary_sha256 before use; a mismatch fails
// closed (#96). Returns the staged absolute executable path.
func (g genericHarness) Provision(ctx context.Context, dir string) (string, string, error) {
	src := strings.TrimSpace(g.cfg.Binary)
	if src == "" {
		return "", "", fmt.Errorf("binary is unset")
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return "", "", fmt.Errorf("read pinned binary %s: %w", src, err)
	}
	sum := sha256.Sum256(data)
	got := hex.EncodeToString(sum[:])
	want := strings.ToLower(strings.TrimSpace(g.cfg.BinarySHA256))
	if got != want {
		return "", "", fmt.Errorf("pinned binary sha256 mismatch: got %s want %s", got, want)
	}
	root, err := stageRoot(ctx, dir)
	if err != nil {
		return "", "", err
	}
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		return "", "", err
	}
	dst := filepath.Join(binDir, filepath.Base(src))
	if err := writeFileAtomic(dst, data); err != nil {
		return "", "", err
	}
	if err := os.Chmod(dst, 0o755); err != nil {
		return "", "", err
	}
	return dst, fmt.Sprintf("staged %s (sha256 verified)", filepath.Base(src)), nil
}

// StageCatalog copies catalog_cache into the agent's staging dir, returning the
// staged path for offline model resolution (#96).
func (g genericHarness) StageCatalog(ctx context.Context, dir string) (string, error) {
	src := strings.TrimSpace(g.cfg.CatalogCache)
	if src == "" {
		return "", fmt.Errorf("catalog_cache is unset")
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return "", fmt.Errorf("read catalog %s: %w", src, err)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return "", fmt.Errorf("catalog %s is empty", src)
	}
	root, err := stageRoot(ctx, dir)
	if err != nil {
		return "", err
	}
	dst := filepath.Join(root, "catalog.json")
	if err := writeFileAtomic(dst, data); err != nil {
		return "", err
	}
	return dst, nil
}

// Version captures the pinned binary's version when one is configured, else the
// declared executable's, best-effort (#96).
func (g genericHarness) Version(ctx context.Context, dir string) string {
	exe := strings.TrimSpace(g.cfg.Executable)
	if strings.TrimSpace(g.cfg.Binary) != "" {
		if root, err := stageRoot(ctx, dir); err == nil {
			exe = filepath.Join(root, "bin", filepath.Base(g.cfg.Binary))
		}
	}
	return captureVersion(ctx, dir, g.Env(dir), exe)
}

// WriteMCPConfig writes the resolved #119 MCP server set into the harness's
// expected worktree-relative location and excludes it from git so it cannot
// dirty status or be committed (#96). An empty set writes the #96 static
// operator config; a non-empty set renders the resolved registry servers as a
// conventional `{"mcpServers": {...}}` document (headers are written here but
// never recorded in evidence).
func (g genericHarness) WriteMCPConfig(ctx context.Context, dir string, servers []ResolvedMCPServer) error {
	if g.cfg.MCP == nil {
		return fmt.Errorf("mcp is unset")
	}
	rel := strings.TrimSpace(g.cfg.MCP.Path)
	if rel == "" {
		return fmt.Errorf("mcp.path is unset")
	}
	clean := filepath.Clean(rel)
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("mcp.path %q escapes the worktree", rel)
	}
	content := g.cfg.MCP.Config
	if len(servers) > 0 {
		rendered, err := renderMCPConfig(servers)
		if err != nil {
			return err
		}
		content = rendered
	}
	if err := writeFileAtomic(filepath.Join(dir, clean), []byte(content)); err != nil {
		return err
	}
	return addGitExclude(ctx, dir, filepath.ToSlash(clean))
}

// renderMCPConfig renders the resolved MCP servers as a deterministic
// `{"mcpServers": {"<name>": {"url": ..., "headers": {...}}}}` document, name
// sorted. Pure.
func renderMCPConfig(servers []ResolvedMCPServer) (string, error) {
	type serverView struct {
		URL     string            `json:"url"`
		Headers map[string]string `json:"headers,omitempty"`
	}
	doc := struct {
		MCPServers map[string]serverView `json:"mcpServers"`
	}{MCPServers: make(map[string]serverView, len(servers))}
	for _, s := range servers {
		doc.MCPServers[s.Name] = serverView{URL: s.URL, Headers: s.Headers}
	}
	b, err := json.Marshal(doc)
	if err != nil {
		return "", fmt.Errorf("render MCP config: %w", err)
	}
	return string(b), nil
}

// prependPath puts dir first on PATH in env (adding PATH when absent).
func prependPath(env []string, dir string) []string {
	for i, kv := range env {
		if strings.HasPrefix(kv, "PATH=") {
			env[i] = "PATH=" + dir + string(os.PathListSeparator) + strings.TrimPrefix(kv, "PATH=")
			return env
		}
	}
	return append(env, "PATH="+dir)
}

// validateAgentModel rejects a model that is not a provider/model id, so a
// mistyped model fails with a clear message instead of being adopted silently.
func validateAgentModel(model string) (string, error) {
	m := strings.TrimSpace(model)
	if !strings.Contains(m, "/") || strings.ContainsAny(m, " \t\n") {
		return "", fmt.Errorf("unknown model %q: want a provider/model id", model)
	}
	return m, nil
}
