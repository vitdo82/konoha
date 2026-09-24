package konoha

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// AC #85: the opencode harness produces the exact argv and env of today's
// agentArgs/cmdEnv (byte-equal argv, PWD present).
func TestOpencodeHarnessMatchesLegacy(t *testing.T) {
	h := opencodeHarness{}
	if h.Name() != "opencode" || h.Executable() != "opencode" || h.PromptMode() != promptModeArg {
		t.Fatalf("opencode harness identity wrong: %s/%s/%s", h.Name(), h.Executable(), h.PromptMode())
	}
	if got, want := h.Args("PROMPT", "provider/model"), agentArgs("PROMPT", "provider/model"); !reflect.DeepEqual(got, want) {
		t.Fatalf("opencode Args = %v, want legacy %v", got, want)
	}
	dir := t.TempDir()
	if got, want := h.Env(dir), cmdEnv(dir); !reflect.DeepEqual(got, want) {
		t.Fatalf("opencode Env != cmdEnv")
	}
	var pwd string
	for _, kv := range h.Env(dir) {
		if strings.HasPrefix(kv, "PWD=") {
			pwd = kv
		}
	}
	if pwd != "PWD="+dir {
		t.Fatalf("opencode Env missing PWD=%s: %q", dir, pwd)
	}
}

func TestRegistryNamesSortedAndAvailability(t *testing.T) {
	r, err := newHarnessRegistry(
		genericHarness{cfg: GenericHarnessConfig{Executable: "konoha-no-such-harness-xyz"}},
		opencodeHarness{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := r.Names(), []string{"generic", "opencode"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Names = %v, want %v", got, want)
	}
	// An unavailable executable is reported, never fatal.
	avail := r.Availability()
	if avail["generic"] {
		t.Fatalf("fake generic executable must report unavailable: %v", avail)
	}
	if _, err := r.Lookup("opencode"); err != nil {
		t.Fatalf("opencode must resolve: %v", err)
	}
	if _, err := r.Lookup(""); err != nil {
		t.Fatalf("empty name must fall back to opencode: %v", err)
	}
	if _, err := r.Lookup("GENERIC"); err != nil {
		t.Fatalf("lookup must be case-insensitive: %v", err)
	}
	if _, err := r.Lookup("nope"); err == nil || !strings.Contains(err.Error(), "unknown harness") {
		t.Fatalf("unknown harness must fail: %v", err)
	}
	if _, err := newHarnessRegistry(opencodeHarness{}, opencodeHarness{}); err == nil {
		t.Fatal("duplicate harness name must be rejected")
	}
}

// AC #85: per-repo lifecycle.harness selects a harness; an unknown name fails at
// load, in repos.yaml and in konoha.yaml.
func TestHarnessConfigSelectionAndLoadFailure(t *testing.T) {
	ok := writeRepos(t, `
repos:
  - repo: r
    owner: o
    repoPath: /tmp/r
    lifecycle:
      harness: generic
`)
	repos, err := LoadReposConfig(ok)
	if err != nil {
		t.Fatal(err)
	}
	if repos[0].Lifecycle.Harness != "generic" {
		t.Fatalf("per-repo harness = %q, want generic", repos[0].Lifecycle.Harness)
	}

	bad := writeRepos(t, `
repos:
  - repo: r
    owner: o
    repoPath: /tmp/r
    lifecycle:
      harness: nope
`)
	if _, err := LoadReposConfig(bad); err == nil || !strings.Contains(err.Error(), "unknown harness") {
		t.Fatalf("unknown per-repo harness must fail at load: %v", err)
	}

	cfgPath := filepath.Join(t.TempDir(), "konoha.yaml")
	if err := os.WriteFile(cfgPath, []byte("defaults:\n  lifecycle:\n    harness: nope\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadAppConfig(cfgPath); err == nil || !strings.Contains(err.Error(), "unknown harness") {
		t.Fatalf("unknown defaults harness must fail at load: %v", err)
	}

	// "name, not path": config may never name an arbitrary binary.
	pathCfg := filepath.Join(t.TempDir(), "konoha.yaml")
	if err := os.WriteFile(pathCfg, []byte("harness:\n  executable: /usr/bin/agent\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadAppConfig(pathCfg); err == nil || !strings.Contains(err.Error(), "bare executable name") {
		t.Fatalf("path executable must fail at load: %v", err)
	}
}

func TestHarnessDefaultSelection(t *testing.T) {
	old := appCfg
	defer func() { appCfg = old }()
	appCfg = defaultAppConfig()
	if got := effectiveHarnessName(""); got != "opencode" {
		t.Fatalf("effective default = %q, want opencode", got)
	}
	appCfg.Defaults.Lifecycle.Harness = "generic"
	if got := effectiveHarnessName(""); got != "generic" {
		t.Fatalf("process default = %q, want generic", got)
	}
	if got := effectiveHarnessName("opencode"); got != "opencode" {
		t.Fatalf("repo selection must win over process default: %q", got)
	}
}

// AC #85: {{model_args}} expands at a position that differs from opencode.
func TestGenericHarnessArgsPlaceholders(t *testing.T) {
	g := genericHarness{cfg: GenericHarnessConfig{
		Executable: "fake",
		Args:       []string{"--headless", "{{model_args}}", "--prompt", "{{prompt}}"},
		ModelFlag:  "--model",
	}}
	got := g.Args("PROMPT", "provider/model")
	want := []string{"--headless", "--model", "provider/model", "--prompt", "PROMPT"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("generic Args = %v, want %v", got, want)
	}
	if reflect.DeepEqual(got, agentArgs("PROMPT", "provider/model")) {
		t.Fatal("generic argv must differ from opencode's position")
	}
	// No model → {{model_args}} is removed.
	got = g.Args("PROMPT", "")
	if want := []string{"--headless", "--prompt", "PROMPT"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("empty model Args = %v, want %v", got, want)
	}
	// Stdin transport never puts the prompt in argv.
	gs := genericHarness{cfg: GenericHarnessConfig{
		Args: []string{"run", "{{model_args}}"}, ModelFlag: "--model", PromptOnStdin: true,
	}}
	if got := gs.Args("PROMPT", "p/m"); !reflect.DeepEqual(got, []string{"run", "--model", "p/m"}) {
		t.Fatalf("stdin Args = %v", got)
	}
	if gs.PromptMode() != promptModeStdin {
		t.Fatalf("PromptOnStdin must report stdin, got %q", gs.PromptMode())
	}
}

func TestGenericHarnessRunsFakeBinary(t *testing.T) {
	stubDir := t.TempDir()
	writeTestFile(t, stubDir, "fake-generic", "#!/bin/sh\necho \"$@\"\n")
	if err := os.Chmod(filepath.Join(stubDir, "fake-generic"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	old := appCfg
	defer func() { appCfg = old }()
	appCfg = defaultAppConfig()
	appCfg.Harness = GenericHarnessConfig{
		Executable: "fake-generic",
		Args:       []string{"--headless", "{{model_args}}"},
		ModelFlag:  "--model",
	}

	cmd, err := agentCommand(context.Background(), "PROMPT", "provider/model", "generic", stubDir)
	if err != nil {
		t.Fatal(err)
	}
	out, _, err := runCapture(cmd)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.TrimSpace(out), "--headless --model provider/model PROMPT"; got != want {
		t.Fatalf("generic argv = %q, want %q", got, want)
	}
}

// AC #85: generic forwards only passEnv-listed variables; GH_* is always
// stripped regardless of harness.
func TestHarnessEnvAllowlist(t *testing.T) {
	t.Setenv("KONOHA_HARNESS_PASS", "visible")
	t.Setenv("KONOHA_HARNESS_SECRET", "hidden")
	t.Setenv("GH_TOKEN", "ghs_should_never_reach_child")

	g := genericHarness{cfg: GenericHarnessConfig{PassEnv: []string{"KONOHA_HARNESS_PASS"}}}
	env := g.Env("/repo")
	joined := strings.Join(env, "\n")
	if !strings.Contains(joined, "PWD=/repo") {
		t.Fatalf("env missing PWD: %v", env)
	}
	if !strings.Contains(joined, "KONOHA_HARNESS_PASS=visible") {
		t.Fatalf("env missing passEnv var: %v", env)
	}
	if strings.Contains(joined, "KONOHA_HARNESS_SECRET") {
		t.Fatalf("unlisted secret leaked: %v", env)
	}
	if strings.Contains(joined, "GH_TOKEN") {
		t.Fatalf("GH_* leaked through generic harness: %v", env)
	}

	// opencode keeps today's behavior but still strips GH_*.
	openEnv := strings.Join(opencodeHarness{}.Env("/repo"), "\n")
	if strings.Contains(openEnv, "GH_TOKEN") {
		t.Fatalf("GH_* leaked through opencode harness: %v", openEnv)
	}
}

// AC #85: model resolution is explicit; an unknown/malformed model is rejected.
func TestHarnessResolveModel(t *testing.T) {
	m, err := opencodeHarness{}.ResolveModel("")
	if err != nil || m != defaultAgentModel {
		t.Fatalf("opencode empty model = %q, %v; want %q", m, err, defaultAgentModel)
	}
	if _, err := (opencodeHarness{}).ResolveModel("bad"); err == nil {
		t.Fatal("malformed model must be rejected")
	}
	if m, err := (opencodeHarness{}).ResolveModel("provider/model"); err != nil || m != "provider/model" {
		t.Fatalf("valid model = %q, %v", m, err)
	}
	g := genericHarness{cfg: GenericHarnessConfig{ModelFlag: "--model"}}
	if m, err := g.ResolveModel("provider/model"); err != nil || m != "provider/model" {
		t.Fatalf("generic valid model = %q, %v", m, err)
	}
	gNoFlag := genericHarness{}
	if _, err := gNoFlag.ResolveModel("provider/model"); err == nil || !strings.Contains(err.Error(), "cannot express model") {
		t.Fatalf("generic model without modelFlag must fail: %v", err)
	}
	if m, err := gNoFlag.ResolveModel(""); err != nil || m != "" {
		t.Fatalf("generic empty model = %q, %v; want empty", m, err)
	}
}

func TestAgentCommandRejectsUnknownModelAndHarness(t *testing.T) {
	if _, err := agentCommand(context.Background(), "p", "bad", "opencode", t.TempDir()); err == nil {
		t.Fatal("unknown model must be rejected before exec")
	}
	if _, err := agentCommand(context.Background(), "p", "provider/model", "nope", t.TempDir()); err == nil {
		t.Fatal("unknown harness must be rejected before exec")
	}
}
