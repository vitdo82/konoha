package konoha

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// §36: a malformed form submission (missing required, wrong type, over cap,
// unknown enum/field/decision) is rejected and never becomes a decision.
func TestValidateFormSubmissionRejectsInvalid(t *testing.T) {
	fd, err := ValidateFormSubmission(FormSubmission{
		Decision: "request-changes",
		Values:   map[string]any{"summary": "please add a test", "severity": "high"},
	})
	if err != nil || fd.Action != humanDecisionRequestChanges || fd.Feedback != "please add a test" {
		t.Fatalf("valid request-changes = %+v err=%v", fd, err)
	}

	cases := []struct {
		name string
		sub  FormSubmission
	}{
		{"unknown decision", FormSubmission{Decision: "ship-it"}},
		{"missing required text", FormSubmission{Decision: "request-changes", Values: map[string]any{}}},
		{"whitespace-only required", FormSubmission{Decision: "request-changes", Values: map[string]any{"summary": "   "}}},
		{"wrong text type", FormSubmission{Decision: "request-changes", Values: map[string]any{"summary": 42}}},
		{"unknown field", FormSubmission{Decision: "request-changes", Values: map[string]any{"summary": "ok", "extra": "x"}}},
		{"bad choice", FormSubmission{Decision: "request-changes", Values: map[string]any{"summary": "ok", "severity": "apocalyptic"}}},
		{"choice wrong type", FormSubmission{Decision: "request-changes", Values: map[string]any{"summary": "ok", "severity": 3}}},
		{"over text cap", FormSubmission{Decision: "request-changes", Values: map[string]any{"summary": strings.Repeat("x", maxDecisionFeedback+1)}}},
		{"number out of range", FormSubmission{Decision: "clarification", Values: map[string]any{"answer": "ok", "confidence": 5.0}}},
		{"number wrong type", FormSubmission{Decision: "clarification", Values: map[string]any{"answer": "ok", "confidence": "high"}}},
		{"required missing on clarification", FormSubmission{Decision: "clarification", Values: map[string]any{"confidence": 0.5}}},
	}
	for _, tc := range cases {
		if _, err := ValidateFormSubmission(tc.sub); err == nil {
			t.Fatalf("%s: must fail closed", tc.name)
		}
	}
}

// §18/§36: a valid form maps onto an EXISTING decision action only — approve →
// approve, request-changes → request-changes, clarification → resume. No new
// state is introduced.
func TestFormDecisionMapsToExistingAction(t *testing.T) {
	want := map[string]string{
		"approve":         humanDecisionApprove,
		"request-changes": humanDecisionRequestChanges,
		"clarification":   humanDecisionResume,
	}
	for decision, action := range want {
		schema, ok := lookupFormSchema(decision)
		if !ok {
			t.Fatalf("schema %q missing", decision)
		}
		if schema.Action != action {
			t.Fatalf("schema %q maps to %q, want %q", decision, schema.Action, action)
		}
		if err := ValidateHumanDecision(schema.Action); err != nil {
			t.Fatalf("mapped action %q must be in the §18 set: %v", schema.Action, err)
		}
	}
	// A clarification answer maps to resume, which the deterministic policy
	// treats as "continue" — never a terminal state.
	tr, err := ApplyHumanDecision(HumanDecision{Action: humanDecisionResume, Present: true})
	if err != nil || !tr.Resume || tr.TargetState != "" {
		t.Fatalf("clarification resume = %+v err=%v", tr, err)
	}
	// The mapped request passes the §108 validator unchanged.
	fd, err := ValidateFormSubmission(FormSubmission{Decision: "clarification", Values: map[string]any{"answer": "it is a race"}})
	if err != nil {
		t.Fatal(err)
	}
	req := fd.decisionRequest(DecisionRequest{CycleID: "r-issue-7", Attempt: 1, SHA: "s1"})
	if err := ValidateDecisionRequest(req); err != nil {
		t.Fatalf("mapped clarification request: %v", err)
	}
}

// §36: size caps are enforced and a secret in a typed field is redacted before
// the value becomes a decision/spec body (§82).
func TestFormCapsAndRedaction(t *testing.T) {
	if _, err := ValidateFormSubmission(FormSubmission{
		Decision: "request-changes",
		Values:   map[string]any{"summary": strings.Repeat("a", maxDecisionFeedback)},
	}); err != nil {
		t.Fatalf("at-cap text must pass: %v", err)
	}
	fd, err := ValidateFormSubmission(FormSubmission{
		Decision: "request-changes",
		Values:   map[string]any{"summary": "leaked API_TOKEN=supersecretvalue here"},
	})
	if err != nil {
		t.Fatal(err)
	}
	req := fd.decisionRequest(DecisionRequest{CycleID: "r-issue-7", Attempt: 1, SHA: "s1"})
	if strings.Contains(req.Feedback, "supersecretvalue") {
		t.Fatalf("form feedback not redacted: %q", req.Feedback)
	}
	if !strings.Contains(req.Feedback, redacted) {
		t.Fatalf("redaction marker missing: %q", req.Feedback)
	}
}

// §40: a clarification form pauses at the gate and resumes on a valid response;
// a timeout keeps the existing BLOCKED semantics (never a silent proceed).
func TestClarificationFormPausesAndResumes(t *testing.T) {
	schema, ok := lookupFormSchema("clarification")
	if !ok || schema.Action != humanDecisionResume {
		t.Fatalf("clarification schema = %+v ok=%v", schema, ok)
	}
	// A valid clarification answer is an approval-gate "grant" (resume).
	if v := EvaluateApproval(HumanDecision{Action: humanDecisionResume, Present: true}); v != approvalGranted {
		t.Fatalf("clarification resume = %v, want granted", v)
	}
	// While awaiting, the cycle stays put; past the timeout it is TimedOut and
	// must remain BLOCKED.
	if d := EvaluateHumanGate(time.Minute, 2*time.Minute); !d.Awaiting || d.TimedOut {
		t.Fatalf("awaiting = %+v", d)
	}
	if d := EvaluateHumanGate(3*time.Minute, 2*time.Minute); !d.TimedOut || d.Awaiting {
		t.Fatalf("timeout = %+v", d)
	}
}

// §24/§55: a form-derived decision is written as one idempotent §18 marker
// comment and audited with actor attribution.
func TestFormSubmissionIdempotentAndAudited(t *testing.T) {
	fd, err := ValidateFormSubmission(FormSubmission{
		Decision: "request-changes",
		Values:   map[string]any{"summary": "add coverage"},
		Comments: []DiffComment{{File: "a.go", Line: 3, SHA: "sha2", Body: "guard this"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	req := fd.decisionRequest(DecisionRequest{
		Owner: "o", Repo: "r", Issue: 7, CycleID: "r-issue-7",
		Attempt: 2, SHA: "sha2", Actor: "console",
	})
	if req.Comments[0].SHA != "sha2" {
		t.Fatalf("comment must stay bound to the reviewed SHA: %+v", req.Comments[0])
	}
	// Two identical submissions for the same gate share the idempotency key.
	other := fd.decisionRequest(req)
	if decisionIdentity(req) != decisionIdentity(other) {
		t.Fatal("identical form submissions must share the gate identity")
	}

	posted := decisionFakeGh(t, nil)
	sink := filepath.Join(t.TempDir(), "audit.jsonl")
	res, err := SubmitHumanDecision(context.Background(), AuditConfig{Enabled: true, Sink: sink}, req)
	if err != nil || !res.Accepted || res.Duplicate {
		t.Fatalf("first submit = %+v err=%v", res, err)
	}
	if len(*posted) != 1 {
		t.Fatalf("posted %d comments, want 1", len(*posted))
	}
	// Re-submit: the existing marker makes it a no-op.
	decisionFakeGh(t, *posted)
	res, err = SubmitHumanDecision(context.Background(), AuditConfig{Enabled: true, Sink: sink}, req)
	if err != nil || !res.Accepted || !res.Duplicate {
		t.Fatalf("duplicate submit = %+v err=%v", res, err)
	}
	b, err := os.ReadFile(sink)
	if err != nil {
		t.Fatal(err)
	}
	var rec struct {
		Actor    string `json:"actor"`
		Decision string `json:"decision"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(b))), &rec); err != nil {
		t.Fatalf("audit line: %v (%q)", err, string(b))
	}
	if rec.Actor != "console" || rec.Decision != "request_changes" {
		t.Fatalf("audit record = %+v", rec)
	}
}

// #120: the schema catalog is closed, deterministic, and valid: three forms,
// unique decision ids, every field type in the closed set, choice fields carry
// options, and every mapped action is an existing §18 action.
func TestFormSchemaCatalogClosed(t *testing.T) {
	schemas := decisionFormSchemas()
	if len(schemas) != 3 {
		t.Fatalf("schemas = %d, want 3", len(schemas))
	}
	seen := map[string]bool{}
	for _, s := range schemas {
		if seen[s.Decision] {
			t.Fatalf("duplicate decision id %q", s.Decision)
		}
		seen[s.Decision] = true
		if err := ValidateHumanDecision(s.Action); err != nil {
			t.Fatalf("schema %q action: %v", s.Decision, err)
		}
		names := map[string]bool{}
		for _, f := range s.Fields {
			if names[f.Name] {
				t.Fatalf("schema %q duplicate field %q", s.Decision, f.Name)
			}
			names[f.Name] = true
			switch f.Type {
			case formFieldText:
				if f.MaxLength <= 0 {
					t.Fatalf("text field %q must declare a cap", f.Name)
				}
			case formFieldChoice:
				if len(f.Options) == 0 {
					t.Fatalf("choice field %q must declare options", f.Name)
				}
			case formFieldNumber:
			default:
				t.Fatalf("field %q has unknown type %q", f.Name, f.Type)
			}
		}
		if s.FeedbackField != "" && !names[s.FeedbackField] {
			t.Fatalf("schema %q feedback field %q not declared", s.Decision, s.FeedbackField)
		}
	}
	if !reflect.DeepEqual(schemas, decisionFormSchemas()) {
		t.Fatal("schema catalog must be deterministic")
	}
}

// #120/§36: an unknown or malformed form decision never reaches a decision.
func TestValidateFormSubmissionJSONBoundary(t *testing.T) {
	var sub FormSubmission
	if err := json.Unmarshal([]byte(`{"decision":"request-changes","values":{"summary":"ok","severity":"low"}}`), &sub); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateFormSubmission(sub); err != nil {
		t.Fatalf("json-decoded submission: %v", err)
	}
	if _, err := ValidateFormSubmission(FormSubmission{Decision: "clarification", Values: map[string]any{"answer": "x", "confidence": json.Number("0.7")}}); err != nil {
		t.Fatalf("json.Number confidence: %v", err)
	}
}

// #120/§108: the console form endpoint validates a typed submission against its
// schema AND the live gate, then maps it onto the existing decision writer; a
// malformed form or a closed gate never reaches a decision. The status payload
// advertises the schema catalog.
func TestConsoleFormEndpoint(t *testing.T) {
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
	srv, err := newConsoleServer(ConsoleConfig{Addr: "127.0.0.1:0", Token: "secret-token"}, data, "test")
	if err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(srv)
	t.Cleanup(hs.Close)

	// The status bootstrap advertises the typed form catalog.
	sres, err := http.Get(hs.URL + "/api/v1/status")
	if err != nil {
		t.Fatal(err)
	}
	var st consoleStatus
	if err := json.NewDecoder(sres.Body).Decode(&st); err != nil {
		t.Fatal(err)
	}
	sres.Body.Close()
	if len(st.Forms) != 3 {
		t.Fatalf("status forms = %d, want 3", len(st.Forms))
	}

	postForm := func(body any) *http.Response {
		t.Helper()
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest(http.MethodPost, hs.URL+"/api/v1/cycles/r-issue-7/forms", bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer secret-token")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}

	// A valid request-changes form maps to the existing action + anchored comment.
	res := postForm(FormSubmission{
		Decision: "request-changes",
		Values:   map[string]any{"summary": "please add a test", "severity": "high"},
		Comments: []DiffComment{{File: "a.go", Line: 3, Body: "guard this"}},
		Attempt:  2, SHA: "sha2",
	})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("valid form = %d, want 200", res.StatusCode)
	}
	res.Body.Close()
	if len(*seen) != 1 {
		t.Fatalf("seen = %d, want 1", len(*seen))
	}
	got := (*seen)[0]
	if got.Action != humanDecisionRequestChanges || got.Feedback != "please add a test" ||
		got.Attempt != 2 || got.SHA != "sha2" || len(got.Comments) != 1 || got.Comments[0].SHA != "sha2" {
		t.Fatalf("mapped form = %+v", got)
	}

	// A clarification form maps to the existing resume action (no new state).
	res = postForm(FormSubmission{Decision: "clarification", Values: map[string]any{"answer": "race condition"}, Attempt: 2, SHA: "sha2"})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("clarification = %d, want 200", res.StatusCode)
	}
	res.Body.Close()
	if (*seen)[1].Action != humanDecisionResume {
		t.Fatalf("clarification action = %q, want resume", (*seen)[1].Action)
	}

	// A malformed form never becomes a decision.
	before := len(*seen)
	res = postForm(FormSubmission{Decision: "request-changes", Values: map[string]any{}})
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid form = %d, want 400", res.StatusCode)
	}
	res.Body.Close()
	if len(*seen) != before {
		t.Fatal("invalid form must never reach the decision writer")
	}

	// A form bound to a superseded attempt is refused as stale (409) and never
	// reaches the writer.
	res = postForm(FormSubmission{
		Decision: "approve", Values: map[string]any{}, Attempt: 1, SHA: "sha1",
	})
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("stale form = %d, want 409", res.StatusCode)
	}
	res.Body.Close()
	if len(*seen) != before {
		t.Fatal("stale form must never reach the decision writer")
	}
}
