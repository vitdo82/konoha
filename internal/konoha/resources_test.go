package konoha

import (
	"strings"
	"testing"
	"time"
)

// §98: a named model resolves to its provider/model id and a stable content
// digest; the credential is referenced by NAME only.
func TestResolveStageModelNamedAndDigest(t *testing.T) {
	agents := AgentsConfig{Models: map[string]AgentModel{
		"cheap":  {Provider: "anthropic", Model: "claude-haiku", APIKeyEnv: "ANTHROPIC_API_KEY"},
		"strong": {Provider: "anthropic", Model: "claude-sonnet", BaseURL: "https://api.example"},
	}}
	lc := LifecycleConfig{Models: map[string]string{stageDeveloper: "strong", stageAnalyst: "cheap"}}

	dev, err := resolveStageModel(agents, lc, stageDeveloper)
	if err != nil {
		t.Fatalf("resolve developer: %v", err)
	}
	if dev.AgentModel() != "anthropic/claude-sonnet" || dev.Provider != "anthropic" || dev.Model != "claude-sonnet" {
		t.Fatalf("resolved = %+v, want anthropic/claude-sonnet", dev)
	}
	if len(dev.Digest) != 64 || strings.Trim(dev.Digest, "0123456789abcdef") != "" {
		t.Fatalf("digest = %q, want a 64-char SHA-256 hex", dev.Digest)
	}
	if dev.Digest != resolveStageModelMust(t, agents, lc, stageDeveloper).Digest {
		t.Fatal("digest must be stable for identical input")
	}

	an, err := resolveStageModel(agents, lc, stageAnalyst)
	if err != nil {
		t.Fatalf("resolve analyst: %v", err)
	}
	// Per-stage selection: a distinct name resolves to a distinct model.
	if an.AgentModel() != "anthropic/claude-haiku" || an.Digest == dev.Digest {
		t.Fatalf("analyst = %+v, want a distinct anthropic/claude-haiku", an)
	}
	if an.APIKeyEnv != "ANTHROPIC_API_KEY" {
		t.Fatalf("apiKeyEnv name must be recorded, got %q", an.APIKeyEnv)
	}

	// A catalog edit flips the digest (reviewable diff).
	edited := agents
	edited.Models["strong"] = AgentModel{Provider: "anthropic", Model: "claude-opus"}
	if resolveStageModelMust(t, edited, lc, stageDeveloper).Digest == dev.Digest {
		t.Fatal("a catalog edit must change the digest")
	}
}

func resolveStageModelMust(t *testing.T, agents AgentsConfig, lc LifecycleConfig, stage string) ResolvedModel {
	t.Helper()
	m, err := resolveStageModel(agents, lc, stage)
	if err != nil {
		t.Fatalf("resolve %s: %v", stage, err)
	}
	return m
}

// §98/§40: an unknown model name fails closed (never silently falls back).
func TestResolveStageModelUnknownFailsClosed(t *testing.T) {
	agents := AgentsConfig{Models: map[string]AgentModel{"cheap": {Provider: "p", Model: "m"}}}
	lc := LifecycleConfig{Models: map[string]string{stageDeveloper: "missing"}}
	if _, err := resolveStageModel(agents, lc, stageDeveloper); err == nil {
		t.Fatal("an unknown model name must fail closed")
	}
	// Legacy raw lifecycle.model still resolves (no catalog name).
	legacy := LifecycleConfig{Model: "openai/gpt"}
	if m, err := resolveStageModel(agents, legacy, stageDeveloper); err != nil || m.AgentModel() != "openai/gpt" {
		t.Fatalf("legacy model = %+v (err=%v), want openai/gpt", m, err)
	}
}

// §98 AC: an unknown model or budget name fails at load (validation), and an
// unknown stage key is rejected. A valid reference passes.
func TestValidateResourceRefsUnknownNameFailsLoad(t *testing.T) {
	agents := AgentsConfig{
		Models:  map[string]AgentModel{"cheap": {Provider: "p", Model: "m"}},
		Budgets: map[string]AgentBudget{"small": {MaxTokens: int64Ptr(100)}},
	}
	ok := []RepoConfig{{Repo: "r", Lifecycle: &LifecycleConfig{
		Models: map[string]string{stageDeveloper: "cheap"}, Budget: "small",
	}}}
	if err := validateResourceRefs(ok, agents); err != nil {
		t.Fatalf("valid references must pass, got %v", err)
	}
	if err := validateResourceRefs([]RepoConfig{{Repo: "r", Lifecycle: &LifecycleConfig{
		Models: map[string]string{stageDeveloper: "nope"},
	}}}, agents); err == nil {
		t.Fatal("an unknown model name must fail at load")
	}
	if err := validateResourceRefs([]RepoConfig{{Repo: "r", Lifecycle: &LifecycleConfig{
		Budget: "nope",
	}}}, agents); err == nil {
		t.Fatal("an unknown budget name must fail at load")
	}
	// Unknown stage keys are rejected by the repos loader itself.
	if err := (&LifecycleConfig{Models: map[string]string{"bogus": "cheap"}}).validate(); err == nil {
		t.Fatal("an unknown lifecycle.models stage must fail at load")
	}
}

// §98/§40: budget crossings are computed from recorded usage as evidence; they
// never gate a verdict (the pure function has no verdict input/output at all).
func TestBudgetCrossingsEvidenceOnly(t *testing.T) {
	cost := 2.0
	tokens := int64(100)
	b := &BudgetEvidence{
		Name: "small", MaxCostUSD: &cost, MaxTokens: &tokens, MaxWallClock: "1h",
	}
	usage := HarnessUsage{TokensIn: 80, TokensOut: 40, CostUSD: 3.5}
	crossings := budgetCrossings(b, usage, 30*time.Minute)
	if len(crossings) != 3 {
		t.Fatalf("crossings = %+v, want 3 (cost, tokens, wall clock)", crossings)
	}
	byMetric := map[string]BudgetCrossing{}
	for _, c := range crossings {
		byMetric[c.Metric] = c
	}
	if c := byMetric["maxCostUsd"]; !c.Exceeded || c.Observed != 3.5 {
		t.Fatalf("cost crossing = %+v, want exceeded observed 3.5", c)
	}
	if c := byMetric["maxTokens"]; !c.Exceeded || c.Observed != 120 {
		t.Fatalf("token crossing = %+v, want exceeded observed 120", c)
	}
	if c := byMetric["maxWallClock"]; c.Exceeded {
		t.Fatalf("wall-clock crossing = %+v, want not exceeded at 30m/1h", c)
	}
	if got := budgetCrossings(nil, usage, time.Hour); got != nil {
		t.Fatalf("no budget means no crossings, got %+v", got)
	}
}

// §98: the catalog is validated at load (positive ceilings, valid model ids).
func TestAgentsConfigValidate(t *testing.T) {
	neg, zero := -1.0, int64(0)
	bad := []AgentsConfig{
		{Models: map[string]AgentModel{"x": {Provider: "p"}}},
		{Models: map[string]AgentModel{"x": {Provider: "p", Model: "bad model"}}},
		{Budgets: map[string]AgentBudget{"b": {MaxCostUSD: &neg}}},
		{Budgets: map[string]AgentBudget{"b": {MaxTokens: &zero}}},
		{Budgets: map[string]AgentBudget{"b": {MaxWallClock: "nope"}}},
	}
	for i, c := range bad {
		if err := c.validate(); err == nil {
			t.Fatalf("bad catalog %d must fail validation", i)
		}
	}
	good := AgentsConfig{
		Models:  map[string]AgentModel{"x": {Provider: "p", Model: "m", APIKeyEnv: "KEY"}},
		Budgets: map[string]AgentBudget{"b": {MaxCostUSD: &cost5, MaxTokens: &tokens100}},
	}
	if err := good.validate(); err != nil {
		t.Fatalf("valid catalog must pass: %v", err)
	}
}

var cost5 = 5.0
var tokens100 = int64(100)

func int64Ptr(v int64) *int64 { return &v }
