# AGENTS.md

Cross-repo PR review orchestrator. A Temporal worker (single `package main`) sweeps
repos for PRs lacking a bot review, reviews each via a local `opencode` agent, and
posts the verdict back as a GitHub App review.

## Verify

```bash
make test   # go vet ./... + go build ./...
go test ./...   # unit tests for idempotency markers + Ensure* reconciliation (reconcile_test.go)
```

## Run

Needs a local Temporal server (`temporal server start-dev`, listens on
`127.0.0.1:7233`, override `TEMPORAL_ADDRESS`), the `gh` CLI on PATH, and GitHub
App credentials (never commit these):

```bash
GH_APP_ID=<id> GH_APP_INSTALL_ID=<id> GH_APP_PEM=<path> make run
./apply.sh            # (re)create one <repo>-review-all Temporal schedule per repos.yaml entry
make token            # smoke-test app auth: prints a masked installation token
```

## Architecture / gotchas

- **Workflows vs activities** (`workflow.go` vs `activities.go`): Temporal workflows
  must be deterministic (no I/O, no time in logic). All I/O — shelling out to `gh`
  and `opencode` — lives in activities. Keep it that way.
- **Workflows are registered by string name** (`main.go:28-33`); activity executions
  are serialized by signature. Changing a workflow/activity signature breaks in-flight
  executions and must happen in lockstep with the registration.
- **Agent output contract** (`activities.go` `ReviewWithOpencode`): the opencode agent
  must print `VERDICT:APPROVE`/`VERDICT:REQUEST_CHANGES` plus `BLOCKING:`/`NITS:`/
  `CONCLUDE:` sections. Parsing is substring/line matching and fail-closed to
  `REQUEST_CHANGES`. Changing the prompt/output shape requires changing this parser.
- **Config source of truth is `repos.yaml`.** `apply.sh` parses it with a grep/awk
  hack (reads only `repo`, one shared `owner`, and per-repo `repoPath`). Adding keys
  to `repos.yaml` may require updating `apply.sh`.
- Child workflow IDs are stable per repo+PR (`<repo>-review-<n>`) — that's what makes
  sweeps idempotent/review-once. Don't break the shape.
- **Idempotent GitHub mutation** (`reconcile.go`): the `Ensure*` activities are
  search-before-create and rely on deterministic HTML-comment markers
  (`konoha-parent`, `konoha-child`, `konoha-issue-cycle`) matched byte-exactly in
  issue/PR bodies. `ghRunner` is a package var so tests simulate GitHub; never call
  `activity.GetLogger` inside these activities (it panics on non-Temporal ctx, which
  the unit tests use).
- `main.go` has two debug entrypoints run as CLI args: `./prbot token` and
  `./prbot start` (starts a `PrReviewWorkflow` for PR 6 of `vitdo82/ducklambda` by
  default; `PR_NUM` env overrides).