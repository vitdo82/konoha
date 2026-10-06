package konoha

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// scopeTestAppConfig builds a minimal resolved AppConfig with a §98 catalog for
// scope-resolution tests.
func scopeTestAppConfig(scopes ScopesConfig) AppConfig {
	cfg := defaultAppConfig()
	cfg.Scopes = scopes
	cfg.Defaults.Agents = AgentsConfig{
		Models: map[string]AgentModel{
			"cheap":  {Provider: "anthropic", Model: "claude-haiku"},
			"strong": {Provider: "anthropic", Model: "claude-sonnet"},
		},
		Budgets: map[string]AgentBudget{"small": {}},
	}
	return cfg
}

func cloneRepos(in []RepoConfig) []RepoConfig {
	out := make([]RepoConfig, len(in))
	for i, r := range in {
		if r.Lifecycle != nil {
			lc := *r.Lifecycle
			r.Lifecycle = &lc
		}
		out[i] = r
	}
	return out
}

// #104 AC: a scope may only narrow. A reference outside the global allowlist
// (repo/model/harness/budget) is rejected at load — never silently dropped.
func TestScopeNarrowOnlyRejectedAtLoad(t *testing.T) {
	base := []RepoConfig{{Owner: "acme", Repo: "payments", RepoPath: "/x"}}
	cases := []struct {
		name string
		sc   ScopesConfig
	}{
		{"unknown repository", ScopesConfig{"payments": {Repositories: []string{"acme/nope"}}}},
		{"unknown harness", ScopesConfig{"payments": {Repositories: []string{"acme/payments"}, Harnesses: []string{"bogus"}}}},
		{"unknown model", ScopesConfig{"payments": {Repositories: []string{"acme/payments"}, Models: []string{"bogus"}}}},
		{"unknown budget", ScopesConfig{"payments": {Repositories: []string{"acme/payments"}, Budgets: []string{"bogus"}}}},
		{"department unknown model", ScopesConfig{"payments": {
			Repositories: []string{"acme/payments"},
			Departments:  map[string]DepartmentScope{"core": {Repositories: []string{"acme/payments"}, Models: []string{"bogus"}}},
		}}},
	}
	for _, tc := range cases {
		if err := resolveScopes(cloneRepos(base), scopeTestAppConfig(tc.sc)); err == nil {
			t.Fatalf("%s: must fail closed at load", tc.name)
		}
	}
}

// #104 AC: a Facility/Product Line/Department hierarchy resolves to effective
// scopes; an unknown product line/department fails at load. The default mapping
// assigns a repo to the scope that lists it.
func TestScopeHierarchyResolvesAndUnknownFails(t *testing.T) {
	scopes := ScopesConfig{
		"payments": {
			Repositories: []string{"acme/payments", "acme/payments-infra"},
			Models:       []string{"cheap", "strong"},
			Budgets:      []string{"small"},
			Departments: map[string]DepartmentScope{
				"checkout": {
					Repositories: []string{"acme/payments-checkout"},
					Models:       []string{"strong"},
					ClosePolicy:  closePolicyHumanRequired,
				},
			},
		},
	}
	repos := []RepoConfig{
		{Owner: "acme", Repo: "payments", RepoPath: "/x", ProductLine: "payments"},                                  // explicit product line
		{Owner: "acme", Repo: "payments-infra", RepoPath: "/x"},                                                     // default mapping
		{Owner: "acme", Repo: "payments-checkout", RepoPath: "/x", ProductLine: "payments", Department: "checkout"}, // department
	}
	cfg := scopeTestAppConfig(scopes)
	if err := resolveScopes(repos, cfg); err != nil {
		t.Fatalf("resolveScopes: %v", err)
	}
	if got := repos[0].Lifecycle.Scope; got != "payments" {
		t.Fatalf("explicit product line scope = %q, want payments", got)
	}
	if got := repos[1].Lifecycle.Scope; got != "payments" {
		t.Fatalf("default-mapped scope = %q, want payments", got)
	}
	if got := repos[2].Lifecycle.Scope; got != "payments/checkout" {
		t.Fatalf("department scope = %q, want payments/checkout", got)
	}
	if got := repos[2].Lifecycle.Autonomy.ClosePolicy; got != closePolicyHumanRequired {
		t.Fatalf("department closePolicy = %q, want human-required (narrowed)", got)
	}

	// Unknown references fail closed.
	if err := resolveScopes([]RepoConfig{{Owner: "acme", Repo: "x", RepoPath: "/x", ProductLine: "nope"}}, cfg); err == nil {
		t.Fatal("an unknown productLine must fail at load")
	}
	if err := resolveScopes([]RepoConfig{{Owner: "acme", Repo: "payments", RepoPath: "/x", ProductLine: "payments", Department: "nope"}}, cfg); err == nil {
		t.Fatal("an unknown department must fail at load")
	}
}

// #104/§40: a repo whose explicit policy widens its scope (closePolicy looser,
// or a model/budget/harness the scope does not allow) fails at load.
func TestScopeRepoWideningFailsClosed(t *testing.T) {
	scopes := ScopesConfig{"payments": {
		Repositories: []string{"acme/payments"},
		Models:       []string{"cheap"},
		Harnesses:    []string{"opencode"},
		ClosePolicy:  closePolicyHumanRequired,
	}}
	base := RepoConfig{Owner: "acme", Repo: "payments", RepoPath: "/x", ProductLine: "payments"}

	model := cloneRepos([]RepoConfig{base})[0]
	model.Lifecycle = &LifecycleConfig{Models: map[string]string{stageDeveloper: "strong"}}
	if err := resolveScopes([]RepoConfig{model}, scopeTestAppConfig(scopes)); err == nil {
		t.Fatal("a model outside the scope allowlist must fail at load")
	}

	harness := base
	harness.Lifecycle = &LifecycleConfig{Harness: "generic"}
	if err := resolveScopes([]RepoConfig{harness}, scopeTestAppConfig(scopes)); err == nil {
		t.Fatal("a harness outside the scope allowlist must fail at load")
	}

	looser := base
	looser.Lifecycle = &LifecycleConfig{Autonomy: Autonomy{ClosePolicy: closePolicyAutonomous}}
	if err := resolveScopes([]RepoConfig{looser}, scopeTestAppConfig(scopes)); err == nil {
		t.Fatal("a closePolicy looser than the scope must fail at load (narrow-only)")
	}
}

// #104: scope approval may only be turned ON, and §55 requires an audit sink —
// a scope that gates without a sink fails at load (never a silent gate).
func TestScopeApprovalNarrowOnlyAndRequiresAudit(t *testing.T) {
	yes := true
	scopes := ScopesConfig{"payments": {Repositories: []string{"acme/payments"}, Approval: &yes}}

	noAudit := RepoConfig{Owner: "acme", Repo: "payments", RepoPath: "/x", ProductLine: "payments"}
	if err := resolveScopes([]RepoConfig{noAudit}, scopeTestAppConfig(scopes)); err == nil {
		t.Fatal("a scope-enabled approval gate without an audit sink must fail at load")
	}

	withAudit := RepoConfig{Owner: "acme", Repo: "payments", RepoPath: "/x", ProductLine: "payments"}
	withAudit.Lifecycle = &LifecycleConfig{Audit: AuditConfig{Enabled: true, Sink: "/tmp/a.jsonl"}}
	repos := []RepoConfig{withAudit}
	if err := resolveScopes(repos, scopeTestAppConfig(scopes)); err != nil {
		t.Fatalf("a scoped approval gate with an audit sink must resolve: %v", err)
	}
	if !repos[0].Lifecycle.Approval.PreGate {
		t.Fatal("scope approval: true must force the pre-execution gate on")
	}
}

// #104 structural validation: an allowlist that widens its parent, a close
// policy looser than the global baseline, and department approval widening all
// fail at konoha.yaml load.
func TestScopesStructuralValidation(t *testing.T) {
	bad := []ScopesConfig{
		{"pl": {Repositories: []string{"o/r"}, ClosePolicy: closePolicyAutonomous}}, // widens global baseline
		{"pl": {Repositories: []string{"o/r"}, Models: []string{"cheap"},
			Departments: map[string]DepartmentScope{"d": {Repositories: []string{"o/r"}, Models: []string{"strong"}}}}}, // not a subset
		{"pl": {Repositories: []string{"o/r"}, Departments: map[string]DepartmentScope{"d": {Repositories: []string{"o/other"}}}}}, // repo not in parent
		{"pl": {Repositories: []string{}}}, // no repositories
	}
	for i, s := range bad {
		if err := s.validate(); err == nil {
			t.Fatalf("scopes config %d must fail structural validation", i)
		}
	}
	yes := true
	good := ScopesConfig{"pl": {
		Repositories: []string{"o/r"}, Models: []string{"cheap", "strong"}, ClosePolicy: closePolicyRiskBased,
		Approval: &yes,
		Departments: map[string]DepartmentScope{
			"d": {Repositories: []string{"o/r"}, Models: []string{"strong"}, ClosePolicy: closePolicyHumanRequired},
		},
	}}
	if err := good.validate(); err != nil {
		t.Fatalf("a valid scope must pass structural validation: %v", err)
	}
}

// #104 AC: the resource digest (#88) changes when the resolved scope changes.
func TestPolicyDigestScopeSensitive(t *testing.T) {
	task := taskHash("o", "r", 7)
	a := LifecycleConfig{Mode: modeSupervisedAutonomous}
	b := a
	b.Scope = "payments/checkout"
	if policyDigest(a, task) == policyDigest(b, task) {
		t.Fatal("a change of scope must change the policy digest")
	}
}

// #104 AC: the GH_* contract is unchanged — no per-tenant credential config is
// introduced. A credential-shaped key in a scope is rejected by strict decoding.
func TestScopesRejectCredentialConfig(t *testing.T) {
	p := writeAppConfig(t, "scopes:\n  payments:\n    repositories: [o/r]\n    githubAppId: 42\n")
	if _, err := loadAppConfig(p); err == nil {
		t.Fatal("a per-tenant credential field must be rejected (GH_* is a non-configurable contract §23)")
	}
}

// #104 AC: resolution happens exactly once at load and is deterministic, and the
// resolved scope travels in the schedule input (no workflow-side resolution).
func TestScopesResolveOnceAtLoadIntoInput(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "konoha.yaml"), []byte(`
defaults:
  agents:
    models:
      cheap: {provider: anthropic, model: claude-haiku}
scopes:
  payments:
    repositories: [acme/payments]
    models: [cheap]
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "repos.yaml"), []byte(`
repos:
  - repo: payments
    owner: acme
    repoPath: /x
    productLine: payments
    lifecycle:
      models:
        developer: cheap
`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KONOHA_CONFIG_DIR", dir)

	lines, err := scheduleInputs(filepath.Join(dir, "repos.yaml"))
	if err != nil {
		t.Fatalf("scheduleInputs: %v", err)
	}
	if len(lines) != 1 || !strings.Contains(lines[0], `"scope":"payments"`) {
		t.Fatalf("schedule input must carry the resolved scope: %v", lines)
	}

	// Repeated loads are deterministic (same resolved scope + digest inputs).
	repos, err := loadReposConfigWithResources(filepath.Join(dir, "repos.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if repos[0].Lifecycle.Scope != "payments" {
		t.Fatalf("resolved scope = %q, want payments", repos[0].Lifecycle.Scope)
	}

	// A scope reference outside the global catalog fails the same load path.
	badDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(badDir, "konoha.yaml"), []byte(`
scopes:
  payments:
    repositories: [acme/payments]
    models: [missing]
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(badDir, "repos.yaml"), []byte(`
repos:
  - repo: payments
    owner: acme
    repoPath: /x
    productLine: payments
`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KONOHA_CONFIG_DIR", badDir)
	if _, err := loadReposConfigWithResources(filepath.Join(badDir, "repos.yaml")); err == nil {
		t.Fatal("an unknown model reference in a scope must fail the load path")
	}
}

// #104 AC: the workflow records the resolved scope (carried in the input) and
// never resolves a scope name itself.
func TestWorkflowRecordsResolvedScope(t *testing.T) {
	resetMetrics()
	root := withEvidenceDir(t)
	env := mockCycleEnv(t, EstimateResult{Complexity: 3, Confidence: "high"})
	mockSmallPath(env)
	env.RegisterActivity(WriteCycleEvidence)

	lc := LifecycleConfig{Mode: modeSupervisedAutonomous, DevelopmentMaxComplexity: 4, Scope: "payments/checkout"}
	runCycleWith(t, env, lc)

	if got := terminalState(t, env); got != stateClosed {
		t.Fatalf("terminal = %q, want CLOSED", got)
	}
	out := readOutcome(t, root)
	if out.Scope != "payments/checkout" {
		t.Fatalf("outcome scope = %q, want the resolved payments/checkout", out.Scope)
	}
	if out.PolicyDigest != policyDigest(lc, taskHash("o", "r", testIssue)) {
		t.Fatalf("outcome digest must equal the digest of the resolved policy (no workflow re-resolution)")
	}
}
