package konoha

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Host resource ceilings (#102). Konoha runs agent and validation child
// processes directly on the operator host (there is no sandbox, docs/security.md),
// so a runaway agent can consume the machine. These are best-effort
// ANTI-ACCIDENT ceilings, explicitly NOT a security boundary: a host process can
// still see the host. Enforcement is Linux-only (systemd-run scope when
// available, else an rlimit `sh` wrapper); on any other OS it is a documented
// no-op and the outcome records SKIPPED with a reason — never a claim of
// enforcement that did not happen (§40).
//
// Limits are configured globally (konoha.yaml `limits:`) and per repo
// (repos.yaml `lifecycle.limits:`); the per-repo block wins. Resolution is pure
// over the process-fixed config, so it is replay-safe.

// LimitEvidence statuses. ENFORCED means the OS applied a ceiling; SKIPPED means
// it was not applied (non-Linux, or no limits configured) — never a false
// success.
const (
	limitEnforced = "ENFORCED"
	limitSkipped  = "SKIPPED"
)

// LimitsConfig is one `limits:` block (#102). Zero-valued fields mean "no
// ceiling for this dimension". MaxWallClock is a Go duration string.
type LimitsConfig struct {
	CPUMilli     int    `yaml:"cpuMilli" json:"cpuMilli,omitempty"`
	MemoryMiB    int    `yaml:"memoryMiB" json:"memoryMiB,omitempty"`
	DiskMiB      int    `yaml:"diskMiB" json:"diskMiB,omitempty"`
	MaxWallClock string `yaml:"maxWallClock" json:"maxWallClock,omitempty"`
}

// IsZero reports whether no ceiling is configured (nothing to enforce).
func (l LimitsConfig) IsZero() bool {
	return l.CPUMilli == 0 && l.MemoryMiB == 0 && l.DiskMiB == 0 && strings.TrimSpace(l.MaxWallClock) == ""
}

// wallClock parses the configured wall-clock ceiling (0 = unset/invalid).
func (l LimitsConfig) wallClock() time.Duration {
	d, err := time.ParseDuration(strings.TrimSpace(l.MaxWallClock))
	if err != nil || d <= 0 {
		return 0
	}
	return d
}

// validate gates one limits block: no negative ceiling and a positive wall
// clock. Out-of-range values fail at load (§33), never at process launch.
func (l LimitsConfig) validate(prefix string) error {
	if l.CPUMilli < 0 {
		return fmt.Errorf("%s.cpuMilli: %d must be >= 0", prefix, l.CPUMilli)
	}
	if l.MemoryMiB < 0 {
		return fmt.Errorf("%s.memoryMiB: %d must be >= 0", prefix, l.MemoryMiB)
	}
	if l.DiskMiB < 0 {
		return fmt.Errorf("%s.diskMiB: %d must be >= 0", prefix, l.DiskMiB)
	}
	if s := strings.TrimSpace(l.MaxWallClock); s != "" {
		if d, err := time.ParseDuration(s); err != nil || d <= 0 {
			return fmt.Errorf("%s.maxWallClock: %q is not a positive duration", prefix, l.MaxWallClock)
		}
	}
	return nil
}

// validateActivityWindow enforces the #102 invariant that a wall-clock ceiling
// never exceeds the activity window it complements: a maxWallClock larger than
// the activity StartToCloseTimeout could never kill before the activity itself
// is torn down, so it is a load-time error rather than a silent no-op.
func (l LimitsConfig) validateActivityWindow(prefix string, window time.Duration) error {
	if d := l.wallClock(); d > window {
		return fmt.Errorf("%s.maxWallClock %s exceeds the activity StartToCloseTimeout %s", prefix, d, window)
	}
	return nil
}

// EffectiveLimits resolves a repo's ceilings: the per-repo override wins,
// otherwise the global `limits:` default. Pure over process-fixed config.
func EffectiveLimits(lc LifecycleConfig) LimitsConfig {
	if lc.Limits != nil {
		return *lc.Limits
	}
	return appCfg.Limits
}

// limitsForOverride resolves a request-level override (nil = global default).
func limitsForOverride(l *LimitsConfig) LimitsConfig {
	if l != nil {
		return *l
	}
	return appCfg.Limits
}

// LimitEvidence records the applied ceilings and whether they were enforced
// (#102). It is evidence only: a ceiling never rewrites a deterministic verdict
// other than by killing the process that would have produced it (§40).
type LimitEvidence struct {
	CPUMilli      int    `json:"cpuMilli,omitempty"`
	MemoryMiB     int    `json:"memoryMiB,omitempty"`
	DiskMiB       int    `json:"diskMiB,omitempty"`
	MaxWallClock  string `json:"maxWallClock,omitempty"`
	Status        string `json:"status"`              // ENFORCED | SKIPPED
	Reason        string `json:"reason,omitempty"`    // why SKIPPED
	Mechanism     string `json:"mechanism,omitempty"` // systemd-run | rlimit (Linux)
	Crossed       bool   `json:"crossed,omitempty"`
	CrossedReason string `json:"crossedReason,omitempty"` // cpu | memory | disk | wallClock | killed
}

// limitsEnforceable reports whether this OS supports the ceilings. Linux only
// (#102 scope boundary); every other host is a documented no-op.
func limitsEnforceable() bool { return runtime.GOOS == "linux" }

// limitsEvidence projects a limits block into its outcome evidence. Returns nil
// when no ceiling is configured (nothing to record). Pure over the process-fixed
// OS, so it is safe to call from workflow code.
func limitsEvidence(l LimitsConfig) *LimitEvidence {
	if l.IsZero() {
		return nil
	}
	ev := &LimitEvidence{
		CPUMilli: l.CPUMilli, MemoryMiB: l.MemoryMiB, DiskMiB: l.DiskMiB,
		MaxWallClock: strings.TrimSpace(l.MaxWallClock),
	}
	if limitsEnforceable() {
		ev.Status = limitEnforced
		return ev
	}
	ev.Status = limitSkipped
	ev.Reason = fmt.Sprintf("host resource ceilings are Linux-only; no enforcement on %s", runtime.GOOS)
	return ev
}

// enforceCommand builds the child process for name/args, applying the ceilings
// best-effort when the OS supports them. dir/env are always applied to the outer
// command; env is the caller's already-scrubbed environment, so a wrapper can
// never re-introduce a GitHub credential (§23). Returns the evidence (with the
// chosen Mechanism set) for the caller to fold into a result.
func enforceCommand(ctx context.Context, l LimitsConfig, name string, args []string, dir string, env []string) (*exec.Cmd, *LimitEvidence) {
	ev := limitsEvidence(l)
	var cmd *exec.Cmd
	if ev != nil && ev.Status == limitEnforced {
		cmd, ev = wrapEnforced(ctx, l, name, args, ev)
	} else {
		cmd = exec.CommandContext(ctx, name, args...)
	}
	cmd.Dir = dir
	cmd.Env = env
	return cmd, ev
}

// wrapEnforced selects a Linux enforcement mechanism: prefer a systemd-run user
// scope (cgroup MemoryMax/CPUQuota/RuntimeMaxSec), else a POSIX rlimit wrapper
// around `/bin/sh`. Both inherit the caller's scrubbed env, so no credential is
// leaked.
func wrapEnforced(ctx context.Context, l LimitsConfig, name string, args []string, ev *LimitEvidence) (*exec.Cmd, *LimitEvidence) {
	if runner, argv, ok := systemdRunArgs(l, name, args); ok {
		ev.Mechanism = "systemd-run"
		return exec.CommandContext(ctx, runner, argv...), ev
	}
	ev.Mechanism = "rlimit"
	shArgs := append([]string{"-c", rlimitScript(l), "sh", name}, args...)
	if d := l.wallClock(); d > 0 {
		if tp, err := exec.LookPath("timeout"); err == nil {
			// A hard wall-clock kill in addition to the activity timeout. `exec`
			// inside the rlimit script means the timed process is the target;
			// `timeout` reports 124 so the crossing is attributable.
			timeoutArgs := append([]string{"-k", "5", strconv.Itoa(int(d.Seconds()) + 1), "sh"}, shArgs...)
			ev.Mechanism = "rlimit+timeout"
			return exec.CommandContext(ctx, tp, timeoutArgs...), ev
		}
	}
	return exec.CommandContext(ctx, "sh", shArgs...), ev
}

// systemdRunAvailable probes, once, whether a usable `systemd-run` user scope
// exists (the binary may be present while the user manager is not). When the
// probe fails, the rlimit wrapper is used instead of failing every launch.
var systemdRunAvailable = sync.OnceValue(func() bool {
	path, err := exec.LookPath("systemd-run")
	if err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, path, "--user", "--scope", "--quiet", "true").Run() == nil
})

// systemdRunArgs renders the preferred systemd-run scope argv, only when a
// usable user manager was probed. It is a scope, so it runs in the caller's
// environment (the scrubbed cmd.Env), never the manager's.
func systemdRunArgs(l LimitsConfig, name string, args []string) (string, []string, bool) {
	if !systemdRunAvailable() {
		return "", nil, false
	}
	path, err := exec.LookPath("systemd-run")
	if err != nil {
		return "", nil, false
	}
	argv := []string{"--user", "--scope", "--quiet"}
	if l.MemoryMiB > 0 {
		argv = append(argv, "-p", fmt.Sprintf("MemoryMax=%dM", l.MemoryMiB))
	}
	if l.CPUMilli > 0 {
		quota := l.CPUMilli / 10 // milli-cores → percent (1000m = 100%)
		if quota < 1 {
			quota = 1
		}
		argv = append(argv, "-p", fmt.Sprintf("CPUQuota=%d%%", quota))
	}
	if d := l.wallClock(); d > 0 {
		argv = append(argv, "-p", fmt.Sprintf("RuntimeMaxSec=%d", int(d.Seconds())+1))
	}
	argv = append(argv, "--", name)
	argv = append(argv, args...)
	return path, argv, true
}

// rlimitScript renders the POSIX `sh` rlimit wrapper body. `exec "$@"` keeps the
// target as the process sh is replaced by, so signals/exit codes are the
// target's. CPU time is derived from the cpuMilli rate applied over the
// wall-clock ceiling (RLIMIT_CPU is a total, not a rate).
func rlimitScript(l LimitsConfig) string {
	var b strings.Builder
	if l.MemoryMiB > 0 {
		// RLIMIT_AS is in KiB.
		fmt.Fprintf(&b, "ulimit -v %d 2>/dev/null; ", l.MemoryMiB*1024)
	}
	if l.CPUMilli > 0 {
		if d := l.wallClock(); d > 0 {
			secs := int((float64(l.CPUMilli) / 1000.0) * d.Seconds())
			if secs < 1 {
				secs = 1
			}
			fmt.Fprintf(&b, "ulimit -t %d 2>/dev/null; ", secs)
		}
	}
	if l.DiskMiB > 0 {
		// RLIMIT_FSIZE is in 512-byte blocks.
		fmt.Fprintf(&b, "ulimit -f %d 2>/dev/null; ", l.DiskMiB*1024*1024/512)
	}
	b.WriteString(`exec "$@"`)
	return b.String()
}

// classifyLimitCross marks, best-effort, which ceiling a killed process crossed.
// It inspects the exit status for the rlimit signals (SIGXCPU/SIGXFSZ/SIGKILL),
// known allocator-abort output for RLIMIT_AS, and the `timeout` exit code. An
// unattributable kill is recorded as "killed". A nil evidence (no limits) or a
// successful run is returned unchanged.
func classifyLimitCross(ev *LimitEvidence, err error, output string) *LimitEvidence {
	if ev == nil || ev.Status != limitEnforced || err == nil {
		return ev
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		return ev
	}
	if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		switch ws.Signal() {
		case syscall.SIGXCPU:
			ev.Crossed, ev.CrossedReason = true, "cpu"
		case syscall.SIGXFSZ:
			ev.Crossed, ev.CrossedReason = true, "disk"
		case syscall.SIGKILL:
			// systemd MemoryMax OOM-kills with SIGKILL; `timeout` reports 124, so a
			// SIGKILL with a memory ceiling configured is the OOM path.
			ev.Crossed, ev.CrossedReason = true, "killed"
			if ev.MemoryMiB > 0 {
				ev.CrossedReason = "memory"
			}
		}
		return ev
	}
	if ee.ExitCode() == 124 {
		ev.Crossed, ev.CrossedReason = true, "wallClock"
		return ev
	}
	// RLIMIT_AS exhaustion aborts the runtime rather than signalling.
	for _, marker := range []string{"out of memory", "cannot allocate memory", "reserve page summary memory"} {
		if strings.Contains(strings.ToLower(output), marker) {
			ev.Crossed, ev.CrossedReason = true, "memory"
			return ev
		}
	}
	return ev
}

// runCaptureWithLimits runs an enforced command and folds the crossed-ceiling
// detection into its evidence. It is the agent-process counterpart of runCapture.
func runCaptureWithLimits(cmd *exec.Cmd, l LimitsConfig) (stdout, stderr string, ev *LimitEvidence, err error) {
	stdout, stderr, err = runCapture(cmd)
	return stdout, stderr, classifyLimitCross(limitsEvidence(l), err, stdout+stderr), err
}
