package konoha

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// #212: runtime.mode is a closed set; an unknown value fails at load instead of
// silently falling back to temporal.
func TestRuntimeConfigModeClosedSet(t *testing.T) {
	p := writeAppConfig(t, "runtime:\n  mode: localish\n")
	_, err := loadAppConfig(p)
	if err == nil || !strings.Contains(err.Error(), "runtime.mode") {
		t.Fatalf("unknown mode must fail at load, err = %v", err)
	}
}

// #212: the local block resolves its values and they survive a save/load cycle.
func TestRuntimeConfigLocalValues(t *testing.T) {
	p := writeAppConfig(t, `
runtime:
  mode: local
  local:
    sweepInterval: 30s
    maxCycles: 3
    once: true
    approvalWait: 2m
`)
	cfg, err := loadAppConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Runtime.mode() != runtimeModeLocal {
		t.Fatalf("mode = %q, want local", cfg.Runtime.mode())
	}
	if cfg.Runtime.Local.sweepInterval() != 30*time.Second || cfg.Runtime.Local.maxCycles() != 3 ||
		!cfg.Runtime.Local.once() || cfg.Runtime.Local.approvalWait() != 2*time.Minute {
		t.Fatalf("local block wrong: %+v", cfg.Runtime.Local)
	}
}

// #212: absent runtime block preserves today's Temporal behavior byte-for-byte.
func TestRuntimeConfigAbsentDefaultsToTemporal(t *testing.T) {
	cfg, err := loadAppConfig(filepath.Join(t.TempDir(), "nope.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Runtime.mode() != runtimeModeTemporal {
		t.Fatalf("mode = %q, want temporal", cfg.Runtime.mode())
	}
	if cfg.Runtime.Local.sweepInterval() != 5*time.Minute || cfg.Runtime.Local.maxCycles() != 1 ||
		cfg.Runtime.Local.once() || cfg.Runtime.Local.approvalWait() != 10*time.Minute {
		t.Fatalf("local defaults wrong: %+v", cfg.Runtime.Local)
	}
}

// #212: out-of-range or malformed local values fail fast at load.
func TestRuntimeConfigLocalValidation(t *testing.T) {
	cases := map[string]struct{ body, want string }{
		"maxCycles":      {"runtime:\n  local:\n    maxCycles: 0\n", "maxCycles"},
		"sweepInterval":  {"runtime:\n  local:\n    sweepInterval: nope\n", "sweepInterval"},
		"approvalWait":   {"runtime:\n  local:\n    approvalWait: nope\n", "approvalWait"},
		"emptySweep":     {"runtime:\n  local:\n    sweepInterval: ''\n", "sweepInterval"},
		"negativeCycles": {"runtime:\n  local:\n    maxCycles: -1\n", "maxCycles"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := loadAppConfig(writeAppConfig(t, tc.body))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}
