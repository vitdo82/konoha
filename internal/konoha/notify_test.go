package konoha

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/testsuite"
)

// #123: slack/webhook/log targets are selectable; an unknown target type (or a
// missing/forbidden URL) fails at load.
func TestNotifyTargetConfigValidation(t *testing.T) {
	good := `
repos:
  - repo: good
    owner: o
    repoPath: /x
    lifecycle:
      alerting:
        enabled: true
        notify: https://hooks.example/legacy
        targets:
          - type: log
          - type: slack
            url: https://hooks.example/slack
          - type: webhook
            url: https://hooks.example/generic
        thresholds: {split_rate: 0.5}
`
	if _, err := LoadReposConfig(writeRepos(t, good)); err != nil {
		t.Fatalf("valid notify targets rejected: %v", err)
	}

	bad := map[string]string{
		"unknown type": `
repos: [{repo: r, owner: o, repoPath: /x, lifecycle: {alerting: {enabled: true, thresholds: {split_rate: 0.5}, targets: [{type: email, url: https://x}]}}}]`,
		"empty type": `
repos: [{repo: r, owner: o, repoPath: /x, lifecycle: {alerting: {enabled: true, thresholds: {split_rate: 0.5}, targets: [{url: https://x}]}}}]`,
		"slack without url": `
repos: [{repo: r, owner: o, repoPath: /x, lifecycle: {alerting: {enabled: true, thresholds: {split_rate: 0.5}, targets: [{type: slack}]}}}]`,
		"webhook bad scheme": `
repos: [{repo: r, owner: o, repoPath: /x, lifecycle: {alerting: {enabled: true, thresholds: {split_rate: 0.5}, targets: [{type: webhook, url: "ftp://x"}]}}}]`,
		"log with url": `
repos: [{repo: r, owner: o, repoPath: /x, lifecycle: {alerting: {enabled: true, thresholds: {split_rate: 0.5}, targets: [{type: log, url: https://x}]}}}]`,
	}
	for name, body := range bad {
		if _, err := LoadReposConfig(writeRepos(t, body)); err == nil {
			t.Fatalf("%s: invalid notify target must fail fast", name)
		}
	}
}

// #40: a delivery failure is returned (recorded) and its dedupe key is NOT
// recorded, so the retry re-attempts; a delivered event is then deduped.
func TestDeliverEventFailClosedThenDeduped(t *testing.T) {
	resetDeliveredNotifications()
	defer resetDeliveredNotifications()

	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		if hits == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	targets := []NotifyTarget{{Type: notifyTargetWebhook, URL: srv.URL}}
	ev := NotifyEvent{Kind: notifyKindTerminal, Owner: "o", Repo: "r", Text: "x", DedupeKey: "run|CLOSED"}

	if err := deliverEvent(context.Background(), targets, ev, false); err == nil {
		t.Fatal("a failing delivery must be returned, never silently dropped")
	}
	if err := deliverEvent(context.Background(), targets, ev, false); err != nil {
		t.Fatalf("retry must re-attempt the undelivered target: %v", err)
	}
	if err := deliverEvent(context.Background(), targets, ev, false); err != nil {
		t.Fatalf("delivered event must be a no-op (deduped): %v", err)
	}
	if hits != 2 {
		t.Fatalf("httptest hits = %d, want 2 (one retry, then deduped)", hits)
	}
}

// #42: dry-run logs but never sends.
func TestDeliverEventDryRunNoSend(t *testing.T) {
	resetDeliveredNotifications()
	defer resetDeliveredNotifications()

	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("dry-run must never hit the network")
	}))
	defer srv.Close()

	err := deliverEvent(context.Background(),
		[]NotifyTarget{{Type: notifyTargetSlack, URL: srv.URL}},
		NotifyEvent{Kind: notifyKindAlert, Owner: "o", Repo: "r", Text: "x", DedupeKey: "k"}, true)
	if err != nil {
		t.Fatalf("dry-run must not fail: %v", err)
	}
}

// #123: the slack notifier posts a Slack incoming-webhook message (`text`).
func TestSlackNotifierPayload(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n, err := newNotifier(NotifyTarget{Type: notifyTargetSlack, URL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if err := n.Notify(context.Background(), NotifyEvent{
		Kind: notifyKindFailure, Owner: "o", Repo: "r", Text: "boom",
	}); err != nil {
		t.Fatal(err)
	}
	text, ok := body["text"].(string)
	if !ok || text == "" {
		t.Fatalf("slack payload missing text: %v", body)
	}
}

// #123: an unknown target type fails closed even outside config validation.
func TestNewNotifierUnknownFailsClosed(t *testing.T) {
	if _, err := newNotifier(NotifyTarget{Type: "email", URL: "https://x"}); err == nil {
		t.Fatal("unknown target type must fail closed")
	}
}

// #123: legacy `notify` folds into a slack target; explicit targets append.
func TestResolveNotifyTargets(t *testing.T) {
	got := resolveNotifyTargets(AlertingConfig{
		Notify:  "https://hooks.example/legacy",
		Targets: []NotifyTarget{{Type: notifyTargetLog}},
	})
	if len(got) != 2 || got[0].Type != notifyTargetSlack || got[0].URL != "https://hooks.example/legacy" {
		t.Fatalf("legacy notify must fold in as a slack target: %+v", got)
	}
	if len(resolveNotifyTargets(AlertingConfig{})) != 0 {
		t.Fatal("no targets must resolve to none")
	}
}

// #123: a terminal cycle outcome is fanned out to a configured target exactly
// once, carrying the run + terminal state as the dedupe key.
func TestWorkflowTerminalNotificationFires(t *testing.T) {
	resetMetrics()
	resetDeliveredNotifications()
	defer resetDeliveredNotifications()

	env := mockCycleEnv(t, EstimateResult{Complexity: 3, Confidence: "high"})
	mockSmallPath(env)
	var got []NotifyCycleRequest
	env.OnActivity(NotifyCycleEvent, mock.Anything, mock.Anything).Return(
		func(_ context.Context, in NotifyCycleRequest) error {
			got = append(got, in)
			return nil
		})

	in := IssueInput{Owner: "o", Repo: "r", IssueNum: testIssue, RepoPath: t.TempDir(),
		Lifecycle: LifecycleConfig{Mode: modeSupervisedAutonomous, DevelopmentMaxComplexity: 4,
			Alerting: AlertingConfig{Enabled: true, Thresholds: map[string]float64{mAutonomousRate: 0.7},
				Targets: []NotifyTarget{{Type: notifyTargetLog}}}}}
	env.ExecuteWorkflow(IssueCycleWorkflow, in)
	if env.IsWorkflowCompleted() {
		if err := env.GetWorkflowError(); err != nil {
			t.Fatalf("workflow error: %v", err)
		}
	}
	if got := terminalState(t, env); got != stateClosed {
		t.Fatalf("terminal = %q, want CLOSED", got)
	}
	if len(got) != 1 {
		t.Fatalf("terminal notifications = %d, want 1: %+v", len(got), got)
	}
	if got[0].Kind != notifyKindTerminal || !strings.Contains(got[0].DedupeKey, stateClosed) {
		t.Fatalf("unexpected terminal notification: %+v", got[0])
	}
}

// #123: the failure-feedback filing notifies the configured target once.
func TestFailureFeedbackNotifies(t *testing.T) {
	env := (&testsuite.WorkflowTestSuite{}).NewTestWorkflowEnvironment()
	env.OnActivity(EnsureFailureIssue, mock.Anything, mock.Anything).Return(FailureIssueResult{Number: 9, Created: true}, nil)
	notified := 0
	env.OnActivity(NotifyCycleEvent, mock.Anything, mock.Anything).Return(
		func(_ context.Context, in NotifyCycleRequest) error {
			notified++
			if in.Kind != notifyKindFailure {
				t.Errorf("notify kind = %q, want failure", in.Kind)
			}
			return nil
		})
	env.ExecuteWorkflow(FailureFeedbackWorkflow, FailureFeedbackRequest{
		Owner: "o", Repo: "r",
		Outcome: CycleOutcome{WorkflowID: "r-issue-7", Issue: 7, TerminalState: stateBlocked},
		Targets: []NotifyTarget{{Type: notifyTargetLog}},
	})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	if notified != 1 {
		t.Fatalf("failure notification fired %d times, want 1", notified)
	}
}
