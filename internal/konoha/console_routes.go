package konoha

import (
	"net/http"
	"strings"
)

// Console route table (#124). This is the SINGLE source of truth for the HTTP
// surface: `ConsoleServer.routes` registers from it and the contract test
// (`console_contract_test.go`) compares it against api/openapi.yaml, so a route,
// method or auth change that drifts from the canonical spec fails the suite.
//
// The spec is authored in OpenAPI 3.1 (`api/openapi.yaml`) and the Go/TS clients
// are generated from it; never hand-edit the generated files.

// consoleAuth is the auth tier a route is registered behind.
type consoleAuth int

const (
	// consoleAuthNone is the unauthenticated bootstrap endpoint (/status).
	consoleAuthNone consoleAuth = iota
	// consoleAuthRead accepts a bearer token or the browser CSRF token.
	consoleAuthRead
	// consoleAuthMutation additionally enforces the loopback Origin for a
	// CSRF-authenticated browser mutation.
	consoleAuthMutation
)

// String is the stable label used by the contract test (and error messages).
func (a consoleAuth) String() string {
	switch a {
	case consoleAuthNone:
		return "none"
	case consoleAuthRead:
		return "read"
	case consoleAuthMutation:
		return "mutation"
	}
	return "unknown"
}

// consoleRoute is one entry in the canonical route table. Path uses the OpenAPI
// template shape (`/api/v1/cycles/{id}`); Wildcard marks a trailing path
// parameter that captures slashes (`{name...}` in a net/http mux pattern).
type consoleRoute struct {
	Method   string
	Path     string
	Auth     consoleAuth
	Wildcard bool
	Handler  func(*ConsoleServer) http.HandlerFunc
}

// muxPattern renders the Go 1.22 net/http ServeMux pattern for the route.
func (r consoleRoute) muxPattern() string {
	p := r.Path
	if r.Wildcard {
		p = strings.TrimSuffix(p, "}") + "...}"
	}
	return r.Method + " " + p
}

// consoleRoutes returns the canonical console API route table.
func consoleRoutes() []consoleRoute {
	return []consoleRoute{
		{http.MethodGet, "/api/v1/status", consoleAuthNone, false,
			func(s *ConsoleServer) http.HandlerFunc { return s.handleStatus }},
		{http.MethodGet, "/api/v1/metrics", consoleAuthRead, false,
			func(s *ConsoleServer) http.HandlerFunc { return s.handleMetrics }},
		{http.MethodGet, "/api/v1/cycles", consoleAuthRead, false,
			func(s *ConsoleServer) http.HandlerFunc { return s.handleCycles }},
		{http.MethodGet, "/api/v1/cycles/{id}", consoleAuthRead, false,
			func(s *ConsoleServer) http.HandlerFunc { return s.handleCycleDetail }},
		{http.MethodGet, "/api/v1/cycles/{id}/artifacts", consoleAuthRead, false,
			func(s *ConsoleServer) http.HandlerFunc { return s.handleArtifacts }},
		{http.MethodGet, "/api/v1/cycles/{id}/artifacts/{name}", consoleAuthRead, true,
			func(s *ConsoleServer) http.HandlerFunc { return s.handleArtifactPreview }},
		{http.MethodGet, "/api/v1/cycles/{id}/download/{name}", consoleAuthRead, true,
			func(s *ConsoleServer) http.HandlerFunc { return s.handleArtifactDownload }},
		{http.MethodGet, "/api/v1/cycles/{id}/stream", consoleAuthRead, false,
			func(s *ConsoleServer) http.HandlerFunc { return s.handleCycleStream }},
		{http.MethodGet, "/api/v1/queue", consoleAuthRead, false,
			func(s *ConsoleServer) http.HandlerFunc { return s.handleQueue }},
		{http.MethodPost, "/api/v1/cycles/{id}/decisions", consoleAuthMutation, false,
			func(s *ConsoleServer) http.HandlerFunc { return s.handleDecision }},
		{http.MethodPost, "/api/v1/cycles/{id}/forms", consoleAuthMutation, false,
			func(s *ConsoleServer) http.HandlerFunc { return s.handleFormDecision }},
		{http.MethodPost, "/api/v1/cycles/{id}/actions", consoleAuthMutation, false,
			func(s *ConsoleServer) http.HandlerFunc { return s.handleQueueAction }},
	}
}

// register wraps a route handler in its auth middleware and binds it to the mux.
func (s *ConsoleServer) register(rt consoleRoute) {
	h := rt.Handler(s)
	switch rt.Auth {
	case consoleAuthRead:
		h = s.requireAuth(h)
	case consoleAuthMutation:
		h = s.requireMutation(h)
	}
	s.mux.HandleFunc(rt.muxPattern(), h)
}
