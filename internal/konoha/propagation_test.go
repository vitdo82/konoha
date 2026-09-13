package konoha

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/testsuite"
)

// propConfig returns an enabled §54 config with two downstreams.
func propConfig() PropagationConfig {
	return PropagationConfig{
		Enabled: true,
		Downstream: map[string]PropagationTarget{
			"repoB": {Trigger: "pkg/api/**", Type: "api"},
			"repoC": {Trigger: "go.mod", Type: "breaking"},
		},
	}
}

// §54/§33: repos.yaml parses the propagation block; a malformed one fails at
// load (fail-fast), and the schedule input carries the downstream map.
func TestLoadReposConfigPropagation(t *testing.T) {
	p := writeRepos(t, `
repos:
  - repo: up
    owner: o
    repoPath: /x
    lifecycle:
      propagation:
        enabled: true
        maxPerMerge: 2
        downstream:
          down:
            trigger: "pkg/api/**"
            type: breaking
`)
	repos, err := LoadReposConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	pc := repos[0].Lifecycle.Propagation
	if !PropagationEnabled(*repos[0].Lifecycle) || pc.maxPerMerge() != 2 ||
		pc.Downstream["down"].Trigger != "pkg/api/**" || pc.Downstream["down"].Type != "breaking" {
		t.Fatalf("unexpected propagation config: %+v", pc)
	}
	lines, err := scheduleInputs(p)
	if err != nil || len(lines) != 1 || !strings.Contains(lines[0], `"propagation":{"enabled":true`) {
		t.Fatalf("schedule input missing propagation: %v (%v)", lines, err)
	}

	bad := writeRepos(t, `
repos:
  - repo: up
    owner: o
    repoPath: /x
    lifecycle:
      propagation:
        enabled: true
`)
	if _, err := LoadReposConfig(bad); err == nil {
		t.Fatal("enabled propagation without downstream must fail at load")
	}
}

// §54/§22: konoha:cross-repo is a source label — created by EnsureRepoLabels
// but never projectable, so ApplyIssueLabels cannot strip it as stale.
func TestCrossRepoLabelNotProjectable(t *testing.T) {
	if !containsString(sourceLabels(), labelCrossRepo) {
		t.Fatalf("konoha:cross-repo must be a source label, got %v", sourceLabels())
	}
	if containsString(projectableLabels(), labelCrossRepo) {
		t.Fatal("konoha:cross-repo must NOT be projectable (would be stripped)")
	}
}

// §54: the map matcher fires exactly the configured downstreams whose trigger
// glob matches a changed file, in deterministic order.
func TestMatchAffectedDownstreams(t *testing.T) {
	got := MatchAffectedDownstreams(propConfig(), []string{"README.md", "pkg/api/server.go"}, 0)
	if len(got) != 1 || got[0].Repo != "repoB" || got[0].Type != "api" {
		t.Fatalf("matcher = %+v, want only repoB", got)
	}
	// a go.mod touch matches repoC
	got = MatchAffectedDownstreams(propConfig(), []string{"go.mod"}, 0)
	if len(got) != 1 || got[0].Repo != "repoC" || got[0].Type != "breaking" {
		t.Fatalf("matcher = %+v, want only repoC", got)
	}
	// unconfigured pairs never match
	if got = MatchAffectedDownstreams(propConfig(), []string{"docs/x.md"}, 0); len(got) != 0 {
		t.Fatalf("unconfigured touch matched %+v, want none", got)
	}
}

// §54/§43: the fan-out is bounded by maxPerMerge (unset → default).
func TestMatchAffectedDownstreamsCap(t *testing.T) {
	cfg := PropagationConfig{Enabled: true, Downstream: map[string]PropagationTarget{
		"a": {Trigger: "**", Type: "x"},
		"b": {Trigger: "**", Type: "x"},
		"c": {Trigger: "**", Type: "x"},
	}}
	if got := MatchAffectedDownstreams(cfg, []string{"f"}, 2); len(got) != 2 {
		t.Fatalf("cap 2 matched %d, want 2", len(got))
	}
	if got := MatchAffectedDownstreams(cfg, []string{"f"}, 0); len(got) != defaultPropagationMaxPerMerge {
		t.Fatalf("default cap matched %d, want %d", len(got), defaultPropagationMaxPerMerge)
	}
	// explicit config cap wins over the default
	cfg.MaxPerMerge = 1
	if got := MatchAffectedDownstreams(cfg, []string{"f"}, 0); len(got) != 1 {
		t.Fatalf("configured cap matched %d, want 1", len(got))
	}
}

// §54/§33: an enabled map needs downstreams; each entry needs trigger+type.
func TestPropagationConfigValidate(t *testing.T) {
	cases := []struct {
		name string
		cfg  PropagationConfig
		ok   bool
	}{
		{"disabled empty", PropagationConfig{}, true},
		{"enabled no downstream", PropagationConfig{Enabled: true}, false},
		{"missing trigger", PropagationConfig{Enabled: true, Downstream: map[string]PropagationTarget{"b": {Type: "api"}}}, false},
		{"missing type", PropagationConfig{Enabled: true, Downstream: map[string]PropagationTarget{"b": {Trigger: "x"}}}, false},
		{"negative cap", PropagationConfig{Enabled: true, MaxPerMerge: -1, Downstream: map[string]PropagationTarget{"b": {Trigger: "x", Type: "api"}}}, false},
		{"valid", propConfig(), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.validate("lifecycle.propagation")
			if (err == nil) != tc.ok {
				t.Fatalf("validate err=%v, want ok=%v", err, tc.ok)
			}
		})
	}
}

// propFakeGh simulates the list + create calls of EnsureCrossRepoIssue.
func propFakeGh(t *testing.T, existing []ghItem, created int) *[]string {
	t.Helper()
	prev := ghRunner
	var calls []string
	ghRunner = func(_ context.Context, args ...string) ([]byte, error) {
		line := strings.Join(args, " ")
		calls = append(calls, line)
		switch {
		case strings.Contains(line, "issues?state=all"):
			return json.Marshal(existing)
		case strings.HasPrefix(line, "issue create"):
			return []byte(fmt.Sprintf("https://github.com/o/r/issues/%d", created)), nil
		default:
			return nil, fmt.Errorf("unexpected gh call: %s", line)
		}
	}
	t.Cleanup(func() { ghRunner = prev })
	return &calls
}

// §54/§24: a second run finds the marker and never creates a duplicate; the
// create carries the required konoha:cross-repo label and no konoha:managed.
func TestEnsureCrossRepoIssueIdempotent(t *testing.T) {
	req := CrossRepoIssueRequest{
		UpstreamOwner: "o", UpstreamRepo: "r", PrNum: 101, Sha: "abc123",
		Repo: "repoB", Type: "api", Trigger: "pkg/api/**",
	}
	calls := propFakeGh(t, nil, 42)

	first, err := EnsureCrossRepoIssue(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Created || first.Number != 42 {
		t.Fatalf("first run = %+v, want created #42", first)
	}
	created := strings.Join(createdIssueCalls(*calls), " ")
	if !strings.Contains(created, labelCrossRepo) {
		t.Fatalf("cross-repo issue must carry %s: %s", labelCrossRepo, created)
	}
	if strings.Contains(created, labelManaged) {
		t.Fatalf("cross-repo issue must NOT carry %s (downstream sweep drives it): %s", labelManaged, created)
	}

	// existing issue carrying the marker → matched, no create
	*calls = nil
	existing := []ghItem{{Number: 42, Body: "intro\n" + crossMarker("o", "r", "abc123") + "\nrest", State: "open"}}
	prev := ghRunner
	ghRunner = func(_ context.Context, args ...string) ([]byte, error) {
		line := strings.Join(args, " ")
		*calls = append(*calls, line)
		if strings.Contains(line, "issues?state=all") {
			return json.Marshal(existing)
		}
		return nil, fmt.Errorf("must not create: %s", line)
	}
	t.Cleanup(func() { ghRunner = prev })

	second, err := EnsureCrossRepoIssue(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if second.Created || second.Number != 42 {
		t.Fatalf("second run = %+v, want matched #42 (no create)", second)
	}
	// a different sha is a different change → distinct marker
	if crossMarker("o", "r", "abc123") == crossMarker("o", "r", "def456") {
		t.Fatal("marker must differ per sha")
	}
}

// §54/§40: a missing sha cannot be deduped, so it fails closed.
func TestEnsureCrossRepoIssueRequiresSha(t *testing.T) {
	if _, err := EnsureCrossRepoIssue(context.Background(), CrossRepoIssueRequest{Repo: "b"}); err == nil {
		t.Fatal("missing sha must error (no un-dedupable create)")
	}
}

// §54/§42: dry-run files nothing and never calls gh.
func TestEnsureCrossRepoIssueDryRunNoop(t *testing.T) {
	prev := ghRunner
	ghRunner = func(context.Context, ...string) ([]byte, error) {
		t.Fatal("dry-run must not call gh")
		return nil, nil
	}
	t.Cleanup(func() { ghRunner = prev })

	res, err := EnsureCrossRepoIssue(context.Background(), CrossRepoIssueRequest{
		UpstreamOwner: "o", UpstreamRepo: "r", Sha: "abc", Repo: "b", DryRun: true,
	})
	if err != nil || res.Number != 0 || res.Created {
		t.Fatalf("dry-run = %+v err=%v, want no-op", res, err)
	}
}

// §54: detection matches the merged PR's changed files via gh; disabled config
// or a missing PR reads nothing.
func TestDetectAffectedDownstreams(t *testing.T) {
	var calls []string
	prev := ghRunner
	ghRunner = func(_ context.Context, args ...string) ([]byte, error) {
		line := strings.Join(args, " ")
		calls = append(calls, line)
		if strings.Contains(line, "pr diff 101") && strings.Contains(line, "--name-only") {
			return []byte("pkg/api/server.go\nREADME.md\n"), nil
		}
		return nil, fmt.Errorf("unexpected gh call: %s", line)
	}
	t.Cleanup(func() { ghRunner = prev })

	got, err := DetectAffectedDownstreams(context.Background(), PropagationDetectRequest{
		Owner: "o", Repo: "r", PrNum: 101, Config: propConfig(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Repo != "repoB" {
		t.Fatalf("detect = %+v, want repoB only", got)
	}

	// disabled config → no gh read
	calls = nil
	if got, err := DetectAffectedDownstreams(context.Background(), PropagationDetectRequest{
		Owner: "o", Repo: "r", PrNum: 101, Config: PropagationConfig{},
	}); err != nil || len(got) != 0 {
		t.Fatalf("disabled detect = %+v err=%v", got, err)
	}
	// no PR (dry-run shadow) → no gh read
	if got, err := DetectAffectedDownstreams(context.Background(), PropagationDetectRequest{
		Owner: "o", Repo: "r", PrNum: 0, Config: propConfig(),
	}); err != nil || len(got) != 0 {
		t.Fatalf("missing-PR detect = %+v err=%v", got, err)
	}
	if len(calls) != 0 {
		t.Fatalf("disabled/missing-PR must not read gh, got calls %v", calls)
	}
}

// §54: an enabled PostMergeWorkflow files one cross-repo issue per affected
// downstream through the idempotent activity.
func TestPostMergePropagationFilesCrossRepoIssues(t *testing.T) {
	var filed []CrossRepoIssueRequest
	env := (&testsuite.WorkflowTestSuite{}).NewTestWorkflowEnvironment()
	env.OnActivity(DetectAffectedDownstreams, mock.Anything, mock.Anything).Return([]AffectedDownstream{
		{Repo: "repoB", Type: "api", Trigger: "pkg/api/**"},
	}, nil)
	env.OnActivity(EnsureRepoLabels, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(EnsureCrossRepoIssue, mock.Anything, mock.Anything).Return(
		func(_ context.Context, in CrossRepoIssueRequest) (CrossRepoIssueResult, error) {
			filed = append(filed, in)
			return CrossRepoIssueResult{Number: 42, Created: true}, nil
		})

	in := PostMergeInput{Owner: "o", Repo: "r", PrNum: 101, Sha: "abc",
		Lifecycle: LifecycleConfig{Propagation: propConfig()}}
	env.ExecuteWorkflow(PostMergeWorkflow, in)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	if len(filed) != 1 || filed[0].Repo != "repoB" || filed[0].Sha != "abc" || filed[0].UpstreamRepo != "r" {
		t.Fatalf("filed = %+v, want one issue for repoB bound to sha abc", filed)
	}
	if filed[0].DryRun {
		t.Fatal("non-dry-run must file for real")
	}
}

// §54/§42: a disabled config never detects or files; DryRun detects (read-only)
// but passes the shadow flag to every mutation.
func TestPostMergePropagationDisabledAndDryRun(t *testing.T) {
	// disabled → no detection at all
	detects := 0
	env := (&testsuite.WorkflowTestSuite{}).NewTestWorkflowEnvironment()
	env.OnActivity(DetectAffectedDownstreams, mock.Anything, mock.Anything).Return(
		func(context.Context, PropagationDetectRequest) ([]AffectedDownstream, error) {
			detects++
			return nil, nil
		})
	env.ExecuteWorkflow(PostMergeWorkflow, PostMergeInput{Owner: "o", Repo: "r", PrNum: 1})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	if detects != 0 {
		t.Fatalf("disabled propagation detected %d times, want 0", detects)
	}

	// enabled + dryRun → detection still runs, every mutation receives DryRun=true
	var labelDry, issueDry *bool
	dryEnv := (&testsuite.WorkflowTestSuite{}).NewTestWorkflowEnvironment()
	dryEnv.OnActivity(DetectAffectedDownstreams, mock.Anything, mock.Anything).Return([]AffectedDownstream{
		{Repo: "b", Type: "x", Trigger: "**"},
	}, nil)
	dryEnv.OnActivity(EnsureRepoLabels, mock.Anything, mock.Anything).Return(
		func(_ context.Context, in LabelsTheirsRequest) error {
			b := in.DryRun
			labelDry = &b
			return nil
		})
	dryEnv.OnActivity(EnsureCrossRepoIssue, mock.Anything, mock.Anything).Return(
		func(_ context.Context, in CrossRepoIssueRequest) (CrossRepoIssueResult, error) {
			b := in.DryRun
			issueDry = &b
			return CrossRepoIssueResult{}, nil
		})
	dryEnv.ExecuteWorkflow(PostMergeWorkflow, PostMergeInput{Owner: "o", Repo: "r", PrNum: 1, Sha: "s",
		Lifecycle: LifecycleConfig{DryRun: true, Propagation: propConfig()}})
	if err := dryEnv.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	if labelDry == nil || !*labelDry || issueDry == nil || !*issueDry {
		t.Fatalf("dry-run must pass DryRun=true to mutations (label=%v issue=%v)", labelDry, issueDry)
	}
}
