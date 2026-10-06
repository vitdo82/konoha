package konoha

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// #105 AC: a non-loopback bind address is rejected; loopback (incl. localhost /
// ::1) is accepted; an empty host (all interfaces) is not loopback.
func TestConsoleAddrLoopbackOnly(t *testing.T) {
	reject := []string{"0.0.0.0:8080", ":8080", "192.168.1.5:9000", "10.0.0.1:80", "example.com:80", "127.0.0.1"}
	for _, a := range reject {
		if err := validateLoopbackAddr(a); err == nil {
			t.Fatalf("validateLoopbackAddr(%q) = nil, want an error", a)
		}
	}
	accept := []string{"127.0.0.1:8080", "127.0.0.1:0", "[::1]:9000", "localhost:1234"}
	for _, a := range accept {
		if err := validateLoopbackAddr(a); err != nil {
			t.Fatalf("validateLoopbackAddr(%q) = %v, want nil", a, err)
		}
	}
}

// #105 AC: the console is disabled by default (no listener unless the env var
// is set) and a non-loopback address fails fast at construction.
func TestConsoleDisabledAndFailClosed(t *testing.T) {
	empty := func(string) string { return "" }
	cfg := consoleConfigFromEnv(empty)
	if cfg.enabled() {
		t.Fatal("console must be disabled with no address")
	}
	srv, err := newConsoleServer(cfg, &consoleData{}, "dev")
	if err != nil || srv != nil {
		t.Fatalf("disabled console = (%v, %v), want (nil, nil)", srv, err)
	}
	if _, err := newConsoleServer(ConsoleConfig{Addr: "0.0.0.0:8080", Token: "t"}, &consoleData{}, "dev"); err == nil {
		t.Fatal("non-loopback console addr must fail at construction")
	}
}

// #105 AC: the startup entrypoint honors the disable-by-default rule and
// rejects a non-loopback KONOHA_CONSOLE_ADDR before any listener starts.
func TestStartConsoleServerEnv(t *testing.T) {
	t.Setenv(consoleAddrEnv, "")
	if srv, err := StartConsoleServer(nil, &EvidenceStore{}, "dev"); err != nil || srv != nil {
		t.Fatalf("disabled StartConsoleServer = (%v, %v), want (nil, nil)", srv, err)
	}
	t.Setenv(consoleAddrEnv, "0.0.0.0:8765")
	if srv, err := StartConsoleServer(nil, &EvidenceStore{}, "dev"); err == nil || srv != nil {
		t.Fatalf("non-loopback StartConsoleServer = (%v, %v), want (nil, error)", srv, err)
	}
}

// #105: a console with no configured bearer token generates a per-process one.
func TestConsoleGeneratesTokenWhenUnset(t *testing.T) {
	srv, err := newConsoleServer(ConsoleConfig{Addr: "127.0.0.1:0"}, &consoleData{}, "dev")
	if err != nil {
		t.Fatal(err)
	}
	if srv.token == "" || !srv.generatedToken {
		t.Fatalf("token = %q generated=%v, want a generated token", srv.token, srv.generatedToken)
	}
}

// newTestConsole builds a console over a temp evidence store; the httptest
// server carries a known bearer token.
func newTestConsole(t *testing.T, o CycleOutcome) (*ConsoleServer, *EvidenceStore, *httptest.Server) {
	t.Helper()
	store, err := NewEvidenceStore(EvidenceConfig{Dir: t.TempDir()}, &defaultRedactor)
	if err != nil {
		t.Fatalf("evidence store: %v", err)
	}
	if o.WorkflowID != "" {
		b, _ := json.MarshalIndent(o, "", "  ")
		if err := store.Write(o.WorkflowID, evidenceOutcome, string(b), false); err != nil {
			t.Fatalf("write outcome: %v", err)
		}
		if err := store.Write(o.WorkflowID, evidenceStdout, "agent said hello", false); err != nil {
			t.Fatalf("write stdout: %v", err)
		}
	}
	srv, err := newConsoleServer(ConsoleConfig{Addr: "127.0.0.1:0", Token: "secret-token"}, &consoleData{store: store}, "test-1")
	if err != nil {
		t.Fatalf("new console: %v", err)
	}
	hs := httptest.NewServer(srv)
	t.Cleanup(hs.Close)
	return srv, store, hs
}

func getURL(t *testing.T, url, bearer, csrf string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if csrf != "" {
		req.Header.Set(consoleCSRFHeader, csrf)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// #105 AC: requests without a valid bearer token are rejected with a
// WWW-Authenticate: Bearer challenge; the correct token is accepted.
func TestConsoleBearerAuth(t *testing.T) {
	_, _, hs := newTestConsole(t, CycleOutcome{})

	res := getURL(t, hs.URL+"/api/v1/cycles", "", "")
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no auth status = %d, want 401", res.StatusCode)
	}
	if got := res.Header.Get("WWW-Authenticate"); !strings.Contains(got, "Bearer") {
		t.Fatalf("WWW-Authenticate = %q, want a Bearer challenge", got)
	}
	res.Body.Close()

	res = getURL(t, hs.URL+"/api/v1/cycles", "wrong-token", "")
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad bearer status = %d, want 401", res.StatusCode)
	}
	res.Body.Close()

	res = getURL(t, hs.URL+"/api/v1/cycles", "secret-token", "")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("valid bearer status = %d, want 200", res.StatusCode)
	}
	res.Body.Close()
}

// #105 AC: GET /api/v1/status issues a CSRF token and the token unlocks the API.
func TestConsoleCSRFPlumbing(t *testing.T) {
	_, _, hs := newTestConsole(t, CycleOutcome{})

	res := getURL(t, hs.URL+"/api/v1/status", "", "")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	var st consoleStatus
	if err := json.NewDecoder(res.Body).Decode(&st); err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if st.CSRFToken == "" || st.CSRFHeader == "" || !st.ReadOnly || st.Version != "test-1" {
		t.Fatalf("status = %+v, want a CSRF token + version + read-only", st)
	}
	if st.CSRFToken == "secret-token" {
		t.Fatal("CSRF token must be independent of the bearer token")
	}

	res = getURL(t, hs.URL+"/api/v1/cycles", "", st.CSRFToken)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("CSRF-authenticated request = %d, want 200", res.StatusCode)
	}
	res.Body.Close()

	// The plumbing is present for the future mutations ticket: browser mutation
	// needs CSRF + same-origin, CLI mutation needs the bearer token.
	srv, _, _ := newTestConsole(t, CycleOutcome{})
	post := func(bearer, csrf, origin string) bool {
		req, _ := http.NewRequest(http.MethodPost, "/", nil)
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		if csrf != "" {
			req.Header.Set(consoleCSRFHeader, csrf)
		}
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		return srv.mutationAllowed(req)
	}
	if post("", srv.csrf, "") != true {
		t.Fatal("csrf + no origin (non-browser) must be allowed")
	}
	if post("", srv.csrf, "http://127.0.0.1:8080") != true {
		t.Fatal("csrf + loopback origin must be allowed")
	}
	if post("", srv.csrf, "https://evil.example") != false {
		t.Fatal("csrf + foreign origin must be rejected")
	}
	if post("secret-token", "", "https://evil.example") != true {
		t.Fatal("bearer (CLI/worker) mutation must not require Origin")
	}
	if post("", "", "") != false {
		t.Fatal("no credential must be rejected")
	}
}

// #105 AC: the board and cycle detail render for a finished cycle with NO
// Temporal server (evidence-store-only).
func TestConsoleEvidenceOnlyBoardAndDetail(t *testing.T) {
	o := CycleOutcome{
		WorkflowID:    "duck-issue-7",
		Owner:         "acme",
		Repo:          "duck",
		Issue:         7,
		TerminalState: stateClosed,
		Route:         "SMALL",
		Complexity:    3,
	}
	_, _, hs := newTestConsole(t, o)

	res := getURL(t, hs.URL+"/api/v1/cycles", "secret-token", "")
	var board struct {
		Cycles []CycleSummary `json:"cycles"`
	}
	if err := json.NewDecoder(res.Body).Decode(&board); err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if len(board.Cycles) != 1 || board.Cycles[0].ID != "duck-issue-7" || board.Cycles[0].State != stateClosed {
		t.Fatalf("board = %+v, want the finished cycle", board.Cycles)
	}

	res = getURL(t, hs.URL+"/api/v1/cycles/duck-issue-7", "secret-token", "")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("detail = %d, want 200", res.StatusCode)
	}
	var detail CycleDetail
	if err := json.NewDecoder(res.Body).Decode(&detail); err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if detail.Outcome == nil || detail.Outcome.Owner != "acme" || detail.State != stateClosed {
		t.Fatalf("detail = %+v, want the full outcome", detail)
	}
	if !contains(detail.Artifacts, evidenceOutcome) {
		t.Fatalf("artifacts = %v, want %s", detail.Artifacts, evidenceOutcome)
	}

	res = getURL(t, hs.URL+"/api/v1/cycles/duck-issue-7/artifacts", "secret-token", "")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("artifacts = %d, want 200", res.StatusCode)
	}
	res.Body.Close()
}

// #105 AC: unknown cycle → 404; malformed id → 400.
func TestConsoleNotFoundAndMalformed(t *testing.T) {
	_, _, hs := newTestConsole(t, CycleOutcome{})

	res := getURL(t, hs.URL+"/api/v1/cycles/ghost-issue-99", "secret-token", "")
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown cycle = %d, want 404", res.StatusCode)
	}
	res.Body.Close()

	res = getURL(t, hs.URL+"/api/v1/cycles/bad%20id", "secret-token", "")
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed id = %d, want 400", res.StatusCode)
	}
	res.Body.Close()
}

// #105 AC: the console process reads no GitHub credential. consoleConfigFromEnv
// reads only KONOHA_CONSOLE_* keys; the source references no GH_* variable.
func TestConsoleReadsNoGitHubCredential(t *testing.T) {
	var read []string
	getenv := func(k string) string {
		read = append(read, k)
		return os.Getenv(k)
	}
	_ = consoleConfigFromEnv(getenv)
	for _, k := range read {
		if strings.HasPrefix(k, "GH_") || k == "GITHUB_TOKEN" {
			t.Fatalf("console read GitHub credential env %q", k)
		}
	}
	if len(read) == 0 {
		t.Fatal("expected the console to read its own env keys")
	}

	src, err := os.ReadFile("console.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{`"GH_`, "GH_APP", "GITHUB_TOKEN", "appToken("} {
		if strings.Contains(string(src), bad) {
			t.Fatalf("console.go references %q; the console must not read GitHub credentials", bad)
		}
	}
}

// #105 AC: CSP is set with no inline script; the SPA assets are served.
func TestConsoleSecurityHeadersAndSPA(t *testing.T) {
	_, _, hs := newTestConsole(t, CycleOutcome{})

	res := getURL(t, hs.URL+"/", "", "")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("index = %d, want 200", res.StatusCode)
	}
	csp := res.Header.Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'none'", "script-src 'self'", "connect-src 'self'"} {
		if !strings.Contains(csp, want) {
			t.Fatalf("CSP = %q, want %q", csp, want)
		}
	}
	if res.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("missing X-Content-Type-Options: nosniff")
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if !strings.Contains(string(body), "/assets/app.js") {
		t.Fatalf("index must reference the external SPA script: %s", body)
	}
	if strings.Contains(string(body), "<script>") {
		t.Fatal("index must not contain an inline <script>")
	}

	res = getURL(t, hs.URL+"/assets/app.js", "", "")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("app.js = %d, want 200", res.StatusCode)
	}
	res.Body.Close()

	res = getURL(t, hs.URL+"/api/v1/nope", "secret-token", "")
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown api = %d, want 404", res.StatusCode)
	}
	res.Body.Close()
}

// #106 AC1: the board groups cycles into the five fixed columns, with
// deterministic newest-first ordering, without a browser or Temporal.
func TestBoardColumnsProjection(t *testing.T) {
	now := time.Now().UTC()
	cycles := []CycleSummary{
		{ID: "a-issue-1", State: stateClosed, At: now.Add(-time.Hour)},
		{ID: "b-issue-2", State: stateClosed, At: now},
		{ID: "c-issue-3", State: stateBlocked},
		{ID: "d-issue-4", State: statePendingApproval},
		{ID: "e-issue-5", State: stateHumanRequired},
		{ID: "f-issue-6", State: stateFailed},
		{ID: "g-issue-7", State: stateDeveloping, Running: true},
		{ID: "h-issue-8", State: stateCancelled},
		{ID: "i-issue-9", State: "SOMETHING_ELSE"},
	}
	cols := boardColumns(cycles)
	if len(cols) != 5 {
		t.Fatalf("columns = %d, want 5", len(cols))
	}
	wantOrder := []string{boardColRunning, boardColAttention, boardColHuman, boardColClosed, boardColFailed}
	byID := map[string]BoardColumn{}
	for i, c := range cols {
		if c.ID != wantOrder[i] {
			t.Fatalf("column %d = %q, want %q", i, c.ID, wantOrder[i])
		}
		byID[c.ID] = c
	}
	if n := len(byID[boardColRunning].Cards); n != 1 {
		t.Fatalf("running cards = %d, want 1", n)
	}
	if n := len(byID[boardColAttention].Cards); n != 2 {
		t.Fatalf("attention cards = %d, want 2 (blocked + pending-approval)", n)
	}
	if n := len(byID[boardColHuman].Cards); n != 1 {
		t.Fatalf("human cards = %d, want 1", n)
	}
	if n := len(byID[boardColFailed].Cards); n != 3 {
		t.Fatalf("failed cards = %d, want 3 (failed + cancelled + unknown)", n)
	}
	// Newest first within a column.
	closed := byID[boardColClosed].Cards
	if len(closed) != 2 || closed[0].ID != "b-issue-2" {
		t.Fatalf("closed cards = %+v, want b-issue-2 first (newest)", closed)
	}
	for _, c := range cols {
		if c.Count != len(c.Cards) {
			t.Fatalf("column %s count %d != len(cards) %d", c.ID, c.Count, len(c.Cards))
		}
	}
}

// #106 AC2: the canonical agent-SUCCESS / verification-FAILED mismatch is a
// distinct predicate, never collapsed.
func TestOutcomesConflict(t *testing.T) {
	cases := []struct {
		agent AgentOutcome
		ver   VerificationOutcome
		want  bool
	}{
		{agentOutcomeSuccess, verificationFailed, true},
		{agentOutcomeSuccess, verificationError, true},
		{agentOutcomeSuccess, verificationSuccess, false},
		{agentOutcomeSuccess, verificationSkipped, false},
		{agentOutcomeBlocked, verificationFailed, false},
		{agentOutcomeSkipped, verificationSkipped, false},
	}
	for _, tc := range cases {
		if got := outcomesConflict(tc.agent, tc.ver); got != tc.want {
			t.Fatalf("outcomesConflict(%s,%s) = %v, want %v", tc.agent, tc.ver, got, tc.want)
		}
	}
}

// #106 AC4/AC6: the diff parses into changed files with per-file stats, and
// comments group into line-anchored threads (deterministic ordering).
func TestParseUnifiedDiffAndThreads(t *testing.T) {
	patch := "diff --git a/a.go b/a.go\n" +
		"--- a/a.go\n+++ b/a.go\n" +
		"@@ -1,2 +1,2 @@\n-old\n+new\n ctx\n"
	files := parseUnifiedDiff(patch)
	if len(files) != 1 || files[0].Path != "a.go" || files[0].Added != 1 || files[0].Removed != 1 {
		t.Fatalf("parseUnifiedDiff = %+v, want a.go +1/-1", files)
	}
	threads := diffThreads([]DiffComment{
		{File: "b.go", Line: 5, Body: "b"},
		{File: "a.go", Line: 2, Body: "a2"},
		{File: "a.go", Line: 1, Body: "a1-first"},
		{File: "a.go", Line: 1, Body: "a1-second"},
	})
	if len(threads) != 3 {
		t.Fatalf("threads = %d, want 3", len(threads))
	}
	if threads[0].File != "a.go" || threads[0].Line != 1 || len(threads[0].Comments) != 2 {
		t.Fatalf("thread[0] = %+v, want a.go:1 with 2 comments", threads[0])
	}
	if threads[0].Comments[0].Body != "a1-first" || threads[0].Comments[1].Body != "a1-second" {
		t.Fatalf("thread[0] comment order = %+v, want input order preserved", threads[0].Comments)
	}
	if threads[2].File != "b.go" || threads[2].Line != 5 {
		t.Fatalf("thread[2] = %+v, want b.go:5 last", threads[2])
	}
}

// #106 AC1/AC5/AC7: the board endpoint returns the grouped columns and the
// summary projection carries priority/assignee when present (absent = empty).
func TestConsoleBoardColumnsAndCardMeta(t *testing.T) {
	store, err := NewEvidenceStore(EvidenceConfig{Dir: t.TempDir()}, &defaultRedactor)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range []CycleOutcome{
		{WorkflowID: "duck-issue-7", Owner: "acme", Repo: "duck", Issue: 7,
			TerminalState: stateClosed, Priority: "P1", Assignee: "vitali"},
		{WorkflowID: "duck-issue-8", Owner: "acme", Repo: "duck", Issue: 8,
			TerminalState: stateBlocked},
	} {
		b, _ := json.Marshal(o)
		if err := store.Write(o.WorkflowID, evidenceOutcome, string(b), false); err != nil {
			t.Fatal(err)
		}
	}
	srv, err := newConsoleServer(ConsoleConfig{Addr: "127.0.0.1:0", Token: "secret-token"}, &consoleData{store: store}, "test-1")
	if err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(srv)
	t.Cleanup(hs.Close)

	res := getURL(t, hs.URL+"/api/v1/cycles", "secret-token", "")
	var body struct {
		Columns []BoardColumn `json:"columns"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if len(body.Columns) != 5 {
		t.Fatalf("columns = %d, want 5", len(body.Columns))
	}
	closed := body.Columns[3]
	if closed.ID != boardColClosed || len(closed.Cards) != 1 {
		t.Fatalf("closed column = %+v, want one card", closed)
	}
	card := closed.Cards[0]
	if card.Priority != "P1" || card.Assignee != "vitali" {
		t.Fatalf("card meta = %+v, want priority P1 + assignee vitali", card)
	}
	blocked := body.Columns[1]
	if len(blocked.Cards) != 1 || blocked.Cards[0].Priority != "" || blocked.Cards[0].Assignee != "" {
		t.Fatalf("blocked column = %+v, want one card with absent meta", blocked)
	}
}

// #106 AC2/AC3/AC4/AC6: an evidence-backed detail separates the outcomes, keeps
// conditions ordered, and renders diff + per-gate validation + anchored threads.
func TestConsoleDetailProjections(t *testing.T) {
	store, err := NewEvidenceStore(EvidenceConfig{Dir: t.TempDir()}, &defaultRedactor)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	o := CycleOutcome{
		WorkflowID: "duck-issue-7", Owner: "acme", Repo: "duck", Issue: 7,
		TerminalState:       stateHumanRequired,
		AgentOutcome:        agentOutcomeSuccess,
		VerificationOutcome: verificationFailed,
		Priority:            "P1",
		Assignee:            "vitali",
		Conditions: []Condition{
			{Type: conditionAgent, Status: string(agentOutcomeSuccess), At: now},
			{Type: conditionVerification, Status: string(verificationFailed), At: now},
			{Type: conditionTerminal, Status: stateHumanRequired, Reason: "verification-failed", At: now},
		},
	}
	b, _ := json.MarshalIndent(o, "", "  ")
	writes := map[string]string{
		evidenceOutcome: string(b),
		evidenceDiff: "diff --git a/x.go b/x.go\n--- a/x.go\n+++ b/x.go\n" +
			"@@ -1,1 +1,2 @@\n old\n+new\n",
		evidenceValidation: "go test ./... ok",
		evidenceComments:   `[{"id":"c1","file":"x.go","line":2,"body":"nit here","author":"alice"}]`,
	}
	for name, content := range writes {
		if err := store.Write("duck-issue-7", name, content, false); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.WriteVerification("duck-issue-7", "unit", "stdout", "ok", false); err != nil {
		t.Fatal(err)
	}
	srv, err := newConsoleServer(ConsoleConfig{Addr: "127.0.0.1:0", Token: "secret-token"}, &consoleData{store: store}, "test-1")
	if err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(srv)
	t.Cleanup(hs.Close)

	res := getURL(t, hs.URL+"/api/v1/cycles/duck-issue-7", "secret-token", "")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("detail = %d, want 200", res.StatusCode)
	}
	var d CycleDetail
	if err := json.NewDecoder(res.Body).Decode(&d); err != nil {
		t.Fatal(err)
	}
	res.Body.Close()

	if d.Outcome == nil || d.Outcome.AgentOutcome != agentOutcomeSuccess || d.Outcome.VerificationOutcome != verificationFailed {
		t.Fatalf("outcomes = %+v, want agent SUCCESS + verification FAILED distinct", d.Outcome)
	}
	if !d.OutcomeConflict {
		t.Fatal("OutcomeConflict = false, want true for SUCCESS/FAILED")
	}
	if len(d.Outcome.Conditions) != 3 ||
		d.Outcome.Conditions[0].Type != conditionAgent ||
		d.Outcome.Conditions[1].Type != conditionVerification ||
		d.Outcome.Conditions[2].Type != conditionTerminal {
		t.Fatalf("conditions = %+v, want agent→verification→terminal order", d.Outcome.Conditions)
	}
	if d.Diff == nil || len(d.Diff.Files) != 1 || d.Diff.Files[0].Path != "x.go" || d.Diff.Files[0].Added != 1 {
		t.Fatalf("diff = %+v, want x.go +1", d.Diff)
	}
	if len(d.Diff.Threads) != 1 || d.Diff.Threads[0].File != "x.go" || d.Diff.Threads[0].Line != 2 ||
		d.Diff.Threads[0].Comments[0].Body != "nit here" {
		t.Fatalf("threads = %+v, want x.go:2 nit here", d.Diff.Threads)
	}
	if d.Validation == nil || d.Validation.Outcome != verificationFailed ||
		!strings.Contains(d.Validation.Output, "go test") ||
		len(d.Validation.Gates) != 1 || d.Validation.Gates[0].ID != "unit" || d.Validation.Gates[0].Stdout != "ok" {
		t.Fatalf("validation = %+v, want outcome + combined output + unit gate", d.Validation)
	}
	if d.Priority != "P1" || d.Assignee != "vitali" {
		t.Fatalf("meta = %+v, want priority P1 + assignee vitali", d)
	}
}

// #112 AC1/AC2: the metrics dashboard projection renders the §39 gauges,
// throughput, and #87 cost/tokens from evidence; absent data is nil ("no data"),
// never a fabricated zero, and a registry snapshot takes precedence when present.
func TestBuildMetricsView(t *testing.T) {
	find := func(mv MetricsView, key string) *MetricView {
		for i := range mv.Metrics {
			if mv.Metrics[i].Key == key {
				return &mv.Metrics[i]
			}
		}
		t.Fatalf("metric %q missing from %+v", key, mv.Metrics)
		return nil
	}
	now := time.Now().UTC()
	outcomes := []CycleOutcome{
		{TerminalState: stateClosed, AutonomousClose: true, Route: stateSmall,
			ReviewRounds: 2, Disagreements: 1,
			Conditions: []Condition{{At: now.Add(-48 * time.Hour)}}},
		{TerminalState: stateClosed, Route: stateLarge, ReviewRounds: 2,
			Usage:      HarnessUsage{TokensIn: 100, TokensOut: 50, CostUSD: 2},
			Conditions: []Condition{{At: now}}},
		{TerminalState: stateBlocked, HumanRequired: true},
	}
	mv := buildMetricsView(nil, outcomes)
	if mv.Completed != 2 || mv.WindowDays != 2 {
		t.Fatalf("completed=%d windowDays=%d, want 2/2", mv.Completed, mv.WindowDays)
	}
	check := func(key string, want float64) {
		v := find(mv, key).Value
		if v == nil || *v != want {
			t.Fatalf("%s = %v, want %v", key, v, want)
		}
	}
	check(mAutonomousRate, 0.5)          // 1 autonomous / 2 closed
	check(mSplitRate, 0.5)               // 1 large / 2 routed
	check(mReviewDisagreementRate, 0.25) // 1 / 4 rounds
	check("throughput_per_day", 1)       // 2 closed over 2 days
	check("tokens_in_per_completed_issue", 50)
	check("tokens_out_per_completed_issue", 25)
	check(mCostPerCompletedIssue, 1)
	check(mAgentCostUSD, 2)
	// human_escalation_rate = 1 / 3 outcomes
	check(mDidHumanEscalationRate, 1.0/3.0)

	// AC2: no reports → no data (nil), never zero.
	empty := buildMetricsView(nil, nil)
	for _, key := range []string{mCostPerCompletedIssue, mAgentCostUSD, mAgentTokensIn, "throughput_per_day"} {
		if v := find(empty, key).Value; v != nil {
			t.Fatalf("%s = %v, want nil (no data)", key, *v)
		}
	}

	// The registry snapshot takes precedence for the §39 gauges + #87 counters.
	snap := map[string]float64{mAutonomousRate: 0.9, mCyclesCompleted: 4, mAgentCostUSD: 10}
	mv2 := buildMetricsView(snap, nil)
	if v := find(mv2, mAutonomousRate).Value; v == nil || *v != 0.9 {
		t.Fatalf("autonomous rate = %v, want the registry value 0.9", v)
	}
	if v := find(mv2, mCostPerCompletedIssue).Value; v == nil || *v != 2.5 {
		t.Fatalf("cost/issue = %v, want 10/4 = 2.5", v)
	}
}

// #112 AC3: the dashboard endpoint is read-only and renders over an evidence
// store with no Temporal server.
func TestConsoleMetricsEndpoint(t *testing.T) {
	store, err := NewEvidenceStore(EvidenceConfig{Dir: t.TempDir()}, &defaultRedactor)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for _, o := range []CycleOutcome{
		{WorkflowID: "duck-issue-1", TerminalState: stateClosed, AutonomousClose: true,
			Usage: HarnessUsage{TokensIn: 10, CostUSD: 1}, Conditions: []Condition{{At: now}}},
		{WorkflowID: "duck-issue-2", TerminalState: stateClosed, Conditions: []Condition{{At: now}}},
	} {
		b, _ := json.Marshal(o)
		if err := store.Write(o.WorkflowID, evidenceOutcome, string(b), false); err != nil {
			t.Fatal(err)
		}
	}
	srv, err := newConsoleServer(ConsoleConfig{Addr: "127.0.0.1:0", Token: "secret-token"}, &consoleData{store: store}, "test-1")
	if err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(srv)
	t.Cleanup(hs.Close)

	res := getURL(t, hs.URL+"/api/v1/metrics", "secret-token", "")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("metrics = %d, want 200", res.StatusCode)
	}
	var mv MetricsView
	if err := json.NewDecoder(res.Body).Decode(&mv); err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if mv.Completed != 2 || len(mv.Metrics) == 0 {
		t.Fatalf("metrics = %+v, want 2 completed + metrics", mv)
	}
	// Unauthenticated is rejected like every other data route.
	res = getURL(t, hs.URL+"/api/v1/metrics", "", "")
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated metrics = %d, want 401", res.StatusCode)
	}
	res.Body.Close()
}

func TestParseCycleID(t *testing.T) {
	repo, issue, ok := parseCycleID("my-repo-issue-42")
	if !ok || repo != "my-repo" || issue != 42 {
		t.Fatalf("parseCycleID = (%q,%d,%v)", repo, issue, ok)
	}
	for _, bad := range []string{"", "noissue", "x-review-1", "x-issue-0", "x-issue-abc"} {
		if _, _, ok := parseCycleID(bad); ok {
			t.Fatalf("parseCycleID(%q) = ok, want false", bad)
		}
	}
}
