package konoha

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// #87 AC: parseHarnessUsage is best-effort and never fabricates. Absence,
// malformed JSON, and negative values all contribute zero.
func TestParseHarnessUsageBestEffort(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want HarnessUsage
	}{
		{"absent", "no usage here\nVERDICT:APPROVE\n", HarnessUsage{}},
		{"single", "text\nKONOHA_USAGE {\"tokensIn\":10,\"tokensOut\":4,\"costUSD\":0.5}\nmore", HarnessUsage{10, 4, 0.5}},
		{"summed steps", "KONOHA_USAGE {\"tokensIn\":10}\nKONOHA_USAGE {\"tokensIn\":2,\"tokensOut\":3}\n", HarnessUsage{12, 3, 0}},
		{"malformed json", "KONOHA_USAGE {not json}\n", HarnessUsage{}},
		{"negative ignored", "KONOHA_USAGE {\"tokensIn\":-5,\"costUSD\":-1}\n", HarnessUsage{}},
		{"unmarked object ignored", "{\"tokensIn\":9,\"costUSD\":9}\n", HarnessUsage{}},
	}
	for _, c := range cases {
		if got := parseHarnessUsage(c.raw); got != c.want {
			t.Fatalf("%s: parseHarnessUsage = %+v, want %+v", c.name, got, c.want)
		}
	}
}

// #87 AC: a mocked agent that reports tokens yields the expected §39 counters
// through MetricsText (the real harness/exec path, not a hand-built struct).
func TestMockedAgentUsageCounters(t *testing.T) {
	resetMetrics()
	defer resetMetrics()

	stubDir := t.TempDir()
	writeTestFile(t, stubDir, "opencode",
		"#!/bin/sh\necho 'KONOHA_USAGE {\"tokensIn\":123,\"tokensOut\":45,\"costUSD\":0.25}'\n")
	if err := os.Chmod(filepath.Join(stubDir, "opencode"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	cmd, err := agentCommand(context.Background(), "PROMPT", "", "opencode", stubDir)
	if err != nil {
		t.Fatal(err)
	}
	stdout, _, err := runCapture(cmd)
	if err != nil {
		t.Fatal(err)
	}
	u := parseHarnessUsage(stdout)
	if u != (HarnessUsage{123, 45, 0.25}) {
		t.Fatalf("parsed mocked usage = %+v", u)
	}

	RecordCycleMetrics(bg(), CycleOutcome{
		WorkflowID: "w-1", Issue: 1, TerminalState: stateClosed, AutonomousClose: true, Usage: u,
	})
	text := MetricsText()
	for _, want := range []string{
		"agent_tokens_in_total 123",
		"agent_tokens_out_total 45",
		"agent_cost_usd_total 0.25",
		"cost_per_completed_issue 0.25",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("metrics missing %q:\n%s", want, text)
		}
	}
}

// #87 AC: an agent reporting nothing contributes zero — no counter is created
// and no number is fabricated.
func TestNoUsageContributesZero(t *testing.T) {
	resetMetrics()
	defer resetMetrics()

	if u := parseHarnessUsage("VERDICT:APPROVE\nno usage envelope\n"); !u.IsZero() {
		t.Fatalf("absent usage must be zero, got %+v", u)
	}
	RecordCycleMetrics(bg(), CycleOutcome{
		WorkflowID: "w-2", Issue: 2, TerminalState: stateClosed, AutonomousClose: true,
	})
	for _, name := range []string{mAgentTokensIn, mAgentTokensOut, mAgentCostUSD} {
		if got := sink.get(name); got != 0 {
			t.Fatalf("unreported usage fabricated %s = %v", name, got)
		}
	}
	if got := sink.get(mCostPerCompletedIssue); got != 0 {
		t.Fatalf("cost_per_completed_issue with no cost must be 0, got %v", got)
	}
}

// #87 AC: cost_per_completed_issue recomputes from terminal outcomes, mirroring
// the existing derived-rate gauges.
func TestCostPerCompletedIssueDerived(t *testing.T) {
	resetMetrics()
	defer resetMetrics()

	RecordCycleMetrics(bg(), CycleOutcome{
		WorkflowID: "w-1", Issue: 1, TerminalState: stateClosed, AutonomousClose: true,
		Usage: HarnessUsage{CostUSD: 3},
	})
	RecordCycleMetrics(bg(), CycleOutcome{
		WorkflowID: "w-2", Issue: 2, TerminalState: stateClosed, AutonomousClose: true,
	})
	// A blocked run is not a completion and must not deflate the per-issue cost.
	RecordCycleMetrics(bg(), CycleOutcome{
		WorkflowID: "w-3", Issue: 3, TerminalState: stateBlocked, Usage: HarnessUsage{CostUSD: 4},
	})

	if got := sink.get(mAgentCostUSD); got != 7 {
		t.Fatalf("agent_cost_usd_total = %v, want 7", got)
	}
	if got := sink.get(mCyclesCompleted); got != 2 {
		t.Fatalf("completes = %v, want 2", got)
	}
	if got := sink.get(mCostPerCompletedIssue); got != 3.5 {
		t.Fatalf("cost_per_completed_issue = %v, want 3.5 (7/2)", got)
	}
}
