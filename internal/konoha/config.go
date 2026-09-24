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
	Harness                       string            `yaml:"harness" json:"harness,omitempty"` // §85 agent harness by NAME; "" = defaults.lifecycle.harness
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
	Validation                    []ValidationGate  `yaml:"validation" json:"validation,omitempty"`       // §26 structured argv gates
	ValidationCmd                 string            `yaml:"validationCmd" json:"validationCmd,omitempty"` // §26 legacy shell-string fallback; mutually exclusive with validation
	POGateTimeout                 string            `yaml:"poGateTimeout" json:"poGateTimeout,omitempty"`
	DryRun                        bool              `yaml:"dryRun" json:"dryRun,omitempty"`
	MaxConcurrentIssueCycles      int               `yaml:"maxConcurrentIssueCycles" json:"maxConcurrentIssueCycles,omitempty"`
	MaxConcurrentChildrenPerIssue int               `yaml:"maxConcurrentChildrenPerIssue" json:"maxConcurrentChildrenPerIssue,omitempty"`
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
	Owner     string           `yaml:"owner"`
	Repo      string           `yaml:"repo"`
	RepoPath  string           `yaml:"repoPath"`
	Schedule  string           `yaml:"schedule"`
	Lifecycle *LifecycleConfig `yaml:"lifecycle"`
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
	repos, err := LoadReposConfig(path)
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
	repos, err := LoadReposConfig(path)
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
	repos, err := LoadReposConfig(path)
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
	repos, err := LoadReposConfig(path)
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
