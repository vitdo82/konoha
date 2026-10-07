package konoha

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Enums from §33. Empty values are allowed and mean "use the default".
const (
	modeReviewOnly           = "review-only"
	modeSupervisedAutonomous = "supervised-autonomous"

	autonomyAutonomous = "autonomous"
	autonomySupervised = "supervised"

	closePolicyAutonomous    = "autonomous"
	closePolicyRiskBased     = "risk-based"
	closePolicyHumanRequired = "human-required"
)

// LifecycleConfig is the per-repo lifecycle policy (§33, §43). Live in
// repos.yaml under `lifecycle:` and threaded into RepoScope/IssueInput via
// apply.sh (`prbot config`) so workflows and activities carry the policy
// without re-reading files.
type LifecycleConfig struct {
	Mode                          string            `yaml:"mode" json:"mode,omitempty"`
	Model                         string            `yaml:"model" json:"model,omitempty"`     // opencode agent model for this repo; "" = built-in default
	Models                        map[string]string `yaml:"models" json:"models,omitempty"`   // §98 per-stage catalog name (analyst/estimator/developer/reviewer/repair/completion)
	Budget                        string            `yaml:"budget" json:"budget,omitempty"`   // §98 named budget from defaults.agents.budgets; "" = none
	Harness                       string            `yaml:"harness" json:"harness,omitempty"` // §85 agent harness by NAME; "" = defaults.lifecycle.harness
	Agents                        map[string]string `yaml:"agents" json:"agents,omitempty"`   // #117 per-stage profile selection; "default" = repo-level default
	DevelopmentMaxComplexity      int               `yaml:"developmentMaxComplexity" json:"developmentMaxComplexity,omitempty"`
	MaxDevReviewAttempts          int               `yaml:"maxDevReviewAttempts" json:"maxDevReviewAttempts,omitempty"`
	Autonomy                      Autonomy          `yaml:"autonomy" json:"autonomy,omitempty"`
	Merge                         MergeConfig       `yaml:"merge" json:"merge,omitempty"`
	Release                       ReleaseConfig     `yaml:"release" json:"release,omitempty"`
	Deploy                        DeployConfig      `yaml:"deploy" json:"deploy,omitempty"`
	Deps                          DependencyConfig  `yaml:"deps" json:"deps,omitempty"`
	Grooming                      GroomingConfig    `yaml:"grooming" json:"grooming,omitempty"`
	Alerting                      AlertingConfig    `yaml:"alerting" json:"alerting,omitempty"`
	Propagation                   PropagationConfig `yaml:"propagation" json:"propagation,omitempty"`
	Approval                      ApprovalConfig    `yaml:"approval" json:"approval,omitempty"`
	Audit                         AuditConfig       `yaml:"audit" json:"audit,omitempty"`
	Publish                       PublishConfig     `yaml:"publish" json:"publish,omitempty"`             // #115 PR-description policy
	Validation                    []ValidationGate  `yaml:"validation" json:"validation,omitempty"`       // §26 structured argv gates
	ValidationCmd                 string            `yaml:"validationCmd" json:"validationCmd,omitempty"` // §26 legacy shell-string fallback; mutually exclusive with validation
	Limits                        *LimitsConfig     `yaml:"limits" json:"limits,omitempty"`               // #102 per-repo host resource ceilings (nil = global konoha.yaml limits)
	URLPolicy                     URLPolicy         `yaml:"urlPolicy" json:"urlPolicy,omitempty"`         // #122 outbound allow/deny hosts/CIDRs (advisory on host; enforced in a sandbox)
	SecretNames                   []string          `yaml:"secrets" json:"secrets,omitempty"`             // #122 referenced secret NAMES only (resolution is #CU1; values never recorded)
	POGateTimeout                 string            `yaml:"poGateTimeout" json:"poGateTimeout,omitempty"`
	DryRun                        bool              `yaml:"dryRun" json:"dryRun,omitempty"`
	MaxConcurrentIssueCycles      int               `yaml:"maxConcurrentIssueCycles" json:"maxConcurrentIssueCycles,omitempty"`
	MaxConcurrentChildrenPerIssue int               `yaml:"maxConcurrentChildrenPerIssue" json:"maxConcurrentChildrenPerIssue,omitempty"`
	// Scope is the resolved #104 tenancy scope name governing this repo
	// (productLine or productLine/department). It is set ONCE at load by
	// resolveScopes from konoha.yaml — never accepted from repos.yaml
	// (yaml:"-") — travels in the schedule input, and is part of the #88 policy
	// digest, so a change of scope is a visible diff. Empty = global baseline.
	Scope string `yaml:"-" json:"scope,omitempty"`
	// Profiles/ProfileDigest are the #117 load-resolved agent-profile selection:
	// stage → profile name, and the content digest of the resolved bundle. Set
	// ONCE at load by resolveAgentProfiles (yaml:"-", never accepted from
	// repos.yaml), carried in the schedule input, and stamped onto outcome.json.
	// The workflow only reads the flattened policy (harness/model/limits); it
	// never resolves a profile name.
	Profiles      map[string]string `yaml:"-" json:"profiles,omitempty"`
	ProfileDigest string            `yaml:"-" json:"profileDigest,omitempty"`
	// Tooling is the #119 load-resolved tool + MCP policy: the union of the
	// selected profiles' allow/deny tool lists and MCP server names. Set ONCE at
	// load by resolveAgentProfiles (yaml:"-", never accepted from repos.yaml),
	// carried in the schedule input, recorded in outcome.json, and enforced by
	// the agent activities (an unknown/denied tool fails closed).
	Tooling AgentTooling `yaml:"-" json:"tooling,omitempty"`
}

// ValidationGate is one structured §26 verification gate: a bare argv run in a
// repo-relative dir with a per-gate timeout. Mandatory gates must exit 0 for the
// aggregate to pass; an advisory failure is recorded but tolerated. A gate that
// cannot start records ERROR and fails the aggregate regardless (§40).
type ValidationGate struct {
	ID        string   `yaml:"id" json:"id"`
	Argv      []string `yaml:"argv" json:"argv"`
	Mandatory *bool    `yaml:"mandatory" json:"mandatory,omitempty"` // default true
	Timeout   string   `yaml:"timeout" json:"timeout,omitempty"`     // Go duration; default 10m
	Dir       string   `yaml:"dir" json:"dir,omitempty"`             // repo-relative
}

// isMandatory reports the effective mandatory flag (unset = true).
func (g ValidationGate) isMandatory() bool { return g.Mandatory == nil || *g.Mandatory }

// timeout parses the gate window, defaulting to 10m.
func (g ValidationGate) timeout() time.Duration {
	if d, err := time.ParseDuration(g.Timeout); err == nil && d > 0 {
		return d
	}
	return 10 * time.Minute
}

// validateGates gates one profile's §26 validation block: unique non-empty ids,
// non-empty argv, a repo-relative dir, a positive timeout, and never a
// shell-string ValidationCmd alongside the structured list (ambiguous).
func validateGates(prefix string, gates []ValidationGate, cmd string) error {
	if len(gates) > 0 && strings.TrimSpace(cmd) != "" {
		return fmt.Errorf("%s: validation and validationCmd are mutually exclusive (ambiguous)", prefix)
	}
	seen := map[string]bool{}
	for i, g := range gates {
		if strings.TrimSpace(g.ID) == "" {
			return fmt.Errorf("%s.validation[%d].id: required", prefix, i)
		}
		if seen[g.ID] {
			return fmt.Errorf("%s.validation: duplicate gate id %q", prefix, g.ID)
		}
		seen[g.ID] = true
		if len(g.Argv) == 0 {
			return fmt.Errorf("%s.validation[%s].argv: required (structured argv, never a shell string)", prefix, g.ID)
		}
		for j, a := range g.Argv {
			if strings.TrimSpace(a) == "" {
				return fmt.Errorf("%s.validation[%s].argv[%d]: must be non-empty", prefix, g.ID, j)
			}
		}
		if d := strings.TrimSpace(g.Dir); d != "" {
			if strings.HasPrefix(d, "/") || strings.Contains(d, "..") {
				return fmt.Errorf("%s.validation[%s].dir: %q must be a repo-relative path (no .. / leading /)", prefix, g.ID, g.Dir)
			}
		}
		if g.Timeout != "" {
			if d, err := time.ParseDuration(g.Timeout); err != nil || d <= 0 {
				return fmt.Errorf("%s.validation[%s].timeout: %q is not a positive duration", prefix, g.ID, g.Timeout)
			}
		}
	}
	return nil
}

// MergeConfig is the per-repo PR-merge + CI-gate policy (§49). RequireCI is a
// pointer so "unset" is distinguishable from an explicit false: unset falls
// back to the configured default (true, fail-closed), false opts the repo out.
type MergeConfig struct {
	Strategy  string `yaml:"strategy" json:"strategy,omitempty"`   // squash|rebase|merge
	RequireCI *bool  `yaml:"requireCI" json:"requireCI,omitempty"` // gate the merge on green CI
	CIMaxWait string `yaml:"ciMaxWait" json:"ciMaxWait,omitempty"` // bounded CI poll window (Go duration)
}

// PublishConfig is the per-repo §25 publish policy (#115). AIDescription opts a
// repo into one bounded AI generation of the PR body over already-validated
// artifacts; it defaults OFF (opt-in). Any generation failure falls back to the
// deterministic body, so the PR still opens. The body is evidence only.
type PublishConfig struct {
	AIDescription bool `yaml:"aiDescription" json:"aiDescription,omitempty"`
}

// Autonomy is the §33 autonomy block.
type Autonomy struct {
	Default             string   `yaml:"default" json:"default,omitempty"`
	ClosePolicy         string   `yaml:"closePolicy" json:"closePolicy,omitempty"`
	HumanRequiredPaths  []string `yaml:"humanRequiredPaths" json:"humanRequiredPaths,omitempty"`
	HumanRequiredLabels []string `yaml:"humanRequiredLabels" json:"humanRequiredLabels,omitempty"`
}

// ConcurrentIssueCycles returns the repo-level cap on concurrently running
// IssueCycleWorkflow children (§43). 0/unset means 1 (serialized, the default).
func (lc LifecycleConfig) ConcurrentIssueCycles() int {
	if lc.MaxConcurrentIssueCycles < 1 {
		return 1
	}
	return lc.MaxConcurrentIssueCycles
}

// ConcurrentChildrenPerIssue returns the §43 cap on concurrently running child
// cycles of one split parent. 0/unset means 1 (serialized, the default).
func (lc LifecycleConfig) ConcurrentChildrenPerIssue() int {
	if lc.MaxConcurrentChildrenPerIssue < 1 {
		return 1
	}
	return lc.MaxConcurrentChildrenPerIssue
}

// RepoConfig is one `repos:` entry.
type RepoConfig struct {
	Owner       string           `yaml:"owner"`
	Repo        string           `yaml:"repo"`
	RepoPath    string           `yaml:"repoPath"`
	Schedule    string           `yaml:"schedule"`
	ProductLine string           `yaml:"productLine"` // #104: explicit Product Line scope; default = match by repositories
	Department  string           `yaml:"department"`  // #104: optional Department within the product line
	Lifecycle   *LifecycleConfig `yaml:"lifecycle"`
}

type reposFile struct {
	Repos []RepoConfig `yaml:"repos"`
}

// LoadReposConfig loads and validates repos.yaml. Deterministic: unknown keys
// are rejected (yaml KnownFields) and out-of-range/enum values fail here, not
// at schedule-apply time (§33 "fails fast at load").
func LoadReposConfig(path string) ([]RepoConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var f reposFile
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for i := range f.Repos {
		r := &f.Repos[i]
		if r.Repo == "" || r.Owner == "" {
			return nil, fmt.Errorf("%s: repos[%d]: repo and owner are required", path, i)
		}
		if r.RepoPath == "" {
			return nil, fmt.Errorf("%s: %s: repoPath is required", path, r.Repo)
		}
		if err := r.Lifecycle.validate(); err != nil {
			return nil, fmt.Errorf("%s: %s: %w", path, r.Repo, err)
		}
	}
	return f.Repos, nil
}

// loadReposConfigWithResources loads repos.yaml AND validates its §98
// model/budget name references against the catalog in konoha.yaml, so an unknown
// name fails at apply time (`prbot config`), never mid-cycle. The app config is
// resolved through the same env > file > default path as the worker.
func loadReposConfigWithResources(path string) ([]RepoConfig, error) {
	repos, err := LoadReposConfig(path)
	if err != nil {
		return nil, err
	}
	cfg, err := loadAppConfig(defaultAppConfigPath())
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := validateResourceRefs(repos, cfg.Defaults.Agents); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	// #117: resolve each repo's agent-profile selection ONCE at load, flattening
	// it into the lifecycle (harness/model/limits) so no workflow-side name
	// resolution occurs. Runs BEFORE scopes so a scope narrows the resolved
	// harness/model the profile actually selected.
	if err := resolveAgentProfiles(repos, cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	// #104: resolve the narrow-only tenancy scopes once at load, so the emitted
	// schedule input carries the resolved scope + effective policy (the workflow
	// never resolves a scope name).
	if err := resolveScopes(repos, cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	// #102: a per-repo wall-clock ceiling must not exceed the agent activity
	// window it complements (validated against the resolved worker config).
	for i := range repos {
		if repos[i].Lifecycle != nil && repos[i].Lifecycle.Limits != nil {
			if err := repos[i].Lifecycle.Limits.validateActivityWindow(repos[i].Repo+": lifecycle.limits", cfg.Worker.devWindow()); err != nil {
				return nil, fmt.Errorf("%s: %w", path, err)
			}
		}
	}
	return repos, nil
}

func (lc *LifecycleConfig) validate() error {
	if lc == nil {
		return nil
	}
	switch lc.Mode {
	case "", modeReviewOnly, modeSupervisedAutonomous:
	default:
		return fmt.Errorf("lifecycle.mode: %q not one of %q, %q", lc.Mode, modeReviewOnly, modeSupervisedAutonomous)
	}
	if m := lc.Model; m != "" {
		if strings.TrimSpace(m) != m || !strings.Contains(m, "/") {
			return fmt.Errorf("lifecycle.model: %q must be a provider/model id", m)
		}
	}
	if h := lc.Harness; h != "" && !knownHarnessName(h) {
		return fmt.Errorf("lifecycle.harness: unknown harness %q (known: %s)", h, strings.Join(builtinHarnessNames, ", "))
	}
	// §98: stage keys are a closed set; the catalog NAMEs they reference are
	// validated against the catalog at load by validateResourceRefs.
	for stage := range lc.Models {
		if !knownStage(stage) {
			return fmt.Errorf("lifecycle.models.%s: unknown stage (known: %s)", stage, strings.Join(pipelineStages, ", "))
		}
	}
	// #117: profile selection keys are the closed stage set plus the repo-level
	// `default`; the profile NAMEs are validated against the `agents:` catalog at
	// load by resolveAgentProfiles.
	for stage := range lc.Agents {
		if stage != profileDefaultStage && !knownStage(stage) {
			return fmt.Errorf("lifecycle.agents.%s: unknown stage (known: %s, %s)", stage, strings.Join(pipelineStages, ", "), profileDefaultStage)
		}
	}
	if lc.DevelopmentMaxComplexity < 0 || lc.DevelopmentMaxComplexity > 10 {
		return fmt.Errorf("lifecycle.developmentMaxComplexity: %d out of range 0-10", lc.DevelopmentMaxComplexity)
	}
	if lc.MaxDevReviewAttempts < 0 || lc.MaxDevReviewAttempts > 10 {
		return fmt.Errorf("lifecycle.maxDevReviewAttempts: %d out of range 0-10", lc.MaxDevReviewAttempts)
	}
	switch lc.Autonomy.Default {
	case "", autonomyAutonomous, autonomySupervised:
	default:
		return fmt.Errorf("lifecycle.autonomy.default: %q not one of %q, %q", lc.Autonomy.Default, autonomyAutonomous, autonomySupervised)
	}
	switch lc.Autonomy.ClosePolicy {
	case "", closePolicyAutonomous, closePolicyRiskBased, closePolicyHumanRequired:
	default:
		return fmt.Errorf("lifecycle.autonomy.closePolicy: %q not one of %q, %q, %q", lc.Autonomy.ClosePolicy, closePolicyAutonomous, closePolicyRiskBased, closePolicyHumanRequired)
	}
	if lc.POGateTimeout != "" {
		if _, err := time.ParseDuration(lc.POGateTimeout); err != nil {
			return fmt.Errorf("lifecycle.poGateTimeout: %q is not a duration (want e.g. 168h): %v", lc.POGateTimeout, err)
		}
	}
	if lc.MaxConcurrentIssueCycles < 0 {
		return fmt.Errorf("lifecycle.maxConcurrentIssueCycles: %d must be >= 0", lc.MaxConcurrentIssueCycles)
	}
	if lc.MaxConcurrentChildrenPerIssue < 0 {
		return fmt.Errorf("lifecycle.maxConcurrentChildrenPerIssue: %d must be >= 0", lc.MaxConcurrentChildrenPerIssue)
	}
	if err := lc.Merge.validate("lifecycle.merge"); err != nil {
		return err
	}
	if err := lc.Release.validate("lifecycle.release"); err != nil {
		return err
	}
	if err := lc.Deploy.validate("lifecycle.deploy"); err != nil {
		return err
	}
	if err := lc.Deps.validate("lifecycle.deps"); err != nil {
		return err
	}
	if err := lc.Grooming.validate("lifecycle.grooming"); err != nil {
		return err
	}
	if err := lc.Alerting.validate("lifecycle.alerting"); err != nil {
		return err
	}
	if err := lc.Propagation.validate("lifecycle.propagation"); err != nil {
		return err
	}
	if err := lc.Approval.validate("lifecycle.approval"); err != nil {
		return err
	}
	if err := lc.Audit.validate("lifecycle.audit"); err != nil {
		return err
	}
	if err := validateGates("lifecycle", lc.Validation, lc.ValidationCmd); err != nil {
		return err
	}
	if lc.Limits != nil {
		if err := lc.Limits.validate("lifecycle.limits"); err != nil {
			return err
		}
	}
	// #122: an outbound URL policy or secret reference must be well-formed at
	// load; a bad rule/name never reaches the child evidence path.
	if err := lc.URLPolicy.validate("lifecycle.urlPolicy"); err != nil {
		return err
	}
	for i, s := range lc.SecretNames {
		if err := validateSecretRef(s); err != nil {
			return fmt.Errorf("lifecycle.secrets[%d]: %w", i, err)
		}
	}
	// §55 AC: every gated decision must be recorded. An enabled pre-gate with no
	// audit sink would gate silently, so it fails at load (never a config that
	// gates but records nothing).
	if lc.Approval.PreGate && !lc.Audit.Enabled {
		return fmt.Errorf("lifecycle.audit.enabled: required when lifecycle.approval.preGate is on (every gated decision must be recorded)")
	}
	return nil
}

// validate gates the §50 release block: the changelog path must be repo-relative
// and the version prefix must be a safe tag fragment.
func (rc ReleaseConfig) validate(prefix string) error {
	if p := strings.TrimSpace(rc.ChangelogPath); p != "" {
		if strings.HasPrefix(p, "/") || strings.Contains(p, "..") {
			return fmt.Errorf("%s.changelogPath: %q must be a repo-relative path", prefix, p)
		}
	}
	if strings.ContainsAny(rc.VersionPrefix, " \t\n/") || strings.Contains(rc.VersionPrefix, "..") {
		return fmt.Errorf("%s.versionPrefix: %q contains invalid characters", prefix, rc.VersionPrefix)
	}
	return nil
}

// validate gates the §49 merge block: the strategy is a closed whitelist and
// ciMaxWait (when set) is a positive Go duration.
func (mc MergeConfig) validate(prefix string) error {
	if mc.Strategy != "" && !ValidMergeStrategy(mc.Strategy) {
		return fmt.Errorf("%s.strategy: %q not one of %q, %q, %q",
			prefix, mc.Strategy, mergeStrategySquash, mergeStrategyRebase, mergeStrategyMerge)
	}
	if mc.CIMaxWait != "" {
		if d, err := time.ParseDuration(mc.CIMaxWait); err != nil || d <= 0 {
			return fmt.Errorf("%s.ciMaxWait: %q is not a positive duration", prefix, mc.CIMaxWait)
		}
	}
	return nil
}

// repoScope builds the schedule workflow input for one repos.yaml entry.
func repoScope(r RepoConfig) RepoScope {
	scope := RepoScope{Owner: r.Owner, Repo: r.Repo, RepoPath: r.RepoPath}
	if r.Lifecycle != nil {
		scope.Lifecycle = *r.Lifecycle
	}
	return scope
}

// scheduleInputs renders repos.yaml into the per-repo schedule workflow input
// (RepoScope JSON, one "<repo>\t<json>" line each). apply.sh feeds these lines
// to `temporal schedule create`, so parsing/validation lives in Go and never
// drifts from what the workflows decode.
func scheduleInputs(path string) ([]string, error) {
	repos, err := loadReposConfigWithResources(path)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, r := range repos {
		b, err := json.Marshal(repoScope(r))
		if err != nil {
			return nil, err
		}
		out = append(out, r.Repo+"\t"+string(b))
	}
	return out, nil
}

// depsScheduleInputs renders the §52 dependency schedules: only repos with
// deps.enabled emit a line, as "<repo>\t<interval>\t<scope json>". apply.sh
// creates one <repo>-deps DependencyWorkflow schedule per line, so a disabled
// repo has no scan and no issues.
func depsScheduleInputs(path string) ([]string, error) {
	repos, err := loadReposConfigWithResources(path)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, r := range repos {
		if r.Lifecycle == nil || !DependencyEnabled(*r.Lifecycle) {
			continue
		}
		b, err := json.Marshal(repoScope(r))
		if err != nil {
			return nil, err
		}
		out = append(out, r.Repo+"\t"+DependencySchedule(*r.Lifecycle)+"\t"+string(b))
	}
	return out, nil
}

// alertsScheduleInputs renders the §53 alert schedules: only repos with
// alerting.enabled emit a line, as "<repo>\t<interval>\t<scope json>". apply.sh
// creates one <repo>-alerts AlertWorkflow schedule per line, so a disabled repo
// has no evaluation.
func alertsScheduleInputs(path string) ([]string, error) {
	repos, err := loadReposConfigWithResources(path)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, r := range repos {
		if r.Lifecycle == nil || !AlertingEnabled(*r.Lifecycle) {
			continue
		}
		b, err := json.Marshal(repoScope(r))
		if err != nil {
			return nil, err
		}
		out = append(out, r.Repo+"\t"+AlertingSchedule(*r.Lifecycle)+"\t"+string(b))
	}
	return out, nil
}

// groomingScheduleInputs renders the §56 backlog-grooming schedules: only repos
// with grooming.enabled emit a line, as "<repo>\t<interval>\t<scope json>".
// apply.sh creates one <repo>-groom BacklogGroomWorkflow schedule per line, so a
// disabled repo never grooms.
func groomingScheduleInputs(path string) ([]string, error) {
	repos, err := loadReposConfigWithResources(path)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, r := range repos {
		if r.Lifecycle == nil || !GroomingEnabled(*r.Lifecycle) {
			continue
		}
		b, err := json.Marshal(repoScope(r))
		if err != nil {
			return nil, err
		}
		out = append(out, r.Repo+"\t"+GroomingSchedule(*r.Lifecycle)+"\t"+string(b))
	}
	return out, nil
}
