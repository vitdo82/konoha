package konoha

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

// tuiTestData writes one finished cycle with a diff, agent output, and prompt so
// every pane has something to render.
func tuiTestData(t *testing.T) (*consoleData, *EvidenceStore) {
	t.Helper()
	store, err := NewEvidenceStore(EvidenceConfig{Dir: t.TempDir()}, &defaultRedactor)
	if err != nil {
		t.Fatal(err)
	}
	o := CycleOutcome{
		WorkflowID:          "repo-issue-1",
		TerminalState:       stateClosed,
		Owner:               "acme",
		Repo:                "repo",
		Issue:               1,
		Route:               stateSmall,
		Complexity:          3,
		Priority:            "P2",
		AgentOutcome:        agentOutcomeSuccess,
		VerificationOutcome: verificationFailed,
		Conditions: []Condition{
			{Type: "agent", Status: "SUCCESS", At: time.Now()},
			{Type: "verification", Status: "FAILED", Reason: "gate", At: time.Now()},
		},
	}
	b, _ := json.MarshalIndent(o, "", "  ")
	if err := store.Write(o.WorkflowID, evidenceOutcome, string(b), false); err != nil {
		t.Fatal(err)
	}
	if err := store.Write(o.WorkflowID, evidenceDiff, "diff --git a/main.go b/main.go\n--- a/main.go\n+++ b/main.go\n@@ -1 +1,2 @@\n-old\n+new\n+more\n", false); err != nil {
		t.Fatal(err)
	}
	if err := store.Write(o.WorkflowID, evidenceStdout, "line one\nline two\n", false); err != nil {
		t.Fatal(err)
	}
	if err := store.Write(o.WorkflowID, evidencePrompt, "prompt text", false); err != nil {
		t.Fatal(err)
	}
	d := buildConsoleData(store, nil, nil)
	return d, store
}

// #143 AC: the list renders cycles by state. The projection is pure and needs no
// terminal.
func TestTUIRendersListByState(t *testing.T) {
	cycles := []CycleSummary{
		{ID: "repo-issue-1", State: stateClosed, Route: stateSmall, Complexity: 3},
		{ID: "repo-issue-2", State: stateBlocked},
		{ID: "repo-issue-3", State: "RUNNING", Running: true},
	}
	m := tuiModel{Cycles: cycles, Columns: boardColumns(cycles), Pane: tuiPaneList}
	out := tuiView(m)
	for _, want := range []string{"RUNNING (1)", "NEEDS ATTENTION (1)", "CLOSED (1)", "repo-issue-2", "> repo-issue"} {
		if !strings.Contains(out, want) {
			t.Fatalf("list missing %q:\n%s", want, out)
		}
	}
}

// #143 AC: the detail view surfaces the terminal §6 state and the separate #84
// outcomes (never collapsed into a single "success").
func TestTUIRendersDetailOutcomes(t *testing.T) {
	o := CycleOutcome{
		TerminalState: stateHumanRequired, Owner: "acme", Repo: "repo", Issue: 7,
		AgentOutcome: agentOutcomeSuccess, VerificationOutcome: verificationFailed,
		Conditions: []Condition{{Type: "terminal", Status: "HUMAN_REQUIRED", At: time.Now()}},
	}
	det := CycleDetail{CycleSummary: CycleSummary{ID: "repo-issue-7", State: stateHumanRequired}, Outcome: &o, OutcomeConflict: true}
	out := tuiView(tuiModel{Pane: tuiPaneDetail, Detail: &det})
	for _, want := range []string{"state: HUMAN_REQUIRED", "agent outcome: SUCCESS", "verification outcome: FAILED", "CONFLICT"} {
		if !strings.Contains(out, want) {
			t.Fatalf("detail missing %q:\n%s", want, out)
		}
	}
}

// #143 AC: the diff, preview, and log-tail panes render from the recorded
// evidence projections.
func TestTUIRendersPanes(t *testing.T) {
	d, _ := tuiTestData(t)
	ctx := context.Background()
	m := &tuiModel{Pane: tuiPaneList}
	tuiLoad(ctx, d, m, tuiActRefresh|tuiActDetail|tuiActArtifacts|tuiActPreview)
	if m.Detail == nil || m.Detail.Diff == nil {
		t.Fatal("expected detail + diff loaded")
	}
	if len(m.Artifacts) == 0 {
		t.Fatal("expected artifacts listed")
	}

	m.Pane = tuiPaneDiff
	if out := tuiView(*m); !strings.Contains(out, "main.go (+2 -1)") || !strings.Contains(out, "+new") {
		t.Fatalf("diff pane missing content:\n%s", out)
	}

	m.Pane = tuiPanePreview
	for i, a := range m.Artifacts {
		if a.Path == evidencePrompt {
			m.Artifact = i
		}
	}
	tuiLoad(ctx, d, m, tuiActPreview)
	if out := tuiView(*m); !strings.Contains(out, "prompt text") {
		t.Fatalf("preview pane missing artifact text:\n%s", out)
	}

	m.Pane = tuiPaneLog
	tuiLoad(ctx, d, m, tuiActDetail)
	if out := tuiView(*m); !strings.Contains(out, "line two") {
		t.Fatalf("log pane missing tail:\n%s", out)
	}

	// A binary artifact is listed but never rendered as a preview.
	m.Pane = tuiPanePreview
	m.Artifacts = append(m.Artifacts, ArtifactView{Path: "blob.bin", Kind: artifactKindBinary, Size: 10})
	m.Artifact = len(m.Artifacts) - 1
	m.PreviewFor = ""
	tuiLoadPreview(d, m)
	if out := tuiView(*m); !strings.Contains(out, "not previewable") {
		t.Fatalf("binary preview must report download-only:\n%s", out)
	}
}

// #143 AC: attach is reported as SKIPPED without a live session provider, never
// a false success (§40).
func TestTUIAttachSkippedWithoutProvider(t *testing.T) {
	d, _ := tuiTestData(t)
	res := tuiAttach(context.Background(), d, "repo-issue-1")
	if res.Outcome != queueOutcomeSkipped || res.Action != queueActionAttach {
		t.Fatalf("attach = %+v, want SKIPPED attach", res)
	}
	if !strings.Contains(res.Reason, "#136") {
		t.Fatalf("attach reason = %q, want the provider note", res.Reason)
	}

	m := &tuiModel{Pane: tuiPaneList}
	ctx := context.Background()
	tuiLoad(ctx, d, m, tuiActRefresh)
	tuiLoad(ctx, d, m, tuiActAttach)
	if !strings.Contains(m.Status, queueOutcomeSkipped) {
		t.Fatalf("status = %q, want the reported SKIPPED", m.Status)
	}
}

// #143 AC: the TUI reads the same records as the console (`listCycles` /
// `cycleDetail`) — it is a projection, not a second source of truth.
func TestTUIReadsConsoleData(t *testing.T) {
	d, _ := tuiTestData(t)
	ctx := context.Background()
	m := &tuiModel{Pane: tuiPaneList}
	tuiLoad(ctx, d, m, tuiActRefresh|tuiActDetail)

	wantCycles, err := d.listCycles(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(m.Cycles, wantCycles) {
		t.Fatalf("tui cycles = %+v, want %+v", m.Cycles, wantCycles)
	}
	wantDetail, ok, err := d.cycleDetail(ctx, "repo-issue-1")
	if err != nil || !ok {
		t.Fatalf("cycleDetail = (%+v, %v, %v)", wantDetail, ok, err)
	}
	if !reflect.DeepEqual(m.Detail, &wantDetail) {
		t.Fatalf("tui detail drifted from the console projection")
	}
}

// #143 AC: no new network surface and no GitHub credential. The source starts no
// listener and names no credential env/helper.
func TestTUINoNetworkOrCredential(t *testing.T) {
	src, err := os.ReadFile("tui.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{`net.Listen`, `ListenAndServe`, `http.Server`, `http.Handle`, `"GH_`, "GH_APP", "GITHUB_TOKEN", "appToken("} {
		if strings.Contains(string(src), bad) {
			t.Fatalf("tui.go references %q; the TUI must add no network surface and read no GitHub credential", bad)
		}
	}
}

// #143: navigation moves through the deterministic board order, pane keys load
// the pane they name, and preview j/k moves the artifact cursor not the cycle.
func TestTUIKeyMap(t *testing.T) {
	m := &tuiModel{Pane: tuiPaneList, Columns: boardColumns([]CycleSummary{
		{ID: "repo-issue-1", State: stateClosed},
		{ID: "repo-issue-2", State: stateClosed},
	})}
	if act := tuiKey(m, "j"); act&tuiActDetail == 0 || m.Selected != 1 {
		t.Fatalf("j: selected=%d act=%b, want 1 + detail load", m.Selected, act)
	}
	if act := tuiKey(m, "2"); act&tuiActDetail == 0 || m.Pane != tuiPaneDetail {
		t.Fatalf("2: pane=%s act=%b, want detail", m.Pane, act)
	}
	if act := tuiKey(m, "4"); act&(tuiActArtifacts|tuiActPreview) != tuiActArtifacts|tuiActPreview {
		t.Fatalf("4: act=%b, want artifact+preview loads", act)
	}
	m.Artifacts = []ArtifactView{{Path: "a"}, {Path: "b"}}
	m.Pane = tuiPanePreview
	m.Selected = 0
	if act := tuiKey(m, "j"); m.Artifact != 1 || m.Selected != 0 || act&tuiActPreview == 0 {
		t.Fatalf("preview j: artifact=%d selected=%d act=%b", m.Artifact, m.Selected, act)
	}
	if act := tuiKey(m, "?"); !m.Help || act != 0 {
		t.Fatalf("? must open help")
	}
	if act := tuiKey(m, "esc"); m.Help || act != 0 {
		t.Fatalf("esc must close help")
	}
	if act := tuiKey(m, "q"); act&tuiActQuit == 0 {
		t.Fatalf("q must quit")
	}
}

// #143: the help overlay lists every binding.
func TestTUIHelpOverlay(t *testing.T) {
	out := tuiView(tuiModel{Help: true})
	for _, want := range []string{"Keys", "1-5 panes", "attach", "quit"} {
		if !strings.Contains(out, want) {
			t.Fatalf("help missing %q:\n%s", want, out)
		}
	}
}

// #143 security: evidence text is untrusted, so terminal control bytes are
// stripped at the render choke point and can never move the cursor or erase the
// authoritative state line.
func TestTUISafeStripsControlBytes(t *testing.T) {
	raw := "state: CLOSED\n\x1b[2K\rFAKE\x1b]0;pwned\x07\ttab"
	safe := tuiSafe(raw)
	for _, bad := range []string{"\x1b", "\r", "\x07"} {
		if strings.Contains(safe, bad) {
			t.Fatalf("tuiSafe kept %q: %q", bad, safe)
		}
	}
	for _, want := range []string{"state: CLOSED", "FAKE", "pwned", "\ttab"} {
		if !strings.Contains(safe, want) {
			t.Fatalf("tuiSafe dropped %q: %q", want, safe)
		}
	}
	m := tuiModel{Pane: tuiPaneLog, Detail: &CycleDetail{CycleSummary: CycleSummary{ID: "repo-issue-1"}},
		LogTail: "ok\n\x1b[2Kstate: CLOSED\r\n"}
	if out := tuiView(m); strings.ContainsAny(out, "\x1b\r") {
		t.Fatalf("rendered frame must not contain control bytes: %q", out)
	}
}

// #143: raw key decoding consumes a whole CSI/SS3 sequence, so a modifier arrow
// cannot leak its digits into the pane bindings.
func TestTUIReadKey(t *testing.T) {
	cases := map[string]string{
		"j":           "j",
		"\r":          "enter",
		"\x03":        "ctrl+c",
		"\x1b[A":      "up",
		"\x1b[B":      "down",
		"\x1bOA":      "up",
		"\x1b[H":      "home",
		"\x1bOF":      "end",
		"\x1b[1;5C":   "esc",
		"\x1b[3~":     "esc",
		"\x1b":        "esc",
		"\x1b[99;99X": "esc",
	}
	for in, want := range cases {
		r := bufio.NewReader(strings.NewReader(in))
		if got := tuiReadKey(r); got != want {
			t.Fatalf("tuiReadKey(%q) = %q, want %q", in, got, want)
		}
	}
}

// #143: a tall pane is clipped in the interactive painter while the header and
// footer stay visible; the log pane keeps the newest lines.
func TestTUIPaintClipsBodyToHeight(t *testing.T) {
	var lines []string
	for i := 0; i < 100; i++ {
		lines = append(lines, fmt.Sprintf("log-%02d", i))
	}
	m := tuiModel{Pane: tuiPaneLog, Detail: &CycleDetail{CycleSummary: CycleSummary{ID: "repo-issue-1"}},
		LogTail: strings.Join(lines, "\n") + "\n"}
	var out strings.Builder
	tuiPaint(&out, m, 10)
	got := strings.Split(strings.TrimPrefix(out.String(), "\x1b[H\x1b[2J"), "\n")
	if len(got) > 10 {
		t.Fatalf("painted %d lines for height 10: %q", len(got), out.String())
	}
	if !strings.Contains(out.String(), "Konoha operator console") || !strings.Contains(out.String(), "q quit") {
		t.Fatalf("header/footer must survive clipping:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "… [more]") {
		t.Fatalf("clipped body must be marked:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "log-99") || strings.Contains(out.String(), "log-00") {
		t.Fatalf("log clip must keep the newest lines:\n%s", out.String())
	}
}

// #143: a failed/absent detail load clears the previous cycle's data, so the
// cursor can never sit on cycle B while the pane renders cycle A.
func TestTUIStaleDetailCleared(t *testing.T) {
	d, _ := tuiTestData(t)
	cycles := []CycleSummary{{ID: "ghost-issue-1", State: stateBlocked}}
	m := &tuiModel{
		Pane: tuiPaneDetail, Cycles: cycles, Columns: boardColumns(cycles),
		Detail:    &CycleDetail{CycleSummary: CycleSummary{ID: "repo-issue-1"}},
		Artifacts: []ArtifactView{{Path: "agent.stdout"}}, Preview: "stale", PreviewFor: "agent.stdout",
	}
	tuiLoad(context.Background(), d, m, tuiActDetail)
	if m.Detail != nil || m.Artifacts != nil || m.Preview != "" || m.PreviewFor != "" {
		t.Fatalf("stale detail survived an absent load: %+v", m)
	}
}

// #143: the log tail is re-read only on an explicit refresh/detail load, not on
// every keypress.
func TestTUILogTailReloadGate(t *testing.T) {
	d, store := tuiTestData(t)
	ctx := context.Background()
	m := &tuiModel{Pane: tuiPaneLog}
	tuiLoad(ctx, d, m, tuiActRefresh|tuiActDetail)
	if !strings.Contains(m.LogTail, "line two") {
		t.Fatalf("initial tail = %q", m.LogTail)
	}
	if err := store.Write("repo-issue-1", evidenceStdout, "replaced\n", false); err != nil {
		t.Fatal(err)
	}
	tuiLoad(ctx, d, m, 0) // a keypress: no reload
	if !strings.Contains(m.LogTail, "line two") {
		t.Fatalf("keypress re-read the log: %q", m.LogTail)
	}
	tuiLoad(ctx, d, m, tuiActDetail)
	if !strings.Contains(m.LogTail, "replaced") {
		t.Fatalf("explicit refresh did not re-read the log: %q", m.LogTail)
	}
}

// #143: the preview pane clips long lines and g/G move the artifact cursor.
func TestTUIPreviewClipAndArtifactEnds(t *testing.T) {
	m := tuiModel{Pane: tuiPanePreview, Detail: &CycleDetail{CycleSummary: CycleSummary{ID: "repo-issue-1"}},
		Artifacts: []ArtifactView{{Path: "a"}, {Path: "b"}}, PreviewFor: "a",
		Preview: strings.Repeat("x", tuiLineMaxWidth+50)}
	out := tuiView(m)
	for _, line := range strings.Split(out, "\n") {
		if len([]rune(line)) > tuiLineMaxWidth+1 {
			t.Fatalf("preview line not clipped (%d runes): %q", len([]rune(line)), line)
		}
	}
	if act := tuiKey(&m, "G"); m.Artifact != 1 || act&tuiActPreview == 0 {
		t.Fatalf("G: artifact=%d act=%b", m.Artifact, act)
	}
	if act := tuiKey(&m, "g"); m.Artifact != 0 || act&tuiActPreview == 0 {
		t.Fatalf("g: artifact=%d act=%b", m.Artifact, act)
	}
	if m.Selected != 0 {
		t.Fatalf("g/G in preview must not move the cycle cursor")
	}
}
