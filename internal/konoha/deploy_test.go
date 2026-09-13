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

// deployFake is a stateful gh seam for the §51 activities: it answers the
// GitHub deployments API and records mutations.
type deployFake struct {
	deployments []int64  // deployment ids for the ref
	state       string   // latest deployment status state ("" = none yet)
	created     int      // POST /deployments count
	statuses    []string // status states posted
	lastCreate  []string
}

func (f *deployFake) run(_ context.Context, _ string, args ...string) ([]byte, error) {
	line := strings.Join(args, " ")
	switch {
	case strings.Contains(line, "deployments?ref="):
		rows := make([]map[string]int64, 0, len(f.deployments))
		for _, id := range f.deployments {
			rows = append(rows, map[string]int64{"id": id})
		}
		return json.Marshal(rows)
	case strings.Contains(line, "/statuses?per_page=1"):
		if f.state == "" {
			return []byte("[]"), nil
		}
		return json.Marshal([]map[string]string{{"state": f.state}})
	case strings.HasPrefix(line, "api -X POST repos/o/r/deployments/") && strings.Contains(line, "/statuses"):
		f.statuses = append(f.statuses, argValue(args, "state="))
		f.state = argValue(args, "state=")
		return []byte(`{}`), nil
	case strings.HasPrefix(line, "api -X POST repos/o/r/deployments"):
		f.created++
		f.lastCreate = args
		f.deployments = append(f.deployments, 7)
		return []byte(`{"id":7}`), nil
	}
	return nil, fmt.Errorf("unexpected gh call: %s", line)
}

func argValue(args []string, prefix string) string {
	for _, a := range args {
		if strings.HasPrefix(a, prefix) {
			return a[len(prefix):]
		}
	}
	return ""
}

// §51/§42: disabled deploy mutates nothing and never shells out.
func TestDeployReleaseDisabledNoop(t *testing.T) {
	useFakeToken(t)
	withTokenRunner(t, func(context.Context, string, ...string) ([]byte, error) {
		t.Fatal("disabled deploy must not call gh")
		return nil, nil
	})
	res, err := DeployRelease(context.Background(), DeployRequest{
		Owner: "o", Repo: "r", Tag: "v1.0.0", Config: DeployConfig{Enabled: false},
	})
	if err != nil || !res.Skipped || !strings.Contains(res.Reason, "disabled") {
		t.Fatalf("want disabled skip, got %+v err=%v", res, err)
	}
}

// §51/§42: dry-run mutates nothing.
func TestDeployReleaseDryRunNoop(t *testing.T) {
	useFakeToken(t)
	withTokenRunner(t, func(context.Context, string, ...string) ([]byte, error) {
		t.Fatal("dry run must not call gh")
		return nil, nil
	})
	res, err := DeployRelease(context.Background(), DeployRequest{
		Owner: "o", Repo: "r", Tag: "v1.0.0",
		Config: DeployConfig{Enabled: true, Command: "true"}, DryRun: true,
	})
	if err != nil || !res.Skipped || res.Reason != "dryRun" {
		t.Fatalf("want dryRun skip, got %+v err=%v", res, err)
	}
}

// §51: no released tag → nothing to deploy (fail-closed skip).
func TestDeployReleaseNoTagNoop(t *testing.T) {
	useFakeToken(t)
	withTokenRunner(t, func(context.Context, string, ...string) ([]byte, error) {
		t.Fatal("no tag must not call gh")
		return nil, nil
	})
	res, err := DeployRelease(context.Background(), DeployRequest{
		Owner: "o", Repo: "r", Config: DeployConfig{Enabled: true, Command: "true"},
	})
	if err != nil || !res.Skipped || !strings.Contains(res.Reason, "no release tag") {
		t.Fatalf("want no-tag skip, got %+v err=%v", res, err)
	}
}

// §51/§24: a tag whose latest deployment already succeeded is never re-deployed
// (the tag-based guard) and the hook is not run again.
func TestDeployReleaseTagGuardSkips(t *testing.T) {
	useFakeToken(t)
	f := &deployFake{deployments: []int64{5}, state: deployStateSuccess}
	withTokenRunner(t, f.run)
	res, err := DeployRelease(context.Background(), DeployRequest{
		Owner: "o", Repo: "r", Tag: "v1.0.0",
		Config: DeployConfig{Enabled: true, Command: "false"},
	})
	if err != nil || !res.Skipped || !strings.Contains(res.Reason, "already deployed") {
		t.Fatalf("want tag-guard skip, got %+v err=%v", res, err)
	}
	if f.created != 0 {
		t.Fatalf("guard must not create a deployment, created=%d", f.created)
	}
}

// §51: an enabled repo records a deployment, runs the hook against the tag, and
// marks success; a second run for the same (now successful) tag is a no-op.
func TestDeployReleaseRunsOncePerTag(t *testing.T) {
	useFakeToken(t)
	f := &deployFake{}
	withTokenRunner(t, f.run)
	req := DeployRequest{
		Owner: "o", Repo: "r", RepoPath: t.TempDir(), Tag: "v1.0.1",
		Config: DeployConfig{Enabled: true, Command: `test "$KONOHA_RELEASE_TAG" = v1.0.1`},
	}
	res, err := DeployRelease(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Deployed || res.Tag != "v1.0.1" {
		t.Fatalf("unexpected deploy result: %+v", res)
	}
	if f.created != 1 || len(f.statuses) != 1 || f.statuses[0] != deployStateSuccess {
		t.Fatalf("want one deployment + success status, got created=%d statuses=%v", f.created, f.statuses)
	}
	if !strings.Contains(strings.Join(f.lastCreate, " "), "environment=production") {
		t.Fatalf("deployment must record the environment: %v", f.lastCreate)
	}

	res2, err := DeployRelease(context.Background(), req)
	if err != nil || !res2.Skipped || f.created != 1 {
		t.Fatalf("second deploy must be a no-op: res=%+v created=%d err=%v", res2, f.created, err)
	}
}

// §51: a failing hook returns a rollback recommendation and marks the
// deployment failed — never a silent success.
func TestDeployReleaseHookFailureRollback(t *testing.T) {
	useFakeToken(t)
	f := &deployFake{}
	withTokenRunner(t, f.run)
	res, err := DeployRelease(context.Background(), DeployRequest{
		Owner: "o", Repo: "r", RepoPath: t.TempDir(), Tag: "v1.0.2",
		Config: DeployConfig{Enabled: true, Command: "false"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Deployed || !res.Rollback {
		t.Fatalf("failed hook must recommend rollback: %+v", res)
	}
	if len(f.statuses) != 1 || f.statuses[0] != deployStateFailure {
		t.Fatalf("deployment must be marked failed, got %v", f.statuses)
	}
}

// §51/§40: verification disabled or unconfigured is a no-op.
func TestVerifyDeploymentNoopWhenUnconfigured(t *testing.T) {
	res, err := VerifyDeployment(context.Background(), VerifyRequest{
		Config: DeployConfig{Enabled: false},
	})
	if err != nil || !res.Skipped || !strings.Contains(res.Detail, "disabled") {
		t.Fatalf("disabled verify must skip, got %+v err=%v", res, err)
	}
	useFakeToken(t)
	res, err = VerifyDeployment(context.Background(), VerifyRequest{
		Config: DeployConfig{Enabled: true},
	})
	if err != nil || !res.Skipped || !strings.Contains(res.Detail, "no verify") {
		t.Fatalf("unconfigured verify must skip, got %+v err=%v", res, err)
	}
}

// §51: a passing verify is success; a permanently failing verify is not.
func TestVerifyDeploymentPassAndPermanentFailure(t *testing.T) {
	useFakeToken(t)
	// Pass: no gh call needed (no rollback signal).
	withTokenRunner(t, func(context.Context, string, ...string) ([]byte, error) {
		t.Fatal("passing verify must not call gh")
		return nil, nil
	})
	ok, err := VerifyDeployment(context.Background(), VerifyRequest{
		RepoPath: t.TempDir(), Tag: "v1.0.1",
		Config: DeployConfig{Enabled: true, Verify: "true"},
	})
	if err != nil || !ok.Verified {
		t.Fatalf("verify true must pass: %+v err=%v", ok, err)
	}

	f := &deployFake{}
	withTokenRunner(t, f.run)
	bad, err := VerifyDeployment(context.Background(), VerifyRequest{
		Owner: "o", Repo: "r", RepoPath: t.TempDir(), Tag: "v1.0.1", DeploymentID: 7,
		Config: DeployConfig{Enabled: true, Verify: "false", MaxWait: "30ms"}, RollbackOnFail: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if bad.Verified || !bad.Rollback || bad.Attempts < 1 {
		t.Fatalf("failing verify must be permanent + rollback: %+v", bad)
	}
	if len(f.statuses) != 1 || f.statuses[0] != deployStateInactive {
		t.Fatalf("rollback signal must mark the deployment inactive, got %v", f.statuses)
	}
}

// §51/§40: a permanent post-deploy verification failure routes the issue to
// HUMAN_REQUIRED (reopen + konoha:human-required) and the workflow is not a
// silent success.
func TestPostMergeVerifyFailureEscalates(t *testing.T) {
	env := (&testsuite.WorkflowTestSuite{}).NewTestWorkflowEnvironment()
	env.OnActivity(DeployRelease, mock.Anything, mock.Anything).Return(
		DeployResult{Deployed: true, Tag: "v1.0.1", DeploymentID: 7}, nil)
	env.OnActivity(VerifyDeployment, mock.Anything, mock.Anything).Return(
		VerifyResult{Verified: false, Rollback: true, Detail: "smoke failed"}, nil)
	var reopened bool
	var labels []string
	env.OnActivity(EnsureIssueState, mock.Anything, mock.Anything).Run(func(args mock.Arguments) {
		if args.Get(1).(StateRequest).State == "open" {
			reopened = true
		}
	}).Return(nil)
	env.OnActivity(ApplyIssueLabels, mock.Anything, mock.Anything).Run(func(args mock.Arguments) {
		labels = args.Get(1).(LabelsRequest).Labels
	}).Return(nil)

	in := PostMergeInput{Owner: "o", Repo: "r", IssueNum: testIssue, PrNum: 101, RepoPath: t.TempDir(),
		Lifecycle: LifecycleConfig{Deploy: DeployConfig{Enabled: true, Command: "true", Verify: "false"}}}
	env.ExecuteWorkflow(PostMergeWorkflow, in)
	if !env.IsWorkflowCompleted() {
		t.Fatal("workflow did not complete")
	}
	if err := env.GetWorkflowError(); err == nil {
		t.Fatal("verification failure must fail the workflow (no silent success)")
	}
	if !reopened {
		t.Fatal("verification failure must reopen the issue to HUMAN_REQUIRED")
	}
	if !containsString(labels, labelHumanRequired) {
		t.Fatalf("expected konoha:human-required label, got %v", labels)
	}
}

// §51: a repo with deploy disabled does not run the post-merge deploy/verify
// activities at all.
func TestPostMergeDeployDisabledNoop(t *testing.T) {
	env := (&testsuite.WorkflowTestSuite{}).NewTestWorkflowEnvironment()
	env.OnActivity(DeployRelease, mock.Anything, mock.Anything).Run(func(mock.Arguments) {
		t.Fatal("disabled deploy must not run")
	}).Return(DeployResult{}, nil)
	in := PostMergeInput{Owner: "o", Repo: "r", IssueNum: testIssue, PrNum: 101,
		Lifecycle: LifecycleConfig{}}
	env.ExecuteWorkflow(PostMergeWorkflow, in)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("disabled deploy must be a clean no-op: %v", err)
	}
}
