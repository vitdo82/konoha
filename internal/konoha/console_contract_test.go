package konoha

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"prreview/internal/konoha/consoleapi"
)

// #124 contract tests: api/openapi.yaml is canonical. These fail when the
// server's routes, auth tiers or response schemas drift from the spec.

// oasDoc is the subset of the OpenAPI document the contract test inspects.
type oasDoc struct {
	OpenAPI string `yaml:"openapi"`
	Info    struct {
		Title   string `yaml:"title"`
		Version string `yaml:"version"`
	} `yaml:"info"`
	Paths      map[string]oasPathItem `yaml:"paths"`
	Components struct {
		SecuritySchemes map[string]oasSecurityScheme `yaml:"securitySchemes"`
		Schemas         map[string]any               `yaml:"schemas"`
	} `yaml:"components"`
}

type oasPathItem struct {
	Get    *oasOperation `yaml:"get"`
	Post   *oasOperation `yaml:"post"`
	Put    *oasOperation `yaml:"put"`
	Patch  *oasOperation `yaml:"patch"`
	Delete *oasOperation `yaml:"delete"`
}

type oasOperation struct {
	OperationID string                `yaml:"operationId"`
	Security    []map[string][]string `yaml:"security"`
	Responses   map[string]any        `yaml:"responses"`
}

type oasSecurityScheme struct {
	Type   string `yaml:"type"`
	Scheme string `yaml:"scheme"`
	In     string `yaml:"in"`
	Name   string `yaml:"name"`
}

// loadOpenAPISpec parses api/openapi.yaml (anchors expand automatically).
func loadOpenAPISpec(t *testing.T) oasDoc {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "api", "openapi.yaml"))
	if err != nil {
		t.Fatalf("read openapi.yaml: %v", err)
	}
	var doc oasDoc
	if err := yaml.Unmarshal(b, &doc); err != nil {
		t.Fatalf("parse openapi.yaml: %v", err)
	}
	return doc
}

// specOperations flattens the spec into "METHOD path" -> operation.
func specOperations(doc oasDoc) map[string]oasOperation {
	out := map[string]oasOperation{}
	add := func(path, method string, op *oasOperation) {
		if op != nil {
			out[method+" "+path] = *op
		}
	}
	for path, item := range doc.Paths {
		add(path, http.MethodGet, item.Get)
		add(path, http.MethodPost, item.Post)
		add(path, http.MethodPut, item.Put)
		add(path, http.MethodPatch, item.Patch)
		add(path, http.MethodDelete, item.Delete)
	}
	return out
}

// specAuth derives the auth tier the spec expresses for an operation: an
// explicit/absent empty security list is unauthenticated; otherwise a mutating
// method is a mutation tier and a read method is the read tier.
func specAuth(method string, op oasOperation) consoleAuth {
	if len(op.Security) == 0 {
		return consoleAuthNone
	}
	if method == http.MethodPost || method == http.MethodPut || method == http.MethodPatch || method == http.MethodDelete {
		return consoleAuthMutation
	}
	return consoleAuthRead
}

// #124 AC: the spec validates (version/info/security schemes/operations) so a
// malformed or truncated spec fails the suite.
func TestOpenAPISpecValidates(t *testing.T) {
	doc := loadOpenAPISpec(t)
	if len(doc.OpenAPI) < 5 || doc.OpenAPI[:4] != "3.1." {
		t.Fatalf("openapi = %q, want an OpenAPI 3.1.x document", doc.OpenAPI)
	}
	if doc.Info.Title == "" || doc.Info.Version == "" {
		t.Fatalf("info.title/version must be set: %+v", doc.Info)
	}
	if len(doc.Paths) == 0 {
		t.Fatal("spec has no paths")
	}
	if len(doc.Components.Schemas) == 0 {
		t.Fatal("spec has no component schemas")
	}
	// The two auth schemes the console enforces must be declared.
	bearer, ok := doc.Components.SecuritySchemes["bearerAuth"]
	if !ok || bearer.Type != "http" || bearer.Scheme != "bearer" {
		t.Fatalf("bearerAuth = %+v, want an http bearer scheme", bearer)
	}
	csrf, ok := doc.Components.SecuritySchemes["csrfAuth"]
	if !ok || csrf.Type != "apiKey" || csrf.In != "header" || csrf.Name != consoleCSRFHeader {
		t.Fatalf("csrfAuth = %+v, want an apiKey header %q", csrf, consoleCSRFHeader)
	}
	// Every operation is fully specified.
	for key, op := range specOperations(doc) {
		if op.OperationID == "" {
			t.Errorf("%s: missing operationId", key)
		}
		if len(op.Responses) == 0 {
			t.Errorf("%s: missing responses", key)
		}
	}
}

// #124 AC: the registered routes/methods/auth must match the spec exactly.
func TestOpenAPIRoutesMatchServer(t *testing.T) {
	doc := loadOpenAPISpec(t)
	spec := map[string]consoleAuth{}
	for key, op := range specOperations(doc) {
		spec[key] = specAuth(opMethod(key), op)
	}
	got := map[string]consoleAuth{}
	for _, rt := range consoleRoutes() {
		got[rt.Method+" "+rt.Path] = rt.Auth
	}
	for key, want := range spec {
		have, ok := got[key]
		if !ok {
			t.Errorf("spec route %q is not registered by the console", key)
			continue
		}
		if have != want {
			t.Errorf("route %q auth = %s, spec says %s", key, have, want)
		}
	}
	for key := range got {
		if _, ok := spec[key]; !ok {
			t.Errorf("console route %q is not in the spec (drift)", key)
		}
	}
}

// opMethod extracts the HTTP method from a "METHOD path" key.
func opMethod(key string) string {
	for i := 0; i < len(key); i++ {
		if key[i] == ' ' {
			return key[:i]
		}
	}
	return key
}

// decodeStrict decodes JSON into T rejecting unknown fields, so a handler that
// starts returning a field absent from the spec fails the contract.
func decodeStrict[T any](t *testing.T, label string, body []byte) T {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	var v T
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("%s: response does not conform to the spec: %v\nbody: %s", label, err, body)
	}
	return v
}

// #124 AC: live handler responses decode into the generated spec types with no
// unknown fields — the schema half of the contract test.
func TestOpenAPIResponseSchemasMatchSpec(t *testing.T) {
	o := CycleOutcome{
		WorkflowID: "r-issue-1", Owner: "acme", Repo: "r", Issue: 1,
		TerminalState: stateHumanRequired, Route: stateSmall, Complexity: 3,
		AgentOutcome: agentOutcomeSuccess, VerificationOutcome: verificationSuccess,
		Conditions: []Condition{{Type: conditionTerminal, Status: string(stateHumanRequired)}},
	}
	_, _, hs := newTestConsole(t, o)
	tok := "secret-token"

	decodeStrict[consoleapi.ConsoleStatus](t, "status",
		getBody(t, hs.URL+"/api/v1/status", tok))
	decodeStrict[consoleapi.MetricsView](t, "metrics",
		getBody(t, hs.URL+"/api/v1/metrics", tok))
	decodeStrict[consoleapi.CycleListResponse](t, "cycles",
		getBody(t, hs.URL+"/api/v1/cycles", tok))
	decodeStrict[consoleapi.CycleDetail](t, "cycle detail",
		getBody(t, hs.URL+"/api/v1/cycles/"+o.WorkflowID, tok))
	decodeStrict[consoleapi.ArtifactListResponse](t, "artifacts",
		getBody(t, hs.URL+"/api/v1/cycles/"+o.WorkflowID+"/artifacts", tok))
	decodeStrict[consoleapi.QueueView](t, "queue",
		getBody(t, hs.URL+"/api/v1/queue", tok))
}

// getBody performs an authenticated GET and returns the raw response body.
func getBody(t *testing.T, url, token string) []byte {
	t.Helper()
	res := getURL(t, url, token, "")
	defer res.Body.Close()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(res.Body); err != nil {
		t.Fatalf("read body %s: %v", url, err)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d, body: %s", url, res.StatusCode, buf.Bytes())
	}
	return buf.Bytes()
}

// #124 AC: the generated Go client compiles and is exercised end-to-end against
// a real console (reads + auth + a disabled-mutation response).
func TestGeneratedClientSmoke(t *testing.T) {
	o := CycleOutcome{
		WorkflowID: "r-issue-1", Owner: "acme", Repo: "r", Issue: 1,
		TerminalState: stateClosed, Route: stateSmall, AutonomousClose: true,
		AgentOutcome: agentOutcomeSuccess, VerificationOutcome: verificationSuccess,
	}
	_, _, hs := newTestConsole(t, o)
	ctx := context.Background()

	client, err := consoleapi.NewClientWithResponses(hs.URL, consoleapi.WithRequestEditorFn(
		func(_ context.Context, req *http.Request) error {
			req.Header.Set("Authorization", "Bearer secret-token")
			return nil
		}))
	if err != nil {
		t.Fatalf("new generated client: %v", err)
	}

	st, err := client.GetStatusWithResponse(ctx)
	if err != nil || st.JSON200 == nil || st.StatusCode() != http.StatusOK {
		t.Fatalf("getStatus: err=%v status=%v body=%s", err, st.StatusCode(), st.Body)
	}
	if st.JSON200.Name != "konoha" || st.JSON200.CsrfToken == "" {
		t.Fatalf("status = %+v, want name=konoha + csrf token", st.JSON200)
	}

	list, err := client.ListCyclesWithResponse(ctx)
	if err != nil || list.JSON200 == nil {
		t.Fatalf("listCycles: err=%v body=%s", err, list.Body)
	}
	if len(list.JSON200.Cycles) != 1 || list.JSON200.Cycles[0].Id != o.WorkflowID {
		t.Fatalf("cycles = %+v, want the one recorded cycle", list.JSON200.Cycles)
	}

	det, err := client.GetCycleWithResponse(ctx, o.WorkflowID)
	if err != nil || det.JSON200 == nil || det.JSON200.Id != o.WorkflowID {
		t.Fatalf("getCycle: err=%v body=%s", err, det.Body)
	}

	arts, err := client.ListArtifactsWithResponse(ctx, o.WorkflowID)
	if err != nil || arts.JSON200 == nil || len(arts.JSON200.Artifacts) == 0 {
		t.Fatalf("listArtifacts: err=%v body=%s", err, arts.Body)
	}

	// A mutation reaches the route through the generated client and is refused
	// because this test console has no decision writer (501, never a 2xx).
	dec, err := client.SubmitDecisionWithResponse(ctx, o.WorkflowID, consoleapi.SubmitDecisionJSONRequestBody{
		Action: consoleapi.DecisionRequestActionApprove, Attempt: 1, Sha: "deadbeef",
	})
	if err != nil || dec.StatusCode() != http.StatusNotImplemented {
		t.Fatalf("submitDecision: err=%v status=%v body=%s, want 501", err, dec.StatusCode(), dec.Body)
	}

	// A client without credentials is rejected by the auth middleware.
	anon, err := consoleapi.NewClientWithResponses(hs.URL)
	if err != nil {
		t.Fatal(err)
	}
	unauth, err := anon.ListCyclesWithResponse(ctx)
	if err != nil || unauth.StatusCode() != http.StatusUnauthorized {
		t.Fatalf("anonymous listCycles = %v (err=%v), want 401", unauth.StatusCode(), err)
	}
}

// #124 AC: the CLI reads the console through the generated client (no ad-hoc
// requests), resolving the base URL + bearer token from the environment.
func TestConsoleAPICLIUsesGeneratedClient(t *testing.T) {
	o := CycleOutcome{
		WorkflowID: "r-issue-1", Owner: "acme", Repo: "r", Issue: 1,
		TerminalState: stateClosed, Route: stateSmall,
		AgentOutcome: agentOutcomeSuccess, VerificationOutcome: verificationSuccess,
	}
	_, _, hs := newTestConsole(t, o)
	host := strings.TrimPrefix(hs.URL, "http://")
	t.Setenv(consoleAddrEnv, host)
	t.Setenv(consoleTokenEnv, "secret-token")

	payload, err := consoleAPIPayload([]string{"cycles"})
	if err != nil {
		t.Fatalf("api cycles: %v", err)
	}
	list, ok := payload.(*consoleapi.CycleListResponse)
	if !ok || list == nil || len(list.Cycles) != 1 || list.Cycles[0].Id != o.WorkflowID {
		t.Fatalf("api cycles payload = %#v, want the generated CycleListResponse with the one cycle", payload)
	}

	detail, err := consoleAPIPayload([]string{"cycle", o.WorkflowID})
	if err != nil {
		t.Fatalf("api cycle: %v", err)
	}
	if d, ok := detail.(*consoleapi.CycleDetail); !ok || d.Id != o.WorkflowID {
		t.Fatalf("api cycle payload = %#v, want the generated CycleDetail", detail)
	}

	if _, err := consoleAPIPayload([]string{"bogus"}); err == nil {
		t.Fatal("unknown api resource must error")
	}
}
