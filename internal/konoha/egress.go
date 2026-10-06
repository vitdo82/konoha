package konoha

import (
	"fmt"
	"net/url"
	"strings"
)

// Advisory network egress policy (#103). Konoha runs agents and validation
// gates directly on the operator host, so there is no sandbox to enforce a
// network policy (docs/security.md). This is therefore advisory ONLY: when
// configured it sets HTTPS_PROXY/HTTP_PROXY/NO_PROXY on agent and validation
// children so well-behaved tools route through the operator's allowlisting
// proxy. It is BYPASSABLE — a process that ignores the proxy or opens a raw
// socket is not stopped — and is never a security boundary. Real enforcement
// (deny-by-default egress + L7 credential header injection) belongs to the
// container/microVM executor (#92).
//
// Egress is process-global (konoha.yaml `egress:`), resolved once at bootstrap
// like the rest of appCfg, so it is replay-safe.

// EgressConfig is the optional advisory `egress:` block. An absent/empty proxy
// means today's behavior: no proxy env is set on any child.
type EgressConfig struct {
	Proxy    string   `yaml:"proxy" json:"proxy,omitempty"`
	AllowOut []string `yaml:"allowOut" json:"allowOut,omitempty"`
}

// Configured reports whether an advisory proxy is set.
func (e EgressConfig) Configured() bool { return strings.TrimSpace(e.Proxy) != "" }

// validate gates the egress block: a set proxy must be an http(s) URL and
// allowOut entries must be non-empty. An absent block is valid (no egress).
func (e EgressConfig) validate(prefix string) error {
	p := strings.TrimSpace(e.Proxy)
	if p != "" {
		u, err := url.Parse(p)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("%s.proxy: %q must be an http(s) URL", prefix, e.Proxy)
		}
	}
	for _, h := range e.AllowOut {
		if strings.TrimSpace(h) == "" {
			return fmt.Errorf("%s.allowOut: entries must be non-empty host names", prefix)
		}
	}
	return nil
}

// egressAdvisory is the only status the host path records. There is deliberately
// no "enforced" value here: enforcement is not achievable on the host, so no
// host code path may ever record it (§40, never a silent over-claim).
const egressAdvisory = "advisory"

// EgressEvidence is the outcome.json record: advisory only, plus the configured
// proxy + allowlist, so evidence can never be read as enforcement.
type EgressEvidence struct {
	Status   string   `json:"status"` // advisory
	Proxy    string   `json:"proxy,omitempty"`
	AllowOut []string `json:"allowOut,omitempty"`
}

// egressEvidence projects the configured egress into its advisory outcome
// evidence. Returns nil when unconfigured (today's behavior). Pure over
// process-fixed config, so it is safe to call from workflow code.
func egressEvidence(e EgressConfig) *EgressEvidence {
	if !e.Configured() {
		return nil
	}
	return &EgressEvidence{
		Status:   egressAdvisory,
		Proxy:    strings.TrimSpace(e.Proxy),
		AllowOut: append([]string(nil), e.AllowOut...),
	}
}

// egressEnv applies the advisory proxy env vars to an already-scrubbed child
// env. Unconfigured means env is returned unchanged (today's behavior). NO_PROXY
// keeps loopback off the proxy so local tooling (and the proxy itself) still
// works. This shapes well-behaved tools only; it is not a boundary.
func egressEnv(env []string, e EgressConfig) []string {
	if !e.Configured() {
		return env
	}
	p := strings.TrimSpace(e.Proxy)
	env = setEnv(env, "HTTPS_PROXY", p)
	env = setEnv(env, "HTTP_PROXY", p)
	env = setEnv(env, "NO_PROXY", "localhost,127.0.0.1,::1")
	return env
}

// setEnv sets key=value in env, replacing an existing entry (so config wins over
// an inherited value) and appending otherwise.
func setEnv(env []string, key, value string) []string {
	prefix := key + "="
	for i, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			env[i] = prefix + value
			return env
		}
	}
	return append(env, prefix+value)
}
