//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// Scenario 02 — scripted repair loop: reviewer returns REQUEST_CHANGES on the
// first review (line 1) then APPROVE on the second (line 2). The workflow must
// run exactly one repair round and close.
func TestScenario02RepairLoop(t *testing.T) {
	seedState(t, `{"issues":[{"number":2,"state":"open","title":"t","body":"b",
	  "labels":["bug"],"isPr":false}],"labels":["bug"],"next":3}`)
	setAgentFixture("analyst", `{"summary":"s","spec":"generated-temp-spec",
	  "requirements":["r"],"acceptanceCriteria":["Given x When y Then z"]}`)
	setAgentFixture("estimator", `{"complexity":3,"confidence":"high","rationale":"small"}`)
	setAgentFixture("developer", `{"summary":"the change","concerns":[]}`)
	setAgentFixture("repair", `{"summary":"fixed auth","concerns":[]}`)
	// reviewer.txt: line 1 REQUEST_CHANGES (first review), line 2 APPROVE.
	setAgentFixture("reviewer", "VERDICT:REQUEST_CHANGES\nBLOCKING:\n- auth gap\nNITS:\n- none\n\nVERDICT:APPROVE\nBLOCKING:\n- none\nNITS:\n- none\n")

	run, err := startIssueCycle(2, nil)
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
		if st.Issues[i].Number == 2 && !st.Issues[i].IsPR {
			issue = &st.Issues[i]
		}
		if st.Issues[i].IsPR {
			pr = &st.Issues[i]
		}
	}
	if issue == nil || issue.State != "closed" {
		t.Fatalf("issue #2 must be closed after repair+approve (got %+v)", issue)
	}
	if pr == nil {
		t.Fatal("one PR expected")
	}
	_ = fmt.Sprint // keep fmt for potential future asserts
}

// Scenario 03 — reviewer always REQUEST_CHANGES → §16 cap reached → workflow
// must end BLOCKED and NEVER close the issue.
func TestScenario03RepairCapBlocked(t *testing.T) {
	seedState(t, `{"issues":[{"number":3,"state":"open","title":"t","body":"b",
	  "labels":["bug"],"isPr":false}],"labels":["bug"],"next":4}`)
	setAgentFixture("analyst", `{"summary":"s","spec":"generated-temp-spec",
	  "requirements":["r"],"acceptanceCriteria":["Given x When y Then z"]}`)
	setAgentFixture("estimator", `{"complexity":3,"confidence":"high","rationale":"small"}`)
	setAgentFixture("developer", `{"summary":"the change","concerns":[]}`)
	setAgentFixture("repair", `{"summary":"try again","concerns":[]}`)
	setAgentFixture("reviewer", "VERDICT:REQUEST_CHANGES\nBLOCKING:\n- nope\nNITS:\n- none\n")

	run, err := startIssueCycle(3, nil)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	if err := run.Get(ctx, nil); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	st := readState(t)
	for _, it := range st.Issues {
		if it.Number == 3 && !it.IsPR && it.State == "closed" {
			t.Fatal("issue #3 must NOT close when the cap is reached (§40 no silent success)")
		}
	}
}

// Scenario 04 — dev writes under infra/** (FAKE_DEV_PATH) + risk-based lifecycle
// with humanRequiredPaths=infra/** → publish suppressed, issue NOT published as
// PR / no PR, workflow ends HUMAN_REQUIRED (no GitHub mutation; issue stays
// open, no PR created).
func TestScenario04RiskGate(t *testing.T) {
	seedState(t, `{"issues":[{"number":4,"state":"open","title":"t","body":"b",
	  "labels":["enhancement"],"isPr":false}],"labels":["enhancement"],"next":5}`)
	setAgentFixture("analyst", `{"summary":"s","spec":"generated-temp-spec",
	  "requirements":["r"],"acceptanceCriteria":["Given x When y Then z"]}`)
	setAgentFixture("estimator", `{"complexity":3,"confidence":"high","rationale":"small"}`)
	setAgentFixture("developer", `{"summary":"touch infra","concerns":[]}`)
	// devpath forces the fake developer to write under infra/** (§20 risk gate).
	setAgentFixture("devpath", "infra/deploy.yaml")

	lc := map[string]interface{}{
		"mode": "supervised-autonomous", "developmentMaxComplexity": 4,
		"autonomy": map[string]interface{}{
			"closePolicy":        "risk-based",
			"humanRequiredPaths": []string{"infra/**"},
		},
	}
	run, err := startIssueCycle(4, lc)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	if err := run.Get(ctx, nil); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	st := readState(t)
	for _, it := range st.Issues {
		if it.Number == 4 && !it.IsPR && it.State == "closed" {
			t.Fatal("risk-gated issue must not close")
		}
		if it.IsPR {
			t.Fatal("§20: publish must be suppressed for restricted paths (0 PRs)")
		}
	}
}
