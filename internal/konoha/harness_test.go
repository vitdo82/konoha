package konoha

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// #96 AC: a pinned binary is staged into the agent's PATH only after its sha256
// is verified; a mismatch fails closed.
func TestProvisionStagesAndVerifiesSHA256(t *testing.T) {
	repoPath, _ := initGitRepo(t)
	srcDir := t.TempDir()
	src := filepath.Join(srcDir, "pinned-agent")
	body := "#!/bin/sh\necho pinned\n"
	if err := os.WriteFile(src, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(body))
	goodSHA := hex.EncodeToString(sum[:])

	g := genericHarness{cfg: GenericHarnessConfig{
		Executable: "pinned-agent", Binary: src, BinarySHA256: goodSHA,
	}}
	pa, err := prepareAgent(context.Background(), g, repoPath, nil)
	if err != nil {
		t.Fatalf("provision with correct sha256: %v", err)
	}
	if pa.Executable == "" {
		t.Fatal("expected a staged executable path")
	}
	if got, _ := os.ReadFile(pa.Executable); string(got) != body {
		t.Fatalf("staged binary content mismatch: %q", got)
	}
	if info, err := os.Stat(pa.Executable); err != nil || info.Mode().Perm()&0o100 == 0 {
		t.Fatalf("staged binary must be executable: %v %v", info, err)
	}
	if !strings.HasPrefix(pa.Executable, repoPath) {
		t.Fatalf("staged binary must live under the agent dir %s: %s", repoPath, pa.Executable)
	}
	if o := outcomeFor(t, pa.Outcomes, "provision"); o.Status != capabilityApplied {
		t.Fatalf("provision outcome = %+v, want APPLIED", o)
	}

	// A mismatched digest fails closed and records FAILED (never a false success).
	g.cfg.BinarySHA256 = strings.Repeat("0", 64)
	pa, err = prepareAgent(context.Background(), g, repoPath, nil)
	if err == nil {
		t.Fatal("sha256 mismatch must fail closed")
	}
	if o := outcomeFor(t, pa.Outcomes, "provision"); o.Status != capabilityFailed {
		t.Fatalf("mismatch outcome = %+v, want FAILED", o)
	}
}

// #96 AC: version capture is best-effort — a working binary reports its version,
// a failing one contributes nothing and never fails the run.
func TestVersionCaptureBestEffort(t *testing.T) {
	repoPath, _ := initGitRepo(t)
	stubDir := t.TempDir()
	writeTestFile(t, stubDir, "v-good", "#!/bin/sh\necho 'agent 1.2.3'\n")
	writeTestFile(t, stubDir, "v-bad", "#!/bin/sh\nexit 7\n")
	for _, n := range []string{"v-good", "v-bad"} {
		if err := os.Chmod(filepath.Join(stubDir, n), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	if got := (genericHarness{cfg: GenericHarnessConfig{Executable: "v-good"}}).Version(context.Background(), repoPath); got != "agent 1.2.3" {
		t.Fatalf("version = %q, want %q", got, "agent 1.2.3")
	}
	if got := (genericHarness{cfg: GenericHarnessConfig{Executable: "v-bad"}}).Version(context.Background(), repoPath); got != "" {
		t.Fatalf("a failing --version must contribute nothing, got %q", got)
	}

	// prepareAgent records version APPLIED either way and never errors on it.
	pa, err := prepareAgent(context.Background(), genericHarness{cfg: GenericHarnessConfig{Executable: "v-bad"}}, repoPath, nil)
	if err != nil {
		t.Fatalf("a failing version capture must not fail the run: %v", err)
	}
	if o := outcomeFor(t, pa.Outcomes, "version"); o.Status != capabilityApplied {
		t.Fatalf("version outcome = %+v, want APPLIED", o)
	}
}

// #96 AC: stdin_file delivers the prompt over stdin and never puts it in argv,
// writing it outside the tracked tree so it cannot dirty status.
func TestStdinFileKeepsPromptOutOfArgv(t *testing.T) {
	repoPath, _ := initGitRepo(t)
	stubDir := t.TempDir()
	writeTestFile(t, stubDir, "fake-agent", "#!/bin/sh\necho \"ARGV:$@\"\necho STREAM\ncat\n")
	if err := os.Chmod(filepath.Join(stubDir, "fake-agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	old := appCfg
	defer func() { appCfg = old }()
	appCfg = defaultAppConfig()
	appCfg.Harness = GenericHarnessConfig{
		Executable: "fake-agent",
		Args:       []string{"run", "{{model_args}}"},
		ModelFlag:  "--model",
		PromptMode: promptModeStdinFile,
	}

	const secret = "PROMPT-MUST-NOT-APPEAR-IN-ARGV"
	cmd, _, err := agentCommand(context.Background(), secret, "provider/model", "generic", repoPath, LimitsConfig{}, AgentTooling{})
	if err != nil {
		t.Fatal(err)
	}
	out, _, err := runCapture(cmd)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.SplitN(out, "STREAM", 2)[0], secret) {
		t.Fatalf("prompt leaked into argv: %q", out)
	}
	if !strings.Contains(out, "STREAM\n"+secret) {
		t.Fatalf("prompt not delivered over stdin: %q", out)
	}
	if status := strings.TrimSpace(runOut(t, context.Background(), repoPath, "git", "status", "--porcelain")); status != "" {
		t.Fatalf("stdin_file must not dirty the worktree: %q", status)
	}
}

// #96 AC: a harness that does not implement/declare a capability records
// SKIPPED — never a false success (§40).
func TestCapabilitiesSkippedWhenAbsent(t *testing.T) {
	pa, err := prepareAgent(context.Background(), opencodeHarness{}, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"provision", "catalog", "version", "mcp"} {
		if o := outcomeFor(t, pa.Outcomes, name); o.Status != capabilitySkipped {
			t.Fatalf("%s outcome = %+v, want SKIPPED", name, o)
		}
	}
	// A generic harness with nothing configured is likewise all-SKIPPED.
	pa, err = prepareAgent(context.Background(), genericHarness{cfg: GenericHarnessConfig{Executable: "agent"}}, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"provision", "catalog", "mcp"} {
		if o := outcomeFor(t, pa.Outcomes, name); o.Status != capabilitySkipped {
			t.Fatalf("unconfigured %s = %+v, want SKIPPED", name, o)
		}
	}
}

// #96 AC: MCP config is written for a supporting harness and excluded from git;
// a harness without the capability records SKIPPED.
func TestMCPConfigWrittenOrSkipped(t *testing.T) {
	repoPath, _ := initGitRepo(t)
	g := genericHarness{cfg: GenericHarnessConfig{
		Executable: "agent",
		MCP:        &MCPConfig{Path: ".mcp.json", Config: `{"servers":{}}`},
	}}
	pa, err := prepareAgent(context.Background(), g, repoPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	if o := outcomeFor(t, pa.Outcomes, "mcp"); o.Status != capabilityApplied {
		t.Fatalf("mcp outcome = %+v, want APPLIED", o)
	}
	got, err := os.ReadFile(filepath.Join(repoPath, ".mcp.json"))
	if err != nil || string(got) != `{"servers":{}}` {
		t.Fatalf("mcp config not written: %q err=%v", got, err)
	}
	if status := strings.TrimSpace(runOut(t, context.Background(), repoPath, "git", "status", "--porcelain")); status != "" {
		t.Fatalf("mcp config must be git-excluded: %q", status)
	}

	pa, err = prepareAgent(context.Background(), opencodeHarness{}, repoPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	if o := outcomeFor(t, pa.Outcomes, "mcp"); o.Status != capabilitySkipped {
		t.Fatalf("opencode mcp outcome = %+v, want SKIPPED", o)
	}
}

// #96 AC: with a staged catalog, model selection is deterministic and offline.
func TestCatalogOfflineModelResolution(t *testing.T) {
	repoPath, _ := initGitRepo(t)
	catDir := t.TempDir()
	catPath := filepath.Join(catDir, "catalog.json")
	if err := os.WriteFile(catPath, []byte(`["openai/gpt-4","anthropic/claude-3"]`), 0o644); err != nil {
		t.Fatal(err)
	}
	g := genericHarness{cfg: GenericHarnessConfig{Executable: "agent", ModelFlag: "--model", CatalogCache: catPath}}
	pa, err := prepareAgent(context.Background(), g, repoPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	if o := outcomeFor(t, pa.Outcomes, "catalog"); o.Status != capabilityApplied {
		t.Fatalf("catalog outcome = %+v, want APPLIED", o)
	}
	if m, err := resolveHarnessModel(g, "anthropic/claude-3", pa.CatalogPath); err != nil || m != "anthropic/claude-3" {
		t.Fatalf("catalog resolve = %q, %v", m, err)
	}
	// Empty request selects the lexicographically-first entry (deterministic).
	if m, err := resolveHarnessModel(g, "", pa.CatalogPath); err != nil || m != "anthropic/claude-3" {
		t.Fatalf("empty model resolve = %q, %v", m, err)
	}
	// A model absent from the catalog fails closed rather than going to the network.
	if _, err := resolveHarnessModel(g, "vendor/unknown", pa.CatalogPath); err == nil {
		t.Fatal("model absent from staged catalog must fail closed")
	}
	// A malformed catalog fails closed too.
	if _, err := resolveModelFromCatalog([]byte("not json"), "a/b"); err == nil {
		t.Fatal("malformed catalog must fail closed")
	}
}

// #96 AC: config validation gates the new provisioning fields at load.
func TestHarnessProvisionConfigValidation(t *testing.T) {
	write := func(body string) string {
		p := filepath.Join(t.TempDir(), "konoha.yaml")
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	cases := []struct {
		name, body, want string
	}{
		{"prompt mode", "harness:\n  prompt_mode: bogus\n", "prompt_mode"},
		{"sha without binary", "harness:\n  binary_sha256: " + strings.Repeat("a", 64) + "\n", "without harness.generic.binary"},
		{"binary without sha", "harness:\n  binary: /opt/agent\n", "binary_sha256"},
		{"bad sha", "harness:\n  binary: /opt/agent\n  binary_sha256: deadbeef\n", "64-char hex"},
		{"mcp traversal", "harness:\n  mcp:\n    path: ../escape.json\n    config: '{}'\n", "worktree-relative"},
		{"mcp empty config", "harness:\n  mcp:\n    path: .mcp.json\n", "must not be empty"},
		{"catalog without modelFlag", "harness:\n  catalog_cache: /opt/catalog.json\n", "requires harness.generic.modelFlag"},
	}
	for _, tc := range cases {
		if _, err := loadAppConfig(write(tc.body)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: want error containing %q, got %v", tc.name, tc.want, err)
		}
	}
	// A fully-specified block loads.
	if _, err := loadAppConfig(write(
		"harness:\n  executable: agent\n  modelFlag: --model\n  prompt_mode: stdin_file\n" +
			"  binary: /opt/agent\n  binary_sha256: " + strings.Repeat("a", 64) + "\n" +
			"  catalog_cache: /opt/catalog.json\n" +
			"  mcp:\n    path: .mcp.json\n    config: '{}'\n")); err != nil {
		t.Fatalf("valid provisioning block rejected: %v", err)
	}
}

// #96: capability outcomes (incl. the version capture) land in the cycle's
// provisioning.json evidence via the shared AgentEvidence path.
func TestProvisioningEvidenceRecorded(t *testing.T) {
	var got EvidenceRequest
	ev := &AgentEvidence{
		Prompt: "p",
		Provisioning: []CapabilityOutcome{
			{Capability: "version", Status: capabilityApplied, Detail: "agent 1.2.3"},
			{Capability: "provision", Status: capabilitySkipped, Detail: "not configured"},
		},
	}
	writeAgentEvidence(func(r EvidenceRequest) { got = r }, ev, ReviewResult{Verdict: "APPROVE"}, "")
	if !strings.Contains(got.Provisioning, `"capability":"version"`) || !strings.Contains(got.Provisioning, `"status":"SKIPPED"`) {
		t.Fatalf("provisioning not carried into evidence: %q", got.Provisioning)
	}
}

func outcomeFor(t *testing.T, outs []CapabilityOutcome, name string) CapabilityOutcome {
	t.Helper()
	for _, o := range outs {
		if o.Capability == name {
			return o
		}
	}
	t.Fatalf("no outcome for capability %q in %+v", name, outs)
	return CapabilityOutcome{}
}

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

	cmd, _, err := agentCommand(context.Background(), "PROMPT", "provider/model", "generic", stubDir, LimitsConfig{}, AgentTooling{})
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

// §90(a): common cloud/provider credentials are absent from the child agent env
// unless an operator explicitly allowlists one via the generic passEnv. GitHub
// credentials can never be allowlisted.
func TestHarnessEnvStripsCloudCredentials(t *testing.T) {
	creds := []string{
		"AWS_SECRET_ACCESS_KEY", "AWS_ACCESS_KEY_ID", "GOOGLE_APPLICATION_CREDENTIALS",
		"AZURE_CLIENT_SECRET", "OPENAI_API_KEY", "ANTHROPIC_API_KEY", "NPM_TOKEN",
	}
	for _, k := range creds {
		t.Setenv(k, "sekret-"+k)
	}
	t.Setenv("GH_TOKEN", "ghs_never")

	opencodeEnv := strings.Join(opencodeHarness{}.Env("/repo"), "\n")
	for _, k := range creds {
		if strings.Contains(opencodeEnv, k+"=") {
			t.Fatalf("opencode env leaked %s: %v", k, opencodeEnv)
		}
	}

	// Unlisted cloud credential stays out of the generic harness.
	unlisted := strings.Join(genericHarness{cfg: GenericHarnessConfig{PassEnv: []string{"KONOHA_NOPE"}}}.Env("/repo"), "\n")
	if strings.Contains(unlisted, "AWS_ACCESS_KEY_ID=") {
		t.Fatalf("unlisted cloud credential leaked: %v", unlisted)
	}

	// Explicitly allowlisting a cloud var opts it in; GitHub creds never opt in.
	listed := strings.Join(genericHarness{cfg: GenericHarnessConfig{
		PassEnv: []string{"AWS_ACCESS_KEY_ID", "GH_TOKEN"},
	}}.Env("/repo"), "\n")
	if !strings.Contains(listed, "AWS_ACCESS_KEY_ID=sekret-AWS_ACCESS_KEY_ID") {
		t.Fatalf("allowlisted cloud credential must pass: %v", listed)
	}
	if strings.Contains(listed, "GH_TOKEN=") {
		t.Fatalf("GitHub credential must never be allowlistable: %v", listed)
	}
}

// §90(a): validation gates inherit the same scrubbed env (no cloud creds).
func TestValidationGateScrubsCloudCredentials(t *testing.T) {
	t.Setenv("AWS_SECRET_ACCESS_KEY", "sekret")
	t.Setenv("OPENAI_API_KEY", "sk-sekret")
	mandatory := true
	dir := t.TempDir()
	_, _, out, err := validateRepo(PublishInput{RepoPath: dir, Validation: []ValidationGate{
		{ID: "env", Argv: []string{"sh", "-c", "env"}, Mandatory: &mandatory},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "AWS_SECRET_ACCESS_KEY=") || strings.Contains(out, "OPENAI_API_KEY=") {
		t.Fatalf("gate env leaked cloud credentials: %q", out)
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
	if _, _, err := agentCommand(context.Background(), "p", "bad", "opencode", t.TempDir(), LimitsConfig{}, AgentTooling{}); err == nil {
		t.Fatal("unknown model must be rejected before exec")
	}
	if _, _, err := agentCommand(context.Background(), "p", "provider/model", "nope", t.TempDir(), LimitsConfig{}, AgentTooling{}); err == nil {
		t.Fatal("unknown harness must be rejected before exec")
	}
}
