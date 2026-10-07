package konoha

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

// Model-provider protocol abstraction (#118). Konoha normally runs agents via an
// external harness (harness.go). This layer is the wire-level foundation: a
// canonical Envelope, one adapter per protocol, gateway presets, typed transport
// retry, and credentials-by-name. It is NOT wired into the agent path yet — it
// feeds agent profiles (#PL1) and the secrets store (#PL6).

// Protocol wire formats. Closed set; an unknown value fails at load.
const (
	protocolOpenAIResponses   = "openai-responses"
	protocolOpenAIChat        = "openai-chat"
	protocolAnthropicMessages = "anthropic-messages"
)

var knownProtocols = []string{protocolAnthropicMessages, protocolOpenAIChat, protocolOpenAIResponses}

// knownGateways maps a gateway name to its OpenAI-chat base URL preset.
var knownGateways = map[string]string{
	"openrouter": "https://openrouter.ai/api/v1",
	"litellm":    "http://127.0.0.1:4000/v1",
	"ollama":     "http://127.0.0.1:11434/v1",
}

func knownProtocol(p string) bool {
	for _, k := range knownProtocols {
		if p == k {
			return true
		}
	}
	return false
}

func knownGateway(g string) bool { _, ok := knownGateways[g]; return ok }

func sortedGatewayNames() []string {
	out := make([]string, 0, len(knownGateways))
	for k := range knownGateways {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Message roles in the canonical envelope.
const (
	roleSystem    = "system"
	roleUser      = "user"
	roleAssistant = "assistant"
	roleTool      = "tool"
)

// Message is one canonical conversation turn.
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"toolCalls,omitempty"`
	ToolCallID string     `json:"toolCallId,omitempty"`
	Name       string     `json:"name,omitempty"`
}

// ToolDef declares a callable tool.
type ToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// ToolCall is one tool invocation in a response.
type ToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// Envelope carries a canonical request and, after Invoke, the response. No
// provider/wire type leaks past this boundary. Usage is the #87 canonical type
// so a response feeds the §39 sink with no translation.
type Envelope struct {
	Model       string    `json:"model"`
	System      string    `json:"system,omitempty"`
	Messages    []Message `json:"messages,omitempty"`
	Tools       []ToolDef `json:"tools,omitempty"`
	MaxTokens   int       `json:"maxTokens,omitempty"`
	Temperature *float64  `json:"temperature,omitempty"`

	Text      string       `json:"text,omitempty"`
	ToolCalls []ToolCall   `json:"toolCalls,omitempty"`
	Stop      string       `json:"stop,omitempty"`
	Usage     HarnessUsage `json:"usage,omitempty"`
}

// ModelProvider is one named provider instance. Protocol is the wire format.
type ModelProvider interface {
	Name() string
	Protocol() string
	Invoke(ctx context.Context, req Envelope) (Envelope, error)
}

// ProviderConfig is one `defaults.providers.<name>` entry. APIKeyEnv is a NAME.
type ProviderConfig struct {
	Protocol  string `yaml:"protocol" json:"protocol,omitempty"`
	Gateway   string `yaml:"gateway" json:"gateway,omitempty"`
	BaseURL   string `yaml:"baseUrl" json:"baseUrl,omitempty"`
	APIKeyEnv string `yaml:"apiKeyEnv" json:"apiKeyEnv,omitempty"`
}

// effectiveProtocol resolves the wire format (a gateway implies OpenAI-chat).
func (p ProviderConfig) effectiveProtocol() string {
	if proto := strings.TrimSpace(p.Protocol); proto != "" {
		return proto
	}
	return protocolOpenAIChat
}

// baseURL resolves the endpoint: explicit override, else the gateway preset,
// else the protocol default. Never a credential.
func (p ProviderConfig) baseURL() (string, error) {
	if b := strings.TrimSpace(p.BaseURL); b != "" {
		return strings.TrimRight(b, "/"), nil
	}
	if gw := strings.ToLower(strings.TrimSpace(p.Gateway)); gw != "" {
		if u, ok := knownGateways[gw]; ok {
			return u, nil
		}
		return "", fmt.Errorf("unknown gateway %q", gw)
	}
	switch p.effectiveProtocol() {
	case protocolOpenAIResponses, protocolOpenAIChat:
		return "https://api.openai.com/v1", nil
	case protocolAnthropicMessages:
		return "https://api.anthropic.com", nil
	}
	return "", fmt.Errorf("no default base URL for protocol %q", p.effectiveProtocol())
}

// validate gates one provider entry at load. Unknown protocol/gateway, a
// gateway/protocol mismatch, a non-http(s) base URL, or a non-bare apiKeyEnv
// fails fast.
func (p ProviderConfig) validate(prefix string) error {
	proto := strings.TrimSpace(p.Protocol)
	gw := strings.ToLower(strings.TrimSpace(p.Gateway))
	if proto == "" && gw == "" {
		return fmt.Errorf("%s: protocol or gateway is required", prefix)
	}
	if proto != "" && !knownProtocol(proto) {
		return fmt.Errorf("%s.protocol: unknown protocol %q (known: %s)", prefix, p.Protocol, strings.Join(knownProtocols, ", "))
	}
	if gw != "" && !knownGateway(gw) {
		return fmt.Errorf("%s.gateway: unknown gateway %q (known: %s)", prefix, p.Gateway, strings.Join(sortedGatewayNames(), ", "))
	}
	if gw != "" && proto != "" && proto != protocolOpenAIChat {
		return fmt.Errorf("%s: gateway %q is OpenAI-chat compatible; protocol %q is not", prefix, p.Gateway, p.Protocol)
	}
	if b := strings.TrimSpace(p.BaseURL); b != "" {
		u, err := url.Parse(b)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("%s.baseUrl: %q must be an http(s) URL", prefix, p.BaseURL)
		}
	}
	if p.APIKeyEnv != strings.TrimSpace(p.APIKeyEnv) || strings.ContainsAny(p.APIKeyEnv, " \t\n") {
		return fmt.Errorf("%s.apiKeyEnv: %q must be a bare env var NAME", prefix, p.APIKeyEnv)
	}
	return nil
}

// ProviderDescriptor is the persisted, non-secret identity of a provider: the
// apiKeyEnv NAME only, never the value.
type ProviderDescriptor struct {
	Name      string `json:"name"`
	Protocol  string `json:"protocol"`
	BaseURL   string `json:"baseUrl,omitempty"`
	APIKeyEnv string `json:"apiKeyEnv,omitempty"`
}

// descriptor projects the provider identity. The secret value is never read.
func (p ProviderConfig) descriptor(name string) ProviderDescriptor {
	base, _ := p.baseURL()
	return ProviderDescriptor{Name: name, Protocol: p.effectiveProtocol(), BaseURL: base, APIKeyEnv: p.APIKeyEnv}
}

// SecretResolver resolves a credential NAME to its value at Invoke time only.
// The default reads the env var `name`; implementers may back this by the future
// secrets store (#PL6). A value is never logged, recorded, or placed in a prompt.
type SecretResolver interface {
	Resolve(name string) (string, error)
}

// envSecretResolver resolves a credential NAME from the process environment.
type envSecretResolver struct{}

func (envSecretResolver) Resolve(name string) (string, error) {
	n := strings.TrimSpace(name)
	if n == "" {
		return "", fmt.Errorf("secret name is empty")
	}
	v := os.Getenv(n)
	if strings.TrimSpace(v) == "" {
		return "", fmt.Errorf("secret %q is not set", n)
	}
	RegisterSecrets(v) // #82: mask the value everywhere it might surface
	return v, nil
}

// transportError means the request never produced a response — a retryable
// infrastructure fault.
type transportError struct{ err error }

func (e *transportError) Error() string { return "provider transport: " + e.err.Error() }
func (e *transportError) Unwrap() error { return e.err }

// httpStatusError is a non-2xx provider response. 5xx is retryable; 4xx is a
// policy/model error and fails closed without a retry.
type httpStatusError struct {
	Status int
	Body   string
}

func (e *httpStatusError) Error() string {
	return fmt.Sprintf("provider HTTP %d: %s", e.Status, truncate(strings.TrimSpace(e.Body), 200))
}

// isRetryable reports the #95 retry class of a provider error: transport faults
// and 5xx are retryable; everything else (incl. 4xx model/policy errors) is not.
func isRetryable(err error) bool {
	var se *httpStatusError
	if errors.As(err, &se) {
		return se.Status >= 500
	}
	var te *transportError
	return errors.As(err, &te)
}

// retryingProvider decorates any provider with the #95 retry contract. Sleep is
// injectable so tests never wait.
type retryingProvider struct {
	inner    ModelProvider
	attempts int
	base     time.Duration
	sleep    func(context.Context, time.Duration) error
}

func (r *retryingProvider) Name() string     { return r.inner.Name() }
func (r *retryingProvider) Protocol() string { return r.inner.Protocol() }

func (r *retryingProvider) Invoke(ctx context.Context, req Envelope) (Envelope, error) {
	attempts := r.attempts
	if attempts < 1 {
		attempts = 1
	}
	var last error
	for attempt := 1; attempt <= attempts; attempt++ {
		resp, err := r.inner.Invoke(ctx, req)
		if err == nil {
			return resp, nil
		}
		if !isRetryable(err) {
			return Envelope{}, err
		}
		last = err
		if attempt == attempts {
			break
		}
		if r.sleep != nil {
			if err := r.sleep(ctx, r.base<<(attempt-1)); err != nil {
				return Envelope{}, err
			}
		}
	}
	return Envelope{}, last
}

// sleepCtx waits d or until ctx is done.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// providerRegistry is a name-keyed set of built provider instances.
type providerRegistry struct {
	byName map[string]ModelProvider
}

// newProviderRegistry validates and builds every configured provider, wrapping
// each in the #95 retry decorator. An unknown protocol/gateway fails closed.
// client may be nil (http.DefaultClient). attempts defaults to 1 when < 1.
func newProviderRegistry(cfg map[string]ProviderConfig, secrets SecretResolver, client *http.Client) (*providerRegistry, error) {
	r := &providerRegistry{byName: make(map[string]ModelProvider, len(cfg))}
	attempts := appCfg.Worker.infraAttempts()
	for name, pc := range cfg {
		if err := pc.validate("defaults.providers." + name); err != nil {
			return nil, err
		}
		base, err := pc.baseURL()
		if err != nil {
			return nil, fmt.Errorf("defaults.providers.%s: %w", name, err)
		}
		var inner ModelProvider
		switch pc.effectiveProtocol() {
		case protocolOpenAIResponses:
			inner = &openAIResponsesProvider{name: name, baseURL: base, apiKeyEnv: pc.APIKeyEnv, secrets: secrets, client: client}
		case protocolOpenAIChat:
			inner = &openAIChatProvider{name: name, baseURL: base, apiKeyEnv: pc.APIKeyEnv, secrets: secrets, client: client}
		case protocolAnthropicMessages:
			inner = &anthropicMessagesProvider{name: name, baseURL: base, apiKeyEnv: pc.APIKeyEnv, secrets: secrets, client: client}
		default:
			return nil, fmt.Errorf("defaults.providers.%s: unknown protocol %q", name, pc.Protocol)
		}
		r.byName[name] = &retryingProvider{inner: inner, attempts: attempts, base: time.Second, sleep: sleepCtx}
	}
	return r, nil
}

// Lookup resolves a provider by name.
func (r *providerRegistry) Lookup(name string) (ModelProvider, error) {
	p, ok := r.byName[name]
	if !ok {
		return nil, fmt.Errorf("unknown provider %q (known: %s)", name, strings.Join(r.Names(), ", "))
	}
	return p, nil
}

// Names returns the sorted provider names.
func (r *providerRegistry) Names() []string {
	out := make([]string, 0, len(r.byName))
	for n := range r.byName {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
