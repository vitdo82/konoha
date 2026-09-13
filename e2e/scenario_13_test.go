//go:build e2e

package e2e

import (
	"context"
	"testing"
	"time"
)

// Scenario 13 — §49 CI gate: the seeded CI check fails, so the PR must NOT be
// merged and the issue must NOT close; the workflow ends in escalation (a clean
// nil return, but no close/merge mutation).
func TestScenario13CIFailureBlocksMerge(t *testing.T) {
	seedState(t, `{"issues":[{"number":13,"state":"open","title":"t","body":"b",
	  "labels":["enhancement"],"isPr":false}],"labels":["enhancement"],"next":14,
	  "ciBucket":"fail"}`)
	setAgentFixture("analyst", `{"summary":"s","spec":"generated-temp-spec",
	  "requirements":["r"],"acceptanceCriteria":["Given x When y Then z"]}`)
	setAgentFixture("estimator", `{"complexity":3,"confidence":"high","rationale":"small"}`)
	setAgentFixture("developer", `{"summary":"the change","concerns":[]}`)
	setAgentFixture("reviewer", "VERDICT:APPROVE\nBLOCKING:\n- none\nNITS:\n- none\nCONCLUDE: ok\n")

	run, err := startIssueCycle(13, nil) // default lifecycle: requireCI=true
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
		if it.Number == 13 && !it.IsPR && it.State == "closed" {
			t.Fatal("§49: a failing CI gate must never close the issue")
		}
		if it.IsPR && it.State == "merged" {
			t.Fatal("§49: a failing CI gate must never merge the PR")
		}
	}
}

// Scenario 14 — §49 requireCI:false opts a repo out of the CI gate: the same
// failing checks are ignored and the PR merges (explicit config, not a default).
func TestScenario14CIFailureIgnoredWhenNotRequired(t *testing.T) {
	seedState(t, `{"issues":[{"number":14,"state":"open","title":"t","body":"b",
	  "labels":["enhancement"],"isPr":false}],"labels":["enhancement"],"next":15,
	  "ciBucket":"fail"}`)
	setAgentFixture("analyst", `{"summary":"s","spec":"generated-temp-spec",
	  "requirements":["r"],"acceptanceCriteria":["Given x When y Then z"]}`)
	setAgentFixture("estimator", `{"complexity":3,"confidence":"high","rationale":"small"}`)
	setAgentFixture("developer", `{"summary":"the change","concerns":[]}`)
	setAgentFixture("reviewer", "VERDICT:APPROVE\nBLOCKING:\n- none\nNITS:\n- none\nCONCLUDE: ok\n")

	lc := map[string]interface{}{
		"mode": "supervised-autonomous", "developmentMaxComplexity": 4,
		"autonomy": map[string]interface{}{"closePolicy": "risk-based"},
		"merge":    map[string]interface{}{"requireCI": false},
	}
	run, err := startIssueCycle(14, lc)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	if err := run.Get(ctx, nil); err != nil {
		t.Fatalf("workflow error: %v", err)
	}

	st := readState(t)
	var merged bool
	for _, it := range st.Issues {
		if it.Number == 14 && !it.IsPR && it.State != "closed" {
			t.Fatalf("issue #14 must close after merge, state=%q", it.State)
		}
		if it.IsPR && it.State == "merged" {
			merged = true
		}
	}
	if !merged {
		t.Fatal("requireCI=false must merge despite failing checks")
	}
}
