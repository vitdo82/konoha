package konoha

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Resource catalogs (§98): named models selectable per stage and named budgets
// recorded as evidence. A repo/stage selects a catalog entry BY NAME; the
// resolved provider/model + a content digest land in outcome.json so "same
// config, different result" is investigable and a catalog edit is a reviewable
// diff. Unknown names fail at load.
//
// Credential values never reach evidence: a model entry names the env var that
// holds the key (`apiKeyEnv`), and only that NAME is ever recorded.
//
// Egress policies and workspaces are deliberately NOT part of this catalog:
// without a sandbox (see docs/security.md) there is no egress to policy and no
// isolated workspace to name, so stubbing them would be a false capability.

// Model-selectable pipeline stages (§98). The set is closed; an unknown stage in
// lifecycle.models fails at load.
const (
	stageAnalyst    = "analyst"
	stageEstimator  = "estimator"
	stageDeveloper  = "developer"
	stageReviewer   = "reviewer"
	stageRepair     = "repair"
	stageCompletion = "completion"
)

// pipelineStages is the ordered set of stages a repo may select a model for.
var pipelineStages = []string{
	stageAnalyst, stageEstimator, stageDeveloper,
	stageReviewer, stageRepair, stageCompletion,
}

// knownStage reports whether stage is a model-selectable pipeline stage.
func knownStage(stage string) bool {
	for _, s := range pipelineStages {
		if s == stage {
			return true
		}
	}
	return false
}

// AgentModel is one named model catalog entry (§98). Provider + Model form the
// agent's provider/model id. APIKeyEnv is the NAME of the env var holding the
// credential — never its value.
type AgentModel struct {
	Provider  string `yaml:"provider" json:"provider"`
	Model     string `yaml:"model" json:"model"`
	BaseURL   string `yaml:"baseUrl" json:"baseUrl,omitempty"`
	APIKeyEnv string `yaml:"apiKeyEnv" json:"apiKeyEnv,omitempty"`
}

// AgentBudget is one named budget ceiling (§98). Each ceiling is optional
// (pointer = unset). Crossings are computed from recorded #87 usage and recorded
// as evidence — they never rewrite a verdict or shorten a configured timeout.
type AgentBudget struct {
	MaxCostUSD   *float64 `yaml:"maxCostUsd" json:"maxCostUsd,omitempty"`
	MaxTokens    *int64   `yaml:"maxTokens" json:"maxTokens,omitempty"`
	MaxWallClock string   `yaml:"maxWallClock" json:"maxWallClock,omitempty"`
}

// AgentsConfig is the `defaults.agents` block: the named model + budget catalog.
type AgentsConfig struct {
	Models  map[string]AgentModel  `yaml:"models" json:"models,omitempty"`
	Budgets map[string]AgentBudget `yaml:"budgets" json:"budgets,omitempty"`
}

// ResolvedModel is a resolved stage model recorded as evidence: identity +
// content digest, never a credential value (only the apiKeyEnv NAME). Name is
// empty for a legacy raw `lifecycle.model` id.
type ResolvedModel struct {
	Stage     string `json:"stage"`
	Name      string `json:"name,omitempty"`
	Provider  string `json:"provider,omitempty"`
	Model     string `json:"model,omitempty"`
	BaseURL   string `json:"baseUrl,omitempty"`
	APIKeyEnv string `json:"apiKeyEnv,omitempty"` // credential NAME only
	Digest    string `json:"digest"`
}

// AgentModel renders the provider/model id passed to the harness.
func (r ResolvedModel) AgentModel() string {
	if r.Name == "" {
		return r.Model
	}
	return r.Provider + "/" + r.Model
}

// modelDigest is the SHA-256 of the resolved model's non-secret identity
// (canonical JSON). It never includes a credential value — only the apiKeyEnv
// NAME — so a catalog edit is a reviewable digest change.
func modelDigest(name, provider, model, baseURL, apiKeyEnv string) string {
	b, _ := json.Marshal(struct {
		Name      string `json:"name"`
		Provider  string `json:"provider"`
		Model     string `json:"model"`
		BaseURL   string `json:"baseUrl"`
		APIKeyEnv string `json:"apiKeyEnv"`
	}{name, provider, model, baseURL, apiKeyEnv})
	return sha256Hex(b)
}

// sortedModelNames returns the sorted catalog keys (stable error messages).
func sortedModelNames(m map[string]AgentModel) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// sortedBudgetNames returns the sorted budget keys.
func sortedBudgetNames(m map[string]AgentBudget) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// resolveStageModel resolves the model for one stage: an explicit
// lifecycle.models.<stage> name (looked up in the catalog; unknown fails
// closed), else the legacy raw lifecycle.model id, else the harness default
// (empty). Pure given the catalog, so it is replay-safe in workflow code.
func resolveStageModel(agents AgentsConfig, lc LifecycleConfig, stage string) (ResolvedModel, error) {
	name := ""
	if lc.Models != nil {
		name = strings.TrimSpace(lc.Models[stage])
	}
	if name == "" {
		raw := strings.TrimSpace(lc.Model)
		return ResolvedModel{
			Stage:  stage,
			Model:  raw,
			Digest: modelDigest("", "", raw, "", ""),
		}, nil
	}
	spec, ok := agents.Models[name]
	if !ok {
		return ResolvedModel{}, fmt.Errorf("lifecycle.models.%s: unknown model %q (known: %s)", stage, name, strings.Join(sortedModelNames(agents.Models), ", "))
	}
	return ResolvedModel{
		Stage:     stage,
		Name:      name,
		Provider:  spec.Provider,
		Model:     spec.Model,
		BaseURL:   spec.BaseURL,
		APIKeyEnv: spec.APIKeyEnv,
		Digest:    modelDigest(name, spec.Provider, spec.Model, spec.BaseURL, spec.APIKeyEnv),
	}, nil
}

// stageAgentModel resolves a stage's agent provider/model id ("" = harness
// default). An unknown catalog name fails closed.
func stageAgentModel(agents AgentsConfig, lc LifecycleConfig, stage string) (string, error) {
	m, err := resolveStageModel(agents, lc, stage)
	if err != nil {
		return "", err
	}
	return m.AgentModel(), nil
}

// resolveBudget resolves a repo's selected budget by name (nil when unset). An
// unknown name fails closed.
func resolveBudget(agents AgentsConfig, lc LifecycleConfig) (*BudgetEvidence, error) {
	name := strings.TrimSpace(lc.Budget)
	if name == "" {
		return nil, nil
	}
	spec, ok := agents.Budgets[name]
	if !ok {
		return nil, fmt.Errorf("lifecycle.budget: unknown budget %q (known: %s)", name, strings.Join(sortedBudgetNames(agents.Budgets), ", "))
	}
	return &BudgetEvidence{
		Name: name, MaxCostUSD: spec.MaxCostUSD, MaxTokens: spec.MaxTokens, MaxWallClock: spec.MaxWallClock,
	}, nil
}

// ResourceEvidence is the resolved-catalog record persisted in outcome.json
// (§98): every stage's resolved model + digest, and the selected budget.
type ResourceEvidence struct {
	Models []ResolvedModel `json:"models,omitempty"`
	Budget *BudgetEvidence `json:"budget,omitempty"`
}

// modelFor returns the resolved model for a stage (zero value when absent).
func (r *ResourceEvidence) modelFor(stage string) ResolvedModel {
	if r != nil {
		for _, m := range r.Models {
			if m.Stage == stage {
				return m
			}
		}
	}
	return ResolvedModel{Stage: stage}
}

// BudgetCrossing is one ceiling crossing, computed from recorded #87 usage (or
// elapsed wall clock). Evidence only: it never rewrites a verdict.
type BudgetCrossing struct {
	Metric   string  `json:"metric"` // maxCostUsd | maxTokens | maxWallClock
	Limit    float64 `json:"limit"`
	Observed float64 `json:"observed"`
	Exceeded bool    `json:"exceeded"`
}

// BudgetEvidence is the selected budget + its crossings against this cycle's
// recorded usage. It is descriptive evidence (§98); no verdict or timeout reads
// it.
type BudgetEvidence struct {
	Name         string           `json:"name"`
	MaxCostUSD   *float64         `json:"maxCostUsd,omitempty"`
	MaxTokens    *int64           `json:"maxTokens,omitempty"`
	MaxWallClock string           `json:"maxWallClock,omitempty"`
	Crossings    []BudgetCrossing `json:"crossings,omitempty"`
}

// budgetCrossings computes each configured ceiling against the cycle's recorded
// usage + elapsed wall clock. A metric with no reported usage (zero) is recorded
// with Exceeded=false — never inferred (§87: a missing number is not a zero
// observation). Pure, so it is replay-safe.
func budgetCrossings(b *BudgetEvidence, usage HarnessUsage, elapsed time.Duration) []BudgetCrossing {
	if b == nil {
		return nil
	}
	var out []BudgetCrossing
	if b.MaxCostUSD != nil {
		out = append(out, BudgetCrossing{
			Metric: "maxCostUsd", Limit: *b.MaxCostUSD,
			Observed: usage.CostUSD, Exceeded: usage.CostUSD > *b.MaxCostUSD,
		})
	}
	if b.MaxTokens != nil {
		obs := float64(usage.TokensIn + usage.TokensOut)
		out = append(out, BudgetCrossing{
			Metric: "maxTokens", Limit: float64(*b.MaxTokens),
			Observed: obs, Exceeded: obs > float64(*b.MaxTokens),
		})
	}
	if d, err := time.ParseDuration(strings.TrimSpace(b.MaxWallClock)); err == nil && d > 0 {
		obs := elapsed.Seconds()
		out = append(out, BudgetCrossing{
			Metric: "maxWallClock", Limit: d.Seconds(),
			Observed: obs, Exceeded: elapsed > d,
		})
	}
	return out
}

// resolveResources resolves every pipeline stage's model + the selected budget
// for a repo's lifecycle. A catalog name that does not resolve (or a malformed
// id) fails closed. Pure given the catalog.
func resolveResources(agents AgentsConfig, lc LifecycleConfig) (*ResourceEvidence, error) {
	res := &ResourceEvidence{}
	for _, stage := range pipelineStages {
		m, err := resolveStageModel(agents, lc, stage)
		if err != nil {
			return nil, err
		}
		res.Models = append(res.Models, m)
	}
	b, err := resolveBudget(agents, lc)
	if err != nil {
		return nil, err
	}
	res.Budget = b
	return res, nil
}

// validateResourceRefs validates every repo's model/budget name references
// against the catalog, so an unknown name fails at load (apply.sh `prbot
// config`), never mid-cycle. It also rejects an unknown lifecycle.models stage
// key (known only from the closed stage set).
func validateResourceRefs(repos []RepoConfig, agents AgentsConfig) error {
	for i := range repos {
		r := &repos[i]
		if r.Lifecycle == nil {
			continue
		}
		for stage := range r.Lifecycle.Models {
			if !knownStage(stage) {
				return fmt.Errorf("%s: lifecycle.models.%s: unknown stage (known: %s)", r.Repo, stage, strings.Join(pipelineStages, ", "))
			}
		}
		for _, stage := range pipelineStages {
			m, err := resolveStageModel(agents, *r.Lifecycle, stage)
			if err != nil {
				return fmt.Errorf("%s: %w", r.Repo, err)
			}
			if m.AgentModel() != "" {
				if _, err := validateAgentModel(m.AgentModel()); err != nil {
					return fmt.Errorf("%s: lifecycle.models.%s: %w", r.Repo, stage, err)
				}
			}
		}
		if _, err := resolveBudget(agents, *r.Lifecycle); err != nil {
			return fmt.Errorf("%s: %w", r.Repo, err)
		}
	}
	return nil
}

// validate gates the `defaults.agents` catalog: model keys/fields form a valid
// provider/model id, credential references are NAMEs only, and budget ceilings
// are positive. Unknown keys are rejected by the strict YAML decoder.
func (a AgentsConfig) validate() error {
	for name, m := range a.Models {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("defaults.agents.models: empty model name")
		}
		if strings.TrimSpace(m.Provider) == "" || strings.TrimSpace(m.Model) == "" {
			return fmt.Errorf("defaults.agents.models.%s: provider and model are required", name)
		}
		id := m.Provider + "/" + m.Model
		if _, err := validateAgentModel(id); err != nil {
			return fmt.Errorf("defaults.agents.models.%s: %w", name, err)
		}
		if strings.ContainsAny(m.APIKeyEnv, " \t\n") {
			return fmt.Errorf("defaults.agents.models.%s.apiKeyEnv: %q must be a bare env var NAME", name, m.APIKeyEnv)
		}
	}
	for name, b := range a.Budgets {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("defaults.agents.budgets: empty budget name")
		}
		if b.MaxCostUSD != nil && *b.MaxCostUSD <= 0 {
			return fmt.Errorf("defaults.agents.budgets.%s.maxCostUsd: must be > 0", name)
		}
		if b.MaxTokens != nil && *b.MaxTokens <= 0 {
			return fmt.Errorf("defaults.agents.budgets.%s.maxTokens: must be > 0", name)
		}
		if d := strings.TrimSpace(b.MaxWallClock); d != "" {
			if dur, err := time.ParseDuration(d); err != nil || dur <= 0 {
				return fmt.Errorf("defaults.agents.budgets.%s.maxWallClock: %q is not a positive duration", name, b.MaxWallClock)
			}
		}
	}
	return nil
}
