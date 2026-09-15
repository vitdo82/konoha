package konoha

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// §55: preGate off (default) gates nothing — the pipeline stays autonomous.
// preGate on with an empty list gates every step (fail-closed default); an
// explicit subset gates only those.
func TestGatedStepsAndApprovalRequired(t *testing.T) {
	off := LifecycleConfig{}
	if len(GatedSteps(off)) != 0 || ApprovalRequired(off, approvalStepPublish) {
		t.Fatal("preGate off must gate nothing (no behavior change)")
	}

	all := LifecycleConfig{Approval: ApprovalConfig{PreGate: true}}
	if got := GatedSteps(all); len(got) != 3 {
		t.Fatalf("preGate with empty requiredSteps must default to all, got %v", got)
	}
	for _, s := range approvalSteps {
		if !ApprovalRequired(all, s) {
			t.Fatalf("step %q must be gated", s)
		}
	}

	subset := LifecycleConfig{Approval: ApprovalConfig{PreGate: true, RequiredSteps: []string{approvalStepMerge}}}
	if !ApprovalRequired(subset, approvalStepMerge) || ApprovalRequired(subset, approvalStepPublish) {
		t.Fatal("explicit subset must gate only the listed steps")
	}
}

// §55/§36: only approve (and the legacy resume synonym) grants; a valid but
// inapplicable decision denies rather than silently proceeding.
func TestEvaluateApproval(t *testing.T) {
	for _, tc := range []struct {
		name string
		d    HumanDecision
		want approvalVerdict
	}{
		{"absent", HumanDecision{}, approvalPending},
		{"approve", HumanDecision{Action: humanDecisionApprove, Present: true}, approvalGranted},
		{"resume", HumanDecision{Action: humanDecisionResume, Present: true}, approvalGranted},
		{"deny", HumanDecision{Action: humanDecisionDeny, Present: true}, approvalDenied},
		{"block", HumanDecision{Action: humanDecisionBlock, Present: true}, approvalDenied},
		{"cancel", HumanDecision{Action: humanDecisionCancel, Present: true}, approvalDenied},
		{"close", HumanDecision{Action: humanDecisionClose, Present: true}, approvalDenied},
	} {
		if got := EvaluateApproval(tc.d); got != tc.want {
			t.Fatalf("%s: EvaluateApproval = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// §55: an unknown step fails closed; the closed set is validated at load.
func TestValidateApprovalStep(t *testing.T) {
	for _, s := range approvalSteps {
		if err := ValidateApprovalStep(s); err != nil {
			t.Fatalf("step %q rejected: %v", s, err)
		}
	}
	if err := ValidateApprovalStep("ship"); err == nil {
		t.Fatal("unknown step must fail closed")
	}
}

// §55 config: approval/audit parse, and a typo'd step or a sink-less enabled
// audit fails fast at load (never a silently un-gated or un-recorded run).
func TestApprovalAuditConfig(t *testing.T) {
	p := writeRepos(t, `
repos:
  - repo: r
    owner: o
    repoPath: /x
    lifecycle:
      approval:
        preGate: true
        requiredSteps: [publish, merge, deploy]
      audit:
        enabled: true
        sink: /var/log/konoha-audit.jsonl
`)
	repos, err := LoadReposConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	lc := *repos[0].Lifecycle
	if !ApprovalRequired(lc, approvalStepDeploy) || !lc.Audit.Enabled || lc.Audit.Sink == "" {
		t.Fatalf("unexpected approval/audit config: %+v %+v", lc.Approval, lc.Audit)
	}

	for _, bad := range []string{
		`{approval: {preGate: true, requiredSteps: [ship]}}`,
		`{audit: {enabled: true}}`,
		// §55 AC #4: an enabled gate must record every decision, so preGate
		// without an audit sink fails at load.
		`{approval: {preGate: true}}`,
		`{approval: {preGate: true}, audit: {enabled: false, sink: /tmp/a.jsonl}}`,
	} {
		p := writeRepos(t, `repos: [{repo: r, owner: o, repoPath: /x, lifecycle:`+bad+`}]`)
		if _, err := LoadReposConfig(p); err == nil {
			t.Fatalf("expected rejection for lifecycle %s", bad)
		}
	}
}

// §55: the audit activity appends one JSON line per event to the sink; disabled
// config writes nothing.
func TestRecordAuditEvent(t *testing.T) {
	sinkPath := filepath.Join(t.TempDir(), "audit.jsonl")
	cfg := AuditConfig{Enabled: true, Sink: sinkPath}
	for _, ev := range []AuditEvent{
		{Owner: "o", Repo: "r", Issue: 7, Step: approvalStepPublish, Actor: "human", Decision: "allow"},
		{Owner: "o", Repo: "r", Issue: 7, Step: approvalStepMerge, Actor: "policy", Decision: "timeout"},
	} {
		if err := RecordAuditEvent(context.Background(), AuditRequest{Event: ev, Config: cfg}); err != nil {
			t.Fatal(err)
		}
	}
	b, err := os.ReadFile(sinkPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 2 {
		t.Fatalf("audit sink has %d lines, want 2: %q", len(lines), string(b))
	}
	var rec struct {
		Time     string `json:"time"`
		Decision string `json:"decision"`
		Step     string `json:"step"`
	}
	if err := json.Unmarshal([]byte(lines[1]), &rec); err != nil {
		t.Fatalf("audit line is not JSON: %v", err)
	}
	if rec.Decision != "timeout" || rec.Step != approvalStepMerge || rec.Time == "" {
		t.Fatalf("unexpected audit record: %+v", rec)
	}

	// Disabled → no file, no error (§42-friendly no-op).
	off := filepath.Join(t.TempDir(), "nope.jsonl")
	if err := RecordAuditEvent(context.Background(), AuditRequest{
		Event: AuditEvent{Repo: "r", Step: approvalStepPublish}, Config: AuditConfig{Enabled: false, Sink: off},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(off); !os.IsNotExist(err) {
		t.Fatalf("disabled audit must not create the sink, stat err=%v", err)
	}
}

// §55/§40: the gate reads the §18 decision channel; approve grants, deny denies,
// and an absent decision times out (never a silent proceed).
func TestAwaitApproval(t *testing.T) {
	prevInterval := approvalPollInterval
	approvalPollInterval = time.Millisecond
	t.Cleanup(func() { approvalPollInterval = prevInterval })

	body := func(marker string) []byte {
		b, _ := json.Marshal(struct{ Body string }{marker})
		return b
	}
	stub := func(bodyBytes []byte) {
		prev := ghRunner
		t.Cleanup(func() { ghRunner = prev })
		ghRunner = func(_ context.Context, args ...string) ([]byte, error) {
			if len(args) >= 2 && strings.HasSuffix(args[1], "/issues/7") {
				return bodyBytes, nil
			}
			if len(args) >= 2 && strings.HasSuffix(args[1], "/issues/7/comments") {
				return []byte(`[]`), nil
			}
			return nil, nil
		}
	}

	stub(body(humanDecisionMarker(humanDecisionApprove)))
	res, err := AwaitApproval(context.Background(), ApprovalRequest{Owner: "o", Repo: "r", Num: 7, Step: approvalStepPublish})
	if err != nil || !res.Granted || res.Denied || res.TimedOut {
		t.Fatalf("approve → %+v err=%v", res, err)
	}
	if res.Action != humanDecisionApprove {
		t.Fatalf("action = %q, want approve", res.Action)
	}

	stub(body(humanDecisionMarker(humanDecisionDeny)))
	res, err = AwaitApproval(context.Background(), ApprovalRequest{Owner: "o", Repo: "r", Num: 7, Step: approvalStepMerge})
	if err != nil || !res.Denied || res.Granted {
		t.Fatalf("deny → %+v err=%v", res, err)
	}

	stub(body("no decision here"))
	res, err = AwaitApproval(context.Background(), ApprovalRequest{
		Owner: "o", Repo: "r", Num: 7, Step: approvalStepDeploy, Timeout: "5ms",
	})
	if err != nil || !res.TimedOut || res.Granted || res.Denied {
		t.Fatalf("absent decision must time out (never proceed), got %+v err=%v", res, err)
	}

	// §42: shadow mode has no mutation to gate.
	if res, err := AwaitApproval(context.Background(), ApprovalRequest{
		Owner: "o", Repo: "r", Num: 7, Step: approvalStepPublish, DryRun: true,
	}); err != nil || !res.Skipped {
		t.Fatalf("dryRun gate must skip, got %+v err=%v", res, err)
	}

	// An unknown step fails closed (§36).
	if _, err := AwaitApproval(context.Background(), ApprovalRequest{Owner: "o", Repo: "r", Num: 7, Step: "ship"}); err == nil {
		t.Fatal("unknown gated step must fail closed")
	}
}
