package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"

	"go.temporal.io/sdk/client"
)

// Event-driven trigger (§32): a GitHub webhook starts the lifecycle workflow on
// issue events while the periodic sweep stays the reconciliation backstop. The
// deterministic <repo>-issue-<n> workflow ID makes a webhook start and a sweep
// start CONVERGE on one active execution (duplicate starts are harmless).
//
// Auth: GitHub signs each delivery with the App webhook secret
// ("X-Hub-Signature-256: sha256=<hex>", HMAC-SHA256 over the raw body).
// Deliveries without a valid signature are rejected (§22: webhook auth
// verified).

// webhookSecret returns the GitHub App webhook secret from env
// (GH_WEBHOOK_SECRET). Empty disables webhook receiving.
func webhookSecret() string { return os.Getenv("GH_WEBHOOK_SECRET") }

// verifySignature validates the X-Hub-Signature-256 HMAC-SHA256 header over the
// raw body using the App webhook secret (§22). Constant-time compare; a missing
// header or secret rejects.
func verifySignature(secret, header string, body []byte) bool {
	if secret == "" || header == "" {
		return false
	}
	const prefix = "sha256="
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	want, err := hex.DecodeString(strings.TrimPrefix(header, prefix))
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal(want, mac.Sum(nil))
}

// webhookEvent is the GitHub issue-event payload subset we act on (§32:
// opened/edited/labeled/closed).
type webhookEvent struct {
	Action string `json:"action"`
	Issue  struct {
		Number int    `json:"number"`
		State  string `json:"state"`
	} `json:"issue"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
}

// webhookStarter is the fast-path seam: given a repo scope + issue it starts
// (or reuses) the IssueCycleWorkflow with the deterministic §32 ID. Tests inject
// a fake; production uses WebhookServer's default starter.
type webhookStarter func(ctx context.Context, scope RepoScope, issue int) error

// scopeForRepo finds the configured RepoScope (owner/repo) from repos.yaml, so
// the webhook inherits the repo's lifecycle policy (§33).
func scopeForRepo(owner, repo string, repos []RepoConfig) (RepoScope, bool) {
	for _, r := range repos {
		if r.Owner == owner && r.Repo == repo {
			scope := RepoScope{Owner: r.Owner, Repo: r.Repo, RepoPath: r.RepoPath}
			if r.Lifecycle != nil {
				scope.Lifecycle = *r.Lifecycle
			}
			return scope, true
		}
	}
	return RepoScope{}, false
}

// WebhookServer serves the GitHub webhook endpoint. Starter is swappable so
// tests can simulate the fast-path without a Temporal client.
type WebhookServer struct {
	repos   []RepoConfig
	starter webhookStarter
}

// DefaultStarter is the production fast-path: start IssueCycleWorkflow with the
// deterministic <repo>-issue-<n> ID. WorkflowExecutionAlreadyStarted is surfaced
// (handleWebhook normalizes it to convergence) — the webhook and the sweep
// converge on one active run (§32).
func DefaultStarter(c client.Client) webhookStarter {
	return func(ctx context.Context, scope RepoScope, issue int) error {
		opts := client.StartWorkflowOptions{
			ID:        fmt.Sprintf("%s-issue-%d", scope.Repo, issue),
			TaskQueue: taskQueue,
		}
		_, err := c.ExecuteWorkflow(ctx, opts, "IssueCycleWorkflow", IssueInput{
			Owner: scope.Owner, Repo: scope.Repo, IssueNum: issue,
			RepoPath: scope.RepoPath, Lifecycle: scope.Lifecycle,
		})
		return err
	}
}

// isAlreadyStarted reports whether a Temporal start error is
// WorkflowExecutionAlreadyStarted — the deterministic-ID convergence case (§32).
func isAlreadyStarted(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "already started") ||
		strings.Contains(err.Error(), "WorkflowExecutionAlreadyStarted"))
}

// handleWebhook is the HTTP handler: verify HMAC → parse event → route.
//   - opened  → fast-path start (idempotent by workflow ID)
//   - edited / labeled / closed → reconcile (do NOT restart a completed
//     workflow; the sweep + running workflow re-read GitHub state, §31/§32)
func (wh *WebhookServer) handleWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	if !verifySignature(webhookSecret(), r.Header.Get("X-Hub-Signature-256"), body) {
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}
	var ev webhookEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if ev.Repository.FullName == "" || ev.Issue.Number == 0 {
		http.Error(w, "missing payload fields", http.StatusBadRequest)
		return
	}
	split := strings.Split(ev.Repository.FullName, "/")
	if len(split) != 2 {
		http.Error(w, "bad repo full_name", http.StatusBadRequest)
		return
	}
	owner, repo := split[0], split[1]
	scope, ok := scopeForRepo(owner, repo, wh.repos)
	if !ok {
		// Unconfigured repo: the sweep never sweeps it either, so ignore the
		// event; still acked (200) since the delivery was authenticated.
		log.Printf("webhook ignored for unconfigured repo %s", ev.Repository.FullName)
		w.WriteHeader(http.StatusOK)
		return
	}

	switch ev.Action {
	case "opened":
		if err := wh.starter(r.Context(), scope, ev.Issue.Number); err != nil {
			// §32: a concurrent sweep start (deterministic ID) yells
			// "already started". That is CONVERGENCE, not failure — ack it.
			if !isAlreadyStarted(err) {
				http.Error(w, "start workflow: "+err.Error(), http.StatusInternalServerError)
				return
			}
		}
		log.Printf("webhook opened → started %s-issue-%d", scope.Repo, ev.Issue.Number)
	case "edited", "labeled", "closed":
		// §31/§32 reconcile: never restart a completed workflow; state is
		// reconciled by the running cycle + the sweep on the next pass.
		log.Printf("webhook %s %s/%s#%d → reconcile (no restart)", ev.Action, owner, repo, ev.Issue.Number)
	}
	w.WriteHeader(http.StatusOK)
	io.WriteString(w, "ok")
}

// StartWebhookServer runs the webhook HTTP listener at WEBHOOK_ADDR when
// GH_WEBHOOK_SECRET is set (both required). Returns nil when disabled. The
// listener is best-effort (logs bind errors via net/http).
func StartWebhookServer(c client.Client, repos []RepoConfig) *http.Server {
	if webhookSecret() == "" {
		return nil
	}
	addr := os.Getenv("WEBHOOK_ADDR")
	if addr == "" {
		return nil
	}
	wh := &WebhookServer{
		repos:   repos,
		starter: DefaultStarter(c),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/webhook", wh.handleWebhook)
	srv := &http.Server{Addr: addr, Handler: mux}
	go srv.ListenAndServe()
	log.Printf("webhook server listening on %s (path /webhook)", addr)
	return srv
}
