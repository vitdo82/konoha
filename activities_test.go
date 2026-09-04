package main

import (
	"os"
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
