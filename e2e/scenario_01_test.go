//go:build e2e

package e2e

import (
	"context"
	"testing"
	"time"
)

// Scenario 01 — SMALL autonomous happy path (#47): a SMALL issue runs
// dev → publish → APPROVE → CLOSED, exactly one PR created, issue closed.
func TestScenario01SmallHappyPath(t *testing.T) {
	// Pre-existing GitHub state: one open issue #1 (the sweep/Analyst input).
	seedState(t, `{"issues":[{"number":1,"state":"open","title":"t","body":"b",
	  "labels":["enhancement"],"isPr":false}],"labels":["enhancement"],"next":2}`)

	// Canned agent outputs.
	setAgentFixture("analyst", `{"summary":"s","spec":"generated-temp-spec",
	  "requirements":["r"],"acceptanceCriteria":["Given x When y Then z"]}`)
	setAgentFixture("estimator", `{"complexity":3,"confidence":"high","rationale":"small"}`)
	setAgentFixture("developer", `{"summary":"the change","concerns":[]}`)
	setAgentFixture("reviewer", "VERDICT:APPROVE\nBLOCKING:\n- none\nNITS:\n- none\nCONCLUDE: ok\n")

	run, err := startIssueCycle(1, nil)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	if err := run.Get(ctx, nil); err != nil {
		t.Fatalf("workflow error: %v", err)
	}

	st := readState(t)
	var issue, pr *fakeGhItem
	for i := range st.Issues {
		if st.Issues[i].Number == 1 && !st.Issues[i].IsPR {
			issue = &st.Issues[i]
		}
		if st.Issues[i].IsPR {
			pr = &st.Issues[i]
		}
	}
	if issue == nil {
		t.Fatal("issue #1 not found in fake store")
	}
	if issue.State != "closed" {
		t.Fatalf("issue #1 not closed (state=%q)", issue.State)
	}
	if pr == nil {
		t.Fatal("expected exactly one PR created")
	}
	if pr.State != "open" {
		t.Fatalf("PR unexpectedly %q", pr.State)
	}
}
