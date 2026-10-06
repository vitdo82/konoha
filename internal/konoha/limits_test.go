package konoha

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// #102: host resource ceilings — config validation, effective resolution,
// evidence, Linux enforcement, and the non-Linux no-op.

func TestLimitsConfigValidate(t *testing.T) {
	bad := []LimitsConfig{
		{CPUMilli: -1},
		{MemoryMiB: -1},
		{DiskMiB: -1},
		{MaxWallClock: "notaduration"},
		{MaxWallClock: "-1s"},
		{MaxWallClock: "0s"},
	}
	for _, l := range bad {
		if err := l.validate("limits"); err == nil {
			t.Fatalf("expected rejection for %+v", l)
		}
	}
	good := LimitsConfig{CPUMilli: 2000, MemoryMiB: 2048, DiskMiB: 1024, MaxWallClock: "30m"}
	if err := good.validate("limits"); err != nil {
		t.Fatalf("valid limits rejected: %v", err)
	}
}

func TestAppConfigLimitsWallClockWithinActivityWindow(t *testing.T) {
	cfg := defaultAppConfig()
	window := cfg.Worker.devWindow()

	cfg.Limits = LimitsConfig{MaxWallClock: (window + time.Minute).String()}
	if err := cfg.validate(); err == nil {
		t.Fatal("wall-clock beyond the activity window must fail at load")
	}

	cfg.Limits = LimitsConfig{MaxWallClock: (window - time.Minute).String()}
	if err := cfg.validate(); err != nil {
		t.Fatalf("wall-clock within the activity window rejected: %v", err)
	}

	cfg.Limits = LimitsConfig{CPUMilli: -1}
	if err := cfg.validate(); err == nil {
		t.Fatal("negative cpuMilli must fail at load")
	}
}

func TestLoadReposConfigRejectsNegativeLimits(t *testing.T) {
	for _, lc := range []string{
		`{limits: {cpuMilli: -1}}`,
		`{limits: {memoryMiB: -1}}`,
		`{limits: {diskMiB: -1}}`,
		`{limits: {maxWallClock: "nope"}}`,
	} {
		p := writeRepos(t, `repos: [{repo: r, owner: o, repoPath: /x, lifecycle:`+lc+`}]`)
		if _, err := LoadReposConfig(p); err == nil {
			t.Fatalf("expected rejection for lifecycle %s", lc)
		}
	}
}

func TestLoadReposConfigLimitsWallClockVsWorkerWindow(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "konoha.yaml"), []byte("worker:\n  devTimeout: 10m\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KONOHA_CONFIG_DIR", dir)

	over := writeRepos(t, `repos: [{repo: r, owner: o, repoPath: /x, lifecycle: {limits: {maxWallClock: 30m}}}]`)
	if _, err := loadReposConfigWithResources(over); err == nil {
		t.Fatal("per-repo maxWallClock beyond the worker dev window must fail at load")
	}

	within := writeRepos(t, `repos: [{repo: r, owner: o, repoPath: /x, lifecycle: {limits: {maxWallClock: 5m}}}]`)
	if _, err := loadReposConfigWithResources(within); err != nil {
		t.Fatalf("maxWallClock within the worker dev window rejected: %v", err)
	}
}

func TestEffectiveLimitsPerRepoOverride(t *testing.T) {
	saved := appCfg
	defer func() { appCfg = saved }()
	appCfg.Limits = LimitsConfig{MemoryMiB: 512}

	if got := EffectiveLimits(LifecycleConfig{}); got.MemoryMiB != 512 {
		t.Fatalf("expected global default, got %+v", got)
	}
	override := LimitsConfig{MemoryMiB: 1024}
	if got := EffectiveLimits(LifecycleConfig{Limits: &override}); got.MemoryMiB != 1024 {
		t.Fatalf("expected per-repo override, got %+v", got)
	}
}

func TestLimitsEvidence(t *testing.T) {
	if ev := limitsEvidence(LimitsConfig{}); ev != nil {
		t.Fatalf("unset limits must record nothing, got %+v", ev)
	}
	ev := limitsEvidence(LimitsConfig{CPUMilli: 2000, MemoryMiB: 128, MaxWallClock: "1m"})
	if ev == nil {
		t.Fatal("configured limits must record evidence")
	}
	if limitsEnforceable() {
		if ev.Status != limitEnforced || ev.Reason != "" {
			t.Fatalf("Linux should enforce: %+v", ev)
		}
		return
	}
	if ev.Status != limitSkipped || ev.Reason == "" {
		t.Fatalf("non-Linux must record SKIPPED with a reason: %+v", ev)
	}
}

func TestEnforceCommandOffLinuxNoop(t *testing.T) {
	if limitsEnforceable() {
		t.Skip("Linux enforces")
	}
	cmd, ev := enforceCommand(context.Background(), LimitsConfig{MemoryMiB: 64}, "true", nil, t.TempDir(), os.Environ())
	if ev == nil || ev.Status != limitSkipped || ev.Reason == "" {
		t.Fatalf("non-Linux must record SKIPPED: %+v", ev)
	}
	if filepath.Base(cmd.Path) != "true" {
		t.Fatalf("non-Linux must not wrap the command, got %q", cmd.Path)
	}
}

// TestLimitsDoNotChangeGateVerdict is the §regression guard: ceilings that are
// not crossed never change a gate's pass/fail.
func TestLimitsDoNotChangeGateVerdict(t *testing.T) {
	limits := &LimitsConfig{MemoryMiB: 512, MaxWallClock: "1m"}
	dir := t.TempDir()

	ok, skipped, out, err := validateRepo(PublishInput{
		RepoPath: dir, Limits: limits,
		Validation: []ValidationGate{{ID: "ok", Argv: []string{"true"}}},
	})
	if err != nil || !ok || skipped {
		t.Fatalf("passing gate must still pass under limits: ok=%v skipped=%v out=%q err=%v", ok, skipped, out, err)
	}

	ok, _, _, _ = validateRepo(PublishInput{
		RepoPath: dir, Limits: limits,
		Validation: []ValidationGate{{ID: "bad", Argv: []string{"false"}}},
	})
	if ok {
		t.Fatal("failing gate must still fail under limits")
	}
}

// TestLimitsEnforceCommandUsesScrubbedEnv is the §23 guard: a wrapper never
// re-introduces a credential the caller scrubbed.
func TestLimitsEnforceCommandUsesScrubbedEnv(t *testing.T) {
	env := []string{"PATH=/usr/bin", "PWD=/x"}
	cmd, _ := enforceCommand(context.Background(), LimitsConfig{MemoryMiB: 128}, "true", nil, t.TempDir(), env)
	if len(cmd.Env) != len(env) {
		t.Fatalf("wrapper changed the child env: %v", cmd.Env)
	}
	for i := range env {
		if cmd.Env[i] != env[i] {
			t.Fatalf("wrapper changed the child env: %v", cmd.Env)
		}
	}
}

// TestCycleOutcomeJSONIncludesLimits confirms the applied ceilings + status land
// in outcome.json (via the CycleOutcome record).
func TestCycleOutcomeJSONIncludesLimits(t *testing.T) {
	out := CycleOutcome{Limits: limitsEvidence(LimitsConfig{MemoryMiB: 256, MaxWallClock: "1m"})}
	s := evidenceJSON(out)
	if !strings.Contains(s, `"limits"`) || !strings.Contains(s, `"status"`) {
		t.Fatalf("outcome JSON missing limits evidence: %s", s)
	}
}

// TestLimitsMemoryKillLinux exercises the rlimit enforcement path directly (the
// systemd-run path is host-dependent): a memory-hungry fixture is killed and the
// evidence records the crossed ceiling.
func TestLimitsMemoryKillLinux(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("host resource ceilings are Linux-only")
	}
	l := LimitsConfig{MemoryMiB: 64}
	script := rlimitScript(l)
	args := []string{"-c", script, "sh", os.Args[0], "-test.run=TestLimitsMemoryHelperProcess"}
	cmd := exec.CommandContext(context.Background(), "sh", args...)
	cmd.Dir = t.TempDir()
	cmd.Env = append(os.Environ(), "KONOHA_LIMITS_HELPER=1")

	stdout, stderr, err := runCapture(cmd)
	if err == nil {
		t.Fatalf("expected the memory-hungry fixture to be killed (stdout=%q stderr=%q)", stdout, stderr)
	}
	ev := classifyLimitCross(limitsEvidence(l), err, stdout+stderr)
	if ev == nil || !ev.Crossed || ev.CrossedReason != "memory" {
		t.Fatalf("expected a recorded memory-crossing, got %+v (stderr=%q)", ev, stderr)
	}
}

// TestLimitsMemoryHelperProcess is the fixture re-exec'd by
// TestLimitsMemoryKillLinux; it allocates until the ceiling bites.
func TestLimitsMemoryHelperProcess(t *testing.T) {
	if os.Getenv("KONOHA_LIMITS_HELPER") != "1" {
		return
	}
	var keep [][]byte
	for {
		b := make([]byte, 8<<20)
		for i := range b {
			b[i] = 1
		}
		keep = append(keep, b)
	}
}
