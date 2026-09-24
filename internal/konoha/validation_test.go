package konoha

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// §26 extension: the structured validation block parses per-gate id/argv/
// mandatory/timeout/dir; mandatory defaults to true when omitted.
func TestValidationGateConfigParses(t *testing.T) {
	p := writeRepos(t, `
repos:
  - repo: r
    owner: o
    repoPath: /x
    lifecycle:
      validation:
        - id: unit-tests
          argv: ["go", "test", "./..."]
          timeout: 20m
          dir: backend
        - id: lint
          argv: ["golangci-lint", "run"]
          mandatory: false
`)
	repos, err := LoadReposConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	gates := repos[0].Lifecycle.Validation
	if len(gates) != 2 {
		t.Fatalf("got %d gates, want 2", len(gates))
	}
	if gates[0].ID != "unit-tests" || len(gates[0].Argv) != 3 || gates[0].Dir != "backend" {
		t.Fatalf("unexpected gate[0]: %+v", gates[0])
	}
	if !gates[0].isMandatory() {
		t.Fatal("mandatory must default to true")
	}
	if gates[1].isMandatory() {
		t.Fatal("explicit mandatory:false must be honored")
	}
	if got := gates[0].timeout(); got != 20*time.Minute {
		t.Fatalf("timeout = %v, want 20m", got)
	}
	if got := gates[1].timeout(); got != 10*time.Minute {
		t.Fatalf("unset timeout = %v, want default 10m", got)
	}
}

// §26: malformed gate profiles fail at load (never at run time).
func TestValidationGateConfigRejects(t *testing.T) {
	bad := []string{
		`{validation: [{id: a, argv: [x]}, {id: a, argv: [y]}]}`,         // duplicate id
		`{validation: [{id: "", argv: [x]}]}`,                            // empty id
		`{validation: [{id: a}]}`,                                        // empty argv
		`{validation: [{id: a, argv: []}]}`,                              // empty argv
		`{validation: [{id: a, argv: [x], dir: "../etc"}]}`,              // traversal
		`{validation: [{id: a, argv: [x], dir: "/etc"}]}`,                // absolute dir
		`{validation: [{id: a, argv: [x], timeout: "-1s"}]}`,             // negative timeout
		`{validation: [{id: a, argv: [x], timeout: "notaduration"}]}`,    // bad timeout
		`{validation: [{id: a, argv: [x]}], validationCmd: "make test"}`, // ambiguous
		`{validationCmd: "make test", validation: [{id: a, argv: [x]}]}`, // ambiguous
	}
	for _, lc := range bad {
		p := writeRepos(t, `repos: [{repo: r, owner: o, repoPath: /x, lifecycle:`+lc+`}]`)
		if _, err := LoadReposConfig(p); err == nil {
			t.Fatalf("expected rejection for lifecycle %s", lc)
		}
	}
}

// §26: with several gates, all run (no early exit) and an advisory failure does
// not fail the aggregate; a mandatory failure does.
func TestValidateRepoMultiGate(t *testing.T) {
	dir := t.TempDir()
	mandatory, advisory := true, false

	in := PublishInput{RepoPath: dir, Validation: []ValidationGate{
		{ID: "advisory", Argv: []string{"sh", "-c", "echo advisory-ran; exit 3"}, Mandatory: &advisory},
		{ID: "mandatory", Argv: []string{"sh", "-c", "echo mandatory-ran; exit 0"}, Mandatory: &mandatory},
	}}
	ok, skipped, out, err := validateRepo(in)
	if err != nil || skipped {
		t.Fatalf("ok=%v skipped=%v err=%v", ok, skipped, err)
	}
	if !ok {
		t.Fatal("an advisory failure must not fail the aggregate")
	}
	if !strings.Contains(out, "advisory-ran") || !strings.Contains(out, "mandatory-ran") {
		t.Fatalf("every gate must run (no early exit), got %q", out)
	}

	// Mandatory failure: aggregate fails, but both gates still ran.
	in.Validation[1].Argv = []string{"sh", "-c", "echo mandatory-fail; exit 5"}
	ok, _, out, err = validateRepo(in)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("a mandatory gate failure must fail the aggregate")
	}
	if !strings.Contains(out, "advisory-ran") || !strings.Contains(out, "mandatory-fail") {
		t.Fatalf("every gate must still run after a failure, got %q", out)
	}
}

// §40: a gate that cannot start is ERROR and fails the aggregate, even when
// advisory (fail closed).
func TestValidateRepoGateStartErrorFailsClosed(t *testing.T) {
	dir := t.TempDir()
	advisory := false
	_, _, out, err := validateRepo(PublishInput{RepoPath: dir, Validation: []ValidationGate{
		{ID: "missing", Argv: []string{"definitely-not-a-real-binary-xyz"}, Mandatory: &advisory},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "ERROR") {
		t.Fatalf("a gate that cannot start must record ERROR, got %q", out)
	}
	ok, _, _, _ := validateRepo(PublishInput{RepoPath: dir, Validation: []ValidationGate{
		{ID: "missing", Argv: []string{"definitely-not-a-real-binary-xyz"}, Mandatory: &advisory},
	}})
	if ok {
		t.Fatal("an ERROR gate must fail closed")
	}
}

// §23/§26: gates run with the scrubbed agent env (no GH_*).
func TestValidateRepoGatesScrubbedEnv(t *testing.T) {
	t.Setenv("GH_TOKEN", "sekret-gate-token")
	dir := t.TempDir()
	mandatory := true
	_, _, out, err := validateRepo(PublishInput{RepoPath: dir, Validation: []ValidationGate{
		{ID: "env", Argv: []string{"sh", "-c", "env"}, Mandatory: &mandatory},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "GH_TOKEN=") {
		t.Fatalf("gate env must be scrubbed of GitHub credentials, got %q", out)
	}
}

// §26: a repo with neither field inherits the process default; a repo that
// declares either form wins.
func TestValidationGatesResolveDefaults(t *testing.T) {
	saved := appCfg
	t.Cleanup(func() { appCfg = saved })
	appCfg = defaultAppConfig()
	appCfg.Defaults.Lifecycle.Validation = []ValidationGate{{ID: "g", Argv: []string{"true"}}}

	var lc LifecycleConfig
	if got := ValidationGates(lc); len(got) != 1 || got[0].ID != "g" {
		t.Fatalf("unset repo must inherit the default gates, got %+v", got)
	}

	lc.Validation = []ValidationGate{{ID: "repo", Argv: []string{"true"}}}
	if got := ValidationGates(lc); len(got) != 1 || got[0].ID != "repo" {
		t.Fatalf("per-repo gates must win, got %+v", got)
	}

	cmd := LifecycleConfig{ValidationCmd: "make test"}
	if g := ValidationGates(cmd); g != nil {
		t.Fatalf("a legacy-command repo has no gates, got %+v", g)
	}
	if got := ValidationCommand(cmd); got != "make test" {
		t.Fatalf("ValidationCommand = %q, want make test", got)
	}
}

// cycles/<id>/verification/<gate>.stdout|stderr, redacted.
func TestValidateRepoGateEvidenceRedacted(t *testing.T) {
	root := withEvidenceDir(t)
	RegisterSecrets("gate-secret-42")
	dir := t.TempDir()
	mandatory := true
	_, _, _, err := validateRepo(PublishInput{
		RepoPath: dir, CycleID: "ducklambda-issue-7",
		Validation: []ValidationGate{
			{ID: "unit-tests", Argv: []string{"sh", "-c", "echo gate-secret-42; echo err-line 1>&2; exit 1"}, Mandatory: &mandatory},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(root, "cycles", "ducklambda-issue-7", "verification")
	stdout, err := os.ReadFile(filepath.Join(base, "unit-tests.stdout"))
	if err != nil {
		t.Fatalf("gate stdout evidence missing: %v", err)
	}
	if strings.Contains(string(stdout), "gate-secret-42") || !strings.Contains(string(stdout), redacted) {
		t.Fatalf("gate stdout must be redacted, got %q", stdout)
	}
	stderr, err := os.ReadFile(filepath.Join(base, "unit-tests.stderr"))
	if err != nil {
		t.Fatalf("gate stderr evidence missing: %v", err)
	}
	if !strings.Contains(string(stderr), "err-line") {
		t.Fatalf("gate stderr not persisted, got %q", stderr)
	}
}
