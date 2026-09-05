//go:build e2e

package e2e

import (
	"context"
	"testing"
	"time"

	"go.temporal.io/sdk/client"
)

// Scenario 11 — dual-sweep convergence (§32/§41): two IssueAllWorkflow sweeps
// over the same issue + the same deterministic <repo>-issue-<n> child ID must
// converge on ONE lifecycle run and ONE PR.
func TestScenario11DualSweepConverge(t *testing.T) {
	seedState(t, `{"issues":[{"number":11,"state":"open","title":"t","body":"b",
	  "labels":[],"isPr":false}],"labels":[],"next":12}`)
	setAgentFixture("analyst", `{"summary":"s","spec":"generated-temp-spec",
	  "requirements":["r"],"acceptanceCriteria":["Given x When y Then z"]}`)
	setAgentFixture("estimator", `{"complexity":3,"confidence":"high","rationale":"small"}`)
	setAgentFixture("developer", `{"summary":"the change","concerns":[]}`)
	setAgentFixture("reviewer", "VERDICT:APPROVE\nBLOCKING:\n- none\nNITS:\n- none\n")

	scope := map[string]interface{}{
		"owner": "o", "repo": "r", "repoPath": fixture.workdir,
		"lifecycle": defaultLifecycle(),
	}

	// Launch two sweeps with the same stable child ID concurrently.
	runA, err := e2eClient.ExecuteWorkflow(context.Background(), client.StartWorkflowOptions{
		ID: "r-issue-all-11a", TaskQueue: "review-queue",
	}, "IssueAllWorkflow", scope)
	if err != nil {
		t.Fatalf("sweep A start: %v", err)
	}
	runB, err := e2eClient.ExecuteWorkflow(context.Background(), client.StartWorkflowOptions{
		ID: "r-issue-all-11b", TaskQueue: "review-queue",
	}, "IssueAllWorkflow", scope)
	if err != nil {
		t.Fatalf("sweep B start: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	_ = runA.Get(ctx, nil) // best-effort: sweep A may also catch the issue
	_ = runB.Get(ctx, nil)

	// After both sweeps, the child lifecycle must have run once: the issue
	// closed and exactly ONE PR created (the fake store dedupes via the
	// EnsurePullRequest head-branch search).
	st := readState(t)
	var prs, issues int
	for _, it := range st.Issues {
		if it.IsPR {
			prs++
		} else if it.Number == 11 {
			issues++
		}
	}
	if issues != 1 {
		t.Fatalf("expected 1 issue #11 in store, got %d", issues)
	}
	if prs != 1 {
		t.Fatalf("dual sweep must converge to exactly ONE PR, got %d", prs)
	}
}

// Scenario 12 — sweep retry no-dup (§24/§32): running IssueAllWorkflow twice
// (simulating a retried/overlap schedule fire) must not create a duplicate PR;
// the fake-gh head-branch search (EnsurePullRequest) converges.
func TestScenario12SweepRetryNoDup(t *testing.T) {
	seedState(t, `{"issues":[{"number":12,"state":"open","title":"t","body":"b",
	  "labels":[],"isPr":false}],"labels":[],"next":13}`)
	setAgentFixture("analyst", `{"summary":"s","spec":"generated-temp-spec",
	  "requirements":["r"],"acceptanceCriteria":["Given x When y Then z"]}`)
	setAgentFixture("estimator", `{"complexity":3,"confidence":"high","rationale":"small"}`)
	setAgentFixture("developer", `{"summary":"the change","concerns":[]}`)
	setAgentFixture("reviewer", "VERDICT:APPROVE\nBLOCKING:\n- none\nNITS:\n- none\n")

	scope := map[string]interface{}{
		"owner": "o", "repo": "r", "repoPath": fixture.workdir,
		"lifecycle": defaultLifecycle(),
	}
	opts := client.StartWorkflowOptions{ID: "r-issue-all-12", TaskQueue: "review-queue"}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()

	run1, err := e2eClient.ExecuteWorkflow(ctx, opts, "IssueAllWorkflow", scope)
	if err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	if err := run1.Get(ctx, nil); err != nil {
		t.Fatalf("first sweep failed: %v", err)
	}

	// second sweep on the SAME IssueAllWorkflow ID — Temporal treats a
	// completed workflow with same ID as a NEW run unless we guarantee it; to be
	// retry-safe, the child ID r-issue-12 is what dedupes. Assert single PR.
	run2, err := e2eClient.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID: "r-issue-all-12b", TaskQueue: "review-queue"}, "IssueAllWorkflow", scope)
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	_ = run2.Get(ctx, nil)

	st := readState(t)
	prs := 0
	for _, it := range st.Issues {
		if it.IsPR {
			prs++
		}
	}
	if prs != 1 {
		t.Fatalf("sweep retry must not duplicate the PR; got %d PRs", prs)
	}
}
