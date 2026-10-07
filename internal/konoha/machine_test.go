package konoha

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// fakeMachineProvider is a negotiation stub that advertises a chosen capability
// set; it never creates a real machine.
type fakeMachineProvider struct{ caps []MachineCapability }

func (f fakeMachineProvider) Name() string                                         { return "fake" }
func (f fakeMachineProvider) Capabilities() []MachineCapability                    { return f.caps }
func (f fakeMachineProvider) Create(context.Context, MachineSpec) (Machine, error) { return nil, nil }
func (f fakeMachineProvider) Reattach(context.Context, MachineSpec) (Machine, error) {
	return nil, nil
}
func (f fakeMachineProvider) Destroy(context.Context, MachineSpec) error  { return nil }
func (f fakeMachineProvider) List(context.Context) ([]MachineInfo, error) { return nil, nil }
func (f fakeMachineProvider) Ping(context.Context) error                  { return nil }

func withTestAppConfig(t *testing.T, egress EgressConfig) {
	t.Helper()
	saved := appCfg
	appCfg = defaultAppConfig()
	appCfg.Egress = egress
	t.Cleanup(func() { appCfg = saved })
}

// AC3: the Command type exposes structured argv ONLY — there is no shell-string
// field a caller could smuggle `sh -c` through.
func TestMachineCommandHasNoShellStringForm(t *testing.T) {
	rt := reflect.TypeOf(Command{})
	if f, ok := rt.FieldByName("Argv"); !ok || f.Type != reflect.TypeOf([]string{}) {
		t.Fatalf("Command.Argv must be []string, got %+v", f)
	}
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		name := strings.ToLower(f.Name)
		for _, bad := range []string{"shell", "commandline", "cmdline", "script"} {
			if strings.Contains(name, bad) {
				t.Fatalf("Command field %q reintroduces a shell-string form", f.Name)
			}
		}
		if f.Type.Kind() == reflect.String && (name == "command" || name == "cmd") {
			t.Fatalf("Command field %q is a shell-string form", f.Name)
		}
	}
}

// AC1: the local provider reproduces current host/worktree behavior exactly —
// an owned worktree is claimed, the child runs with the §23 scrubbed env (#103
// advisory egress applied), and Read/Write stay inside the worktree.
func TestLocalProviderReproducesHostWorktreeBehavior(t *testing.T) {
	withTestAppConfig(t, EgressConfig{Proxy: "http://127.0.0.1:8888", AllowOut: []string{"api.anthropic.com"}})
	t.Setenv("GH_TOKEN", "must-be-stripped")
	t.Setenv("KONOHA_MACHINE_TEST", "kept")
	repo, base := initGitRepo(t)
	dir := worktreeDir(repo, "machine", 7)

	p := newLocalProvider()
	spec := MachineSpec{WorkflowID: "wf-1", RepoPath: repo, Dir: dir, BaseSHA: base}
	m, err := p.Create(context.Background(), spec)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if m.ID() != dir {
		t.Fatalf("machine id = %q, want %q", m.ID(), dir)
	}
	if _, ok := readOwner(dir); !ok {
		t.Fatal("create must leave an ownership claim (§4.4)")
	}

	// §23: a GitHub token in the process env is stripped; a caller-supplied
	// GH_TOKEN cannot re-add it; the #103 advisory proxy is applied.
	if got := runMachine(t, m, "printenv", "GH_TOKEN"); got.ExitCode == 0 || strings.TrimSpace(got.Stdout) != "" {
		t.Fatalf("GH_TOKEN leaked into the machine env: %+v", got)
	}
	leak, err := m.Execute(context.Background(), Command{Argv: []string{"printenv", "GH_TOKEN"}, Env: []string{"GH_TOKEN=leak"}})
	if err != nil || leak.ExitCode == 0 || strings.TrimSpace(leak.Stdout) != "" {
		t.Fatalf("caller-supplied GH_TOKEN leaked: %+v err=%v", leak, err)
	}
	if got := runMachine(t, m, "printenv", "HTTPS_PROXY"); strings.TrimSpace(got.Stdout) != "http://127.0.0.1:8888" {
		t.Fatalf("advisory egress proxy not applied: %+v", got)
	}
	if got := runMachine(t, m, "printenv", "KONOHA_MACHINE_TEST"); strings.TrimSpace(got.Stdout) != "kept" {
		t.Fatalf("non-credential env must pass through: %+v", got)
	}

	// Structured argv execution + a nonzero exit is reported, not an error.
	if got := runMachine(t, m, "printenv", "KONOHA_DOES_NOT_EXIST"); got.ExitCode != 1 {
		t.Fatalf("missing env exit code = %d, want 1", got.ExitCode)
	}

	// ReadFile/WriteFile round-trip inside the worktree; a `..` escape fails.
	if err := m.WriteFile(context.Background(), "sub/note.txt", []byte("hi")); err != nil {
		t.Fatalf("write: %v", err)
	}
	b, err := m.ReadFile(context.Background(), "sub/note.txt")
	if err != nil || string(b) != "hi" {
		t.Fatalf("read = %q err=%v", b, err)
	}
	if _, err := m.ReadFile(context.Background(), "../escape.txt"); err == nil {
		t.Fatal("a `..` path must be rejected")
	}
	if _, err := m.ReadFile(context.Background(), "/etc/hosts"); err == nil {
		t.Fatal("an absolute path must be rejected")
	}
}

// env parity: machineEnv with an unconfigured egress block equals today's
// scrubbed pipeline env (cmdEnv) exactly.
func TestMachineEnvParityWithScrubbedPipeline(t *testing.T) {
	withTestAppConfig(t, EgressConfig{})
	t.Setenv("GH_TOKEN", "x")
	t.Setenv("KONOHA_MACHINE_TEST", "y")
	dir := t.TempDir()
	got := envMap(machineEnv(dir, nil))
	want := envMap(cmdEnv(dir))
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("machineEnv[%q] = %q, want %q (parity with cmdEnv)", k, got[k], v)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("machineEnv added/removed vars: got %d want %d", len(got), len(want))
	}
}

// AC2 + AC4: a capability the provider does not advertise is SKIPPED (never a
// false success), and a required-but-missing capability fails closed.
func TestMachineCapabilityNegotiation(t *testing.T) {
	local := newLocalProvider()
	neg := negotiateCapabilities(local, machineCapabilities)
	if len(neg.Outcomes) != len(machineCapabilities) {
		t.Fatalf("negotiation outcomes = %d, want %d", len(neg.Outcomes), len(machineCapabilities))
	}
	for _, o := range neg.Outcomes {
		if o.Status != capabilitySkipped {
			t.Fatalf("local capability %s = %s, want SKIPPED", o.Capability, o.Status)
		}
	}
	if err := requireCapabilities(local, []MachineCapability{machineCapSnapshot}); err == nil {
		t.Fatal("requiring an absent capability must fail closed (§40)")
	}
	if err := requireCapabilities(local, []MachineCapability{"bogus"}); err == nil {
		t.Fatal("an unknown capability must fail closed")
	}

	advertising := fakeMachineProvider{caps: []MachineCapability{machineCapSnapshot}}
	if got := negotiateCapabilities(advertising, []MachineCapability{machineCapSnapshot}); got.Outcomes[0].Status != capabilityApplied {
		t.Fatalf("advertised capability = %s, want APPLIED", got.Outcomes[0].Status)
	}
	if err := requireCapabilities(advertising, []MachineCapability{machineCapSnapshot, machineCapClone}); err == nil {
		t.Fatal("a partially-satisfied requirement must fail closed")
	}
	if err := requireCapabilities(advertising, []MachineCapability{machineCapSnapshot}); err != nil {
		t.Fatalf("an advertised capability must be accepted: %v", err)
	}
}

// AC4: claim authority is enforced — a foreign workflow cannot create over,
// reattach to, or destroy an owned worktree.
func TestLocalProviderHonorsClaimAuthority(t *testing.T) {
	withTestAppConfig(t, EgressConfig{})
	repo, base := initGitRepo(t)
	dir := worktreeDir(repo, "machine", 8)
	ctx := context.Background()
	p := newLocalProvider()

	owned := MachineSpec{WorkflowID: "wf-owner", RepoPath: repo, Dir: dir, BaseSHA: base}
	if _, err := p.Create(ctx, owned); err != nil {
		t.Fatalf("owner create: %v", err)
	}
	if _, err := p.Reattach(ctx, MachineSpec{WorkflowID: "wf-owner", RepoPath: repo, Dir: dir}); err != nil {
		t.Fatalf("owner reattach: %v", err)
	}
	if _, err := p.Reattach(ctx, MachineSpec{WorkflowID: "wf-thief", RepoPath: repo, Dir: dir}); err == nil {
		t.Fatal("foreign reattach must fail closed")
	}
	if _, err := p.Create(ctx, MachineSpec{WorkflowID: "wf-thief", RepoPath: repo, Dir: dir, BaseSHA: base}); err == nil {
		t.Fatal("foreign create over an owned worktree must fail closed")
	}
	// A non-owner destroy is a no-op: the worktree survives.
	if err := p.Destroy(ctx, MachineSpec{WorkflowID: "wf-thief", RepoPath: repo, Dir: dir}); err != nil {
		t.Fatalf("foreign destroy errored: %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("foreign destroy removed an owned worktree: %v", err)
	}
	// The owner may destroy it.
	if err := p.Destroy(ctx, owned); err != nil {
		t.Fatalf("owner destroy: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("owner destroy left the worktree: %v", err)
	}
}

// AC: no agent code path manages infrastructure — the provider/machine symbols
// live only in machine.go (the workflow-owned foundation). Any agent or
// activity file referencing them would be a boundary violation.
func TestNoAgentCodeManagesInfrastructure(t *testing.T) {
	infra := []string{"newMachineRegistry", "localProvider", "localMachine", "machineLimits", "MachineSpec", "MachineInfo", "MachineCapability"}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if f == "machine.go" || strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, sym := range infra {
			if strings.Contains(string(src), sym) {
				t.Fatalf("%s references machine infrastructure %q; only the workflow may manage machines (#121/#142)", f, sym)
			}
		}
	}
}

// AC (per #142): an adapter cannot bypass the control plane. machine.go
// references no GitHub runner, policy gate, label projection or reconciliation,
// so it has no path to mutate state or bypass a gate.
func TestMachineAdapterCannotBypassControlPlane(t *testing.T) {
	src, err := os.ReadFile("machine.go")
	if err != nil {
		t.Fatal(err)
	}
	forbidden := []string{
		"ghRunner", "ghTokenRunner", "appToken(", "ProjectLabels", "ApplyIssueLabels",
		"EnsureRepoLabels", "EnsureIssueState", "EnsurePullRequest", "MergePullRequest",
		"EvaluateClosePolicy", "EvaluateCompletion", "RecordActivation", "RecordChange",
	}
	for _, bad := range forbidden {
		if strings.Contains(string(src), bad) {
			t.Fatalf("machine.go references %q; an adapter must not bypass the control plane (#142)", bad)
		}
	}
}

// AC: the docs record the machine model and the #92 decision (no sandbox
// provider; local stands) with the boundary called out.
func TestMachineDocsRecordProviderModelAndNoSandbox(t *testing.T) {
	readme, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(readme)
	for _, want := range []string{"Machine providers (#121)", "local", "no sandbox"} {
		if !strings.Contains(s, want) {
			t.Fatalf("README must document %q for #121", want)
		}
	}
	agents, err := os.ReadFile(filepath.Join("..", "..", "AGENTS.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(agents), "machine.go") {
		t.Fatal("AGENTS.md must carry a machine.go note (#121)")
	}
}

func runMachine(t *testing.T, m Machine, argv ...string) CommandResult {
	t.Helper()
	res, err := m.Execute(context.Background(), Command{Argv: argv})
	if err != nil {
		t.Fatalf("execute %v: %v", argv, err)
	}
	return res
}

func envMap(env []string) map[string]string {
	out := make(map[string]string, len(env))
	for _, kv := range env {
		if i := strings.IndexByte(kv, '='); i > 0 {
			out[kv[:i]] = kv[i+1:]
		}
	}
	return out
}
