# Konoha

Cross-repo PR review orchestrator. Runs a Temporal worker that sweeps repos for
open PRs without a `kakashi-82[bot]` review and reviews each via a local opencode
agent, posting the verdict back to the PR.

## Architecture

- **Worker** (Go, Temporal SDK): `ReviewAllWorkflow` (cron-driven per repo via
  Temporal Schedule) → child `PrReviewWorkflow` (`<repo>-review-<n>`) →
  activities `ListUnreviewedPRs` → `FetchDiff` → `ReviewWithOpencode` →
  `SubmitReview`. `IssueAllWorkflow` sweeps unmanaged issues into
  `IssueCycleWorkflow` (`<repo>-issue-<n>`), capped by
  `maxConcurrentIssueCycles`.
- **Config**: `repos.yaml` — one entry per repo (source of truth); per-repo
  `lifecycle:` policy (§33/§43 of `docs/new-design-v2.md`): mode, complexity,
  review attempts, autonomy/escalation, `poGateTimeout`, `dryRun`, and §43
  concurrency caps.
- **Apply**: `scripts/apply.sh` builds the worker and asks `prbot config` (the
  shared Go loader) for each repo's schedule input JSON — unknown keys / bad
  values in `repos.yaml` fail fast. It then (re)creates `<repo>-review-all` and
  `<repo>-issue-all` schedules per repo.

## Layout

Standard Go layout — entrypoint first, all logic internal, config/scripts
separated:

```
cmd/konoha/main.go       # the only entrypoint: CLI dispatch + worker + all w.Register*
internal/konoha/         # ALL logic + tests (single package konoha): workflows,
                         #   agent activities, policy, reconcile, labels, webhook, metrics
configs/repos.yaml       # per-repo lifecycle policy (source of truth; KONOHA_CONFIG_DIR overrides)
scripts/apply.sh         # schedule creation
docs/new-design-v2.md    # design authority
```

## Run

Needs a local Temporal server (`temporal server start-dev`), the `gh` CLI on
PATH, and GitHub App credentials:

```bash
make run                          # build + run worker (make build => /tmp/prbot)
./scripts/apply.sh                # apply/recreate schedules from repos.yaml
make token                        # smoke-test GitHub App auth (masked token)
make test                         # go vet + go test + go build (config, markers, dry-run)
```

Env: `GH_APP_ID`, `GH_APP_INSTALL_ID`, `GH_APP_PEM` (path to private key —
never committed) and `TEMPORAL_ADDRESS` (default `127.0.0.1:7233`).
`KONOHA_CONFIG_DIR` overrides where `repos.yaml` is read (default `configs/`).

## Debug

The binary also has CLI entrypoints used with `make build`:
`/tmp/prbot token`, `/tmp/prbot config [configs/repos.yaml]` (prints schedule
inputs for `apply.sh`), `/tmp/prbot start` (runs a `PrReviewWorkflow` for PR 6
of `vitdo82/ducklambda`; `PR_NUM` overrides), and `/tmp/prbot publish`
(`DRY_RUN=1` to shadow-run).

## Add a repo

Add one entry to `configs/repos.yaml`, then `./scripts/apply.sh`.