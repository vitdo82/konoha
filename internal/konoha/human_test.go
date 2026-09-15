package konoha

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// §18/§36: the human decision channel is a bounded set; anything else (or an
// empty signal) is rejected and can never drive a transition.
func TestValidateHumanDecision(t *testing.T) {
	for _, a := range []string{"resume", "reestimate", "repartition", "retry", "block", "cancel", "close", "approve", "deny"} {
		if err := ValidateHumanDecision(a); err != nil {
			t.Fatalf("bounded decision %q rejected: %v", a, err)
		}
	}
	for _, a := range []string{"", "ship", "merge", "SHIP IT", "resume "} {
		if err := ValidateHumanDecision(a); err == nil {
			t.Fatalf("decision %q must be rejected, got nil", a)
		}
	}
}

// §18/§36: only the controlled reasons may be recorded as escalating an issue.
func TestValidateHumanReason(t *testing.T) {
	for _, r := range []string{
		"requirements_ambiguous", "security_sensitive", "architecture_change",
		"repeated_review_failure", "insufficient_test_coverage", "external_dependency",
		"policy_violation", "agent_failure", "conflicting_requirements",
	} {
		if err := ValidateHumanReason(r); err != nil {
			t.Fatalf("bounded reason %q rejected: %v", r, err)
		}
	}
	for _, r := range []string{"", "typo", "need help", "PO_GATE"} {
		if err := ValidateHumanReason(r); err == nil {
			t.Fatalf("reason %q must be rejected, got nil", r)
		}
	}
}

// §40: a person-gate timeout never silently becomes success — before the
// window we're still awaiting, after it we're timed-out-blocked, and there is
// NO signal that ever says "proceed anyway".
func TestEvaluateHumanGateNoSilentSuccess(t *testing.T) {
	timeout := 168 * time.Hour
	d := EvaluateHumanGate(timeout-1*time.Minute, timeout)
	if !d.Awaiting || d.TimedOut {
		t.Fatalf("before timeout must be Awaiting, got %+v", d)
	}
	d = EvaluateHumanGate(timeout, timeout)
	if !d.TimedOut || d.Awaiting {
		t.Fatalf("at timeout must be TimedOut/blocked, got %+v", d)
	}
	d = EvaluateHumanGate(timeout+72*time.Hour, timeout)
	if !d.TimedOut || d.Awaiting {
		t.Fatalf("past timeout must stay blocked, got %+v", d)
	}
}

// §33: poGateTimeout defaults to 168h and honors a configured value.
func TestPOGateTimeout(t *testing.T) {
	if got := POGateTimeout(LifecycleConfig{}); got != 168*time.Hour {
		t.Fatalf("default poGateTimeout = %v, want 168h", got)
	}
	if got := POGateTimeout(LifecycleConfig{POGateTimeout: "24h"}); got != 24*time.Hour {
		t.Fatalf("configured poGateTimeout = %v, want 24h", got)
	}
	if got := POGateTimeout(LifecycleConfig{POGateTimeout: "garbage"}); got != 168*time.Hour {
		t.Fatalf("unparseable poGateTimeout must fall back to default, got %v", got)
	}
}

// §18: each human decision maps deterministically; terminal ones to their
// state, continue-ones to Resume; malformed fails closed.
func TestApplyHumanDecision(t *testing.T) {
	for _, tc := range []struct {
		action string
		state  string
		resume bool
	}{
		{"block", stateBlocked, false},
		{"cancel", stateCancelled, false},
		{"close", stateClosed, false},
		{"resume", "", true},
		{"reestimate", "", true},
		{"repartition", "", true},
		{"retry", "", true},
		{"approve", "", true},               // §55: pre-execution gate approval continues
		{"deny", stateHumanRequired, false}, // §55: gate denial escalates to a human
	} {
		tv, err := ApplyHumanDecision(HumanDecision{Action: tc.action, Present: true})
		if err != nil {
			t.Fatalf("ApplyHumanDecision(%q): %v", tc.action, err)
		}
		if tv.TargetState != tc.state || tv.Resume != tc.resume {
			t.Fatalf("ApplyHumanDecision(%q) = %+v, want state=%q resume=%v",
				tc.action, tv, tc.state, tc.resume)
		}
	}
	if _, err := ApplyHumanDecision(HumanDecision{Action: "merge", Present: true}); err == nil {
		t.Fatal("invalid decision must fail closed, got nil")
	}
}

// §21/§22 label projection: human-required carries konoha:human-required;
// blocked additionally carries konoha:blocked.
func TestHumanLabelProjection(t *testing.T) {
	if got := HumanRequiredLabels(); len(got) != 1 || got[0] != "konoha:human-required" {
		t.Fatalf("HumanRequiredLabels() = %v", got)
	}
	got := BlockedLabels()
	if len(got) != 2 || got[0] != "konoha:human-required" || got[1] != "konoha:blocked" {
		t.Fatalf("BlockedLabels() = %v", got)
	}
}

// §24: the decision marker round-trips and is byte-exact for idempotent reads.
func TestHumanDecisionMarker(t *testing.T) {
	want := "<!-- konoha-decision: resume -->"
	if got := humanDecisionMarker("resume"); got != want {
		t.Fatalf("marker = %q want %q", got, want)
	}
	a, ok := parseHumanDecisionMarker(want)
	if !ok || a != "resume" {
		t.Fatalf("parse marker: %q ok=%v", a, ok)
	}
	if _, ok := parseHumanDecisionMarker("no marker"); ok {
		t.Fatal("missing marker must not parse")
	}
}

// §18 decision channel via ghRunner seam: newest comment wins over the body,
// an absent signal yields Present=false, and a malformed signal fails closed.
func TestReadHumanDecision(t *testing.T) {
	bodyAction, err := json.Marshal(struct{ Body string }{humanDecisionMarker("block")})
	if err != nil {
		t.Fatal(err)
	}
	commentResume := []byte(`[{"body":"oops"},{"body":"` + "<!-- konoha-decision: resume -->" + `"}]`)
	commentMalformed := []byte(`[{"body":"<!-- konoha-decision: merge -->"}]`)

	for _, tc := range []struct {
		name       string
		body       []byte
		comments   []byte
		wantAbsent bool
		wantAction string
		wantErr    bool
	}{
		{"body marker only", bodyAction, []byte(`[]`), false, "block", false},
		{"newest comment wins over body", bodyAction, commentResume, false, "resume", false},
		{"no signal", []byte(`{"body":"deploy notes"}`), []byte(`[]`), true, "", false},
		{"malformed signal fails closed", bodyAction, commentMalformed, false, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prev := ghRunner
			ghRunner = func(_ context.Context, args ...string) ([]byte, error) {
				if len(args) >= 2 && args[1] == "repos/o/r/issues/7" {
					return tc.body, nil
				}
				if len(args) >= 2 && args[1] == "repos/o/r/issues/7/comments" {
					return tc.comments, nil
				}
				return nil, fmt.Errorf("unexpected gh call: %v", args)
			}
			defer func() { ghRunner = prev }()

			d, err := ReadHumanDecision(context.Background(), HumanDecisionInput{Owner: "o", Repo: "r", Num: 7})
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected malformed signal error, got %+v", d)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if d.Present != !tc.wantAbsent {
				t.Fatalf("Present=%v, want absent=%v", d.Present, tc.wantAbsent)
			}
			if !tc.wantAbsent && d.Action != tc.wantAction {
				t.Fatalf("action=%q, want %q", d.Action, tc.wantAction)
			}
		})
	}
}
