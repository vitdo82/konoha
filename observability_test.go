package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// resetMetrics clears the global registry so tests are independent.
func resetMetrics() {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	sink.counters = map[string]float64{}
	sink.gauges = map[string]float64{}
}

// §39: autonomous_completion_rate = autonomous closes / total completes, and the
// human-required close path is counted as human (NOT autonomous).
func TestAutonomousCompletionRate(t *testing.T) {
	resetMetrics()
	defer resetMetrics()

	// 3 completes: 2 autonomous + 1 human (human-required close).
	record(1, stateClosed, true)
	record(2, stateClosed, true)
	record(3, stateClosed, false) // human-required close

	if got := sink.get(mAutonomousRate); got != 2.0/3.0 {
		t.Fatalf("autonomous_completion_rate = %v, want %v", got, 2.0/3.0)
	}
	if got := sink.get(mCyclesCompletedAuton); got != 2 {
		t.Fatalf("autonomous completes = %v, want 2", got)
	}
	if got := sink.get(mCyclesCompletedHuman); got != 1 {
		t.Fatalf("human completes = %v, want 1", got)
	}
	if got := sink.get(mCyclesCompleted); got != 3 {
		t.Fatalf("total completes = %v, want 3", got)
	}
}

// No completes yet → rate is 0 (not NaN), and jamming in blocked/failed runs
// does not count as a completion.
func TestAutonomousRateZeroAndBlocked(t *testing.T) {
	resetMetrics()
	defer resetMetrics()

	record(1, stateBlocked, false)
	record(2, stateFailed, false)
	if got := sink.get(mAutonomousRate); got != 0 {
		t.Fatalf("rate with no completes must be 0, got %v", got)
	}
	if got := sink.get(mCyclesBlocked); got != 2 {
		t.Fatalf("blocked+failed = %v, want 2", got)
	}
}

// §39: human_escalation_rate = escalations / started; split_rate = large/total;
// review_disagreement_rate = disagreements / review rounds.
func TestDerivedRates(t *testing.T) {
	resetMetrics()
	defer resetMetrics()

	// 4 started: 3 small + 1 large; the first carries 2 review rounds, 1 disagreement.
	RecordCycleMetrics(bg(), CycleOutcome{WorkflowID: "w-s1", Issue: 1, Route: stateSmall, Complexity: 3, ReviewRounds: 2, Disagreements: 1})
	RecordCycleMetrics(bg(), CycleOutcome{WorkflowID: "w-s2", Issue: 2, Route: stateSmall, Complexity: 3})
	RecordCycleMetrics(bg(), CycleOutcome{WorkflowID: "w-s3", Issue: 3, Route: stateSmall, Complexity: 3})
	RecordCycleMetrics(bg(), CycleOutcome{WorkflowID: "w-l1", Issue: 4, Route: stateLarge, Complexity: 6})
	// 2 escalations WITHOUT a fresh start (no WorkflowID) so 'started' stays 4.
	RecordCycleMetrics(bg(), CycleOutcome{HumanRequired: true, EscalationReason: reasonRepeatedReviewFailure})
	RecordCycleMetrics(bg(), CycleOutcome{HumanRequired: true, EscalationReason: reasonAgentFailure})

	if got := sink.get(mDidHumanEscalationRate); got != 0.5 {
		t.Fatalf("human_escalation_rate = %v, want 0.5 (2/4)", got)
	}
	if got := sink.get(mSplitRate); got != 0.25 {
		t.Fatalf("split_rate = %v, want 0.25 (1/4)", got)
	}
	if got := sink.get(mReviewDisagreementRate); got != 0.5 {
		t.Fatalf("review_disagreement_rate = %v, want 0.5 (1/2)", got)
	}
}

// §39 stable naming: every metric a workflow records appears in the text export
// under its exact documented name (no drift).
func TestMetricsStableNames(t *testing.T) {
	resetMetrics()
	defer resetMetrics()

	record(1, stateClosed, true)
	recordHuman(2)
	RecordCycleMetrics(bg(), CycleOutcome{WorkflowID: "w-lrg", Issue: 3, Route: stateLarge, Complexity: 7})

	text := MetricsText()
	for _, name := range []string{
		mCyclesStarted, mCyclesCompleted, mCyclesCompletedAuton, mCyclesCompletedHuman,
		mHumanEscalations, mAutonomousRate, mDidHumanEscalationRate, mSplitRate,
		mDevelopmentAttempts, mReviewRounds, mReviewDisagreements, mSplitSmall, mSplitLarge,
	} {
		if !strings.Contains(text, name) {
			t.Fatalf("stable metric %q missing from export:\n%s", name, text)
		}
	}
	// complexity distribution recorded as estimate_complexity_<n>.
	if !strings.Contains(text, "estimate_complexity_3") {
		t.Fatalf("complexity distribution missing:\n%s", text)
	}
}

// §39 field set: logCycleEvent emits the documented fields through a narrow
// logger (substituted with a recorder) — deterministic and replay-safe.
func TestLogCycleEventFields(t *testing.T) {
	rec := &recordingLogger{}
	logCycleEvent(rec, "konoha-issue-5", 5, "r", stateReviewing, 3, "high", 2, 1)

	if rec.msg != "issue state" {
		t.Fatalf("msg = %q, want 'issue state'", rec.msg)
	}
	for _, want := range []string{
		"workflow", "konoha-issue-5", "issue", "5", "repo", "r",
		"state", stateReviewing, "complexity", "3", "confidence", "high",
		"attempts", "2", "review_rounds", "1",
	} {
		if !containsStringKV(rec.kv, want) {
			t.Fatalf("field %q missing from log fields %v", want, rec.kv)
		}
	}
}

// Helpers.
func record(issue int, terminal string, autonomous bool) {
	RecordCycleMetrics(bg(), CycleOutcome{
		WorkflowID: fmt.Sprintf("w-%d", issue), Issue: issue, TerminalState: terminal,
		AutonomousClose: autonomous, Route: stateSmall, Complexity: 3,
		Attempts: 1, ReviewRounds: 1,
	})
}

func recordHuman(issue int) {
	RecordCycleMetrics(bg(), CycleOutcome{
		WorkflowID: fmt.Sprintf("w-%d", issue), Issue: issue,
		TerminalState:   stateClosed, // human-required CLOSE (not autonomous)
		AutonomousClose: false,
		HumanRequired:   true, EscalationReason: reasonRepeatedReviewFailure,
		Route: stateSmall, Complexity: 3, Attempts: 1, ReviewRounds: 1, Disagreements: 1,
	})
}

func bg() context.Context { return context.Background() }

type recordingLogger struct {
	msg string
	kv  []interface{}
}

func (r *recordingLogger) Info(msg string, kv ...interface{}) {
	r.msg = msg
	r.kv = kv
}

func containsStringKV(kv []interface{}, s string) bool {
	for _, k := range kv {
		if fmt.Sprint(k) == s {
			return true
		}
	}
	return false
}
