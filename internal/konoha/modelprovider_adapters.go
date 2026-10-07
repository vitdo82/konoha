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

// Protocol adapters (#118). Each adapter is a pure request/response translator
// over an injected *http.Client; the retry policy lives in retryingProvider.

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
	if secrets == nil {
		return nil, fmt.Errorf("secret %q needs a resolver", apiKeyEnv)
	}
	key, err := secrets.Resolve(apiKeyEnv)
	if err != nil {
		return nil, err
	}
	return map[string]string{"Authorization": "Bearer " + key}, nil
}

// rawOrEmptyObject returns raw JSON, or an empty JSON object when absent, so a
// required schema field is never null.
func rawOrEmptyObject(raw json.RawMessage) json.RawMessage {
	if len(bytes.TrimSpace(raw)) == 0 {
		return json.RawMessage("{}")
	}
	return raw
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
		if p.secrets == nil {
			return Envelope{}, fmt.Errorf("secret %q needs a resolver", p.apiKeyEnv)
		}
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
