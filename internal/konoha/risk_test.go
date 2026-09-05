package konoha

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
)

// §20: a diff touching humanRequiredPaths or an issue carrying a
// humanRequiredLabels label escalates; unlisted paths/labels stay autonomous.
func TestDetectHumanEscalation(t *testing.T) {
	lc := LifecycleConfig{Autonomy: Autonomy{
		HumanRequiredPaths:  []string{".github/**", "infra/**", "migrations/**"},
		HumanRequiredLabels: []string{"security", "breaking-change"},
	}}

	// restricted path in the dev diff → escalate, reason names the trigger.
	d := DetectHumanEscalation(lc, []string{"pkg/a.go", "infra/db/migration.sql"}, nil)
	if !d.Escalate || len(d.Reasons) == 0 {
		t.Fatalf("restricted path must escalate with a reason, got %+v", d)
	}
	if d.Reasons[0] != "touches restricted path" {
		t.Fatalf("expected 'touches restricted path' reason, got %v", d.Reasons)
	}

	// escalation label on the issue → escalate.
	d = DetectHumanEscalation(lc, nil, []string{"bug", "security"})
	if !d.Escalate || len(d.Reasons) != 1 || d.Reasons[0] != "human-required label" {
		t.Fatalf("security label must escalate, got %+v", d)
	}

	// both → escalate with both reasons.
	d = DetectHumanEscalation(lc, []string{".github/workflows/ci.yml"}, []string{"security"})
	if !d.Escalate || len(d.Reasons) != 2 {
		t.Fatalf("both triggers must escalate with two reasons, got %+v", d)
	}

	// unlisted paths + no escalation label → autonomous.
	d = DetectHumanEscalation(lc, []string{"pkg/a.go", "cmd/main.go"}, []string{"enhancement"})
	if d.Escalate || len(d.Reasons) != 0 {
		t.Fatalf("ordinary change must stay autonomous, got %+v", d)
	}

	// empty config → everything autonomous (no restrictions configured).
	empty := LifecycleConfig{}
	if d := DetectHumanEscalation(empty, []string{"infra/x"}, []string{"security"}); d.Escalate {
		t.Fatalf("empty config must not escalate, got %+v", d)
	}

	// bare pattern (no **) matches only the exact path, not descendants.
	lc2 := LifecycleConfig{Autonomy: Autonomy{HumanRequiredPaths: []string{"migrations"}}}
	if d := DetectHumanEscalation(lc2, []string{"migrations"}, nil); !d.Escalate {
		t.Fatalf("bare pattern must match the exact path, got %+v", d)
	}
	if d := DetectHumanEscalation(lc2, []string{"migrations/007_x.sql"}, nil); d.Escalate {
		t.Fatalf("bare pattern must NOT match descendants (needs migrations/**), got %+v", d)
	}
}

// §20 boundary: `**` spans whole segments; a partially-matching path or a
// reversed segment order must NOT accidentally escalate.
func TestDetectHumanEscalationBoundaries(t *testing.T) {
	lc := LifecycleConfig{Autonomy: Autonomy{HumanRequiredPaths: []string{"infra/**"}}}

	// deep path matches; a sibling dir does not.
	if d := DetectHumanEscalation(lc, []string{"infra/k8s/prod.yaml"}, nil); !d.Escalate {
		t.Fatal("infra/** must match nested infra paths")
	}
	for _, p := range []string{"infrastructure/prod.yaml", "pkg/infra/prod.yaml", "infrafile.txt"} {
		if d := DetectHumanEscalation(lc, []string{p}, nil); d.Escalate {
			t.Fatalf("%q must NOT match infra/**: %+v", p, d)
		}
	}
}

// §20: escalated issues are never closed by the risk-based policy (only via a
// human decision); a clean change under risk-based IS autonomously closable.
func TestRiskBasedCloseBlocksEscalated(t *testing.T) {
	lc := LifecycleConfig{Autonomy: Autonomy{
		HumanRequiredPaths:  []string{"infra/**"},
		HumanRequiredLabels: []string{"security"},
	}}
	gatePasses := CompletionCheck{
		ImplementationExists: true, PREnabled: true, LatestCommitReviewed: true,
		ReviewApproved: true, TestsPass: true, CompletionAgentComplete: true,
		NoBlockingOpenQuestions: true, NoPolicyRestriction: true, RequiredChildrenComplete: true,
	}

	// restricted path → escalate, never Close.
	d := EvaluateClosePolicy(lc, gatePasses, []string{"infra/db/x.sql"}, nil)
	if d.Close || !d.Escalation {
		t.Fatalf("risk-based close must escalate restricted-path changes, got %+v", d)
	}
	// escalation label → escalate, never Close.
	d = EvaluateClosePolicy(lc, gatePasses, nil, []string{"security"})
	if d.Close || !d.Escalation {
		t.Fatalf("risk-based close must escalate labeled issues, got %+v", d)
	}
	// ordinary change under risk-based → Close (no human gate, §19).
	d = EvaluateClosePolicy(lc, gatePasses, []string{"pkg/a.go"}, []string{"enhancement"})
	if !d.Close || d.Escalation {
		t.Fatalf("risk-based close of an ordinary change must close, got %+v", d)
	}
}

// §23/§31: FetchIssueLabels reads label names via the ghRunner seam (no
// mutation), returning the names for the risk gate.
func TestFetchIssueLabels(t *testing.T) {
	prev := ghRunner
	ghRunner = func(_ context.Context, args ...string) ([]byte, error) {
		if len(args) >= 2 && args[1] == "repos/o/r/issues/7" {
			return json.Marshal(ghItem{
				Number: 7,
				Labels: []ghLabel{{Name: "bug"}, {Name: "security"}},
			})
		}
		return nil, fmt.Errorf("unexpected gh call: %v", args)
	}
	defer func() { ghRunner = prev }()

	labels, err := FetchIssueLabels(context.Background(), IssueInput{Owner: "o", Repo: "r", IssueNum: 7})
	if err != nil {
		t.Fatal(err)
	}
	if len(labels) != 2 || labels[0] != "bug" || labels[1] != "security" {
		t.Fatalf("labels = %v", labels)
	}
}

// A diff's ChangedFiles feed the gate: the workflow passes DevelopResult
// ChangedFiles (repo-relative, §26) straight into DetectHumanEscalation.
func TestChangedFilesDriveEscalation(t *testing.T) {
	lc := LifecycleConfig{Autonomy: Autonomy{HumanRequiredPaths: []string{".github/**"}}}
	res := DevelopResult{ChangedFiles: []string{"pkg/worker.go", ".github/workflows/ci.yml"}}
	if d := DetectHumanEscalation(lc, res.ChangedFiles, nil); !d.Escalate {
		t.Fatalf("developer diff touching .github must escalate; changed=%v", res.ChangedFiles)
	}
}
