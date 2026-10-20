package konoha

import (
	"fmt"
	"strings"
	"time"
)

// #212: runtime selection. Temporal stays the default and the only durable /
// scheduled path; local is a sequential, in-process driver for headless hosts.
// The mode is resolved once at load (like every other konoha.yaml block) and an
// unknown value fails fast instead of silently falling back.
const (
	runtimeModeTemporal = "temporal"
	runtimeModeLocal    = "local"
)

// RuntimeConfig is the top-level `runtime:` block.
type RuntimeConfig struct {
	Mode  string             `yaml:"mode"`
	Local LocalRuntimeConfig `yaml:"local"`
}

// LocalRuntimeConfig is the `runtime.local` block: the idle delay between
// sweeps, how many cycles one sweep starts per repo, whether to run a single
// sweep and exit, and the bounded wait at a §55 gate.
type LocalRuntimeConfig struct {
	SweepInterval string `yaml:"sweepInterval"`
	MaxCycles     int    `yaml:"maxCycles"`
	Once          bool   `yaml:"once"`
	ApprovalWait  string `yaml:"approvalWait"`
}

// mode resolves the configured runtime mode, defaulting to temporal.
func (r RuntimeConfig) mode() string {
	switch m := strings.ToLower(strings.TrimSpace(r.Mode)); m {
	case "":
		return runtimeModeTemporal
	default:
		return m
	}
}

func (l LocalRuntimeConfig) sweepInterval() time.Duration {
	d, err := time.ParseDuration(strings.TrimSpace(l.SweepInterval))
	if err != nil || d <= 0 {
		return 5 * time.Minute
	}
	return d
}

func (l LocalRuntimeConfig) maxCycles() int {
	if l.MaxCycles < 1 {
		return 1
	}
	return l.MaxCycles
}

func (l LocalRuntimeConfig) once() bool { return l.Once }

func (l LocalRuntimeConfig) approvalWait() time.Duration {
	d, err := time.ParseDuration(strings.TrimSpace(l.ApprovalWait))
	if err != nil || d <= 0 {
		return 10 * time.Minute
	}
	return d
}

// validate gates the `runtime:` block: a closed mode set and, for local mode,
// well-formed positive durations and a non-zero cycle cap.
func (r RuntimeConfig) validate() error {
	switch r.mode() {
	case runtimeModeTemporal, runtimeModeLocal:
	default:
		return fmt.Errorf("runtime.mode: %q must be temporal or local", r.Mode)
	}
	if d, err := time.ParseDuration(strings.TrimSpace(r.Local.SweepInterval)); err != nil || d < time.Second {
		return fmt.Errorf("runtime.local.sweepInterval %q must be a duration >= 1s", r.Local.SweepInterval)
	}
	if d, err := time.ParseDuration(strings.TrimSpace(r.Local.ApprovalWait)); err != nil || d <= 0 {
		return fmt.Errorf("runtime.local.approvalWait %q must be a positive duration", r.Local.ApprovalWait)
	}
	if r.Local.MaxCycles < 1 {
		return fmt.Errorf("runtime.local.maxCycles must be >= 1")
	}
	return nil
}
