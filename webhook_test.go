package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
)

// §22: an authenticated delivery (correct HMAC) is accepted; a missing/wrong/
// malformed header or a missing secret is rejected.
func TestVerifySignature(t *testing.T) {
	body := []byte(`{"action":"opened"}`)
	sum := hmac.New(sha256.New, []byte("sekret"))
	sum.Write(body)
	good := "sha256=" + hex.EncodeToString(sum.Sum(nil))

	cases := []struct {
		name   string
		secret string
		header string
		body   []byte
		want   bool
	}{
		{"valid", "sekret", good, body, true},
		{"no secret", "", good, body, false},
		{"no header", "sekret", "", body, false},
		{"wrong secret", "other", good, body, false},
		{"malformed hex", "sekret", "sha256=zz", body, false},
		{"bad prefix", "sekret", "md5=abc", body, false},
		{"tampered body", "sekret", good, []byte(`{"action":"closed"}`), false},
	}
	for _, tc := range cases {
		if got := verifySignature(tc.secret, tc.header, tc.body); got != tc.want {
			t.Fatalf("%s: verifySignature=%v want %v", tc.name, got, tc.want)
		}
	}
}

// ghWebhookRequest signs body with the given secret via X-Hub-Signature-256.
func ghWebhookRequest(secret, event, body string) *http.Request {
	req := httptest.NewRequest("POST", "/webhook", strings.NewReader(body))
	if secret != "" {
		sum := hmac.New(sha256.New, []byte(secret))
		sum.Write([]byte(body))
		req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(sum.Sum(nil)))
	}
	req.Header.Set("X-GitHub-Event", event)
	return req
}

func webhookEnv(t *testing.T) {
	t.Helper()
	os.Setenv("GH_WEBHOOK_SECRET", "sekret")
	t.Cleanup(func() { os.Unsetenv("GH_WEBHOOK_SECRET") })
}

func payload(action, fullPath string, num int) string {
	b, _ := json.Marshal(map[string]interface{}{
		"action":     action,
		"issue":      map[string]interface{}{"number": num, "state": "open"},
		"repository": map[string]interface{}{"full_name": fullPath},
	})
	return string(b)
}

// §32/§41 dual-start convergence: an `opened` webhook fast-path starts the
// workflow with the deterministic <repo>-issue-<n> ID; a second start
// (duplicate sweep) hitting the "already started" error is treated as success —
// webhook + sweep converge on one active run.
func TestWebhookOpenedStartsAndConverges(t *testing.T) {
	webhookEnv(t)
	started := map[string]bool{}
	wh := &WebhookServer{
		repos: []RepoConfig{{Owner: "o", Repo: "r", RepoPath: "/tmp/repo"}},
		starter: func(_ context.Context, scope RepoScope, issue int) error {
			id := scope.Repo + "-issue-" + strconv.Itoa(issue)
			if started[id] {
				return errAlreadyStarted
			}
			started[id] = true
			return nil
		},
	}

	// First delivery: opened starts it.
	w := httptest.NewRecorder()
	wh.handleWebhook(w, ghWebhookRequest("sekret", "issues", payload("opened", "o/r", 7)))
	if w.Code != 200 {
		t.Fatalf("opened status = %d want 200", w.Code)
	}
	if !started["r-issue-7"] {
		t.Fatal("opened webhook must fast-path start r-issue-7")
	}

	// Duplicate (sweep + webhook both fire): converges, no error.
	w = httptest.NewRecorder()
	wh.handleWebhook(w, ghWebhookRequest("sekret", "issues", payload("opened", "o/r", 7)))
	if w.Code != 200 {
		t.Fatalf("duplicate opened status = %d want 200 (converged)", w.Code)
	}
}

// §31/§32: edited/labeled/closed events reconcile WITHOUT restarting; the
// starter is never invoked for them.
func TestWebhookEditedLabeledClosedReconcileOnly(t *testing.T) {
	webhookEnv(t)
	calls := 0
	wh := &WebhookServer{
		repos: []RepoConfig{{Owner: "o", Repo: "r", RepoPath: "/tmp/repo"}},
		starter: func(_ context.Context, _ RepoScope, _ int) error {
			calls++
			return nil
		},
	}
	for _, action := range []string{"edited", "labeled", "closed"} {
		w := httptest.NewRecorder()
		wh.handleWebhook(w, ghWebhookRequest("sekret", "issues", payload(action, "o/r", 7)))
		if w.Code != 200 {
			t.Fatalf("%s status = %d want 200", action, w.Code)
		}
	}
	if calls != 0 {
		t.Fatalf("%s events must never start a workflow, got %d starts", "edited/labeled/closed", calls)
	}
}

// §22: an unsigned or poorly-signed delivery is rejected.
func TestWebhookRejectsBadSignature(t *testing.T) {
	webhookEnv(t)
	wh := &WebhookServer{repos: []RepoConfig{{Owner: "o", Repo: "r", RepoPath: "/tmp/repo"}},
		starter: func(_ context.Context, _ RepoScope, _ int) error { return nil },
	}
	for _, tc := range []struct {
		name string
		req  *http.Request
	}{
		{"missing header", httptest.NewRequest("POST", "/webhook", strings.NewReader(payload("opened", "o/r", 7)))},
		{"wrong secret", ghWebhookRequest("wrong", "issues", payload("opened", "o/r", 7))},
	} {
		w := httptest.NewRecorder()
		wh.handleWebhook(w, tc.req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("%s status = %d want 401", tc.name, w.Code)
		}
	}
}

// Unconfigured repo: authenticated but not in repos.yaml → ignored, still acked.
func TestWebhookUnconfiguredRepoIgnored(t *testing.T) {
	webhookEnv(t)
	wh := &WebhookServer{repos: nil,
		starter: func(_ context.Context, _ RepoScope, _ int) error {
			t.Fatal("starter must not fire for unconfigured repo")
			return nil
		},
	}
	w := httptest.NewRecorder()
	wh.handleWebhook(w, ghWebhookRequest("sekret", "issues", payload("opened", "other/project", 7)))
	if w.Code != 200 {
		t.Fatalf("unconfigured opened status = %d want 200", w.Code)
	}
}

// scopeForRepo inherits the repo's lifecycle config (§33), the resource the
// fast-path start passes down.
func TestScopeForRepoMapsLifecycle(t *testing.T) {
	repos := []RepoConfig{
		{Owner: "o", Repo: "r", RepoPath: "/tmp/r", Lifecycle: &LifecycleConfig{Mode: modeSupervisedAutonomous, DevelopmentMaxComplexity: 5}},
	}
	scope, ok := scopeForRepo("o", "r", repos)
	if !ok || scope.RepoPath != "/tmp/r" || scope.Lifecycle.DevelopmentMaxComplexity != 5 {
		t.Fatalf("scopeForRepo = %+v ok=%v", scope, ok)
	}
	if _, ok := scopeForRepo("o", "other", repos); ok {
		t.Fatal("unknown repo must not map")
	}
}

// errAlreadyStarted is what the Temporal client returns on the deterministic ID.
var errAlreadyStarted = &alreadyStartedError{}

type alreadyStartedError struct{}

func (e *alreadyStartedError) Error() string {
	return "WorkflowExecutionAlreadyStarted: already running"
}
