package konoha

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeRepos(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "repos.yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadReposConfigFullSchema(t *testing.T) {
	p := writeRepos(t, `
repos:
  - repo: ducklambda
    owner: vitdo82
    repoPath: /work/ducklambda
    schedule: "*/5 * * * *"
    lifecycle:
      mode: supervised-autonomous
      developmentMaxComplexity: 4
      maxDevReviewAttempts: 2
      autonomy:
        default: autonomous
        closePolicy: risk-based
        humanRequiredPaths:
          - ".github/**"
          - "infra/**"
        humanRequiredLabels:
          - "security"
      merge:
        strategy: rebase
        requireCI: false
        ciMaxWait: 5m
      poGateTimeout: 168h
      dryRun: false
      maxConcurrentIssueCycles: 4
      maxConcurrentChildrenPerIssue: 3
`)
	repos, err := LoadReposConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 1 {
		t.Fatalf("got %d repos, want 1", len(repos))
	}
	lc := *repos[0].Lifecycle
	if lc.Mode != modeSupervisedAutonomous || lc.DryRun || lc.MaxConcurrentIssueCycles != 4 {
		t.Fatalf("unexpected lifecycle: %+v", lc)
	}
	if len(lc.Autonomy.HumanRequiredPaths) != 2 || lc.Autonomy.HumanRequiredPaths[0] != ".github/**" ||
		len(lc.Autonomy.HumanRequiredLabels) != 1 || lc.Autonomy.HumanRequiredLabels[0] != "security" {
		t.Fatalf("unexpected autonomy: %+v", lc.Autonomy)
	}

	// §49 merge block is parsed and resolvable.
	if MergeStrategy(lc) != mergeStrategyRebase || RequireCI(lc) || CIMaxWait(lc) != 5*time.Minute {
		t.Fatalf("unexpected merge policy: strategy=%q requireCI=%v maxWait=%v",
			MergeStrategy(lc), RequireCI(lc), CIMaxWait(lc))
	}

	// The schedule input serializes the same schema the workflows decode.
	lines, err := scheduleInputs(p)
	if err != nil || len(lines) != 1 {
		t.Fatalf("scheduleInputs: %v, %v", lines, err)
	}
	if !strings.Contains(lines[0], `"maxConcurrentIssueCycles":4`) ||
		!strings.Contains(lines[0], `"closePolicy":"risk-based"`) ||
		!strings.Contains(lines[0], `"strategy":"rebase"`) ||
		!strings.Contains(lines[0], `"requireCI":false`) {
		t.Fatalf("schedule input missing lifecycle fields: %s", lines[0])
	}
}

// §49: unset merge config resolves to the fail-closed defaults (squash, CI
// required) from the bundled application defaults.
func TestMergePolicyDefaults(t *testing.T) {
	saved := appCfg
	t.Cleanup(func() { appCfg = saved })
	appCfg = defaultAppConfig()

	var lc LifecycleConfig // everything unset
	if got := MergeStrategy(lc); got != mergeStrategySquash {
		t.Fatalf("default strategy = %q, want squash", got)
	}
	if !RequireCI(lc) {
		t.Fatal("CI must be required by default (fail-closed)")
	}
	if got := CIMaxWait(lc); got != 10*time.Minute {
		t.Fatalf("default ciMaxWait = %v, want 10m", got)
	}

	// An explicit per-repo opt-out wins over the default.
	off := false
	lc.Merge = MergeConfig{RequireCI: &off}
	if RequireCI(lc) {
		t.Fatal("explicit requireCI=false must disable the gate")
	}
}

func TestLoadReposConfigUnknownKey(t *testing.T) {
	p := writeRepos(t, `repos: [{repo: r, owner: o, repoPath: /x, lifecycle: {bogus: 1}}]`)
	if _, err := LoadReposConfig(p); err == nil {
		t.Fatal("expected unknown key to be rejected")
	}
}

func TestLoadReposConfigRejectsOutOfRange(t *testing.T) {
	bad := []string{
		`{mode: "nope"}`,
		`{developmentMaxComplexity: 11}`,
		`{developmentMaxComplexity: -1}`,
		`{maxDevReviewAttempts: -1}`,
		`{autonomy: {default: "nope"}}`,
		`{autonomy: {closePolicy: "nope"}}`,
		`{poGateTimeout: "notaduration"}`,
		`{maxConcurrentIssueCycles: -1}`,
		`{maxConcurrentChildrenPerIssue: -1}`,
		`{merge: {strategy: "fast-forward"}}`,
		`{merge: {ciMaxWait: "notaduration"}}`,
		`{merge: {ciMaxWait: "-1s"}}`,
	}
	for _, lc := range bad {
		p := writeRepos(t, `repos: [{repo: r, owner: o, repoPath: /x, lifecycle:`+lc+`}]`)
		if _, err := LoadReposConfig(p); err == nil {
			t.Fatalf("expected rejection for lifecycle %s", lc)
		}
	}
}

func TestLoadReposConfigRequiresIdentity(t *testing.T) {
	for _, body := range []string{
		`repos: [{repo: r, repoPath: /x}]`,  // no owner
		`repos: [{owner: o, repoPath: /x}]`, // no repo
		`repos: [{repo: r, owner: o}]`,      // no repoPath
	} {
		p := writeRepos(t, body)
		if _, err := LoadReposConfig(p); err == nil {
			t.Fatalf("expected rejection for %s", body)
		}
	}
}

func TestLifecycleDefaultsWhenOmitted(t *testing.T) {
	p := writeRepos(t, `repos: [{repo: r, owner: o, repoPath: /x}]`)
	repos, err := LoadReposConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 1 {
		t.Fatalf("got %d repos", len(repos))
	}
	if repos[0].Lifecycle != nil {
		t.Fatalf("expected nil lifecycle when omitted, got %+v", *repos[0].Lifecycle)
	}
	lines, err := scheduleInputs(p)
	if err != nil {
		t.Fatal(err)
	}
	// The emitted input decodes back to a zero lifecycle: dry-run off,
	// concurrency defaulted to serialized — i.e. pre-schema behavior.
	var scope RepoScope
	if err := json.Unmarshal([]byte(lines[0][strings.Index(lines[0], "\t")+1:]), &scope); err != nil {
		t.Fatal(err)
	}
	if scope.Repo != "r" || scope.RepoPath != "/x" || scope.Lifecycle.DryRun {
		t.Fatalf("unexpected decoded scope: %+v", scope)
	}
	if got := scope.Lifecycle.ConcurrentIssueCycles(); got != 1 {
		t.Fatalf("omitted lifecycle must default to serialized (1), got %d", got)
	}
}

func TestConcurrentIssueCyclesDefault(t *testing.T) {
	if got := (LifecycleConfig{}).ConcurrentIssueCycles(); got != 1 {
		t.Fatalf("zero cap must default to serialized (1), got %d", got)
	}
	if got := (LifecycleConfig{MaxConcurrentIssueCycles: 4}).ConcurrentIssueCycles(); got != 4 {
		t.Fatalf("got %d, want 4", got)
	}
}
