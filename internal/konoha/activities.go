package konoha

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

// defaultAgentModel is the fallback opencode model for a repo that leaves
// lifecycle.model unset in repos.yaml. A per-repo value always wins.
const defaultAgentModel = "opencode-go/deepseek-v4.1-flash"

// agentArgs is the shared opencode CLI invocation (run + resolved model) used by
// every agent entrypoint (§4.1-§4.8). model comes from the repo's
// lifecycle.model; empty falls back to defaultAgentModel so the run is never
// left to the host's opencode default.
func agentArgs(prompt, model string) []string {
	if strings.TrimSpace(model) == "" {
		model = defaultAgentModel
	}
	return []string{"run", "--model", model, prompt}
}

// agentEnv returns an env for child opencode processes with every GitHub and
// common cloud/provider credential stripped (§23/§90(a)): the App PEM path +
// any token the shell may carry (GH_TOKEN/GITHUB_TOKEN and friends), plus
// AWS_*/GOOGLE_*/AZURE_*/OPENAI_API_KEY/ANTHROPIC_API_KEY/NPM_TOKEN. Missing
// vars are dropped entirely (GH_APP_* pointing at still-readable paths would be
// an opaque hint, not a secret, but dropping is simpler and stricter). An
// operator who needs a provider var in the agent uses the generic harness's
// explicit passEnv allowlist.
func agentEnv() []string {
	var out []string
	for _, kv := range os.Environ() {
		if i := strings.IndexByte(kv, '='); i > 0 {
			k := kv[:i]
			if isGitHubCredentialEnv(k) || isCloudCredentialEnv(k) {
				continue
			}
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
// KONOHA_TOKEN_FIXED (e2e tier-1 only): a fixed installation token for hermetic
// local runs where GitHub is faked; never honored in production (no env var).
func appToken(ctx context.Context) (string, error) {
	if fixed := os.Getenv("KONOHA_TOKEN_FIXED"); fixed != "" {
		return fixed, nil
	}
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
	Trigger   string          `json:"trigger,omitempty"` // sweep | webhook | split (claim-comment wording)
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
		out = append(out, IssueInput{Owner: scope.Owner, Repo: scope.Repo, IssueNum: n, RepoPath: scope.RepoPath, Trigger: "sweep", Lifecycle: scope.Lifecycle})
	}
	activity.GetLogger(ctx).Info("found unmanaged issues", "repo", ownerRepo, "count", len(out))
	return out, nil
}

// ReviewResult is what a reviewer (independent or legacy) returns. Verdict is
// APPROVE or REQUEST_CHANGES; Summary is the rendered review body. PRNumber and
// CommitSHA bind the verdict per §15: later code should prefer the richer
// reviewer.go flow, which always fills them. Blocking/Nits are the structured
// findings (§35) — the raw bullets fed to the repair agent; a renderer derives
// Summary from them.
type ReviewResult struct {
	Verdict   string         `json:"verdict"` // APPROVE or REQUEST_CHANGES
	Summary   string         `json:"summary"`
	PRNumber  int            `json:"prNumber,omitempty"`
	CommitSHA string         `json:"commitSha,omitempty"`
	Blocking  []string       `json:"blocking,omitempty"`
	Nits      []string       `json:"nits,omitempty"`
	Evidence  *AgentEvidence `json:"evidence,omitempty"` // #83 raw prompt/streams (redacted on write)
	Usage     HarnessUsage   `json:"usage,omitempty"`    // #87 best-effort tokens/cost (zero = not reported)
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

	cmd, err := agentCommand(ctx, prompt, in.Lifecycle.Model, in.Lifecycle.Harness, in.RepoPath)
	if err != nil {
		return ReviewResult{}, err
	}
	out, stderr, err := runCapture(cmd)
	combined := out + stderr
	if err != nil {
		return ReviewResult{}, fmt.Errorf("opencode run: %s: %w", Redact(strings.TrimSpace(combined)), err)
	}

	// Shared fail-closed parser (reviewer.go): VERDICT:/BLOCKING:/NITS:/CONCLUDE:
	// sections, defaulting to REQUEST_CHANGES on any malformed output.
	verdict, blocking, nits, summary := parseReviewOutput([]byte(combined))
	return ReviewResult{Verdict: verdict, Summary: summary, PRNumber: in.PrNum,
		Blocking: blocking, Nits: nits,
		Evidence: &AgentEvidence{Prompt: prompt, Stdout: out, Stderr: stderr},
		Usage:    parseHarnessUsage(combined)}, nil
}

// selfReviewError reports whether gh refused a formal review because the App
// authored the PR ("Can not request changes/approve on your own pull request").
// Only this specific restriction triggers the comment fallback; every other
// error must propagate (§40 fail-closed).
func selfReviewError(out []byte) bool {
	return strings.Contains(string(out), "your own pull request")
}

// SubmitReview posts the verdict as a single formal review (the --body carries
// the summary). Posts as the kakashi-82 GitHub App so it can approve its own PRs.
// GitHub refuses a formal REQUEST_CHANGES/APPROVE review on a PR the App itself
// authored (§25 publish); that specific error falls back to a non-blocking
// COMMENT review, which GitHub permits and which still records the review so
// ListUnreviewedPRs stops re-firing. Any other failure is fatal.
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
	repo := in.Owner + "/" + in.Repo
	out, err := ghTokenRunner(ctx, tok, "pr", "review", fmt.Sprint(in.PrNum), "-R", repo,
		flag, "--body", body)
	if err != nil {
		if !selfReviewError(out) {
			return fmt.Errorf("gh pr review: %s: %w", strings.TrimSpace(string(out)), err)
		}
		note := fmt.Sprintf("%s\n\n> Posted as a comment: a formal `%s` review is refused on a PR the App authored.", body, flag)
		fout, ferr := ghTokenRunner(ctx, tok, "pr", "review", fmt.Sprint(in.PrNum), "-R", repo,
			"--comment", "--body", note)
		if ferr != nil {
			return fmt.Errorf("gh pr review (self-authored comment fallback): %s: %w", strings.TrimSpace(string(fout)), ferr)
		}
		activity.GetLogger(ctx).Info("submitted review as comment (self-authored pr)", "pr", in.PrNum, "verdict", verdict)
		return nil
	}
	activity.GetLogger(ctx).Info("submitted review", "pr", in.PrNum, "verdict", r.Verdict)
	return nil
}
