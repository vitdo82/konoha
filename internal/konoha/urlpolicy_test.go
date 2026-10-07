package konoha

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// #122: outbound-request (SSRF) URL policy + referenced secret names — config
// validation, the pure allow/deny engine, the advisory-vs-enforced contract, and
// the "names only, never a value" guarantee for evidence.

func TestURLPolicyValidate(t *testing.T) {
	good := []URLPolicy{
		{},
		{Allow: []string{"api.anthropic.com", "*.example.com"}},
		{Deny: []string{"10.0.0.0/8", "169.254.169.254", "::1"}},
		{Allow: []string{"registry.npmjs.org"}, Deny: []string{"metadata.internal"}},
	}
	for _, p := range good {
		if err := p.validate("lifecycle.urlPolicy"); err != nil {
			t.Fatalf("valid URLPolicy rejected: %+v: %v", p, err)
		}
	}
	bad := []URLPolicy{
		{Allow: []string{""}},
		{Allow: []string{"  "}},
		{Deny: []string{"https://api.example.com"}},
		{Allow: []string{"api.example.com/path"}},
		{Deny: []string{"bad host"}},
		{Allow: []string{"[::1"}},
	}
	for _, p := range bad {
		if err := p.validate("lifecycle.urlPolicy"); err == nil {
			t.Fatalf("expected rejection for %+v", p)
		}
	}
}

func TestURLPolicyDenyWins(t *testing.T) {
	p := URLPolicy{Allow: []string{"api.anthropic.com"}, Deny: []string{"api.anthropic.com"}}
	allowed, reason := EvaluateURLPolicy(p, "api.anthropic.com")
	if allowed {
		t.Fatalf("deny must win over allow, reason=%q", reason)
	}
}

func TestURLPolicyAllowListDefaultDeny(t *testing.T) {
	p := URLPolicy{Allow: []string{"api.anthropic.com"}}
	if ok, _ := EvaluateURLPolicy(p, "api.anthropic.com"); !ok {
		t.Fatal("allow-listed host must be allowed")
	}
	if ok, _ := EvaluateURLPolicy(p, "evil.example.com"); ok {
		t.Fatal("a non-empty allow list must default-deny an unmatched host")
	}
}

func TestURLPolicyCIDRAndGlob(t *testing.T) {
	cidr := URLPolicy{Deny: []string{"10.0.0.0/8"}}
	if ok, _ := EvaluateURLPolicy(cidr, "10.1.2.3"); ok {
		t.Fatal("CIDR deny must match an in-range IP literal")
	}
	if ok, _ := EvaluateURLPolicy(cidr, "11.1.2.3"); !ok {
		t.Fatal("out-of-range IP must not match the CIDR")
	}
	glob := URLPolicy{Allow: []string{"*.example.com"}}
	if ok, _ := EvaluateURLPolicy(glob, "api.example.com"); !ok {
		t.Fatal("host glob must match a subdomain")
	}
	if ok, _ := EvaluateURLPolicy(glob, "evil.com"); ok {
		t.Fatal("host glob must not match an unrelated host")
	}
}

func TestURLPolicyEmptyAllowsAll(t *testing.T) {
	if ok, _ := EvaluateURLPolicy(URLPolicy{}, "anything.example.com"); !ok {
		t.Fatal("an absent policy must allow all (today's behavior)")
	}
}

// TestGuardOutboundAdvisoryOnHost is AC1/AC2: a denied host is NOT blocked on
// the host (advisory, bypassable) — the denial is recorded, never enforced.
func TestGuardOutboundAdvisoryOnHost(t *testing.T) {
	saved := outboundEnforceable
	defer func() { outboundEnforceable = saved }()
	outboundEnforceable = func() bool { return false }

	p := URLPolicy{Deny: []string{"blocked.example.com"}}
	if err := GuardOutbound(p, "blocked.example.com"); err != nil {
		t.Fatalf("host denial must be advisory, got error: %v", err)
	}
}

// TestGuardOutboundBlockedWhereEnforceable is AC1: where enforcement is possible
// (a sandbox) a denied host fails closed.
func TestGuardOutboundBlockedWhereEnforceable(t *testing.T) {
	saved := outboundEnforceable
	defer func() { outboundEnforceable = saved }()
	outboundEnforceable = func() bool { return true }

	p := URLPolicy{Deny: []string{"blocked.example.com"}}
	if err := GuardOutbound(p, "blocked.example.com"); err == nil {
		t.Fatal("enforceable denial must fail closed")
	}
	if err := GuardOutbound(p, "allowed.example.com"); err != nil {
		t.Fatalf("non-denied host must pass, got %v", err)
	}
}

func TestRequireHTTPSOrLoopback(t *testing.T) {
	ok := []string{"https://api.example.com/x", "http://127.0.0.1:8080/x", "http://localhost/x", "http://[::1]:9/x"}
	for _, u := range ok {
		if err := RequireHTTPSOrLoopback(u); err != nil {
			t.Fatalf("%q must be allowed: %v", u, err)
		}
	}
	bad := []string{"http://api.example.com", "https://user:pass@api.example.com", "https://api.example.com/x#f", "/relative", "mailto:foo"}
	for _, u := range bad {
		if err := RequireHTTPSOrLoopback(u); err == nil {
			t.Fatalf("%q must be rejected", u)
		}
	}
}

// TestURLPolicyEvidenceAdvisoryOnly is AC2: the host records advisory (never
// "enforced") + the resolved rules, and an unset policy records nothing.
func TestURLPolicyEvidenceAdvisoryOnly(t *testing.T) {
	saved := outboundEnforceable
	defer func() { outboundEnforceable = saved }()
	outboundEnforceable = func() bool { return false }

	if ev := urlPolicyEvidence(URLPolicy{}); ev != nil {
		t.Fatalf("unset policy must record nothing, got %+v", ev)
	}
	ev := urlPolicyEvidence(URLPolicy{Allow: []string{"api.anthropic.com"}, Deny: []string{"10.0.0.0/8"}})
	if ev == nil {
		t.Fatal("configured policy must record evidence")
	}
	if ev.Status != outboundAdvisory {
		t.Fatalf("host policy must record %q, got %q", outboundAdvisory, ev.Status)
	}
	if len(ev.Allow) != 1 || len(ev.Deny) != 1 {
		t.Fatalf("resolved rules not recorded: %+v", ev)
	}
	s := evidenceJSON(CycleOutcome{URLPolicy: ev})
	if !strings.Contains(s, `"urlPolicy"`) || !strings.Contains(s, `"status":"advisory"`) {
		t.Fatalf("outcome JSON missing advisory policy: %s", s)
	}
	if strings.Contains(s, "enforced") {
		t.Fatalf("host URL policy evidence must never claim enforcement: %s", s)
	}
}

func TestOutboundSecretNamesDedupSorted(t *testing.T) {
	got := OutboundSecretNames(LifecycleConfig{SecretNames: []string{"op://vault/b", "env://A", "op://vault/b", " env://A "}})
	want := []string{"env://A", "op://vault/b"}
	if len(got) != len(want) {
		t.Fatalf("want %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("want %v, got %v", want, got)
		}
	}
	if OutboundSecretNames(LifecycleConfig{}) != nil {
		t.Fatal("no references must record nothing")
	}
}

func TestValidateSecretRef(t *testing.T) {
	for _, s := range []string{"op://vault/key", "vault://secret/x", "env://ANTHROPIC_API_KEY", "file:///etc/k"} {
		if err := validateSecretRef(s); err != nil {
			t.Fatalf("%q must be valid: %v", s, err)
		}
	}
	for _, s := range []string{"", " ANTHROPIC_API_KEY", "ANTHROPIC_API_KEY", "aws://x", "env://", "op//vault"} {
		if err := validateSecretRef(s); err == nil {
			t.Fatalf("%q must be rejected", s)
		}
	}
}

// TestSecretNamesRecordedNoValueLeak is AC3/AC4: the reference NAME lands in
// evidence and the resolved VALUE never does. The value is never read here.
func TestSecretNamesRecordedNoValueLeak(t *testing.T) {
	const secret = "sk-super-secret-value-1234567890"
	t.Setenv("KONOHA_TEST_PROVIDER_KEY", secret)

	lc := LifecycleConfig{SecretNames: []string{"env://KONOHA_TEST_PROVIDER_KEY"}}
	out := CycleOutcome{
		URLPolicy:   urlPolicyEvidence(URLPolicy{Deny: []string{"metadata.internal"}}),
		SecretNames: OutboundSecretNames(lc),
	}
	s := evidenceJSON(out)
	if !strings.Contains(s, "env://KONOHA_TEST_PROVIDER_KEY") {
		t.Fatalf("referenced secret NAME must be recorded: %s", s)
	}
	if strings.Contains(s, secret) {
		t.Fatalf("secret VALUE leaked into evidence: %s", s)
	}
}

func TestReposConfigRejectsBadURLPolicy(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "repos.yaml")
	body := "repos:\n  - owner: o\n    repo: r\n    repoPath: /tmp/r\n    lifecycle:\n      urlPolicy:\n        deny: [\"https://internal.example\"]\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadReposConfig(path); err == nil {
		t.Fatal("a URL-shaped deny entry must fail at load")
	}
}

func TestReposConfigRejectsBadSecretRef(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "repos.yaml")
	body := "repos:\n  - owner: o\n    repo: r\n    repoPath: /tmp/r\n    lifecycle:\n      secrets: [\"not-a-reference\"]\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadReposConfig(path); err == nil {
		t.Fatal("a malformed secret reference must fail at load")
	}
}

// TestURLPolicyDocsStateAdvisory is the AC2 doc-assert: the README documents the
// outbound URL policy as advisory/bypassable and never as host enforcement.
func TestURLPolicyDocsStateAdvisory(t *testing.T) {
	readme, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Fatalf("read README: %v", err)
	}
	s := string(readme)
	if !strings.Contains(s, "urlPolicy") {
		t.Fatal("README must document the per-repo urlPolicy block (#122)")
	}
	for _, want := range []string{"advisory", "bypassable", "SSRF"} {
		if !strings.Contains(s, want) {
			t.Fatalf("README urlPolicy docs must state %q", want)
		}
	}
	security, err := os.ReadFile(filepath.Join("..", "..", "docs", "security.md"))
	if err != nil {
		t.Fatalf("read security doc: %v", err)
	}
	if !strings.Contains(string(security), "SSRF") {
		t.Fatal("docs/security.md must record the SSRF/URL policy rationale (#122)")
	}
}
