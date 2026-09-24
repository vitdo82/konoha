package konoha

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// §23/§27: the redactor masks registered secret values and every
// credential-shaped pattern rule. Table-driven so a new rule must add a case.
func TestRedactorPatternRules(t *testing.T) {
	const registered = "s3cr3t-value-9f2a"
	googleKey := "AIza" + strings.Repeat("a", 35)
	r := &Redactor{}
	r.Add(registered)

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"registered value", "env=" + registered + " end", "env=[REDACTED] end"},
		{"authorization header", "Authorization: Bearer abc123XYZ", "Authorization: [REDACTED]"},
		{"lowercase authorization", "authorization: token ghp_deadbeef", "authorization: [REDACTED]"},
		{"bare bearer token", "curl -H 'bearer abc.def-123_XY'", "curl -H 'bearer [REDACTED]'"},
		{"openai key", "key sk-abcdefghijklmnop1234", "key [REDACTED]"},
		{"github token", "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789", "[REDACTED]"},
		{"github pat", "ghs_0123456789abcdefghij", "[REDACTED]"},
		{"slack token", "xoxb-1234567890-abcdefghij", "[REDACTED]"},
		{"aws access key id", "AKIAIOSFODNN7EXAMPLE", "[REDACTED]"},
		{"google api key", googleKey, "[REDACTED]"},
		{
			"url userinfo",
			"https://x-access-token:tok3n@github.com/o/r.git",
			"https://x-access-token:[REDACTED]@github.com/o/r.git",
		},
		{"password assignment", "MY_PASSWORD=p@ssw0rd!", "MY_PASSWORD=[REDACTED]"},
		{"token assignment", "GH_TOKEN=abc123", "GH_TOKEN=[REDACTED]"},
		{"secret assignment", "APP_SECRET: shhh", "APP_SECRET: [REDACTED]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := r.Redact(tc.in); got != tc.want {
				t.Fatalf("Redact(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// A registered value shorter than 4 chars is too collision-prone to mask.
func TestRedactorIgnoresShortSecrets(t *testing.T) {
	r := &Redactor{}
	r.Add("abc")
	if got := r.Redact("abc stays"); got != "abc stays" {
		t.Fatalf("short secret must not be masked, got %q", got)
	}
}

// A longer registered secret must not be partially masked by a shorter prefix.
func TestRedactorOverlappingSecrets(t *testing.T) {
	for _, order := range [][]string{{"secret", "secretlonger"}, {"secretlonger", "secret"}} {
		r := &Redactor{}
		r.Add(order...)
		if got := r.Redact("secretlonger"); got != "[REDACTED]" {
			t.Fatalf("add order %v: got %q, want full mask", order, got)
		}
	}
}

// The VERDICT:/BLOCKING:/NITS:/CONCLUDE: parser contract must survive redaction.
func TestRedactLeavesAgentReportIntact(t *testing.T) {
	report := "VERDICT:APPROVE\nBLOCKING:\n- none\nNITS:\n- none\nCONCLUDE: looks good\n"
	if got := Redact(report); got != report {
		t.Fatalf("redaction altered an agent report:\n got %q\nwant %q", got, report)
	}
}

// pushBranch must delegate to the redactor: an installation token echoed by a
// failing git must not reach the returned error.
func TestPushBranchRedactsToken(t *testing.T) {
	stubDir := t.TempDir()
	writeTestFile(t, stubDir, "git",
		"#!/bin/sh\necho \"fatal: unable to access 'https://x-access-token:$GH_TOKEN@github.com/o/r.git'\" >&2\nexit 1\n")
	if err := os.Chmod(filepath.Join(stubDir, "git"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("KONOHA_GIT_BASE", "")

	tok := "ghs_exactinstallationtoken0001"
	err := pushBranch(context.Background(), tok,
		PublishInput{Owner: "o", Repo: "r", Branch: "b", RepoPath: t.TempDir()})
	if err == nil {
		t.Fatal("expected push to fail")
	}
	if strings.Contains(err.Error(), tok) {
		t.Fatalf("token leaked into push error: %v", err)
	}
	if !strings.Contains(err.Error(), "[REDACTED]") {
		t.Fatalf("expected redacted push error, got: %v", err)
	}
}

// An opencode failure that echoes a registered GH_TOKEN or a bearer token must
// return a fail-closed, redacted error (§23/§40).
func TestOpencodeFailureRedactsSecrets(t *testing.T) {
	secret := "ghs_opencodeleak00000001"
	t.Setenv("GH_TOKEN", secret)
	registerOperatorSecrets()

	stubDir := t.TempDir()
	writeTestFile(t, stubDir, "opencode",
		"#!/bin/sh\necho \""+secret+" leaked\" >&2\necho 'Authorization: Bearer abcdefghijklmnop1234' >&2\nexit 3\n")
	if err := os.Chmod(filepath.Join(stubDir, "opencode"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	_, err := ReviewWithOpencode(context.Background(),
		PrInput{Owner: "o", Repo: "r", PrNum: 1, RepoPath: t.TempDir()}, "diff")
	if err == nil {
		t.Fatal("expected opencode failure")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("registered secret leaked: %v", err)
	}
	if strings.Contains(err.Error(), "abcdefghijklmnop1234") {
		t.Fatalf("bearer token leaked: %v", err)
	}
	if !strings.Contains(err.Error(), "[REDACTED]") {
		t.Fatalf("expected redacted error, got: %v", err)
	}
}

// #CU1: a resolved secret value injected into a child environment at run time
// is masked in captured output even when it matches no pattern rule.
func TestInjectedValueRedactedWithoutPattern(t *testing.T) {
	value := "injected-runtime-value-42"
	RegisterSecrets(value)

	stubDir := t.TempDir()
	writeTestFile(t, stubDir, "opencode", "#!/bin/sh\necho '"+value+"' >&2\nexit 2\n")
	if err := os.Chmod(filepath.Join(stubDir, "opencode"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	_, err := ReviewWithOpencode(context.Background(),
		PrInput{Owner: "o", Repo: "r", PrNum: 1, RepoPath: t.TempDir()}, "diff")
	if err == nil {
		t.Fatal("expected opencode failure")
	}
	if strings.Contains(err.Error(), value) {
		t.Fatalf("injected value leaked: %v", err)
	}
	if !strings.Contains(err.Error(), "[REDACTED]") {
		t.Fatalf("expected redacted error, got: %v", err)
	}
}
