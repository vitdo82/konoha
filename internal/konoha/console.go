package konoha

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"
)

// Local review console foundation (#105). An opt-in, LOOPBACK-ONLY, read-only
// HTTP server that surfaces the board and cycle detail from the durable evidence
// store (#83) plus the Temporal status query (#89). No GitHub credentials are
// read here; no artifact content is executed; there is no mutation endpoint in
// this ticket (the CSRF/Origin plumbing is present for the decisions ticket).
//
// Auth model:
//   - API/CLI/worker clients present a bearer token (KONOHA_CONSOLE_TOKEN).
//   - Browsers fetch a per-process CSRF token from GET /api/v1/status and send
//     it on every API call; browser MUTATIONS (later) additionally check Origin.
//   - Every comparison is constant-time.

// Console environment keys (the console reads these and nothing else — it never
// touches GH_*; see TestConsoleReadsNoGitHubCredential).
const (
	consoleAddrEnv  = "KONOHA_CONSOLE_ADDR"
	consoleTokenEnv = "KONOHA_CONSOLE_TOKEN"

	// consoleCSRFHeader is the browser's CSRF token transport.
	consoleCSRFHeader = "X-Konoha-CSRF"

	// consoleCSP is the strict baseline: no inline script/style, no external
	// origins, no framing, no form submission.
	consoleCSP = "default-src 'none'; script-src 'self'; style-src 'self'; " +
		"img-src 'self' data: blob:; font-src 'self'; connect-src 'self'; " +
		"base-uri 'none'; form-action 'none'; frame-ancestors 'none'"
)

//go:embed consoleassets
var consoleAssets embed.FS

// ConsoleConfig is the resolved console configuration (env-only, default
// disabled: no listener starts unless KONOHA_CONSOLE_ADDR is set).
type ConsoleConfig struct {
	Addr  string
	Token string
}

// consoleConfigFromEnv resolves the console config through an injected getenv so
// tests can assert exactly which environment keys are read (never GH_*).
func consoleConfigFromEnv(getenv func(string) string) ConsoleConfig {
	return ConsoleConfig{
		Addr:  strings.TrimSpace(getenv(consoleAddrEnv)),
		Token: getenv(consoleTokenEnv),
	}
}

// enabled reports whether a listen address was configured.
func (c ConsoleConfig) enabled() bool { return c.Addr != "" }

// validateLoopbackAddr rejects any bind address that is not an explicit loopback
// host, so the console can never be exposed on a routable interface.
func validateLoopbackAddr(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("console addr %q: %w", addr, err)
	}
	if port == "" {
		return fmt.Errorf("console addr %q: missing port", addr)
	}
	if !isLoopbackHost(host) {
		return fmt.Errorf("console addr %q: must bind a loopback host (127.0.0.1, ::1 or localhost), not %q", addr, host)
	}
	return nil
}

// isLoopbackHost reports whether host is loopback: the literal "localhost" or a
// loopback IP. An empty host (all interfaces) is never loopback.
func isLoopbackHost(host string) bool {
	h := strings.TrimSpace(host)
	if h == "" {
		return false
	}
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// constantTimeEqual compares two secrets without early exit (length is not
// itself a secret here).
func constantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// randomToken returns a 32-byte hex token from crypto/rand.
func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// consoleVersion resolves the running build version for the status endpoint,
// falling back to "dev" for a local (devel) build.
func consoleVersion() string {
	if bi, ok := debug.ReadBuildInfo(); ok {
		if v := strings.TrimSpace(bi.Main.Version); v != "" && v != "(devel)" {
			return v
		}
	}
	return "dev"
}

// CycleSummary is one board CARD: a finished cycle (from outcome.json) or a
// running one (from the Temporal status query). At is the latest observed
// condition timestamp (deterministic newest-first board ordering); Priority and
// Assignee are projected from recorded issue intent when present and omitted
// otherwise (cards degrade gracefully).
type CycleSummary struct {
	ID         string    `json:"id"`
	Owner      string    `json:"owner,omitempty"`
	Repo       string    `json:"repo,omitempty"`
	Issue      int       `json:"issue,omitempty"`
	State      string    `json:"state"`
	Running    bool      `json:"running,omitempty"`
	Route      string    `json:"route,omitempty"`
	Complexity int       `json:"complexity,omitempty"`
	Priority   string    `json:"priority,omitempty"`
	Assignee   string    `json:"assignee,omitempty"`
	At         time.Time `json:"at,omitempty"`
}

// CycleDetail is the cycle detail view. It carries the board card plus the full
// outcome (finished) or status snapshot (running), the artifact name list, and
// the Diff/Validation tabs' projections. OutcomeConflict is the deterministic
// "summaries are not proof" flag (agent SUCCESS while verification failed) —
// the UI must never collapse it. Activations is the #88 lineage (harness/model/
// digest/usage) for the Metadata tab, when recorded.
type CycleDetail struct {
	CycleSummary
	Outcome         *CycleOutcome       `json:"outcome,omitempty"`
	Status          *StatusQuery        `json:"status,omitempty"`
	Artifacts       []string            `json:"artifacts"`
	OutcomeConflict bool                `json:"outcomeConflict,omitempty"`
	Diff            *DiffView           `json:"diff,omitempty"`
	Validation      *ValidationView     `json:"validation,omitempty"`
	Activations     []LineageActivation `json:"activations,omitempty"`
}

// consoleStatus is the GET /api/v1/status payload: version + the per-process
// CSRF token the browser uses on every subsequent call.
type consoleStatus struct {
	Name       string `json:"name"`
	Version    string `json:"version"`
	ReadOnly   bool   `json:"readOnly"`
	CSRFHeader string `json:"csrfHeader"`
	CSRFToken  string `json:"csrfToken"`
}

// consoleStatusQuery is the #89 running-cycle lookup seam (nil for an
// evidence-only server / no Temporal).
type consoleStatusQuery func(ctx context.Context, workflowID string) (StatusQuery, bool)

// consoleRunningLister enumerates in-flight cycles (nil = none). The bool
// reports whether the lookup succeeded (false = best-effort miss, never fatal).
type consoleRunningLister func(ctx context.Context) ([]CycleSummary, bool)

// consoleData backs the read-only API. The evidence store is the source of
// truth for finished cycles; the optional Temporal seams enrich the board and
// detail with running cycles.
type consoleData struct {
	store   *EvidenceStore
	status  consoleStatusQuery
	running consoleRunningLister
}

// ConsoleServer serves the read-only API + embedded SPA. Construct via
// newConsoleServer / StartConsoleServer.
type ConsoleServer struct {
	token          string
	csrf           string
	version        string
	generatedToken bool
	data           *consoleData
	mux            *http.ServeMux
	server         *http.Server
}

// newConsoleServer builds the server (disabled → nil, nil). A non-loopback bind
// address fails fast; the bearer token is generated when unset.
func newConsoleServer(cfg ConsoleConfig, data *consoleData, version string) (*ConsoleServer, error) {
	if !cfg.enabled() {
		return nil, nil
	}
	if err := validateLoopbackAddr(cfg.Addr); err != nil {
		return nil, err
	}
	token := cfg.Token
	generated := false
	if token == "" {
		t, err := randomToken()
		if err != nil {
			return nil, fmt.Errorf("console token: %w", err)
		}
		token, generated = t, true
	}
	csrf, err := randomToken()
	if err != nil {
		return nil, fmt.Errorf("console csrf: %w", err)
	}
	if version == "" {
		version = "dev"
	}
	s := &ConsoleServer{token: token, csrf: csrf, version: version, generatedToken: generated, data: data}
	s.mux = http.NewServeMux()
	s.routes()
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

// routes registers the read-only surface. Method-qualified patterns make an
// unsupported method a 405 without a handler.
func (s *ConsoleServer) routes() {
	s.mux.HandleFunc("GET /api/v1/status", s.handleStatus)
	s.mux.HandleFunc("GET /api/v1/cycles", s.requireAuth(s.handleCycles))
	s.mux.HandleFunc("GET /api/v1/cycles/{id}", s.requireAuth(s.handleCycleDetail))
	s.mux.HandleFunc("GET /api/v1/cycles/{id}/artifacts", s.requireAuth(s.handleArtifacts))
	s.mux.HandleFunc("GET /api/v1/cycles/{id}/artifacts/{name...}", s.requireAuth(s.handleArtifactPreview))
	s.mux.HandleFunc("GET /api/v1/cycles/{id}/download/{name...}", s.requireAuth(s.handleArtifactDownload))

	sub, err := fs.Sub(consoleAssets, "consoleassets")
	if err != nil {
		// Impossible: the //go:embed directory exists at build time.
		panic("console assets: " + err.Error())
	}
	s.mux.Handle("GET /assets/", http.StripPrefix("/assets/", http.FileServer(http.FS(sub))))
	s.mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			writeConsoleError(w, http.StatusNotFound, "not found")
			return
		}
		index, err := fs.ReadFile(sub, "index.html")
		if err != nil {
			writeConsoleError(w, http.StatusInternalServerError, "console assets unavailable")
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(index)
	})
}

// ServeHTTP applies the security baseline to every response.
func (s *ConsoleServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Security-Policy", consoleCSP)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cache-Control", "no-store")
	s.mux.ServeHTTP(w, r)
}

// handleStatus is the unauthenticated bootstrap endpoint: it issues the CSRF
// token the browser then presents on every API call.
func (s *ConsoleServer) handleStatus(w http.ResponseWriter, _ *http.Request) {
	writeConsoleJSON(w, http.StatusOK, consoleStatus{
		Name:       "konoha",
		Version:    s.version,
		ReadOnly:   true,
		CSRFHeader: consoleCSRFHeader,
		CSRFToken:  s.csrf,
	})
}

// handleCycles returns the board (finished + running best-effort).
func (s *ConsoleServer) handleCycles(w http.ResponseWriter, r *http.Request) {
	cycles, err := s.data.listCycles(r.Context())
	if err != nil {
		writeConsoleError(w, http.StatusInternalServerError, "read cycles")
		return
	}
	writeConsoleJSON(w, http.StatusOK, map[string]any{
		"cycles":  cycles,
		"columns": boardColumns(cycles),
	})
}

// handleCycleDetail returns one cycle's detail; malformed id → 400, unknown → 404.
func (s *ConsoleServer) handleCycleDetail(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validConsoleCycleID(id) {
		writeConsoleError(w, http.StatusBadRequest, "malformed cycle id")
		return
	}
	detail, ok, err := s.data.cycleDetail(r.Context(), id)
	if err != nil {
		writeConsoleError(w, http.StatusInternalServerError, "read cycle")
		return
	}
	if !ok {
		writeConsoleError(w, http.StatusNotFound, "unknown cycle")
		return
	}
	writeConsoleJSON(w, http.StatusOK, detail)
}

// handleArtifacts returns one cycle's artifacts with metadata (path, size,
// content type, previewable), or 404 for an unknown cycle.
func (s *ConsoleServer) handleArtifacts(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validConsoleCycleID(id) {
		writeConsoleError(w, http.StatusBadRequest, "malformed cycle id")
		return
	}
	if _, ok, err := s.data.cycleDetail(r.Context(), id); err != nil {
		writeConsoleError(w, http.StatusInternalServerError, "read cycle")
		return
	} else if !ok {
		writeConsoleError(w, http.StatusNotFound, "unknown cycle")
		return
	}
	views, err := s.data.store.artifactViews(id)
	if err != nil {
		writeConsoleError(w, http.StatusInternalServerError, "list artifacts")
		return
	}
	writeConsoleJSON(w, http.StatusOK, map[string]any{"id": id, "artifacts": views})
}

// requireAuth enforces a valid bearer token or CSRF token, else 401.
func (s *ConsoleServer) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.authorized(r) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="konoha-console"`)
			writeConsoleError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next(w, r)
	}
}

// authorized accepts a valid bearer token (API/CLI/worker) or the browser CSRF
// token.
func (s *ConsoleServer) authorized(r *http.Request) bool {
	if t := bearerToken(r); t != "" && constantTimeEqual(t, s.token) {
		return true
	}
	return constantTimeEqual(r.Header.Get(consoleCSRFHeader), s.csrf)
}

// mutationAllowed is the #105 token/Origin plumbing for the (future) decisions
// ticket: a CLI/worker mutation presents the bearer token; a browser mutation
// must present the CSRF token AND an allowed Origin. No route uses it yet.
func (s *ConsoleServer) mutationAllowed(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	}
	if t := bearerToken(r); t != "" {
		return constantTimeEqual(t, s.token)
	}
	if !constantTimeEqual(r.Header.Get(consoleCSRFHeader), s.csrf) {
		return false
	}
	return s.originAllowed(r)
}

// originAllowed rejects a browser Origin that is not a loopback origin. A
// missing Origin is allowed (non-browser client).
func (s *ConsoleServer) originAllowed(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return true
	}
	u, err := url.Parse(o)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	return isLoopbackHost(u.Hostname())
}

// bearerToken extracts the bearer credential, case-insensitively on the scheme.
func bearerToken(r *http.Request) string {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if len(h) > len(prefix) && strings.EqualFold(h[:len(prefix)], prefix) {
		return h[len(prefix):]
	}
	return ""
}

// validConsoleCycleID is the HTTP-boundary id guard: a conservative charset
// (no whitespace/separators) plus the evidence store's traversal guard. A
// non-matching id is a 400, never a filesystem lookup.
func validConsoleCycleID(id string) bool {
	if !validCycleID(id) {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.':
		default:
			return false
		}
	}
	return true
}

// writeConsoleJSON writes an application/json response.
func writeConsoleJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeConsoleError writes a JSON error body.
func writeConsoleError(w http.ResponseWriter, status int, msg string) {
	writeConsoleJSON(w, status, map[string]string{"error": msg})
}

// listCycles returns the board. Finished cycles come from the evidence store;
// running cycles are a best-effort Temporal enrichment (never fatal).
func (d *consoleData) listCycles(ctx context.Context) ([]CycleSummary, error) {
	ids, err := d.store.listCycleIDs()
	if err != nil {
		return nil, err
	}
	out := make([]CycleSummary, 0, len(ids))
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		o, ok, err := d.store.loadOutcomeOptional(id)
		if err != nil || !ok {
			continue
		}
		out = append(out, summaryFromOutcome(id, o))
		seen[id] = true
	}
	if d.running != nil {
		if rs, ok := d.running(ctx); ok {
			for _, r := range rs {
				if !seen[r.ID] {
					out = append(out, r)
				}
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// cycleDetail returns a finished cycle (evidence) or a running one (Temporal
// status). ok=false means neither exists (→ 404).
func (d *consoleData) cycleDetail(ctx context.Context, id string) (CycleDetail, bool, error) {
	if o, ok, err := d.store.loadOutcomeOptional(id); err != nil {
		return CycleDetail{}, false, err
	} else if ok {
		arts, _ := d.store.listArtifacts(id)
		return d.buildDetail(summaryFromOutcome(id, o), arts, &o, nil,
			o.AgentOutcome, o.VerificationOutcome), true, nil
	}
	if d.status != nil {
		if st, ok := d.status(ctx, id); ok {
			arts, _ := d.store.listArtifacts(id)
			sum := CycleSummary{ID: id, State: st.State, Running: true, At: latestConditionTime(st.Conditions)}
			return d.buildDetail(sum, arts, nil, &st, st.AgentOutcome, st.VerificationOutcome), true, nil
		}
	}
	return CycleDetail{}, false, nil
}

// buildDetail assembles the detail projection shared by the finished and
// running paths: outcome/status, ordered conditions, the deterministic
// outcome-conflict flag, and the Diff/Validation/Metadata tabs.
func (d *consoleData) buildDetail(sum CycleSummary, arts []string, out *CycleOutcome, st *StatusQuery, agent AgentOutcome, ver VerificationOutcome) CycleDetail {
	if arts == nil {
		arts = []string{}
	}
	det := CycleDetail{
		CycleSummary:    sum,
		Outcome:         out,
		Status:          st,
		Artifacts:       arts,
		OutcomeConflict: outcomesConflict(agent, ver),
		Diff:            d.loadDiffView(sum.ID),
		Validation:      d.loadValidationView(sum.ID, ver),
	}
	if out != nil && out.Owner != "" && out.Repo != "" {
		if l, err := d.store.loadLineage(out.Owner, out.Repo, out.Issue, false); err == nil {
			det.Activations = l.Activations
		}
	}
	return det
}

// summaryFromOutcome projects a terminal outcome to a board row.
func summaryFromOutcome(id string, o CycleOutcome) CycleSummary {
	state := o.TerminalState
	if strings.TrimSpace(state) == "" {
		state = "UNKNOWN"
	}
	return CycleSummary{
		ID:         id,
		Owner:      o.Owner,
		Repo:       o.Repo,
		Issue:      o.Issue,
		State:      state,
		Route:      o.Route,
		Complexity: o.Complexity,
		Priority:   o.Priority,
		Assignee:   o.Assignee,
		At:         latestConditionTime(o.Conditions),
	}
}

// listCycleIDs returns the sorted cycle ids present under evidence cycles/.
func (s *EvidenceStore) listCycleIDs() ([]string, error) {
	paths, err := s.sink.List("cycles")
	if err != nil {
		return nil, err
	}
	set := map[string]bool{}
	for _, p := range paths {
		parts := strings.Split(p, "/")
		if len(parts) >= 3 && parts[0] == "cycles" && validCycleID(parts[1]) {
			set[parts[1]] = true
		}
	}
	ids := make([]string, 0, len(set))
	for id := range set {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, nil
}

// listArtifacts returns the store-root-relative names under one cycle dir.
func (s *EvidenceStore) listArtifacts(id string) ([]string, error) {
	rel, err := s.relDirFor(id, false)
	if err != nil {
		return nil, err
	}
	paths, err := s.sink.List(rel)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		name := strings.TrimPrefix(p, rel+"/")
		if name != p {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out, nil
}

// loadOutcomeOptional reads outcome.json for one cycle; ok=false when absent
// (never an error), so the caller can fall through to the running-cycle path.
func (s *EvidenceStore) loadOutcomeOptional(id string) (CycleOutcome, bool, error) {
	rel, err := s.relDirFor(id, false)
	if err != nil {
		return CycleOutcome{}, false, err
	}
	b, err := s.sink.Read(path.Join(rel, evidenceOutcome))
	if os.IsNotExist(err) {
		return CycleOutcome{}, false, nil
	}
	if err != nil {
		return CycleOutcome{}, false, err
	}
	var o CycleOutcome
	if err := json.Unmarshal(b, &o); err != nil {
		return CycleOutcome{}, false, err
	}
	return o, true, nil
}

// StartConsoleServer starts the console when KONOHA_CONSOLE_ADDR is set, else a
// no-op (nil, nil). A non-loopback address fails fast. The listener is
// best-effort (bind errors surface via net/http), mirroring StartWebhookServer.
func StartConsoleServer(c client.Client, store *EvidenceStore, version string) (*http.Server, error) {
	cfg := consoleConfigFromEnv(os.Getenv)
	if !cfg.enabled() {
		return nil, nil
	}
	data := &consoleData{store: store}
	if c != nil {
		data.status = temporalStatusQuery(c)
		data.running = temporalRunningLister(c)
	}
	srv, err := newConsoleServer(cfg, data, version)
	if err != nil {
		return nil, err
	}
	if srv.generatedToken {
		log.Printf("console: generated bearer token: %s", srv.token)
	}
	go func() {
		if err := srv.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("console server: %v", err)
		}
	}()
	log.Printf("console listening on %s (loopback, read-only)", cfg.Addr)
	return srv.server, nil
}

// temporalStatusQuery wires the #89 status query into the console.
func temporalStatusQuery(c client.Client) consoleStatusQuery {
	return func(ctx context.Context, workflowID string) (StatusQuery, bool) {
		val, err := c.QueryWorkflow(ctx, workflowID, "", "status")
		if err != nil {
			return StatusQuery{}, false
		}
		var st StatusQuery
		if err := val.Get(&st); err != nil {
			return StatusQuery{}, false
		}
		return st, true
	}
}

// consoleRunningCap bounds how many running cycles the board enumerates.
const consoleRunningCap = 200

// temporalRunningLister enumerates in-flight issue cycles (best-effort, bounded).
func temporalRunningLister(c client.Client) consoleRunningLister {
	status := temporalStatusQuery(c)
	return func(ctx context.Context) ([]CycleSummary, bool) {
		resp, err := c.ListWorkflow(ctx, &workflowservice.ListWorkflowExecutionsRequest{
			Query:    "ExecutionStatus = 'Running' AND WorkflowType = 'IssueCycleWorkflow'",
			PageSize: consoleRunningCap,
		})
		if err != nil {
			return nil, false
		}
		out := make([]CycleSummary, 0, len(resp.Executions))
		for _, e := range resp.Executions {
			id := e.GetExecution().GetWorkflowId()
			if id == "" {
				continue
			}
			// Display only own lifecycle ids (never a foreign workflow id).
			if _, _, ok := parseCycleID(id); !ok {
				continue
			}
			repo, issue, _ := parseCycleID(id)
			s := CycleSummary{ID: id, Repo: repo, Issue: issue, State: "RUNNING", Running: true}
			if st, ok := status(ctx, id); ok {
				s.State = st.State
			}
			out = append(out, s)
			if len(out) >= consoleRunningCap {
				break
			}
		}
		return out, true
	}
}

// runConsole is the `prbot console` entrypoint (#105): a standalone, read-only
// console process. It reads only the evidence store and (best-effort) Temporal
// for running cycles — never a GitHub credential. Blocks until interrupted.
func runConsole() error {
	cfg := consoleConfigFromEnv(os.Getenv)
	if !cfg.enabled() {
		return fmt.Errorf("console: %s is not set (console disabled)", consoleAddrEnv)
	}
	// Fail fast on a non-loopback bind before touching anything else.
	if err := validateLoopbackAddr(cfg.Addr); err != nil {
		return err
	}
	// Load konoha.yaml here: this entrypoint is dispatched before the worker
	// bootstrap so the console process reads no GitHub credential.
	if app, err := loadAppConfig(defaultAppConfigPath()); err != nil {
		return err
	} else {
		appCfg = app
	}
	store, err := NewEvidenceStore(appCfg.Evidence, &defaultRedactor)
	if err != nil {
		return err
	}
	data := &consoleData{store: store}
	addr := os.Getenv("TEMPORAL_ADDRESS")
	if addr == "" {
		addr = "127.0.0.1:7233"
	}
	// Best-effort: running cycles need Temporal; the console is fully functional
	// (finished cycles) without it.
	if c, err := client.Dial(client.Options{HostPort: addr}); err == nil {
		defer c.Close()
		data.status = temporalStatusQuery(c)
		data.running = temporalRunningLister(c)
	}
	srv, err := newConsoleServer(cfg, data, consoleVersion())
	if err != nil {
		return err
	}
	if srv.generatedToken {
		log.Printf("console: generated bearer token: %s", srv.token)
	}
	log.Printf("console listening on %s (loopback, read-only)", cfg.Addr)
	return srv.server.ListenAndServe()
}

// parseCycleID parses the stable `<repo>-issue-<n>` workflow-id shape.
func parseCycleID(id string) (repo string, issue int, ok bool) {
	i := strings.LastIndex(id, "-issue-")
	if i <= 0 {
		return "", 0, false
	}
	n, err := strconv.Atoi(id[i+len("-issue-"):])
	if err != nil || n <= 0 {
		return "", 0, false
	}
	return id[:i], n, true
}
