package konoha

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// #116 AC: the MCP surface is disabled by default and loopback-only when
// enabled; a non-loopback bind is rejected. A token is generated when unset.
func TestMCPConfigLoopbackAndDisabled(t *testing.T) {
	empty := func(string) string { return "" }
	cfg := mcpConfigFromEnv(empty)
	if cfg.enabled() {
		t.Fatal("mcp must be disabled with no address")
	}
	srv, err := newMCPServer(cfg, &consoleData{}, "dev")
	if err != nil || srv != nil {
		t.Fatalf("disabled mcp = (%v, %v), want (nil, nil)", srv, err)
	}
	if _, err := newMCPServer(MCPServerConfig{Addr: "0.0.0.0:8080", Token: "t"}, &consoleData{}, "dev"); err == nil {
		t.Fatal("non-loopback mcp addr must fail at construction")
	}
	gen, err := newMCPServer(MCPServerConfig{Addr: "127.0.0.1:0"}, &consoleData{}, "dev")
	if err != nil {
		t.Fatal(err)
	}
	if gen.token == "" || !gen.generatedToken {
		t.Fatalf("token = %q generated=%v, want a generated token", gen.token, gen.generatedToken)
	}
}

// newTestMCP builds an MCP server over a temp evidence store (or a supplied
// data) with a known bearer token.
func newTestMCP(t *testing.T, control bool, data *consoleData) (*MCPServer, *EvidenceStore, *httptest.Server) {
	t.Helper()
	store, err := NewEvidenceStore(EvidenceConfig{Dir: t.TempDir()}, &defaultRedactor)
	if err != nil {
		t.Fatalf("evidence store: %v", err)
	}
	if data == nil {
		data = &consoleData{store: store}
	}
	srv, err := newMCPServer(MCPServerConfig{Addr: "127.0.0.1:0", Token: "secret-token", Control: control}, data, "test-1")
	if err != nil {
		t.Fatalf("new mcp: %v", err)
	}
	hs := httptest.NewServer(srv)
	t.Cleanup(hs.Close)
	return srv, store, hs
}

type mcpRawResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *mcpError       `json:"error"`
}

// mcpPost sends one JSON-RPC request over the streamable-HTTP transport.
func mcpPost(t *testing.T, hs *httptest.Server, bearer, method string, params any) (int, mcpRawResponse) {
	t.Helper()
	body := map[string]any{"jsonrpc": "2.0", "id": 1, "method": method}
	if params != nil {
		body["params"] = params
	}
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, hs.URL+"/mcp", bytes.NewReader(b))
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out mcpRawResponse
	if res.StatusCode == http.StatusOK {
		if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
			t.Fatalf("decode mcp response: %v", err)
		}
	}
	return res.StatusCode, out
}

// mcpRawCallResult is the tools/call result with the structured payload kept as
// raw JSON so each test can decode it into its own typed shape.
type mcpRawCallResult struct {
	Content           []mcpContent    `json:"content"`
	StructuredContent json.RawMessage `json:"structuredContent"`
	IsError           bool            `json:"isError"`
}

// mcpToolCall posts a tools/call and returns the decoded call result.
func mcpToolCall(t *testing.T, hs *httptest.Server, bearer, name string, args any) (mcpRawResponse, mcpRawCallResult) {
	t.Helper()
	status, res := mcpPost(t, hs, bearer, "tools/call", map[string]any{"name": name, "arguments": args})
	if status != http.StatusOK {
		t.Fatalf("tools/call status = %d, want 200", status)
	}
	var cr mcpRawCallResult
	if res.Error == nil && len(res.Result) > 0 {
		if err := json.Unmarshal(res.Result, &cr); err != nil {
			t.Fatalf("decode call result: %v", err)
		}
	}
	return res, cr
}

// #116 AC: auth requires a valid bearer token.
func TestMCPBearerAuth(t *testing.T) {
	_, _, hs := newTestMCP(t, false, nil)

	res := getURL(t, hs.URL+"/mcp", "", "")
	if res.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET /mcp = %d, want 405 (POST only)", res.StatusCode)
	}
	res.Body.Close()

	status, _ := mcpPost(t, hs, "", "initialize", nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("no bearer = %d, want 401", status)
	}
	status, _ = mcpPost(t, hs, "wrong-token", "initialize", nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("bad bearer = %d, want 401", status)
	}
	status, out := mcpPost(t, hs, "secret-token", "initialize", nil)
	if status != http.StatusOK {
		t.Fatalf("valid bearer = %d, want 200", status)
	}
	var init struct {
		ProtocolVersion string `json:"protocolVersion"`
		ServerInfo      struct {
			Name string `json:"name"`
		} `json:"serverInfo"`
	}
	if err := json.Unmarshal(out.Result, &init); err != nil {
		t.Fatal(err)
	}
	if init.ProtocolVersion != mcpProtocolVersion || init.ServerInfo.Name != "konoha" {
		t.Fatalf("initialize = %+v, want protocol %s / konoha", init, mcpProtocolVersion)
	}
}

// #116 AC: the surface is read-only by default; tools/list exposes only the read
// tools and every tool carries a JSON-schema input.
func TestMCPToolsListDefaultReadOnly(t *testing.T) {
	srv, _, hs := newTestMCP(t, false, nil)

	_, out := mcpPost(t, hs, "secret-token", "tools/list", nil)
	var list struct {
		Tools []struct {
			Name        string         `json:"name"`
			Description string         `json:"description"`
			InputSchema map[string]any `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(out.Result, &list); err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, tool := range list.Tools {
		got[tool.Name] = true
		if tool.Description == "" || tool.InputSchema["type"] != "object" {
			t.Fatalf("tool %q missing description/schema: %+v", tool.Name, tool)
		}
	}
	for _, want := range []string{"list_cycles", "get_cycle", "get_evidence", "environment_list", "environment_log", "environment_diff"} {
		if !got[want] {
			t.Fatalf("tools/list missing %q: %v", want, got)
		}
	}
	if got["post_decision"] {
		t.Fatal("post_decision must be default-closed")
	}
	if _, ok := srv.lookupTool("post_decision"); ok {
		t.Fatal("post_decision must not be exposed without control")
	}
	// A mutating environment tool is never exposed.
	for name := range got {
		for _, bad := range []string{"environment_create", "environment_write", "environment_delete", "environment_exec", "environment_run", "environment_set", "environment_remove"} {
			if name == bad {
				t.Fatalf("mutating environment tool %q exposed", name)
			}
		}
	}
}

// mustJSON renders a value deterministically for equality comparison.
func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// #116 AC: list_cycles / get_cycle / get_evidence return the same data as the
// console for the same cycle.
func TestMCPReadToolsMatchConsoleData(t *testing.T) {
	o := CycleOutcome{
		WorkflowID:    "duck-issue-7",
		Owner:         "acme",
		Repo:          "duck",
		Issue:         7,
		TerminalState: stateClosed,
		Route:         "SMALL",
		Complexity:    3,
	}
	_, store, hs := newTestMCP(t, false, nil)
	b, _ := json.MarshalIndent(o, "", "  ")
	if err := store.Write(o.WorkflowID, evidenceOutcome, string(b), false); err != nil {
		t.Fatal(err)
	}
	if err := store.Write(o.WorkflowID, evidenceStdout, "agent said hello", false); err != nil {
		t.Fatal(err)
	}
	data := &consoleData{store: store}
	ctx := context.Background()

	// list_cycles
	wantCycles, err := data.listCycles(ctx)
	if err != nil {
		t.Fatal(err)
	}
	raw, cr := mcpToolCall(t, hs, "secret-token", "list_cycles", map[string]any{})
	var board struct {
		Cycles []CycleSummary `json:"cycles"`
	}
	if err := json.Unmarshal(cr.StructuredContent, &board); err != nil {
		t.Fatal(err)
	}
	if mustJSON(t, board.Cycles) != mustJSON(t, wantCycles) {
		t.Fatalf("list_cycles = %s, want %s", mustJSON(t, board.Cycles), mustJSON(t, wantCycles))
	}

	// get_cycle
	wantDetail, ok, err := data.cycleDetail(ctx, "duck-issue-7")
	if err != nil || !ok {
		t.Fatalf("console detail ok=%v err=%v", ok, err)
	}
	_, cr = mcpToolCall(t, hs, "secret-token", "get_cycle", map[string]any{"id": "duck-issue-7"})
	if mustJSON(t, cr.StructuredContent) != mustJSON(t, wantDetail) {
		t.Fatalf("get_cycle = %s, want %s", mustJSON(t, cr.StructuredContent), mustJSON(t, wantDetail))
	}

	// get_evidence (listing)
	wantViews, err := store.artifactViews("duck-issue-7")
	if err != nil {
		t.Fatal(err)
	}
	_, cr = mcpToolCall(t, hs, "secret-token", "get_evidence", map[string]any{"id": "duck-issue-7"})
	var ev struct {
		Artifacts []ArtifactView `json:"artifacts"`
	}
	if err := json.Unmarshal(cr.StructuredContent, &ev); err != nil {
		t.Fatal(err)
	}
	if mustJSON(t, ev.Artifacts) != mustJSON(t, wantViews) {
		t.Fatalf("get_evidence = %s, want %s", mustJSON(t, ev.Artifacts), mustJSON(t, wantViews))
	}

	// unknown cycle → isError, not a crash.
	raw, _ = mcpToolCall(t, hs, "secret-token", "get_cycle", map[string]any{"id": "ghost-issue-99"})
	if raw.Error != nil || !callResultIsError(t, raw) {
		t.Fatalf("unknown cycle must be a tool error, got %s", string(raw.Result))
	}
}

// callResultIsError reports whether a tools/call result set isError.
func callResultIsError(t *testing.T, res mcpRawResponse) bool {
	t.Helper()
	var cr mcpRawCallResult
	if err := json.Unmarshal(res.Result, &cr); err != nil {
		t.Fatal(err)
	}
	return cr.IsError
}

// #116 AC: environment_list/log/diff return the same data as the CLI (#99); no
// mutating environment tool is exposed.
func TestMCPEnvironmentTools(t *testing.T) {
	_, store, hs := newTestMCP(t, false, nil)
	id := "duck-issue-9"
	for name, content := range map[string]string{
		evidenceOutcome:    `{"workflowId":"duck-issue-9"}`,
		evidenceStdout:     "agent said hello",
		evidenceStderr:     "warn: stderr text",
		evidenceValidation: "all gates passed",
		evidenceDiff:       "diff --git a/x b/x\n+hi\n",
	} {
		if err := store.Write(id, name, content, false); err != nil {
			t.Fatal(err)
		}
	}

	_, cr := mcpToolCall(t, hs, "secret-token", "environment_list", map[string]any{"id": id})
	var list struct {
		Artifacts []ArtifactView `json:"artifacts"`
	}
	if err := json.Unmarshal(cr.StructuredContent, &list); err != nil {
		t.Fatal(err)
	}
	if !contains(artifactPaths(list.Artifacts), evidenceStdout) {
		t.Fatalf("environment_list = %+v, want %s", list.Artifacts, evidenceStdout)
	}

	_, cr = mcpToolCall(t, hs, "secret-token", "environment_log", map[string]any{"id": id})
	var lg struct {
		Log string `json:"log"`
	}
	if err := json.Unmarshal(cr.StructuredContent, &lg); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"agent said hello", "warn: stderr text", "all gates passed"} {
		if !strings.Contains(lg.Log, want) {
			t.Fatalf("environment_log = %q, want %q", lg.Log, want)
		}
	}

	_, cr = mcpToolCall(t, hs, "secret-token", "environment_diff", map[string]any{"id": id})
	var df struct {
		Diff string `json:"diff"`
	}
	if err := json.Unmarshal(cr.StructuredContent, &df); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(df.Diff, "+hi") {
		t.Fatalf("environment_diff = %q, want the recorded diff", df.Diff)
	}
}

func artifactPaths(views []ArtifactView) []string {
	out := make([]string, 0, len(views))
	for _, v := range views {
		out = append(out, v.Path)
	}
	return out
}

// #116 AC: get_evidence output is redacted (#82) and path-validated (#83).
func TestMCPEvidenceRedactedAndPathValidated(t *testing.T) {
	_, store, hs := newTestMCP(t, false, nil)
	id := "duck-issue-11"
	if err := store.Write(id, evidenceOutcome, `{"workflowId":"duck-issue-11"}`, false); err != nil {
		t.Fatal(err)
	}
	secret := "sk-ABCDEFGHIJKLMNOPQRSTUV"
	if err := store.Write(id, evidenceStdout, "token "+secret+" end", false); err != nil {
		t.Fatal(err)
	}

	_, cr := mcpToolCall(t, hs, "secret-token", "get_evidence", map[string]any{"id": id, "name": evidenceStdout})
	var ev struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(cr.StructuredContent, &ev); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(ev.Content, secret) {
		t.Fatalf("get_evidence leaked a secret: %q", ev.Content)
	}
	if !strings.Contains(ev.Content, redacted) {
		t.Fatalf("get_evidence content = %q, want %s", ev.Content, redacted)
	}

	// Traversal path → tool error, never a read outside the cycle.
	raw, _ := mcpToolCall(t, hs, "secret-token", "get_evidence", map[string]any{"id": id, "name": "../outcome.json"})
	if raw.Error != nil || !callResultIsError(t, raw) {
		t.Fatalf("traversal path must be a tool error, got %s", string(raw.Result))
	}
}

// #116 AC: any optional control tool is default-closed and writes only through
// the authoritative §18 channel (#108).
func TestMCPControlDefaultClosedUsesDecisionChannel(t *testing.T) {
	// Default-closed: an explicit tools/call is refused.
	_, _, hs := newTestMCP(t, false, nil)
	raw, _ := mcpToolCall(t, hs, "secret-token", "post_decision", map[string]any{"id": "r-issue-7", "action": "approve", "attempt": 2, "sha": "sha2"})
	if raw.Error == nil || raw.Error.Code != mcpErrInvalidParams {
		t.Fatalf("post_decision while disabled = %+v, want invalid-params error", raw)
	}

	// Control enabled: it routes through the console's #108 decision writer.
	store, err := NewEvidenceStore(EvidenceConfig{Dir: t.TempDir()}, &defaultRedactor)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.recordActivation("o", "r", 7, LineageActivation{
		Attempt: 2, WorkflowID: "wf", Reason: activationReasonInitial, CommitSHA: "sha2",
	}, false); err != nil {
		t.Fatal(err)
	}
	seen := &[]DecisionRequest{}
	data := &consoleData{
		store: store,
		repos: map[string]RepoConfig{"r": {Owner: "o", Repo: "r"}},
		status: func(context.Context, string) (StatusQuery, bool) {
			return StatusQuery{State: statePendingApproval}, true
		},
		decide: func(_ context.Context, in DecisionRequest) (DecisionResult, error) {
			*seen = append(*seen, in)
			return DecisionResult{Accepted: true, Action: in.Action, Attempt: in.Attempt, SHA: in.SHA}, nil
		},
	}
	srv, _, hs2 := newTestMCP(t, true, data)
	if _, ok := srv.lookupTool("post_decision"); !ok {
		t.Fatal("post_decision must be exposed when control is enabled")
	}
	_, out := mcpPost(t, hs2, "secret-token", "tools/list", nil)
	if !strings.Contains(string(out.Result), "post_decision") {
		t.Fatal("tools/list must include post_decision when control is enabled")
	}

	raw, _ = mcpToolCall(t, hs2, "secret-token", "post_decision", map[string]any{
		"id": "r-issue-7", "action": "approve", "attempt": 2, "sha": "sha2",
	})
	if raw.Error != nil || callResultIsError(t, raw) {
		t.Fatalf("post_decision failed: %s", string(raw.Result))
	}
	if len(*seen) != 1 {
		t.Fatalf("decision writer calls = %d, want 1", len(*seen))
	}
	got := (*seen)[0]
	if got.Owner != "o" || got.Repo != "r" || got.Issue != 7 || got.CycleID != "r-issue-7" || got.Action != humanDecisionApprove {
		t.Fatalf("mapped decision = %+v", got)
	}

	// A stale attempt is refused (the #108 gate binding still applies).
	raw, _ = mcpToolCall(t, hs2, "secret-token", "post_decision", map[string]any{
		"id": "r-issue-7", "action": "approve", "attempt": 1, "sha": "sha1",
	})
	if raw.Error != nil || !callResultIsError(t, raw) {
		t.Fatalf("stale attempt must be a tool error, got %s", string(raw.Result))
	}
}

// #116 AC: the MCP process reads no GitHub credential. mcpConfigFromEnv reads
// only KONOHA_MCP_* keys; the source references no GH_* variable.
func TestMCPReadsNoGitHubCredential(t *testing.T) {
	var read []string
	getenv := func(k string) string {
		read = append(read, k)
		return os.Getenv(k)
	}
	_ = mcpConfigFromEnv(getenv)
	for _, k := range read {
		if strings.HasPrefix(k, "GH_") || k == "GITHUB_TOKEN" {
			t.Fatalf("mcp read GitHub credential env %q", k)
		}
	}
	if len(read) == 0 {
		t.Fatal("expected the mcp surface to read its own env keys")
	}

	src, err := os.ReadFile("mcp.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{`"GH_`, "GH_APP", "GITHUB_TOKEN", "appToken("} {
		if strings.Contains(string(src), bad) {
			t.Fatalf("mcp.go references %q; the MCP process must not read GitHub credentials", bad)
		}
	}
}
