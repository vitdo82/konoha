package konoha

import (
	"fmt"
	"net/url"
	"sort"
	"strings"
)

// Tool registry + permissions + MCP server registry (#119). Depends on #96
// (harness capabilities) and #90 (the trusted-vs-untrusted model).
//
// Konoha grants an agent only what its operator config declares. Issue text,
// PR bodies, diffs, and specs are UNTRUSTED DATA (§27) and can never widen the
// tool or MCP set: resolution reads only konoha.yaml (catalog + registry) and
// repos.yaml (profile selection), never an issue-derived field.
//
//   - Catalog: the built-in tool ids a bundled harness may use, each with a
//     description, plus operator-declared custom tools (`tools.custom.<id>`).
//   - Permissions: a profile's `tools` is a recorded allow list; `deny` is a
//     hard restriction. The bundled harnesses receive no tool allowlist, so a
//     deny entry cannot be honored — konoha fails the run CLOSED rather than
//     silently running with the denied tool still available (§40).
//   - MCP registry: `mcpServers.<name>` names an HTTP MCP server; a profile
//     references servers by name (`mcp`). Only the resolved set is handed to a
//     supporting harness (#96); an unknown reference fails at load.
//
// The resolved set is recorded in outcome.json (#83) and covered by the #88
// profile digest.

// builtinToolCatalog is the closed set of built-in tool ids a bundled harness
// may use, each with a one-line description.
var builtinToolCatalog = map[string]string{
	"read":      "Read file contents",
	"grep":      "Search file contents by pattern",
	"glob":      "Find files by name pattern",
	"edit":      "Apply targeted edits to a file",
	"write":     "Create or overwrite a file",
	"bash":      "Run a shell command",
	"webfetch":  "Fetch a URL",
	"task":      "Delegate a subtask to a sub-agent",
	"todowrite": "Track a task list",
	"skill":     "Load a specialized skill",
}

// ToolSpec is one operator-declared custom tool (`tools.custom.<id>`).
type ToolSpec struct {
	Description string `yaml:"description" json:"description,omitempty"`
}

// ToolsConfig is the top-level `tools:` block (#119).
type ToolsConfig struct {
	Custom map[string]ToolSpec `yaml:"custom" json:"custom,omitempty"`
}

// MCPServerSpec is one named HTTP MCP server (`mcpServers.<name>`). Headers is
// operator configuration (e.g. an auth header); it is only ever written into the
// harness's git-excluded config, never recorded in evidence.
type MCPServerSpec struct {
	URL     string            `yaml:"url" json:"url"`
	Headers map[string]string `yaml:"headers" json:"headers,omitempty"`
}

// AgentTooling is the resolved #119 tool + MCP policy for a repo: the union of
// its selected profiles' allow/deny lists and MCP server NAMES (names only —
// never a credential value). Carried on the LifecycleConfig and recorded as
// outcome.json evidence.
type AgentTooling struct {
	Allow []string `json:"allow,omitempty"`
	Deny  []string `json:"deny,omitempty"`
	MCP   []string `json:"mcp,omitempty"` // server names
}

// isZero reports whether no tool/MCP policy is declared.
func (t AgentTooling) isZero() bool {
	return len(t.Allow) == 0 && len(t.Deny) == 0 && len(t.MCP) == 0
}

// toolCatalog returns the sorted built-in + custom tool ids.
func toolCatalog(cfg ToolsConfig) []string {
	seen := map[string]bool{}
	for id := range builtinToolCatalog {
		seen[id] = true
	}
	for id := range cfg.Custom {
		seen[id] = true
	}
	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// knownTool reports whether id resolves in the catalog (built-in or custom).
func knownTool(cfg ToolsConfig, id string) bool {
	if _, ok := builtinToolCatalog[id]; ok {
		return true
	}
	_, ok := cfg.Custom[id]
	return ok
}

// sortedMCPServerNames returns the sorted registry keys (stable error messages).
func sortedMCPServerNames(m map[string]MCPServerSpec) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// validate gates the `tools:` block: every custom tool has a non-empty id and
// description. Unknown keys are rejected by the strict YAML decoder.
func (c ToolsConfig) validate() error {
	for id, spec := range c.Custom {
		if strings.TrimSpace(id) == "" {
			return fmt.Errorf("tools.custom: empty tool id")
		}
		if _, builtin := builtinToolCatalog[id]; builtin {
			return fmt.Errorf("tools.custom.%s: shadows a built-in tool", id)
		}
		if strings.TrimSpace(spec.Description) == "" {
			return fmt.Errorf("tools.custom.%s.description: required", id)
		}
	}
	return nil
}

// validate gates the `mcpServers:` registry: every server has a non-empty name,
// an http(s) URL, and non-empty header names.
func validateMCPServers(m map[string]MCPServerSpec) error {
	for name, s := range m {
		prefix := "mcpServers." + name
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("mcpServers: empty server name")
		}
		u := strings.TrimSpace(s.URL)
		if u == "" {
			return fmt.Errorf("%s.url: required", prefix)
		}
		parsed, err := url.Parse(u)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			return fmt.Errorf("%s.url: %q must be an http(s) URL", prefix, s.URL)
		}
		for k := range s.Headers {
			if strings.TrimSpace(k) == "" {
				return fmt.Errorf("%s.headers: header names must be non-empty", prefix)
			}
		}
	}
	return nil
}

// validateToolRefs gates one profile's allow/deny lists against the catalog:
// every reference must resolve (unknown fails closed), a tool cannot be both
// allowed and denied, and a deny list fails closed because the bundled
// harnesses cannot remove a tool they expose (silently running with it would be
// broader capability than declared, §40).
func validateToolRefs(prefix string, allow, deny []string, cfg ToolsConfig) error {
	if err := validateStringList(prefix+".tools", allow); err != nil {
		return err
	}
	if err := validateStringList(prefix+".deny", deny); err != nil {
		return err
	}
	allowed := map[string]bool{}
	for _, id := range allow {
		if !knownTool(cfg, id) {
			return fmt.Errorf("%s.tools: unknown tool %q (known: %s)", prefix, id, strings.Join(toolCatalog(cfg), ", "))
		}
		allowed[id] = true
	}
	for _, id := range deny {
		if !knownTool(cfg, id) {
			return fmt.Errorf("%s.deny: unknown tool %q (known: %s)", prefix, id, strings.Join(toolCatalog(cfg), ", "))
		}
		if allowed[id] {
			return fmt.Errorf("%s.deny: tool %q is both allowed and denied", prefix, id)
		}
	}
	if len(deny) > 0 {
		return fmt.Errorf("%s.deny: the agent harness receives no tool allowlist, so a denied tool cannot be removed; fail closed rather than run with broader capability (§40)", prefix)
	}
	return nil
}

// ResolvedMCPServer is one named MCP server resolved from the registry: the
// operator-declared name plus its config, ready for a supporting harness.
type ResolvedMCPServer struct {
	Name    string            `json:"name"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`
}

// resolveMCPServerSpecs resolves server NAMES to their registry specs, name
// sorted. An unknown name fails closed. Pure given the registry.
func resolveMCPServerSpecs(names []string, registry map[string]MCPServerSpec) ([]ResolvedMCPServer, error) {
	if len(names) == 0 {
		return nil, nil
	}
	sorted := append([]string(nil), names...)
	sort.Strings(sorted)
	out := make([]ResolvedMCPServer, 0, len(sorted))
	for _, name := range sorted {
		spec, ok := registry[name]
		if !ok {
			return nil, fmt.Errorf("unknown MCP server %q (known: %s)", name, strings.Join(sortedMCPServerNames(registry), ", "))
		}
		out = append(out, ResolvedMCPServer{Name: name, URL: spec.URL, Headers: spec.Headers})
	}
	return out, nil
}

// sortedToolingNames returns deduplicated, sorted non-empty entries (stable
// digests/evidence regardless of profile iteration order).
func sortedToolingNames(xs []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range xs {
		x = strings.TrimSpace(x)
		if x == "" || seen[x] {
			continue
		}
		seen[x] = true
		out = append(out, x)
	}
	sort.Strings(out)
	return out
}
