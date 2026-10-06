package konoha

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// decisionFakeGh simulates the gh CLI for SubmitHumanDecision: `issue view
// --json comments` returns existing comment bodies, `issue comment --body` is
// captured. Returns the captured posted bodies.
func decisionFakeGh(t *testing.T, existing []string) *[]string {
	t.Helper()
	posted := &[]string{}
	prev := ghRunner
	t.Cleanup(func() { ghRunner = prev })
	ghRunner = func(_ context.Context, args ...string) ([]byte, error) {
		if len(args) >= 2 && args[0] == "issue" && args[1] == "view" {
			var doc struct {
				Comments []struct {
					Body string `json:"body"`
				} `json:"comments"`
			}
			for _, b := range existing {
				doc.Comments = append(doc.Comments, struct {
					Body string `json:"body"`
				}{Body: b})
			}
			return json.Marshal(doc)
		}
		if len(args) >= 2 && args[0] == "issue" && args[1] == "comment" {
			for i := range args {
				if args[i] == "--body" && i+1 < len(args) {
					*posted = append(*posted, args[i+1])
				}
			}
		}
		return nil, nil
	}
	return posted
}

// §108: request-changes requires non-empty, size-bounded feedback for the gate;
// approve/deny carry none; a diff-anchored comment on a superseded SHA is stale.
func TestValidateDecisionRequest(t *testing.T) {
	base := func(a string) DecisionRequest {
		return DecisionRequest{CycleID: "r-issue-7", Attempt: 2, SHA: "sha2", Action: a}
	}
	if err := ValidateDecisionRequest(base(humanDecisionApprove)); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if err := ValidateDecisionRequest(base(humanDecisionDeny)); err != nil {
		t.Fatalf("deny: %v", err)
	}
	if err := ValidateDecisionRequest(base("ship")); err == nil {
		t.Fatal("unknown action must fail closed")
	}
	// request-changes needs feedback.
	rc := base(humanDecisionRequestChanges)
	if err := ValidateDecisionRequest(rc); err == nil {
		t.Fatal("request-changes without feedback must fail")
	}
	rc.Feedback = "   "
	if err := ValidateDecisionRequest(rc); err == nil {
		t.Fatal("whitespace-only feedback must fail")
	}
	rc.Feedback = strings.Repeat("x", maxDecisionFeedback+1)
	if err := ValidateDecisionRequest(rc); err == nil {
		t.Fatal("over-long feedback must fail")
	}
	rc.Feedback = "please fix the nil deref"
	if err := ValidateDecisionRequest(rc); err != nil {
		t.Fatalf("valid request-changes: %v", err)
	}
	// A comment anchored to a superseded SHA is stale.
	rc.Comments = []DiffComment{{File: "a.go", Line: 3, SHA: "oldsa", Body: "nit"}}
	if err := ValidateDecisionRequest(rc); err == nil {
		t.Fatal("comment on a superseded SHA must be rejected")
	}
	rc.Comments = []DiffComment{{File: "a.go", Line: 3, SHA: "sha2", Body: "nit"}}
	if err := ValidateDecisionRequest(rc); err != nil {
		t.Fatalf("valid anchored comment: %v", err)
	}
	// approve carrying feedback is malformed.
	bad := base(humanDecisionApprove)
	bad.Feedback = "nope"
	if err := ValidateDecisionRequest(bad); err == nil {
		t.Fatal("approve with feedback must be rejected")
	}
	// attempt/sha are the gate identity.
	noAttempt := base(humanDecisionApprove)
	noAttempt.Attempt = 0
	if err := ValidateDecisionRequest(noAttempt); err == nil {
		t.Fatal("missing attempt must be rejected")
	}
}

// §108: a decision is written as one §18 marker comment and a repeated submit is
// a no-op (idempotent), never a second decision.
func TestSubmitHumanDecisionIdempotent(t *testing.T) {
	posted := decisionFakeGh(t, nil)
	in := DecisionRequest{
		Owner: "o", Repo: "r", Issue: 7, CycleID: "r-issue-7",
		Action: humanDecisionRequestChanges, Feedback: "fix the leak",
		Attempt: 2, SHA: "sha2", Actor: "alice",
		Comments: []DiffComment{{File: "a.go", Line: 3, SHA: "sha2", Body: "close it"}},
	}
	res, err := SubmitHumanDecision(context.Background(), AuditConfig{}, in)
	if err != nil || !res.Accepted || res.Duplicate {
		t.Fatalf("first submit = %+v err=%v", res, err)
	}
	if len(*posted) != 1 {
		t.Fatalf("posted %d comments, want 1", len(*posted))
	}
	body := (*posted)[0]
	if !strings.Contains(body, "<!-- konoha-decision: "+humanDecisionRequestChanges+" -->") {
		t.Fatalf("comment must carry the §18 marker: %q", body)
	}
	if !strings.Contains(body, decisionIDMarker(in)) {
		t.Fatalf("comment must carry the idempotency marker: %q", body)
	}
	// The payload decodes back to the feedback + anchored comment.
	if p, ok := parseDecisionPayload(body); !ok || p.Feedback != "fix the leak" || len(p.Comments) != 1 {
		t.Fatalf("payload = %+v ok=%v", p, ok)
	}

	// Re-submit the same gate: the existing comment carries the id marker → no-op.
	decisionFakeGhWith := func() *[]string {
		return decisionFakeGh(t, []string{body})
	}
	posted2 := decisionFakeGhWith()
	res, err = SubmitHumanDecision(context.Background(), AuditConfig{}, in)
	if err != nil || !res.Accepted || !res.Duplicate {
		t.Fatalf("duplicate submit = %+v err=%v", res, err)
	}
	if len(*posted2) != 0 {
		t.Fatalf("duplicate must post nothing, got %d", len(*posted2))
	}
}

// §108/§55: every accepted decision is appended to the audit sink with actor
// attribution and the mapped decision.
func TestSubmitHumanDecisionAudits(t *testing.T) {
	decisionFakeGh(t, nil)
	sink := filepath.Join(t.TempDir(), "audit.jsonl")
	res, err := SubmitHumanDecision(context.Background(), AuditConfig{Enabled: true, Sink: sink}, DecisionRequest{
		Owner: "o", Repo: "r", Issue: 7, CycleID: "r-issue-7",
		Action: humanDecisionApprove, Attempt: 2, SHA: "sha2", Actor: "bob",
	})
	if err != nil || !res.Accepted {
		t.Fatalf("submit = %+v err=%v", res, err)
	}
	b, err := os.ReadFile(sink)
	if err != nil {
		t.Fatal(err)
	}
	var rec struct {
		Actor    string `json:"actor"`
		Decision string `json:"decision"`
		Step     string `json:"step"`
		Issue    int    `json:"issue"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(b))), &rec); err != nil {
		t.Fatalf("audit line: %v (%q)", err, string(b))
	}
	if rec.Actor != "bob" || rec.Decision != "allow" || rec.Step != auditStepConsoleDecision || rec.Issue != 7 {
		t.Fatalf("audit record = %+v", rec)
	}
}

// §108/#97: a request-changes decision records a rework activation carrying the
// feedback, the change feedback thread, and the cycle's diff-anchored comments.
func TestWriteConsoleDecisionRequestChangesEvidence(t *testing.T) {
	decisionFakeGh(t, nil)
	store, err := NewEvidenceStore(EvidenceConfig{Dir: t.TempDir()}, &defaultRedactor)
	if err != nil {
		t.Fatal(err)
	}
	in := DecisionRequest{
		Owner: "o", Repo: "r", Issue: 7, CycleID: "r-issue-7",
		Action: humanDecisionRequestChanges, Feedback: "please add a test",
		Attempt: 2, SHA: "sha2", Actor: "carol",
		Comments: []DiffComment{{File: "a.go", Line: 9, SHA: "sha2", Body: "guard this"}},
	}
	res, err := WriteConsoleDecision(context.Background(), store, AuditConfig{}, in)
	if err != nil || !res.Accepted {
		t.Fatalf("write = %+v err=%v", res, err)
	}

	l, err := store.loadLineage("o", "r", 7, false)
	if err != nil {
		t.Fatal(err)
	}
	if l.Attempts != 1 || l.Activations[0].ReworkNote != "please add a test" ||
		l.Activations[0].Reason != activationReasonRepair || l.Activations[0].Attempt != 3 {
		t.Fatalf("lineage = %+v", l.Activations)
	}

	c, err := store.loadChange("r", 7)
	if err != nil {
		t.Fatal(err)
	}
	if c.ReworkNote != "please add a test" || len(c.Feedback) != 1 || c.Feedback[0].Author != "carol" {
		t.Fatalf("change = %+v", c)
	}
	if c.Status != changeStatusOpen {
		t.Fatalf("request-changes must return the change to the working path (OPEN), got %q", c.Status)
	}

	raw, ok := store.readArtifact("r-issue-7", evidenceComments)
	if !ok {
		t.Fatal("comments.json missing")
	}
	var cm []DiffComment
	if err := json.Unmarshal([]byte(raw), &cm); err != nil || len(cm) != 1 {
		t.Fatalf("comments evidence = %q err=%v", raw, err)
	}
	if cm[0].SHA != "sha2" || cm[0].Attempt != 2 || cm[0].CycleID != "r-issue-7" || cm[0].ID == "" {
		t.Fatalf("carried comment = %+v", cm[0])
	}
}

// §108/§55: an approval written by the console (SubmitHumanDecision) is the
// exact signal AwaitApproval consumes — the console path drives the workflow
// gate without any parallel state.
func TestConsoleDecisionReachesAwaitApproval(t *testing.T) {
	posted := decisionFakeGh(t, nil)
	if _, err := SubmitHumanDecision(context.Background(), AuditConfig{}, DecisionRequest{
		Owner: "o", Repo: "r", Issue: 7, CycleID: "r-issue-7",
		Action: humanDecisionApprove, Attempt: 1, SHA: "s1", Actor: "console",
	}); err != nil {
		t.Fatal(err)
	}
	if len(*posted) != 1 {
		t.Fatalf("posted %d comments, want 1", len(*posted))
	}
	body := (*posted)[0]

	state := struct {
		Body string `json:"body"`
	}{}
	prev := ghRunner
	t.Cleanup(func() { ghRunner = prev })
	ghRunner = func(_ context.Context, args ...string) ([]byte, error) {
		if len(args) >= 2 && strings.HasSuffix(args[1], "/issues/7/comments") {
			return []byte(`[]`), nil
		}
		// Serve the console's comment as the issue body carrier.
		state.Body = body
		return json.Marshal(state)
	}
	res, err := AwaitApproval(context.Background(), ApprovalRequest{
		Owner: "o", Repo: "r", Num: 7, Step: approvalStepPublish,
	})
	if err != nil || !res.Granted || res.Denied || res.Rework {
		t.Fatalf("console approve → %+v err=%v", res, err)
	}
}

// §108/§18: the decision comment round-trips through ReadHumanDecision, carrying
// the request-changes feedback and comments; the marker remains the source of
// truth (no parallel state).
func TestReadHumanDecisionCarriesPayload(t *testing.T) {
	in := DecisionRequest{
		Owner: "o", Repo: "r", Issue: 7, CycleID: "r-issue-7",
		Action: humanDecisionRequestChanges, Feedback: "tighten the loop",
		Attempt: 1, SHA: "s1", Actor: "dan",
		Comments: []DiffComment{{File: "b.go", Line: 2, SHA: "s1", Body: "bounds"}},
	}
	body := humanDecisionCommentBody(in)
	prev := ghRunner
	t.Cleanup(func() { ghRunner = prev })
	ghRunner = func(_ context.Context, args ...string) ([]byte, error) {
		if len(args) >= 2 && strings.HasSuffix(args[1], "/issues/7") {
			b, _ := json.Marshal(struct{ Body string }{Body: ""})
			return b, nil
		}
		if len(args) >= 2 && strings.HasSuffix(args[1], "/issues/7/comments") {
			b, _ := json.Marshal([]struct {
				Body string `json:"body"`
			}{{Body: body}})
			return b, nil
		}
		return nil, nil
	}
	d, err := ReadHumanDecision(context.Background(), HumanDecisionInput{Owner: "o", Repo: "r", Num: 7})
	if err != nil {
		t.Fatal(err)
	}
	if !d.Present || d.Action != humanDecisionRequestChanges || d.Feedback != "tighten the loop" || len(d.Comments) != 1 {
		t.Fatalf("decision = %+v", d)
	}
}

// §108/§15: the console decision endpoint validates against the LIVE gate: an
// open gate + current attempt/SHA accepts; a superseded attempt or a closed gate
// is refused with 409; the action is mapped onto the request; malformed input is
// 400; a disabled console is 501.
func TestConsoleDecisionEndpoint(t *testing.T) {
	newDecisionConsole := func(t *testing.T, open bool) (*ConsoleServer, *httptest.Server, *[]DecisionRequest) {
		t.Helper()
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
				if !open {
					return DecisionResult{}, errors.New("should not be called")
				}
				*seen = append(*seen, in)
				return DecisionResult{Accepted: true, Action: in.Action, Attempt: in.Attempt, SHA: in.SHA}, nil
			},
		}
		if !open {
			data.status = func(context.Context, string) (StatusQuery, bool) { return StatusQuery{State: stateDeveloping}, true }
		}
		srv, err := newConsoleServer(ConsoleConfig{Addr: "127.0.0.1:0", Token: "secret-token"}, data, "test")
		if err != nil {
			t.Fatal(err)
		}
		hs := httptest.NewServer(srv)
		t.Cleanup(hs.Close)
		return srv, hs, seen
	}
	post := func(t *testing.T, hs *httptest.Server, bearer string, body any, origin string) *http.Response {
		t.Helper()
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest(http.MethodPost, hs.URL+"/api/v1/cycles/r-issue-7/decisions", bytes.NewReader(b))
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}

	_, hs, seen := newDecisionConsole(t, true)
	res := post(t, hs, "secret-token", DecisionRequest{Action: humanDecisionApprove, Attempt: 2, SHA: "sha2"}, "")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("approve = %d, want 200", res.StatusCode)
	}
	res.Body.Close()
	if len(*seen) != 1 || (*seen)[0].Owner != "o" || (*seen)[0].Issue != 7 || (*seen)[0].CycleID != "r-issue-7" || (*seen)[0].Actor != "console" {
		t.Fatalf("mapped request = %+v", *seen)
	}

	// Superseded attempt → 409.
	res = post(t, hs, "secret-token", DecisionRequest{Action: humanDecisionApprove, Attempt: 1, SHA: "sha1"}, "")
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("stale attempt = %d, want 409", res.StatusCode)
	}
	res.Body.Close()
	// request-changes without feedback → 400.
	res = post(t, hs, "secret-token", DecisionRequest{Action: humanDecisionRequestChanges, Attempt: 2, SHA: "sha2"}, "")
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("request-changes without feedback = %d, want 400", res.StatusCode)
	}
	res.Body.Close()
	// Malformed JSON → 400.
	req, _ := http.NewRequest(http.MethodPost, hs.URL+"/api/v1/cycles/r-issue-7/decisions", strings.NewReader("{not json"))
	req.Header.Set("Authorization", "Bearer secret-token")
	res, _ = http.DefaultClient.Do(req)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed body = %d, want 400", res.StatusCode)
	}
	res.Body.Close()
	// A foreign Origin on a browser mutation → 403 (CSRF token alone is not enough).
	srv, _, _ := newDecisionConsole(t, true)
	req, _ = http.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set(consoleCSRFHeader, srv.csrf)
	req.Header.Set("Origin", "https://evil.example")
	if srv.mutationAllowed(req) {
		t.Fatal("foreign-origin browser mutation must be rejected")
	}

	// Closed gate → 409.
	_, hsClosed, _ := newDecisionConsole(t, false)
	res = post(t, hsClosed, "secret-token", DecisionRequest{Action: humanDecisionApprove, Attempt: 2, SHA: "sha2"}, "")
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("closed gate = %d, want 409", res.StatusCode)
	}
	res.Body.Close()

	// Disabled console → 501.
	disabled, _ := newConsoleServer(ConsoleConfig{Addr: "127.0.0.1:0", Token: "t"},
		&consoleData{store: &EvidenceStore{}}, "test")
	hs2 := httptest.NewServer(disabled)
	t.Cleanup(hs2.Close)
	req, _ = http.NewRequest(http.MethodPost, hs2.URL+"/api/v1/cycles/r-issue-7/decisions", strings.NewReader(`{"action":"approve","attempt":2,"sha":"sha2"}`))
	req.Header.Set("Authorization", "Bearer t")
	res, _ = http.DefaultClient.Do(req)
	if res.StatusCode != http.StatusNotImplemented {
		t.Fatalf("disabled decisions = %d, want 501", res.StatusCode)
	}
	res.Body.Close()
}

// §108: a URL-parse smoke check keeps the test file's net/url import honest and
// documents the loopback-origin rule the endpoint relies on.
func TestLoopbackOriginRule(t *testing.T) {
	for _, tc := range []struct {
		origin string
		ok     bool
	}{
		{"http://127.0.0.1:8080", true},
		{"http://localhost:3000", true},
		{"https://evil.example", false},
		{"", true},
	} {
		u, err := url.Parse(tc.origin)
		if tc.origin == "" {
			if err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		got := isLoopbackHost(u.Hostname())
		if got != tc.ok {
			t.Fatalf("origin %q loopback = %v, want %v", tc.origin, got, tc.ok)
		}
	}
}
