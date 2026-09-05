package konoha

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// Child-issue orchestration (§8/§9/§10/§43): a large issue's validated
// SplitResult becomes deterministic child issues with parent+child markers,
// scheduled as a dependency-respecting DAG within a concurrency cap. The
// workflow keeps the parent open until every child completes (§14/§40).

// ChildIssue is the §9 deterministic identity linking a validated SplitTicket
// to its GitHub issue: the ticket key + parent issue number + the GitHub issue
// number created/found by EnsureChildren.
type ChildIssue struct {
	Key               string `json:"key"`
	ParentIssueNumber int    `json:"parentIssueNumber"`
	GitHubIssueNumber int    `json:"githubIssueNumber"`
	Complexity        int    `json:"complexity"`
	Title             string `json:"title"`
}

// childTicketBody is the child issue body: the ticket's structured
// requirements/acceptance criteria so the child's own cycle has a spec. The
// deterministic parent+child markers (§9) are prepended by childIssueBody.
func childTicketBody(t SplitTicket) string {
	var b strings.Builder
	fmt.Fprintf(&b, "**Complexity:** %d\n", t.Complexity)
	if len(t.Dependencies) > 0 {
		fmt.Fprintf(&b, "**Dependencies:** %s\n", strings.Join(t.Dependencies, ", "))
	}
	b.WriteString("\n## Requirements\n")
	for _, r := range t.Requirements {
		fmt.Fprintf(&b, "- %s\n", r)
	}
	b.WriteString("\n## Acceptance criteria\n")
	for _, c := range t.AcceptanceCriteria {
		fmt.Fprintf(&b, "- %s\n", c)
	}
	return b.String()
}

// scheduleChildBatches returns the child ticket keys in dependency-respecting
// batch order (§10). Dependencies of each ticket must be in an earlier batch;
// every batch is at most maxConcurrent wide (§43). Order within a batch is
// deterministic (sorted keys). A missing dependency or a dependency cycle fails
// closed — the workflow must not blindly fan out an invalid DAG.
func scheduleChildBatches(tickets []SplitTicket, maxConcurrent int) ([][]string, error) {
	if maxConcurrent < 1 {
		maxConcurrent = 1
	}
	byKey := make(map[string]SplitTicket, len(tickets))
	for _, t := range tickets {
		if _, dup := byKey[t.Key]; dup {
			return nil, fmt.Errorf("duplicate child key %q", t.Key)
		}
		byKey[t.Key] = t
	}
	for _, t := range tickets {
		for _, d := range t.Dependencies {
			if _, ok := byKey[d]; !ok {
				return nil, fmt.Errorf("child %q depends on unknown key %q", t.Key, d)
			}
		}
	}

	remaining := make(map[string]bool, len(byKey))
	for k := range byKey {
		remaining[k] = true
	}
	var levels [][]string
	for len(remaining) > 0 {
		var level []string
		for k := range remaining {
			ready := true
			for _, d := range byKey[k].Dependencies {
				if remaining[d] {
					ready = false
					break
				}
			}
			if ready {
				level = append(level, k)
			}
		}
		if len(level) == 0 {
			return nil, fmt.Errorf("dependency cycle detected among children (§10); no progress possible")
		}
		sort.Strings(level)
		for _, k := range level {
			delete(remaining, k)
		}
		levels = append(levels, level)
	}

	// Break each independent level into cap-sized batches so the workflow never
	// has more than maxConcurrent children in flight at once (§43).
	var out [][]string
	for _, l := range levels {
		for i := 0; i < len(l); i += maxConcurrent {
			j := i + maxConcurrent
			if j > len(l) {
				j = len(l)
			}
			out = append(out, l[i:j])
		}
	}
	return out, nil
}

// ChildCreateRequest carries the validated split + parent context for
// idempotent child-issue creation (§8/§9/§24).
type ChildCreateRequest struct {
	Owner     string        `json:"owner"`
	Repo      string        `json:"repo"`
	ParentNum int           `json:"parent"`
	Tickets   []SplitTicket `json:"tickets"`
	DryRun    bool          `json:"dryRun,omitempty"`
}

// EnsureChildren idempotently creates (or finds via markers) one GitHub issue
// per validated SplitTicket, each carrying the deterministic
// `<!-- konoha-parent: owner/repo#<n> -->` + `<!-- konoha-child: <key> -->`
// markers (§9) and the konoha:managed + konoha:child labels (§21). It reuses
// EnsureIssue's search-before-create so retries/duplicate sweeps never duplicate
// (§24). Returns the ChildIssue identities (§9) for workflow fan-out.
func EnsureChildren(ctx context.Context, in ChildCreateRequest) ([]ChildIssue, error) {
	out := make([]ChildIssue, 0, len(in.Tickets))
	for _, t := range in.Tickets {
		p := ParentRef{Owner: in.Owner, Repo: in.Repo, Num: in.ParentNum, Key: t.Key}
		num, err := EnsureIssue(ctx, IssueRequest{
			Owner: in.Owner, Repo: in.Repo,
			Title:  t.Title,
			Body:   childIssueBody(p, childTicketBody(t)),
			Labels: []string{labelManaged, labelChild},
			Parent: &p,
			DryRun: in.DryRun,
		})
		if err != nil {
			return nil, fmt.Errorf("create child %q: %w", t.Key, err)
		}
		out = append(out, ChildIssue{
			Key: t.Key, ParentIssueNumber: in.ParentNum, GitHubIssueNumber: num,
			Complexity: t.Complexity, Title: t.Title,
		})
	}
	return out, nil
}
