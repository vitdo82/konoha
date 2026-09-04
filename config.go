package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
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
	Mode                          string   `yaml:"mode" json:"mode,omitempty"`
	DevelopmentMaxComplexity      int      `yaml:"developmentMaxComplexity" json:"developmentMaxComplexity,omitempty"`
	MaxDevReviewAttempts          int      `yaml:"maxDevReviewAttempts" json:"maxDevReviewAttempts,omitempty"`
	Autonomy                      Autonomy `yaml:"autonomy" json:"autonomy,omitempty"`
	POGateTimeout                 string   `yaml:"poGateTimeout" json:"poGateTimeout,omitempty"`
	DryRun                        bool     `yaml:"dryRun" json:"dryRun,omitempty"`
	MaxConcurrentIssueCycles      int      `yaml:"maxConcurrentIssueCycles" json:"maxConcurrentIssueCycles,omitempty"`
	MaxConcurrentChildrenPerIssue int      `yaml:"maxConcurrentChildrenPerIssue" json:"maxConcurrentChildrenPerIssue,omitempty"`
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
	return nil
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
		scope := RepoScope{Owner: r.Owner, Repo: r.Repo, RepoPath: r.RepoPath}
		if r.Lifecycle != nil {
			scope.Lifecycle = *r.Lifecycle
		}
		b, err := json.Marshal(scope)
		if err != nil {
			return nil, err
		}
		out = append(out, r.Repo+"\t"+string(b))
	}
	return out, nil
}
