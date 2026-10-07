package konoha

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// #103: advisory network egress — config validation, evidence, env wiring, and
// the explicit non-enforcement contract.

func TestEgressConfigValidate(t *testing.T) {
	good := []EgressConfig{
		{},
		{Proxy: "http://127.0.0.1:8888", AllowOut: []string{"api.anthropic.com"}},
		{Proxy: "https://proxy.internal:3128"},
	}
	for _, e := range good {
		if err := e.validate("egress"); err != nil {
			t.Fatalf("valid egress rejected: %+v: %v", e, err)
		}
	}
	bad := []EgressConfig{
		{Proxy: "ftp://proxy:21"},
		{Proxy: "not-a-url"},
		{Proxy: "http://"},
		{Proxy: "http://127.0.0.1:8888", AllowOut: []string{""}},
		{Proxy: "http://127.0.0.1:8888", AllowOut: []string{"  "}},
	}
	for _, e := range bad {
		if err := e.validate("egress"); err == nil {
			t.Fatalf("expected rejection for %+v", e)
		}
	}
}

func TestAppConfigRejectsBadEgress(t *testing.T) {
	cfg := defaultAppConfig()
	cfg.Egress = EgressConfig{Proxy: "ftp://nope"}
	if err := cfg.validate(); err == nil {
		t.Fatal("malformed egress.proxy must fail at load")
	}
	cfg.Egress = EgressConfig{Proxy: "http://127.0.0.1:8888", AllowOut: []string{"api.anthropic.com"}}
	if err := cfg.validate(); err != nil {
		t.Fatalf("valid egress rejected by AppConfig.validate: %v", err)
	}
}

func TestEgressEvidenceAdvisoryOnly(t *testing.T) {
	if ev := egressEvidence(EgressConfig{}); ev != nil {
		t.Fatalf("unset egress must record nothing, got %+v", ev)
	}
	ev := egressEvidence(EgressConfig{Proxy: " http://127.0.0.1:8888 ", AllowOut: []string{"api.anthropic.com"}})
	if ev == nil {
		t.Fatal("configured egress must record evidence")
	}
	if ev.Status != egressAdvisory {
		t.Fatalf("host egress must record %q, got %q", egressAdvisory, ev.Status)
	}
	if ev.Proxy != "http://127.0.0.1:8888" {
		t.Fatalf("proxy not normalized: %q", ev.Proxy)
	}
	if len(ev.AllowOut) != 1 || ev.AllowOut[0] != "api.anthropic.com" {
		t.Fatalf("allowlist not recorded: %+v", ev.AllowOut)
	}
}

func TestEgressEnvAppliedAndAbsent(t *testing.T) {
	base := []string{"PATH=/usr/bin", "PWD=/x"}
	if got := egressEnv(base, EgressConfig{}); len(got) != len(base) || got[0] != base[0] || got[1] != base[1] {
		t.Fatalf("absent config must leave env unchanged, got %v", got)
	}

	got := egressEnv(append([]string{}, base...), EgressConfig{Proxy: "http://127.0.0.1:8888"})
	for _, want := range []string{"HTTPS_PROXY=http://127.0.0.1:8888", "HTTP_PROXY=http://127.0.0.1:8888", "NO_PROXY=localhost,127.0.0.1,::1"} {
		if !containsEnv(got, want) {
			t.Fatalf("configured egress missing %q in %v", want, got)
		}
	}
}

func TestEgressEnvOverridesInheritedProxy(t *testing.T) {
	got := egressEnv([]string{"HTTPS_PROXY=http://stale:1"}, EgressConfig{Proxy: "http://127.0.0.1:8888"})
	if n := countEnv(got, "HTTPS_PROXY="); n != 1 {
		t.Fatalf("expected exactly one HTTPS_PROXY, got %d: %v", n, got)
	}
	if !containsEnv(got, "HTTPS_PROXY=http://127.0.0.1:8888") {
		t.Fatalf("config proxy must win over inherited: %v", got)
	}
}

// TestEgressAppliesToAgentChild is AC1: the proxy env reaches the agent child.
func TestEgressAppliesToAgentChild(t *testing.T) {
	saved := appCfg
	defer func() { appCfg = saved }()
	appCfg.Egress = EgressConfig{Proxy: "http://127.0.0.1:8888", AllowOut: []string{"api.anthropic.com"}}

	cmd, _, err := agentCommand(context.Background(), "prompt", "", "opencode", t.TempDir(), LimitsConfig{}, AgentTooling{})
	if err != nil {
		t.Fatalf("agentCommand: %v", err)
	}
	if !containsEnv(cmd.Env, "HTTPS_PROXY=http://127.0.0.1:8888") || !containsEnv(cmd.Env, "HTTP_PROXY=http://127.0.0.1:8888") {
		t.Fatalf("agent child missing proxy env: %v", cmd.Env)
	}
}

// TestEgressAppliesToValidationGate is AC1 for the validation path.
func TestEgressAppliesToValidationGate(t *testing.T) {
	saved := appCfg
	defer func() { appCfg = saved }()
	appCfg.Egress = EgressConfig{Proxy: "http://127.0.0.1:8888"}

	r := runGate(PublishInput{RepoPath: t.TempDir()}, ValidationGate{ID: "env", Argv: []string{"sh", "-c", "printenv HTTPS_PROXY"}})
	if r.Err != "" || r.ExitCode != 0 {
		t.Fatalf("gate failed: %+v", r)
	}
	if !strings.Contains(r.Stdout, "http://127.0.0.1:8888") {
		t.Fatalf("validation gate missing proxy env, stdout=%q", r.Stdout)
	}
}

// TestCycleOutcomeJSONIncludesEgress is AC2: outcome.json records advisory + the
// allowlist, and never claims enforcement.
func TestCycleOutcomeJSONIncludesEgress(t *testing.T) {
	out := CycleOutcome{Egress: egressEvidence(EgressConfig{Proxy: "http://127.0.0.1:8888", AllowOut: []string{"api.anthropic.com"}})}
	s := evidenceJSON(out)
	for _, want := range []string{`"egress"`, `"status":"advisory"`, `"api.anthropic.com"`} {
		if !strings.Contains(s, want) {
			t.Fatalf("outcome JSON missing %q: %s", want, s)
		}
	}
	if strings.Contains(s, "enforced") {
		t.Fatalf("host egress evidence must never claim enforcement: %s", s)
	}
	// Absent config records nothing (today's behavior regression).
	if egressEvidence(EgressConfig{}) != nil {
		t.Fatal("absent egress must not appear in outcome.json")
	}
}

// TestEgressDocsStateAdvisoryOnly is the AC3 doc-assert: the README documents
// egress as advisory/bypassable and never as a host security boundary.
func TestEgressDocsStateAdvisoryOnly(t *testing.T) {
	readme, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Fatalf("read README: %v", err)
	}
	s := string(readme)
	if !strings.Contains(s, "advisory only (#103)") {
		t.Fatal("README must document the advisory egress block (#103)")
	}
	for _, want := range []string{"bypassable", "not a security boundary"} {
		if !strings.Contains(s, want) {
			t.Fatalf("README egress docs must state %q", want)
		}
	}
	if strings.Contains(s, `"status": "enforced"`) {
		t.Fatal("README must not claim enforced host egress")
	}
}

func containsEnv(env []string, want string) bool {
	for _, kv := range env {
		if kv == want {
			return true
		}
	}
	return false
}

func countEnv(env []string, prefix string) int {
	n := 0
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			n++
		}
	}
	return n
}
