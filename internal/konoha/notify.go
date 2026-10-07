package konoha

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

// #123 notification providers, on top of §53 alerting. A `Notifier` decouples a
// delivery target from the event source: §53 metric alerts, terminal cycle
// outcomes (CLOSED/BLOCKED/FAILED/HUMAN_REQUIRED), and failure-feedback filings
// are rendered to a provider-neutral `NotifyEvent` and fanned out to every
// configured target (Slack incoming webhook, generic webhook, log-only). The
// metrics layer is untouched — it never learns a notification target exists.
//
// Delivery is fail-closed (§40) and idempotent (§24): a target that fails
// returns an error so the calling activity retries, and its dedupe key is not
// recorded, so the retry re-attempts only targets that have not yet accepted the
// event. A `(target, dedupeKey)` already delivered is skipped, so a retry can
// never double-notify. DryRun (§42) logs but never sends.

// The closed set of target types. An unknown type fails at config load
// (AlertingConfig.validate) and again in newNotifier.
const (
	notifyTargetSlack   = "slack"
	notifyTargetWebhook = "webhook"
	notifyTargetLog     = "log"
)

// The closed set of event kinds.
const (
	notifyKindAlert    = "alert"
	notifyKindTerminal = "terminal"
	notifyKindFailure  = "failure"
)

// NotifyTarget is one configured notification destination.
type NotifyTarget struct {
	Type string `yaml:"type" json:"type"`
	URL  string `yaml:"url" json:"url,omitempty"` // required for slack/webhook; forbidden for log
}

// NotifyEvent is the provider-neutral event handed to a Notifier.
type NotifyEvent struct {
	Kind      string `json:"kind"` // alert | terminal | failure
	Owner     string `json:"owner"`
	Repo      string `json:"repo"`
	Title     string `json:"title,omitempty"`
	Text      string `json:"text"`
	DedupeKey string `json:"dedupeKey"`
}

// Notifier delivers one event to one target.
type Notifier interface {
	Notify(ctx context.Context, ev NotifyEvent) error
}

// resolveNotifyTargets folds the legacy single `notify` webhook (slack-
// compatible) into the target list. Pure and replay-safe.
func resolveNotifyTargets(ac AlertingConfig) []NotifyTarget {
	var out []NotifyTarget
	if n := strings.TrimSpace(ac.Notify); n != "" {
		out = append(out, NotifyTarget{Type: notifyTargetSlack, URL: n})
	}
	out = append(out, ac.Targets...)
	return out
}

// AlertingTargets returns the resolved notification targets for a repo.
func AlertingTargets(lc LifecycleConfig) []NotifyTarget { return resolveNotifyTargets(lc.Alerting) }

// validateNotifyURL gates a target URL: an http(s) URL with a host.
func validateNotifyURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("%q must be an http(s) URL", raw)
	}
	return nil
}

// validate gates one target: a known type, a URL for slack/webhook, and no URL
// for log.
func (t NotifyTarget) validate(prefix string) error {
	switch strings.ToLower(strings.TrimSpace(t.Type)) {
	case notifyTargetLog:
		if strings.TrimSpace(t.URL) != "" {
			return fmt.Errorf("%s.url: not allowed for a %s target", prefix, notifyTargetLog)
		}
	case notifyTargetWebhook, notifyTargetSlack:
		if err := validateNotifyURL(t.URL); err != nil {
			return fmt.Errorf("%s.url: %w", prefix, err)
		}
	default:
		return fmt.Errorf("%s.type: %q is not a known notify target (known: %s, %s, %s)",
			prefix, t.Type, notifyTargetSlack, notifyTargetWebhook, notifyTargetLog)
	}
	return nil
}

// newNotifier builds the Notifier for a target. An unknown type fails closed.
func newNotifier(t NotifyTarget) (Notifier, error) {
	switch strings.ToLower(strings.TrimSpace(t.Type)) {
	case notifyTargetLog:
		return logNotifier{}, nil
	case notifyTargetWebhook:
		return webhookNotifier{url: t.URL}, nil
	case notifyTargetSlack:
		return slackNotifier{url: t.URL}, nil
	default:
		return nil, fmt.Errorf("unknown notify target type %q", t.Type)
	}
}

// logNotifier is the log-only target: it never touches the network.
type logNotifier struct{}

func (logNotifier) Notify(_ context.Context, ev NotifyEvent) error {
	log.Printf("konoha notify [%s] %s/%s: %s", ev.Kind, ev.Owner, ev.Repo, ev.Text)
	return nil
}

// webhookNotifier POSTs the structured event as JSON to a generic webhook.
type webhookNotifier struct{ url string }

func (w webhookNotifier) Notify(ctx context.Context, ev NotifyEvent) error {
	return postNotifyJSON(ctx, w.url, ev)
}

// slackNotifier POSTs a Slack incoming-webhook message (`{"text": ...}`).
type slackNotifier struct{ url string }

func (s slackNotifier) Notify(ctx context.Context, ev NotifyEvent) error {
	return postNotifyJSON(ctx, s.url, map[string]any{"text": slackMessage(ev)})
}

// slackMessage renders a compact Slack line carrying the repo + kind.
func slackMessage(ev NotifyEvent) string {
	return fmt.Sprintf("konoha %s [%s/%s] %s", ev.Kind, ev.Owner, ev.Repo, ev.Text)
}

// postNotifyJSON POSTs a JSON body and requires a 2xx response (a non-2xx is an
// error — never a silent success).
func postNotifyJSON(ctx context.Context, rawURL string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("notify %s: %w", rawURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("notify %s: status %d: %s", rawURL, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}

// deliveredNotifications dedupes delivery per (target, event) so a retry can
// never double-notify. Process-local: single-worker deployments converge; a
// multi-worker deployment would need a durable marker (out of scope while one
// worker per queue is the deployment model).
var (
	deliveredNotifyMu sync.Mutex
	deliveredNotify   = map[string]bool{}
)

func notifyKey(t NotifyTarget, dedupeKey string) string {
	return strings.ToLower(strings.TrimSpace(t.Type)) + "|" + t.URL + "|" + dedupeKey
}

func notifySeen(key string) bool {
	deliveredNotifyMu.Lock()
	defer deliveredNotifyMu.Unlock()
	return deliveredNotify[key]
}

func markNotifySeen(key string) {
	deliveredNotifyMu.Lock()
	defer deliveredNotifyMu.Unlock()
	deliveredNotify[key] = true
}

// resetDeliveredNotifications clears the dedupe set (tests).
func resetDeliveredNotifications() {
	deliveredNotifyMu.Lock()
	defer deliveredNotifyMu.Unlock()
	deliveredNotify = map[string]bool{}
}

// deliverEvent fans an event out to every target, skipping a (target, dedupeKey)
// already delivered. Fail-closed: the first delivery error is returned and that
// target's key is NOT recorded, so the calling activity's retry re-attempts it
// (while already-delivered targets stay deduped). DryRun logs and sends nothing.
func deliverEvent(ctx context.Context, targets []NotifyTarget, ev NotifyEvent, dryRun bool) error {
	if strings.TrimSpace(ev.DedupeKey) == "" {
		return fmt.Errorf("notify: empty dedupe key")
	}
	for _, t := range targets {
		key := notifyKey(t, ev.DedupeKey)
		if notifySeen(key) {
			continue
		}
		if dryRun {
			log.Printf("konoha notify (dry-run) [%s] %s/%s: %s", ev.Kind, ev.Owner, ev.Repo, ev.Text)
			continue
		}
		n, err := newNotifier(t)
		if err != nil {
			return err
		}
		if err := n.Notify(ctx, ev); err != nil {
			return err
		}
		markNotifySeen(key)
	}
	return nil
}

// NotifyCycleRequest parameterizes the NotifyCycleEvent activity — the single
// workflow-side delivery entrypoint for terminal + failure events.
type NotifyCycleRequest struct {
	Kind      string         `json:"kind"`
	Owner     string         `json:"owner"`
	Repo      string         `json:"repo"`
	Title     string         `json:"title,omitempty"`
	Text      string         `json:"text"`
	DedupeKey string         `json:"dedupeKey"`
	Targets   []NotifyTarget `json:"targets,omitempty"`
	DryRun    bool           `json:"dryRun,omitempty"`
}

// NotifyCycleEvent delivers one event to the configured targets. It is I/O (HTTP
// / log), so it lives in an activity; the retry policy is the caller's
// (infrastructure). No targets is a no-op.
func NotifyCycleEvent(ctx context.Context, in NotifyCycleRequest) error {
	if len(in.Targets) == 0 {
		return nil
	}
	return deliverEvent(ctx, in.Targets, NotifyEvent{
		Kind: in.Kind, Owner: in.Owner, Repo: in.Repo,
		Title: in.Title, Text: in.Text, DedupeKey: in.DedupeKey,
	}, in.DryRun)
}
