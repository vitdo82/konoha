# Model-provider protocol abstraction (#118) — design

## Problem

Konoha invokes models only by shelling out to an agent harness (`opencode`/`generic`,
`harness.go`). #98 gives each stage a named `provider/model` id, but there is no
wire-level abstraction: no canonical request/response, no protocol adapters, no
typed transport retry. This ticket adds that foundation. It **feeds** profiles
(#PL1) and the secrets store (#PL6); it is **not** wired into the agent execution
path (every acceptance criterion is a fake-HTTP/unit test).

## Scope

In: a `ModelProvider` interface, a canonical `Envelope`, three protocol adapters,
a gateway preset, typed retry, credentials-by-name, config load validation, tests,
docs.

Out: replacing the harness, model↔provider links, cycle/evidence wiring, an actual
secrets store (#PL6). Unknown/future surfaces are documented as not-present rather
than stubbed.

## Interface

```go
type ModelProvider interface {
    Name() string
    Protocol() string
    Invoke(ctx context.Context, req Envelope) (Envelope, error)
}
```

## Canonical envelope

One type carries request and response; no provider type leaks past the boundary.

```go
type Message struct {
    Role       string // system|user|assistant|tool
    Content    string
    ToolCalls  []ToolCall
    ToolCallID string
    Name       string
}
type ToolDef struct {
    Name        string
    Description string
    Parameters  json.RawMessage
}
type ToolCall struct {
    ID        string
    Name      string
    Arguments json.RawMessage
}
type Envelope struct {
    // request
    Model       string
    System      string
    Messages    []Message
    Tools       []ToolDef
    MaxTokens   int
    Temperature *float64
    // response (filled by Invoke)
    Text      string
    ToolCalls []ToolCall
    Stop      string
    Usage     HarnessUsage // #87 canonical usage
}
```

`HarnessUsage` is reused verbatim so a provider response feeds §39/#87 accounting
with no translation layer.

## Protocols and gateways

Closed sets; an unknown value fails at load.

- Protocols: `openai-responses`, `openai-chat`, `anthropic-messages`.
- Gateways: `openrouter`, `litellm`, `ollama` — OpenAI-chat-compatible presets that
  supply a default base URL. A gateway is config, not a fourth adapter.

Config lives under `defaults.providers`:

```yaml
defaults:
  providers:
    anthropic:
      protocol: anthropic-messages
      apiKeyEnv: ANTHROPIC_API_KEY
    openrouter:
      gateway: openrouter
      apiKeyEnv: OPENROUTER_API_KEY
```

`provider` entry shape and validation:

- at least one of `protocol`/`gateway`;
- `protocol` must be a known protocol;
- `gateway` must be a known gateway, and if `protocol` is also set it must be
  `openai-chat` (mismatch fails);
- `baseUrl`, when set, must be an http(s) URL; otherwise the protocol/gateway
  default applies;
- `apiKeyEnv` must be a bare env var name (never a value).

## Registry and factory

```go
func newProviderRegistry(cfg map[string]ProviderConfig, secrets SecretResolver, client *http.Client) (*providerRegistry, error)
func (r *providerRegistry) Lookup(name string) (ModelProvider, error) // unknown -> error
func (r *providerRegistry) Names() []string                          // sorted
```

`providerRegistryFor()` builds the process registry from `appCfg.Defaults.Providers`.
The HTTP client is injected so tests point it at an `httptest` server.

## Typed retry (#95)

Adapters return typed errors:

- `transportError` — the request never got a response;
- `httpStatusError{Status, Body}` — a non-2xx response.

A `retryingProvider` decorator wraps any provider:

- transport + 5xx → retry up to `appCfg.Worker.infraAttempts()` with exponential
  backoff;
- 4xx → returned immediately (fail-closed, one attempt);
- backoff sleep is injectable so tests do not wait.

This mirrors the existing `retry.go` classes without a Temporal dependency.

## Credentials by name

```go
type SecretResolver interface {
    Resolve(name string) (string, error)
}
```

Default implementation reads the env var `name`. The value is fetched only at
`Invoke` time, written into the auth header, and registered with the process
redactor (`RegisterSecrets`, #82). It is never placed in the envelope, an error,
a log line, or a descriptor. The persisted identity is:

```go
type ProviderDescriptor struct {
    Name      string
    Protocol  string
    BaseURL   string
    APIKeyEnv string // NAME only — never the value
}
```

## Adapters

- **OpenAI Responses** (`POST /responses`): `input` messages + `tools`; parses
  `output` items (`message` text, `function_call`) and `usage{input_tokens,output_tokens}`.
- **OpenAI Chat Completions** (`POST /chat/completions`): `messages` + `tools`
  (`function`); parses `choices[0].message.content`/`tool_calls` and
  `usage{prompt_tokens,completion_tokens}`.
- **Anthropic Messages** (`POST /v1/messages`): `system` + `messages` +
  `tools`; parses `content[].text`/`tool_use` and
  `usage{input_tokens,output_tokens}`; auth via `x-api-key` + `anthropic-version`.

Each is a pure translator over the injected client; each maps its response usage
into `Envelope.Usage`.

## Tests (`modelprovider_test.go`, `httptest`)

1. Each adapter produces the expected request shape and parses the expected
   response shape.
2. A provider response's usage feeds the §39/#87 accounting (`CycleOutcome.Usage`
   → `RecordCycleMetrics`).
3. An unknown protocol and an unknown gateway fail at load.
4. The resolved credential never appears in the descriptor, logs, or prompts; the
   redactor masks it.
5. 5xx/transport is retried per the #95 policy; 4xx is not.
6. No credential value is persisted — only the `apiKeyEnv` name.

`make test` green + `gofmt` clean.

## Docs

- `README.md`: provider/protocol reference + gateway examples.
- `AGENTS.md`: `modelprovider` note.
- `configs/konoha.yaml`: commented `defaults.providers` example.
