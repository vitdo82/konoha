package konoha

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// writeLocalRunConfig materializes a KONOHA_CONFIG_DIR for the headless runtime.
func writeLocalRunConfig(t *testing.T, once bool, maxCycles int) string {
	t.Helper()
	dir := t.TempDir()
	app := "evidence:\n  dir: " + filepath.Join(dir, "evidence") + "\n" +
		"runtime:\n  mode: local\n  local:\n    sweepInterval: 1h\n    maxCycles: " + strconv.Itoa(maxCycles) + "\n" +
		"    once: " + boolStr(once) + "\n    approvalWait: 1s\n"
	if err := os.WriteFile(filepath.Join(dir, "konoha.yaml"), []byte(app), 0o600); err != nil {
		t.Fatal(err)
	}
	repos := "repos:\n  - repo: r\n    owner: o\n    repoPath: " + dir + "\n" +
		"    lifecycle:\n      mode: supervised-autonomous\n      developmentMaxComplexity: 4\n" +
		"      autonomy:\n        closePolicy: risk-based\n"
	if err := os.WriteFile(filepath.Join(dir, "repos.yaml"), []byte(repos), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KONOHA_CONFIG_DIR", dir)
	return dir
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// useLocalFakes installs the activity seam for the duration of a test.
func useLocalFakes(t *testing.T) *localFakes {
	t.Helper()
	f := newLocalFakes()
	saved := localActivitiesFor
	localActivitiesFor = func() localActivities { return f.acts }
	t.Cleanup(func() { localActivitiesFor = saved })
	return f
}

func TestParseLocalArgs(t *testing.T) {
	got, err := parseLocalArgs([]string{"--once", "--repo", "r", "--issue", "7"})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Once || got.Repo != "r" || got.Issue != 7 {
		t.Fatalf("parsed = %+v", got)
	}
	for _, args := range [][]string{
		{"--bogus"},
		{"--issue", "7"},
		{"--issue", "nope", "--repo", "r"},
		{"--repo"},
	} {
		if _, err := parseLocalArgs(args); err == nil {
			t.Fatalf("args %v must fail", args)
		}
	}
}

// #212: one startup warning names the Temporal-only features local mode skips.
func TestLocalModeWarningNamesDisabledFeatures(t *testing.T) {
	w := localModeWarning()
	for _, want := range []string{"schedules", "webhook", "child", "post-merge", "queue"} {
		if !strings.Contains(w, want) {
			t.Fatalf("warning %q missing %q", w, want)
		}
	}
}

// #212: prbot run never dials TEMPORAL_ADDRESS.
func TestLocalRunDoesNotDialTemporal(t *testing.T) {
	withLocalAppCfg(t)
	writeLocalRunConfig(t, true, 1)
	useLocalFakes(t)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	accepted := make(chan struct{}, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		c.Close()
		accepted <- struct{}{}
	}()
	t.Setenv("TEMPORAL_ADDRESS", ln.Addr().String())

	if err := runLocal([]string{"--once", "--repo", "r", "--issue", "7"}); err != nil {
		t.Fatalf("runLocal: %v", err)
	}
	select {
	case <-accepted:
		t.Fatal("local runtime dialed TEMPORAL_ADDRESS")
	case <-time.After(150 * time.Millisecond):
	}
}

// #212: one sweep starts at most maxCycles cycles per repo.
func TestLocalRunOncePerformsOneSweep(t *testing.T) {
	withLocalAppCfg(t)
	writeLocalRunConfig(t, true, 1)
	f := useLocalFakes(t)

	var mu sync.Mutex
	listed := 0
	f.acts.listUnmanagedIssues = func(context.Context, RepoScope) ([]IssueInput, error) {
		mu.Lock()
		listed++
		mu.Unlock()
		return []IssueInput{
			{Owner: "o", Repo: "r", IssueNum: 1, RepoPath: t.TempDir(), Trigger: "sweep", Lifecycle: localTestLifecycle()},
			{Owner: "o", Repo: "r", IssueNum: 2, RepoPath: t.TempDir(), Trigger: "sweep", Lifecycle: localTestLifecycle()},
		}, nil
	}
	if err := runLocal([]string{"--once", "--repo", "r"}); err != nil {
		t.Fatalf("runLocal: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if listed != 1 {
		t.Fatalf("sweeps = %d, want exactly 1", listed)
	}
	if f.analyses != 1 {
		t.Fatalf("cycles = %d, want 1 (maxCycles)", f.analyses)
	}
}

// #212: SIGTERM during a cycle lets the in-flight cycle finish, then exits.
func TestLocalRunSigtermFinishesInflightCycle(t *testing.T) {
	withLocalAppCfg(t)
	writeLocalRunConfig(t, false, 1)
	f := useLocalFakes(t)

	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	f.acts.analyze = func(context.Context, IssueInput) (IssueAnalysis, error) {
		f.analyses++
		once.Do(func() { close(started) })
		<-release
		return IssueAnalysis{Title: "t", Body: "b", Summary: "s", Spec: specFallback}, nil
	}
	f.acts.listUnmanagedIssues = func(context.Context, RepoScope) ([]IssueInput, error) {
		return []IssueInput{{Owner: "o", Repo: "r", IssueNum: 1, RepoPath: t.TempDir(), Trigger: "sweep", Lifecycle: localTestLifecycle()}}, nil
	}

	done := make(chan error, 1)
	go func() { done <- runLocal([]string{"--repo", "r"}) }()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("cycle never started")
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("signal: %v", err)
	}
	close(release)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runLocal after SIGTERM: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runLocal did not exit after the in-flight cycle")
	}
	if f.analyses != 1 {
		t.Fatalf("analyses = %d, want the in-flight cycle only", f.analyses)
	}
	if got := f.localOutcome(t); got.TerminalState != stateClosed {
		t.Fatalf("in-flight cycle terminal = %q, want CLOSED", got.TerminalState)
	}
}

// #212: an explicit --issue run surfaces a failed cycle as a non-zero exit and
// still records the failed terminal.
func TestLocalRunIssueFailureExitsNonZero(t *testing.T) {
	withLocalAppCfg(t)
	writeLocalRunConfig(t, true, 1)
	f := useLocalFakes(t)
	f.acts.developer = func(context.Context, DevelopRequest) (DevelopResult, error) {
		return DevelopResult{}, context.DeadlineExceeded
	}
	if err := runLocal([]string{"--once", "--repo", "r", "--issue", "7"}); err == nil {
		t.Fatal("a failed --issue cycle must exit non-zero")
	}
	out := f.localOutcome(t)
	if out.TerminalState != stateBlocked || !out.HumanRequired {
		t.Fatalf("failure terminal = %q humanRequired=%v, want BLOCKED/true", out.TerminalState, out.HumanRequired)
	}
}

// #212: a failed cycle in a sweep is recorded, not silently dropped, and does
// not abort the remaining cycles.
func TestLocalSweepFailureRecordsTerminal(t *testing.T) {
	withLocalAppCfg(t)
	writeLocalRunConfig(t, true, 2)
	f := useLocalFakes(t)
	f.acts.developer = func(_ context.Context, in DevelopRequest) (DevelopResult, error) {
		f.develops++
		return DevelopResult{}, context.DeadlineExceeded
	}
	f.acts.listUnmanagedIssues = func(context.Context, RepoScope) ([]IssueInput, error) {
		return []IssueInput{
			{Owner: "o", Repo: "r", IssueNum: 1, RepoPath: t.TempDir(), Trigger: "sweep", Lifecycle: localTestLifecycle()},
			{Owner: "o", Repo: "r", IssueNum: 2, RepoPath: t.TempDir(), Trigger: "sweep", Lifecycle: localTestLifecycle()},
		}, nil
	}
	if err := runLocal([]string{"--once", "--repo", "r"}); err != nil {
		t.Fatalf("a sweep must not fail-fast: %v", err)
	}
	if f.develops != 2 {
		t.Fatalf("cycles = %d, want 2 (siblings still run)", f.develops)
	}
	blocked := 0
	for _, req := range f.evidence {
		if req.Outcome == "" {
			continue
		}
		var out CycleOutcome
		if err := json.Unmarshal([]byte(req.Outcome), &out); err != nil {
			t.Fatal(err)
		}
		if out.TerminalState == stateBlocked {
			blocked++
		}
	}
	if blocked != 2 {
		t.Fatalf("blocked terminal records = %d, want 2", blocked)
	}
}
