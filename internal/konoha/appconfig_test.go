package konoha

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeAppConfig(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "konoha.yaml")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// Missing file → bundled defaults, no error.
func TestLoadAppConfigMissingFileDefaults(t *testing.T) {
	cfg, err := loadAppConfig(filepath.Join(t.TempDir(), "nope.yaml"))
	if err != nil {
		t.Fatalf("missing file must not error: %v", err)
	}
	if cfg.Worker.TaskQueue != "review-queue" || cfg.Worker.RetryMaxAttempts != defaultInfraRetryMaxAttempts ||
		cfg.Defaults.Lifecycle.DevelopmentMaxComplexity != 4 || cfg.Defaults.Lifecycle.MaxResolutionAttempts != 1 {
		t.Fatalf("defaults wrong: %+v", cfg)
	}
}

// Strict: unknown keys fail fast.
func TestLoadAppConfigUnknownKeyFails(t *testing.T) {
	p := writeAppConfig(t, "worker:\n  taskQueue: q\n  bogusKey: 1\n")
	if _, err := loadAppConfig(p); err == nil {
		t.Fatal("unknown key must fail strict load")
	}
}

// File overrides defaults; durations parse.
func TestLoadAppConfigFileValues(t *testing.T) {
	p := writeAppConfig(t, `
worker:
  taskQueue: custom-q
  startToCloseTimeout: 30m
defaults:
  lifecycle:
    developmentMaxComplexity: 2
    maxResolutionAttempts: 3
    poGateTimeout: 72h
`)
	cfg, err := loadAppConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Worker.TaskQueue != "custom-q" || cfg.Worker.startToClose() != 30*time.Minute {
		t.Fatalf("worker overrides wrong: %+v", cfg.Worker)
	}
	if cfg.Defaults.Lifecycle.DevelopmentMaxComplexity != 2 || cfg.Defaults.Lifecycle.MaxResolutionAttempts != 3 ||
		cfg.Defaults.Lifecycle.poTimeout() != 72*time.Hour {
		t.Fatalf("lifecycle defaults overrides wrong: %+v", cfg.Defaults)
	}
}

// #83: evidence.dir keeps existing deployments working with no config, and an
// explicit empty value fails fast at load.
func TestLoadAppConfigEvidenceDir(t *testing.T) {
	cfg, err := loadAppConfig(filepath.Join(t.TempDir(), "nope.yaml"))
	if err != nil {
		t.Fatalf("missing file must not error: %v", err)
	}
	if cfg.Evidence.Dir != defaultEvidenceDir {
		t.Fatalf("default evidence.dir = %q, want %q", cfg.Evidence.Dir, defaultEvidenceDir)
	}
	if _, err := loadAppConfig(writeAppConfig(t, "evidence:\n  dir: \"\"\n")); err == nil {
		t.Fatal("empty evidence.dir must fail fast at load")
	}
	if cfg, err := loadAppConfig(writeAppConfig(t, "evidence:\n  dir: /var/lib/konoha/evidence\n")); err != nil || cfg.Evidence.Dir != "/var/lib/konoha/evidence" {
		t.Fatalf("evidence.dir override not honored: %q err=%v", cfg.Evidence.Dir, err)
	}
}

// Out-of-range values fail validation.
func TestLoadAppConfigOutOfRange(t *testing.T) {
	for _, body := range []string{
		"worker:\n  maxConcurrentActivities: 0\n",
		"defaults:\n  lifecycle:\n    developmentMaxComplexity: 11\n",
		"defaults:\n  lifecycle:\n    maxDevReviewAttempts: 100\n",
	} {
		if _, err := loadAppConfig(writeAppConfig(t, body)); err == nil {
			t.Fatalf("out-of-range config must fail: %q", body)
		}
	}
}

// Env override matrix: env > file. Worker capacity + task queue are
// env-overridable; defaults.lifecycle is NOT (stays file/default).
func TestEnvOverridePrecedence(t *testing.T) {
	savedCfg := appCfg
	t.Cleanup(func() { appCfg = savedCfg })

	saved := map[string]string{}
	for _, k := range []string{"WORKER_MAX_CONCURRENT_ACTIVITIES", "KONOHA_TASK_QUEUE"} {
		if v, ok := os.LookupEnv(k); ok {
			saved[k] = v
			os.Unsetenv(k)
		}
	}
	t.Cleanup(func() {
		for k, v := range saved {
			if v != "" {
				os.Setenv(k, v)
			} else {
				os.Unsetenv(k)
			}
		}
	})

	appCfg = defaultAppConfig()
	os.Setenv("WORKER_MAX_CONCURRENT_ACTIVITIES", "7")
	os.Setenv("KONOHA_TASK_QUEUE", "env-q")
	defer os.Unsetenv("WORKER_MAX_CONCURRENT_ACTIVITIES")
	defer os.Unsetenv("KONOHA_TASK_QUEUE")

	applyEnvOverrides()
	if appCfg.Worker.MaxConcurrentActivities != 7 || appCfg.Worker.TaskQueue != "env-q" {
		t.Fatalf("env overrides not applied: %+v", appCfg.Worker)
	}
	// lifecycle defaults are NOT env-overridable — still bundled defaults.
	if appCfg.Defaults.Lifecycle.DevelopmentMaxComplexity != 4 {
		t.Fatalf("lifecycle defaults must stay file/default, got %d", appCfg.Defaults.Lifecycle.DevelopmentMaxComplexity)
	}
}

// Policy helpers honor the resolved default when repos.yaml leaves a field unset.
func TestPolicyDefaultsCfgAware(t *testing.T) {
	saved := appCfg
	t.Cleanup(func() { appCfg = saved })

	appCfg = defaultAppConfig()
	appCfg.Defaults.Lifecycle.DevelopmentMaxComplexity = 3
	appCfg.Defaults.Lifecycle.MaxDevReviewAttempts = 5
	appCfg.Defaults.Lifecycle.POGateTimeout = "24h"
	appCfg.Defaults.Lifecycle.MaxResolutionAttempts = 2

	if got := DevelopmentMaxComplexity(LifecycleConfig{}); got != 3 {
		t.Fatalf("DevelopmentMaxComplexity default = %d want 3", got)
	}
	if got := MaxDevelopmentAttempts(LifecycleConfig{}); got != 5 {
		t.Fatalf("MaxDevelopmentAttempts default = %d want 5", got)
	}
	if got := POGateTimeout(LifecycleConfig{}); got != 24*time.Hour {
		t.Fatalf("POGateTimeout default = %v want 24h", got)
	}
	// per-repo value still wins.
	if got := DevelopmentMaxComplexity(LifecycleConfig{DevelopmentMaxComplexity: 4}); got != 4 {
		t.Fatalf("per-repo ceiling must win, got %d", got)
	}
	// resolution cap honors config: cap=2 → attempt 1 resolves, attempt 2 escalates.
	if d := EvaluateDisagreement(true, 1); d.Human || !d.Resolve {
		t.Fatalf("resolution cap 2: attempt 1 must still resolve, got %+v", d)
	}
	if d := EvaluateDisagreement(true, 2); !d.Human {
		t.Fatalf("resolution cap 2: attempt 2 must escalate, got %+v", d)
	}
}
