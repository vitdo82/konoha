package konoha

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Declarative agent profiles (#117). A profile bundles the per-agent layers that
// used to be separate keys — harness (#85), model (#98), limits (#102), plus the
// tool/MCP references and prompt transport (#96) — into one named unit an
// operator declares once in konoha.yaml `agents:`. A repo selects a profile per
// stage in repos.yaml (`lifecycle.agents.<stage>`, with a repo-level `default`).
//
// Resolution happens ONCE at load (apply.sh / prbot config): the selected profile
// is flattened into the LifecycleConfig the workflow already consumes, so no
// workflow-side name resolution ever occurs and the resolved bundle carries a
// content digest (#88). An unknown profile/harness/model reference fails at load.
//
// Backward compatibility: a repo with no `lifecycle.agents` keeps today's
// behavior byte-for-byte (legacy harness/model/limits are the implicit profile).
// Declaring both a profile and a CONFLICTING legacy key fails at load.
//
// tools/mcp/promptMode are resolved + recorded + digested here; their
// cross-reference enforcement lands with the tool/MCP registry (#119), which
// owns the named catalogs. Until then a profile records them as evidence, never
// a false claim of capability.

// profileDefaultStage is the repos.yaml selection key that supplies the profile
// for every stage without an explicit selection.
const profileDefaultStage = "default"

// AgentProfile is one declarative `agents.<name>` profile (#117).
type AgentProfile struct {
	Harness    string        `yaml:"harness" json:"harness,omitempty"`
	Model      string        `yaml:"model" json:"model,omitempty"` // §98 catalog NAME (never a raw id)
	Tools      []string      `yaml:"tools" json:"tools,omitempty"` // #119 advisory allow list (recorded + digested)
	Deny       []string      `yaml:"deny" json:"deny,omitempty"`   // #119 hard deny list (fails closed; not enforceable by bundled harnesses)
	MCP        []string      `yaml:"mcp" json:"mcp,omitempty"`     // #119 named MCP servers (resolved from `mcpServers`)
	Limits     *LimitsConfig `yaml:"limits" json:"limits,omitempty"`
	PromptMode string        `yaml:"promptMode" json:"promptMode,omitempty"`
}

// AgentProfilesConfig is the top-level `agents:` map (konoha.yaml).
type AgentProfilesConfig map[string]AgentProfile

// knownPromptMode reports whether mode is a prompt transport a harness can honor.
// `argv` is accepted as an alias for `arg` (matching the #117 example).
func knownPromptMode(mode string) bool {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case promptModeArg, promptModeStdin, promptModeStdinFile, "argv":
		return true
	}
	return false
}

// normalizePromptMode canonicalizes a declared prompt mode (argv → arg).
func normalizePromptMode(mode string) string {
	m := strings.ToLower(strings.TrimSpace(mode))
	if m == "argv" {
		return promptModeArg
	}
	return m
}

// sortedProfileNames returns the sorted profile keys (stable error messages).
func sortedProfileNames(m AgentProfilesConfig) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// validate gates one profile: a known harness, a known §98 catalog model name,
// known tool/MCP references (against the #119 catalogs), valid limits and a
// known prompt mode.
func (p AgentProfile) validate(name string, models AgentsConfig, tools ToolsConfig, mcp map[string]MCPServerSpec) error {
	prefix := "agents." + name
	if h := strings.TrimSpace(p.Harness); h != "" && !knownHarnessName(h) {
		return fmt.Errorf("%s.harness: unknown harness %q (known: %s)", prefix, p.Harness, strings.Join(builtinHarnessNames, ", "))
	}
	if m := strings.TrimSpace(p.Model); m != "" {
		if _, ok := models.Models[m]; !ok {
			return fmt.Errorf("%s.model: unknown model %q (known: %s)", prefix, m, strings.Join(sortedModelNames(models.Models), ", "))
		}
	}
	if err := validateToolRefs(prefix, p.Tools, p.Deny, tools); err != nil {
		return err
	}
	if err := validateStringList(prefix+".mcp", p.MCP); err != nil {
		return err
	}
	for _, name := range p.MCP {
		if _, ok := mcp[name]; !ok {
			return fmt.Errorf("%s.mcp: unknown MCP server %q (known: %s)", prefix, name, strings.Join(sortedMCPServerNames(mcp), ", "))
		}
	}
	if p.Limits != nil {
		if err := p.Limits.validate(prefix + ".limits"); err != nil {
			return err
		}
	}
	if m := strings.TrimSpace(p.PromptMode); m != "" && !knownPromptMode(m) {
		return fmt.Errorf("%s.promptMode: %q must be arg, stdin or stdin_file", prefix, p.PromptMode)
	}
	return nil
}

// validate gates the `agents:` map: non-empty names and each profile valid.
func (ps AgentProfilesConfig) validate(models AgentsConfig, tools ToolsConfig, mcp map[string]MCPServerSpec) error {
	for name, p := range ps {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("agents: empty profile name")
		}
		if err := p.validate(name, models, tools, mcp); err != nil {
			return err
		}
	}
	return nil
}

// resolvedProfile is the flattened, digestable form of one selected profile: the
// effective harness + the catalog model NAME for one stage, plus the recorded
// tool/MCP/prompt-mode references.
type resolvedProfile struct {
	Stage      string        `json:"stage"`
	Name       string        `json:"name"`
	Harness    string        `json:"harness,omitempty"`
	Model      string        `json:"model,omitempty"`
	Tools      []string      `json:"tools,omitempty"`
	Deny       []string      `json:"deny,omitempty"`
	MCP        []string      `json:"mcp,omitempty"`
	Limits     *LimitsConfig `json:"limits,omitempty"`
	PromptMode string        `json:"promptMode,omitempty"`
}

// profileDigest is the SHA-256 of the resolved profiles, canonical-JSON encoded
// (stage-sorted) so identical selections yield an identical digest across
// processes and any profile edit flips it.
func profileDigest(profiles []resolvedProfile) string {
	cp := append([]resolvedProfile(nil), profiles...)
	sort.Slice(cp, func(i, j int) bool { return cp[i].Stage < cp[j].Stage })
	b, _ := json.Marshal(cp)
	return sha256Hex(b)
}

// limitsEqual compares two limits blocks field-by-field.
func limitsEqual(a, b LimitsConfig) bool {
	return a.CPUMilli == b.CPUMilli && a.MemoryMiB == b.MemoryMiB &&
		a.DiskMiB == b.DiskMiB && strings.TrimSpace(a.MaxWallClock) == strings.TrimSpace(b.MaxWallClock)
}

// resolveAgentProfiles resolves every repo's stage→profile selection ONCE at
// load, flattening the selected profile into the LifecycleConfig the workflow
// consumes (harness + per-stage model + limits). It fails closed on an unknown
// profile/harness/model reference, on profiles that disagree on the single
// harness/limits the pipeline can express, and on a conflicting legacy key. It
// mutates repos in place; pure given (repos, cfg).
func resolveAgentProfiles(repos []RepoConfig, cfg AppConfig) error {
	for i := range repos {
		lc := repos[i].Lifecycle
		if lc == nil || len(lc.Agents) == 0 {
			continue
		}
		repo := repos[i].Repo
		defaultHarness := strings.TrimSpace(lc.Harness)
		if defaultHarness == "" {
			defaultHarness = strings.TrimSpace(cfg.Defaults.Lifecycle.Harness)
		}
		if defaultHarness == "" {
			defaultHarness = defaultHarnessName
		}
		resolvedHarness := ""
		var resolvedLimits *LimitsConfig
		var tooling AgentTooling

		var resolved []resolvedProfile
		for _, stage := range pipelineStages {
			name := strings.TrimSpace(lc.Agents[stage])
			if name == "" {
				name = strings.TrimSpace(lc.Agents[profileDefaultStage])
			}
			if name == "" {
				continue
			}
			p, ok := cfg.Agents[name]
			if !ok {
				return fmt.Errorf("%s: lifecycle.agents.%s: unknown profile %q (known: %s)", repo, stage, name, strings.Join(sortedProfileNames(cfg.Agents), ", "))
			}
			if err := p.validate(name, cfg.Defaults.Agents, cfg.Tools, cfg.MCPServers); err != nil {
				return fmt.Errorf("%s: %w", repo, err)
			}
			rp := resolvedProfile{Stage: stage, Name: name, Tools: p.Tools, Deny: p.Deny, MCP: p.MCP, PromptMode: normalizePromptMode(p.PromptMode)}
			// #119: the repo-level tool/MCP policy is the union of every selected
			// profile (names only; deduped + sorted for a stable digest/evidence).
			tooling.Allow = append(tooling.Allow, p.Tools...)
			tooling.Deny = append(tooling.Deny, p.Deny...)
			tooling.MCP = append(tooling.MCP, p.MCP...)

			// Harness: a profile may set it; every selected profile must agree on
			// the single harness the pipeline can express.
			h := strings.TrimSpace(p.Harness)
			if h != "" && strings.TrimSpace(lc.Harness) != "" && h != strings.TrimSpace(lc.Harness) {
				return fmt.Errorf("%s: lifecycle.agents.%s profile %q harness %q conflicts with lifecycle.harness %q", repo, stage, name, h, lc.Harness)
			}
			if h == "" {
				h = defaultHarness
			}
			if resolvedHarness == "" {
				resolvedHarness = h
			} else if resolvedHarness != h {
				return fmt.Errorf("%s: lifecycle.agents.%s profile %q harness %q conflicts with harness %q selected by another stage", repo, stage, name, h, resolvedHarness)
			}
			rp.Harness = h

			// Model: a §98 catalog name; unknown fails closed. It must agree with
			// any legacy per-stage model and never coexist with the legacy raw id.
			if m := strings.TrimSpace(p.Model); m != "" {
				if strings.TrimSpace(lc.Model) != "" {
					return fmt.Errorf("%s: lifecycle.agents.%s profile %q model %q conflicts with legacy lifecycle.model %q", repo, stage, name, m, lc.Model)
				}
				if cur := strings.TrimSpace(lc.Models[stage]); cur != "" && cur != m {
					return fmt.Errorf("%s: lifecycle.agents.%s profile %q model %q conflicts with lifecycle.models.%s %q", repo, stage, name, m, stage, cur)
				}
				if lc.Models == nil {
					lc.Models = map[string]string{}
				}
				lc.Models[stage] = m
				rp.Model = m
			}

			// Limits: profiles that declare them must agree; a declared block
			// overrides the global default.
			if p.Limits != nil {
				if resolvedLimits != nil && !limitsEqual(*resolvedLimits, *p.Limits) {
					return fmt.Errorf("%s: lifecycle.agents.%s profile %q limits conflict with another selected profile", repo, stage, name)
				}
				if lc.Limits != nil && !limitsEqual(*lc.Limits, *p.Limits) {
					return fmt.Errorf("%s: lifecycle.agents.%s profile %q limits conflict with lifecycle.limits", repo, stage, name)
				}
				resolvedLimits = p.Limits
			}
			rp.Limits = p.Limits
			resolved = append(resolved, rp)
		}
		if len(resolved) == 0 {
			continue
		}
		lc.Harness = resolvedHarness
		if resolvedLimits != nil {
			lc.Limits = resolvedLimits
		}
		lc.Tooling = AgentTooling{
			Allow: sortedToolingNames(tooling.Allow),
			Deny:  sortedToolingNames(tooling.Deny),
			MCP:   sortedToolingNames(tooling.MCP),
		}
		if lc.Profiles == nil {
			lc.Profiles = map[string]string{}
		}
		for _, rp := range resolved {
			lc.Profiles[rp.Stage] = rp.Name
		}
		lc.ProfileDigest = profileDigest(resolved)
	}
	return nil
}

// stampProfiles copies the load-resolved profile names + digest onto a terminal
// outcome so they appear in outcome.json evidence (#88). Pure.
func stampProfiles(out *CycleOutcome, lc LifecycleConfig) {
	if len(lc.Profiles) > 0 {
		out.Profiles = lc.Profiles
	}
	if lc.ProfileDigest != "" {
		out.ProfileDigest = lc.ProfileDigest
	}
	// #119: record the resolved tool/MCP set (names only) alongside the profiles.
	if !lc.Tooling.isZero() {
		t := lc.Tooling
		out.Tooling = &t
	}
}
