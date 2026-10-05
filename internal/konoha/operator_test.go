package konoha

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// doctorStubs installs the doctor external seams and restores them on cleanup.
type doctorStubs struct {
	lookPath func(string) (string, error)
	run      func(context.Context, string, ...string) ([]byte, error)
	health   func(context.Context, string) error
	writable func(string) error
}

// passingDoctorStubs is a baseline where every external seam succeeds.
func passingDoctorStubs() doctorStubs {
	return doctorStubs{
		lookPath: func(name string) (string, error) { return "/usr/bin/" + name, nil },
		run:      func(context.Context, string, ...string) ([]byte, error) { return nil, nil },
		health:   func(context.Context, string) error { return nil },
		writable: func(string) error { return nil },
	}
}

func withDoctorStubs(t *testing.T, s doctorStubs) {
	t.Helper()
	savedLP, savedRun, savedHealth, savedWritable := doctorLookPath, doctorRunCommand, doctorTemporalHealth, doctorWritable
	t.Cleanup(func() {
		doctorLookPath, doctorRunCommand, doctorTemporalHealth, doctorWritable = savedLP, savedRun, savedHealth, savedWritable
	})
	if s.lookPath != nil {
		doctorLookPath = s.lookPath
	}
	if s.run != nil {
		doctorRunCommand = s.run
	}
	if s.health != nil {
		doctorTemporalHealth = s.health
	}
	if s.writable != nil {
		doctorWritable = s.writable
	}
}

func findCheck(checks []doctorCheck, name string) *doctorCheck {
	for i := range checks {
		if checks[i].name == name {
			return &checks[i]
		}
	}
	return nil
}

// (writeRepos is defined in config_test.go.)

// #99/AC: every failure mode fails its named check with a specific message, and
// an all-good environment passes every check.
func TestDoctorChecksFailureModes(t *testing.T) {
	ctx := context.Background()
	missingApp := filepath.Join(t.TempDir(), "absent-konoha.yaml") // defaults, no error
	validRepos := func(t *testing.T) string { t.Helper(); return writeRepos(t, "repos: []\n") }

	t.Run("all pass", func(t *testing.T) {
		withDoctorStubs(t, passingDoctorStubs())
		checks := runDoctorChecks(ctx, missingApp, validRepos(t), "127.0.0.1:7233")
		for _, c := range checks {
			if c.err != nil {
				t.Fatalf("check %s unexpectedly failed: %v", c.name, c.err)
			}
		}
		for _, name := range []string{"konoha.yaml", "repos.yaml", "harness", "evidence dir", "gh", "temporal"} {
			if findCheck(checks, name) == nil {
				t.Fatalf("missing check %q", name)
			}
		}
	})

	t.Run("konoha.yaml load", func(t *testing.T) {
		withDoctorStubs(t, passingDoctorStubs())
		bad := filepath.Join(t.TempDir(), "konoha.yaml")
		if err := os.WriteFile(bad, []byte("bogus_key: true\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		c := findCheck(runDoctorChecks(ctx, bad, validRepos(t), "a:1"), "konoha.yaml")
		if c == nil || c.err == nil || !strings.Contains(c.err.Error(), "konoha.yaml") {
			t.Fatalf("konoha.yaml check = %+v, want load error naming the file", c)
		}
	})

	t.Run("repos.yaml load", func(t *testing.T) {
		withDoctorStubs(t, passingDoctorStubs())
		missing := filepath.Join(t.TempDir(), "absent-repos.yaml")
		c := findCheck(runDoctorChecks(ctx, missingApp, missing, "a:1"), "repos.yaml")
		if c == nil || c.err == nil {
			t.Fatalf("repos.yaml check = %+v, want load error", c)
		}
	})

	t.Run("temporal unreachable", func(t *testing.T) {
		s := passingDoctorStubs()
		s.health = func(context.Context, string) error { return fmt.Errorf("temporal 127.0.0.1:1 unreachable: refused") }
		withDoctorStubs(t, s)
		c := findCheck(runDoctorChecks(ctx, missingApp, validRepos(t), "127.0.0.1:1"), "temporal")
		if c == nil || c.err == nil || !strings.Contains(c.err.Error(), "unreachable") {
			t.Fatalf("temporal check = %+v, want unreachable error", c)
		}
	})

	t.Run("gh missing", func(t *testing.T) {
		s := passingDoctorStubs()
		s.lookPath = func(name string) (string, error) {
			if name == "gh" {
				return "", fmt.Errorf("executable file not found")
			}
			return "/usr/bin/" + name, nil
		}
		withDoctorStubs(t, s)
		c := findCheck(runDoctorChecks(ctx, missingApp, validRepos(t), "a:1"), "gh")
		if c == nil || c.err == nil || !strings.Contains(c.err.Error(), "not found on PATH") {
			t.Fatalf("gh check = %+v, want not-found error", c)
		}
	})

	t.Run("gh unauthenticated", func(t *testing.T) {
		s := passingDoctorStubs()
		s.run = func(context.Context, string, ...string) ([]byte, error) {
			return []byte("not logged into any GitHub hosts"), fmt.Errorf("exit status 1")
		}
		withDoctorStubs(t, s)
		c := findCheck(runDoctorChecks(ctx, missingApp, validRepos(t), "a:1"), "gh")
		if c == nil || c.err == nil || !strings.Contains(c.err.Error(), "not authenticated") {
			t.Fatalf("gh check = %+v, want not-authenticated error", c)
		}
	})

	t.Run("harness not on path", func(t *testing.T) {
		s := passingDoctorStubs()
		s.lookPath = func(name string) (string, error) {
			if name == "opencode" {
				return "", fmt.Errorf("executable file not found")
			}
			return "/usr/bin/" + name, nil
		}
		withDoctorStubs(t, s)
		c := findCheck(runDoctorChecks(ctx, missingApp, validRepos(t), "a:1"), "harness")
		if c == nil || c.err == nil || !strings.Contains(c.err.Error(), "opencode") || !strings.Contains(c.err.Error(), "PATH") {
			t.Fatalf("harness check = %+v, want opencode not-found error", c)
		}
	})

	t.Run("evidence dir unwritable", func(t *testing.T) {
		s := passingDoctorStubs()
		s.writable = func(string) error { return fmt.Errorf("evidence dir /x not writable: permission denied") }
		withDoctorStubs(t, s)
		c := findCheck(runDoctorChecks(ctx, missingApp, validRepos(t), "a:1"), "evidence dir")
		if c == nil || c.err == nil || !strings.Contains(c.err.Error(), "not writable") {
			t.Fatalf("evidence check = %+v, want not-writable error", c)
		}
	})
}

// #99: a generic harness selected by a repo is checked by its configured name
// (never a path), and an unset generic executable fails.
func TestDoctorHarnessCheckGeneric(t *testing.T) {
	withDoctorStubs(t, passingDoctorStubs())
	cfg := defaultAppConfig()
	cfg.Harness.Executable = "myagent"
	repos := []RepoConfig{{Owner: "o", Repo: "r", RepoPath: "/tmp/r", Lifecycle: &LifecycleConfig{Harness: "generic"}}}
	if err := doctorHarnessCheck(cfg, repos); err != nil {
		t.Fatalf("generic harness with executable on PATH: %v", err)
	}

	withDoctorStubs(t, doctorStubs{lookPath: func(name string) (string, error) {
		if name == "myagent" {
			return "", fmt.Errorf("not found")
		}
		return "/usr/bin/" + name, nil
	}})
	if err := doctorHarnessCheck(cfg, repos); err == nil || !strings.Contains(err.Error(), "myagent") {
		t.Fatalf("missing generic executable must fail naming it, got %v", err)
	}

	empty := defaultAppConfig()
	empty.Harness.Executable = ""
	if err := doctorHarnessCheck(empty, repos); err == nil || !strings.Contains(err.Error(), "no executable") {
		t.Fatalf("unset generic executable must fail, got %v", err)
	}
}

// #99: the evidence dir must round-trip a write; a non-directory path fails.
func TestDoctorWritableDir(t *testing.T) {
	dir := t.TempDir()
	if err := writableDir(dir); err != nil {
		t.Fatalf("writable temp dir: %v", err)
	}
	file := filepath.Join(dir, "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writableDir(file); err == nil || !strings.Contains(err.Error(), "not writable") {
		t.Fatalf("writableDir(file) = %v, want not-writable error", err)
	}
}

// #99/AC: runDoctor returns non-zero (an error) when a check fails.
func TestRunDoctorNonZeroOnFailure(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "repos.yaml"), []byte("repos: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KONOHA_CONFIG_DIR", dir) // konoha.yaml absent -> defaults
	s := passingDoctorStubs()
	s.lookPath = func(name string) (string, error) {
		if name == "gh" {
			return "", fmt.Errorf("executable file not found")
		}
		return "/usr/bin/" + name, nil
	}
	withDoctorStubs(t, s)
	if err := runDoctor(); err == nil {
		t.Fatal("runDoctor must return an error when a check fails")
	}
}

// #99: logs + describe read the evidence store with no Temporal server.
func TestOperatorLogsDescribeNoTemporal(t *testing.T) {
	withEvidenceDir(t)
	outcome := `{"workflowId":"ducklambda-issue-7","terminalState":"CLOSED",` +
		`"agentOutcome":"SUCCESS","verificationOutcome":"SUCCESS",` +
		`"usage":{"tokensIn":10,"tokensOut":20,"costUSD":0.5},` +
		`"conditions":[{"type":"agent","status":"SUCCESS","at":"2026-01-01T00:00:00Z"}]}`
	if err := WriteCycleEvidence(context.Background(), EvidenceRequest{
		CycleID: "ducklambda-issue-7", Stdout: "agent output\n", Validation: "ok\n", Outcome: outcome,
	}); err != nil {
		t.Fatal(err)
	}

	var logs bytes.Buffer
	if err := runLogs([]string{"ducklambda", "7"}, &logs); err != nil {
		t.Fatalf("runLogs: %v", err)
	}
	if !strings.Contains(logs.String(), "agent output") || !strings.Contains(logs.String(), evidenceStdout) {
		t.Fatalf("logs missing agent stdout: %q", logs.String())
	}

	var desc bytes.Buffer
	if err := runDescribeOutcome([]string{"ducklambda", "7"}, &desc); err != nil {
		t.Fatalf("runDescribeOutcome: %v", err)
	}
	for _, want := range []string{"state: CLOSED", "agent: SUCCESS", "verification: SUCCESS", "usage: tokens_in=10", "conditions:"} {
		if !strings.Contains(desc.String(), want) {
			t.Fatalf("describe missing %q:\n%s", want, desc.String())
		}
	}
}

// #99: a cycle with no evidence is a specific error, not an empty success.
func TestOperatorLogsDescribeNoEvidence(t *testing.T) {
	withEvidenceDir(t)
	if err := runLogs([]string{"ducklambda", "9"}, &bytes.Buffer{}); err == nil {
		t.Fatal("runLogs without evidence must fail")
	}
	if err := runDescribeOutcome([]string{"ducklambda", "9"}, &bytes.Buffer{}); err == nil {
		t.Fatal("runDescribeOutcome without evidence must fail")
	}
	if _, _, err := repoIssueArgs([]string{"ducklambda"}, "logs"); err == nil {
		t.Fatal("missing issue must be a usage error")
	}
}
