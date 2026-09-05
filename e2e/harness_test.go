//go:build e2e

// E2E harness (#47): a real Temporal dev-server + the real worker binary with
// stateful fake `gh`/`opencode` on PATH and a real local fixture git repo +
// bare remote. Run:
//
//	go test -tags=e2e ./e2e/...
//
// Network-independent: GitHub API/opencode are faked; git pushes go to the
// local bare remote via KONOHA_GIT_BASE.
package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/testsuite"
)

var (
	e2eClient     client.Client
	e2eDev        *testsuite.DevServer
	e2eHostPort   string
	fakeBinDir    string
	fakeOpenDir   string
	fakeStateDir  string
	workerProcess *exec.Cmd
	fixture       *gitFixture
)

type gitFixture struct{ workdir, remote string }

func TestMain(m *testing.M) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dev, err := testsuite.StartDevServer(ctx, testsuite.DevServerOptions{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "start dev-server: %v\n", err)
		os.Exit(1)
	}
	e2eDev, e2eClient, e2eHostPort = dev, dev.Client(), dev.FrontendHostPort()

	fakeBinDir, _ = os.MkdirTemp("", "konoha-fakebin")
	fakeOpenDir, _ = os.MkdirTemp("", "konoha-fakeopencode")
	fakeStateDir, _ = os.MkdirTemp("", "konoha-fakestate")
	e2eDir, _ := os.Getwd()
	repoRoot := filepath.Dir(e2eDir) // parent of the e2e test package
	buildFakes(repoRoot)

	fixture = createFixtureRepo()

	env := append(os.Environ(),
		"TEMPORAL_ADDRESS="+e2eHostPort,
		"PATH="+fakeBinDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"KONOHA_TOKEN_FIXED=e2e-token",
		"KONOHA_GIT_BASE="+fixture.remote,
		"FAKE_GIT_REMOTE="+fixture.remote,
		"FAKE_GH_STATE="+fakeStateDir,
		"FAKE_OPENCODE_DIR="+fakeOpenDir,
		"KONOHA_CONFIG_DIR="+filepath.Join(repoRoot, "configs"),
	)

	cmd := exec.Command("go", "run", "./cmd/konoha")
	cmd.Dir = repoRoot
	cmd.Env = env
	// Detach worker output so the subprocess does not hold the test's I/O pipes
	// open past TestMain exit (avoids "Test I/O incomplete").
	logf, _ := os.Create(filepath.Join(fakeStateDir, "worker.log"))
	cmd.Stdout = logf
	cmd.Stderr = logf
	workerProcess = cmd
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "start worker: %v\n", err)
		os.Exit(1)
	}
	time.Sleep(3 * time.Second)

	code := m.Run()
	if workerProcess.Process != nil {
		_ = workerProcess.Process.Kill()
	}
	_ = dev.Stop()
	os.Exit(code)
}

func buildFakes(repoRoot string) {
	for name, pkg := range map[string]string{"gh": "e2e/fakegh", "opencode": "e2e/fakeopencode"} {
		c := exec.Command("go", "build", "-tags=e2e", "-o", filepath.Join(fakeBinDir, name), "./"+pkg)
		c.Dir = repoRoot
		if out, err := c.CombinedOutput(); err != nil {
			panic(fmt.Sprintf("build fake %s: %s: %v", name, out, err))
		}
	}
}

func createFixtureRepo() *gitFixture {
	workdir, _ := os.MkdirTemp("", "konoha-fixture")
	git := func(dir string, args ...string) {
		c := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := c.CombinedOutput(); err != nil {
			panic(fmt.Sprintf("git %v: %s: %v", args, out, err))
		}
	}
	git(workdir, "init", "-q", "-b", "main")
	git(workdir, "config", "user.email", "e2e@konoha")
	git(workdir, "config", "user.name", "e2e")
	os.WriteFile(filepath.Join(workdir, "README.md"), []byte("# fixture\n"), 0o644)
	git(workdir, "add", ".")
	git(workdir, "commit", "-qm", "init")

	// Remote layout mirrors KONOHA_GIT_BASE/<owner>/<repo>.git
	basedir, _ := os.MkdirTemp("", "konoha-remote")
	remote := filepath.Join(basedir, "o", "r.git")
	os.MkdirAll(filepath.Dir(remote), 0o755)
	git(workdir, "clone", "-q", "--bare", workdir, remote)
	return &gitFixture{workdir: workdir, remote: basedir}
}

// setAgentFixture writes the canned opencode output for a role.
func setAgentFixture(role, content string) {
	if err := os.WriteFile(filepath.Join(fakeOpenDir, role+".txt"), []byte(content), 0o644); err != nil {
		panic(err)
	}
}

// startIssueCycle starts a workflow via the client (direct-sweep mode).
func startIssueCycle(num int, lifecycle map[string]interface{}) (client.WorkflowRun, error) {
	if lifecycle == nil {
		lifecycle = defaultLifecycle()
	}
	in := map[string]interface{}{
		"owner": "o", "repo": "r", "issue": num, "repoPath": fixture.workdir,
		"lifecycle": lifecycle,
	}
	return e2eClient.ExecuteWorkflow(context.Background(), client.StartWorkflowOptions{
		ID:        "r-issue-" + strconv.Itoa(num),
		TaskQueue: "review-queue",
	}, "IssueCycleWorkflow", in)
}

func defaultLifecycle() map[string]interface{} {
	return map[string]interface{}{
		"mode": "supervised-autonomous", "developmentMaxComplexity": 4,
		"autonomy": map[string]interface{}{"closePolicy": "risk-based"},
	}
}
