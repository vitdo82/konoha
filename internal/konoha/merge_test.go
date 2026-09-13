package konoha

import (
	"context"
	"errors"
	"testing"
)

// §49 merge strategy whitelist (§36 fail-closed for anything else).
func TestValidMergeStrategy(t *testing.T) {
	for _, s := range []string{mergeStrategySquash, mergeStrategyRebase, mergeStrategyMerge} {
		if !ValidMergeStrategy(s) {
			t.Fatalf("strategy %q must be valid", s)
		}
	}
	for _, s := range []string{"", "fast-forward", "SQUASH", "banana"} {
		if ValidMergeStrategy(s) {
			t.Fatalf("strategy %q must be rejected", s)
		}
	}
}

func useFakeToken(t *testing.T) {
	t.Helper()
	t.Setenv("KONOHA_TOKEN_FIXED", "test-token")
}

// withTokenRunner overrides the merge command seam for the duration of a test.
func withTokenRunner(t *testing.T, fn func(ctx context.Context, tok string, args ...string) ([]byte, error)) {
	t.Helper()
	prev := ghTokenRunner
	t.Cleanup(func() { ghTokenRunner = prev })
	ghTokenRunner = fn
}

// §49/§24: an already-merged PR is a no-op probe — the merge command is never
// issued, so a retry can never double-merge.
func TestMergePullRequestIdempotentWhenAlreadyMerged(t *testing.T) {
	useFakeToken(t)
	var mergeCalls int
	withTokenRunner(t, func(_ context.Context, _ string, args ...string) ([]byte, error) {
		if args[0] == "pr" && args[1] == "merge" {
			mergeCalls++
		}
		return []byte(`{"state":"MERGED"}`), nil
	})
	res, err := MergePullRequest(context.Background(), MergeRequest{
		Owner: "o", Repo: "r", PrNum: 1, Strategy: mergeStrategySquash,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Merged {
		t.Fatalf("want merged=true, got %+v", res)
	}
	if mergeCalls != 0 {
		t.Fatalf("already-merged PR must not be merged again, got %d calls", mergeCalls)
	}
}

// §49/§24: two invocations (retry/overlap) merge exactly once.
func TestMergePullRequestMergesExactlyOnce(t *testing.T) {
	useFakeToken(t)
	merged := false
	var mergeCalls int
	withTokenRunner(t, func(_ context.Context, _ string, args ...string) ([]byte, error) {
		if args[0] == "pr" && args[1] == "merge" {
			mergeCalls++
			merged = true
			return []byte("merged"), nil
		}
		if merged {
			return []byte(`{"state":"MERGED"}`), nil
		}
		return []byte(`{"state":"OPEN"}`), nil
	})
	for i := 0; i < 2; i++ {
		res, err := MergePullRequest(context.Background(), MergeRequest{
			Owner: "o", Repo: "r", PrNum: 2, Strategy: mergeStrategyRebase,
		})
		if err != nil {
			t.Fatalf("attempt %d: %v", i, err)
		}
		if !res.Merged {
			t.Fatalf("attempt %d: want merged=true, got %+v", i, res)
		}
	}
	if mergeCalls != 1 {
		t.Fatalf("merge must happen exactly once, got %d", mergeCalls)
	}
}

// §24: a lost merge response is reconciled by re-reading state — success, not a
// failure that would spuriously escalate.
func TestMergePullRequestLostResponseReconciled(t *testing.T) {
	useFakeToken(t)
	merged := false
	withTokenRunner(t, func(_ context.Context, _ string, args ...string) ([]byte, error) {
		if args[0] == "pr" && args[1] == "merge" {
			merged = true // merge actually landed, but the response was lost
			return nil, errors.New("network reset")
		}
		if merged {
			return []byte(`{"state":"MERGED"}`), nil
		}
		return []byte(`{"state":"OPEN"}`), nil
	})
	res, err := MergePullRequest(context.Background(), MergeRequest{
		Owner: "o", Repo: "r", PrNum: 3, Strategy: mergeStrategyMerge,
	})
	if err != nil {
		t.Fatalf("lost response must reconcile to success: %v", err)
	}
	if !res.Merged {
		t.Fatalf("want merged=true, got %+v", res)
	}
}

// §49: an invalid strategy fails closed before any mutation.
func TestMergePullRequestRejectsBadStrategy(t *testing.T) {
	useFakeToken(t)
	withTokenRunner(t, func(context.Context, string, ...string) ([]byte, error) {
		t.Fatal("no gh command may run for an invalid strategy")
		return nil, nil
	})
	if _, err := MergePullRequest(context.Background(), MergeRequest{
		Owner: "o", Repo: "r", PrNum: 4, Strategy: "fast-forward",
	}); err == nil {
		t.Fatal("invalid strategy must be rejected")
	}
}

// §49: a PR closed without merge can never be reported as merged.
func TestMergePullRequestClosedWithoutMergeFails(t *testing.T) {
	useFakeToken(t)
	withTokenRunner(t, func(context.Context, string, ...string) ([]byte, error) {
		return []byte(`{"state":"CLOSED"}`), nil
	})
	if _, err := MergePullRequest(context.Background(), MergeRequest{
		Owner: "o", Repo: "r", PrNum: 5, Strategy: mergeStrategySquash,
	}); err == nil {
		t.Fatal("closed-unmerged PR must fail (no silent success)")
	}
}

// §42: shadow mode mutates nothing.
func TestMergePullRequestDryRunSkips(t *testing.T) {
	withTokenRunner(t, func(context.Context, string, ...string) ([]byte, error) {
		t.Fatal("dry run must not invoke gh")
		return nil, nil
	})
	res, err := MergePullRequest(context.Background(), MergeRequest{
		Owner: "o", Repo: "r", PrNum: 6, Strategy: mergeStrategySquash, DryRun: true,
	})
	if err != nil || !res.Skipped || res.Merged {
		t.Fatalf("dry run must skip: res=%+v err=%v", res, err)
	}
}

func withCheckRunner(t *testing.T, fn func(ctx context.Context, args ...string) ([]byte, error)) {
	t.Helper()
	prev := ghRunner
	t.Cleanup(func() { ghRunner = prev })
	ghRunner = fn
}

// §49: all checks pass → Green.
func TestWaitForCIGreen(t *testing.T) {
	withCheckRunner(t, func(context.Context, ...string) ([]byte, error) {
		return []byte(`[{"bucket":"pass","name":"build"},{"bucket":"skipping","name":"lint"}]`), nil
	})
	st, err := WaitForCI(context.Background(), CIRequest{Owner: "o", Repo: "r", PrNum: 1, RequireCI: true, MaxWait: "1s"})
	if err != nil || !st.Green {
		t.Fatalf("want green, got %+v err=%v", st, err)
	}
}

// §49: a failing check → Failed, Green=false (the workflow escalates).
func TestWaitForCIFailed(t *testing.T) {
	withCheckRunner(t, func(context.Context, ...string) ([]byte, error) {
		return []byte(`[{"bucket":"pass","name":"build"},{"bucket":"fail","name":"test"}]`), nil
	})
	st, err := WaitForCI(context.Background(), CIRequest{Owner: "o", Repo: "r", PrNum: 1, RequireCI: true, MaxWait: "1s"})
	if err != nil {
		t.Fatal(err)
	}
	if st.Green || !st.Failed {
		t.Fatalf("want failed, got %+v", st)
	}
}

// §49/§40: an unclassifiable check is never a silent pass.
func TestWaitForCIUnknownIsFailClosed(t *testing.T) {
	withCheckRunner(t, func(context.Context, ...string) ([]byte, error) {
		return []byte(`[{"bucket":"wat","name":"x"}]`), nil
	})
	st, err := WaitForCI(context.Background(), CIRequest{Owner: "o", Repo: "r", PrNum: 1, RequireCI: true, MaxWait: "1s"})
	if err != nil || st.Green || !st.Failed {
		t.Fatalf("unknown bucket must fail closed, got %+v err=%v", st, err)
	}
}

// §49/§40: pending past the bounded window → not green (escalate, no hang).
func TestWaitForCITimeout(t *testing.T) {
	withCheckRunner(t, func(context.Context, ...string) ([]byte, error) {
		return []byte(`[{"bucket":"pending","name":"build"}]`), nil
	})
	st, err := WaitForCI(context.Background(), CIRequest{Owner: "o", Repo: "r", PrNum: 1, RequireCI: true, MaxWait: "1ms"})
	if err != nil {
		t.Fatal(err)
	}
	if st.Green || !st.Pending {
		t.Fatalf("want timed-out pending, got %+v", st)
	}
}

// §49: requireCI disabled → no-op gate that passes (repo opted out).
func TestWaitForCISkippedWhenNotRequired(t *testing.T) {
	withCheckRunner(t, func(context.Context, ...string) ([]byte, error) {
		t.Fatal("requireCI=false must not call gh")
		return nil, nil
	})
	st, err := WaitForCI(context.Background(), CIRequest{Owner: "o", Repo: "r", PrNum: 1, RequireCI: false})
	if err != nil || !st.Skipped || !st.Green {
		t.Fatalf("requireCI=false must be a passing skip, got %+v err=%v", st, err)
	}
}
