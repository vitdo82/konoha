package konoha

import (
	"fmt"
	"sort"
	"strings"
)

// Policy tenancy (#104): named scopes that can only NARROW the operator's
// global policy. The tenancy shape is Paddock's Facility > Product Line >
// Department: the Facility is the whole `scopes:` block (an optional label, see
// AppConfig.Facility), a Product Line groups repositories for one product, and
// an optional Department is a functional area within it.
//
// A scope lists the `owner/repo` entries it covers and allowlists for the
// harness/model/budget catalog names those repos may use. It NEVER widens:
//   - a reference outside the global allowlist (a repo not in repos.yaml, an
//     unknown harness, or an unknown §98 model/budget name) fails at load;
//   - closePolicy may only become MORE human-gated (risk-based or stricter);
//   - approval may only be turned ON, never off.
//
// This is POLICY tenancy, NOT execution isolation: scopes share the host
// filesystem/process space and there is NO per-tenant GitHub App credential —
// the single-App GH_* contract (§23) is unchanged. See docs/non-goals.md B3.

// scopeCloseFloor is the global baseline a scope may only narrow. The bundled
// default close policy is risk-based (policy.go EffectiveClosePolicy), so a
// scope may select risk-based or human-required — never the less-gated
// autonomous (#104 narrow-only).
const scopeCloseFloor = closePolicyRiskBased

// ScopesConfig is the top-level `scopes:` map (konoha.yaml): product-line name →
// scope. The optional sibling top-level `facility:` key (AppConfig.Facility)
// names the authenticated boundary; it is recorded/evidence only (Konoha is
// single-operator, so there is no per-facility auth here).
type ScopesConfig map[string]ScopeConfig

// ScopeConfig is one Product Line scope. Repositories is the set of `owner/repo`
// entries the product line covers (the default repo→scope mapping), and the
// resource lists are narrow-only allowlists over the global catalogs.
// Departments are optional functional areas that narrow the product line.
type ScopeConfig struct {
	Repositories []string                   `yaml:"repositories"`
	Harnesses    []string                   `yaml:"harnesses"`
	Models       []string                   `yaml:"models"`
	Budgets      []string                   `yaml:"budgets"`
	Approval     *bool                      `yaml:"approval"`
	ClosePolicy  string                     `yaml:"closePolicy"`
	Departments  map[string]DepartmentScope `yaml:"departments"`
}

// DepartmentScope is one functional area within a product line. It may only
// narrow: its lists must be subsets of the product line's, its closePolicy at
// least as strict, and its repositories within the product line's. It cannot
// nest further.
type DepartmentScope struct {
	Repositories []string `yaml:"repositories"`
	Harnesses    []string `yaml:"harnesses"`
	Models       []string `yaml:"models"`
	Budgets      []string `yaml:"budgets"`
	Approval     *bool    `yaml:"approval"`
	ClosePolicy  string   `yaml:"closePolicy"`
}

// scopeCloseStrictness orders the §19 close policies from least to most
// human-gated, so "may only narrow" is a numeric comparison. -1 = unset/invalid.
func scopeCloseStrictness(p string) int {
	switch p {
	case closePolicyAutonomous:
		return 0
	case closePolicyRiskBased:
		return 1
	case closePolicyHumanRequired:
		return 2
	default:
		return -1
	}
}

// stricterClose returns whichever close policy is more human-gated (an unset
// value is the least strict).
func stricterClose(a, b string) string {
	if scopeCloseStrictness(b) > scopeCloseStrictness(a) {
		return b
	}
	return a
}

// validClosePolicy enumerates the §19 close policies for error messages.
func validClosePolicy(p string) bool { return scopeCloseStrictness(p) >= 0 }

// validateScopeClose checks a scope/department closePolicy is a known value at
// least as strict as floor (the baseline it narrows).
func validateScopeClose(prefix, val string, floor int) error {
	if strings.TrimSpace(val) == "" {
		return nil
	}
	st := scopeCloseStrictness(val)
	if st < 0 {
		return fmt.Errorf("%s: %q not one of %q, %q, %q", prefix, val, closePolicyAutonomous, closePolicyRiskBased, closePolicyHumanRequired)
	}
	if st < floor {
		return fmt.Errorf("%s: %q widens the global baseline (a scope may only narrow: risk-based or stricter)", prefix, val)
	}
	return nil
}

// validateStringList rejects empty or duplicate entries so a typo'd allowlist
// fails closed rather than silently matching nothing.
func validateStringList(prefix string, xs []string) error {
	seen := map[string]bool{}
	for _, x := range xs {
		if strings.TrimSpace(x) == "" {
			return fmt.Errorf("%s: entries must be non-empty", prefix)
		}
		if seen[x] {
			return fmt.Errorf("%s: duplicate entry %q", prefix, x)
		}
		seen[x] = true
	}
	return nil
}

// sortedScopeNames returns the sorted product-line keys (stable error messages
// + deterministic auto-matching).
func sortedScopeNames(s ScopesConfig) []string {
	out := make([]string, 0, len(s))
	for k := range s {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// sortedDepartmentNames returns the sorted department keys of one product line.
func sortedDepartmentNames(d map[string]DepartmentScope) []string {
	out := make([]string, 0, len(d))
	for k := range d {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// validate structurally gates the `scopes:` block: non-empty product-line names
// with at least one repository, known close-policy values no looser than the
// global baseline, and department narrowing (subset lists / stricter close
// policy / no widening approval). References against the global repo/harness/
// model/budget allowlists are checked by resolveScopes, which has repos.yaml and
// the catalogs.
func (s ScopesConfig) validate() error {
	for _, name := range sortedScopeNames(s) {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("scopes: empty product line name")
		}
		sc := s[name]
		if len(sc.Repositories) == 0 {
			return fmt.Errorf("scopes.%s.repositories: at least one repository is required", name)
		}
		for _, l := range []struct {
			field string
			vals  []string
		}{
			{"repositories", sc.Repositories},
			{"harnesses", sc.Harnesses},
			{"models", sc.Models},
			{"budgets", sc.Budgets},
		} {
			if err := validateStringList("scopes."+name+"."+l.field, l.vals); err != nil {
				return err
			}
		}
		if err := validateScopeClose("scopes."+name+".closePolicy", sc.ClosePolicy, scopeCloseStrictness(scopeCloseFloor)); err != nil {
			return err
		}
		parentStrict := scopeCloseStrictness(sc.ClosePolicy)
		if parentStrict < 0 {
			parentStrict = scopeCloseStrictness(scopeCloseFloor)
		}
		for _, dn := range sortedDepartmentNames(sc.Departments) {
			if err := sc.Departments[dn].validateDepartment("scopes."+name+".departments."+dn, sc, parentStrict); err != nil {
				return err
			}
		}
	}
	return nil
}

// validateDepartment gates one department against its product line: it must
// cover ≥1 repository, and every list/closePolicy/approval may only narrow.
func (d DepartmentScope) validateDepartment(prefix string, parent ScopeConfig, parentStrict int) error {
	if len(d.Repositories) == 0 {
		return fmt.Errorf("%s.repositories: at least one repository is required", prefix)
	}
	if err := requireSubset(prefix+".repositories", d.Repositories, parent.Repositories); err != nil {
		return err
	}
	if err := requireSubsetIfParent(prefix+".harnesses", d.Harnesses, parent.Harnesses); err != nil {
		return err
	}
	if err := requireSubsetIfParent(prefix+".models", d.Models, parent.Models); err != nil {
		return err
	}
	if err := requireSubsetIfParent(prefix+".budgets", d.Budgets, parent.Budgets); err != nil {
		return err
	}
	for _, l := range []struct {
		field string
		vals  []string
	}{
		{"repositories", d.Repositories},
		{"harnesses", d.Harnesses},
		{"models", d.Models},
		{"budgets", d.Budgets},
	} {
		if err := validateStringList(prefix+"."+l.field, l.vals); err != nil {
			return err
		}
	}
	if err := validateScopeClose(prefix+".closePolicy", d.ClosePolicy, parentStrict); err != nil {
		return err
	}
	if parent.Approval != nil && *parent.Approval && d.Approval != nil && !*d.Approval {
		return fmt.Errorf("%s.approval: a department cannot widen an approval gate (product line sets approval: true)", prefix)
	}
	return nil
}

// requireSubset rejects child entries not present in the (non-empty) parent
// list. An empty parent list means "unrestricted", so any child entry is a
// narrowing.
func requireSubsetIfParent(prefix string, child, parent []string) error {
	if len(parent) == 0 {
		return nil
	}
	return requireSubset(prefix, child, parent)
}

func requireSubset(prefix string, child, parent []string) error {
	for _, c := range child {
		if !containsString(parent, c) {
			return fmt.Errorf("%s: %q is not in the parent product line (a department may only narrow)", prefix, c)
		}
	}
	return nil
}

// scopeAllowlist is the global allowlist a scope may only narrow: the repos
// declared in repos.yaml, the builtin harness names, and the §98 catalog names.
type scopeAllowlist struct {
	repos     map[string]bool
	harnesses map[string]bool
	models    map[string]bool
	budgets   map[string]bool
	// sorted names for stable, helpful error messages.
	repoNames, harnessNames, modelNames, budgetNames []string
}

// buildScopeAllowlist derives the global allowlist from the loaded repos + the
// resolved app config. Deterministic (names sorted).
func buildScopeAllowlist(repos []RepoConfig, cfg AppConfig) scopeAllowlist {
	a := scopeAllowlist{
		repos: map[string]bool{}, harnesses: map[string]bool{},
		models: map[string]bool{}, budgets: map[string]bool{},
	}
	for _, r := range repos {
		key := r.Owner + "/" + r.Repo
		if !a.repos[key] {
			a.repos[key] = true
			a.repoNames = append(a.repoNames, key)
		}
	}
	for _, h := range builtinHarnessNames {
		a.harnesses[h] = true
		a.harnessNames = append(a.harnessNames, h)
	}
	for _, m := range sortedModelNames(cfg.Defaults.Agents.Models) {
		a.models[m] = true
		a.modelNames = append(a.modelNames, m)
	}
	for _, b := range sortedBudgetNames(cfg.Defaults.Agents.Budgets) {
		a.budgets[b] = true
		a.budgetNames = append(a.budgetNames, b)
	}
	sort.Strings(a.repoNames)
	sort.Strings(a.harnessNames)
	return a
}

// validateScopeRefs fails closed on any scope reference outside the global
// allowlist. Because the refs are checked here, a scope can only ever choose a
// SUBSET of what the operator already granted (narrow-only).
func validateScopeRefs(prefix string, repositories, harnesses, models, budgets []string, allow scopeAllowlist) error {
	for _, r := range repositories {
		if !allow.repos[r] {
			return fmt.Errorf("%s.repositories: %q is not a repo in repos.yaml (a scope may only narrow; unknown refs fail closed)", prefix, r)
		}
	}
	for _, h := range harnesses {
		if !allow.harnesses[h] {
			return fmt.Errorf("%s.harnesses: unknown harness %q (known: %s)", prefix, h, strings.Join(allow.harnessNames, ", "))
		}
	}
	for _, m := range models {
		if !allow.models[m] {
			return fmt.Errorf("%s.models: unknown model %q (known: %s)", prefix, m, strings.Join(allow.modelNames, ", "))
		}
	}
	for _, b := range budgets {
		if !allow.budgets[b] {
			return fmt.Errorf("%s.budgets: unknown budget %q (known: %s)", prefix, b, strings.Join(allow.budgetNames, ", "))
		}
	}
	return nil
}

// effectiveScope is a repo's resolved scope: the product line, narrowed by an
// optional department, flattened to the effective allowlists.
type effectiveScope struct {
	name         string
	repositories []string
	harnesses    []string
	models       []string
	budgets      []string
	approval     bool
	closePolicy  string
}

// mergeScope flattens a product line + optional department into one effective
// scope, applying the already-validated narrowing (department overrides only
// where it is more specific).
func mergeScope(name string, pl ScopeConfig, d DepartmentScope) *effectiveScope {
	e := &effectiveScope{
		name:         name,
		repositories: pl.Repositories,
		harnesses:    pl.Harnesses,
		models:       pl.Models,
		budgets:      pl.Budgets,
		closePolicy:  pl.ClosePolicy,
	}
	if pl.Approval != nil {
		e.approval = *pl.Approval
	}
	if d.Repositories != nil {
		e.repositories = d.Repositories
	}
	if d.Harnesses != nil {
		e.harnesses = d.Harnesses
	}
	if d.Models != nil {
		e.models = d.Models
	}
	if d.Budgets != nil {
		e.budgets = d.Budgets
	}
	if d.Approval != nil && *d.Approval {
		e.approval = true // approval may only be turned on
	}
	e.closePolicy = stricterClose(pl.ClosePolicy, d.ClosePolicy)
	return e
}

// resolveRepoScope maps one repos.yaml entry to its effective scope:
//   - an explicit productLine (+ optional department) must name a real scope;
//   - otherwise the repo is matched by membership in the scopes' repositories
//     (the default product-line mapping), preferring the most specific single
//     match and failing closed on ambiguity.
//
// It returns (nil, nil) when the repo belongs to no scope (global baseline
// unchanged, back-compatible).
func resolveRepoScope(r RepoConfig, cfg AppConfig) (*effectiveScope, error) {
	key := r.Owner + "/" + r.Repo
	if r.ProductLine != "" {
		pl, ok := cfg.Scopes[r.ProductLine]
		if !ok {
			return nil, fmt.Errorf("%s: unknown productLine %q (known: %s)", r.Repo, r.ProductLine, strings.Join(sortedScopeNames(cfg.Scopes), ", "))
		}
		if r.Department != "" {
			d, ok := pl.Departments[r.Department]
			if !ok {
				return nil, fmt.Errorf("%s: unknown department %q in product line %q (known: %s)", r.Repo, r.Department, r.ProductLine, strings.Join(sortedDepartmentNames(pl.Departments), ", "))
			}
			return mergeScope(r.ProductLine+"/"+r.Department, pl, d), nil
		}
		return mergeScope(r.ProductLine, pl, DepartmentScope{}), nil
	}
	if r.Department != "" {
		return nil, fmt.Errorf("%s: department %q requires productLine", r.Repo, r.Department)
	}

	// Default mapping: match by repository membership. Prefer a department (most
	// specific); ambiguous matches at the same level fail closed.
	var deptMatches, plMatches []*effectiveScope
	for _, name := range sortedScopeNames(cfg.Scopes) {
		pl := cfg.Scopes[name]
		for _, dn := range sortedDepartmentNames(pl.Departments) {
			d := pl.Departments[dn]
			if containsString(d.Repositories, key) {
				deptMatches = append(deptMatches, mergeScope(name+"/"+dn, pl, d))
			}
		}
		if containsString(pl.Repositories, key) {
			plMatches = append(plMatches, mergeScope(name, pl, DepartmentScope{}))
		}
	}
	switch {
	case len(deptMatches) > 1:
		return nil, fmt.Errorf("%s: matched multiple department scopes (%s); assign an explicit productLine/department", r.Repo, scopeNames(deptMatches))
	case len(deptMatches) == 1:
		return deptMatches[0], nil
	case len(plMatches) > 1:
		return nil, fmt.Errorf("%s: matched multiple product-line scopes (%s); assign an explicit productLine", r.Repo, scopeNames(plMatches))
	case len(plMatches) == 1:
		return plMatches[0], nil
	default:
		return nil, nil
	}
}

func scopeNames(scopes []*effectiveScope) string {
	out := make([]string, 0, len(scopes))
	for _, s := range scopes {
		out = append(out, s.name)
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}

// scopeEffectiveHarness resolves the harness a repo would use, falling back to
// the resolved global default, without reading the process-fixed appCfg (pure
// given cfg, so load-time resolution is deterministic).
func scopeEffectiveHarness(cfg AppConfig, lc *LifecycleConfig) string {
	h := ""
	if lc != nil {
		h = strings.TrimSpace(lc.Harness)
	}
	if h == "" {
		h = strings.TrimSpace(cfg.Defaults.Lifecycle.Harness)
	}
	if h == "" {
		h = defaultHarnessName
	}
	return h
}

// applyScopeToRepo narrows one repo's effective policy to its resolved scope and
// records the scope name on the lifecycle. It fails closed on any widening
// attempt (a harness/model/budget/closePolicy the scope does not allow) and on
// an approval gate that would be enabled without the §55 audit sink.
func applyScopeToRepo(r *RepoConfig, cfg AppConfig, eff *effectiveScope) error {
	key := r.Owner + "/" + r.Repo
	if !containsString(eff.repositories, key) {
		return fmt.Errorf("%s: repository %q is not covered by scope %q's repositories (a scope may only narrow)", r.Repo, key, eff.name)
	}
	if r.Lifecycle == nil {
		r.Lifecycle = &LifecycleConfig{}
	}
	lc := r.Lifecycle

	if len(eff.harnesses) > 0 {
		h := scopeEffectiveHarness(cfg, lc)
		if !containsString(eff.harnesses, h) {
			return fmt.Errorf("%s: harness %q is not allowed by scope %q (allowed: %s)", r.Repo, h, eff.name, strings.Join(eff.harnesses, ", "))
		}
	}
	if len(eff.models) > 0 {
		for _, stage := range pipelineStages {
			name := strings.TrimSpace(lc.Models[stage])
			if name == "" {
				name = strings.TrimSpace(lc.Model) // legacy raw id: still must be in the allowlist
			}
			if name == "" {
				continue
			}
			if !containsString(eff.models, name) {
				return fmt.Errorf("%s: model %q (stage %s) is not allowed by scope %q (allowed: %s)", r.Repo, name, stage, eff.name, strings.Join(eff.models, ", "))
			}
		}
	}
	if len(eff.budgets) > 0 {
		if b := strings.TrimSpace(lc.Budget); b != "" && !containsString(eff.budgets, b) {
			return fmt.Errorf("%s: budget %q is not allowed by scope %q (allowed: %s)", r.Repo, b, eff.name, strings.Join(eff.budgets, ", "))
		}
	}

	if eff.closePolicy != "" {
		repoClose := lc.Autonomy.ClosePolicy
		if repoClose != "" && scopeCloseStrictness(repoClose) < scopeCloseStrictness(eff.closePolicy) {
			return fmt.Errorf("%s: closePolicy %q is looser than scope %q's %q (a scope may only narrow)", r.Repo, repoClose, eff.name, eff.closePolicy)
		}
		lc.Autonomy.ClosePolicy = stricterClose(repoClose, eff.closePolicy)
	}

	if eff.approval {
		lc.Approval.PreGate = true
		if !lc.Audit.Enabled {
			return fmt.Errorf("%s: scope %q enables the approval gate but lifecycle.audit.enabled is false (every gated decision must be recorded, §55)", r.Repo, eff.name)
		}
	}

	lc.Scope = eff.name
	return nil
}

// resolveScopes resolves the #104 tenancy scopes ONCE at load: it validates every
// scope reference against the global allowlist, maps each repo to its scope
// (explicit productLine/department, else by repository membership), and narrows
// the repo's effective lifecycle. It mutates repos in place and is pure given
// (repos, cfg). No workflow ever resolves a scope name — the resolved Scope +
// effective policy travel in the schedule input.
func resolveScopes(repos []RepoConfig, cfg AppConfig) error {
	allow := buildScopeAllowlist(repos, cfg)
	for _, name := range sortedScopeNames(cfg.Scopes) {
		sc := cfg.Scopes[name]
		if err := validateScopeRefs("scopes."+name, sc.Repositories, sc.Harnesses, sc.Models, sc.Budgets, allow); err != nil {
			return err
		}
		for _, dn := range sortedDepartmentNames(sc.Departments) {
			d := sc.Departments[dn]
			if err := validateScopeRefs("scopes."+name+".departments."+dn, d.Repositories, d.Harnesses, d.Models, d.Budgets, allow); err != nil {
				return err
			}
		}
	}
	for i := range repos {
		eff, err := resolveRepoScope(repos[i], cfg)
		if err != nil {
			return err
		}
		if eff == nil {
			continue
		}
		if err := applyScopeToRepo(&repos[i], cfg, eff); err != nil {
			return err
		}
	}
	return nil
}
