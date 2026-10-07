package konoha

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
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

// Console decision sentinels (#108). The HTTP handler maps each to a status:
// stale/superseded gate → 409, unknown cycle → 404, malformed → 400, disabled
// console → 501; anything else is a 500.
var (
	errStaleGate         = errors.New("stale gate: no open gate or a superseded attempt/sha")
	errDecisionsDisabled = errors.New("console decisions are not enabled")
	errUnknownCycle      = errors.New("unknown cycle")
	errBadDecision       = errors.New("invalid decision")
	// #109 needs-attention queue + recovery action errors.
	errQueueDisabled     = errors.New("console queue actions are not enabled")
	errBadQueueAction    = errors.New("invalid queue action")
	errNeedsConfirmation = errors.New("interrupted cycle requires an explicit stopped confirmation")
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
	return validateLoopback("console", addr)
}

// validateLoopback rejects any bind address that is not an explicit loopback
// host, so the named surface (console/mcp) can never be exposed on a routable
// interface.
func validateLoopback(what, addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("%s addr %q: %w", what, addr, err)
	}
	if port == "" {
		return fmt.Errorf("%s addr %q: missing port", what, addr)
	}
	if !isLoopbackHost(host) {
		return fmt.Errorf("%s addr %q: must bind a loopback host (127.0.0.1, ::1 or localhost), not %q", what, addr, host)
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
	// Gate is the #108 live-gate identity, present only while a decision can be
	// actioned (the cycle is PENDING_APPROVAL). It lets the console bind a
	// submission to the exact attempt/SHA the human saw.
	Gate *GateView `json:"gate,omitempty"`
}

// GateView is the open-gate identity exposed to the console UI (#108).
type GateView struct {
	Open    bool   `json:"open"`
	Attempt int    `json:"attempt"`
	SHA     string `json:"sha"`
}

// consoleStatus is the GET /api/v1/status payload: version + the per-process
// CSRF token the browser uses on every subsequent call. Decisions reports
// whether the #108 mutation surface is wired.
type consoleStatus struct {
	Name       string `json:"name"`
	Version    string `json:"version"`
	ReadOnly   bool   `json:"readOnly"`
	Decisions  bool   `json:"decisions"`
	Queue      bool   `json:"queue"`
	Stream     bool   `json:"stream"`
	CSRFHeader string `json:"csrfHeader"`
	CSRFToken  string `json:"csrfToken"`
	// Forms is the #120 typed interaction-form catalog (static schema), present
	// when the decision surface is enabled.
	Forms []FormSchema `json:"forms,omitempty"`
}

// consoleStatusQuery is the #89 running-cycle lookup seam (nil for an
// evidence-only server / no Temporal).
type consoleStatusQuery func(ctx context.Context, workflowID string) (StatusQuery, bool)

// consoleRunningLister enumerates in-flight cycles (nil = none). The bool
// reports whether the lookup succeeded (false = best-effort miss, never fatal).
type consoleRunningLister func(ctx context.Context) ([]CycleSummary, bool)

// consoleDecisionSubmitter writes one #108 decision. nil means the console was
// built without the decision surface (decisions disabled → 501).
type consoleDecisionSubmitter func(ctx context.Context, in DecisionRequest) (DecisionResult, error)

// consoleQueueAction is the #109 authoritative retry/cancel writer. nil means
// the queue action surface is disabled (→ 501).
type consoleQueueAction func(ctx context.Context, in QueueActionRequest, item QueueItem) (QueueActionResult, error)

// consoleData backs the console API. The evidence store is the source of truth
// for finished cycles; the optional Temporal seams enrich the board and detail
// with running cycles, and the optional decision seam writes #108 decisions.
type consoleData struct {
	store   *EvidenceStore
	status  consoleStatusQuery
	running consoleRunningLister
	// #108: repo -> repos.yaml config (owner + §55 audit). Nil when unavailable;
	// a nil map disables owner resolution from config (outcome/lineage still work)
	// and leaves the audit disabled.
	repos  map[string]RepoConfig
	decide consoleDecisionSubmitter
	// #109: needs-attention queue recovery actions.
	queue consoleQueueAction
	// #110: live SSE source. Nil resolves the default evidence/status source;
	// tests inject a simulated source.
	stream consoleStreamSource
	// Authoritative Temporal lifecycle seams (nil without a Temporal client, so
	// retry/cancel fail closed with 501 rather than pretending).
	cancelWorkflow func(ctx context.Context, workflowID string) error
	startCycle     func(ctx context.Context, in IssueInput) error
	execStatus     func(ctx context.Context, workflowID string) (string, bool)
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
	hub            *streamHub
	stream         consoleStreamSource
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
	s := &ConsoleServer{token: token, csrf: csrf, version: version, generatedToken: generated, data: data, hub: newStreamHub()}
	if data != nil {
		s.stream = data.stream
		if s.stream == nil {
			s.stream = data.streamSource()
		}
	}
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

// routes registers the API surface from the canonical route table (#124).
// Method-qualified patterns make an unsupported method a 405 without a handler;
// the table is contract-tested against api/openapi.yaml.
func (s *ConsoleServer) routes() {
	for _, rt := range consoleRoutes() {
		s.register(rt)
	}

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
	decisions := s.data != nil && s.data.decide != nil
	queue := s.data != nil && s.data.queue != nil
	stream := s.data != nil && s.stream != nil
	st := consoleStatus{
		Name:       "konoha",
		Version:    s.version,
		ReadOnly:   !decisions && !queue,
		Decisions:  decisions,
		Queue:      queue,
		Stream:     stream,
		CSRFHeader: consoleCSRFHeader,
		CSRFToken:  s.csrf,
	}
	if decisions {
		st.Forms = decisionFormSchemas()
	}
	writeConsoleJSON(w, http.StatusOK, st)
}

// handleMetrics returns the #112 read-only metrics dashboard: the existing §39
// rate gauges plus throughput and #87 cost/token figures. Never a mutation.
func (s *ConsoleServer) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	if s.data == nil || s.data.store == nil {
		writeConsoleJSON(w, http.StatusOK, buildMetricsView(nil, nil))
		return
	}
	writeConsoleJSON(w, http.StatusOK, s.data.metricsView())
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

// handleQueue returns the #109 needs-attention queue (awaiting approval /
// blocked / failed / interrupted), with each item's available recovery actions.
func (s *ConsoleServer) handleQueue(w http.ResponseWriter, r *http.Request) {
	if s.data == nil || s.data.store == nil {
		writeConsoleJSON(w, http.StatusOK, QueueView{Items: []QueueItem{}})
		return
	}
	q, err := s.data.listQueue(r.Context())
	if err != nil {
		writeConsoleError(w, http.StatusInternalServerError, "read queue")
		return
	}
	if q.Items == nil {
		q.Items = []QueueItem{}
	}
	writeConsoleJSON(w, http.StatusOK, q)
}

// handleQueueAction is the #109 recovery mutation endpoint: retry / cancel
// (and the disabled session verbs). Guarded by the live item state; a mutation
// goes through the authoritative channel and is audited.
func (s *ConsoleServer) handleQueueAction(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validConsoleCycleID(id) {
		writeConsoleError(w, http.StatusBadRequest, "malformed cycle id")
		return
	}
	if s.data == nil || s.data.queue == nil {
		writeConsoleError(w, http.StatusNotImplemented, "queue actions disabled")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxDecisionBody)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var in QueueActionRequest
	if err := dec.Decode(&in); err != nil {
		writeConsoleError(w, http.StatusBadRequest, "malformed queue action body")
		return
	}
	res, err := s.data.submitQueueAction(r.Context(), id, in)
	switch {
	case err == nil:
		writeConsoleJSON(w, http.StatusOK, res)
	case errors.Is(err, errBadQueueAction):
		writeConsoleError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, errNeedsConfirmation):
		writeConsoleError(w, http.StatusConflict, err.Error())
	case errors.Is(err, errUnknownCycle):
		writeConsoleError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, errQueueDisabled):
		writeConsoleError(w, http.StatusNotImplemented, "queue actions disabled")
	default:
		writeConsoleError(w, http.StatusInternalServerError, "queue action failed")
	}
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

// handleDecision is the #108 mutation endpoint: it accepts one
// approve / request-changes / deny decision, validates it against the LIVE gate
// (open + current attempt/SHA), and writes it to the §18 authoritative channel.
// It is NOT a second source of truth: the marker is the decision.
func (s *ConsoleServer) handleDecision(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validConsoleCycleID(id) {
		writeConsoleError(w, http.StatusBadRequest, "malformed cycle id")
		return
	}
	if s.data == nil || s.data.decide == nil {
		writeConsoleError(w, http.StatusNotImplemented, "decisions disabled")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxDecisionBody)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var in DecisionRequest
	if err := dec.Decode(&in); err != nil {
		writeConsoleError(w, http.StatusBadRequest, "malformed decision body")
		return
	}
	res, err := s.data.submitDecision(r.Context(), id, in)
	switch {
	case err == nil:
		writeConsoleJSON(w, http.StatusOK, res)
	case errors.Is(err, errBadDecision):
		writeConsoleError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, errStaleGate):
		writeConsoleError(w, http.StatusConflict, err.Error())
	case errors.Is(err, errUnknownCycle):
		writeConsoleError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, errDecisionsDisabled):
		writeConsoleError(w, http.StatusNotImplemented, "decisions disabled")
	default:
		writeConsoleError(w, http.StatusInternalServerError, "decision failed")
	}
}

// handleFormDecision is the #120 mutation endpoint: it validates a typed form
// submission against its schema (fail-closed, §36), maps it onto an EXISTING
// §18 decision action, and writes it through the same gate/validation/writer as
// a raw #108 decision. It introduces no new state and no parallel writer.
func (s *ConsoleServer) handleFormDecision(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validConsoleCycleID(id) {
		writeConsoleError(w, http.StatusBadRequest, "malformed cycle id")
		return
	}
	if s.data == nil || s.data.decide == nil {
		writeConsoleError(w, http.StatusNotImplemented, "decisions disabled")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxDecisionBody)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var sub FormSubmission
	if err := dec.Decode(&sub); err != nil {
		writeConsoleError(w, http.StatusBadRequest, "malformed form body")
		return
	}
	fd, err := ValidateFormSubmission(sub)
	if err != nil {
		writeConsoleError(w, http.StatusBadRequest, err.Error())
		return
	}
	// Bind each anchored comment to the reviewed SHA the form was filled against
	// (the client may omit it), then map + redact via the shared writer path.
	for i := range fd.Comments {
		fd.Comments[i].SHA = sub.SHA
	}
	req := fd.decisionRequest(DecisionRequest{Attempt: sub.Attempt, SHA: sub.SHA, Actor: sub.Actor})
	res, err := s.data.submitBoundDecision(r.Context(), id, req)
	switch {
	case err == nil:
		writeConsoleJSON(w, http.StatusOK, res)
	case errors.Is(err, errBadDecision):
		writeConsoleError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, errStaleGate):
		writeConsoleError(w, http.StatusConflict, err.Error())
	case errors.Is(err, errUnknownCycle):
		writeConsoleError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, errDecisionsDisabled):
		writeConsoleError(w, http.StatusNotImplemented, "decisions disabled")
	default:
		writeConsoleError(w, http.StatusInternalServerError, "form decision failed")
	}
}

// requireMutation enforces the #105 mutation auth plumbing: a CLI/worker bearer
// token, or the browser CSRF token plus a loopback Origin. Unlike requireAuth it
// rejects a bare CSRF token with a foreign Origin on a mutating method.
func (s *ConsoleServer) requireMutation(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.mutationAllowed(r) {
			writeConsoleError(w, http.StatusForbidden, "forbidden")
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

// metricsView builds the #112 dashboard from the in-process §39 registry
// snapshot (may be empty for a standalone console) plus the recorded outcomes.
// Pure projection; read-only.
func (d *consoleData) metricsView() MetricsView {
	return buildMetricsView(metricsSnapshot(), d.listOutcomes())
}

// listOutcomes reads every finished cycle's outcome.json (best-effort: a bad or
// absent record is skipped, never fatal).
func (d *consoleData) listOutcomes() []CycleOutcome {
	ids, err := d.store.listCycleIDs()
	if err != nil {
		return nil
	}
	out := make([]CycleOutcome, 0, len(ids))
	for _, id := range ids {
		if o, ok, err := d.store.loadOutcomeOptional(id); err == nil && ok {
			out = append(out, o)
		}
	}
	return out
}

// cycleDetail returns a finished cycle (evidence) or a running one (Temporal
// status). ok=false means neither exists (→ 404).
func (d *consoleData) cycleDetail(ctx context.Context, id string) (CycleDetail, bool, error) {
	if o, ok, err := d.store.loadOutcomeOptional(id); err != nil {
		return CycleDetail{}, false, err
	} else if ok {
		arts, _ := d.store.listArtifacts(id)
		det := d.buildDetail(summaryFromOutcome(id, o), arts, &o, nil,
			o.AgentOutcome, o.VerificationOutcome)
		d.attachGate(ctx, id, &det)
		return det, true, nil
	}
	if d.status != nil {
		if st, ok := d.status(ctx, id); ok {
			arts, _ := d.store.listArtifacts(id)
			sum := CycleSummary{ID: id, State: st.State, Running: true, At: latestConditionTime(st.Conditions)}
			det := d.buildDetail(sum, arts, nil, &st, st.AgentOutcome, st.VerificationOutcome)
			d.attachGate(ctx, id, &det)
			return det, true, nil
		}
	}
	return CycleDetail{}, false, nil
}

// attachGate adds the #108 live-gate identity to a detail view when the console
// has a decision writer and a gate is currently open. Best-effort: a snapshot
// error simply leaves the field absent.
func (d *consoleData) attachGate(ctx context.Context, id string, det *CycleDetail) {
	if d.decide == nil {
		return
	}
	if snap, ok, err := d.gateSnapshot(ctx, id); err == nil && ok && snap.Open {
		det.Gate = &GateView{Open: true, Attempt: snap.Attempt, SHA: snap.SHA}
	}
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

// cycleExists reports whether any evidence artifact is recorded for a cycle id,
// so a caller can tell a known-but-interrupted cycle from a typo/unknown id.
func (s *EvidenceStore) cycleExists(id string) bool {
	names, err := s.listArtifacts(id)
	return err == nil && len(names) > 0
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

// gateSnapshot resolves the current gate identity for one cycle: the owner (from
// repos.yaml, else the finished outcome), whether a gate is open (the running
// status is PENDING_APPROVAL), and the live attempt + reviewed SHA (from the
// recorded #88 lineage). ok=false when neither config nor outcome identifies the
// owner, so the handler reports an unknown cycle.
func (d *consoleData) gateSnapshot(ctx context.Context, id string) (GateSnapshot, bool, error) {
	repo, issue, ok := parseCycleID(id)
	if !ok {
		return GateSnapshot{}, false, nil
	}
	snap := GateSnapshot{Repo: repo, Issue: issue}
	if rc, ok := d.repos[repo]; ok {
		snap.Owner = rc.Owner
	}
	if o, ok, err := d.store.loadOutcomeOptional(id); err != nil {
		return GateSnapshot{}, false, err
	} else if ok && o.Owner != "" {
		snap.Owner = o.Owner
	}
	if snap.Owner == "" {
		return GateSnapshot{}, false, nil
	}
	if l, err := d.store.loadLineage(snap.Owner, repo, issue, false); err == nil {
		snap.Attempt, snap.SHA = currentAttemptSHA(l.Activations)
	}
	if d.status != nil {
		if st, ok := d.status(ctx, id); ok {
			snap.Open = st.State == statePendingApproval
		}
	}
	return snap, true, nil
}

// submitDecision validates a decision against the live gate and delegates to the
// wired writer. Owner/repo/issue/cycleId and the actor default are filled here,
// so the browser only supplies the action + gate identity + feedback.
func (d *consoleData) submitDecision(ctx context.Context, id string, in DecisionRequest) (DecisionResult, error) {
	return d.submitBoundDecision(ctx, id, in)
}

// submitBoundDecision binds a request to the live gate (owner + open +
// attempt/SHA), validates it (§36), and delegates to the writer. Every mutation
// path (a raw #108 decision or a #120 form) funnels through here, so the gate
// authority and validation are identical.
func (d *consoleData) submitBoundDecision(ctx context.Context, id string, in DecisionRequest) (DecisionResult, error) {
	if d.decide == nil {
		return DecisionResult{}, errDecisionsDisabled
	}
	bound, err := d.bindDecision(ctx, id, in)
	if err != nil {
		return DecisionResult{}, err
	}
	if err := ValidateDecisionRequest(bound); err != nil {
		return DecisionResult{}, fmt.Errorf("%w: %v", errBadDecision, err)
	}
	return d.decide(ctx, bound)
}

// bindDecision resolves the live gate and fills the request identity fields,
// failing closed on a disabled surface, an unknown cycle, or a stale/closed gate
// (the sentinels the handlers map to status codes).
func (d *consoleData) bindDecision(ctx context.Context, id string, in DecisionRequest) (DecisionRequest, error) {
	snap, ok, err := d.gateSnapshot(ctx, id)
	if err != nil {
		return DecisionRequest{}, err
	}
	if !ok {
		return DecisionRequest{}, errUnknownCycle
	}
	if !snap.Open {
		return DecisionRequest{}, fmt.Errorf("%w: cycle is not waiting at a gate", errStaleGate)
	}
	if in.Attempt != snap.Attempt || in.SHA != snap.SHA {
		return DecisionRequest{}, fmt.Errorf("%w: attempt %d/%s is superseded by %d/%s",
			errStaleGate, in.Attempt, shortSHA(in.SHA), snap.Attempt, shortSHA(snap.SHA))
	}
	in.Owner, in.Repo, in.Issue, in.CycleID = snap.Owner, snap.Repo, snap.Issue, id
	if strings.TrimSpace(in.Actor) == "" {
		in.Actor = "console"
	}
	return in, nil
}

// shortSHA renders an abbreviated SHA for error messages (never a secret).
func shortSHA(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

// consoleDecisionWriter builds the real #108 writer: it resolves the repo's §55
// audit config from repos.yaml (disabled when absent) and writes the decision to
// the §18 channel + the derived #97 evidence.
func consoleDecisionWriter(d *consoleData) consoleDecisionSubmitter {
	return func(ctx context.Context, in DecisionRequest) (DecisionResult, error) {
		var audit AuditConfig
		if rc, ok := d.repos[in.Repo]; ok && rc.Lifecycle != nil {
			audit = rc.Lifecycle.Audit
		}
		return WriteConsoleDecision(ctx, d.store, audit, in)
	}
}

// repoConfigMap keys repos.yaml entries by repo name. A load failure yields nil
// (owner resolution then falls back to the outcome; the audit stays disabled).
func repoConfigMap(repos []RepoConfig) map[string]RepoConfig {
	if len(repos) == 0 {
		return nil
	}
	m := make(map[string]RepoConfig, len(repos))
	for _, r := range repos {
		m[r.Repo] = r
	}
	return m
}

// consoleRepos best-effort loads repos.yaml for owner/audit resolution. The
// console process still reads no GitHub credential (repos.yaml is policy only).
func consoleRepos() map[string]RepoConfig {
	repos, err := loadReposConfigWithResources(defaultReposPath())
	if err != nil {
		return nil
	}
	return repoConfigMap(repos)
}

// StartConsoleServer starts the console when KONOHA_CONSOLE_ADDR is set, else a
// no-op (nil, nil). A non-loopback address fails fast. The listener is
// best-effort (bind errors surface via net/http), mirroring StartWebhookServer.
func StartConsoleServer(c client.Client, store *EvidenceStore, version string) (*http.Server, error) {
	cfg := consoleConfigFromEnv(os.Getenv)
	if !cfg.enabled() {
		return nil, nil
	}
	data := buildConsoleData(store, consoleRepos(), c)
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
	log.Printf("console listening on %s (loopback; decisions via the §18 channel)", cfg.Addr)
	return srv.server, nil
}

// buildConsoleData assembles the console data: the evidence store, repos.yaml
// policy, the #108 decision writer, and — when a Temporal client is present —
// the #89 status/running seams plus the #109 queue recovery lifecycle seams.
func buildConsoleData(store *EvidenceStore, repos map[string]RepoConfig, c client.Client) *consoleData {
	d := &consoleData{store: store, repos: repos}
	d.decide = consoleDecisionWriter(d)
	d.queue = consoleQueueWriter(d)
	if c != nil {
		d.status = temporalStatusQuery(c)
		d.running = temporalRunningLister(c)
		d.cancelWorkflow = temporalCancelWorkflow(c)
		d.startCycle = temporalStartCycle(c)
		d.execStatus = temporalExecStatus(c)
	}
	return d
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
	addr := os.Getenv("TEMPORAL_ADDRESS")
	if addr == "" {
		addr = "127.0.0.1:7233"
	}
	// Best-effort: running cycles + queue retry/cancel need Temporal; the console
	// is fully functional (finished cycles, read-only) without it.
	var c client.Client
	if dialed, err := client.Dial(client.Options{HostPort: addr}); err == nil {
		defer dialed.Close()
		c = dialed
	}
	data := buildConsoleData(store, consoleRepos(), c)
	srv, err := newConsoleServer(cfg, data, consoleVersion())
	if err != nil {
		return err
	}
	if srv.generatedToken {
		log.Printf("console: generated bearer token: %s", srv.token)
	}
	log.Printf("console listening on %s (loopback; decisions via the §18 channel)", cfg.Addr)
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
