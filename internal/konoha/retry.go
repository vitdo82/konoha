package konoha

import (
	"context"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// Typed activity retries (#95). Konoha previously applied one uniform
// RetryMaxAttempts to every activity. The classes below make the retry contract
// explicit per activity kind:
//
//	deterministic  pure / policy / derived-record activities; a retry is at
//	               best wasteful and at worst doubles a counter → 1 attempt.
//	infrastructure gh / git / evidence I/O; transient network loss → 5 attempts
//	               with exponential backoff.
//	agent          a fresh LLM run. A transport-loss retry would replay an
//	               agent whose side effects we cannot reason about → exactly 1.
//	verification   the CI / deploy verification gates → 3 attempts.
//
// The sweep workflows (ReviewAll/IssueAll) intentionally keep their own
// `sweepRetryMaxAttempts` policy and never route through these helpers (#95 AC4).
type retryClass int

const (
	retryDeterministic retryClass = iota
	retryInfrastructure
	retryAgent
	retryVerification
)

// Attempt counts. The agent/verification contracts are fixed (never
// config-driven): a policy must not fork per host. Infrastructure attempts are
// the worker's `retryMaxAttempts` (host/runtime), defaulting to 5.
const (
	agentRetryMaxAttempts        = 1
	verifRetryMaxAttempts        = 3
	defaultInfraRetryMaxAttempts = 5

	// agentHeartbeatTimeout bounds the gap between heartbeats on long agent
	// activities, so a hung agent process surfaces as a heartbeat timeout
	// instead of blocking until StartToCloseTimeout.
	agentHeartbeatTimeout  = time.Minute
	agentHeartbeatInterval = agentHeartbeatTimeout / 2
)

// retryPolicyFor returns the RetryPolicy for a class.
func retryPolicyFor(c retryClass) *temporal.RetryPolicy {
	switch c {
	case retryInfrastructure:
		return &temporal.RetryPolicy{
			MaximumAttempts:    int32(appCfg.Worker.infraAttempts()),
			InitialInterval:    time.Second,
			BackoffCoefficient: 2,
			MaximumInterval:    time.Minute,
		}
	case retryAgent:
		return &temporal.RetryPolicy{MaximumAttempts: agentRetryMaxAttempts}
	case retryVerification:
		return &temporal.RetryPolicy{MaximumAttempts: verifRetryMaxAttempts}
	default:
		return &temporal.RetryPolicy{MaximumAttempts: 1}
	}
}

// activityOptionsFor builds the ActivityOptions for a class + window. Agent
// activities also get a HeartbeatTimeout (see startAgentHeartbeat).
func activityOptionsFor(c retryClass, window time.Duration) workflow.ActivityOptions {
	ao := workflow.ActivityOptions{
		StartToCloseTimeout: window,
		RetryPolicy:         retryPolicyFor(c),
	}
	if c == retryAgent {
		ao.HeartbeatTimeout = agentHeartbeatTimeout
	}
	return ao
}

// activityAttempt returns the Temporal activity attempt number (1-based), or 1
// outside a Temporal activity context (unit tests call the activities directly,
// where activity.GetInfo would panic).
func activityAttempt(ctx context.Context) int {
	if !activity.IsActivity(ctx) {
		return 1
	}
	return int(activity.GetInfo(ctx).Attempt)
}

// startAgentHeartbeat periodically heartbeats while an agent child process runs,
// so Temporal marks a hung run as timed out rather than waiting for
// StartToCloseTimeout. It is a no-op outside a Temporal activity context. The
// returned stop func is idempotent.
func startAgentHeartbeat(ctx context.Context) (stop func()) {
	if !activity.IsActivity(ctx) {
		return func() {}
	}
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		t := time.NewTicker(agentHeartbeatInterval)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				activity.RecordHeartbeat(ctx, "agent running")
			}
		}
	}()
	var once bool
	return func() {
		if once {
			return
		}
		once = true
		close(done)
		<-finished // never let a heartbeat outlive the activity
	}
}
