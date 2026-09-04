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
- **Agent output contract** (`reviewer.go` `parseReviewOutput`, reused by `ReviewWithOpencode`):
  the opencode agent must print `VERDICT:APPROVE`/`VERDICT:REQUEST_CHANGES` plus
  `BLOCKING:`/`NITS:`/`CONCLUDE:` sections. Parsing is line matching, fail-closed
  to `REQUEST_CHANGES`. Changing the prompt/output shape requires changing this parser.
- **Config source of truth is `repos.yaml`.** `apply.sh` builds the worker and
  shells out to `prbot config` (the shared Go loader, `yaml.v3` `KnownFields`)
  for each repo's schedule-input JSON; unknown keys and out-of-range values
  fail fast at apply time. The schema lives in `config.go`
  (`RepoConfig`/`LifecycleConfig`, §§33/43) — adding keys to `repos.yaml`
  requires updating those structs, not grep/awk in `apply.sh`.
- Child workflow IDs are stable per repo+PR (`<repo>-review-<n>`) — that's what makes
  sweeps idempotent/review-once. Don't break the shape.
- **Idempotent GitHub mutation** (`reconcile.go`): the `Ensure*` activities are
  search-before-create and rely on deterministic HTML-comment markers
  (`konoha-parent`, `konoha-child`) matched byte-exactly in issue/PR bodies.
  `ghRunner` is a package var so tests simulate GitHub; never call
  `activity.GetLogger` inside these activities (it panics on non-Temporal ctx, which
  the unit tests use).
- PR idempotency for the publish path lives in `publish.go` (`EnsurePullRequest`,
  search by head branch, not by marker).
- `main.go` has several debug entrypoints run as CLI args: `./prbot token`,
  `./prbot start` (starts a `PrReviewWorkflow` for PR 6 of `vitdo82/ducklambda`
  by default; `PR_NUM` env overrides), `./prbot publish` (runs
  `PublishDeveloperChange` directly; `REPO_PATH`/`BRANCH`/`BASE`/`TITLE`/
  `BODY`/`VALIDATION_CMD`/`ALLOW_ON_FAILURE` env override defaults), and
`./prbot analyze` (runs the Issue Analyst agent directly and prints the
validated `IssueAnalysis` JSON; `OWNER`/`REPO`/`REPO_PATH`/`ISSUE_NUM` env
overrides; no Temporal needed), and
`./prbot estimate` (runs the Issue Analyst + Estimator agents directly and
prints the validated `EstimateResult` JSON; same env overrides; no Temporal),
and `./prbot develop` (runs the Issue Analyst + Developer agents and prints
the validated `DevelopResult` JSON; same env overrides plus `BASE_SHA`/
`VALIDATION_CMD`), and `./prbot review` (runs the Issue Analyst +
Reviewer agents and prints the validated, commit-bound `ReviewResult` JSON;
same env overrides plus `PR_NUM`/`REVIEW_SHA`).
- `analyst.go` owns the Issue Analyst (§4.1/§28/§36): spec-first discovery
  (`findSpecs`), the system-instruction constant + untrusted-data prompt
  (`analystPrompt`, §27), strict JSON parsing (`parseIssueAnalysisJSON`), and
  fail-closed validation (`ValidateIssueAnalysis`). Issue text is always data,
  never instructions; adding a new required output field means updating the
  prompt contract, the struct, and the validator together.
- `estimator.go` owns the Estimator agent (§4.2/§11/§12/§38): complexity
  (1-10 rubric) + confidence (controlled set `low|medium|high`) produced as a
  validated `EstimateResult` (`EstimateIssue`). It only *reports*; the
  workflow applies the deterministic routing policy. It reuses the analyst's
  `extractJSONObject`/`cmdEnv`. The `EstimateResult` struct (with `Risks`/
  `Assumptions`) lives in `estimator.go`, not `activities.go`.
- `policy.go` owns the deterministic policy engine (§4.8): `EvaluateAutonomy`
  (§12 predicate), `EvaluateCompletion` (§14 gate, blocks on agent
  disagreement §18), `EvaluateClosePolicy` (§19 `autonomous`/`risk-based`/
  `human-required`, default risk-based), plus the `**`-aware `globMatch`
  for §20 `humanRequiredPaths`. Pure Go, zero LLM calls; inputs are only
  validated `EstimateResult`/config/observed facts, never issue text (§5/§27
  authority ordering). No agent can self-authorize a close (§44 #5) —
  `EvaluateCompletion` is conjunctive and `CompletionAgentComplete` alone is
  never sufficient.
- `developer.go` owns the Developer agent (§4.4/§25/§26/§34/§35): `RunDeveloper`
  runs a fresh opencode execution inside an **isolated, workflow-owned worktree**
  (deterministic sibling `.konoha/<repo>-issue-<n>` dir, ownership claim file
  `<dir>.owner`). `ensureWorktree` verifies the claim before reuse: a directory
  with no claim, or claimed by another workflow, is rejected untouched; a dirty
  or base-moved **owned** worktree is wiped and recreated; a clean owned one is
  reused. `cleanupWorktree` runs deferred so it fires on failure too. The agent
  receives structured spec artifacts (§35); raw issue text is JSON-escaped inside
  `UNTRUSTED ISSUE DATA` markers (§27). The activity returns only a local commit
  artifact (never pushes/PRs) and records the §26 validation result.
- `reviewer.go` owns the Reviewer agent (§4.5/§13/§15/§27): `RunReviewer` runs an
  **independent** opencode execution that gets issue requirements/spec/diff/tests
  — never the developer's reasoning (§13). `parseReviewOutput` is the **single
  shared fail-closed parser** for the `VERDICT:`/`BLOCKING:`/`NITS:`/`CONCLUDE:`
  contract; the legacy `ReviewWithOpencode` (activities.go) reuses it, so any
  output-shape change touches one place. The verdict is read from a dedicated
  `VERDICT:<value>` line and defaults to `REQUEST_CHANGES`. `ValidateReview`
  gates verdict+commit SHA; `ReviewAppliesTo` is the §15 commit-binding
  predicate (an approval is valid only for the exact SHA it reviewed).
