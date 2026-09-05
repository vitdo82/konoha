# AGENTS.md

Cross-repo PR review orchestrator. A Temporal worker (single `package main`) sweeps
repos for PRs lacking a bot review, reviews each via a local `opencode` agent, and
posts the verdict back as a GitHub App review.

## Verify

```bash
make test   # go vet ./... + go build ./... + go test ./... (§41 test suite)
```

`go test ./...` runs the full §41 matrix: activity/reconcile idempotency
(`reconcile_test.go`), every agent validator (§36), the derived §39 metric
gauges, the §44 invariant assertions (`invariants_test.go`), and
Temporal-testsuite workflow runs (`workflow_suite_test.go`) — the agent
activities are mocked with testify `Anything` matchers (one per activity
argument) and `RecordCycleMetrics` runs for real, so the terminal §6 state is
assertable via the `state` query handler.

## Run

Needs a local Temporal server (`temporal server start-dev`, listens on
`127.0.0.1:7233`, override `TEMPORAL_ADDRESS`), the `gh` CLI on PATH, and GitHub
App credentials (never commit these):

```bash
GH_APP_ID=<id> GH_APP_INSTALL_ID=<id> GH_APP_PEM=<path> make run
./apply.sh            # (re)create one <repo>-review-all Temporal schedule per repos.yaml entry
make token            # smoke-test app auth: prints a masked installation token
```

Webhook fast-path (optional, §32): set `GH_WEBHOOK_SECRET` (App secret) and
`WEBHOOK_ADDR` (e.g. `127.0.0.1:8080`) so issue `opened` events start
`IssueCycleWorkflow` within seconds instead of waiting for the sweep. The
deterministic `<repo>-issue-<n>` ID makes webhook + sweep starts converge
(§32); `edited`/`labeled`/`closed` reconcile via the sweep/running cycle
without restarting a completed workflow. Deliveries with an invalid
`X-Hub-Signature-256` HMAC are rejected.

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
`VALIDATION_CMD`), and `./prbot review` (runs the Issue Analyst + Reviewer agents and prints the
validated, commit-bound `ReviewResult` JSON; same env overrides plus
`PR_NUM`/`REVIEW_SHA`), and `./prbot repair` (runs the Issue Analyst +
Repair agents and prints the validated `DevelopResult` JSON; same env
overrides plus `PRIOR_SHA` and newline-separated `REVIEW_BLOCKING`/
`REVIEW_NITS`), and `./prbot complete` (runs the Issue Analyst + Completion
agents and prints the validated `CompletionResult` JSON; same env overrides
plus `PR_NUM`/`REVIEW_VERDICT`/`REVIEW_SUMMARY`/`DIFF`/`VALIDATION_OUTPUT`),
and `./prbot resolve` (runs the Issue Analyst + Resolution agents and prints
the validated `ResolutionResult` JSON; same env overrides plus `REVIEW_VERDICT`/
`REVIEW_BLOCKING`/`REVIEW_NITS`/`COMPLETE`/`COMPLETION_RECOMMENDATION`/
`DEVELOPER_SUMMARY`), and `./prbot po` (reads + validates the human decision
channel for an issue and prints the `HumanDecision` JSON; none/malformed
fails closed, §36), and `./prbot split` (runs the Issue Analyst + Estimator +
Splitter agents and prints the validated `SplitResult` JSON; same env
overrides; no Temporal), and `./prbot diagnose` (runs the Issue Analyst +
Diagnosis agents and prints the validated `DiagnosisResult` JSON; same env
overrides plus `DIAG_REASON`/`ATTEMPTS`/`ATTEMPT_SUMMARIES`), and
`./prbot labels` (ensures the full §21 `konoha:*` label set exists in the
repo and prints it; `OWNER`/`REPO` override, `DRY_RUN=1` for shadow mode),
and `./prbot children` (runs the Analyst + Estimator + Splitter + child
creation; `DRY_RUN=1` prints the validated `ChildIssue` identities without
mutating).
- `observability.go` owns the §39 metrics sink + structured workflow logging:
  `RecordCycleMetrics` is an ACTIVITY (metrics are incremented once, never in
  replay-prone workflow code) that applies a `CycleOutcome` against a
  sync-guarded registry and recomputes the derived §39 gauges:
  `autonomous_completion_rate = autonomous closes / total completes`,
  `human_escalation_rate`, `split_rate`, `review_disagreement_rate`.
  `MetricsText`/`StartMetricsServer` export the registry with STABLE §39 names
  (no prometheus dep in go.mod — stdlib `/metrics` only; METRICS_ADDR env);
  `logCycleEvent` emits the §39 field set on every workflow transition.
- `webhook.go` owns the §32 event-driven fast-path: `StartWebhookServer`
  (needs `GH_WEBHOOK_SECRET` + `WEBHOOK_ADDR`) serves `/webhook` and verifies
  each delivery's `X-Hub-Signature-256` HMAC-SHA256 (constant-time, reject on
  bad/missing). `opened` → `DefaultStarter` starts `IssueCycleWorkflow` with the
  deterministic `<repo>-issue-<n>` ID and treats `WorkflowExecutionAlreadyStarted`
  as convergence (§32/§41 dual-sweep); `edited`/`labeled`/`closed` → reconcile
  only (never restart a completed workflow, §31). The sweep stays the backstop.
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
- `split.go` owns the Splitter agent (§4.3/§11/§36): decomposes a LARGE issue
  into validated `SplitTicket`s (`RunSplit`). `ValidateSplit` is the §36 gate:
  children > 0, unique non-empty keys, each complexity ≤
  `DevelopmentMaxComplexity`, non-empty requirements/acceptanceCriteria, and
  dependencies that resolve to a real child key (§10). Malformed or
  contradictory output fails closed and can never reach child-issue creation —
  the §16 hard invariant "no GitHub mutation from an unvalidated agent output".
- `children.go` owns child-issue orchestration (§8/§9/§10/§43): `ChildIssue` is
  the §9 deterministic identity (key + parent issue number + GitHub number);
  `EnsureChildren` idempotently creates/finds child issues via the existing
  `konoha-parent`/`konoha-child` markers + `childTicketBody` spec (managed+child
  labels, §21); `scheduleChildBatches` is the pure §10 DAG scheduler —
  dependency-respecting topological levels, cap-sized batches
  (`ConcurrentChildrenPerIssue`, §43), sorted deterministically, cycles/unknown
  deps fail closed so the workflow never blindly fans out an invalid DAG.
- `diagnosis.go` owns the §17 AI Diagnosis agent: `RunDiagnosis` is a fresh,
  **read-only** opencode execution (no GitHub write credential, §23) fed the
  structured `DevelopmentFailure` (issue, attempt history, review feedback,
  trigger reason). It emits a `DiagnosisResult` with a **bounded**
  recommendation `{retry, repartition, block}`; `ValidateDiagnosis` is the §36
  gate and `ApplyDiagnosis` (policy.go) is the deterministic transition —
  retry → `DEVELOPING`, repartition → `SPLITTING`, block → `HUMAN_REQUIRED`.
  The agent never applies its recommendation (§17/§44 #4); a malformed or
  unresolvable diagnosis fails closed to `HUMAN_REQUIRED` (§40, no silent
  success).
- `policy.go` owns the deterministic policy engine (§4.8): `EvaluateAutonomy`
  (§12 predicate), `EvaluateCompletion` (§14 gate, blocks on agent
  disagreement §18), `EvaluateClosePolicy` (§19 `autonomous`/`risk-based`/
  `human-required`, default risk-based), `EvaluateRepairLoop` (§16 bounded
  REQUEST_CHANGES loop: cap on development/repair attempts → §17 AI escalation),
  `EvaluateDisagreement` (§37/§18 gate: disagreement routes to Resolution and,
  past `defaultMaxResolutionAttempts` (1), escalates to `HUMAN_REQUIRED`),
  `POGateTimeout` (§33 default 168h) + `EvaluateHumanGate` (§18/§40: a timeout
  never resumes/closes — it stays `BLOCKED`, no silent success) + `ApplyHumanDecision`
  (maps §18 `resume|reestimate|repartition|retry|block|cancel|close` to a
  deterministic transition), `ApplyDiagnosis` (maps §17 `retry|repartition|block`
  to `DEVELOPING`/`SPLITTING`/`HUMAN_REQUIRED`; malformed → `HUMAN_REQUIRED`), `DetectHumanEscalation` (the §20 risk gate:
  `RiskDecision` from diff `paths` + issue `labels` — restricted globs/labels →
  escalate before publish/close), plus
  the `**`-aware `globMatch`
  for §20 `humanRequiredPaths`. Pure Go, zero LLM calls; inputs are only
  validated `EstimateResult`/config/observed facts, never issue text (§5/§27
  authority ordering). No agent can self-authorize a close (§44 #5) —
  `EvaluateCompletion` is conjunctive and `CompletionAgentComplete` alone is
  never sufficient.
- `human.go` owns the HUMAN_REQUIRED intervention surface (§18/§19/§40): the
  §18 reason set (`ValidateHumanReason`) and decision set (`ValidateHumanDecision`,
  §36 fail-closed), the bounded `HumanDecision` channel read by
  `ReadHumanDecision` (issue body/comments carrying the deterministic
  `<!-- konoha-decision: <action> -->` marker; newest comment wins; malformed
  fails closed), and the §21/§22 label projections (`HumanRequiredLabels`/
  `BlockedLabels`). The workflow's state constants (`HUMAN_REQUIRED`/`BLOCKED`/
  `CANCELLED`/`CLOSED`/`FAILED`, workflow.go §6) are the authoritative record;
  labels are only a projection (§44 #20).
- `labels.go` owns the §21/§22 label projection: the full `konoha:*` label set
  (`projectableLabels`), `ProjectLabels` — the PURE state→label mapping (exactly
  one complexity label, `estimate-large` for complexity>4, transient
  `review`/`changes-requested` only while active) — plus two reconcile
  activities: `EnsureRepoLabels` (create the §21 set if missing, idempotent) and
  `ApplyIssueLabels` (diff current labels → desired: add missing, drop stale
  projectable transients/old-complexity, leave non-konoha human labels alone;
  §24/§31). Temporal state drives it; labels never drive workflow state (§44 #20).
- `completion.go` owns the Completion agent (§4.7/§14/§29/§30): `RunCompletion`
  is a **separate** opencode execution (independent from the developer, §4.7)
  that re-checks the implementation against the machine-checkable acceptance
  criteria and emits a validated `CompletionResult` (`CriterionResult` per
  criterion, `Recommendation` ∈ `close|reopen|blocked`, §30). It only
  *recommends*: the workflow must run the §14 gate (`EvaluateCompletion`) and
  §19 close policy (`EvaluateClosePolicy`) before any `EnsureIssueState(CLOSED)`;
  `CompletionConflict` flags a `close` recommendation the policy denies so the
  workflow routes to §37 disagreement/resolution instead of ever force-closing
  (`LLMs propose; deterministic policy decides`, §44 #4).
- `disagreement.go` owns §37/§13/§18 disagreement handling: `DetectDisagreement`
  is the first-class reviewer↔completion conflict predicate (reference case:
  reviewer APPROVED while completion says not-done; unknown verdicts fail
  closed), and `RunResolution` is the Resolution agent — a fresh opencode
  execution fed the §37 structured inputs (requirements, developer summary,
  review result, completion result) that returns a validated, **bounded** action
  set: `re-review` | `repair` | `completion-recheck` | `escalate`. The agent
  never executes the action; `EvaluateDisagreement` (policy.go) is the
  deterministic gate — disagreement routes to resolution, and after the
  `defaultMaxResolutionAttempts` (1) it escalates to `HUMAN_REQUIRED` (fix #13).
  History logging of the disagreement + resolution is the workflow's job (the
  debug `./prbot resolve` prints the validated result JSON).
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
- `repair.go` owns the Repair agent (§4.6/§16/§27): `RunRepair` runs a **fresh**
  opencode execution — same model ok, never the developer's/reviewer's
  transcript — fed the Reviewer's **structured** findings (`ReviewResult`
  `Blocking`/`Nits`, §35). It reuses `runAgentInWorktree` (the shared §4.4/§4.6
  body extracted from `RunDeveloper`), resetting the owned worktree onto
  `PreviousCommit` (the SHA the review actually looked at, §15) so the repair
  commit sits on top of the reviewed commit. Returns a new local commit only
  (never pushes, §25). The bounded REQUEST_CHANGES loop is `EvaluateRepairLoop`
  (policy.go): a pure §16 decision answering "can another development attempt
  occur?" — within the `maxDevReviewAttempts` cap (default 2, counted per issue,
  1 = initial develop) it allows a repair, at/over the cap it returns
  `Escalate` so the workflow must route to the §17 AI-diagnosis path and never
  treat a REQUEST_CHANGES verdict as silent success (§40). Changing the verdict
  parser or prompt shape touches `parseReviewOutput`/`repairSystem` in one
  place each.
