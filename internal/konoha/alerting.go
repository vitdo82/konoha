package konoha

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"go.temporal.io/sdk/workflow"
)

// §53 operations/alerting layer, on top of the §39 metrics sink. Two jobs:
//
//  1. Rate-drift alerts — a schedule-driven AlertWorkflow reads the derived §39
//     gauges (autonomous completion rate, human escalation rate, ...) and emits
//     a structured alert (log + optional `notify` webhook) when a threshold is
//     crossed. Emission is idempotent per (repo, metric, window): a Temporal
//     retry or a duplicate schedule run in the same window never double-alerts.
//  2. Failure feedback — a terminal BLOCKED/FAILED cycle files at most one bug
//     issue per run (marker `<!-- konoha-failure: <runId> -->`), carrying the
//     structured failure, so the pipeline debugs itself in issue form. The filed
//     issue is a NORMAL issue (no `konoha:managed` label) and flows back through
//     the lifecycle via the existing sweep.
//
// Alerting NEVER resolves a stuck run: the alerts report drift, and the
// underlying run keeps its authoritative Temporal state (§40/§44 #1). Everything
// is opt-in per repo (`alerting.enabled`) and honors `DryRun` (§42).

// AlertingConfig is the per-repo §53 policy (repos.yaml `lifecycle.alerting`).
// Thresholds maps an alertable §39 metric name to its bound; OnRates optionally
// narrows evaluation to a subset. Direction is intrinsic to the metric (see
// alertableMetrics), so no operator parsing is needed.
type AlertingConfig struct {
	Enabled    bool               `yaml:"enabled" json:"enabled,omitempty"`
	Notify     string             `yaml:"notify" json:"notify,omitempty"`   // legacy single webhook URL (slack-compatible); empty = log only
	Targets    []NotifyTarget     `yaml:"targets" json:"targets,omitempty"` // #123 named targets; multiple allowed
	OnRates    []string           `yaml:"onRates" json:"onRates,omitempty"` // optional subset of alertable metrics
	Thresholds map[string]float64 `yaml:"thresholds" json:"thresholds,omitempty"`
	Schedule   string             `yaml:"schedule" json:"schedule,omitempty"` // evaluation cadence (Go duration)
}

const defaultAlertingSchedule = "24h"

// alertableMetrics maps each alertable §39 metric to its direction: true = alert
// when the value drops BELOW the threshold, false = alert when it rises ABOVE.
// The set is closed (a threshold on an unknown name fails config validation) so
// a typo can never silently disable an alert.
var alertableMetrics = map[string]bool{
	mAutonomousRate:         true,  // higher is better
	mDidHumanEscalationRate: false, // lower is better
	mSplitRate:              false,
	mReviewDisagreementRate: false,
	mCycleDurationLast:      false, // merge/cycle latency proxy (seconds)
}

// AlertingEnabled reports whether the repo opted into §53 alerting + failure
// feedback. Absent/false is a no-op everywhere.
func AlertingEnabled(lc LifecycleConfig) bool { return lc.Alerting.Enabled }

// AlertingOnRates returns the optional metric subset to evaluate.
func AlertingOnRates(lc LifecycleConfig) []string { return lc.Alerting.OnRates }

// AlertingThresholds returns the metric→bound map.
func AlertingThresholds(lc LifecycleConfig) map[string]float64 { return lc.Alerting.Thresholds }

// AlertingSchedule resolves the per-repo evaluation cadence (Go duration).
func AlertingSchedule(lc LifecycleConfig) string {
	if s := strings.TrimSpace(lc.Alerting.Schedule); s != "" {
		return s
	}
	return defaultAlertingSchedule
}

// alertInterval parses AlertingSchedule, falling back to 24h.
func alertInterval(lc LifecycleConfig) time.Duration {
	if d, err := time.ParseDuration(AlertingSchedule(lc)); err == nil && d > 0 {
		return d
	}
	return 24 * time.Hour
}

// validate gates the §53 alerting block: an enabled block needs at least one
// alertable threshold (otherwise it would be a silent no-op), every threshold
// name is in the closed alertable set with a sane value, onRates names are
// valid, notify (when set) is an http(s) URL, and the schedule is positive.
func (ac AlertingConfig) validate(prefix string) error {
	if ac.Schedule != "" {
		if d, err := time.ParseDuration(ac.Schedule); err != nil || d <= 0 {
			return fmt.Errorf("%s.schedule: %q is not a positive duration", prefix, ac.Schedule)
		}
	}
	for _, name := range ac.OnRates {
		if _, ok := alertableMetrics[name]; !ok {
			return fmt.Errorf("%s.onRates: %q is not an alertable metric", prefix, name)
		}
	}
	if ac.Notify != "" {
		if err := validateNotifyURL(ac.Notify); err != nil {
			return fmt.Errorf("%s.notify: %w", prefix, err)
		}
	}
	for i, t := range ac.Targets {
		if err := t.validate(fmt.Sprintf("%s.targets[%d]", prefix, i)); err != nil {
			return err
		}
	}
	for name, thr := range ac.Thresholds {
		below, ok := alertableMetrics[name]
		if !ok {
			return fmt.Errorf("%s.thresholds: %q is not an alertable metric", prefix, name)
		}
		if thr < 0 {
			return fmt.Errorf("%s.thresholds.%s: %v must be >= 0", prefix, name, thr)
		}
		if below && thr > 1 { // autonomous_completion_rate is a ratio in [0,1]
			return fmt.Errorf("%s.thresholds.%s: %v must be within 0-1", prefix, name, thr)
		}
	}
	if ac.Enabled && len(ac.Thresholds) == 0 {
		return fmt.Errorf("%s.thresholds: at least one threshold is required when enabled", prefix)
	}
	return nil
}

// RateAlert is one threshold violation.
type RateAlert struct {
	Metric    string  `json:"metric"`
	Value     float64 `json:"value"`
	Threshold float64 `json:"threshold"`
	Direction string  `json:"direction"` // "below" | "above"
	Window    string  `json:"window"`
}

// AlertRequest parameterizes EvaluateAlerts.
type AlertRequest struct {
	Owner      string             `json:"owner"`
	Repo       string             `json:"repo"`
	Window     string             `json:"window"` // deterministic evaluation window
	Targets    []NotifyTarget     `json:"targets,omitempty"`
	OnRates    []string           `json:"onRates,omitempty"`
	Thresholds map[string]float64 `json:"thresholds"`
	DryRun     bool               `json:"dryRun,omitempty"`
}

// AlertResult reports what was emitted vs. suppressed as already-seen.
type AlertResult struct {
	Emitted []RateAlert `json:"emitted,omitempty"`
	Deduped []RateAlert `json:"deduped,omitempty"`
}

// EvaluateRateAlerts is the PURE threshold math: given a metric snapshot it
// returns the violations, one per metric, sorted by name. onRates (when
// non-empty) narrows the set. No I/O and no dedupe — the caller supplies the
// snapshot and window, which keeps the decision unit-testable and deterministic.
func EvaluateRateAlerts(thresholds map[string]float64, onRates []string, values map[string]float64, window string) []RateAlert {
	subset := map[string]bool{}
	for _, n := range onRates {
		subset[n] = true
	}
	var out []RateAlert
	for name, thr := range thresholds {
		below, ok := alertableMetrics[name]
		if !ok {
			continue
		}
		if len(onRates) > 0 && !subset[name] {
			continue
		}
		v := values[name]
		if !(below && v < thr) && !(!below && v > thr) {
			continue
		}
		dir := "above"
		if below {
			dir = "below"
		}
		out = append(out, RateAlert{Metric: name, Value: v, Threshold: thr, Direction: dir, Window: window})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Metric < out[j].Metric })
	return out
}

// emittedAlerts dedupes alert emission per (repo, metric, window) so a Temporal
// retry or a duplicate schedule run in the same window alerts exactly once.
// Process-local: single-worker deployments converge; a multi-worker deployment
// would need a durable marker (out of scope while one worker per queue is the
// deployment model).
var (
	emittedAlertsMu sync.Mutex
	emittedAlerts   = map[string]bool{}
)

func alertKey(owner, repo, metric, window string) string {
	return owner + "/" + repo + "|" + metric + "|" + window
}

func alertSeen(key string) bool {
	emittedAlertsMu.Lock()
	defer emittedAlertsMu.Unlock()
	return emittedAlerts[key]
}

func markAlertSeen(key string) {
	emittedAlertsMu.Lock()
	defer emittedAlertsMu.Unlock()
	emittedAlerts[key] = true
}

// resetEmittedAlerts clears the dedupe set (tests).
func resetEmittedAlerts() {
	emittedAlertsMu.Lock()
	defer emittedAlertsMu.Unlock()
	emittedAlerts = map[string]bool{}
}

// MetricValue returns the current §39 metric value (gauge or counter).
func MetricValue(name string) float64 { return sink.get(name) }

// EvaluateAlerts is the §39-reading activity: snapshot the alertable gauges,
// compute the violations, and emit each unseen one (log + optional webhook).
// Idempotent under retry: a (repo, metric, window) already emitted is skipped.
// A webhook failure is returned (fail-closed) and the key is NOT recorded, so
// the retry re-attempts the emission rather than silently dropping it.
func EvaluateAlerts(ctx context.Context, in AlertRequest) (AlertResult, error) {
	values := make(map[string]float64, len(alertableMetrics))
	for name := range alertableMetrics {
		values[name] = MetricValue(name)
	}
	alerts := EvaluateRateAlerts(in.Thresholds, in.OnRates, values, in.Window)

	var res AlertResult
	for _, a := range alerts {
		key := alertKey(in.Owner, in.Repo, a.Metric, a.Window)
		if alertSeen(key) {
			res.Deduped = append(res.Deduped, a)
			continue
		}
		if err := emitRateAlert(ctx, in, a); err != nil {
			return res, err
		}
		markAlertSeen(key)
		res.Emitted = append(res.Emitted, a)
	}
	return res, nil
}

// emitRateAlert logs the alert (§53 alerts are always logged) and fans it out
// to the configured targets. A delivery failure is returned so EvaluateAlerts'
// retry re-attempts it; the alert key is only recorded on success, and
// per-target dedupe stops a retry double-notifying.
func emitRateAlert(ctx context.Context, in AlertRequest, a RateAlert) error {
	msg := fmt.Sprintf("konoha alert [%s/%s] %s %s threshold: value=%.3f threshold=%.3f window=%s",
		in.Owner, in.Repo, a.Metric, a.Direction, a.Value, a.Threshold, a.Window)
	log.Print(msg)

	if len(in.Targets) == 0 {
		return nil
	}
	return deliverEvent(ctx, in.Targets, NotifyEvent{
		Kind: notifyKindAlert, Owner: in.Owner, Repo: in.Repo, Text: msg,
		DedupeKey: alertKey(in.Owner, in.Repo, a.Metric, a.Window),
	}, in.DryRun)
}

// alertWindow buckets now into the evaluation interval so retries in the same
// window share one dedupe key (§24-style determinism). Pure and replay-safe.
func alertWindow(now time.Time, interval time.Duration) string {
	if interval <= 0 {
		interval = 24 * time.Hour
	}
	return now.UTC().Truncate(interval).Format(time.RFC3339)
}

// AlertWorkflow is the schedule-driven §53 entrypoint: evaluate the §39 rate
// gauges against the repo thresholds and emit one alert per violated metric per
// window. Disabled config is a no-op. It never mutates a run's state.
func AlertWorkflow(ctx workflow.Context, scope RepoScope) error {
	lc := scope.Lifecycle
	if !AlertingEnabled(lc) {
		return nil
	}
	ctx = workflow.WithActivityOptions(ctx, activityOptionsFor(retryInfrastructure, appCfg.Worker.startToClose()))

	window := alertWindow(workflow.Now(ctx), alertInterval(lc))
	var res AlertResult
	if err := workflow.ExecuteActivity(ctx, EvaluateAlerts, AlertRequest{
		Owner: scope.Owner, Repo: scope.Repo,
		Window: window, Targets: AlertingTargets(lc),
		OnRates: AlertingOnRates(lc), Thresholds: AlertingThresholds(lc),
		DryRun: lc.DryRun,
	}).Get(ctx, &res); err != nil {
		return err
	}
	workflow.GetLogger(ctx).Info("rate alerts evaluated",
		"repo", scope.Owner+"/"+scope.Repo, "window", window,
		"emitted", len(res.Emitted), "deduped", len(res.Deduped))
	return nil
}

// --- failure feedback --------------------------------------------------------

// failureMarkerOpen is the §24 idempotency marker for a failure-feedback issue:
// `<!-- konoha-failure: <runId> -->`, matched byte-exactly.
const failureMarkerOpen = "<!-- konoha-failure: "

func failureMarker(runID string) string { return failureMarkerOpen + runID + " -->" }

// FailureFeedbackRequest parameterizes EnsureFailureIssue / FailureFeedbackWorkflow.
type FailureFeedbackRequest struct {
	Owner    string             `json:"owner"`
	Repo     string             `json:"repo"`
	RepoPath string             `json:"repoPath,omitempty"`
	IssueNum int                `json:"issue"`
	Outcome  CycleOutcome       `json:"outcome"`
	Failure  DevelopmentFailure `json:"failure"`
	Targets  []NotifyTarget     `json:"targets,omitempty"` // #123 notification targets for the filing event
	DryRun   bool               `json:"dryRun,omitempty"`
}

// FailureIssueResult reports the filed (or matched) bug issue. Number is 0 under
// DryRun.
type FailureIssueResult struct {
	Number  int    `json:"number,omitempty"`
	Created bool   `json:"created,omitempty"`
	Marker  string `json:"marker,omitempty"`
}

// EnsureFailureIssue files one bug issue per failed/blocked run, idempotent under
// retry (§24) by scanning for the deterministic marker first. The issue carries
// NO `konoha:managed` label, so it is an ordinary issue: the existing sweep (and
// the `opened` webhook) drives it back through the normal lifecycle — a
// self-healing stream, never a Konoha-started cycle (no double-start).
func EnsureFailureIssue(ctx context.Context, in FailureFeedbackRequest) (FailureIssueResult, error) {
	if in.DryRun {
		return FailureIssueResult{}, nil // §42: shadow mode mutates nothing
	}
	runID := strings.TrimSpace(in.Outcome.WorkflowID)
	if runID == "" {
		return FailureIssueResult{}, fmt.Errorf("EnsureFailureIssue: outcome.workflowId is required")
	}
	if in.Owner == "" || in.Repo == "" {
		return FailureIssueResult{}, fmt.Errorf("EnsureFailureIssue: owner and repo are required")
	}
	marker := failureMarker(runID)
	items, err := listIssues(ctx, in.Owner, in.Repo)
	if err != nil {
		return FailureIssueResult{}, err
	}
	if n := findMarkerIssue(items, marker); n != 0 {
		return FailureIssueResult{Number: n, Marker: marker}, nil
	}
	out, err := ghRunner(ctx, "issue", "create", "-R", in.Owner+"/"+in.Repo,
		"--title", failureIssueTitle(in.Outcome), "--body", failureIssueBody(in, marker))
	if err != nil {
		return FailureIssueResult{}, fmt.Errorf("gh issue create (failure): %s: %w", strings.TrimSpace(string(out)), err)
	}
	n, err := parseIssueNumber(string(out))
	if err != nil {
		return FailureIssueResult{}, err
	}
	return FailureIssueResult{Number: n, Created: true, Marker: marker}, nil
}

// failureIssueTitle renders a deterministic per-run title.
func failureIssueTitle(o CycleOutcome) string {
	state := o.TerminalState
	if state == "" {
		state = stateFailed
	}
	return fmt.Sprintf("Pipeline failure: %s (%s)", o.WorkflowID, state)
}

// failureIssueBody renders the marker first, then the structured run facts and
// the DevelopmentFailure. The issue is intentionally unlabeled so the normal
// issue sweep picks it up.
func failureIssueBody(in FailureFeedbackRequest, marker string) string {
	o := in.Outcome
	var b strings.Builder
	b.WriteString(marker + "\n\n")
	fmt.Fprintf(&b, "Konoha run `%s` ended in **%s** for issue #%d.\n\n", o.WorkflowID, o.TerminalState, o.Issue)
	if o.Route != "" {
		fmt.Fprintf(&b, "- Route: `%s`\n", o.Route)
	}
	if o.EscalationReason != "" {
		fmt.Fprintf(&b, "- Reason: `%s`\n", o.EscalationReason)
	}
	fmt.Fprintf(&b, "- Attempts: %d\n", o.Attempts)
	fmt.Fprintf(&b, "- Review rounds: %d\n", o.ReviewRounds)
	if o.Disagreements > 0 {
		fmt.Fprintf(&b, "- Disagreements: %d\n", o.Disagreements)
	}
	if o.Complexity > 0 {
		fmt.Fprintf(&b, "- Complexity: %d\n", o.Complexity)
	}
	if o.Confidence != "" {
		fmt.Fprintf(&b, "- Confidence: %s\n", o.Confidence)
	}
	if o.DurationSeconds > 0 {
		fmt.Fprintf(&b, "- Duration: %.0fs\n", o.DurationSeconds)
	}
	b.WriteString("\n## Structured failure\n\n")
	if fjs, err := json.MarshalIndent(in.Failure, "", "  "); err == nil {
		b.WriteString("```json\n")
		b.Write(fjs)
		b.WriteString("\n```\n")
	}
	b.WriteString("\nFiled by Konoha alerting (§53). This is a normal issue: the existing sweep picks it up and runs it through the lifecycle.")
	b.WriteString("\n")
	return b.String()
}

// FailureFeedbackWorkflow is the §46/§53 terminal hook: a BLOCKED/FAILED run
// files at most one bug issue carrying the structured failure. The workflow's
// I/O is the EnsureFailureIssue activity plus, when a target is configured, the
// #123 NotifyCycleEvent delivery for the filing.
func FailureFeedbackWorkflow(ctx workflow.Context, in FailureFeedbackRequest) error {
	ctx = workflow.WithActivityOptions(ctx, activityOptionsFor(retryInfrastructure, appCfg.Worker.startToClose()))

	var res FailureIssueResult
	if err := workflow.ExecuteActivity(ctx, EnsureFailureIssue, in).Get(ctx, &res); err != nil {
		return err
	}
	workflow.GetLogger(ctx).Info("failure feedback filed",
		"run", in.Outcome.WorkflowID, "issue", res.Number, "created", res.Created)

	if len(in.Targets) > 0 {
		text := fmt.Sprintf("cycle %s ended %s: failure feedback filed (issue #%d)",
			in.Outcome.WorkflowID, in.Outcome.TerminalState, res.Number)
		if err := workflow.ExecuteActivity(ctx, NotifyCycleEvent, NotifyCycleRequest{
			Kind: notifyKindFailure, Owner: in.Owner, Repo: in.Repo, Text: text,
			DedupeKey: in.Outcome.WorkflowID + "|" + notifyKindFailure,
			Targets:   in.Targets, DryRun: in.DryRun,
		}).Get(ctx, nil); err != nil {
			return err
		}
	}
	return nil
}

// notifyTerminal fires a #123 terminal-outcome notification when the repo
// configured a target. It runs in workflow code (deterministic); the delivery is
// I/O in the NotifyCycleEvent activity, retried per the infrastructure policy,
// and a delivery problem is only logged — it never changes the terminal state
// (§40). The dedupe key (run + state) means a replay/retry never double-notifies.
func notifyTerminal(ctx workflow.Context, in IssueInput, out CycleOutcome) {
	targets := AlertingTargets(in.Lifecycle)
	if len(targets) == 0 {
		return
	}
	runID := out.WorkflowID
	if runID == "" {
		runID = workflow.GetInfo(ctx).WorkflowExecution.ID
	}
	nctx := workflow.WithActivityOptions(ctx, activityOptionsFor(retryInfrastructure, appCfg.Worker.startToClose()))
	err := workflow.ExecuteActivity(nctx, NotifyCycleEvent, NotifyCycleRequest{
		Kind: notifyKindTerminal, Owner: in.Owner, Repo: in.Repo,
		Text:      fmt.Sprintf("cycle %s ended %s", runID, out.TerminalState),
		DedupeKey: runID + "|" + out.TerminalState,
		Targets:   targets, DryRun: in.Lifecycle.DryRun,
	}).Get(nctx, nil)
	if err != nil {
		workflow.GetLogger(ctx).Error("terminal notification failed (non-fatal)", "run", runID, "err", err)
	}
}

// startFailureFeedback fires the §53 feedback for a terminal BLOCKED/FAILED run,
// but only when the repo opted into alerting and never lets a feedback problem
// change the run's terminal state: the child is awaited (deterministic child ID
// `<runId>-failure`, §24) and its error is only logged. HUMAN_REQUIRED and
// CLOSED are not failures and never file a bug.
func startFailureFeedback(ctx workflow.Context, in IssueInput, out CycleOutcome) {
	if out.TerminalState != stateBlocked && out.TerminalState != stateFailed {
		return
	}
	if !AlertingEnabled(in.Lifecycle) || out.WorkflowID == "" {
		return
	}
	cwo := workflow.ChildWorkflowOptions{WorkflowID: out.WorkflowID + "-failure"}
	cctx := workflow.WithChildOptions(ctx, cwo)
	err := workflow.ExecuteChildWorkflow(cctx, FailureFeedbackWorkflow, FailureFeedbackRequest{
		Owner: out.Owner, Repo: out.Repo, RepoPath: in.RepoPath,
		IssueNum: out.Issue, Outcome: out,
		Failure: DevelopmentFailure{
			Owner: out.Owner, Repo: out.Repo, IssueNum: out.Issue, RepoPath: in.RepoPath,
			Reason: out.EscalationReason, Attempts: out.Attempts,
		},
		Targets: AlertingTargets(in.Lifecycle),
		DryRun:  in.Lifecycle.DryRun,
	}).Get(cctx, nil)
	if err != nil {
		workflow.GetLogger(ctx).Error("failure feedback failed (non-fatal)", "run", out.WorkflowID, "err", err)
	}
}
