# Model-provider protocol abstraction (#118) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a standalone `ModelProvider` abstraction — canonical request/response envelope, OpenAI Responses / OpenAI Chat / Anthropic Messages adapters, gateway presets, typed transport retry, and credentials-by-name — validated at config load.

**Architecture:** `modelprovider.go` owns the interface, envelope, config catalog + validation, secret resolver, registry/factory, and typed retry decorator. `modelprovider_adapters.go` owns the three pure request/response translators over an injected `*http.Client`. Config gains `defaults.providers`. Nothing is wired into the agent execution path.

**Tech Stack:** Go 1.27, stdlib `net/http`/`net/http/httptest`, `gopkg.in/yaml.v3`, `testify/require`.

**Spec:** `docs/superpowers/specs/2026-10-07-model-provider-design.md`

---

## File structure

- Create `internal/konoha/modelprovider.go` — interface, envelope, protocol/gateway closed sets, `ProviderConfig` + validate, `SecretResolver`, typed errors, `retryingProvider`, registry/factory.
- Create `internal/konoha/modelprovider_adapters.go` — `doJSON`, `openAIResponsesProvider`, `openAIChatProvider`, `anthropicMessagesProvider`.
- Modify `internal/konoha/appconfig.go` — add `Defaults.Providers`, validate each entry.
- Create `internal/konoha/modelprovider_test.go` — fake-HTTP + unit tests.
- Modify `configs/konoha.yaml`, `README.md`, `AGENTS.md` — docs.

---

### Task 1: Canonical envelope, interface, config catalog + load validation

**Files:**
- Create: `internal/konoha/modelprovider.go`
- Modify: `internal/konoha/appconfig.go`

- [ ] **Step 1: Write the failing test** (in `modelprovider_test.go`)

```go
func TestProviderConfigValidate(t *testing.T) {
	cases := []struct {
		name string
		cfg  ProviderConfig
		ok   bool
	}{
		{"known protocol", ProviderConfig{Protocol: protocolAnthropicMessages}, true},
		{"known gateway", ProviderConfig{Gateway: "openrouter"}, true},
		{"gateway + matching protocol", ProviderConfig{Gateway: "ollama", Protocol: protocolOpenAIChat}, true},
		{"unknown protocol", ProviderConfig{Protocol: "grpc"}, false},
		{"unknown gateway", ProviderConfig{Gateway: "bedrock"}, false},
		{"gateway/protocol mismatch", ProviderConfig{Gateway: "openrouter", Protocol: protocolAnthropicMessages}, false},
		{"neither", ProviderConfig{}, false},
		{"bad base url", ProviderConfig{Protocol: protocolOpenAIChat, BaseURL: "not a url"}, false},
		{"api key env with space", ProviderConfig{Protocol: protocolOpenAIChat, APIKeyEnv: "A KEY"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.validate("defaults.providers.x")
			if tc.ok && err != nil {
				t.Fatalf("want ok, got %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatalf("want error")
			}
		})
	}
}

func TestUnknownProtocolFailsAtLoad(t *testing.T) {
	cfg := defaultAppConfig()
	cfg.Defaults.Providers = map[string]ProviderConfig{"x": {Protocol: "grpc"}}
	if err := cfg.validate(); err == nil {
		t.Fatal("unknown protocol must fail at load")
	}
	cfg.Defaults.Providers = map[string]ProviderConfig{"x": {Gateway: "bedrock"}}
	if err := cfg.validate(); err == nil {
		t.Fatal("unknown gateway must fail at load")
	}
}
```

- [ ] **Step 2: Run it, confirm it fails to compile** — `go test ./internal/konoha/ -run TestProviderConfig -v` → undefined types.

- [ ] **Step 3: Add the core to `modelprovider.go`**

```go
package konoha

import (
	"context"
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
```

Add `encoding/json` to the import block (used by `json.RawMessage`).

- [ ] **Step 4: Wire config** — in `appconfig.go`, add to `DefaultsConfig`:

```go
type DefaultsConfig struct {
	Lifecycle LifecycleDefaults          `yaml:"lifecycle"`
	Agents    AgentsConfig               `yaml:"agents"`
	Providers map[string]ProviderConfig  `yaml:"providers"`
}
```

In `AppConfig.validate()` after `c.Defaults.Agents.validate()`:

```go
for name, pc := range c.Defaults.Providers {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("defaults.providers: empty provider name")
	}
	if err := pc.validate("defaults.providers." + name); err != nil {
		return err
	}
}
```

- [ ] **Step 5: Run** `go test ./internal/konoha/ -run 'TestProviderConfig|TestUnknownProtocol' -v` → PASS.
- [ ] **Step 6: Commit** `git add internal/konoha/modelprovider.go internal/konoha/appconfig.go internal/konoha/modelprovider_test.go && git commit -m "feat(#118): canonical model-provider envelope + config catalog"`

---

### Task 2: Credential-by-name resolution

**Files:** Modify `internal/konoha/modelprovider.go`, `modelprovider_test.go`

- [ ] **Step 1: Failing test**

```go
func TestCredentialResolvedByNameAndMasked(t *testing.T) {
	const secret = "sk-test-super-secret-value"
	t.Setenv("KONOHA_TEST_PROVIDER_KEY", secret)
	r := envSecretResolver{}
	got, err := r.Resolve("KONOHA_TEST_PROVIDER_KEY")
	if err != nil || got != secret {
		t.Fatalf("resolve = %q, %v", got, err)
	}
	// #82: a resolved value is registered with the process redactor.
	if masked := defaultRedactor.Redact("token=" + secret); strings.Contains(masked, secret) {
		t.Fatalf("resolved secret not masked: %q", masked)
	}
	if _, err := (envSecretResolver{}).Resolve("KONOHA_UNSET_KEY_XYZ"); err == nil {
		t.Fatal("unset secret must fail closed")
	}
}

func TestDescriptorNeverCarriesValue(t *testing.T) {
	const secret = "sk-anthropic-secret"
	t.Setenv("ANTHROPIC_API_KEY", secret)
	d := ProviderConfig{Protocol: protocolAnthropicMessages, APIKeyEnv: "ANTHROPIC_API_KEY"}.descriptor("anthropic")
	b, _ := json.Marshal(d)
	if strings.Contains(string(b), secret) {
		t.Fatalf("descriptor leaked the value: %s", b)
	}
	if !strings.Contains(string(b), "ANTHROPIC_API_KEY") {
		t.Fatalf("descriptor must record the NAME: %s", b)
	}
}
```

- [ ] **Step 2: Run** → undefined `envSecretResolver`.
- [ ] **Step 3: Implement**

```go
// SecretResolver resolves a credential NAME to its value at Invoke time only.
// The default reads the env var `name`; implementers may back this by the
// future secrets store (#PL6). A value is never logged, recorded, or prompted.
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
```

- [ ] **Step 4: Run** → PASS. **Step 5: Commit** `... -m "feat(#118): resolve provider credentials by name, mask with the redactor"`

---

### Task 3: Typed retry decorator (#95)

**Files:** Modify `modelprovider.go`, `modelprovider_test.go`

- [ ] **Step 1: Failing test**

```go
type countingProvider struct {
	protocol string
	err      error
	calls    int
}

func (c *countingProvider) Name() string     { return "counting" }
func (c *countingProvider) Protocol() string { return c.protocol }
func (c *countingProvider) Invoke(context.Context, Envelope) (Envelope, error) {
	c.calls++
	return Envelope{}, c.err
}

func TestRetryClassifiesErrors(t *testing.T) {
	noSleep := func(context.Context, time.Duration) error { return nil }

	five := &countingProvider{protocol: protocolOpenAIChat, err: &httpStatusError{Status: 503}}
	r := &retryingProvider{inner: five, attempts: 3, base: time.Millisecond, sleep: noSleep}
	if _, err := r.Invoke(context.Background(), Envelope{}); err == nil {
		t.Fatal("want error")
	}
	if five.calls != 3 {
		t.Fatalf("5xx calls = %d, want 3 (retried)", five.calls)
	}

	four := &countingProvider{protocol: protocolOpenAIChat, err: &httpStatusError{Status: 400}}
	r = &retryingProvider{inner: four, attempts: 3, base: time.Millisecond, sleep: noSleep}
	if _, err := r.Invoke(context.Background(), Envelope{}); err == nil {
		t.Fatal("want error")
	}
	if four.calls != 1 {
		t.Fatalf("4xx calls = %d, want 1 (never retried)", four.calls)
	}

	tr := &countingProvider{protocol: protocolOpenAIChat, err: &transportError{err: errors.New("boom")}}
	r = &retryingProvider{inner: tr, attempts: 3, base: time.Millisecond, sleep: noSleep}
	_, _ = r.Invoke(context.Background(), Envelope{})
	if tr.calls != 3 {
		t.Fatalf("transport calls = %d, want 3 (retried)", tr.calls)
	}
}
```

- [ ] **Step 2: Run** → undefined types.
- [ ] **Step 3: Implement**

```go
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
	return fmt.Sprintf("provider HTTP %d: %s", e.Status, truncate(e.Body, 200))
}

// truncate shortens s to at most n bytes (best-effort, rune-safe enough here).
func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
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
```

- [ ] **Step 4: Run** → PASS. **Step 5: Commit** `... -m "feat(#118): typed provider retry (5xx/transport retried, 4xx fail-closed)"`

---

### Task 4: OpenAI Responses adapter

**Files:** Create `internal/konoha/modelprovider_adapters.go`, test

- [ ] **Step 1: Failing test**

```go
func TestOpenAIResponsesAdapter(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"output":[
				{"type":"message","content":[{"type":"output_text","text":"hello"}]},
				{"type":"function_call","call_id":"c1","name":"do_thing","arguments":"{\"x\":1}"}
			],
			"status":"completed",
			"usage":{"input_tokens":11,"output_tokens":5}
		}`))
	}))
	defer srv.Close()

	t.Setenv("OPENAI_API_KEY", "sk-openai-test")
	p := &openAIResponsesProvider{name: "oa", baseURL: srv.URL, apiKeyEnv: "OPENAI_API_KEY", secrets: envSecretResolver{}, client: srv.Client()}
	resp, err := p.Invoke(context.Background(), Envelope{
		Model: "gpt-4o", System: "be nice",
		Messages: []Message{{Role: roleUser, Content: "hi"}},
	})
	require.NoError(t, err)
	if gotPath != "/responses" || gotAuth != "Bearer sk-openai-test" {
		t.Fatalf("path=%q auth=%q", gotPath, gotAuth)
	}
	if gotBody["model"] != "gpt-4o" || gotBody["instructions"] != "be nice" {
		t.Fatalf("body=%v", gotBody)
	}
	if resp.Text != "hello" || len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Name != "do_thing" {
		t.Fatalf("resp=%+v", resp)
	}
	if resp.Usage.TokensIn != 11 || resp.Usage.TokensOut != 5 {
		t.Fatalf("usage=%+v", resp.Usage)
	}
}
```

- [ ] **Step 2: Run** → undefined. **Step 3: Implement** the shared `doJSON` + adapter:

```go
package konoha

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// doJSON POSTs a JSON body and decodes a JSON response, translating failures
// into the typed provider errors the retry decorator classifies (#95).
func doJSON(ctx context.Context, client *http.Client, method, url string, headers map[string]string, body, out any) error {
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(body); err != nil {
		return err
	}
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, method, url, &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return &transportError{err: err}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return &transportError{err: err}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &httpStatusError{Status: resp.StatusCode, Body: string(data)}
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("decode provider response: %w", err)
		}
	}
	return nil
}

// authHeader resolves the credential NAME and returns the bearer header. The
// value is never returned to a caller for recording.
func authHeader(secrets SecretResolver, apiKeyEnv string) (map[string]string, error) {
	if strings.TrimSpace(apiKeyEnv) == "" {
		return map[string]string{}, nil
	}
	key, err := secrets.Resolve(apiKeyEnv)
	if err != nil {
		return nil, err
	}
	return map[string]string{"Authorization": "Bearer " + key}, nil
}

// openAIResponsesProvider implements the OpenAI Responses protocol.
type openAIResponsesProvider struct {
	name, baseURL, apiKeyEnv string
	secrets                  SecretResolver
	client                   *http.Client
}

func (p *openAIResponsesProvider) Name() string     { return p.name }
func (p *openAIResponsesProvider) Protocol() string { return protocolOpenAIResponses }

func (p *openAIResponsesProvider) Invoke(ctx context.Context, req Envelope) (Envelope, error) {
	headers, err := authHeader(p.secrets, p.apiKeyEnv)
	if err != nil {
		return Envelope{}, err
	}
	body := map[string]any{"model": req.Model}
	if req.System != "" {
		body["instructions"] = req.System
	}
	if req.MaxTokens > 0 {
		body["max_output_tokens"] = req.MaxTokens
	}
	if req.Temperature != nil {
		body["temperature"] = *req.Temperature
	}
	if len(req.Messages) > 0 {
		body["input"] = responsesInput(req.Messages)
	}
	if len(req.Tools) > 0 {
		tools := make([]map[string]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			tools = append(tools, map[string]any{
				"type": "function", "name": t.Name,
				"description": t.Description, "parameters": rawOrEmptyObject(t.Parameters),
			})
		}
		body["tools"] = tools
	}
	var out struct {
		Output []struct {
			Type      string `json:"type"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
			Content   []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
		Status string `json:"status"`
		Usage  struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := doJSON(ctx, p.client, http.MethodPost, p.baseURL+"/responses", headers, body, &out); err != nil {
		return Envelope{}, err
	}
	resp := Envelope{Stop: out.Status}
	resp.Usage = HarnessUsage{TokensIn: out.Usage.InputTokens, TokensOut: out.Usage.OutputTokens}
	for _, item := range out.Output {
		switch item.Type {
		case "message":
			for _, c := range item.Content {
				if c.Type == "output_text" {
					resp.Text += c.Text
				}
			}
		case "function_call":
			resp.ToolCalls = append(resp.ToolCalls, ToolCall{
				ID: item.CallID, Name: item.Name, Arguments: rawOrEmptyObject(json.RawMessage(item.Arguments)),
			})
		}
	}
	return resp, nil
}

// responsesInput maps canonical messages to the Responses `input` array.
func responsesInput(msgs []Message) []map[string]any {
	out := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		role := m.Role
		if role == "" {
			role = roleUser
		}
		out = append(out, map[string]any{"role": role, "content": m.Content})
	}
	return out
}

// rawOrEmptyObject returns raw JSON, or an empty JSON object when absent, so a
// required schema field is never null.
func rawOrEmptyObject(raw json.RawMessage) json.RawMessage {
	if len(bytes.TrimSpace(raw)) == 0 {
		return json.RawMessage("{}")
	}
	return raw
}
```

- [ ] **Step 4: Run** → PASS. **Step 5: Commit** `... -m "feat(#118): OpenAI Responses protocol adapter"`

---

### Task 5: OpenAI Chat Completions adapter

**Files:** Modify `modelprovider_adapters.go`, test

- [ ] **Step 1: Failing test**

```go
func TestOpenAIChatAdapter(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte(`{
			"choices":[{"message":{"content":"hi there","tool_calls":[{"id":"t1","type":"function","function":{"name":"f","arguments":"{}"}}]},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":7,"completion_tokens":3}
		}`))
	}))
	defer srv.Close()

	p := &openAIChatProvider{name: "gw", baseURL: srv.URL, client: srv.Client()}
	resp, err := p.Invoke(context.Background(), Envelope{Model: "gpt-4o-mini", Messages: []Message{{Role: roleUser, Content: "hi"}}})
	require.NoError(t, err)
	if gotPath != "/chat/completions" || gotBody["model"] != "gpt-4o-mini" {
		t.Fatalf("path=%q body=%v", gotPath, gotBody)
	}
	if resp.Text != "hi there" || len(resp.ToolCalls) != 1 || resp.ToolCalls[0].ID != "t1" {
		t.Fatalf("resp=%+v", resp)
	}
	if resp.Usage.TokensIn != 7 || resp.Usage.TokensOut != 3 {
		t.Fatalf("usage=%+v", resp.Usage)
	}
}
```

- [ ] **Step 2: Run** → undefined. **Step 3: Implement**

```go
// openAIChatProvider implements the OpenAI Chat Completions protocol, including
// OpenAI-chat-compatible gateways (OpenRouter/LiteLLM/Ollama).
type openAIChatProvider struct {
	name, baseURL, apiKeyEnv string
	secrets                  SecretResolver
	client                   *http.Client
}

func (p *openAIChatProvider) Name() string     { return p.name }
func (p *openAIChatProvider) Protocol() string { return protocolOpenAIChat }

func (p *openAIChatProvider) Invoke(ctx context.Context, req Envelope) (Envelope, error) {
	headers, err := authHeader(p.secrets, p.apiKeyEnv)
	if err != nil {
		return Envelope{}, err
	}
	msgs := make([]map[string]any, 0, len(req.Messages)+1)
	if req.System != "" {
		msgs = append(msgs, map[string]any{"role": roleSystem, "content": req.System})
	}
	for _, m := range req.Messages {
		mm := map[string]any{"role": m.Role, "content": m.Content}
		if m.ToolCallID != "" {
			mm["tool_call_id"] = m.ToolCallID
		}
		if len(m.ToolCalls) > 0 {
			mm["tool_calls"] = chatToolCalls(m.ToolCalls)
		}
		msgs = append(msgs, mm)
	}
	body := map[string]any{"model": req.Model, "messages": msgs}
	if req.MaxTokens > 0 {
		body["max_tokens"] = req.MaxTokens
	}
	if req.Temperature != nil {
		body["temperature"] = *req.Temperature
	}
	if len(req.Tools) > 0 {
		tools := make([]map[string]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			tools = append(tools, map[string]any{"type": "function", "function": map[string]any{
				"name": t.Name, "description": t.Description, "parameters": rawOrEmptyObject(t.Parameters),
			}})
		}
		body["tools"] = tools
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := doJSON(ctx, p.client, http.MethodPost, p.baseURL+"/chat/completions", headers, body, &out); err != nil {
		return Envelope{}, err
	}
	resp := Envelope{Usage: HarnessUsage{TokensIn: out.Usage.PromptTokens, TokensOut: out.Usage.CompletionTokens}}
	if len(out.Choices) > 0 {
		ch := out.Choices[0]
		resp.Text = ch.Message.Content
		resp.Stop = ch.FinishReason
		for _, tc := range ch.Message.ToolCalls {
			resp.ToolCalls = append(resp.ToolCalls, ToolCall{
				ID: tc.ID, Name: tc.Function.Name, Arguments: rawOrEmptyObject(json.RawMessage(tc.Function.Arguments)),
			})
		}
	}
	return resp, nil
}

func chatToolCalls(calls []ToolCall) []map[string]any {
	out := make([]map[string]any, 0, len(calls))
	for _, c := range calls {
		out = append(out, map[string]any{"id": c.ID, "type": "function", "function": map[string]any{
			"name": c.Name, "arguments": string(rawOrEmptyObject(c.Arguments)),
		}})
	}
	return out
}
```

- [ ] **Step 4: Run** → PASS. **Step 5: Commit** `... -m "feat(#118): OpenAI Chat Completions adapter + gateway support"`

---

### Task 6: Anthropic Messages adapter

**Files:** Modify `modelprovider_adapters.go`, test

- [ ] **Step 1: Failing test**

```go
func TestAnthropicAdapter(t *testing.T) {
	var gotPath, gotKey, gotVersion string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotKey = r.Header.Get("x-api-key")
		gotVersion = r.Header.Get("anthropic-version")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte(`{
			"content":[{"type":"text","text":"hey"},{"type":"tool_use","id":"u1","name":"f","input":{"a":1}}],
			"stop_reason":"tool_use",
			"usage":{"input_tokens":4,"output_tokens":2}
		}`))
	}))
	defer srv.Close()

	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-test")
	p := &anthropicMessagesProvider{name: "anth", baseURL: srv.URL, apiKeyEnv: "ANTHROPIC_API_KEY", secrets: envSecretResolver{}, client: srv.Client()}
	resp, err := p.Invoke(context.Background(), Envelope{Model: "claude-sonnet", System: "sys", Messages: []Message{{Role: roleUser, Content: "hi"}}})
	require.NoError(t, err)
	if gotPath != "/v1/messages" || gotKey != "sk-ant-test" || gotVersion == "" {
		t.Fatalf("path=%q key=%q version=%q", gotPath, gotKey, gotVersion)
	}
	if gotBody["system"] != "sys" || gotBody["model"] != "claude-sonnet" {
		t.Fatalf("body=%v", gotBody)
	}
	if resp.Text != "hey" || len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Name != "f" {
		t.Fatalf("resp=%+v", resp)
	}
	if resp.Usage.TokensIn != 4 || resp.Usage.TokensOut != 2 {
		t.Fatalf("usage=%+v", resp.Usage)
	}
}
```

- [ ] **Step 2: Run** → undefined. **Step 3: Implement**

```go
// anthropicVersion is the pinned Anthropic Messages API version.
const anthropicVersion = "2023-06-01"

// anthropicMessagesProvider implements the Anthropic Messages protocol.
type anthropicMessagesProvider struct {
	name, baseURL, apiKeyEnv string
	secrets                  SecretResolver
	client                   *http.Client
}

func (p *anthropicMessagesProvider) Name() string     { return p.name }
func (p *anthropicMessagesProvider) Protocol() string { return protocolAnthropicMessages }

func (p *anthropicMessagesProvider) Invoke(ctx context.Context, req Envelope) (Envelope, error) {
	headers := map[string]string{"anthropic-version": anthropicVersion}
	if strings.TrimSpace(p.apiKeyEnv) != "" {
		key, err := p.secrets.Resolve(p.apiKeyEnv)
		if err != nil {
			return Envelope{}, err
		}
		headers["x-api-key"] = key
	}
	msgs := make([]map[string]any, 0, len(req.Messages))
	for _, m := range req.Messages {
		role := m.Role
		if role != roleAssistant {
			role = roleUser
		}
		msgs = append(msgs, map[string]any{"role": role, "content": m.Content})
	}
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 4096 // Anthropic requires max_tokens
	}
	body := map[string]any{"model": req.Model, "messages": msgs, "max_tokens": maxTokens}
	if req.System != "" {
		body["system"] = req.System
	}
	if req.Temperature != nil {
		body["temperature"] = *req.Temperature
	}
	if len(req.Tools) > 0 {
		tools := make([]map[string]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			tools = append(tools, map[string]any{"name": t.Name, "description": t.Description, "input_schema": rawOrEmptyObject(t.Parameters)})
		}
		body["tools"] = tools
	}
	var out struct {
		Content []struct {
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
		Usage      struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := doJSON(ctx, p.client, http.MethodPost, p.baseURL+"/v1/messages", headers, body, &out); err != nil {
		return Envelope{}, err
	}
	resp := Envelope{Stop: out.StopReason, Usage: HarnessUsage{TokensIn: out.Usage.InputTokens, TokensOut: out.Usage.OutputTokens}}
	for _, c := range out.Content {
		switch c.Type {
		case "text":
			resp.Text += c.Text
		case "tool_use":
			resp.ToolCalls = append(resp.ToolCalls, ToolCall{ID: c.ID, Name: c.Name, Arguments: rawOrEmptyObject(c.Input)})
		}
	}
	return resp, nil
}
```

- [ ] **Step 4: Run** → PASS. **Step 5: Commit** `... -m "feat(#118): Anthropic Messages adapter"`

---

### Task 7: Registry / factory + usage→§39

**Files:** Modify `modelprovider.go`, test

- [ ] **Step 1: Failing tests**

```go
func TestProviderRegistryBuildsAndRejects(t *testing.T) {
	cfg := map[string]ProviderConfig{
		"oa":  {Protocol: protocolOpenAIResponses, BaseURL: "http://127.0.0.1:9"},
		"gw":  {Gateway: "litellm"},
		"bad": {Protocol: "grpc"},
	}
	if _, err := newProviderRegistry(cfg, envSecretResolver{}, nil); err == nil {
		t.Fatal("unknown protocol must fail registry build")
	}
	delete(cfg, "bad")
	r, err := newProviderRegistry(cfg, envSecretResolver{}, nil)
	require.NoError(t, err)
	if _, err := r.Lookup("missing"); err == nil {
		t.Fatal("unknown provider must fail Lookup")
	}
	p, err := r.Lookup("oa")
	require.NoError(t, err)
	if p.Protocol() != protocolOpenAIResponses {
		t.Fatalf("protocol = %q", p.Protocol())
	}
	if got := r.Names(); len(got) != 2 || got[0] != "gw" || got[1] != "oa" {
		t.Fatalf("names = %v", got)
	}
}

func TestProviderUsageFeedsMetricSink(t *testing.T) {
	resetMetrics()
	defer resetMetrics()
	require.NoError(t, RecordCycleMetrics(context.Background(), CycleOutcome{
		WorkflowID: "w-provider", Issue: 1, TerminalState: stateClosed, AutonomousClose: true,
		Usage: HarnessUsage{TokensIn: 11, TokensOut: 5, CostUSD: 0.25},
	}))
	if sink.get(mAgentTokensIn) != 11 || sink.get(mAgentTokensOut) != 5 || sink.get(mAgentCostUSD) != 0.25 {
		t.Fatalf("usage not accounted: in=%v out=%v cost=%v", sink.get(mAgentTokensIn), sink.get(mAgentTokensOut), sink.get(mAgentCostUSD))
	}
}
```

- [ ] **Step 2: Run** → undefined `newProviderRegistry`.
- [ ] **Step 3: Implement** (add `net/http` import already present):

```go
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
```

- [ ] **Step 4: Run** `go test ./internal/konoha/ -run 'TestProvider' -v` → PASS. **Step 5: Commit** `... -m "feat(#118): provider registry/factory + usage accounting test"`

---

### Task 8: Docs

**Files:** `configs/konoha.yaml`, `README.md`, `AGENTS.md`

- [ ] **Step 1:** Add a commented `defaults.providers` example to `configs/konoha.yaml` (after `defaults.agents`, before `worker:` is fine — keep it under `defaults:`), documenting `protocol`/`gateway`/`baseUrl`/`apiKeyEnv` (NAME only), the closed protocol set, gateway presets, and that the layer is not yet wired to the harness.
- [ ] **Step 2:** README: add a "Model providers (protocols + gateways)" subsection after the resource-catalog section, with examples for OpenAI Responses, Anthropic Messages, and an OpenRouter/LiteLLM/Ollama gateway; note typed retry (#95) and credential-by-name (#82).
- [ ] **Step 3:** AGENTS.md: add a `modelprovider` note to the Architecture/gotchas list — files, the closed protocol/gateway sets, that gateways are OpenAI-chat presets, that the layer is standalone (not wired to agents), and that credentials are names resolved at Invoke time and masked by the redactor.
- [ ] **Step 4: Commit** `... -m "docs(#118): provider/protocol reference + modelprovider note"`

---

### Task 9: Verification

- [ ] `gofmt -l internal/konoha/` → no output.
- [ ] `make test` → green.
- [ ] Confirm no exported activity/workflow func was renamed (Temporal type-name invariant) — this change adds none.

---

## Self-review

- **Spec coverage:** interface+envelope (T1), adapters (T4/T5/T6), gateway preset (T1/T5), typed retry (T3), credentials-by-name (T2), unknown protocol/gateway at load (T1/T7), usage→§39 (T7), docs (T8). All ACs covered.
- **Placeholder scan:** none.
- **Type consistency:** `ProviderConfig.validate(prefix)`, `effectiveProtocol()`, `baseURL()`, `descriptor(name)`, `newProviderRegistry(cfg, secrets, client)`, `retryingProvider{inner,attempts,base,sleep}`, `rawOrEmptyObject` used consistently across tasks.
