package konoha

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/mock"
)

// #94: a §26 gate that rewrites a tracked file is recorded as drift — Mutated
// true, a saved post-verification.patch — while the delivered commit SHA stays
// exactly what the agent committed. Covers both the structured-gate and the
// legacy shell-string path.
func TestValidationDriftDetected(t *testing.T) {
	mandatory := true
	cases := []struct {
		name string
		in   PublishInput
	}{
		{"structured-gate", PublishInput{Validation: []ValidationGate{
			{ID: "mutate", Argv: []string{"sh", "-c", "echo drifted >> seed.txt"}, Mandatory: &mandatory},
		}}},
		{"legacy-cmd", PublishInput{ValidationCmd: "echo drifted >> seed.txt"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := withEvidenceDir(t)
			repoPath, _ := initGitRepo(t)
			before := localHead(context.Background(), repoPath)

			c.in.RepoPath = repoPath
			c.in.CycleID = "ducklambda-issue-7"
			ok, skipped, _, drift, err := validateRepoWithDrift(c.in)
			if err != nil {
				t.Fatalf("validateRepoWithDrift: %v", err)
			}
			if !ok || skipped {
				t.Fatalf("a mutating gate exited 0: ok=%v skipped=%v", ok, skipped)
			}
			if !drift.Mutated {
				t.Fatal("a gate that rewrote a tracked file must be recorded as drift")
			}
			if !strings.Contains(drift.Patch, "seed.txt") {
				t.Fatalf("post-verification diff must mention the drifted file, got %q", drift.Patch)
			}

			// AC1: the post-verification patch lands in the evidence store.
			p := filepath.Join(root, "cycles", "ducklambda-issue-7", "verification", "post-verification.patch")
			b, err := os.ReadFile(p)
			if err != nil {
				t.Fatalf("post-verification.patch missing: %v", err)
			}
			if !strings.Contains(string(b), "seed.txt") {
				t.Fatalf("saved drift patch must contain the change, got %q", b)
			}

			// AC3: drift never changes the delivered commit SHA.
			if after := localHead(context.Background(), repoPath); after != before || after == "" {
				t.Fatalf("delivered commit SHA changed by drift: before=%q after=%q", before, after)
			}
		})
	}
}

// #94 AC2: a gate that touches nothing yields verificationMutated=false and no
// post-verification.patch.
func TestValidationDriftClean(t *testing.T) {
	root := withEvidenceDir(t)
	repoPath, _ := initGitRepo(t)
	mandatory := true

	_, _, _, drift, err := validateRepoWithDrift(PublishInput{
		RepoPath: repoPath, CycleID: "ducklambda-issue-7",
		Validation: []ValidationGate{{ID: "noop", Argv: []string{"true"}, Mandatory: &mandatory}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if drift.Mutated {
		t.Fatal("a clean gate must not be recorded as drift")
	}
	p := filepath.Join(root, "cycles", "ducklambda-issue-7", "verification", "post-verification.patch")
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("clean gate must not write post-verification.patch, stat err=%v", err)
	}

	// A skipped profile (no gates) must never flag drift, even when the checkout
	// is already dirty — the publish path reuses it against the main repo.
	dirty, _ := initGitRepo(t)
	writeTestFile(t, dirty, "seed.txt", "dirty local edit\n")
	_, skipped, _, drift, err := validateRepoWithDrift(PublishInput{
		RepoPath: dirty, CycleID: "ducklambda-issue-7",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !skipped || drift.Mutated {
		t.Fatalf("skipped profile must not report drift: skipped=%v mutated=%v", skipped, drift.Mutated)
	}
}

// #94 AC4: verification drift is evidence only — a cycle whose develop attempt
// drifted still closes autonomously; the drift is recorded in outcome.json.
func TestWorkflowVerificationDriftNonFatal(t *testing.T) {
	resetMetrics()
	root := withEvidenceDir(t)

	env := mockCycleEnv(t, EstimateResult{Complexity: 3, Confidence: "high"})
	// Register the drifting developer FIRST so it wins over mockSmallPath's.
	env.OnActivity(RunDeveloper, mock.Anything, mock.Anything).Return(DevelopResult{
		WorktreeDir: "/tmp/wt", CommitSHA: "devsha", ChangedFiles: []string{"pkg/worker.go"},
		Summary: "change", Validated: true, VerificationMutated: true,
	}, nil)
	h := mockSmallPath(env)
	env.RegisterActivity(WriteCycleEvidence) // real outcome writer
	runAndAssert(t, env)

	if got := terminalState(t, env); got != stateClosed {
		t.Fatalf("drift changed terminal state = %q, want CLOSED", got)
	}
	if h.closes != 1 {
		t.Fatalf("drift must not block the close (closes=%d)", h.closes)
	}
	matches, err := filepath.Glob(filepath.Join(root, "cycles", "*", evidenceOutcome))
	if err != nil || len(matches) != 1 {
		t.Fatalf("expected one outcome.json, got %v (err=%v)", matches, err)
	}
	b, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	var out CycleOutcome
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("outcome.json must decode: %v", err)
	}
	if !out.VerificationMutated {
		t.Fatalf("outcome.json must record verificationMutated=true, got %s", b)
	}
}
