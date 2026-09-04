package main

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"go.temporal.io/sdk/activity"
)

func b64(data []byte) string {
	return base64.RawURLEncoding.EncodeToString(data)
}

// agentEnv returns an env for child opencode processes with every GitHub
// credential stripped: the App PEM path + any token the shell may carry
// (GH_TOKEN/GITHUB_TOKEN and friends). §23: agents never receive write creds.
// Missing vars are dropped entirely (GH_APP_* pointing at still-readable paths
// would be an opaque hint, not a secret, but dropping is simpler and stricter).
func agentEnv() []string {
	drops := map[string]bool{
		"GH_APP_ID": true, "GH_APP_INSTALL_ID": true, "GH_APP_PEM": true,
		"GH_TOKEN": true, "GITHUB_TOKEN": true, "GH_ENTERPRISE_TOKEN": true,
	}
	var out []string
	for _, kv := range os.Environ() {
		if i := strings.IndexByte(kv, '='); i > 0 && drops[kv[:i]] {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// cmdEnv returns agentEnv() with PWD pinned to dir. opencode resolves its
// project root from the PWD env var rather than the process cwd, so the
// worker's stale PWD would silently point the agent at the wrong repository.
func cmdEnv(dir string) []string {
	out := make([]string, 0, len(os.Environ())+1)
	seen := false
	for _, kv := range agentEnv() {
		if strings.HasPrefix(kv, "PWD=") {
			if !seen {
				out = append(out, "PWD="+dir)
				seen = true
			}
			continue
		}
		out = append(out, kv)
	}
	if !seen {
		out = append(out, "PWD="+dir)
	}
	return out
}

// appToken mints a short-lived GitHub App installation token (kakashi-82).
// Env: GH_APP_ID, GH_APP_INSTALL_ID, GH_APP_PEM (path to private key).
func appToken(ctx context.Context) (string, error) {
	appID := os.Getenv("GH_APP_ID")
	installID := os.Getenv("GH_APP_INSTALL_ID")
	pemPath := os.Getenv("GH_APP_PEM")
	if appID == "" || installID == "" || pemPath == "" {
		return "", fmt.Errorf("GH_APP_ID/GH_APP_INSTALL_ID/GH_APP_PEM not set")
	}

	keyPEM, err := os.ReadFile(pemPath)
	if err != nil {
		return "", fmt.Errorf("read pem: %w", err)
	}
	block, _ := pem.Decode(keyPEM)
	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		k, e2 := x509.ParsePKCS8PrivateKey(block.Bytes)
		if e2 != nil {
			return "", fmt.Errorf("parse key: %w", err)
		}
		key = k.(*rsa.PrivateKey)
	}

	// JWT (RS256)
	now := time.Now()
	header := b64([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims := b64([]byte(fmt.Sprintf(`{"iat":%d,"exp":%d,"iss":%q}`,
		now.Add(-60*time.Second).Unix(), now.Add(9*time.Minute).Unix(), appID)))
	signing := header + "." + claims
	hashed := sha256.Sum256([]byte(signing))
	sig, _ := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, hashed[:])
	jwt := signing + "." + b64(sig)

	// POST /app/installations/{id}/access_tokens
	req, _ := http.NewRequestWithContext(ctx, "POST",
		"https://api.github.com/app/installations/"+installID+"/access_tokens", nil)
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 201 {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("github install token: %s %s", resp.Status, string(b))
	}
	var tok struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil {
		return "", err
	}
	return tok.Token, nil
}

// PrInput carries what defines a review task.
type PrInput struct {
	Owner     string          `json:"owner"`
	Repo      string          `json:"repo"`
	PrNum     int             `json:"pr"`
	RepoPath  string          `json:"repoPath"`
	Lifecycle LifecycleConfig `json:"lifecycle,omitempty"`
}

// IssueInput identifies one issue to run through the issue lifecycle cycle.
type IssueInput struct {
	Owner     string          `json:"owner"`
	Repo      string          `json:"repo"`
	IssueNum  int             `json:"issue"`
	RepoPath  string          `json:"repoPath"`
	Lifecycle LifecycleConfig `json:"lifecycle,omitempty"`
}

// RepoScope defines one repo to sweep. Passed as schedule input so the same
// workflow type is reused across repos (one schedule per repo, different input).
type RepoScope struct {
	Owner     string          `json:"owner"`
	Repo      string          `json:"repo"`
	RepoPath  string          `json:"repoPath"`
	Lifecycle LifecycleConfig `json:"lifecycle,omitempty"`
}

// ListUnreviewedPRs returns open PRs in the repo indicated by scope that have
// no active review from the kakashi-82 bot.
func ListUnreviewedPRs(ctx context.Context, scope RepoScope) ([]PrInput, error) {
	var out []PrInput
	ownerRepo := scope.Owner + "/" + scope.Repo

	list, err := exec.Command("gh", "pr", "list", "-R", ownerRepo,
		"--state", "open", "--json", "number", "--jq", ".[].number").Output()
	if err != nil {
		return nil, fmt.Errorf("gh pr list %s: %w", ownerRepo, err)
	}
	for _, num := range strings.Fields(string(list)) {
		n := 0
		fmt.Sscan(num, &n)
		// active review present?
		revs, err := exec.Command("gh", "api",
			"/repos/"+ownerRepo+"/pulls/"+num+"/reviews",
			"--jq", `[.[] | select(.user.login|endswith("[bot]")) | select(.state != "DISMISSED")] | length`).Output()
		if err == nil && strings.TrimSpace(string(revs)) != "0" {
			continue // already has an active bot review
		}
		out = append(out, PrInput{Owner: scope.Owner, Repo: scope.Repo, PrNum: n, RepoPath: scope.RepoPath, Lifecycle: scope.Lifecycle})
	}
	activity.GetLogger(ctx).Info("found unreviewed PRs", "repo", ownerRepo, "count", len(out))
	return out, nil
}

// ListUnmanagedIssues returns open issues in the repo indicated by scope that
// are missing the konoha:managed label. Mirror of ListUnreviewedPRs for the
// issue sweep (§32). Label writes are a later issue; for now "unmanaged"
// simply means the label is absent.
func ListUnmanagedIssues(ctx context.Context, scope RepoScope) ([]IssueInput, error) {
	var out []IssueInput
	ownerRepo := scope.Owner + "/" + scope.Repo

	list, err := exec.Command("gh", "issue", "list", "-R", ownerRepo,
		"--state", "open", "--json", "number,labels",
		"--jq", `.[] | select(.labels | any(.name == "konoha:managed") | not) | .number`).Output()
	if err != nil {
		return nil, fmt.Errorf("gh issue list %s: %w", ownerRepo, err)
	}
	for _, num := range strings.Fields(string(list)) {
		n := 0
		fmt.Sscan(num, &n)
		out = append(out, IssueInput{Owner: scope.Owner, Repo: scope.Repo, IssueNum: n, RepoPath: scope.RepoPath, Lifecycle: scope.Lifecycle})
	}
	activity.GetLogger(ctx).Info("found unmanaged issues", "repo", ownerRepo, "count", len(out))
	return out, nil
}

// ReviewResult is what a reviewer (independent or legacy) returns. Verdict is
// APPROVE or REQUEST_CHANGES; Summary is the rendered review body. PRNumber and
// CommitSHA bind the verdict per §15: later code should prefer the richer
// reviewer.go flow, which always fills them.
type ReviewResult struct {
	Verdict   string `json:"verdict"` // APPROVE or REQUEST_CHANGES
	Summary   string `json:"summary"`
	PRNumber  int    `json:"prNumber,omitempty"`
	CommitSHA string `json:"commitSha,omitempty"`
}

// FetchDiff pulls the PR diff via the gh CLI.
func FetchDiff(ctx context.Context, in PrInput) (string, error) {
	out, err := exec.Command("gh", "pr", "diff", fmt.Sprint(in.PrNum),
		"-R", in.Owner+"/"+in.Repo).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("gh pr diff: %s: %w", strings.TrimSpace(string(out)), err)
	}
	activity.GetLogger(ctx).Info("fetched diff", "pr", in.PrNum)
	return string(out), nil
}

// ReviewWithOpencode runs the local opencode agent against the diff.
// The agent must print a line: "VERDICT:<APPROVE|REQUEST_CHANGES>".
func ReviewWithOpencode(ctx context.Context, in PrInput, diff string) (ReviewResult, error) {
	prompt := fmt.Sprintf(
		`Review the code in the diff below (PR #%d of %s/%s) against: correctness, security, code quality, testing, performance, docs.
If any critical or warning-level issue exists, verdict is REQUEST_CHANGES; otherwise APPROVE.
Do not modify any code.
Output EXACTLY this markdown format (no extra text):

VERDICT:APPROVE|VERDICT:REQUEST_CHANGES
BLOCKING:
- concrete finding or "none"
NITS:
- concrete finding or "none"
CONCLUDE: one-line verdict sentence

DIFF:
%s`, in.PrNum, in.Owner, in.Repo, diff)

	cmd := exec.Command("opencode", "run", prompt)
	cmd.Dir = in.RepoPath
	cmd.Env = cmdEnv(in.RepoPath) // §23: no GitHub write credential reaches the agent
	out, err := cmd.CombinedOutput()
	if err != nil {
		return ReviewResult{}, fmt.Errorf("opencode run: %s: %w", strings.TrimSpace(string(out)), err)
	}

	// Shared fail-closed parser (reviewer.go): VERDICT:/BLOCKING:/NITS:/CONCLUDE:
	// sections, defaulting to REQUEST_CHANGES on any malformed output.
	verdict, summary := parseReviewOutput(out)
	return ReviewResult{Verdict: verdict, Summary: summary, PRNumber: in.PrNum}, nil
}

// SubmitReview posts the verdict as a single formal review (the --body carries
// the summary). Posts as the kakashi-82 GitHub App so it can approve its own PRs.
func SubmitReview(ctx context.Context, in PrInput, r ReviewResult) error {
	if in.Lifecycle.DryRun {
		activity.GetLogger(ctx).Info("dryRun: review suppressed", "pr", in.PrNum)
		return nil
	}
	verdict := r.Verdict
	if r.Verdict == "" {
		verdict = "APPROVE"
	}
	flag := "--request-changes"
	if verdict == "APPROVE" {
		flag = "--approve"
	}
	body := r.Summary // already markdown-formatted with verdict + findings

	tok, err := appToken(ctx)
	if err != nil {
		return err
	}
	c := exec.Command("gh", "pr", "review", fmt.Sprint(in.PrNum), "-R", in.Owner+"/"+in.Repo,
		flag, "--body", body)
	c.Env = append(os.Environ(), "GH_TOKEN="+tok)
	if out, err := c.CombinedOutput(); err != nil {
		return fmt.Errorf("gh pr review: %s: %w", strings.TrimSpace(string(out)), err)
	}
	activity.GetLogger(ctx).Info("submitted review", "pr", in.PrNum, "verdict", r.Verdict)
	return nil
}
