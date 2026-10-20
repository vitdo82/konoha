package konoha

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"go.temporal.io/sdk/client"
	"golang.org/x/term"
)

// tuiTemporalClient is the TUI's Temporal dial seam. A test injects nil to stay
// fully offline; production dials the same address the console uses.
var tuiTemporalClient = func(addr string) (client.Client, error) {
	return client.Dial(client.Options{HostPort: addr})
}

// Terminal operator console (#143). `prbot tui` is a shell-native cockpit over
// the SAME records the CLI (#99) and web console (#105) read: the evidence store
// for finished cycles and the #89 status query for running ones. It starts no
// listener and reads no GitHub credential; every projection here is PURE so the
// view is unit-tested without a real terminal.
//
// The §6 state stays authoritative (it is rendered, never inferred from labels),
// and the #84 agent/verification outcomes are shown as separate facts. Attach is
// a session verb owned by #136/#144: with no provider it reports SKIPPED, never
// a false success (§40).

// tuiPane identifies the active pane. The order is the tab order used by the
// numeric key bindings.
type tuiPane string

const (
	tuiPaneList    tuiPane = "list"
	tuiPaneDetail  tuiPane = "detail"
	tuiPaneDiff    tuiPane = "diff"
	tuiPanePreview tuiPane = "preview"
	tuiPaneLog     tuiPane = "log"
)

// tuiPanes is the fixed tab order (also the 1..5 key binding order).
var tuiPanes = []tuiPane{tuiPaneList, tuiPaneDetail, tuiPaneDiff, tuiPanePreview, tuiPaneLog}

// Rendering bounds keep one hostile/large artifact or log from flooding the
// terminal; a truncated view is always marked, never silently cut.
const (
	tuiDiffLineCap  = 400
	tuiPreviewCap   = 64 * 1024
	tuiLogLineCap   = 40
	tuiLineMaxWidth = 240
)

// tuiAction is the set of side effects a key press requests. Navigation and pane
// switching are applied to the model in-place; the caller performs the loads the
// bits name (that is where I/O lives, so the key map stays pure).
type tuiAction uint

const (
	tuiActQuit tuiAction = 1 << iota
	tuiActRefresh
	tuiActDetail
	tuiActArtifacts
	tuiActPreview
	tuiActAttach
)

// tuiModel is the whole TUI state. It carries only projections of the durable
// records, so the interactive layer never becomes a second source of truth.
type tuiModel struct {
	Cycles  []CycleSummary
	Columns []BoardColumn
	// Selected indexes tuiRows(Columns) (the deterministic board order).
	Selected int
	Pane     tuiPane
	Help     bool

	Detail     *CycleDetail
	Artifacts  []ArtifactView
	Artifact   int
	Preview    string
	PreviewFor string
	PreviewMsg string
	LogTail    string

	Status string
	Err    string
}

// tuiRows flattens the board columns into the deterministic navigation order:
// column order first, then each column's newest-first cards. Pure.
func tuiRows(cols []BoardColumn) []CycleSummary {
	var out []CycleSummary
	for _, col := range cols {
		out = append(out, col.Cards...)
	}
	return out
}

// tuiSelectedID returns the id under the cursor ("" when empty). Pure.
func tuiSelectedID(m tuiModel) string {
	rows := tuiRows(m.Columns)
	if m.Selected < 0 || m.Selected >= len(rows) {
		return ""
	}
	return rows[m.Selected].ID
}

// tuiClamp keeps the cursor inside the flattened row list. Pure.
func tuiClamp(m *tuiModel) {
	rows := tuiRows(m.Columns)
	if len(rows) == 0 {
		m.Selected = 0
		return
	}
	if m.Selected < 0 {
		m.Selected = 0
	}
	if m.Selected >= len(rows) {
		m.Selected = len(rows) - 1
	}
}

// tuiKey handles one key, mutating navigation/pane state and returning the loads
// the caller must perform. Pure: no I/O, so the key map is unit-testable.
func tuiKey(m *tuiModel, key string) tuiAction {
	if key == "ctrl+c" {
		return tuiActQuit
	}
	if key == "?" {
		m.Help = !m.Help
		return 0
	}
	if m.Help {
		if key == "esc" || key == "q" || key == "enter" {
			m.Help = false
		}
		return 0
	}
	switch key {
	case "q":
		return tuiActQuit
	case "1":
		m.Pane = tuiPaneList
		return 0
	case "2", "enter", "l":
		if m.Pane == tuiPaneList && len(tuiRows(m.Columns)) == 0 {
			return 0
		}
		m.Pane = tuiPaneDetail
		return tuiActDetail
	case "3", "d":
		m.Pane = tuiPaneDiff
		return tuiActDetail
	case "4", "p":
		m.Pane = tuiPanePreview
		return tuiActDetail | tuiActArtifacts | tuiActPreview
	case "5", "t":
		m.Pane = tuiPaneLog
		return tuiActDetail
	case "a":
		return tuiActDetail | tuiActAttach
	case "r":
		return tuiActRefresh | tuiActDetail | tuiActArtifacts | tuiActPreview
	}
	switch key {
	case "j", "down":
		if m.Pane == tuiPanePreview {
			if tuiArtifactKey(m, key) {
				return tuiActPreview
			}
			return 0
		}
		m.Selected++
	case "k", "up":
		if m.Pane == tuiPanePreview {
			if tuiArtifactKey(m, key) {
				return tuiActPreview
			}
			return 0
		}
		m.Selected--
	case "g", "home":
		if m.Pane == tuiPanePreview {
			m.Artifact = 0
			return tuiActPreview
		}
		m.Selected = 0
	case "G", "end":
		if m.Pane == tuiPanePreview {
			if len(m.Artifacts) > 0 {
				m.Artifact = len(m.Artifacts) - 1
			}
			return tuiActPreview
		}
		m.Selected = len(tuiRows(m.Columns)) - 1
	default:
		return 0
	}
	tuiClamp(m)
	act := tuiActDetail
	if m.Pane == tuiPanePreview {
		act |= tuiActArtifacts | tuiActPreview
	}
	return act
}

// tuiArtifactKey moves the artifact cursor inside the preview pane. It never
// leaves the pane, so a stray key can not change the cycle under review. Pure.
func tuiArtifactKey(m *tuiModel, key string) bool {
	switch key {
	case "j", "down":
		if m.Artifact < len(m.Artifacts)-1 {
			m.Artifact++
			return true
		}
	case "k", "up":
		if m.Artifact > 0 {
			m.Artifact--
			return true
		}
	}
	return false
}

// tuiHeader renders the title + pane tabs.
func tuiHeader(m tuiModel) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Konoha operator console  %d cycle(s)\n", len(m.Cycles))
	for i, p := range tuiPanes {
		mark := " "
		if p == m.Pane && !m.Help {
			mark = ">"
		}
		fmt.Fprintf(&b, "%s%d:%s ", mark, i+1, p)
	}
	b.WriteString("\n\n")
	return b.String()
}

// tuiFooter renders the interaction line: the last status/error, then the keys.
func tuiFooter(m tuiModel) string {
	var b strings.Builder
	if m.Err != "" {
		fmt.Fprintf(&b, "error: %s\n", m.Err)
	}
	if m.Status != "" {
		fmt.Fprintf(&b, "%s\n", m.Status)
	}
	b.WriteString("j/k move  enter detail  1-5 panes  a attach  ? help  r refresh  q quit\n")
	return b.String()
}

// tuiBody renders the active pane (or the help overlay). Pure.
func tuiBody(m tuiModel) string {
	if m.Help {
		return tuiHelp() + "\n"
	}
	switch m.Pane {
	case tuiPaneDetail:
		return tuiRenderDetail(m)
	case tuiPaneDiff:
		return tuiRenderDiff(m)
	case tuiPanePreview:
		return tuiRenderPreview(m)
	case tuiPaneLog:
		return tuiRenderLog(m)
	}
	return tuiRenderList(m)
}

// tuiView is the PURE full-screen projection: header + active body + footer.
// The whole frame passes through tuiSafe because agent stdout, diffs, and review
// comments are attacker-influenced: an ESC/CSI byte could erase or overwrite the
// authoritative state line already drawn.
func tuiView(m tuiModel) string {
	return tuiSafe(tuiJoin(tuiHeader(m), tuiBody(m), tuiFooter(m)))
}

// tuiJoin joins the three frame sections, ensuring the body ends with a newline.
func tuiJoin(head, body, foot string) string {
	if body != "" && !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	return head + body + foot
}

// tuiSafe drops terminal control characters from a rendered frame. Only tab and
// newline survive; ESC/CSI/OSC and other C0/C1 controls are removed, so
// untrusted evidence can never move the cursor, clear a line, or retitle the
// window. Pure.
func tuiSafe(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\n' || r == '\t':
			b.WriteRune(r)
		case r < 0x20, r == 0x7f, r >= 0x80 && r <= 0x9f,
			r == 0x200e || r == 0x200f,
			r >= 0x202a && r <= 0x202e,
			r >= 0x2066 && r <= 0x2069:
			// drop C0 (incl. ESC), DEL, C1, and Unicode bidi controls so a
			// crafted line can neither move the cursor nor reorder the frame
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// tuiRenderList renders the cycle list grouped by the board's fixed columns.
// Pure.
func tuiRenderList(m tuiModel) string {
	rows := tuiRows(m.Columns)
	if len(rows) == 0 {
		return "No cycles recorded yet.\n"
	}
	sel := tuiSelectedID(m)
	var b strings.Builder
	for _, col := range m.Columns {
		fmt.Fprintf(&b, "%s (%d)\n", strings.ToUpper(col.Title), col.Count)
		if len(col.Cards) == 0 {
			b.WriteString("  -\n")
			continue
		}
		for _, c := range col.Cards {
			mark := " "
			if c.ID == sel {
				mark = ">"
			}
			fmt.Fprintf(&b, "%s %-32s %-16s %s\n", mark, tuiClip(c.ID, 32), c.State, tuiMeta(c))
		}
	}
	return b.String()
}

// tuiMeta is the compact list-row suffix. Pure.
func tuiMeta(c CycleSummary) string {
	var parts []string
	if c.Route != "" {
		parts = append(parts, "route="+c.Route)
	}
	if c.Complexity > 0 {
		parts = append(parts, fmt.Sprintf("c%d", c.Complexity))
	}
	if c.Priority != "" {
		parts = append(parts, "priority="+c.Priority)
	}
	if c.Running {
		parts = append(parts, "running")
	}
	if !c.At.IsZero() {
		parts = append(parts, c.At.UTC().Format(time.RFC3339))
	}
	return strings.Join(parts, " ")
}

// tuiRenderDetail renders the deterministic detail facts: §6 state plus the
// separate #84 agent/verification outcomes, the outcome-conflict flag, and the
// ordered conditions. Pure.
func tuiRenderDetail(m tuiModel) string {
	d := m.Detail
	if d == nil {
		return "No cycle selected.\n"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n", d.ID)
	fmt.Fprintf(&b, "state: %s", d.State)
	if d.Running {
		b.WriteString(" (running)")
	}
	b.WriteString("\n")
	if d.Repo != "" {
		fmt.Fprintf(&b, "repo: %s/%s", d.Owner, d.Repo)
		if d.Issue > 0 {
			fmt.Fprintf(&b, "#%d", d.Issue)
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "route: %s  complexity: %d  priority: %s  assignee: %s\n",
		tuiOr(d.Route, "-"), d.Complexity, tuiOr(d.Priority, "-"), tuiOr(d.Assignee, "-"))
	agent, ver := tuiOutcomes(d)
	fmt.Fprintf(&b, "agent outcome: %s\nverification outcome: %s\n", tuiOr(string(agent), "SKIPPED"), tuiOr(string(ver), "SKIPPED"))
	if d.OutcomeConflict {
		b.WriteString("CONFLICT: agent reported SUCCESS while verification did not pass\n")
	}
	if d.Status != nil && d.Status.CurrentStep != "" {
		fmt.Fprintf(&b, "current step: %s\n", d.Status.CurrentStep)
	}
	conds := tuiConditions(d)
	if len(conds) == 0 {
		b.WriteString("conditions: none\n")
	} else {
		b.WriteString("conditions:\n")
		for _, c := range conds {
			at := "-"
			if !c.At.IsZero() {
				at = c.At.UTC().Format(time.RFC3339)
			}
			fmt.Fprintf(&b, "  %s %s %s", at, c.Type, c.Status)
			if c.Reason != "" {
				fmt.Fprintf(&b, " reason=%s", c.Reason)
			}
			if c.Message != "" {
				fmt.Fprintf(&b, " %s", c.Message)
			}
			b.WriteString("\n")
		}
	}
	fmt.Fprintf(&b, "artifacts: %d\n", len(d.Artifacts))
	if d.Gate != nil && d.Gate.Open {
		fmt.Fprintf(&b, "gate: open attempt=%d sha=%s\n", d.Gate.Attempt, shortSHA(d.Gate.SHA))
	}
	b.WriteString("attach: a (reports SKIPPED when no live session provider is wired, #136/#144)\n")
	return tuiClipLines(b.String(), tuiDiffLineCap)
}

// tuiOutcomes picks the #84 outcomes from the finished outcome or the running
// status, so a detail renders the same facts the console summarizes. Pure.
func tuiOutcomes(d *CycleDetail) (AgentOutcome, VerificationOutcome) {
	if d.Outcome != nil {
		return d.Outcome.AgentOutcome, d.Outcome.VerificationOutcome
	}
	if d.Status != nil {
		return d.Status.AgentOutcome, d.Status.VerificationOutcome
	}
	return "", ""
}

// tuiConditions returns the ordered conditions of the finished or running
// record. Pure.
func tuiConditions(d *CycleDetail) []Condition {
	if d.Outcome != nil {
		return d.Outcome.Conditions
	}
	if d.Status != nil {
		return d.Status.Conditions
	}
	return nil
}

// tuiRenderDiff renders the #106 diff projection (files + anchored threads).
// Pure.
func tuiRenderDiff(m tuiModel) string {
	d := m.Detail
	if d == nil || d.Diff == nil || len(d.Diff.Files) == 0 {
		return "No diff recorded for this cycle.\n"
	}
	var b strings.Builder
	used := 0
	for _, f := range d.Diff.Files {
		fmt.Fprintf(&b, "%s (+%d -%d)\n", f.Path, f.Added, f.Removed)
		for _, line := range f.Lines {
			if used >= tuiDiffLineCap {
				b.WriteString("… [diff truncated]\n")
				return b.String()
			}
			fmt.Fprintf(&b, "%s\n", tuiClip(line, tuiLineMaxWidth))
			used++
		}
	}
	if len(d.Diff.Threads) > 0 {
		b.WriteString("\ncomments:\n")
		for _, th := range d.Diff.Threads {
			fmt.Fprintf(&b, "  %s:%d", th.File, th.Line)
			if th.Side != "" {
				fmt.Fprintf(&b, " (%s)", th.Side)
			}
			b.WriteString("\n")
			for _, c := range th.Comments {
				fmt.Fprintf(&b, "    %s: %s\n", tuiOr(c.Author, "reviewer"), tuiClip(c.Body, tuiLineMaxWidth))
			}
		}
	}
	return b.String()
}

// tuiRenderPreview renders the #107 artifact list plus the selected artifact's
// text preview. Images/binary/oversize are listed but never previewed. Pure.
func tuiRenderPreview(m tuiModel) string {
	if m.Detail == nil {
		return "No cycle selected.\n"
	}
	if len(m.Artifacts) == 0 {
		return "No artifacts recorded for this cycle.\n"
	}
	sel := m.Artifact
	if sel < 0 || sel >= len(m.Artifacts) {
		sel = 0
	}
	var b strings.Builder
	b.WriteString("Artifacts (j/k select):\n")
	for i, a := range m.Artifacts {
		mark := " "
		if i == sel {
			mark = ">"
		}
		fmt.Fprintf(&b, "%s %-28s %-6s %8d B  %s\n", mark, tuiClip(a.Path, 28), a.Kind, a.Size, tuiOr(a.ContentType, "-"))
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "preview: %s\n", m.Artifacts[sel].Path)
	if m.PreviewFor != m.Artifacts[sel].Path {
		b.WriteString("(loading)\n")
	} else if m.PreviewMsg != "" {
		fmt.Fprintf(&b, "%s\n", m.PreviewMsg)
	} else {
		for _, line := range strings.Split(strings.TrimRight(m.Preview, "\n"), "\n") {
			b.WriteString(tuiClip(line, tuiLineMaxWidth))
			b.WriteString("\n")
		}
	}
	return b.String()
}

// tuiRenderLog renders the bounded #110 agent stdout tail. Pure.
func tuiRenderLog(m tuiModel) string {
	if m.Detail == nil {
		return "No cycle selected.\n"
	}
	if strings.TrimSpace(m.LogTail) == "" {
		return "No agent output recorded for this cycle.\n"
	}
	lines := strings.Split(strings.TrimRight(m.LogTail, "\n"), "\n")
	if len(lines) > tuiLogLineCap {
		lines = lines[len(lines)-tuiLogLineCap:]
	}
	var b strings.Builder
	for _, line := range lines {
		fmt.Fprintf(&b, "%s\n", tuiClip(line, tuiLineMaxWidth))
	}
	return b.String()
}

// tuiHelp is the key-binding overlay. Pure.
func tuiHelp() string {
	return strings.Join([]string{
		"Keys",
		"  j/k, up/down   move through the cycle list / artifact list",
		"  g/G, home/end  first / last (cycle, or artifact in the preview pane)",
		"  enter, l       open the detail pane",
		"  1              list pane",
		"  2, enter, l    detail pane",
		"  3, d           diff pane (#106)",
		"  4, p           preview/artifacts pane (#107)",
		"  5, t           agent log tail (#110)",
		"  a              attach to a live session (SKIPPED without a provider)",
		"  r              refresh from the evidence store / status query",
		"  ?              toggle this help; esc closes it",
		"  q, ctrl+c      quit",
	}, "\n") + "\n"
}

// tuiClip bounds one rendered line (terminal width is unknown; a hard cap keeps
// a hostile artifact line from wrapping the whole screen). Pure.
func tuiClip(s string, max int) string {
	if max <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

// tuiClipLines caps the number of body lines, marking the cut. Pure.
func tuiClipLines(s string, max int) string {
	lines := strings.Split(s, "\n")
	if len(lines) <= max {
		return s
	}
	return strings.Join(lines[:max], "\n") + "\n… [truncated]\n"
}

// tuiLineCount counts rendered lines (a trailing newline does not add one).
func tuiLineCount(s string) int {
	if s == "" {
		return 0
	}
	return len(strings.Split(strings.TrimSuffix(s, "\n"), "\n"))
}

// tuiLimitLines caps a body to the visible rows, marking the cut so a tall diff
// never scrolls the header/footer off the screen. Pure.
func tuiLimitLines(s string, max int) string {
	if max <= 0 || tuiLineCount(s) <= max {
		return s
	}
	lines := strings.Split(strings.TrimSuffix(s, "\n"), "\n")
	if max == 1 {
		return "… [more]\n"
	}
	return strings.Join(lines[:max-1], "\n") + "\n… [more]\n"
}

// tuiTailLines keeps the NEWEST lines when a streaming pane is taller than the
// screen (a log cut from the top would hide the output the operator is watching).
// Pure.
func tuiTailLines(s string, max int) string {
	if max <= 0 || tuiLineCount(s) <= max {
		return s
	}
	lines := strings.Split(strings.TrimSuffix(s, "\n"), "\n")
	if max == 1 {
		return "… [more]\n"
	}
	return "… [more]\n" + strings.Join(lines[len(lines)-(max-1):], "\n") + "\n"
}

// tuiOr is a small display fallback.
func tuiOr(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}

// tuiLoad refreshes the list and whatever pane data is active. All reads go
// through the console data source, so the TUI sees exactly the CLI/console view.
func tuiLoad(ctx context.Context, d *consoleData, m *tuiModel, act tuiAction) {
	if act&tuiActRefresh != 0 {
		cycles, err := d.listCycles(ctx)
		if err != nil {
			m.Err = err.Error()
		} else {
			m.Err = ""
			m.Cycles = cycles
			m.Columns = boardColumns(cycles)
			tuiClamp(m)
		}
	}
	if act&tuiActDetail != 0 {
		if len(tuiRows(m.Columns)) == 0 {
			m.Detail, m.Artifacts, m.Preview, m.PreviewFor = nil, nil, "", ""
		} else {
			id := tuiSelectedID(*m)
			det, ok, err := d.cycleDetail(ctx, id)
			switch {
			case err != nil:
				m.Err = err.Error()
				m.Detail, m.Artifacts, m.Preview, m.PreviewFor = nil, nil, "", ""
			case !ok:
				m.Err = ""
				m.Detail, m.Artifacts, m.Preview, m.PreviewFor = nil, nil, "", ""
			default:
				m.Err = ""
				m.Detail = &det
			}
		}
	}
	if act&tuiActArtifacts != 0 && m.Detail != nil {
		arts, err := d.store.artifactViews(m.Detail.ID)
		if err != nil {
			m.Err = err.Error()
		} else {
			m.Err = ""
			m.Artifacts = arts
			if m.Artifact >= len(arts) {
				m.Artifact = 0
			}
		}
	}
	if act&tuiActPreview != 0 && m.Detail != nil {
		tuiLoadPreview(d, m)
	}
	if m.Pane == tuiPaneLog && m.Detail != nil && act&(tuiActRefresh|tuiActDetail) != 0 {
		// Reuse the #110 source: the same bounded tail the SSE stream serves. Only
		// on an explicit reload — streamOutput reads the whole artifact first.
		tail, _ := d.streamOutput(m.Detail.ID, true)
		m.LogTail = tail
	}
	if act&tuiActAttach != 0 && len(tuiRows(m.Columns)) > 0 {
		id := tuiSelectedID(*m)
		res := tuiAttach(ctx, d, id)
		m.Status = fmt.Sprintf("attach: %s", res.Outcome)
		if res.Reason != "" {
			m.Status += " — " + res.Reason
		}
	}
}

// tuiLoadPreview reads the selected artifact and renders only a text preview.
// Images/binary/oversize report a note instead of bytes (never a false preview).
func tuiLoadPreview(d *consoleData, m *tuiModel) {
	if m.Artifact < 0 || m.Artifact >= len(m.Artifacts) {
		return
	}
	a := m.Artifacts[m.Artifact]
	m.PreviewFor = a.Path
	m.Preview = ""
	if _, ok := previewContentType(a.Path, a.Size); !ok {
		m.PreviewMsg = "not previewable in the terminal (download from the web console)"
		return
	}
	kind, _ := classifyArtifact(a.Path)
	if kind == artifactKindImage {
		m.PreviewMsg = "image artifact (open the web console to view it)"
		return
	}
	text, ok := d.store.readArtifact(m.Detail.ID, a.Path)
	if !ok {
		m.PreviewMsg = "artifact could not be read"
		return
	}
	m.PreviewMsg = ""
	if len(text) > tuiPreviewCap {
		text = tuiClip(text, tuiPreviewCap)
	}
	m.Preview = text
}

// tuiAttach is the attach action behind #136/#144. There is no live session
// provider wired, so it reports SKIPPED through the authoritative queue verb and
// never claims success (§40). When #136 lands, its provider plugs in here.
func tuiAttach(ctx context.Context, d *consoleData, id string) QueueActionResult {
	if d == nil || d.queue == nil {
		return QueueActionResult{Outcome: queueOutcomeSkipped, Action: queueActionAttach,
			Reason: "no live session provider (#136/#144)"}
	}
	res, err := d.submitQueueAction(ctx, id, QueueActionRequest{Action: queueActionAttach})
	if err != nil {
		return QueueActionResult{Outcome: queueOutcomeSkipped, Action: queueActionAttach, Reason: err.Error()}
	}
	return res
}

// tuiReadKey decodes one key from raw stdin: a single byte, or an ANSI escape
// sequence (arrows/home/end). A lone escape is "esc". Control bytes map to names.
func tuiReadKey(r *bufio.Reader) string {
	b, err := r.ReadByte()
	if err != nil {
		return "ctrl+c"
	}
	if b != 0x1b {
		switch b {
		case 3:
			return "ctrl+c"
		case '\r', '\n':
			return "enter"
		}
		return string(b)
	}
	if r.Buffered() == 0 {
		return "esc"
	}
	seq := []byte{b}
	// Consume a full escape sequence: after ESC, read until a CSI/SS3 final byte
	// (0x40..0x7e) so a modifier sequence (e.g. ctrl+Right `ESC [ 1 ; 5 C`) is
	// consumed whole instead of leaking `5` into the pane bindings.
	for len(seq) < 16 && r.Buffered() > 0 {
		n, err := r.ReadByte()
		if err != nil {
			break
		}
		seq = append(seq, n)
		if len(seq) >= 3 && n >= 0x40 && n <= 0x7e {
			break
		}
	}
	switch string(seq) {
	case "\x1b[A", "\x1bOA":
		return "up"
	case "\x1b[B", "\x1bOB":
		return "down"
	case "\x1b[H", "\x1bOH":
		return "home"
	case "\x1b[F", "\x1bOF":
		return "end"
	}
	return "esc"
}

// tuiPaint redraws the screen (clear + home), clipping only the body to the
// terminal height so the header and footer stay visible. height <= 0 means
// unbounded (piped/`--once` output).
func tuiPaint(w io.Writer, m tuiModel, height int) {
	fmt.Fprint(w, "\x1b[H\x1b[2J")
	if height <= 0 {
		fmt.Fprint(w, tuiView(m))
		return
	}
	head, body, foot := tuiHeader(m), tuiBody(m), tuiFooter(m)
	avail := height - 1 - tuiLineCount(head) - tuiLineCount(foot)
	if avail < 3 {
		avail = 3
	}
	if m.Pane == tuiPaneLog && !m.Help {
		body = tuiTailLines(body, avail)
	} else {
		body = tuiLimitLines(body, avail)
	}
	fmt.Fprint(w, tuiSafe(tuiJoin(head, body, foot)))
}

// tuiTermHeight reads the terminal rows, falling back to 40 when unknown.
func tuiTermHeight(fd int) int {
	if _, h, err := term.GetSize(fd); err == nil && h > 0 {
		return h
	}
	return 40
}

// runTUI is the `prbot tui` entrypoint (#143). It loads konoha.yaml itself and
// is dispatched before the worker bootstrap, so the process reads no GitHub
// credential. Without a TTY (or with --once) it renders one frame and exits,
// which keeps manual/CI checks usable; otherwise it runs the raw-mode key loop.
func runTUI(args []string) error {
	once := false
	for _, a := range args {
		switch a {
		case "--once":
			once = true
		default:
			return fmt.Errorf("usage: prbot tui [--once]")
		}
	}
	if app, err := loadAppConfig(defaultAppConfigPath()); err != nil {
		return err
	} else {
		appCfg = app
	}
	store, err := NewEvidenceStore(appCfg.Evidence, &defaultRedactor)
	if err != nil {
		return err
	}
	addr := os.Getenv("TEMPORAL_ADDRESS")
	if addr == "" {
		addr = "127.0.0.1:7233"
	}
	// Best-effort, exactly like `prbot console`: finished cycles render offline.
	ctx := context.Background()
	var cl client.Client
	if tuiTemporalClient != nil {
		if dialed, err := tuiTemporalClient(addr); err == nil {
			defer dialed.Close()
			cl = dialed
		}
	}
	d := buildConsoleData(store, consoleRepos(), cl)

	m := &tuiModel{Pane: tuiPaneList}
	tuiLoad(ctx, d, m, tuiActRefresh)

	if once || !term.IsTerminal(int(os.Stdin.Fd())) {
		fmt.Print(tuiView(*m))
		return nil
	}
	fd := int(os.Stdin.Fd())
	old, err := term.MakeRaw(fd)
	if err != nil {
		return fmt.Errorf("tui: raw terminal: %w", err)
	}
	defer func() { _ = term.Restore(fd, old) }()
	defer fmt.Print("\n")

	tuiPaint(os.Stdout, *m, tuiTermHeight(fd))
	keys := make(chan string)
	go func() {
		r := bufio.NewReader(os.Stdin)
		for {
			keys <- tuiReadKey(r)
		}
	}()
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		select {
		case key := <-keys:
			act := tuiKey(m, key)
			if act&tuiActQuit != 0 {
				return nil
			}
			tuiLoad(ctx, d, m, act)
			tuiPaint(os.Stdout, *m, tuiTermHeight(fd))
		case <-tick.C:
			// Live refresh of the authoritative state/conditions in every pane;
			// the log tail reload rides the same detail load (bounded, gated in
			// tuiLoad) rather than re-reading the artifact on each keypress.
			tuiLoad(ctx, d, m, tuiActRefresh|tuiActDetail)
			tuiPaint(os.Stdout, *m, tuiTermHeight(fd))
		}
	}
}
