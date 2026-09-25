package konoha

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
)

// Name-based agent harness registry (#85). A repo selects a harness by NAME
// (lifecycle.harness, default opencode); config never names an executable path.
// The harness owns its executable, argv, env allowlist and prompt transport.
// Agents still receive no GH_* credential (§23) regardless of harness.

// Prompt transport modes a harness can declare.
const (
	promptModeArg   = "arg"
	promptModeStdin = "stdin"
)

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

// agentCommand builds the child process for one agent run: the repo-selected
// harness (name, never a path) plus an explicitly resolved model. A malformed
// model is rejected rather than adopted silently.
func agentCommand(ctx context.Context, prompt, model, harnessName, dir string) (*exec.Cmd, error) {
	h, err := harnessRegistryFor().Lookup(effectiveHarnessName(harnessName))
	if err != nil {
		return nil, err
	}
	rm, err := h.ResolveModel(model)
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, h.Executable(), h.Args(prompt, rm)...)
	cmd.Dir = dir
	cmd.Env = h.Env(dir) // §23: no GitHub write credential reaches the agent
	if h.PromptMode() == promptModeStdin {
		cmd.Stdin = strings.NewReader(prompt)
	}
	return cmd, nil
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

// validateAgentModel rejects a model that is not a provider/model id, so a
// mistyped model fails with a clear message instead of being adopted silently.
func validateAgentModel(model string) (string, error) {
	m := strings.TrimSpace(model)
	if !strings.Contains(m, "/") || strings.ContainsAny(m, " \t\n") {
		return "", fmt.Errorf("unknown model %q: want a provider/model id", model)
	}
	return m, nil
}
