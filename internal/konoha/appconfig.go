package konoha

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
	// #95: infrastructure-class activity attempts (gh/git/evidence I/O, with
	// exponential backoff). Agent/verification/deterministic attempt counts are
	// fixed in retry.go, not config-driven.
	RetryMaxAttempts      int `yaml:"retryMaxAttempts"`
	SweepRetryMaxAttempts int `yaml:"sweepRetryMaxAttempts"`
	// #93 worktree teardown: how hard the idempotent CleanupWorktree activity
	// retries (with backoff), and the age past which a konoha-owned worktree
	// whose workflow is gone is swept as an orphan.
	CleanupMaxAttempts int    `yaml:"cleanupMaxAttempts"`
	OrphanWorktreeTTL  string `yaml:"orphanWorktreeTTL"`
}

// LifecycleDefaults is the `defaults.lifecycle` block: the fallback per-repo
// policy when repos.yaml leaves the field unset.
type LifecycleDefaults struct {
	DevelopmentMaxComplexity int              `yaml:"developmentMaxComplexity"`
	MaxDevReviewAttempts     int              `yaml:"maxDevReviewAttempts"`
	MaxResolutionAttempts    int              `yaml:"maxResolutionAttempts"`
	POGateTimeout            string           `yaml:"poGateTimeout"`
	Harness                  string           `yaml:"harness"`
	Merge                    MergeConfig      `yaml:"merge"`
	Deploy                   DeployConfig     `yaml:"deploy"`
	Validation               []ValidationGate `yaml:"validation"`
	ValidationCmd            string           `yaml:"validationCmd"`
}

// AppConfig is the resolved application configuration.
type AppConfig struct {
	Worker     WorkerConfig             `yaml:"worker"`
	Evidence   EvidenceConfig           `yaml:"evidence"`
	Harness    GenericHarnessConfig     `yaml:"harness"`
	Limits     LimitsConfig             `yaml:"limits"`     // #102 global host resource ceilings (per-repo lifecycle.limits overrides)
	Egress     EgressConfig             `yaml:"egress"`     // #103 advisory proxy allowlist (bypassable; NOT a boundary)
	Facility   string                   `yaml:"facility"`   // #104 tenancy Facility label (recorded; not an auth boundary — single-operator)
	Scopes     ScopesConfig             `yaml:"scopes"`     // #104 narrow-only product-line scopes (Paddock Facility > Product Line > Department)
	Agents     AgentProfilesConfig      `yaml:"agents"`     // #117 declarative agent profiles (harness/model/tools/mcp/limits)
	Tools      ToolsConfig              `yaml:"tools"`      // #119 tool catalog (custom tools) — built-ins are code-level
	MCPServers map[string]MCPServerSpec `yaml:"mcpServers"` // #119 named HTTP MCP server registry
	Defaults   DefaultsConfig           `yaml:"defaults"`
}

// GenericHarnessConfig is the operator-declared `generic` harness (§85): a bare
// executable NAME (never a path), fixed argv with {{model_args}}/{{prompt}}
// placeholders, the model flag, an explicit env allowlist, and prompt transport.
// #96 adds optional provisioning: a pinned binary (+ sha256), an offline model
// catalog cache, an explicit prompt mode (stdin_file), and MCP server config.
// All binary/catalog paths are operator configuration; a caller never supplies one.
type GenericHarnessConfig struct {
	Executable    string     `yaml:"executable"`
	Args          []string   `yaml:"args"`
	ModelFlag     string     `yaml:"modelFlag"`
	PassEnv       []string   `yaml:"passEnv"`
	PromptOnStdin bool       `yaml:"promptOnStdin"`
	PromptMode    string     `yaml:"prompt_mode"`   // arg | stdin | stdin_file (overrides promptOnStdin)
	Binary        string     `yaml:"binary"`        // operator path to a standalone binary to stage
	BinarySHA256  string     `yaml:"binary_sha256"` // required sha256 of `binary`; mismatch fails closed
	CatalogCache  string     `yaml:"catalog_cache"` // operator path to a model catalog for offline resolution
	MCP           *MCPConfig `yaml:"mcp"`           // optional MCP server config for supporting harnesses
}

// MCPConfig is the operator-declared MCP server config (#96): the
// worktree-relative Path to write and the Config content.
type MCPConfig struct {
	Path   string `yaml:"path"`
	Config string `yaml:"config"`
}

// EvidenceConfig is the `evidence:` block (#83): the durable per-cycle evidence
// store root. Resolved once at bootstrap so workflow replay sees a stable value.
// Sink selects the storage backend (#100): empty/"local" is the local-directory
// sink; an S3/MinIO backend is the documented deferred seam and is rejected as
// unknown until it exists.
type EvidenceConfig struct {
	Dir  string `yaml:"dir"`
	Sink string `yaml:"sink"`
}

// defaultEvidenceDir is where per-cycle evidence lands when no konoha.yaml
// overrides it. Relative to the worker's working directory.
const defaultEvidenceDir = ".konoha/evidence"

// evidenceSinkLocal is the only implemented evidence backend (#100).
const evidenceSinkLocal = "local"

// sinkName resolves the configured backend name, defaulting an unset value to
// the local-directory sink.
func (e EvidenceConfig) sinkName() string {
	if n := strings.ToLower(strings.TrimSpace(e.Sink)); n != "" {
		return n
	}
	return evidenceSinkLocal
}

// validate gates the evidence block: a non-empty root with no NUL, and a known
// backend name (#100). An unknown/unconfigured backend fails fast at load.
func (e EvidenceConfig) validate() error {
	if strings.TrimSpace(e.Dir) == "" {
		return fmt.Errorf("evidence.dir must not be empty")
	}
	if strings.ContainsRune(e.Dir, 0) {
		return fmt.Errorf("evidence.dir %q contains a NUL byte", e.Dir)
	}
	if name := e.sinkName(); name != evidenceSinkLocal {
		return fmt.Errorf("evidence.sink: unknown backend %q (supported: %s)", e.Sink, evidenceSinkLocal)
	}
	return nil
}

// DefaultsConfig is the `defaults:` block.
type DefaultsConfig struct {
	Lifecycle LifecycleDefaults         `yaml:"lifecycle"`
	Agents    AgentsConfig              `yaml:"agents"`    // §98 named model + budget catalog
	Providers map[string]ProviderConfig `yaml:"providers"` // #118 provider/protocol catalog (validated at load)
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
			RetryMaxAttempts:        defaultInfraRetryMaxAttempts,
			SweepRetryMaxAttempts:   2,
			CleanupMaxAttempts:      5,
			OrphanWorktreeTTL:       defaultOrphanWorktreeTTL,
		},
		Evidence: EvidenceConfig{Dir: defaultEvidenceDir, Sink: evidenceSinkLocal},
		Defaults: DefaultsConfig{
			Lifecycle: LifecycleDefaults{
				DevelopmentMaxComplexity: 4,
				MaxDevReviewAttempts:     2,
				MaxResolutionAttempts:    1,
				POGateTimeout:            "168h",
				Harness:                  defaultHarnessName,
				Merge: MergeConfig{
					Strategy:  mergeStrategySquash,
					RequireCI: boolPtr(true),
					CIMaxWait: "10m",
				},
				Deploy: DeployConfig{
					Env:            defaultDeployEnv,
					MaxWait:        "5m",
					RollbackOnFail: boolPtr(true),
				},
			},
		},
	}
}

// validate gates the §85 generic harness block: the executable is a bare name
// (name, not path), the allowlist entries are non-empty, and the #96
// provisioning fields are consistent (pinned binary needs a sha256, prompt mode
// is a known value, MCP config is complete and worktree-relative).
func (g GenericHarnessConfig) validate() error {
	if e := strings.TrimSpace(g.Executable); e != "" {
		if e != g.Executable || strings.ContainsAny(e, `/\`) || strings.Contains(e, "..") {
			return fmt.Errorf("harness.generic.executable: %q must be a bare executable name, not a path", g.Executable)
		}
	}
	for _, k := range g.PassEnv {
		if strings.TrimSpace(k) == "" {
			return fmt.Errorf("harness.generic.passEnv: entries must be non-empty env var names")
		}
	}
	switch strings.ToLower(strings.TrimSpace(g.PromptMode)) {
	case "", promptModeArg, promptModeStdin, promptModeStdinFile:
	default:
		return fmt.Errorf("harness.generic.prompt_mode: %q must be arg, stdin or stdin_file", g.PromptMode)
	}
	bin := strings.TrimSpace(g.Binary)
	sha := strings.ToLower(strings.TrimSpace(g.BinarySHA256))
	if bin == "" && sha != "" {
		return fmt.Errorf("harness.generic.binary_sha256 set without harness.generic.binary")
	}
	if bin != "" {
		if len(sha) != 64 || strings.Trim(sha, "0123456789abcdef") != "" {
			return fmt.Errorf("harness.generic.binary_sha256: must be a 64-char hex sha256")
		}
	}
	if strings.TrimSpace(g.CatalogCache) != "" && strings.TrimSpace(g.ModelFlag) == "" {
		return fmt.Errorf("harness.generic.catalog_cache requires harness.generic.modelFlag")
	}
	if g.MCP != nil {
		p := filepath.Clean(strings.TrimSpace(g.MCP.Path))
		if strings.TrimSpace(g.MCP.Path) == "" || filepath.IsAbs(p) || p == ".." || strings.HasPrefix(p, ".."+string(filepath.Separator)) {
			return fmt.Errorf("harness.generic.mcp.path: %q must be a worktree-relative path", g.MCP.Path)
		}
		if strings.TrimSpace(g.MCP.Config) == "" {
			return fmt.Errorf("harness.generic.mcp.config must not be empty")
		}
	}
	return nil
}

// boolPtr is a helper for the optional merge.requireCI bool default.
func boolPtr(b bool) *bool { return &b }

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

// infraAttempts resolves the infrastructure-class activity attempt count,
// falling back to the bundled default when unset/invalid (#95).
func (w WorkerConfig) infraAttempts() int {
	if w.RetryMaxAttempts < 1 {
		return defaultInfraRetryMaxAttempts
	}
	return w.RetryMaxAttempts
}

// cleanupAttempts resolves how many times the idempotent CleanupWorktree
// activity retries a removal before recording a failure (#93).
func (w WorkerConfig) cleanupAttempts() int {
	if w.CleanupMaxAttempts < 1 {
		return 1
	}
	return w.CleanupMaxAttempts
}

// orphanTTL parses the orphan-worktree age threshold (#93).
func (w WorkerConfig) orphanTTL() time.Duration {
	d, err := time.ParseDuration(w.OrphanWorktreeTTL)
	if err != nil || d <= 0 {
		return 24 * time.Hour
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
	if err := c.Evidence.validate(); err != nil {
		return err
	}
	// #102: invalid ceilings fail fast; a wall-clock ceiling must not exceed the
	// agent activity window it complements.
	if err := c.Limits.validate("limits"); err != nil {
		return err
	}
	if err := c.Limits.validateActivityWindow("limits", c.Worker.devWindow()); err != nil {
		return err
	}
	// #103: advisory egress must be a well-formed http(s) proxy URL; absent is
	// valid (= today's behavior).
	if err := c.Egress.validate("egress"); err != nil {
		return err
	}
	// #104: the tenancy scopes block is structurally validated here; its
	// references are checked against repos.yaml + the catalogs by resolveScopes.
	if err := c.Scopes.validate(); err != nil {
		return err
	}
	if c.Worker.MaxConcurrentActivities < 1 {
		return fmt.Errorf("worker.maxConcurrentActivities must be >= 1")
	}
	if c.Worker.RetryMaxAttempts < 1 || c.Worker.SweepRetryMaxAttempts < 1 {
		return fmt.Errorf("worker retry attempts must be >= 1")
	}
	if c.Worker.CleanupMaxAttempts < 1 {
		return fmt.Errorf("worker.cleanupMaxAttempts must be >= 1")
	}
	if d, err := time.ParseDuration(c.Worker.OrphanWorktreeTTL); err != nil || d <= 0 {
		return fmt.Errorf("worker.orphanWorktreeTTL %q must be a positive duration", c.Worker.OrphanWorktreeTTL)
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
	if h := c.Defaults.Lifecycle.Harness; h != "" && !knownHarnessName(h) {
		return fmt.Errorf("defaults.lifecycle.harness: unknown harness %q", h)
	}
	if err := c.Harness.validate(); err != nil {
		return err
	}
	// #119: the tool catalog and MCP server registry are validated structurally
	// here; profile references are checked against them below.
	if err := c.Tools.validate(); err != nil {
		return err
	}
	if err := validateMCPServers(c.MCPServers); err != nil {
		return err
	}
	if err := c.Defaults.Agents.validate(); err != nil {
		return err
	}
	// #118: every provider entry is validated (known protocol/gateway, http(s)
	// base URL, bare apiKeyEnv NAME) so a typo fails at load.
	for name, pc := range c.Defaults.Providers {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("defaults.providers: empty provider name")
		}
		if err := pc.validate("defaults.providers." + name); err != nil {
			return err
		}
	}
	// #117/#119: every declared profile is validated (known harness/model/tool/
	// mcp refs against the catalog + registry), even if no repo selects it, so a
	// typo fails at load.
	if err := c.Agents.validate(c.Defaults.Agents, c.Tools, c.MCPServers); err != nil {
		return err
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
	if err := c.Defaults.Lifecycle.Merge.validate("defaults.lifecycle.merge"); err != nil {
		return err
	}
	if err := c.Defaults.Lifecycle.Deploy.validate("defaults.lifecycle.deploy"); err != nil {
		return err
	}
	if err := validateGates("defaults.lifecycle", c.Defaults.Lifecycle.Validation, c.Defaults.Lifecycle.ValidationCmd); err != nil {
		return err
	}
	return nil
}
