package konoha

import (
	"reflect"
	"strings"
	"testing"
)

// pickerConfig builds a small #117 catalog for the picker tests.
func pickerConfig() AppConfig {
	cfg := defaultAppConfig()
	cfg.Defaults.Agents = AgentsConfig{Models: map[string]AgentModel{
		"cheap":  {Provider: "anthropic", Model: "claude-haiku"},
		"strong": {Provider: "anthropic", Model: "claude-sonnet"},
	}}
	cfg.Agents = AgentProfilesConfig{
		"strong": {Harness: "opencode", Model: "strong"},
		"cheap":  {Model: "cheap"}, // harness falls back to the default
	}
	return cfg
}

// #145: the picker projection is sorted and shows each profile's effective
// harness + model, without a real terminal.
func TestProfilePickerProjection(t *testing.T) {
	opts := profilePickerOptions(pickerConfig())
	if len(opts) != 2 || opts[0].Name != "cheap" || opts[1].Name != "strong" {
		t.Fatalf("options = %+v, want sorted [cheap strong]", opts)
	}
	if opts[0].Harness != "opencode" || opts[0].Model != "cheap" {
		t.Fatalf("cheap option = %+v, want harness fallback opencode model cheap", opts[0])
	}
	if opts[1].Harness != "opencode" || opts[1].Model != "strong" {
		t.Fatalf("strong option = %+v", opts[1])
	}

	rendered := renderProfilePicker(opts, "strong")
	if !strings.Contains(rendered, "harness=opencode model=strong") || !strings.Contains(rendered, "harness=opencode model=cheap") {
		t.Fatalf("render misses harness/model: %q", rendered)
	}
	// The selected row is marked with `*`, and the render is deterministic.
	if lines := strings.Split(rendered, "\n"); !strings.Contains(lines[2], "* strong") {
		t.Fatalf("selected profile must be marked: %q", rendered)
	}
	if renderProfilePicker(opts, "strong") != rendered {
		t.Fatal("render must be deterministic")
	}
	if !strings.Contains(renderProfilePicker(nil, "x"), "no agent profiles") {
		t.Fatal("an empty catalog renders an explicit message")
	}
}

// #145 AC: the selected profile is resolved once, recorded (stage→profile map),
// and digested (#88); a different pick flips the digest.
func TestSelectCycleProfileRecordsAndDigests(t *testing.T) {
	cfg := pickerConfig()
	repo := RepoConfig{Owner: "o", Repo: "r", RepoPath: "/x"}

	lc, err := selectCycleProfile(cfg, repo, "strong")
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if lc.Harness != "opencode" {
		t.Fatalf("harness = %q", lc.Harness)
	}
	for _, stage := range pipelineStages {
		if lc.Profiles[stage] != "strong" {
			t.Fatalf("profiles = %+v, want every stage resolved to strong", lc.Profiles)
		}
		if lc.Models[stage] != "strong" {
			t.Fatalf("models = %+v, want every stage model strong", lc.Models)
		}
	}
	if len(lc.ProfileDigest) != 64 || strings.Trim(lc.ProfileDigest, "0123456789abcdef") != "" {
		t.Fatalf("profileDigest = %q, want a 64-char SHA-256 hex", lc.ProfileDigest)
	}

	cheap, err := selectCycleProfile(cfg, repo, "cheap")
	if err != nil {
		t.Fatal(err)
	}
	if cheap.ProfileDigest == lc.ProfileDigest {
		t.Fatal("a different profile pick must change the digest")
	}
	// The #88 policy digest covers the resolved profiles, so the pick is a
	// visible diff on the recorded outcome.
	if policyDigest(lc, "task") == policyDigest(cheap, "task") {
		t.Fatal("policy digest must cover the picked profile")
	}
}

// #145 AC: with no pick the repo's existing selection stands (a default is
// used); a pick only overrides the repo-level default, preserving per-stage
// selections.
func TestSelectCycleProfileDefaultAndOverride(t *testing.T) {
	cfg := pickerConfig()
	repo := RepoConfig{Owner: "o", Repo: "r", RepoPath: "/x", Lifecycle: &LifecycleConfig{
		Agents: map[string]string{profileDefaultStage: "cheap", stageDeveloper: "strong"},
	}}

	// No pick: the repos.yaml default is left untouched (legacy resolution path
	// runs at load, not here).
	lc, err := selectCycleProfile(cfg, repo, "")
	if err != nil {
		t.Fatal(err)
	}
	if lc.Agents[stageDeveloper] != "strong" || lc.Agents[profileDefaultStage] != "cheap" {
		t.Fatalf("no pick must keep the repo selection, got %+v", lc.Agents)
	}

	// A pick replaces the repo-level default but keeps the explicit override.
	lc, err = selectCycleProfile(cfg, repo, "strong")
	if err != nil {
		t.Fatal(err)
	}
	if lc.Agents[profileDefaultStage] != "strong" || lc.Agents[stageDeveloper] != "strong" {
		t.Fatalf("pick must set the default; got %+v", lc.Agents)
	}
	if lc.Profiles[stageDeveloper] != "strong" {
		t.Fatalf("developer override lost: %+v", lc.Profiles)
	}
}

// #145 AC: an unknown profile fails closed — the picker never adopts a name
// that is not in the operator's catalog, so (per §27) a profile named only in
// issue text can never be selected.
func TestSelectCycleProfileUnknownFailsClosed(t *testing.T) {
	cfg := pickerConfig()
	repo := RepoConfig{Owner: "o", Repo: "r", RepoPath: "/x"}
	if _, err := selectCycleProfile(cfg, repo, "cheap"); err != nil {
		t.Fatalf("known profile must resolve: %v", err)
	}
	// Issue text is untrusted data, never a selection source: a name that only
	// appears in an issue body is unknown to the catalog and fails closed.
	issueText := "please run this with --profile super-strong"
	if !strings.Contains(issueText, "--profile super-strong") {
		t.Fatal("test setup")
	}
	if _, err := selectCycleProfile(cfg, repo, "super-strong"); err == nil {
		t.Fatal("a profile named only in issue text must fail closed (§27)")
	}
}

// #145: a profile pick cannot ride in through the generic issue fields — the
// workflow input carries only the flattened lifecycle (harness/model/limits),
// never a raw profile name from an issue.
func TestIssueInputCarriesNoRawProfileName(t *testing.T) {
	rt := reflect.TypeOf(IssueInput{})
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		if strings.Contains(strings.ToLower(f.Name), "profile") {
			t.Fatalf("IssueInput.%s must not carry a profile name (§27)", f.Name)
		}
	}
}
