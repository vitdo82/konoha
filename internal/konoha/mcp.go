package konoha

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"go.temporal.io/sdk/client"
)

// Local MCP server (#116). An opt-in, LOOPBACK-ONLY, read-only Model Context
// Protocol surface so an operator's local agent can ask "what needs attention?",
// "what is the status of cycle X?", "show the latest failure" without scraping
// the evidence store. It is disabled by default (no listener unless
// KONOHA_MCP_ADDR is set) and reads no GitHub credential.
//
// The tools are thin projections over the SAME data the CLI (#99) and console
// (#105) read: list_cycles / get_cycle / get_evidence over the evidence store
// (#83) + the Temporal status query (#89), and environment_list / environment_log
// / environment_diff over the cycle's recorded evidence. Every read is redacted
// (#82) and size-bounded; get_evidence paths are evidence-store-relative only
// (traversal rejected, #83). There are no mutating tools by default.
//
// Optional control: when KONOHA_MCP_CONTROL is set, ONE control tool
// (post_decision) is exposed and it writes ONLY through the authoritative §18
// decision channel (#108) — never a parallel path. Control is default-closed.
//
// Transport: MCP Streamable HTTP (JSON-RPC 2.0 over POST /mcp). Sessions are not
// issued (stateless); a notification (no id) is acknowledged with 202 and no body.

const (
	mcpAddrEnv    = "KONOHA_MCP_ADDR"
	mcpTokenEnv   = "KONOHA_MCP_TOKEN"
	mcpControlEnv = "KONOHA_MCP_CONTROL"

	// mcpProtocolVersion is the MCP revision this surface implements. It echoes
	// this value on initialize; the server is stateless and negotiates no
	// capability beyond tools.
	mcpProtocolVersion = "2025-06-18"

	// maxMCPBody bounds the whole JSON-RPC request body.
	maxMCPBody = 1 << 20

	// JSON-RPC 2.0 error codes.
	mcpErrParse          = -32700
	mcpErrMethodNotFound = -32601
	mcpErrInvalidParams  = -32602
)

// MCPServerConfig is the resolved MCP configuration (env-only, default disabled).
type MCPServerConfig struct {
	Addr    string
	Token   string
	Control bool
}

// mcpConfigFromEnv resolves the MCP config through an injected getenv so tests
// can assert exactly which environment keys are read (never GH_*).
func mcpConfigFromEnv(getenv func(string) string) MCPServerConfig {
	return MCPServerConfig{
		Addr:    strings.TrimSpace(getenv(mcpAddrEnv)),
		Token:   getenv(mcpTokenEnv),
		Control: truthy(getenv(mcpControlEnv)),
	}
}

// truthy reports whether an opt-in env flag is set to a truthy value.
func truthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func (c MCPServerConfig) enabled() bool { return c.Addr != "" }

// MCPServer serves the read-only (and optionally control) MCP surface.
type MCPServer struct {
	token          string
	generatedToken bool
	control        bool
	version        string
	data           *consoleData
	mux            *http.ServeMux
	server         *http.Server
}

// newMCPServer builds the server (disabled → nil, nil). A non-loopback bind
// address fails fast; the bearer token is generated when unset.
func newMCPServer(cfg MCPServerConfig, data *consoleData, version string) (*MCPServer, error) {
	if !cfg.enabled() {
		return nil, nil
	}
	if err := validateLoopback("mcp", cfg.Addr); err != nil {
		return nil, err
	}
	token := cfg.Token
	generated := false
	if token == "" {
		t, err := randomToken()
		if err != nil {
			return nil, fmt.Errorf("mcp token: %w", err)
		}
		token, generated = t, true
	}
	if version == "" {
		version = "dev"
	}
	s := &MCPServer{token: token, generatedToken: generated, control: cfg.Control, version: version, data: data}
	s.mux = http.NewServeMux()
	s.mux.HandleFunc("POST /mcp", s.requireAuth(s.handleMCP))
	s.server = &http.Server{
		Addr:              cfg.Addr,
		Handler:           s,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	return s, nil
}

// ServeHTTP applies the security baseline to every response.
func (s *MCPServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "no-store")
	s.mux.ServeHTTP(w, r)
}

// requireAuth enforces a valid bearer token, else 401.
func (s *MCPServer) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if t := bearerToken(r); t == "" || !constantTimeEqual(t, s.token) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="konoha-mcp"`)
			writeConsoleError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next(w, r)
	}
}

// JSON-RPC 2.0 request/response envelopes.
type mcpRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type mcpResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *mcpError       `json:"error,omitempty"`
}

type mcpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type mcpCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// mcpCallResult is the MCP tools/call result: a text rendering plus the
// structured JSON (api-types-style) so a client can parse either.
type mcpCallResult struct {
	Content           []mcpContent `json:"content"`
	StructuredContent any          `json:"structuredContent,omitempty"`
	IsError           bool         `json:"isError,omitempty"`
}

type mcpContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func mcpResult(id json.RawMessage, result any) mcpResponse {
	return mcpResponse{JSONRPC: "2.0", ID: id, Result: result}
}

func mcpErrorResponse(id json.RawMessage, code int, msg string) mcpResponse {
	return mcpResponse{JSONRPC: "2.0", ID: id, Error: &mcpError{Code: code, Message: msg}}
}

// handleMCP decodes one JSON-RPC message and dispatches it. A notification (no
// id) is acknowledged with 202 and no body.
func (s *MCPServer) handleMCP(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxMCPBody)
	var req mcpRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeConsoleJSON(w, http.StatusOK, mcpErrorResponse(nil, mcpErrParse, "parse error"))
		return
	}
	if len(req.ID) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	writeConsoleJSON(w, http.StatusOK, s.dispatch(r.Context(), req))
}

// dispatch routes one request to a method handler.
func (s *MCPServer) dispatch(ctx context.Context, req mcpRequest) mcpResponse {
	switch req.Method {
	case "initialize":
		return mcpResult(req.ID, map[string]any{
			"protocolVersion": mcpProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "konoha", "version": s.version},
		})
	case "ping":
		return mcpResult(req.ID, map[string]any{})
	case "tools/list":
		return mcpResult(req.ID, map[string]any{"tools": s.toolDefs()})
	case "tools/call":
		var p mcpCallParams
		if err := decodeMCPArgs(req.Params, &p); err != nil {
			return mcpErrorResponse(req.ID, mcpErrInvalidParams, err.Error())
		}
		tool, ok := s.lookupTool(p.Name)
		if !ok {
			return mcpErrorResponse(req.ID, mcpErrInvalidParams, fmt.Sprintf("unknown tool %q", p.Name))
		}
		out, err := tool.call(ctx, p.Arguments)
		if err != nil {
			return mcpResult(req.ID, mcpCallResult{
				Content: []mcpContent{{Type: "text", Text: err.Error()}},
				IsError: true,
			})
		}
		text, _ := json.MarshalIndent(out, "", "  ")
		return mcpResult(req.ID, mcpCallResult{
			Content:           []mcpContent{{Type: "text", Text: string(text)}},
			StructuredContent: out,
		})
	}
	return mcpErrorResponse(req.ID, mcpErrMethodNotFound, fmt.Sprintf("method %q not found", req.Method))
}

// decodeMCPArgs decodes tool arguments strictly; absent/null arguments are fine.
func decodeMCPArgs(raw json.RawMessage, v any) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	return nil
}

// mcpTool is one exposed tool. control marks the (opt-in) tools that are not
// read-only; the read-only set is always exposed.
type mcpTool struct {
	name        string
	description string
	schema      map[string]any
	control     bool
	call        func(context.Context, json.RawMessage) (any, error)
}

// toolList returns the exposed tools in a deterministic order. Control tools are
// present only when the surface was built with control enabled (default-closed).
func (s *MCPServer) toolList() []mcpTool {
	tools := []mcpTool{
		{
			name:        "list_cycles",
			description: "List cycles (finished from the evidence store + running from Temporal), optionally filtered by repo and/or state.",
			schema: mcpSchema(map[string]any{
				"repo":  map[string]any{"type": "string", "description": "filter by repository name"},
				"state": map[string]any{"type": "string", "description": "filter by §6 state, e.g. CLOSED, HUMAN_REQUIRED"},
			}),
			call: s.callListCycles,
		},
		{
			name:        "get_cycle",
			description: "Get one cycle's detail: state, agent/verification outcomes, ordered conditions, artifacts, diff/validation projections.",
			schema:      mcpSchema(map[string]any{"id": map[string]any{"type": "string", "description": "cycle id, e.g. duck-issue-7"}}, "id"),
			call:        s.callGetCycle,
		},
		{
			name:        "get_evidence",
			description: "List one cycle's evidence artifacts, or fetch one artifact's redacted, path-validated content.",
			schema: mcpSchema(map[string]any{
				"id":   map[string]any{"type": "string", "description": "cycle id"},
				"name": map[string]any{"type": "string", "description": "optional artifact path; omit to list"},
			}, "id"),
			call: s.callGetEvidence,
		},
		{
			name:        "environment_list",
			description: "List the recorded evidence artifacts for one cycle (the cycle's inspectable environment).",
			schema:      mcpSchema(map[string]any{"id": map[string]any{"type": "string", "description": "cycle id"}}, "id"),
			call:        s.callEnvironmentList,
		},
		{
			name:        "environment_log",
			description: "Return one cycle's agent stdout/stderr and validation output (the same data as `prbot logs`).",
			schema:      mcpSchema(map[string]any{"id": map[string]any{"type": "string", "description": "cycle id"}}, "id"),
			call:        s.callEnvironmentLog,
		},
		{
			name:        "environment_diff",
			description: "Return one cycle's recorded diff (diff.patch).",
			schema:      mcpSchema(map[string]any{"id": map[string]any{"type": "string", "description": "cycle id"}}, "id"),
			call:        s.callEnvironmentDiff,
		},
	}
	if s.control {
		tools = append(tools, mcpTool{
			name:        "post_decision",
			description: "Post one human decision (approve/request-changes/deny) to the authoritative §18 decision channel; binds to the live gate attempt+SHA. Control tool (opt-in).",
			schema: mcpSchema(map[string]any{
				"id":       map[string]any{"type": "string", "description": "cycle id"},
				"action":   map[string]any{"type": "string", "enum": []string{humanDecisionApprove, humanDecisionRequestChanges, humanDecisionDeny}},
				"feedback": map[string]any{"type": "string", "description": "required for request-changes"},
				"attempt":  map[string]any{"type": "integer", "description": "live gate attempt"},
				"sha":      map[string]any{"type": "string", "description": "live gate reviewed commit SHA"},
			}, "id", "action", "attempt", "sha"),
			control: true,
			call:    s.callPostDecision,
		})
	}
	return tools
}

// mcpSchema builds a JSON-Schema object; required is omitted when empty.
func mcpSchema(props map[string]any, required ...string) map[string]any {
	s := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func (s *MCPServer) lookupTool(name string) (mcpTool, bool) {
	for _, t := range s.toolList() {
		if t.name == name {
			return t, true
		}
	}
	return mcpTool{}, false
}

// toolDefs is the tools/list payload.
func (s *MCPServer) toolDefs() []map[string]any {
	list := s.toolList()
	out := make([]map[string]any, 0, len(list))
	for _, t := range list {
		out = append(out, map[string]any{
			"name":        t.name,
			"description": t.description,
			"inputSchema": t.schema,
		})
	}
	return out
}

// requireStore is the shared guard: the read tools need an evidence store.
func (s *MCPServer) requireStore() (*EvidenceStore, error) {
	if s.data == nil || s.data.store == nil {
		return nil, errors.New("mcp: no evidence store available")
	}
	return s.data.store, nil
}

// callListCycles returns the board rows + columns, filtered by repo/state.
func (s *MCPServer) callListCycles(ctx context.Context, raw json.RawMessage) (any, error) {
	var args struct {
		Repo  string `json:"repo"`
		State string `json:"state"`
	}
	if err := decodeMCPArgs(raw, &args); err != nil {
		return nil, err
	}
	if _, err := s.requireStore(); err != nil {
		return nil, err
	}
	cycles, err := s.data.listCycles(ctx)
	if err != nil {
		return nil, err
	}
	filtered := make([]CycleSummary, 0, len(cycles))
	for _, c := range cycles {
		if args.Repo != "" && c.Repo != args.Repo {
			continue
		}
		if args.State != "" && c.State != args.State {
			continue
		}
		filtered = append(filtered, c)
	}
	return map[string]any{"cycles": filtered, "columns": boardColumns(filtered)}, nil
}

// callGetCycle returns one cycle's detail (same projection as the console).
func (s *MCPServer) callGetCycle(ctx context.Context, raw json.RawMessage) (any, error) {
	id, err := mcpCycleIDArg(raw)
	if err != nil {
		return nil, err
	}
	if _, err := s.requireStore(); err != nil {
		return nil, err
	}
	detail, ok, err := s.data.cycleDetail(ctx, id)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("unknown cycle %q", id)
	}
	return detail, nil
}

// callGetEvidence lists a cycle's artifacts, or reads one redacted artifact.
func (s *MCPServer) callGetEvidence(ctx context.Context, raw json.RawMessage) (any, error) {
	var args struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := decodeMCPArgs(raw, &args); err != nil {
		return nil, err
	}
	if !validConsoleCycleID(args.ID) {
		return nil, fmt.Errorf("malformed cycle id %q", args.ID)
	}
	store, err := s.requireStore()
	if err != nil {
		return nil, err
	}
	if _, ok, err := s.data.cycleDetail(ctx, args.ID); err != nil {
		return nil, err
	} else if !ok {
		return nil, fmt.Errorf("unknown cycle %q", args.ID)
	}
	if args.Name == "" {
		return s.artifactList(store, args.ID)
	}
	if !validArtifactPath(args.Name) {
		return nil, fmt.Errorf("malformed artifact path %q", args.Name)
	}
	data, ok := store.readArtifactBytes(args.ID, args.Name)
	if !ok {
		return nil, fmt.Errorf("unknown artifact %q", args.Name)
	}
	kind, ct := classifyArtifact(args.Name)
	return map[string]any{
		"id":          args.ID,
		"path":        args.Name,
		"contentType": ct,
		"kind":        kind,
		"size":        len(data),
		"truncated":   len(data) > consoleArtifactCap,
		"content":     Redact(truncateConsole(string(data))),
	}, nil
}

// callEnvironmentList lists one cycle's evidence artifacts.
func (s *MCPServer) callEnvironmentList(_ context.Context, raw json.RawMessage) (any, error) {
	id, err := mcpCycleIDArg(raw)
	if err != nil {
		return nil, err
	}
	store, err := s.requireStore()
	if err != nil {
		return nil, err
	}
	return s.artifactList(store, id)
}

// callEnvironmentLog returns the cycle's agent/validation streams (same data as
// `prbot logs`, redacted).
func (s *MCPServer) callEnvironmentLog(_ context.Context, raw json.RawMessage) (any, error) {
	id, err := mcpCycleIDArg(raw)
	if err != nil {
		return nil, err
	}
	store, err := s.requireStore()
	if err != nil {
		return nil, err
	}
	var b strings.Builder
	printed := false
	for _, name := range []string{evidenceStdout, evidenceStderr, evidenceValidation} {
		c, ok := store.readArtifact(id, name)
		if !ok || strings.TrimSpace(c) == "" {
			continue
		}
		fmt.Fprintf(&b, "===== %s =====\n%s", name, c)
		if !strings.HasSuffix(c, "\n") {
			b.WriteString("\n")
		}
		printed = true
	}
	if !printed {
		return nil, fmt.Errorf("no evidence for %q", id)
	}
	return map[string]any{"id": id, "log": Redact(b.String())}, nil
}

// callEnvironmentDiff returns the cycle's recorded diff.
func (s *MCPServer) callEnvironmentDiff(_ context.Context, raw json.RawMessage) (any, error) {
	id, err := mcpCycleIDArg(raw)
	if err != nil {
		return nil, err
	}
	store, err := s.requireStore()
	if err != nil {
		return nil, err
	}
	c, ok := store.readArtifact(id, evidenceDiff)
	if !ok || strings.TrimSpace(c) == "" {
		return nil, fmt.Errorf("no diff evidence for %q", id)
	}
	return map[string]any{"id": id, "diff": Redact(truncateConsole(c))}, nil
}

// callPostDecision is the single optional control tool: it reuses the console's
// #108 submission path, so it binds to the live gate and writes ONLY the §18
// decision channel. Never a parallel write.
func (s *MCPServer) callPostDecision(ctx context.Context, raw json.RawMessage) (any, error) {
	var args struct {
		ID       string `json:"id"`
		Action   string `json:"action"`
		Feedback string `json:"feedback"`
		Attempt  int    `json:"attempt"`
		SHA      string `json:"sha"`
	}
	if err := decodeMCPArgs(raw, &args); err != nil {
		return nil, err
	}
	if !validConsoleCycleID(args.ID) {
		return nil, fmt.Errorf("malformed cycle id %q", args.ID)
	}
	if s.data == nil || s.data.decide == nil {
		return nil, errors.New("mcp: control is disabled")
	}
	return s.data.submitDecision(ctx, args.ID, DecisionRequest{
		Action: args.Action, Feedback: args.Feedback, Attempt: args.Attempt, SHA: args.SHA,
	})
}

// artifactList is the shared artifact-listing projection (same as the console's
// /artifacts endpoint).
func (s *MCPServer) artifactList(store *EvidenceStore, id string) (any, error) {
	views, err := store.artifactViews(id)
	if err != nil {
		return nil, err
	}
	return map[string]any{"id": id, "artifacts": views}, nil
}

// mcpCycleIDArg decodes + validates the shared {id} argument.
func mcpCycleIDArg(raw json.RawMessage) (string, error) {
	var args struct {
		ID string `json:"id"`
	}
	if err := decodeMCPArgs(raw, &args); err != nil {
		return "", err
	}
	if !validConsoleCycleID(args.ID) {
		return "", fmt.Errorf("malformed cycle id %q", args.ID)
	}
	return args.ID, nil
}

// StartMCPServer starts the MCP surface when KONOHA_MCP_ADDR is set, else a
// no-op (nil, nil). A non-loopback address fails fast.
func StartMCPServer(c client.Client, store *EvidenceStore, version string) (*http.Server, error) {
	cfg := mcpConfigFromEnv(os.Getenv)
	if !cfg.enabled() {
		return nil, nil
	}
	data := buildConsoleData(store, consoleRepos(), c)
	srv, err := newMCPServer(cfg, data, version)
	if err != nil {
		return nil, err
	}
	if srv.generatedToken {
		log.Printf("mcp: generated bearer token: %s", srv.token)
	}
	go func() {
		if err := srv.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("mcp server: %v", err)
		}
	}()
	note := "read-only"
	if srv.control {
		note = "read-only + opt-in post_decision via the §18 channel"
	}
	log.Printf("mcp listening on %s (loopback; %s)", cfg.Addr, note)
	return srv.server, nil
}

// runMCP is the `prbot mcp` entrypoint: a standalone, loopback-only MCP process.
// It reads only the evidence store and (best-effort) Temporal for running cycles
// — never a GitHub credential. Blocks until interrupted.
func runMCP() error {
	cfg := mcpConfigFromEnv(os.Getenv)
	if !cfg.enabled() {
		return fmt.Errorf("mcp: %s is not set (mcp disabled)", mcpAddrEnv)
	}
	if err := validateLoopback("mcp", cfg.Addr); err != nil {
		return err
	}
	// Load konoha.yaml here: this entrypoint is dispatched before the worker
	// bootstrap so the MCP process reads no GitHub credential.
	if app, err := loadAppConfig(defaultAppConfigPath()); err != nil {
		return err
	} else {
		appCfg = app
	}
	store, err := NewEvidenceStore(appCfg.Evidence, &defaultRedactor)
	if err != nil {
		return err
	}
	addr := os.Getenv("TEMPORAL_ADDRESS")
	if addr == "" {
		addr = "127.0.0.1:7233"
	}
	var c client.Client
	if dialed, err := client.Dial(client.Options{HostPort: addr}); err == nil {
		defer dialed.Close()
		c = dialed
	}
	data := buildConsoleData(store, consoleRepos(), c)
	srv, err := newMCPServer(cfg, data, consoleVersion())
	if err != nil {
		return err
	}
	if srv.generatedToken {
		log.Printf("mcp: generated bearer token: %s", srv.token)
	}
	log.Printf("mcp listening on %s (loopback)", cfg.Addr)
	return srv.server.ListenAndServe()
}
