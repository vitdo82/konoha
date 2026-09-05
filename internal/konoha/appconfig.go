package konoha

import (
	"bytes"
	"fmt"
	"os"
	"strconv"
	"time"

	"gopkg.in/yaml.v3"
)

// Application configuration (#48): runtime + code-level lifecycle defaults, in
// konoha.yaml (strict), resolved as env > file > bundled default at bootstrap.
// Loaded ONCE before the worker starts; the value is fixed for the process
// lifetime so workflow/activity code may read it deterministically (replays see
// the same value). Per-repo policy stays in repos.yaml (LifecycleConfig) — this
// file sets the FALLBACK defaults and worker/runtime values.

// WorkerConfig is the worker/runtime block. Durations are kept as strings
// (matching repos.yaml's poGateTimeout) and parsed via accessors so a bad value
// fails at load (#48).
type WorkerConfig struct {
	TaskQueue               string `yaml:"taskQueue"`
	MaxConcurrentActivities int    `yaml:"maxConcurrentActivities"`
	StartToCloseTimeout     string `yaml:"startToCloseTimeout"`
	SweepTimeout            string `yaml:"sweepTimeout"`
	DevTimeout              string `yaml:"devTimeout"` // §4.4 agent dev/repair window (default 45m)
	RetryMaxAttempts        int    `yaml:"retryMaxAttempts"`
	SweepRetryMaxAttempts   int    `yaml:"sweepRetryMaxAttempts"`
}

// LifecycleDefaults is the `defaults.lifecycle` block: the fallback per-repo
// policy when repos.yaml leaves the field unset.
type LifecycleDefaults struct {
	DevelopmentMaxComplexity int    `yaml:"developmentMaxComplexity"`
	MaxDevReviewAttempts     int    `yaml:"maxDevReviewAttempts"`
	MaxResolutionAttempts    int    `yaml:"maxResolutionAttempts"`
	POGateTimeout            string `yaml:"poGateTimeout"`
}

// AppConfig is the resolved application configuration.
type AppConfig struct {
	Worker   WorkerConfig   `yaml:"worker"`
	Defaults DefaultsConfig `yaml:"defaults"`
}

// DefaultsConfig is the `defaults:` block.
type DefaultsConfig struct {
	Lifecycle LifecycleDefaults `yaml:"lifecycle"`
}

// defaultAppConfig returns the code-level defaults (#48: these are the bundled
// defaults used when konoha.yaml is absent or a field is unset).
func defaultAppConfig() AppConfig {
	return AppConfig{
		Worker: WorkerConfig{
			TaskQueue:               "review-queue",
			MaxConcurrentActivities: 10,
			StartToCloseTimeout:     "15m",
			SweepTimeout:            "10m",
			DevTimeout:              "45m",
			RetryMaxAttempts:        3,
			SweepRetryMaxAttempts:   2,
		},
		Defaults: DefaultsConfig{
			Lifecycle: LifecycleDefaults{
				DevelopmentMaxComplexity: 4,
				MaxDevReviewAttempts:     2,
				MaxResolutionAttempts:    1,
				POGateTimeout:            "168h",
			},
		},
	}
}

// startToClose parses the cycle/PR activity window.
func (w WorkerConfig) startToClose() time.Duration {
	d, err := time.ParseDuration(w.StartToCloseTimeout)
	if err != nil {
		return 15 * time.Minute
	}
	return d
}

// sweepWindow parses the sweep activity window.
func (w WorkerConfig) sweepWindow() time.Duration {
	d, err := time.ParseDuration(w.SweepTimeout)
	if err != nil {
		return 10 * time.Minute
	}
	return d
}

// devWindow parses the §4.4 agent dev/repair window (generous: real opencode
// work takes minutes).
func (w WorkerConfig) devWindow() time.Duration {
	d, err := time.ParseDuration(w.DevTimeout)
	if err != nil {
		return 45 * time.Minute
	}
	return d
}

// poTimeout parses the human-response window default.
func (l LifecycleDefaults) poTimeout() time.Duration {
	d, err := time.ParseDuration(l.POGateTimeout)
	if err != nil {
		return 168 * time.Hour
	}
	return d
}

// appCfg is the active resolved configuration. Set once by Main() at bootstrap;
// fixed thereafter so replay code sees a stable value.
var appCfg = defaultAppConfig()

// loadAppConfig reads konoha.yaml (strict) and merges over the bundled defaults.
// A missing file is NOT an error (defaults are used). Unknown keys/out-of-range
// values fail fast (same KnownFields discipline as repos.yaml).
func loadAppConfig(path string) (AppConfig, error) {
	cfg := defaultAppConfig()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return cfg, err
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return cfg, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, cfg.validate()
}

// defaultAppConfigPath returns konoha.yaml's location: $KONOHA_CONFIG_DIR if
// set, else ./configs/konoha.yaml.
func defaultAppConfigPath() string {
	if dir := os.Getenv("KONOHA_CONFIG_DIR"); dir != "" {
		return dir + "/konoha.yaml"
	}
	return "configs/konoha.yaml"
}

// applyEnvOverrides applies the env > file > default matrix for the
// deployment/host-level keys only. Policy keys (defaults.lifecycle.*) and
// timeouts are NOT env-overridable — those stay file-authoritative so a
// per-host env cannot fork deterministic behavior.
func applyEnvOverrides() {
	if v := os.Getenv("WORKER_MAX_CONCURRENT_ACTIVITIES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 {
			appCfg.Worker.MaxConcurrentActivities = n
		}
	}
	if v := os.Getenv("KONOHA_TASK_QUEUE"); v != "" {
		appCfg.Worker.TaskQueue = v
	}
}

// workerMaxConcurrentActivities resolves the §43 worker-level cap from the
// resolved config (env > file > 10).
func workerMaxConcurrentActivities() int {
	return appCfg.Worker.MaxConcurrentActivities
}

func (c AppConfig) validate() error {
	if c.Worker.MaxConcurrentActivities < 1 {
		return fmt.Errorf("worker.maxConcurrentActivities must be >= 1")
	}
	if c.Worker.RetryMaxAttempts < 1 || c.Worker.SweepRetryMaxAttempts < 1 {
		return fmt.Errorf("worker retry attempts must be >= 1")
	}
	if _, err := time.ParseDuration(c.Worker.StartToCloseTimeout); err != nil {
		return fmt.Errorf("worker.startToCloseTimeout %q invalid: %v", c.Worker.StartToCloseTimeout, err)
	}
	if _, err := time.ParseDuration(c.Worker.SweepTimeout); err != nil {
		return fmt.Errorf("worker.sweepTimeout %q invalid: %v", c.Worker.SweepTimeout, err)
	}
	if _, err := time.ParseDuration(c.Worker.DevTimeout); err != nil {
		return fmt.Errorf("worker.devTimeout %q invalid: %v", c.Worker.DevTimeout, err)
	}
	if _, err := time.ParseDuration(c.Defaults.Lifecycle.POGateTimeout); err != nil {
		return fmt.Errorf("defaults.lifecycle.poGateTimeout %q invalid: %v", c.Defaults.Lifecycle.POGateTimeout, err)
	}
	d := c.Defaults.Lifecycle
	if d.DevelopmentMaxComplexity < 0 || d.DevelopmentMaxComplexity > 10 {
		return fmt.Errorf("defaults.lifecycle.developmentMaxComplexity out of range 0-10")
	}
	if d.MaxDevReviewAttempts < 0 || d.MaxDevReviewAttempts > 10 {
		return fmt.Errorf("defaults.lifecycle.maxDevReviewAttempts out of range 0-10")
	}
	if d.MaxResolutionAttempts < 0 {
		return fmt.Errorf("defaults.lifecycle.maxResolutionAttempts must be >= 0")
	}
	return nil
}
