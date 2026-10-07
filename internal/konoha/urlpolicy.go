package konoha

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strings"
)

// Outbound-request (SSRF) URL policy + referenced secret names (#122). Konoha
// runs agents and validation gates directly on the operator host, so it cannot
// intercept a child's sockets: a per-repo allow/deny policy for child egress is
// therefore ADVISORY on the host (recorded, bypassable — exactly like the #103
// egress proxy) and becomes ENFORCED only inside a sandbox (#PL5/#92). Konoha
// never labels the host policy "enforced" (§40).
//
// The policy is resolved per repo (repos.yaml `lifecycle.urlPolicy`) and the
// resolved policy + the NAMES (never values) of the secret references a run
// declared are recorded in outcome.json. Credential resolution is delegated to
// #CU1; this file neither reads nor stores a secret value.

// URLPolicy is one `lifecycle.urlPolicy` block: allow/deny lists of hostnames
// (`*.example.com`) or CIDRs (`10.0.0.0/8`). An absent/empty policy is valid and
// means today's behavior (no URL policy). Deny wins; a non-empty allow list
// implies default-deny for hosts that match neither list.
type URLPolicy struct {
	Allow []string `yaml:"allow" json:"allow,omitempty"`
	Deny  []string `yaml:"deny" json:"deny,omitempty"`
}

// Configured reports whether any rule is set.
func (p URLPolicy) Configured() bool { return len(p.Allow) > 0 || len(p.Deny) > 0 }

// validate gates a URL policy block: entries must be non-empty host globs or
// CIDRs, never a URL (a scheme is a mistake here). A malformed rule fails at
// load (§33), never mid-cycle.
func (p URLPolicy) validate(prefix string) error {
	for _, list := range []struct {
		name    string
		entries []string
	}{{"allow", p.Allow}, {"deny", p.Deny}} {
		for i, e := range list.entries {
			if err := validateURLPolicyEntry(e); err != nil {
				return fmt.Errorf("%s.%s[%d]: %w", prefix, list.name, i, err)
			}
		}
	}
	return nil
}

// validateURLPolicyEntry rejects an empty, URL-shaped or malformed rule. A rule
// is either a valid CIDR or a hostname glob with no scheme/whitespace.
func validateURLPolicyEntry(entry string) error {
	e := strings.TrimSpace(entry)
	if e == "" {
		return errors.New("entries must be non-empty host names or CIDRs")
	}
	if e != entry {
		return fmt.Errorf("%q must not have surrounding whitespace", entry)
	}
	if _, _, err := net.ParseCIDR(e); err == nil {
		return nil
	}
	if net.ParseIP(e) != nil {
		return nil // a bare IP is allowed (matches itself)
	}
	if strings.Contains(e, "://") {
		return fmt.Errorf("%q must be a host/CIDR, not a URL", entry)
	}
	if strings.ContainsAny(e, " \t\n/") {
		return fmt.Errorf("%q must not contain whitespace or a path separator", entry)
	}
	if strings.ContainsAny(e, "[]:") {
		return fmt.Errorf("%q is not a valid host name or CIDR", entry)
	}
	return nil
}

// EffectiveURLPolicy resolves a repo's outbound URL policy. There is no global
// default (the ticket scope is per-repo), so this is the repo's block verbatim.
func EffectiveURLPolicy(lc LifecycleConfig) URLPolicy { return lc.URLPolicy }

// hostMatches reports whether host is covered by one policy rule. CIDR rules
// match only an IP-literal host; otherwise the rule is a case-insensitive host
// glob (`*`/`?`, reusing the §20 segMatch).
func hostMatches(rule, host string) bool {
	h := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	r := strings.ToLower(strings.TrimSpace(rule))
	if r == "" || h == "" {
		return false
	}
	if _, ipnet, err := net.ParseCIDR(r); err == nil {
		ip := net.ParseIP(h)
		return ip != nil && ipnet.Contains(ip)
	}
	return segMatch(r, h)
}

// EvaluateURLPolicy is the pure, deterministic decision: deny wins over allow,
// and a non-empty allow list means default-deny. Returns the verdict and a short
// reason for evidence. An unconfigured policy allows everything.
func EvaluateURLPolicy(p URLPolicy, host string) (allowed bool, reason string) {
	for _, r := range p.Deny {
		if hostMatches(r, host) {
			return false, fmt.Sprintf("denied by rule %q", r)
		}
	}
	if len(p.Allow) == 0 {
		return true, ""
	}
	for _, r := range p.Allow {
		if hostMatches(r, host) {
			return true, ""
		}
	}
	return false, "not in allow list"
}

// outboundEnforceable reports whether a child's outbound requests can actually
// be blocked on this host. It is false today: enforcement requires a sandbox
// (docs/non-goals.md A4, #PL5/#92). Overridden in tests to exercise the
// enforceable branch.
var outboundEnforceable = func() bool { return false }

// GuardOutbound evaluates an outbound host against the policy. It fails closed
// ONLY where enforcement is possible (a sandbox); on the host a denial is
// advisory — nil is returned and the caller records it (never a silent block
// claim). Deny wins via EvaluateURLPolicy.
func GuardOutbound(p URLPolicy, host string) error {
	allowed, reason := EvaluateURLPolicy(p, host)
	if allowed {
		return nil
	}
	if !outboundEnforceable() {
		return nil
	}
	return fmt.Errorf("outbound host %q denied by URL policy: %s", host, reason)
}

// RequireHTTPSOrLoopback is the host-enforceable SSRF guard for a URL Konoha
// itself constructs or dials: it must be absolute with no userinfo/fragment, and
// must be HTTPS unless it targets a loopback host. (Mirrors Omnara's urlpolicy.)
func RequireHTTPSOrLoopback(rawURL string) error {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return fmt.Errorf("parse URL: %w", err)
	}
	if u.Hostname() == "" || u.Opaque != "" {
		return errors.New("URL must be absolute and include a host")
	}
	if u.User != nil {
		return errors.New("URL must not include user information")
	}
	if u.Fragment != "" {
		return errors.New("URL must not include a fragment")
	}
	if strings.EqualFold(u.Scheme, "https") {
		return nil
	}
	if strings.EqualFold(u.Scheme, "http") && isLoopbackHost(u.Hostname()) {
		return nil
	}
	return errors.New("URL must use HTTPS unless it uses a loopback host")
}

// Outbound evidence statuses. There is deliberately no host value other than
// "advisory": enforcement is not achievable for a host child, so no host code
// path may ever record "enforced" (§40).
const (
	outboundAdvisory = "advisory"
	outboundEnforced = "enforced"
)

// URLPolicyEvidence is the record: the resolved rules plus the enforcement
// status. On the host status is always "advisory" (bypassable), never a claim.
type URLPolicyEvidence struct {
	Status string   `json:"status"` // advisory | enforced
	Reason string   `json:"reason,omitempty"`
	Allow  []string `json:"allow,omitempty"`
	Deny   []string `json:"deny,omitempty"`
}

// urlPolicyEvidence projects a repo's URL policy into its outcome evidence.
// Returns nil when unconfigured (today's behavior). Pure over the policy + the
// process-fixed enforcement capability, so it is safe to call from workflow code.
func urlPolicyEvidence(p URLPolicy) *URLPolicyEvidence {
	if !p.Configured() {
		return nil
	}
	ev := &URLPolicyEvidence{
		Allow: append([]string(nil), p.Allow...),
		Deny:  append([]string(nil), p.Deny...),
	}
	if outboundEnforceable() {
		ev.Status = outboundEnforced
		return ev
	}
	ev.Status = outboundAdvisory
	ev.Reason = "outbound URL policy is advisory on the host (bypassable); enforcement requires a sandbox (#92)"
	return ev
}

// secretRefSchemes is the closed set of #CU1 secret reference schemes. A
// reference names a credential; its value is resolved out-of-process and is
// never read, recorded, or prompted here.
var secretRefSchemes = map[string]bool{"op": true, "vault": true, "env": true, "file": true}

// validateSecretRef gates one secret reference NAME: `<scheme>://<path>` with a
// known scheme and a non-empty path. The value is never part of the reference.
func validateSecretRef(ref string) error {
	r := strings.TrimSpace(ref)
	if r == "" {
		return errors.New("must be non-empty")
	}
	if r != ref {
		return fmt.Errorf("%q must not have surrounding whitespace", ref)
	}
	i := strings.Index(r, "://")
	if i <= 0 {
		return fmt.Errorf("%q must be a reference of the form op://…, vault://…, env://… or file://…", ref)
	}
	scheme := strings.ToLower(r[:i])
	if !secretRefSchemes[scheme] {
		return fmt.Errorf("unknown scheme %q (known: env, file, op, vault)", scheme)
	}
	if strings.TrimSpace(r[i+3:]) == "" {
		return fmt.Errorf("%q reference path must not be empty", ref)
	}
	return nil
}

// OutboundSecretNames returns the sorted, de-duplicated secret reference NAMES a
// repo declared. It records names only — never a resolved value.
func OutboundSecretNames(lc LifecycleConfig) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(lc.SecretNames))
	for _, s := range lc.SecretNames {
		n := strings.TrimSpace(s)
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	if len(out) == 0 {
		return nil
	}
	sort.Strings(out)
	return out
}
