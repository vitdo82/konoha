package konoha

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/stretchr/testify/mock"
)

// #212 conformance: the local runtime and the Temporal workflow record the same
// terminal §6 state and the same outcome.json fields for the same fixture. This
// is the guard against the two drivers drifting apart.
func TestLocalConformanceWithTemporalOutcome(t *testing.T) {
	withLocalAppCfg(t)

	in := IssueInput{
		Owner: "o", Repo: "r", IssueNum: testIssue, RepoPath: t.TempDir(),
		Trigger: "cli", Lifecycle: localTestLifecycle(),
	}

	// -- Temporal testsuite path --
	var temporalRaw string
	env := mockCycleEnv(t, EstimateResult{Complexity: 3, Confidence: "high"})
	mockSmallPath(env)
	env.OnActivity(WriteCycleEvidence, mock.Anything, mock.Anything).Return(func(_ context.Context, req EvidenceRequest) error {
		if req.Outcome != "" {
			temporalRaw = req.Outcome
		}
		return nil
	})
	env.ExecuteWorkflow(IssueCycleWorkflow, in)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("temporal workflow: %v", err)
	}
	if temporalRaw == "" {
		t.Fatal("temporal path wrote no outcome")
	}
	var temporalOut CycleOutcome
	if err := json.Unmarshal([]byte(temporalRaw), &temporalOut); err != nil {
		t.Fatalf("decode temporal outcome: %v", err)
	}

	// -- local in-process path --
	f := newLocalFakes()
	rt := newLocalRuntime(appCfg, f.acts)
	if err := rt.runCycle(context.Background(), in); err != nil {
		t.Fatalf("local cycle: %v", err)
	}
	localOut := f.localOutcome(t)

	if got := terminalStateOf(temporalOut); got != stateClosed {
		t.Fatalf("temporal terminal = %q, want CLOSED", got)
	}
	if got := terminalStateOf(localOut); got != stateClosed {
		t.Fatalf("local terminal = %q, want CLOSED", got)
	}

	temporalConds, localConds := temporalOut.Conditions, localOut.Conditions
	temporalOut = normalizeConformanceOutcome(temporalOut)
	localOut = normalizeConformanceOutcome(localOut)
	if !reflect.DeepEqual(temporalOut, localOut) {
		t.Fatalf("outcome drift between runtimes:\ntemporal = %+v\nlocal    = %+v", temporalOut, localOut)
	}

	// Conditions carry per-run timestamps; compare the ordered shape instead.
	if len(temporalConds) != len(localConds) {
		t.Fatalf("condition count = %d/%d", len(temporalConds), len(localConds))
	}
	for i := range temporalConds {
		a, b := temporalConds[i], localConds[i]
		if a.Type != b.Type || a.Status != b.Status || a.Reason != b.Reason || a.Message != b.Message {
			t.Fatalf("condition %d drift: temporal=%+v local=%+v", i, a, b)
		}
	}
}

func terminalStateOf(o CycleOutcome) string { return o.TerminalState }

// normalizeConformanceOutcome drops the fields that legitimately differ per run
// (run id, wall-clock duration, timestamps) so the remaining evidence must match.
func normalizeConformanceOutcome(o CycleOutcome) CycleOutcome {
	o.WorkflowID = ""
	o.DurationSeconds = 0
	o.Conditions = nil
	return o
}
