package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

// ghRunner executes the gh CLI. Package-level so tests can simulate GitHub
// and prove the retry-no-duplicate property without network access.
// ponytail: idempotency matches markers by scanning repo issues instead of
// GitHub /search/issues (which strips HTML comments); switch when repos grow.
var ghRunner = func(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "gh", args...)
	return cmd.CombinedOutput()
}

// Deterministic markers (§24). Embedded as HTML comments so they survive
// rendering; matching is substring-on-body, so markers must stay byte-exact.
func parentMarker(owner, repo string, num int) string {
	return fmt.Sprintf("<!-- konoha-parent: %s/%s#%d -->", owner, repo, num)
}

func childMarker(key string) string {
	return fmt.Sprintf("<!-- konoha-child: %s -->", key)
}

func childIssueBody(p ParentRef, body string) string {
	return parentMarker(p.Owner, p.Repo, p.Num) + "\n" +
		childMarker(p.Key) + "\n\n" + strings.TrimSpace(body) + "\n"
}

func parseMarker(body, open string) (string, bool) {
	i := strings.Index(body, open)
	if i < 0 {
		return "", false
	}
	j := strings.Index(body[i+len(open):], " -->")
	if j < 0 {
		return "", false
	}
	return strings.TrimSpace(body[i+len(open) : i+len(open)+j]), true
}

func parseParentMarker(body string) (ParentRef, bool) {
	v, ok := parseMarker(body, "<!-- konoha-parent: ")
	if !ok {
		return ParentRef{}, false
	}
	slash, hash := strings.LastIndex(v, "/"), strings.LastIndex(v, "#")
	if slash <= 0 || hash <= slash {
		return ParentRef{}, false
	}
	var num int
	if _, err := fmt.Sscanf(v[hash+1:], "%d", &num); err != nil || num <= 0 {
		return ParentRef{}, false
	}
	return ParentRef{Owner: v[:slash], Repo: v[slash+1 : hash], Num: num}, true
}

func parseChildMarker(body string) (string, bool) {
	return parseMarker(body, "<!-- konoha-child: ")
}

// ghItem is the subset of a GitHub issue/PR we need to match markers and
// reconcile state. PullRequest != null means the /issues item is a PR.
type ghItem struct {
	Number      int             `json:"number"`
	State       string          `json:"state"`
	Body        string          `json:"body"`
	Labels      []ghLabel       `json:"labels"`
	PullRequest json.RawMessage `json:"pull_request"`
}

type ghLabel struct {
	Name string `json:"name"`
}

func isPR(it ghItem) bool {
	return len(it.PullRequest) > 0 && string(it.PullRequest) != "null"
}

// listIssues returns every issue+PR in the repo, oldest first (§24 scan).
func listIssues(ctx context.Context, owner, repo string) ([]ghItem, error) {
	target := fmt.Sprintf("repos/%s/%s/issues?state=all&per_page=100&sort=created&direction=asc", owner, repo)
	out, err := ghRunner(ctx, "api", target, "--paginate")
	if err != nil {
		return nil, fmt.Errorf("list %s: %s: %w", target, strings.TrimSpace(string(out)), err)
	}
	var items []ghItem
	if err := json.Unmarshal(out, &items); err != nil {
		return nil, fmt.Errorf("parse %s: %w", target, err)
	}
	return items, nil
}

func getIssue(ctx context.Context, owner, repo string, num int) (ghItem, error) {
	target := fmt.Sprintf("repos/%s/%s/issues/%d", owner, repo, num)
	out, err := ghRunner(ctx, "api", target)
	if err != nil {
		return ghItem{}, fmt.Errorf("get %s: %s: %w", target, strings.TrimSpace(string(out)), err)
	}
	var it ghItem
	if err := json.Unmarshal(out, &it); err != nil {
		return ghItem{}, fmt.Errorf("parse %s: %w", target, err)
	}
	return it, nil
}

// FetchIssueLabels reads the label names on one issue (read-only, §23). Used by
// the §20 risk gate: the workflow feeds the result to DetectHumanEscalation so
// a humanRequiredLabels tag (e.g. "security") escalates before publish/close.
func FetchIssueLabels(ctx context.Context, in IssueInput) ([]string, error) {
	it, err := getIssue(ctx, in.Owner, in.Repo, in.IssueNum)
	if err != nil {
		return nil, err
	}
	var labels []string
	for _, l := range it.Labels {
		labels = append(labels, l.Name)
	}
	return labels, nil
}

func parseIssueNumber(s string) (int, error) {
	s = strings.TrimSpace(s)
	if i := strings.LastIndex(s, "/"); i >= 0 {
		s = s[i+1:]
	}
	var n int
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil || n <= 0 {
		return 0, fmt.Errorf("parse issue/PR number from %q", s)
	}
	return n, nil
}

func findChildIssue(items []ghItem, p ParentRef) int {
	pm, cm := parentMarker(p.Owner, p.Repo, p.Num), childMarker(p.Key)
	for _, it := range items {
		if isPR(it) {
			continue
		}
		if strings.Contains(it.Body, pm) && strings.Contains(it.Body, cm) {
			return it.Number
		}
	}
	return 0
}

// ParentRef identifies the parent issue and this child's deterministic key.
type ParentRef struct {
	Owner string `json:"owner"`
	Repo  string `json:"repo"`
	Num   int    `json:"num"`
	Key   string `json:"key"`
}

// IssueRequest creates (or finds) a child issue carrying parent+child markers.
type IssueRequest struct {
	Owner  string     `json:"owner"`
	Repo   string     `json:"repo"`
	Title  string     `json:"title"`
	Body   string     `json:"body"`
	Labels []string   `json:"labels"`
	Parent *ParentRef `json:"parent,omitempty"`
	DryRun bool       `json:"dryRun,omitempty"`
}

// EnsureIssue is idempotent under retry: it scans for an existing issue with
// the parent+child markers before creating. Returns the issue number.
func EnsureIssue(ctx context.Context, in IssueRequest) (int, error) {
	if in.DryRun {
		return 0, nil // §42: shadow mode mutates nothing
	}
	if in.Parent == nil {
		return 0, fmt.Errorf("EnsureIssue: Parent marker required for idempotent create")
	}
	p := *in.Parent
	items, err := listIssues(ctx, in.Owner, in.Repo)
	if err != nil {
		return 0, err
	}
	if n := findChildIssue(items, p); n != 0 {
		return n, nil
	}
	args := []string{"issue", "create", "-R", in.Owner + "/" + in.Repo,
		"--title", in.Title, "--body", childIssueBody(p, in.Body)}
	for _, l := range in.Labels {
		args = append(args, "--label", l)
	}
	out, err := ghRunner(ctx, args...)
	if err != nil {
		return 0, fmt.Errorf("gh issue create: %s: %w", strings.TrimSpace(string(out)), err)
	}
	n, err := parseIssueNumber(string(out))
	if err != nil {
		return 0, err
	}
	return n, nil
}

// StateRequest targets one issue's open/closed state.
type StateRequest struct {
	Owner  string `json:"owner"`
	Repo   string `json:"repo"`
	Num    int    `json:"num"`
	State  string `json:"state"`
	DryRun bool   `json:"dryRun,omitempty"`
}

// EnsureIssueState reconciles external changes (§31): read current state,
// no-op if it already matches, otherwise PATCH. Retries re-read and no-op.
func EnsureIssueState(ctx context.Context, in StateRequest) error {
	if in.DryRun {
		return nil // §42: shadow mode mutates nothing
	}
	if in.State != "open" && in.State != "closed" {
		return fmt.Errorf("EnsureIssueState: invalid state %q", in.State)
	}
	it, err := getIssue(ctx, in.Owner, in.Repo, in.Num)
	if err != nil {
		return err
	}
	if it.State == in.State {
		return nil
	}
	if _, err := ghRunner(ctx, "api", "--method", "PATCH",
		fmt.Sprintf("repos/%s/%s/issues/%d", in.Owner, in.Repo, in.Num),
		"-f", "state="+in.State); err != nil {
		return fmt.Errorf("EnsureIssueState: patch %s/%s#%d: %w", in.Owner, in.Repo, in.Num, err)
	}
	return nil
}

// LabelsRequest targets one issue's labels.
type LabelsRequest struct {
	Owner  string   `json:"owner"`
	Repo   string   `json:"repo"`
	Num    int      `json:"num"`
	Labels []string `json:"labels"`
	DryRun bool     `json:"dryRun,omitempty"`
}

// EnsureIssueLabels reconciles labels: read current, add only the missing
// ones. Retries re-read and no-op, so a lost response never dupes.
func EnsureIssueLabels(ctx context.Context, in LabelsRequest) error {
	if in.DryRun {
		return nil // §42: shadow mode mutates nothing
	}
	it, err := getIssue(ctx, in.Owner, in.Repo, in.Num)
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for _, l := range it.Labels {
		have[l.Name] = true
	}
	var missing []string
	for _, want := range in.Labels {
		if !have[want] {
			missing = append(missing, want)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	args := []string{"issue", "edit", fmt.Sprint(in.Num), "-R", in.Owner + "/" + in.Repo}
	for _, l := range missing {
		args = append(args, "--add-label", l)
	}
	if _, err := ghRunner(ctx, args...); err != nil {
		return fmt.Errorf("EnsureIssueLabels: %s/%s#%d: %w", in.Owner, in.Repo, in.Num, err)
	}
	return nil
}
