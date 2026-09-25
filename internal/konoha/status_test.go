package konoha

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/testsuite"
)

// #89: the pure projection runs without a Temporal server: a snapshot maps to
// printable JSON and round-trips the five fields.
func TestStatusProjectionJSON(t *testing.T) {
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	st := StatusQuery{
		State:               stateDeveloping,
		CurrentStep:         "develop",
		AgentOutcome:        agentOutcomeSuccess,
		VerificationOutcome: verificationSuccess,
		Conditions: []Condition{
			{Type: conditionAgent, Status: "SUCCESS", At: at},
			{Type: conditionVerification, Status: "SUCCESS", At: at},
			{Type: conditionTerminal, Status: stateClosed, At: at},
		},
	}
	out, err := statusJSON(st)
	if err != nil {
		t.Fatalf("statusJSON: %v", err)
	}
	var got StatusQuery
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("json must round-trip: %v", err)
	}
	if got.State != stateDeveloping || got.CurrentStep != "develop" {
		t.Fatalf("state/step = %q/%q, want DEVELOPING/develop", got.State, got.CurrentStep)
	}
	if got.AgentOutcome != agentOutcomeSuccess || got.VerificationOutcome != verificationSuccess {
		t.Fatalf("outcomes = %q/%q, want SUCCESS/SUCCESS", got.AgentOutcome, got.VerificationOutcome)
	}
	if len(got.Conditions) != 3 || !got.Conditions[0].At.Equal(at) {
		t.Fatalf("conditions = %+v, want 3 with the supplied timestamp", got.Conditions)
	}
}

// #89: an empty snapshot still projects a JSON `[]` for conditions, never null.
func TestStatusProjectionEmptyConditions(t *testing.T) {
	out, err := statusJSON(newStatusTracker(stateAnalyzing).snapshot())
	if err != nil {
		t.Fatalf("statusJSON: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		t.Fatal(err)
	}
	if string(raw["conditions"]) != "[]" {
		t.Fatalf("conditions = %s, want []", raw["conditions"])
	}
}

// #89: the tracker appends one condition per step event and records outcomes,
// independent of any Temporal server.
func TestStatusTrackerRecordsSteps(t *testing.T) {
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	tr := newStatusTracker(stateDiscovered)
	tr.setState(stateDeveloping)
	tr.recordAgent(agentOutcomeSuccess, "", "", at)
	tr.recordVerification(verificationFailed, at)

	s := tr.snapshot()
	if s.State != stateDeveloping || s.CurrentStep != "develop" {
		t.Fatalf("snapshot = %+v, want DEVELOPING/develop", s)
	}
	if len(s.Conditions) != 2 || s.Conditions[1].Status != string(verificationFailed) {
		t.Fatalf("conditions = %+v, want agent then FAILED verification", s.Conditions)
	}
	tr.recordTerminal(stateHumanRequired, reasonCIFailed, at)
	s = tr.snapshot()
	if s.State != stateHumanRequired || len(s.Conditions) != 3 || s.Conditions[2].Reason != reasonCIFailed {
		t.Fatalf("terminal snapshot = %+v", s)
	}
	// The snapshot is a copy: mutating it must not mutate the tracker.
	s.Conditions[0].Status = "TAMPERED"
	if tr.snapshot().Conditions[0].Status != "SUCCESS" {
		t.Fatal("snapshot must not alias tracker state")
	}
}

// #89: the coarse step mapping is deterministic.
func TestStatusStepMapping(t *testing.T) {
	want := map[string]string{
		stateAnalyzing:        "analyze",
		stateDeveloping:       "develop",
		stateChangesRequested: "repair",
		stateMerging:          "merge",
		statePendingApproval:  "approval",
		stateClosed:           "close",
	}
	for state, step := range want {
		if got := statusStep(state); got != step {
			t.Fatalf("statusStep(%q) = %q, want %q", state, got, step)
		}
	}
}

// #89/AC1: the "status" query is asserted at each major transition of the SMALL
// path (captured deterministically inside the mocked activities, i.e. right
// after each transition) and at the terminal; the old "state" handler is
// unchanged (backward compatible).
func TestWorkflowStatusQueryPerTransition(t *testing.T) {
	resetMetrics()
	env := mockCycleEnv(t, EstimateResult{Complexity: 3, Confidence: "high"})

	query := func(label string) StatusQuery {
		t.Helper()
		q, err := env.QueryWorkflow("status")
		if err != nil {
			t.Fatalf("query status at %s: %v", label, err)
		}
		var s StatusQuery
		if err := q.Get(&s); err != nil {
			t.Fatalf("decode status at %s: %v", label, err)
		}
		return s
	}
	assertAt := func(label, state, step string, conds int) {
		t.Helper()
		s := query(label)
		if s.State != state || s.CurrentStep != step {
			t.Fatalf("%s status = %q/%q, want %q/%q", label, s.State, s.CurrentStep, state, step)
		}
		if len(s.Conditions) != conds {
			t.Fatalf("%s conditions = %d, want %d (%+v)", label, len(s.Conditions), conds, s.Conditions)
		}
	}

	env.OnActivity(FetchIssueLabels, mock.Anything, mock.Anything).Return([]string{"enhancement"}, nil)
	env.OnActivity(RunDeveloper, mock.Anything, mock.Anything).Return(func(context.Context, DevelopRequest) (DevelopResult, error) {
		assertAt("RunDeveloper", stateDeveloping, "develop", 0)
		return DevelopResult{WorktreeDir: "/tmp/wt", CommitSHA: "devsha", ChangedFiles: []string{"pkg/worker.go"}, Validated: true}, nil
	})
	env.OnActivity(PublishDeveloperChange, mock.Anything, mock.Anything).Return(PublishResult{Branch: "b", PRNumber: 101, Validated: true}, nil)
	env.OnActivity(FetchPRHead, mock.Anything, mock.Anything).Return("headsha", nil)
	env.OnActivity(FetchPRDiff, mock.Anything, mock.Anything).Return("+1 line", nil)
	env.OnActivity(RunReviewer, mock.Anything, mock.Anything).Return(func(context.Context, ReviewRequest) (ReviewResult, error) {
		assertAt("RunReviewer", stateReviewing, "review", 2)
		return ReviewResult{Verdict: "APPROVE", CommitSHA: "headsha", PRNumber: 101}, nil
	})
	env.OnActivity(WaitForCI, mock.Anything, mock.Anything).Return(func(context.Context, CIRequest) (CIStatus, error) {
		assertAt("WaitForCI", stateMerging, "ci", 2)
		return CIStatus{Green: true}, nil
	})
	env.OnActivity(MergePullRequest, mock.Anything, mock.Anything).Return(func(context.Context, MergeRequest) (MergeResult, error) {
		assertAt("MergePullRequest", stateMerging, "merge", 2)
		return MergeResult{Merged: true}, nil
	})
	env.OnActivity(EnsureIssueState, mock.Anything, mock.Anything).Return(func(_ context.Context, s StateRequest) error {
		if s.State == "closed" {
			assertAt("EnsureIssueState", stateClosed, "close", 2)
		}
		return nil
	})

	runAndAssert(t, env)

	// Terminal snapshot: the agent/verification outcomes and the ordered
	// terminal condition are present, and the legacy "state" query still works.
	final := query("terminal")
	if final.State != stateClosed || final.CurrentStep != "close" {
		t.Fatalf("terminal status = %q/%q, want CLOSED/close", final.State, final.CurrentStep)
	}
	if final.AgentOutcome != agentOutcomeSuccess || final.VerificationOutcome != verificationSuccess {
		t.Fatalf("terminal outcomes = %q/%q, want SUCCESS/SUCCESS", final.AgentOutcome, final.VerificationOutcome)
	}
	if len(final.Conditions) != 3 || final.Conditions[2].Type != conditionTerminal || final.Conditions[2].Status != stateClosed {
		t.Fatalf("terminal conditions = %+v, want agent/verification/terminal(CLOSED)", final.Conditions)
	}
	if got := terminalState(t, env); got != stateClosed {
		t.Fatalf("existing state query = %q, want CLOSED (unchanged)", got)
	}
}

// #89/AC1: the PR workflow exposes the same status query; it never mutates and
// reports its own state progression.
func TestWorkflowPrReviewStatusQuery(t *testing.T) {
	env := (&testsuite.WorkflowTestSuite{}).NewTestWorkflowEnvironment()
	env.OnActivity(FetchDiff, mock.Anything, mock.Anything).Return("+1 line", nil)
	env.OnActivity(ReviewWithOpencode, mock.Anything, mock.Anything, mock.Anything).Return(func(context.Context, PrInput, string) (ReviewResult, error) {
		q, err := env.QueryWorkflow("status")
		if err != nil {
			t.Fatalf("query status during review: %v", err)
		}
		var s StatusQuery
		if err := q.Get(&s); err != nil {
			t.Fatalf("decode status during review: %v", err)
		}
		if s.State != stateReviewing || s.CurrentStep != "review" || len(s.Conditions) != 0 {
			t.Fatalf("mid-review status = %+v, want REVIEWING/review with no conditions yet", s)
		}
		return ReviewResult{Verdict: "APPROVE", CommitSHA: "sha", PRNumber: 1}, nil
	})
	env.OnActivity(SubmitReview, mock.Anything, mock.Anything, mock.Anything).Return(nil)

	env.ExecuteWorkflow(PrReviewWorkflow, PrInput{Owner: "o", Repo: "r", PrNum: 1, RepoPath: t.TempDir()})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	q, err := env.QueryWorkflow("status")
	if err != nil {
		t.Fatalf("query status: %v", err)
	}
	var s StatusQuery
	if err := q.Get(&s); err != nil {
		t.Fatal(err)
	}
	if s.State != stateReviewed || s.CurrentStep != "reviewed" {
		t.Fatalf("final status = %q/%q, want REVIEWED/reviewed", s.State, s.CurrentStep)
	}
	if s.AgentOutcome != agentOutcomeSuccess || len(s.Conditions) != 2 || s.Conditions[1].Type != conditionTerminal {
		t.Fatalf("final status = %+v, want SUCCESS + terminal condition", s)
	}
}
