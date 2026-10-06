package konoha

import (
	"math"
	"path"
	"sort"
	"strings"
	"time"
)

// Board + cycle-detail projections (#106). All functions here are PURE and
// deterministic so the view the console renders is unit-testable without a
// browser or Temporal. The console is read-only: nothing in this file mutates
// state and no artifact content is ever executed (only embedded as JSON text).
//
// "Summaries are not proof" (the review-console rule): the detail view exposes
// the deterministic §84 outcomes (agentOutcome / verificationOutcome /
// terminalState) as FIRST-CLASS, separate facts. An agent's prose is never
// presented as success, and a conflict (agent SUCCESS while verification FAILED)
// is a distinct flag the UI must surface, never collapse.

// Board column ids/titles. The five columns are fixed and always present, so a
// column count of zero is visible rather than an absent section.
const (
	boardColRunning   = "running"
	boardColAttention = "attention"
	boardColHuman     = "human"
	boardColClosed    = "closed"
	boardColFailed    = "failed"
)

// boardColumnsList is the deterministic column order.
var boardColumnsList = []struct{ ID, Title string }{
	{boardColRunning, "Running"},
	{boardColAttention, "Needs attention"},
	{boardColHuman, "Human required"},
	{boardColClosed, "Closed"},
	{boardColFailed, "Failed"},
}

// consoleArtifactCap bounds the bytes of one artifact embedded in a detail
// response, so a hostile/huge patch can never balloon the JSON payload.
const consoleArtifactCap = 256 * 1024

// evidenceComments is the optional per-cycle review-comment artifact (#113
// capture target). Absent → the diff renders with no threads. It is deliberately
// NOT part of evidenceArtifactNames (never created empty here).
const evidenceComments = "comments.json"

// BoardColumn is one board column with its cards, newest first.
type BoardColumn struct {
	ID    string         `json:"id"`
	Title string         `json:"title"`
	Count int            `json:"count"`
	Cards []CycleSummary `json:"cards"`
}

// boardColumnFor maps an observed §6 state (+ the running flag) to exactly one
// board column. Terminal states are attributed by their authority; an unknown
// state that is not reported running is surfaced under Failed rather than hidden.
func boardColumnFor(state string, running bool) string {
	switch state {
	case stateClosed:
		return boardColClosed
	case stateFailed, stateCancelled:
		return boardColFailed
	case stateHumanRequired:
		return boardColHuman
	case stateBlocked, statePendingApproval:
		return boardColAttention
	}
	if running {
		return boardColRunning
	}
	return boardColFailed
}

// boardColumns groups cycles into the five fixed columns. Cards within a column
// are ordered newest-first (by the latest condition timestamp, ties broken by a
// descending id) so the ordering is deterministic. Pure.
func boardColumns(cycles []CycleSummary) []BoardColumn {
	byCol := map[string][]CycleSummary{}
	for _, c := range cycles {
		col := boardColumnFor(c.State, c.Running)
		byCol[col] = append(byCol[col], c)
	}
	out := make([]BoardColumn, 0, len(boardColumnsList))
	for _, col := range boardColumnsList {
		cards := byCol[col.ID]
		sort.SliceStable(cards, func(i, j int) bool {
			if !cards[i].At.Equal(cards[j].At) {
				return cards[i].At.After(cards[j].At)
			}
			return cards[i].ID > cards[j].ID
		})
		if cards == nil {
			cards = []CycleSummary{}
		}
		out = append(out, BoardColumn{ID: col.ID, Title: col.Title, Count: len(cards), Cards: cards})
	}
	return out
}

// outcomesConflict reports the canonical mismatch the console must never
// collapse: the agent reported SUCCESS but deterministic verification did not
// pass (FAILED/ERROR).
func outcomesConflict(a AgentOutcome, v VerificationOutcome) bool {
	return a == agentOutcomeSuccess && (v == verificationFailed || v == verificationError)
}

// latestConditionTime returns the most recent condition timestamp (zero when
// there are none), used for deterministic newest-first board ordering.
func latestConditionTime(conds []Condition) time.Time {
	var t time.Time
	for _, c := range conds {
		if c.At.After(t) {
			t = c.At
		}
	}
	return t
}

// DiffFile is one file in a parsed unified diff, with its patch lines and
// added/removed counts (header lines excluded).
type DiffFile struct {
	Path    string   `json:"path"`
	Added   int      `json:"added"`
	Removed int      `json:"removed"`
	Lines   []string `json:"lines"`
}

// DiffThread is the comment thread anchored to one file+line(+side).
type DiffThread struct {
	File     string        `json:"file"`
	Line     int           `json:"line"`
	Side     string        `json:"side,omitempty"`
	Comments []DiffComment `json:"comments"`
}

// DiffView is the Diff tab projection: the parsed patch plus anchored threads
// (threads render on the diff; empty when no comments are recorded).
type DiffView struct {
	Files   []DiffFile   `json:"files"`
	Threads []DiffThread `json:"threads"`
}

// DiffComment is one anchored review comment. The shape matches the #113
// comment model ({id, cycleId, attempt, sha, file, line, side, body, author,
// createdAt}); #106 only RENDERS whatever is recorded — capture/storage is #113.
type DiffComment struct {
	ID        string    `json:"id"`
	CycleID   string    `json:"cycleId,omitempty"`
	Attempt   int       `json:"attempt,omitempty"`
	SHA       string    `json:"sha,omitempty"`
	File      string    `json:"file"`
	Line      int       `json:"line"`
	Side      string    `json:"side,omitempty"`
	Body      string    `json:"body"`
	Author    string    `json:"author,omitempty"`
	CreatedAt time.Time `json:"createdAt,omitempty"`
}

// diffThreads groups comments into per-line threads. Deterministic: threads are
// sorted by file, then line, then side; comments keep their input order so a
// recorded thread renders in submission order. Pure.
func diffThreads(comments []DiffComment) []DiffThread {
	type key struct {
		file string
		line int
		side string
	}
	order := []key{}
	byKey := map[key][]DiffComment{}
	for _, c := range comments {
		k := key{file: c.File, line: c.Line, side: c.Side}
		if _, ok := byKey[k]; !ok {
			order = append(order, k)
		}
		byKey[k] = append(byKey[k], c)
	}
	out := make([]DiffThread, 0, len(order))
	for _, k := range order {
		out = append(out, DiffThread{File: k.file, Line: k.line, Side: k.side, Comments: byKey[k]})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].File != out[j].File {
			return out[i].File < out[j].File
		}
		if out[i].Line != out[j].Line {
			return out[i].Line < out[j].Line
		}
		return out[i].Side < out[j].Side
	})
	return out
}

// parseUnifiedDiff splits a `git diff` patch into files. It is tolerant: a file
// header (`diff --git a/x b/x`) starts a file; `+++ b/x` refines the path; `+`/`-`
// hunk lines are counted (header lines are not). Pure.
func parseUnifiedDiff(patch string) []DiffFile {
	var files []DiffFile
	cur := -1
	for _, line := range strings.Split(patch, "\n") {
		switch {
		case strings.HasPrefix(line, "diff --git "):
			files = append(files, DiffFile{Path: diffHeaderPath(line), Lines: []string{}})
			cur = len(files) - 1
		case cur < 0:
			continue
		case strings.HasPrefix(line, "+++ "):
			if p := diffFilePath(line); p != "" {
				files[cur].Path = p
			}
		case strings.HasPrefix(line, "--- "):
			// old-file header: never counted as a removed line
		case strings.HasPrefix(line, "@@"):
			files[cur].Lines = append(files[cur].Lines, line)
		case strings.HasPrefix(line, "+"):
			files[cur].Added++
			files[cur].Lines = append(files[cur].Lines, line)
		case strings.HasPrefix(line, "-"):
			files[cur].Removed++
			files[cur].Lines = append(files[cur].Lines, line)
		default:
			files[cur].Lines = append(files[cur].Lines, line)
		}
	}
	if files == nil {
		files = []DiffFile{}
	}
	return files
}

// diffHeaderPath extracts the new path from a `diff --git a/x b/x` header.
func diffHeaderPath(line string) string {
	fields := strings.Fields(strings.TrimPrefix(line, "diff --git "))
	if len(fields) < 2 {
		return ""
	}
	return strings.TrimPrefix(fields[len(fields)-1], "b/")
}

// diffFilePath extracts the path from a `+++ b/x` (or `+++ x`) line, dropping a
// trailing tab-separated timestamp and the `/dev/null` sentinel.
func diffFilePath(line string) string {
	p := strings.TrimSpace(strings.TrimPrefix(line, "+++ "))
	if i := strings.IndexByte(p, '\t'); i >= 0 {
		p = p[:i]
	}
	if p == "" || p == "/dev/null" {
		return ""
	}
	return strings.TrimPrefix(strings.TrimPrefix(p, "b/"), "a/")
}

// ValidationGateView is one §26 gate's captured streams (from evidence).
type ValidationGateView struct {
	ID     string `json:"id"`
	Stdout string `json:"stdout,omitempty"`
	Stderr string `json:"stderr,omitempty"`
}

// ValidationView is the Validation tab projection: the deterministic overall
// verification outcome plus the legacy combined output and each gate's streams.
type ValidationView struct {
	Outcome VerificationOutcome  `json:"outcome,omitempty"`
	Output  string               `json:"output,omitempty"`
	Gates   []ValidationGateView `json:"gates,omitempty"`
}

// readArtifact reads one store-relative artifact for a cycle; ok=false on any
// read error (missing is not fatal). Callers cap the returned size.
func (s *EvidenceStore) readArtifact(id, name string) (string, bool) {
	rel, err := s.relDirFor(id, false)
	if err != nil {
		return "", false
	}
	b, err := s.sink.Read(path.Join(rel, name))
	if err != nil {
		return "", false
	}
	return string(b), true
}

// truncateConsole bounds embedded artifact content, appending a marker so a
// truncated view is visibly truncated (never a silent cut).
func truncateConsole(s string) string {
	if len(s) <= consoleArtifactCap {
		return s
	}
	return s[:consoleArtifactCap] + "\n… [truncated]"
}

// loadDiffView builds the Diff tab from the recorded patch + comments. A missing
// or empty patch yields nil (the tab is hidden).
func (d *consoleData) loadDiffView(id string) *DiffView {
	patch, ok := d.store.readArtifact(id, evidenceDiff)
	if !ok || strings.TrimSpace(patch) == "" {
		return nil
	}
	files := parseUnifiedDiff(truncateConsole(patch))
	comments := d.store.readComments(id)
	return &DiffView{Files: files, Threads: diffThreads(comments)}
}

// loadValidationView builds the Validation tab: the deterministic outcome, the
// legacy combined validation.txt, and per-gate streams under verification/.
// Returns nil when no validation evidence exists at all.
func (d *consoleData) loadValidationView(id string, out VerificationOutcome) *ValidationView {
	v := &ValidationView{Outcome: out}
	if txt, ok := d.store.readArtifact(id, evidenceValidation); ok && strings.TrimSpace(txt) != "" {
		v.Output = truncateConsole(txt)
	}
	names, _ := d.store.listArtifacts(id)
	gates := map[string]*ValidationGateView{}
	var order []string
	for _, n := range names {
		base, ok := strings.CutPrefix(n, "verification/")
		if !ok {
			continue
		}
		ext := path.Ext(base)
		gate := strings.TrimSuffix(base, ext)
		if gate == "" || (ext != ".stdout" && ext != ".stderr") {
			continue
		}
		g := gates[gate]
		if g == nil {
			g = &ValidationGateView{ID: gate}
			gates[gate] = g
			order = append(order, gate)
		}
		if c, ok := d.store.readArtifact(id, n); ok {
			if ext == ".stdout" {
				g.Stdout = truncateConsole(c)
			} else {
				g.Stderr = truncateConsole(c)
			}
		}
	}
	sort.Strings(order)
	for _, id := range order {
		v.Gates = append(v.Gates, *gates[id])
	}
	if v.Output == "" && len(v.Gates) == 0 && out == "" {
		return nil
	}
	return v
}

// MetricView is one metrics-dashboard tile (#112). Value is nil when no data has
// been observed — the dashboard renders "no data", NEVER a fabricated zero
// (the §87 rule: a missing number is better than an invented one).
type MetricView struct {
	Key   string   `json:"key"`
	Label string   `json:"label"`
	Value *float64 `json:"value"` // null = no data
	Unit  string   `json:"unit,omitempty"`
}

// MetricsView is the read-only console metrics dashboard (#112): the existing
// §39 rate gauges plus throughput and #87 cost/token figures. It is a PURE
// projection over a registry snapshot + the recorded cycle outcomes — no new
// instrumentation and nothing here mutates state.
type MetricsView struct {
	Metrics    []MetricView `json:"metrics"`
	Completed  int          `json:"completed"`
	WindowDays int          `json:"windowDays,omitempty"`
}

// floatPtr is a small helper so an "optional value" can be built inline.
func floatPtr(v float64) *float64 { return &v }

// buildMetricsView projects the dashboard from a §39 registry snapshot (may be
// nil/empty, e.g. a standalone console with no worker) and the evidence
// outcomes. Registry gauges/counters take precedence when present; otherwise the
// same figures are derived from the durable outcomes so the view still works
// offline. Every figure degrades to "no data" (nil) rather than zero.
func buildMetricsView(snapshot map[string]float64, outcomes []CycleOutcome) MetricsView {
	var (
		closed, autonomous                  int
		small, large, rounds, disagreements int
		humanEsc                            int
	)
	var times []time.Time
	var evTokensIn, evTokensOut int64
	var evCost float64
	for _, o := range outcomes {
		switch o.Route {
		case stateSmall:
			small++
		case stateLarge:
			large++
		}
		rounds += o.ReviewRounds
		disagreements += o.Disagreements
		if o.HumanRequired || o.EscalationReason != "" {
			humanEsc++
		}
		if o.TerminalState == stateClosed {
			closed++
			if o.AutonomousClose {
				autonomous++
			}
			if t := latestConditionTime(o.Conditions); !t.IsZero() {
				times = append(times, t)
			}
		}
		if o.Usage.TokensIn > 0 {
			evTokensIn += o.Usage.TokensIn
		}
		if o.Usage.TokensOut > 0 {
			evTokensOut += o.Usage.TokensOut
		}
		if o.Usage.CostUSD > 0 {
			evCost += o.Usage.CostUSD
		}
	}

	// registryValue prefers the live registry figure when the name was set, else
	// the evidence-derived fallback (nil = no data).
	registryValue := func(name string, fallback *float64) *float64 {
		if v, ok := snapshot[name]; ok {
			return floatPtr(v)
		}
		return fallback
	}
	ratio := func(num, denom int) *float64 {
		if denom <= 0 {
			return nil
		}
		return floatPtr(float64(num) / float64(denom))
	}

	completed := closed
	if v, ok := snapshot[mCyclesCompleted]; ok {
		completed = int(v)
	}
	throughput, windowDays := throughputPerDay(times)

	// #87 counters: registry whenever reported, else the summed evidence usage.
	counter := func(name string, evidence float64) (float64, bool) {
		if v, ok := snapshot[name]; ok {
			return v, true
		}
		if evidence > 0 {
			return evidence, true
		}
		return 0, false
	}
	tokensIn, tokensInOK := counter(mAgentTokensIn, float64(evTokensIn))
	tokensOut, tokensOutOK := counter(mAgentTokensOut, float64(evTokensOut))
	cost, costOK := counter(mAgentCostUSD, evCost)
	// A per-issue figure exists only when the total was reported AND something
	// completed. A total of zero with completes is "no data", never a 0 tile.
	perIssue := func(total float64, ok bool) *float64 {
		if !ok || completed <= 0 {
			return nil
		}
		return floatPtr(total / float64(completed))
	}

	mv := MetricsView{Completed: completed, WindowDays: windowDays}
	add := func(key, label, unit string, v *float64) {
		mv.Metrics = append(mv.Metrics, MetricView{Key: key, Label: label, Value: v, Unit: unit})
	}
	add(mAutonomousRate, "Autonomous completion rate", "ratio",
		registryValue(mAutonomousRate, ratio(autonomous, closed)))
	add(mDidHumanEscalationRate, "Human escalation rate", "ratio",
		registryValue(mDidHumanEscalationRate, ratio(humanEsc, len(outcomes))))
	add(mSplitRate, "Split rate", "ratio",
		registryValue(mSplitRate, ratio(large, small+large)))
	add(mReviewDisagreementRate, "Review disagreement rate", "ratio",
		registryValue(mReviewDisagreementRate, ratio(disagreements, rounds)))
	add("throughput_per_day", "Throughput", "cycles/day", throughput)
	add(mCostPerCompletedIssue, "Cost per completed issue", "usd", perIssue(cost, costOK))
	add("tokens_in_per_completed_issue", "Tokens in per completed issue", "tokens", perIssue(tokensIn, tokensInOK))
	add("tokens_out_per_completed_issue", "Tokens out per completed issue", "tokens", perIssue(tokensOut, tokensOutOK))
	add(mAgentTokensIn, "Total tokens in", "tokens", optionalValue(tokensIn, tokensInOK))
	add(mAgentTokensOut, "Total tokens out", "tokens", optionalValue(tokensOut, tokensOutOK))
	add(mAgentCostUSD, "Total agent cost", "usd", optionalValue(cost, costOK))
	return mv
}

// optionalValue returns a pointer only when the figure was reported.
func optionalValue(v float64, ok bool) *float64 {
	if !ok {
		return nil
	}
	return floatPtr(v)
}

// throughputPerDay is the completed-cycles/day average over the observed window
// (min one day), and the window in whole days. No timestamps → no data.
func throughputPerDay(times []time.Time) (*float64, int) {
	if len(times) == 0 {
		return nil, 0
	}
	earliest, latest := times[0], times[0]
	for _, t := range times[1:] {
		if t.Before(earliest) {
			earliest = t
		}
		if t.After(latest) {
			latest = t
		}
	}
	days := int(math.Ceil(latest.Sub(earliest).Hours() / 24))
	if days < 1 {
		days = 1
	}
	return floatPtr(float64(len(times)) / float64(days)), days
}
