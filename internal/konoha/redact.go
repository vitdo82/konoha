package konoha

import (
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
)

// redacted is the single replacement marker for every masked secret. It is a
// fixed contract string (operators/tests grep for it): never reword it.
const redacted = "[REDACTED]"

// minSecretLen is the shortest registered value the redactor will mask. Below
// this a "secret" is more likely to collide with ordinary text than to protect
// anything, so it is ignored.
const minSecretLen = 4

// Redactor masks registered secret values and credential-shaped patterns from
// text before it is persisted, logged, or returned in an error (§23/§27). It is
// safe for concurrent use: the worker registers at bootstrap while activities
// redact. The zero value is ready to use.
type Redactor struct {
	mu      sync.Mutex
	secrets []string // registered values, masked longest-first
}

// Add registers secret values to mask. Values shorter than minSecretLen are
// ignored and duplicates are dropped. Longest-first ordering means a longer
// secret is never partially masked by one of its shorter prefixes.
func (r *Redactor) Add(secrets ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range secrets {
		if len(s) < minSecretLen {
			continue
		}
		if slices.Contains(r.secrets, s) {
			continue
		}
		r.secrets = append(r.secrets, s)
	}
	sort.Slice(r.secrets, func(i, j int) bool {
		if len(r.secrets[i]) != len(r.secrets[j]) {
			return len(r.secrets[i]) > len(r.secrets[j])
		}
		return r.secrets[i] < r.secrets[j]
	})
}

// Redact returns s with every registered value and credential-shaped pattern
// replaced by [REDACTED]. Best-effort but always runs; it never returns raw
// registered values.
func (r *Redactor) Redact(s string) string {
	r.mu.Lock()
	secrets := append([]string(nil), r.secrets...)
	r.mu.Unlock()
	for _, sec := range secrets {
		s = strings.ReplaceAll(s, sec, redacted)
	}
	for _, rule := range redactionRules {
		s = rule.re.ReplaceAllString(s, rule.repl)
	}
	return s
}

// redactionRule is one pattern rule: a match span and its replacement.
type redactionRule struct {
	re   *regexp.Regexp
	repl string
}

// redactionRules are the credential-shaped patterns masked regardless of whether
// the value was registered. Order matters: a broader rule may consume a span a
// later rule would otherwise match.
var redactionRules = []redactionRule{
	// Authorization: <anything to end of line>
	{regexp.MustCompile(`(?i)(authorization\s*:\s*)[^\r\n]+`), "${1}" + redacted},
	// bearer <token>
	{regexp.MustCompile(`(?i)(\bbearer\s+)[A-Za-z0-9._~+/=-]+`), "${1}" + redacted},
	// OpenAI-style keys: sk-...
	{regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{8,}`), redacted},
	// GitHub tokens: ghp_/gho_/ghu_/ghs_/ghr_
	{regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{16,}`), redacted},
	// Slack tokens: xox[baprs]-...
	{regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{8,}`), redacted},
	// AWS access key IDs
	{regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`), redacted},
	// Google API keys: AIza + 35 chars
	{regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}\b`), redacted},
	// URL userinfo: scheme://user:pass@ -> scheme://user:[REDACTED]@
	{regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://[^/\s:@]+:)[^/\s@]+@`), "${1}" + redacted + "@"},
	// *_PASSWORD= / *_TOKEN= / *_SECRET= assignments (keep the var name). The
	// leading boundary excludes a URL userinfo (`...-token:...`): the url rule
	// above already masked the password, and matching here would eat the host.
	{regexp.MustCompile(`(?i)(^|[\s"'(,;])([A-Z0-9_]*(?:PASSWORD|TOKEN|SECRET)[A-Z0-9_]*\s*[=:]\s*)\S+`), "${1}${2}" + redacted},
}

// defaultRedactor is the process-wide redactor every durable/log boundary uses.
var defaultRedactor Redactor

// RegisterSecrets adds values to the process-wide redactor. Worker bootstrap
// registers operator-provisioned env; child-env value injection (#CU1)
// registers a resolved value for the duration of a run.
func RegisterSecrets(secrets ...string) { defaultRedactor.Add(secrets...) }

// Redact masks secrets in the process-wide redactor.
func Redact(s string) string { return defaultRedactor.Redact(s) }

// registerOperatorSecrets registers the operator-provisioned credential env
// values (never the App PEM file contents, §23) at worker bootstrap.
func registerOperatorSecrets() {
	RegisterSecrets(
		os.Getenv("GH_TOKEN"),
		os.Getenv("GITHUB_TOKEN"),
		os.Getenv("GH_ENTERPRISE_TOKEN"),
		os.Getenv("GH_WEBHOOK_SECRET"),
	)
}
