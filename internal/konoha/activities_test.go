package konoha

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"go.temporal.io/sdk/testsuite"
)

// unsetCreds clears GH_APP_* so activities can't accidentally mint tokens,
// restoring the caller's environment afterwards.
func unsetCreds(t *testing.T) {
	saved := map[string]string{}
	for _, k := range []string{"GH_APP_ID", "GH_APP_INSTALL_ID", "GH_APP_PEM"} {
		if v, ok := os.LookupEnv(k); ok {
			saved[k] = v
			os.Unsetenv(k)
		}
	}
	t.Cleanup(func() {
		for k, v := range saved {
			os.Setenv(k, v)
		}
	})
}

// Acceptance: the opencode agent environment contains no GitHub write credential.
// Even if the worker's own process env is stuffed with tokens/App creds, the
// scrubber must strip every GH_/GITHUB_ one of them.
func TestAgentEnvHasNoGitHubCredential(t *testing.T) {
	creds := map[string]string{
		"GH_APP_ID":           "123",
		"GH_APP_INSTALL_ID":   "456",
		"GH_APP_PEM":          "/tmp/app.pem",
		"GH_TOKEN":            "sekret",
		"GITHUB_TOKEN":        "sekret2",
		"GH_ENTERPRISE_TOKEN": "sekret3",
	}
	for k, v := range creds {
		os.Setenv(k, v)
	}
	defer func() {
		for k := range creds {
			os.Unsetenv(k)
		}
	}()

	for _, kv := range agentEnv() {
		key := kv[:strings.IndexByte(kv, '=')]
		if strings.HasPrefix(key, "GH_") || strings.HasPrefix(key, "GITHUB_") {
			t.Fatalf("agent env leaked GitHub credential var %q", key)
		}
	}
}

// §26 gate: failing validation blocks publishing (PR skipped) unless the
// workflow explicitly allows it, and the failure output is recorded either way.
func TestValidateGate(t *testing.T) {
	dir := t.TempDir()

	// block path
	_, err := validateGate(PublishInput{RepoPath: dir, Branch: "b", ValidationCmd: "sh -c 'echo boom; exit 7'"})
	if err == nil {
		t.Fatal("expected validation gate to block, got nil error")
	}
	if !strings.Contains(err.Error(), "validation failed") {
		t.Fatalf("expected validation failure, got: %v", err)
	}

	// override path: recorded, not silent, but allowed
	res, err := validateGate(PublishInput{
		RepoPath: dir, Branch: "b", ValidationCmd: "sh -c 'echo boom; exit 7'", AllowOnFailure: true,
	})
	if err != nil {
		t.Fatalf("override path should not block: %v", err)
	}
	if res.Validated {
		t.Fatal("Validated must be false when validation fails")
	}
	if !strings.Contains(res.ValidationOutput, "boom") {
		t.Fatalf("failure output must be recorded, got %q", res.ValidationOutput)
	}

	// skipped path (no validation configured)
	res, err = validateGate(PublishInput{RepoPath: dir, Branch: "b"})
	if err != nil {
		t.Fatalf("no validation configured should pass: %v", err)
	}
	if !res.ValidationSkipped {
		t.Fatal("ValidationSkipped must be true when no command configured")
	}
}

// An agent result alone cannot mutate GitHub: publishing still requires the App
// credentials (present only in the worker), so it must fail hard without them.
func TestPublishDeveloperChangeFailsWithoutAppCreds(t *testing.T) {
	unsetCreds(t)
	env := (&testsuite.WorkflowTestSuite{}).NewTestActivityEnvironment()
	env.RegisterActivity(PublishDeveloperChange)

	_, err := env.ExecuteActivity(PublishDeveloperChange, PublishInput{
		Owner: "someowner", Repo: "somerepo", RepoPath: t.TempDir(),
		Branch: "konoha/agent-result", Title: "t", Body: "b",
		ValidationCmd: "true", // agent-side validation passed
	})
	if err == nil {
		t.Fatal("expected publish to fail without App credentials, got success")
	}
	if !strings.Contains(err.Error(), "GH_APP") {
		t.Fatalf("expected a missing-credentials error, got: %v", err)
	}
}

// Validation runs before any GitHub mutation: the gate error surfaces before
// the App token is even consulted.
func TestPublishValidationRunsBeforeMutation(t *testing.T) {
	unsetCreds(t)
	env := (&testsuite.WorkflowTestSuite{}).NewTestActivityEnvironment()
	env.RegisterActivity(PublishDeveloperChange)

	_, err := env.ExecuteActivity(PublishDeveloperChange, PublishInput{
		Owner: "someowner", Repo: "somerepo", RepoPath: t.TempDir(),
		Branch: "konoha/broken", Title: "t", Body: "b",
		ValidationCmd: "sh -c 'echo boom; exit 7'",
	})
	if err == nil {
		t.Fatal("expected validation gate to block, got success")
	}
	if strings.Contains(err.Error(), "GH_APP") {
		t.Fatalf("validation gate must fire before credentials are touched, got: %v", err)
	}
}

// §42 dry-run: a dry-run review must skip the GitHub mutation entirely — no
// App credentials are even needed.
func TestSubmitReviewHonorsDryRun(t *testing.T) {
	unsetCreds(t)
	env := (&testsuite.WorkflowTestSuite{}).NewTestActivityEnvironment()
	env.RegisterActivity(SubmitReview)

	_, err := env.ExecuteActivity(SubmitReview,
		PrInput{Owner: "o", Repo: "r", PrNum: 1, Lifecycle: LifecycleConfig{DryRun: true}},
		ReviewResult{Verdict: "APPROVE", Summary: "s"})
	if err != nil {
		t.Fatalf("dryRun should suppress the review without creds, got: %v", err)
	}
}

// §42 dry-run: publishing must stop before the App token is even minted.
func TestPublishDeveloperChangeHonorsDryRun(t *testing.T) {
	unsetCreds(t)
	env := (&testsuite.WorkflowTestSuite{}).NewTestActivityEnvironment()
	env.RegisterActivity(PublishDeveloperChange)

	_, err := env.ExecuteActivity(PublishDeveloperChange, PublishInput{
		Owner: "o", Repo: "r", RepoPath: t.TempDir(),
		Branch: "konoha/x", Title: "t", Body: "b", DryRun: true,
	})
	if err != nil {
		t.Fatalf("dryRun publish should skip mutation without creds, got: %v", err)
	}
}

// runSubmitReview executes SubmitReview through the Temporal activity test env
// with the gh command seam stubbed, returning the recorded invocations + error.
func runSubmitReview(t *testing.T, r ReviewResult, runner func(ctx context.Context, tok string, args ...string) ([]byte, error)) ([][]string, error) {
	t.Helper()
	useFakeToken(t)
	var calls [][]string
	withTokenRunner(t, func(ctx context.Context, tok string, args ...string) ([]byte, error) {
		calls = append(calls, args)
		return runner(ctx, tok, args...)
	})
	env := (&testsuite.WorkflowTestSuite{}).NewTestActivityEnvironment()
	env.RegisterActivity(SubmitReview)
	_, err := env.ExecuteActivity(SubmitReview,
		PrInput{Owner: "o", Repo: "r", PrNum: 7},
		r)
	return calls, err
}

// A self-authored PR: GitHub rejects the formal --request-changes review, so
// SubmitReview must retry as a non-blocking --comment review and succeed.
func TestSubmitReviewFallsBackToCommentOnSelfReview(t *testing.T) {
	calls, err := runSubmitReview(t, ReviewResult{Verdict: "REQUEST_CHANGES", Summary: "## Agent Review: REQUEST_CHANGES"},
		func(_ context.Context, _ string, args ...string) ([]byte, error) {
			if slices.Contains(args, "--comment") {
				return nil, nil
			}
			return []byte("GraphQL: Review Can not request changes on your own pull request (addPullRequestReview): exit status 1"),
				errors.New("exit status 1")
		})
	if err != nil {
		t.Fatalf("self-review must fall back to a comment review, got: %v", err)
	}
	if len(calls) != 2 {
		t.Fatalf("want formal attempt then comment fallback, got %d calls", len(calls))
	}
	if !slices.Contains(calls[0], "--request-changes") || !slices.Contains(calls[1], "--comment") {
		t.Fatalf("unexpected review calls: %v / %v", calls[0], calls[1])
	}
}

// A formal review that succeeds is posted unchanged (no comment fallback).
func TestSubmitReviewPostsFormalReviewWhenNotSelfAuthored(t *testing.T) {
	calls, err := runSubmitReview(t, ReviewResult{Verdict: "REQUEST_CHANGES", Summary: "s"},
		func(_ context.Context, _ string, _ ...string) ([]byte, error) { return nil, nil })
	if err != nil {
		t.Fatalf("formal review should succeed, got: %v", err)
	}
	if len(calls) != 1 || !slices.Contains(calls[0], "--request-changes") || slices.Contains(calls[0], "--comment") {
		t.Fatalf("unexpected calls: %v", calls)
	}
}

// Any failure other than the self-review restriction is fail-closed (§40).
func TestSubmitReviewPropagatesOtherErrors(t *testing.T) {
	calls, err := runSubmitReview(t, ReviewResult{Verdict: "REQUEST_CHANGES", Summary: "s"},
		func(_ context.Context, _ string, _ ...string) ([]byte, error) {
			return []byte("HTTP 500: server exploded"), errors.New("exit status 1")
		})
	if err == nil {
		t.Fatal("a non-self-review failure must propagate, never silently succeed")
	}
	if len(calls) != 1 {
		t.Fatalf("no fallback expected on a generic error, got %d calls", len(calls))
	}
}

// §4: agentArgs uses the per-repo model when set and falls back to the built-in
// default otherwise, so repos.yaml is the source of truth per repo.
func TestAgentArgsResolvesModel(t *testing.T) {
	if got := strings.Join(agentArgs("p", "openrouter/x/y"), " "); got != "run --model openrouter/x/y p" {
		t.Fatalf("per-repo model not honored: %q", got)
	}
	if got := agentArgs("p", "")[2]; got != defaultAgentModel {
		t.Fatalf("unset model must fall back to %q, got %q", defaultAgentModel, got)
	}
}
