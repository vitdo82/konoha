package konoha

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
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
