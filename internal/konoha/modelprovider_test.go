package konoha

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

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
	cfg.Defaults.Providers = map[string]ProviderConfig{"": {Protocol: protocolOpenAIChat}}
	if err := cfg.validate(); err == nil {
		t.Fatal("empty provider name must fail at load")
	}
}

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
	if _, err := r.Invoke(context.Background(), Envelope{}); err == nil {
		t.Fatal("want error")
	}
	if tr.calls != 3 {
		t.Fatalf("transport calls = %d, want 3 (retried)", tr.calls)
	}
}

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

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":11,"completion_tokens":5}}`))
	}))
	defer srv.Close()

	p := &openAIChatProvider{name: "gw", baseURL: srv.URL, client: srv.Client()}
	resp, err := p.Invoke(context.Background(), Envelope{Model: "m", Messages: []Message{{Role: roleUser, Content: "hi"}}})
	require.NoError(t, err)

	// #118 AC: a provider response's usage feeds the §39/#87 accounting.
	require.NoError(t, RecordCycleMetrics(context.Background(), CycleOutcome{
		WorkflowID: "w-provider", Issue: 1, TerminalState: stateClosed, AutonomousClose: true, Usage: resp.Usage,
	}))
	if sink.get(mAgentTokensIn) != 11 || sink.get(mAgentTokensOut) != 5 {
		t.Fatalf("usage not accounted: in=%v out=%v", sink.get(mAgentTokensIn), sink.get(mAgentTokensOut))
	}
}
