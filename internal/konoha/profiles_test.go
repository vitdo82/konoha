package konoha

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// #117: a named profile flattens into harness/model/limits at load, per-stage
// selection resolves distinct profiles, and the resolved bundle carries a stable
// digest. tools/mcp are recorded + digested (enforcement lands with #119).
func TestResolveAgentProfilesFlattensAndDigests(t *testing.T) {
	cfg := defaultAppConfig()
	cfg.Defaults.Agents = AgentsConfig{Models: map[string]AgentModel{
		"cheap":  {Provider: "anthropic", Model: "claude-haiku"},
		"strong": {Provider: "anthropic", Model: "claude-sonnet"},
	}}
	cfg.MCPServers = map[string]MCPServerSpec{"github": {URL: "https://example.test/mcp"}}
	cfg.Agents = AgentProfilesConfig{
		"cheap":  {Harness: "opencode", Model: "cheap", Tools: []string{"read", "grep"}, MCP: []string{"github"}},
		"strong": {Harness: "opencode", Model: "strong"},
	}
	repos := []RepoConfig{{Owner: "o", Repo: "r", RepoPath: "/tmp/r", Lifecycle: &LifecycleConfig{
		Agents: map[string]string{profileDefaultStage: "cheap", stageDeveloper: "strong"},
	}}}
	if err := resolveAgentProfiles(repos, cfg); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	lc := repos[0].Lifecycle
	if lc.Harness != "opencode" {
		t.Fatalf("harness = %q, want opencode", lc.Harness)
	}
	// Per-stage distinct profiles: analyst falls back to the repo default (cheap),
	// developer selects strong.
	if lc.Models[stageAnalyst] != "cheap" || lc.Models[stageDeveloper] != "strong" {
		t.Fatalf("models = %+v, want analyst=cheap developer=strong", lc.Models)
	}
	if lc.Profiles[stageAnalyst] != "cheap" || lc.Profiles[stageDeveloper] != "strong" {
		t.Fatalf("profiles = %+v, want analyst=cheap developer=strong", lc.Profiles)
	}
	if len(lc.ProfileDigest) != 64 || strings.Trim(lc.ProfileDigest, "0123456789abcdef") != "" {
		t.Fatalf("profileDigest = %q, want a 64-char SHA-256 hex", lc.ProfileDigest)
	}

	// Deterministic: identical input resolves to the identical digest.
	again := []RepoConfig{{Owner: "o", Repo: "r", RepoPath: "/tmp/r", Lifecycle: &LifecycleConfig{
		Agents: map[string]string{profileDefaultStage: "cheap", stageDeveloper: "strong"},
	}}}
	if err := resolveAgentProfiles(again, cfg); err != nil {
		t.Fatal(err)
	}
	if again[0].Lifecycle.ProfileDigest != lc.ProfileDigest {
		t.Fatal("resolution must be deterministic (same input, same digest)")
	}

	// A profile edit flips the digest (reviewable diff).
	edited := cfg
	edited.Agents = AgentProfilesConfig{
		"cheap":  cfg.Agents["cheap"],
		"strong": {Harness: "opencode", Model: "strong", Tools: []string{"read"}},
	}
	ed := []RepoConfig{{Owner: "o", Repo: "r", RepoPath: "/tmp/r", Lifecycle: &LifecycleConfig{
		Agents: map[string]string{profileDefaultStage: "cheap", stageDeveloper: "strong"},
	}}}
	if err := resolveAgentProfiles(ed, edited); err != nil {
		t.Fatal(err)
	}
	if ed[0].Lifecycle.ProfileDigest == lc.ProfileDigest {
		t.Fatal("a profile edit must change the digest")
	}
}

// #117: a profile's limits override the global default and are recorded on the
// flattened lifecycle.
func TestResolveAgentProfilesLimitsOverride(t *testing.T) {
	cfg := defaultAppConfig()
	cfg.Limits = LimitsConfig{MemoryMiB: 256}
	cfg.Defaults.Agents = AgentsConfig{Models: map[string]AgentModel{"m": {Provider: "p", Model: "m"}}}
	cfg.Agents = AgentProfilesConfig{"p": {Harness: "opencode", Model: "m", Limits: &LimitsConfig{MemoryMiB: 2048}}}
	repos := []RepoConfig{{Owner: "o", Repo: "r", RepoPath: "/tmp/r", Lifecycle: &LifecycleConfig{
		Agents: map[string]string{profileDefaultStage: "p"},
	}}}
	if err := resolveAgentProfiles(repos, cfg); err != nil {
		t.Fatal(err)
	}
	if lc := repos[0].Lifecycle; lc.Limits == nil || lc.Limits.MemoryMiB != 2048 {
		t.Fatalf("limits = %+v, want profile override 2048", lc.Limits)
	}
}

// #117: an unknown profile/harness/model/tool/mcp reference fails at load, never
// silently falling back.
func TestResolveAgentProfilesUnknownRefsFailClosed(t *testing.T) {
	cfg := defaultAppConfig()
	cfg.Defaults.Agents = AgentsConfig{Models: map[string]AgentModel{"known": {Provider: "p", Model: "m"}}}
	cfg.Agents = AgentProfilesConfig{
		"ok":      {Harness: "opencode", Model: "known"},
		"badmdl":  {Harness: "opencode", Model: "missing"},
		"badharn": {Harness: "bogus", Model: "known"},
		"badtool": {Harness: "opencode", Tools: []string{"read", "read"}},
		"badmcp":  {Harness: "opencode", MCP: []string{"", "github"}},
		"badmode": {Harness: "opencode", PromptMode: "telepathy"},
	}
	repo := func(name string) []RepoConfig {
		return []RepoConfig{{Owner: "o", Repo: "r", RepoPath: "/tmp/r", Lifecycle: &LifecycleConfig{
			Agents: map[string]string{profileDefaultStage: name},
		}}}
	}
	if err := resolveAgentProfiles(repo("ok"), cfg); err != nil {
		t.Fatalf("valid profile must resolve, got %v", err)
	}
	for _, name := range []string{"nope", "badmdl", "badharn", "badtool", "badmcp", "badmode"} {
		if err := resolveAgentProfiles(repo(name), cfg); err == nil {
			t.Fatalf("profile %q must fail at load", name)
		}
	}
	// Unknown stage key is rejected by the lifecycle validator itself.
	if err := (&LifecycleConfig{Agents: map[string]string{"bogus": "ok"}}).validate(); err == nil {
		t.Fatal("an unknown lifecycle.agents stage must fail at load")
	}
}

// #117: setting both an explicit profile and a conflicting legacy key fails at
// load with a clear error; agreeing values pass.
func TestResolveAgentProfilesLegacyConflict(t *testing.T) {
	cfg := defaultAppConfig()
	cfg.Defaults.Agents = AgentsConfig{Models: map[string]AgentModel{
		"cheap":  {Provider: "p", Model: "m"},
		"strong": {Provider: "p", Model: "n"},
	}}
	cfg.Agents = AgentProfilesConfig{"strong": {Harness: "opencode", Model: "strong", Limits: &LimitsConfig{MemoryMiB: 512}}}

	cases := []struct {
		name string
		lc   *LifecycleConfig
	}{
		{"harness", &LifecycleConfig{Harness: "generic", Agents: map[string]string{profileDefaultStage: "strong"}}},
		{"stage-model", &LifecycleConfig{Models: map[string]string{stageDeveloper: "cheap"}, Agents: map[string]string{profileDefaultStage: "strong"}}},
		{"raw-model", &LifecycleConfig{Model: "openai/gpt", Agents: map[string]string{profileDefaultStage: "strong"}}},
		{"limits", &LifecycleConfig{Limits: &LimitsConfig{MemoryMiB: 1024}, Agents: map[string]string{profileDefaultStage: "strong"}}},
	}
	for _, tc := range cases {
		repos := []RepoConfig{{Owner: "o", Repo: "r", RepoPath: "/tmp/r", Lifecycle: tc.lc}}
		if err := resolveAgentProfiles(repos, cfg); err == nil {
			t.Fatalf("conflicting legacy key %q must fail at load", tc.name)
		}
	}

	// Agreeing legacy values are allowed.
	agree := &LifecycleConfig{
		Harness: "opencode",
		Models:  map[string]string{stageDeveloper: "strong"},
		Agents:  map[string]string{profileDefaultStage: "strong"},
	}
	repos := []RepoConfig{{Owner: "o", Repo: "r", RepoPath: "/tmp/r", Lifecycle: agree}}
	if err := resolveAgentProfiles(repos, cfg); err != nil {
		t.Fatalf("agreeing legacy values must pass, got %v", err)
	}
}

// #117: no profile selection means the legacy path is untouched (backward
// compatibility): resolution is a no-op.
func TestResolveAgentProfilesNoopWithoutSelection(t *testing.T) {
	cfg := defaultAppConfig()
	lc := &LifecycleConfig{Harness: "opencode", Model: "openai/gpt", Limits: &LimitsConfig{MemoryMiB: 128}}
	repos := []RepoConfig{{Owner: "o", Repo: "r", RepoPath: "/tmp/r", Lifecycle: lc}}
	if err := resolveAgentProfiles(repos, cfg); err != nil {
		t.Fatal(err)
	}
	if lc.Model != "openai/gpt" || lc.Harness != "opencode" || lc.Limits.MemoryMiB != 128 || lc.ProfileDigest != "" {
		t.Fatalf("legacy lifecycle must be untouched, got %+v", lc)
	}
}

// #117/§88: the resolved profiles are stamped onto the terminal outcome and the
// policy digest covers them.
func TestProfilesRecordedInOutcomeAndDigest(t *testing.T) {
	lc := LifecycleConfig{
		Profiles:      map[string]string{stageDeveloper: "strong"},
		ProfileDigest: "deadbeef",
	}
	var out CycleOutcome
	stampProfiles(&out, lc)
	if out.Profiles[stageDeveloper] != "strong" || out.ProfileDigest != "deadbeef" {
		t.Fatalf("outcome profiles = %+v digest = %q", out.Profiles, out.ProfileDigest)
	}

	a := LifecycleConfig{Profiles: map[string]string{stageDeveloper: "strong"}}
	b := LifecycleConfig{Profiles: map[string]string{stageDeveloper: "cheap"}}
	if policyDigest(a, "task") == policyDigest(b, "task") {
		t.Fatal("policy digest must cover the resolved profiles")
	}
}

// #117 AC: resolution happens exactly once at load and the resolved profile map
// + digest travel in the schedule input (no workflow-side name resolution); a
// conflicting legacy key fails the same load path.
func TestAgentProfilesResolveAtLoadIntoInput(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "konoha.yaml"), []byte(`
agents:
  cheap: {harness: opencode, model: cheap}
  strong: {harness: opencode, model: strong}
defaults:
  agents:
    models:
      cheap: {provider: anthropic, model: claude-haiku}
      strong: {provider: anthropic, model: claude-sonnet}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "repos.yaml"), []byte(`
repos:
  - repo: r
    owner: o
    repoPath: /x
    lifecycle:
      agents:
        default: cheap
        developer: strong
`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KONOHA_CONFIG_DIR", dir)

	lines, err := scheduleInputs(filepath.Join(dir, "repos.yaml"))
	if err != nil {
		t.Fatalf("scheduleInputs: %v", err)
	}
	if len(lines) != 1 || !strings.Contains(lines[0], `"profiles":{"analyst":"cheap"`) || !strings.Contains(lines[0], `"profileDigest":`) {
		t.Fatalf("schedule input must carry the resolved profiles + digest: %v", lines)
	}
	repos, err := loadReposConfigWithResources(filepath.Join(dir, "repos.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	lc := repos[0].Lifecycle
	if lc.Models[stageDeveloper] != "strong" || lc.Profiles[stageAnalyst] != "cheap" || lc.Harness != "opencode" {
		t.Fatalf("flattened lifecycle = %+v, want strong developer + cheap analyst default", lc)
	}

	// A conflicting legacy key fails the same load path.
	if err := os.WriteFile(filepath.Join(dir, "repos.yaml"), []byte(`
repos:
  - repo: r
    owner: o
    repoPath: /x
    lifecycle:
      harness: generic
      agents:
        default: cheap
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadReposConfigWithResources(filepath.Join(dir, "repos.yaml")); err == nil {
		t.Fatal("a conflicting legacy harness must fail at load")
	}
}
