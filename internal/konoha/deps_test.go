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

// §52/§36: the scan parser is fail-closed — malformed JSON or an entry missing
// package/version|id is an error, never a silent empty report.
func TestParseDependencyReport(t *testing.T) {
	rep, err := parseDependencyReport([]byte(`[
		{"package":"golang.org/x/text","version":"v0.3.0","id":"GO-2021-0113","severity":"HIGH","summary":"s"},
		{"package":"github.com/foo/bar","version":"v1.2.3"}
	]`))
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Findings) != 2 {
		t.Fatalf("got %d findings, want 2", len(rep.Findings))
	}
	if !rep.Findings[0].Vulnerability() || rep.Findings[0].Severity != "high" {
		t.Fatalf("vuln finding not normalized: %+v", rep.Findings[0])
	}
	if rep.Findings[1].Vulnerability() {
		t.Fatalf("finding without id must be an outdated bump: %+v", rep.Findings[1])
	}

	// object wrapper is accepted
	if rep, err = parseDependencyReport([]byte(`{"findings":[{"package":"p","version":"1"}]}`)); err != nil || len(rep.Findings) != 1 {
		t.Fatalf("object wrapper: %+v err=%v", rep, err)
	}

	// empty output == no findings (not an error)
	if rep, err = parseDependencyReport(nil); err != nil || len(rep.Findings) != 0 {
		t.Fatalf("empty output: %+v err=%v", rep, err)
	}

	for name, body := range map[string]string{
		"not json":     `{oops`,
		"missing pkg":  `[{"version":"1.0.0"}]`,
		"missing both": `[{"package":"p"}]`,
	} {
		if _, err := parseDependencyReport([]byte(body)); err == nil {
			t.Fatalf("%s must fail closed", name)
		}
	}
}

// §52: the marker key is deterministic per finding kind.
func TestDependencyFindingIdentity(t *testing.T) {
	vuln := DependencyFinding{Package: "p", Version: "1.0.0", ID: "CVE-1"}
	if got := vuln.Identity(); got != "p@CVE-1" {
		t.Fatalf("vuln identity = %q, want p@CVE-1", got)
	}
	bump := DependencyFinding{Package: "p", Version: "2.0.0"}
	if got := bump.Identity(); got != "p@2.0.0" {
		t.Fatalf("bump identity = %q, want p@2.0.0", got)
	}
}

// §52/§40: a missing command or a failing command is an error, never "0 findings".
func TestScanDependenciesFailClosed(t *testing.T) {
	if _, err := ScanDependencies(context.Background(), DependencyScanRequest{ScanCmd: "  "}); err == nil {
		t.Fatal("empty scanCmd must error")
	}
	_, err := ScanDependencies(context.Background(), DependencyScanRequest{
		RepoPath: t.TempDir(), ScanCmd: "exit 3",
	})
	if err == nil {
		t.Fatal("failing scanCmd must error")
	}
}

// §52: a valid command prints the finding array and it parses.
func TestScanDependenciesParsesCommandOutput(t *testing.T) {
	rep, err := ScanDependencies(context.Background(), DependencyScanRequest{
		RepoPath: t.TempDir(),
		ScanCmd:  `echo '[{"package":"p","version":"1.0.0","id":"CVE-1","severity":"critical"}]'`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Findings) != 1 || rep.Findings[0].Severity != "critical" {
		t.Fatalf("unexpected report: %+v", rep)
	}
}

// depFakeGh simulates the issues-list + issue-create calls of EnsureDependencyIssue.
func depFakeGh(t *testing.T, existing []ghItem, created int) *[]string {
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

func createdIssueCalls(calls []string) []string {
	var out []string
	for _, c := range calls {
		if strings.HasPrefix(c, "issue create") {
			out = append(out, c)
		}
	}
	return out
}

// §52/§24: a second run finds the marker and never creates a duplicate issue.
func TestEnsureDependencyIssueIdempotent(t *testing.T) {
	f := DependencyFinding{Package: "p", Version: "1.0.0", ID: "CVE-1", Severity: "high"}
	calls := depFakeGh(t, nil, 42)

	first, err := EnsureDependencyIssue(context.Background(), DepIssueRequest{
		Owner: "o", Repo: "r", Finding: f, BaseLabel: labelDependency,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !first.Created || first.Number != 42 {
		t.Fatalf("first run = %+v, want created #42", first)
	}
	if n := len(createdIssueCalls(*calls)); n != 1 {
		t.Fatalf("first run must create exactly one issue, got %d", n)
	}

	// existing issue carrying the marker → matched, no create
	*calls = nil
	existing := []ghItem{{Number: 42, Body: "intro\n" + depMarker(f) + "\nrest", State: "open"}}
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

	second, err := EnsureDependencyIssue(context.Background(), DepIssueRequest{
		Owner: "o", Repo: "r", Finding: f, BaseLabel: labelDependency,
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.Created || second.Number != 42 {
		t.Fatalf("second run = %+v, want matched #42 (no create)", second)
	}
}

// §52/§20: a vulnerability issue gets the built-in konoha:security label; an
// outdated bump gets only the base label.
func TestEnsureDependencyIssueLabels(t *testing.T) {
	cases := []struct {
		name     string
		finding  DependencyFinding
		security bool
	}{
		{"vulnerability", DependencyFinding{Package: "p", Version: "1", ID: "CVE-1"}, true},
		{"outdated", DependencyFinding{Package: "p", Version: "2"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := depFakeGh(t, nil, 7)
			if _, err := EnsureDependencyIssue(context.Background(), DepIssueRequest{
				Owner: "o", Repo: "r", Finding: tc.finding, BaseLabel: labelDependency,
			}); err != nil {
				t.Fatal(err)
			}
			created := strings.Join(createdIssueCalls(*calls), " ")
			if !strings.Contains(created, "konoha:dependency") {
				t.Fatalf("missing base label: %s", created)
			}
			if got := strings.Contains(created, labelSecurity); got != tc.security {
				t.Fatalf("konoha:security present=%v, want %v (%s)", got, tc.security, created)
			}
		})
	}
}

// §42: dry-run files nothing.
func TestEnsureDependencyIssueDryRunNoop(t *testing.T) {
	prev := ghRunner
	ghRunner = func(context.Context, ...string) ([]byte, error) {
		t.Fatal("dry-run must not call gh")
		return nil, nil
	}
	t.Cleanup(func() { ghRunner = prev })

	res, err := EnsureDependencyIssue(context.Background(), DepIssueRequest{
		Owner: "o", Repo: "r", Finding: DependencyFinding{Package: "p", Version: "1"}, DryRun: true,
	})
	if err != nil || res.Number != 0 || res.Created {
		t.Fatalf("dry-run = %+v err=%v, want no-op", res, err)
	}
}

// §52/§42: disabled deps never scans.
func TestDependencyWorkflowDisabledNoop(t *testing.T) {
	scans := 0
	env := (&testsuite.WorkflowTestSuite{}).NewTestWorkflowEnvironment()
	env.OnActivity(ScanDependencies, mock.Anything, mock.Anything).Return(func(context.Context, DependencyScanRequest) (DependencyReport, error) {
		scans++
		return DependencyReport{}, nil
	})
	env.ExecuteWorkflow(DependencyWorkflow, RepoScope{Owner: "o", Repo: "r"})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	if scans != 0 {
		t.Fatalf("disabled deps scanned %d times, want 0", scans)
	}
}

// §52: an enabled scan files one issue per finding through the idempotent activity.
func TestDependencyWorkflowFilesIssues(t *testing.T) {
	scans, ensured, filed := 0, 0, 0
	env := (&testsuite.WorkflowTestSuite{}).NewTestWorkflowEnvironment()
	env.OnActivity(ScanDependencies, mock.Anything, mock.Anything).Return(func(context.Context, DependencyScanRequest) (DependencyReport, error) {
		scans++
		return DependencyReport{Findings: []DependencyFinding{
			{Package: "a", Version: "1", ID: "CVE-1"},
			{Package: "b", Version: "2"},
		}}, nil
	})
	env.OnActivity(EnsureRepoLabels, mock.Anything, mock.Anything).Return(func(context.Context, LabelsTheirsRequest) error {
		ensured++
		return nil
	})
	env.OnActivity(EnsureDependencyIssue, mock.Anything, mock.Anything).Return(func(_ context.Context, in DepIssueRequest) (DepIssueResult, error) {
		filed++
		return DepIssueResult{Number: 40 + filed, Created: true}, nil
	})

	in := RepoScope{Owner: "o", Repo: "r", Lifecycle: LifecycleConfig{Deps: DependencyConfig{Enabled: true, ScanCmd: "scan"}}}
	env.ExecuteWorkflow(DependencyWorkflow, in)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	if scans != 1 || ensured != 1 || filed != 2 {
		t.Fatalf("scans=%d ensured=%d filed=%d, want 1/1/2", scans, ensured, filed)
	}
}

// §52: autoFileIssues=false keeps the scan report-only.
func TestDependencyWorkflowAutoFileDisabled(t *testing.T) {
	off := false
	filed := 0
	env := (&testsuite.WorkflowTestSuite{}).NewTestWorkflowEnvironment()
	env.OnActivity(ScanDependencies, mock.Anything, mock.Anything).Return(DependencyReport{
		Findings: []DependencyFinding{{Package: "a", Version: "1"}},
	}, nil)
	env.OnActivity(EnsureDependencyIssue, mock.Anything, mock.Anything).Return(func(context.Context, DepIssueRequest) (DepIssueResult, error) {
		filed++
		return DepIssueResult{}, nil
	})

	in := RepoScope{Owner: "o", Repo: "r", Lifecycle: LifecycleConfig{Deps: DependencyConfig{
		Enabled: true, ScanCmd: "scan", AutoFileIssues: &off,
	}}}
	env.ExecuteWorkflow(DependencyWorkflow, in)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	if filed != 0 {
		t.Fatalf("autoFileIssues=false filed %d issues, want 0", filed)
	}
}

// §52/§42: DryRun still scans but the mutating activities receive DryRun=true.
func TestDependencyWorkflowDryRun(t *testing.T) {
	var issueDryRun *bool
	env := (&testsuite.WorkflowTestSuite{}).NewTestWorkflowEnvironment()
	env.OnActivity(ScanDependencies, mock.Anything, mock.Anything).Return(DependencyReport{
		Findings: []DependencyFinding{{Package: "a", Version: "1"}},
	}, nil)
	env.OnActivity(EnsureRepoLabels, mock.Anything, mock.Anything).Return(func(_ context.Context, in LabelsTheirsRequest) error {
		return nil
	})
	env.OnActivity(EnsureDependencyIssue, mock.Anything, mock.Anything).Return(func(_ context.Context, in DepIssueRequest) (DepIssueResult, error) {
		b := in.DryRun
		issueDryRun = &b
		return DepIssueResult{}, nil
	})

	in := RepoScope{Owner: "o", Repo: "r", Lifecycle: LifecycleConfig{
		DryRun: true,
		Deps:   DependencyConfig{Enabled: true, ScanCmd: "scan"},
	}}
	env.ExecuteWorkflow(DependencyWorkflow, in)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	if issueDryRun == nil || !*issueDryRun {
		t.Fatalf("EnsureDependencyIssue must receive DryRun=true, got %v", issueDryRun)
	}
}
