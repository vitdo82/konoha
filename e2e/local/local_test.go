//go:build e2e

// Hermetic local-runtime E2E (#212): the real pipeline on `prbot run` with fake
// gh/opencode and a fixture remote, and NO Temporal dev-server. TEMPORAL_ADDRESS
// points at a closed port: a dial would fail the run.
package local

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type fixture struct{ workdir, remoteBase, remote string }

func createFixtureRepo(t *testing.T) fixture {
	t.Helper()
	workdir := t.TempDir()
	run := func(dir string, args ...string) {
		c := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s: %v", args, out, err)
		}
	}
	run(workdir, "init", "-q", "-b", "main")
	run(workdir, "config", "user.email", "e2e@konoha")
	run(workdir, "config", "user.name", "e2e")
	if err := os.WriteFile(filepath.Join(workdir, "README.md"), []byte("# fixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(workdir, "add", ".")
	run(workdir, "commit", "-qm", "init")

	base := t.TempDir()
	remote := filepath.Join(base, "o", "r.git")
	if err := os.MkdirAll(filepath.Dir(remote), 0o755); err != nil {
		t.Fatal(err)
	}
	run(workdir, "clone", "-q", "--bare", workdir, remote)
	return fixture{workdir: workdir, remoteBase: base, remote: remote}
}

func buildFakes(t *testing.T, repoRoot string) string {
	t.Helper()
	bin := t.TempDir()
	for name, pkg := range map[string]string{"gh": "e2e/fakegh", "opencode": "e2e/fakeopencode"} {
		c := exec.Command("go", "build", "-tags=e2e", "-o", filepath.Join(bin, name), "./"+pkg)
		c.Dir = repoRoot
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("build fake %s: %s: %v", name, out, err)
		}
	}
	return bin
}

func TestScenarioLocalSmallHappyPath(t *testing.T) {
	e2eDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	repoRoot := filepath.Dir(filepath.Dir(e2eDir)) // <root>/e2e/local → <root>
	bin := buildFakes(t, repoRoot)
	fx := createFixtureRepo(t)

	stateDir := t.TempDir()
	openDir := t.TempDir()
	cfgDir := t.TempDir()
	evidenceDir := filepath.Join(t.TempDir(), "evidence")

	seed := `{"issues":[{"number":1,"state":"open","title":"t","body":"b",` +
		`"labels":["enhancement"],"isPr":false}],"labels":["enhancement"],"next":2}`
	if err := os.WriteFile(filepath.Join(stateDir, "store.json"), []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}
	for role, content := range map[string]string{
		"analyst": `{"summary":"s","spec":"generated-temp-spec",` +
			`"requirements":["r"],"acceptanceCriteria":["Given x When y Then z"]}`,
		"estimator": `{"complexity":3,"confidence":"high","rationale":"small"}`,
		"developer": `{"summary":"the change","concerns":[]}`,
		"reviewer":  "VERDICT:APPROVE\nBLOCKING:\n- none\nNITS:\n- none\nCONCLUDE: ok\n",
	} {
		if err := os.WriteFile(filepath.Join(openDir, role+".txt"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	app := fmt.Sprintf("evidence:\n  dir: %s\nruntime:\n  mode: local\n  local:\n    once: true\n    maxCycles: 1\n    approvalWait: 1s\n", evidenceDir)
	if err := os.WriteFile(filepath.Join(cfgDir, "konoha.yaml"), []byte(app), 0o600); err != nil {
		t.Fatal(err)
	}
	repos := fmt.Sprintf("repos:\n  - repo: r\n    owner: o\n    repoPath: %s\n    lifecycle:\n      mode: supervised-autonomous\n      developmentMaxComplexity: 4\n      autonomy:\n        closePolicy: risk-based\n", fx.workdir)
	if err := os.WriteFile(filepath.Join(cfgDir, "repos.yaml"), []byte(repos), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("go", "run", "./cmd/konoha", "run", "--once", "--repo", "r", "--issue", "1")
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(),
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"TEMPORAL_ADDRESS=127.0.0.1:1", // closed port: a dial would fail the run
		"KONOHA_CONFIG_DIR="+cfgDir,
		"KONOHA_TOKEN_FIXED=e2e-token",
		"KONOHA_GIT_BASE="+fx.remoteBase,
		"FAKE_GIT_REMOTE="+fx.remoteBase,
		"FAKE_GH_STATE="+stateDir,
		"FAKE_OPENCODE_DIR="+openDir,
		"GH_TOKEN=must-not-reach-the-agent",
		"GITHUB_TOKEN=must-not-reach-the-agent",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("prbot run failed (no Temporal server): %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "Temporal-only features inactive") {
		t.Fatalf("startup warning missing:\n%s", out)
	}

	// Fake GitHub store: issue closed and exactly one merged PR.
	var store struct {
		Issues []struct {
			Number int    `json:"number"`
			State  string `json:"state"`
			IsPR   bool   `json:"isPr"`
		} `json:"issues"`
	}
	raw, err := os.ReadFile(filepath.Join(stateDir, "store.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &store); err != nil {
		t.Fatalf("fake state: %v", err)
	}
	prs, closed := 0, false
	for _, it := range store.Issues {
		if it.IsPR {
			prs++
			if it.State != "merged" {
				t.Fatalf("PR state = %q, want merged", it.State)
			}
		}
		if it.Number == 1 && it.State == "closed" {
			closed = true
		}
	}
	if !closed {
		t.Fatalf("issue #1 not closed: %s", raw)
	}
	if prs != 1 {
		t.Fatalf("PRs = %d, want exactly 1", prs)
	}

	// outcome.json: same terminal evidence shape as the Temporal path.
	raw, err = os.ReadFile(filepath.Join(evidenceDir, "cycles", "r-issue-1", "outcome.json"))
	if err != nil {
		t.Fatalf("outcome.json: %v", err)
	}
	var outcome struct {
		TerminalState       string `json:"terminalState"`
		AgentOutcome        string `json:"agentOutcome"`
		VerificationOutcome string `json:"verificationOutcome"`
		PolicyDigest        string `json:"policyDigest"`
	}
	if err := json.Unmarshal(raw, &outcome); err != nil {
		t.Fatalf("decode outcome: %v", err)
	}
	if outcome.TerminalState != "CLOSED" || outcome.AgentOutcome == "" || outcome.VerificationOutcome == "" || outcome.PolicyDigest == "" {
		t.Fatalf("outcome evidence incomplete: %+v", outcome)
	}

	// #93: the worktree is torn down on the normal path too.
	worktree := filepath.Join(filepath.Dir(fx.workdir), ".konoha", "r-issue-1")
	if _, err := os.Stat(worktree); !os.IsNotExist(err) {
		t.Fatalf("worktree %s still present after the cycle (err=%v)", worktree, err)
	}

	// §23: no GitHub credential reaches an agent in local mode either.
	agentEnv, err := os.ReadFile(filepath.Join(openDir, "agent.env"))
	if err != nil {
		t.Fatalf("agent env dump: %v", err)
	}
	for _, line := range strings.Split(string(agentEnv), "\n") {
		if strings.HasPrefix(line, "GH_") || strings.HasPrefix(line, "GITHUB_TOKEN=") {
			t.Fatalf("GitHub credential reached the agent: %q", line)
		}
	}
}
