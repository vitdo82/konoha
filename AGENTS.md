# AGENTS.md

Cross-repo PR review orchestrator. A Temporal worker (single `package main`) sweeps
repos for PRs lacking a bot review, reviews each via a local `opencode` agent, and
posts the verdict back as a GitHub App review.

## Engineering invariants

Agent MUST follow. These are build instructions, not process policy.

- Idempotency markers + the `VERDICT:`/`BLOCKING:`/`NITS:`/`CONCLUDE:` parser
  contract are byte-exact — never reword/reorder/config-drive them.
- Agents never receive `GH_*` credentials (§23); config is bootstrap-only, never
  read during workflow replay.
- Temporal registers activities/workflows by **function name, not package path**
  — never rename exported funcs during refactors (see #57); verify with
  `make test`.
- Workflows stay deterministic: all I/O in activities.

## References

Product/process policy — milestones, ticket format, delivery workflow, and the
non-configurable contract list — lives in `docs/best-practices.md`. That policy
is **NOT** build instruction; do not enforce it as an engineering gate.

## Verify

```bash
make test   # go vet ./... + go build ./... + go test ./... (§41 test suite)
```

`go test ./...` runs the full §41 matrix: activity/reconcile idempotency
(`reconcile_test.go`), every agent validator (§36), the derived §39 metric
gauges, the §53 alert threshold math + per-window dedupe and failure-feedback
idempotency (`alerting_test.go`), the §55 approval-gate policy + audit-sink
writes (`approval_test.go`), the §56 grooming marker idempotency + reject/
dry-run paths (`grooming_test.go`), the §44 invariant assertions
(`invariants_test.go`), and
Temporal-testsuite workflow runs (`workflow_suite_test.go`) — the agent
activities are mocked with testify `Anything` matchers (one per activity
argument) and `RecordCycleMetrics` runs for real, so the terminal §6 state is
assertable via the `state` query handler.

E2E (tier-1 hermetic, §47): `go test -tags=e2e ./e2e/...` runs the real worker +
a Temporal dev-server with fake `gh`/`opencode` on PATH and a local fixture git
remote (see `e2e/README.md`). Test-only seams: `KONOHA_TOKEN_FIXED` and
`KONOHA_GIT_BASE` are honored only in test contexts — never in a real deploy.
Scenarios 05-10 are `t.Skip` until the completion/human/LARGE wiring lands.

## Run

Needs a local Temporal server (`temporal server start-dev`, listens on
`127.0.0.1:7233`, override `TEMPORAL_ADDRESS`), the `gh` CLI on PATH, and GitHub
App credentials (never commit these):

```bash
GH_APP_ID=<id> GH_APP_INSTALL_ID=<id> GH_APP_PEM=<path> make run
./scripts/apply.sh    # (re)create one <repo>-review-all Temporal schedule per repos.yaml entry
                      # (+ <repo>-deps / <repo>-alerts when those are enabled)
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

- **Project layout**: all logic + tests live under `internal/konoha` (single
  package `konoha`); `cmd/konoha/main.go` is the only entrypoint (`Main()`
  owns CLI dispatch, worker start, and every `w.Register*` call). Config lives
  in `configs/`, scripts in `scripts/` (`apply.sh`). `repos.yaml` is read from
  `configs/` (override `KONOHA_CONFIG_DIR`).
- **Workflows vs activities** (`internal/konoha/workflow.go` vs `activities.go`): Temporal workflows
  must be deterministic (no I/O, no time in logic). All I/O — shelling out to `gh`
  and `opencode` — lives in activities. Keep it that way.
- **Workflows are registered by string name** (`cmd/konoha/main.go`, `Main()`);
  activity executions
  are serialized by signature. Changing a workflow/activity signature breaks in-flight
  executions and must happen in lockstep with the registration. Function names are
  package-path-independent — do NOT rename exported activity/workflow funcs during
  refactors (Temporal type names are the function names).
- **File moves are rename-free (#57)**: an `internal/` restructure is safe only
  while exported activity/workflow funcs keep their names (the Temporal type
  names). Never rename one as part of a move — a rename silently changes the
  registered type name; verify with `make test`.
- **Agent output contract** (`reviewer.go` `parseReviewOutput`, reused by `ReviewWithOpencode`):
  the opencode agent must print `VERDICT:APPROVE`/`VERDICT:REQUEST_CHANGES` plus
  `BLOCKING:`/`NITS:`/`CONCLUDE:` sections. Parsing is line matching, fail-closed
  to `REQUEST_CHANGES`. Changing the prompt/output shape requires changing this parser.
- **Config source of truth is `repos.yaml`.** `configs/repos.yaml` is loaded by
  `prbot config` (the shared Go loader, `yaml.v3` `KnownFields`)
  for each repo's schedule-input JSON; unknown keys and out-of-range values
  fail fast at apply time. The schema lives in `config.go`
  (`RepoConfig`/`LifecycleConfig`/`MergeConfig`/`DependencyConfig`/`AlertingConfig`, §§33/43/49/52/53) —
  adding keys to `repos.yaml` requires updating those structs, not grep/awk in
  `apply.sh`.
- **Runtime config is `konoha.yaml`** (`configs/`), resolved once at bootstrap
  as `env > file > bundled default` (`appconfig.go` → `appCfg`, fixed for the
  process lifetime so workflow replay is deterministic). `worker.*` holds the
  task queue, activity timeouts, retries, and worker capacity; `defaults.lifecycle.*`
  are the fallback policy when `repos.yaml` leaves a field unset (threaded into
  the policy helpers). Only deployment/host keys are env-overridable
  (`WORKER_MAX_CONCURRENT_ACTIVITIES`, `KONOHA_TASK_QUEUE`); policy/timeouts are
  NOT — a host env must not fork deterministic behavior. Contract/invariant
  strings (markers, `VERDICT:` parser, §6 states) are never config-driven.
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
- `merge.go` owns the §49 merge + CI status gate: `WaitForCI` polls
  `gh pr checks --json bucket,name` until every check passes or one fails,
  bounded by `merge.ciMaxWait`; a fail, an unclassifiable bucket, or a timeout
  returns `Green=false` (never a silent pass). `MergePullRequest` merges with
  the App token using the config-driven `merge.strategy` (whitelist
  `squash|rebase|merge`), is idempotent under retry (`§24` probe the PR state
  before and after a lost response, never double-merge), and binds
  `--match-head-commit` to the reviewed SHA (§15) so a moved head is refused.
  `ghTokenRunner` is the package var test seam (like `ghRunner`); do not call
  `activity.GetLogger` in these activities (panics on non-Temporal ctx). Config
  is `lifecycle.merge`, defaulting to `defaults.lifecycle.merge` in
  konoha.yaml (`strategy: squash`, `requireCI: true`, `ciMaxWait: 10m`).
- `release.go` owns the §50 post-merge release + changelog: after a successful
  merge, `IssueCycleWorkflow` starts `PostMergeWorkflow`
  (`<repo>-postmerge-<pr>`, deterministic §24) whose only I/O is the `RunRelease`
  activity. `RunRelease` parses the conventional-commit messages since the last
  tag (`feat`→minor, `fix`/`perf`→patch, `BREAKING CHANGE`/`feat!`→major, no
  match→patch — `DetermineBump`/`NextVersion` are pure and unit-matrixed),
  prepends `CHANGELOG.md` via the Contents API, then creates the tag + GitHub
  Release (`gh release create`). It is idempotent: an existing next tag or no
  commits since the last tag is a no-op, so retries/overlaps never double-release
  (§24). Opt-in per repo via `lifecycle.release.{enabled,changelogPath,
  versionPrefix}`; disabled/absent is a no-op and honors `DryRun` (§42). A
  release failure is logged but never changes the issue's terminal state (the PR
  already merged). `ghTokenRunner` is the shared test seam; never call
  `activity.GetLogger` in these activities (panics on non-Temporal ctx).
- `deploy.go` owns the §51 deploy + post-deploy verification: after §50, when
  `lifecycle.deploy.enabled`, `PostMergeWorkflow` runs `DeployRelease` (records
  a GitHub Deployment for the released tag, runs the config `deploy.command` via
  `sh -c` from the MAIN checkout — never the agent worktree — with
  `KONOHA_RELEASE_TAG`/`KONOHA_DEPLOY_ENV`) then `VerifyDeployment` (bounded
  `deploy.verify` poll up to `maxWait`). Deploy is idempotent by a tag-based
  guard: a tag whose latest deployment status is `success` is never
  re-deployed (§24). Failure semantics are fail-closed (§40): a failing hook
  marks the deployment `failure` and a permanently failing verify marks it
  `inactive` (the rollback signal when `rollbackOnFail`), and
  `PostMergeWorkflow` routes the issue to `HUMAN_REQUIRED` by reopening it and
  projecting `konoha:human-required` — a failed deploy/verify is never reported
  as deployed. Disabled/absent and `DryRun` (§42) are no-ops (no remote calls).
  `ghTokenRunner` is the shared test seam; never call `activity.GetLogger` in
  these activities (panics on non-Temporal ctx).
- `deps.go` owns the §52 dependency + security automation: a per-repo
  `<repo>-deps` Temporal schedule runs `DependencyWorkflow` (created by
  `apply.sh` via `prbot config --deps`, only for `lifecycle.deps.enabled`),
  which calls `ScanDependencies` (runs config `scanCmd` via `sh -c`; its output
  must be a JSON array/object of `{package,version,id,severity,summary}` —
  empty `id` = outdated bump, non-empty = vulnerability) then
  `EnsureDependencyIssue` per finding. Filing is idempotent (§24) by the
  byte-exact `<!-- konoha-dep: <pkg>@<id|version> -->` marker (search-before-
  create, like the child markers); a vulnerability issue also carries the
  built-in `konoha:security` label, everything else `konoha:dependency`. The
  workflow NEVER starts `IssueCycleWorkflow` — the issue has no `konoha:managed`
  label, so the existing sweep/webhook drives it through the normal lifecycle
  (no double-start). A malformed scan or entry fails closed (§36/§40), never
  "0 findings".   `lifecycle.deps.{enabled,scanCmd,schedule,label,autoFileIssues}`;
  disabled/absent ⇒ no scan, no issues; `DryRun` files nothing (§42).
- `grooming.go` owns the §56 backlog-grooming pre-stage: an opt-in per-repo
  `<repo>-groom` Temporal schedule (created by `apply.sh` via `prbot config
  --grooming`, only for `lifecycle.grooming.enabled`) runs
  `BacklogGroomWorkflow` → `ListBacklogItems` (open issues carrying
  `grooming.selector`, default `backlog`), then for each item `AnalyzeIssue` +
  `EstimateIssue` in **DRY** (§42: analysis only). An actionable item is filed as
  one fully-specified ready issue by `EnsureReadyIssue`, idempotent (§24) by the
  byte-exact `<!-- konoha-groomed: <sha> -->` marker (search-before-create);
  `<sha>` is a content hash of the raw item (owner/repo/number + title + body),
  so overlapping runs/retries converge and an edit re-grooms. The ready issue
  gets `readyLabel` (default `konoha:ready`, a source label in `labels.go`) plus
  optional `repoLabel`, and deliberately NO `konoha:managed` label — the existing
  sweep/`opened` webhook drives it through the normal lifecycle, so
  `BacklogGroomWorkflow` never starts an `IssueCycleWorkflow` (§32/§52 pattern).
  An unactionable/ambiguous item gets an idempotent note comment by
  `RecordGroomNote` (marker `<!-- konoha-groom-rejected: <sha> -->`) and is left
  OPEN, never closed/edited (§40: no silent drop); a per-item analysis failure is
  a reject, never a workflow failure. The run records the §39-style
  `groomed_count`/`groom_rejected_count` counters + `groom_reject_rate` gauge via
  `RecordGroomMetrics`. `ghRunner` is the shared test seam; never call
  `activity.GetLogger` in these activities (panics on non-Temporal ctx).
  `lifecycle.grooming.{enabled,selector,schedule,repoLabel,readyLabel}`;
  disabled/absent ⇒ no groom, no ready issues.
- `alerting.go` owns the §53 operations/alerting layer on top of the §39 sink:
  a per-repo `<repo>-alerts` Temporal schedule (created by `apply.sh` via
  `prbot config --alerts`, only for `lifecycle.alerting.enabled`) runs
  `AlertWorkflow` → `EvaluateAlerts`, which snapshots the derived §39 rate
  gauges and emits one alert per violated metric per window. Direction is
  intrinsic to the metric (`autonomous_completion_rate` alerts below; the other
  rates and `cycle_duration_seconds_last` alert above), so config is just
  `thresholds: {<metric>: <number>}` + an optional `onRates` subset — the
  alertable set is closed and a typo fails `apply.sh`. Emission is idempotent
  per `(repo, metric, window)` via a process-local `(repo, metric, window)` key
  (single-worker deployment model); a webhook (`notify`) failure is fail-closed
  and the key is not recorded, so the retry re-attempts. Alerting NEVER resolves
  a stuck run (§40/§44 #1). The same flag gates failure feedback: a terminal
  `BLOCKED`/`FAILED` cycle runs `FailureFeedbackWorkflow` (`<runId>-failure`)
  whose only I/O is `EnsureFailureIssue` — idempotent by the byte-exact
  `<!-- konoha-failure: <runId> -->` marker (search-before-create, §24), filing
  at most one bug issue per run with the structured `CycleOutcome` +
  `DevelopmentFailure`. The issue carries NO `konoha:managed` label, so it stays
  a normal issue and the existing sweep/webhook drives it back through the
  lifecycle (self-healing stream, no double-start, §32). `DryRun` logs alerts
  but never POSTs and files nothing (§42).
- `propagation.go` owns the §54 cross-repo change propagation: `PostMergeWorkflow`
  runs `runPropagation` after a merge (independent of §50/§51, so a release/deploy
  failure never blocks it). `DetectAffectedDownstreams` is a **read-only** activity
  — it reads the merged PR's touched files (`gh pr diff --name-only` via the
  `ghRunner` seam) and matches them against `lifecycle.propagation.downstream`
  with the shared `globMatch` (each entry is `{trigger: <changed-file glob>,
  type: <kind>}`). `MatchAffectedDownstreams` is the PURE, deterministic matcher
  (sorted repo-key order, fan-out capped by `maxPerMerge`, default 3, §43-style).
  `EnsureCrossRepoIssue` is the idempotent mutation (§24): search-before-create by
  the byte-exact `<!-- konoha-cross: <owner>/<repo>#<sha> -->` marker (upstream
  repo + commit), labels the issue `konoha:cross-repo` (a source label — never
  projectable, so `ApplyIssueLabels` cannot strip it; defined in `labels.go`) and
  deliberately does NOT add `konoha:managed`. Because the issue is unmanaged, the
  downstream repo's OWN sweep/`opened` webhook starts its `IssueCycleWorkflow`
  with the downstream `RepoScope`/lifecycle — Konoha never starts that cycle here,
  so B's phase/autonomy/§20 gates apply unchanged and nothing double-starts
  (§32/§52). A missing `Sha` fails closed (an un-dedupable create is never made);
  disabled/absent config is a no-op and `DryRun` (§42) keeps the analysis but
  files nothing. An enabled map needs ≥1 downstream; every entry needs `trigger`
  and `type`; a negative `maxPerMerge` fails at `apply.sh` (`config.go` validate).
- `approval.go` owns the §55 pre-execution approval gate + audit: `GatedSteps`/
  `ApprovalRequired` resolve the opt-in `approval.preGate` + `requiredSteps`
  (closed set `publish|merge|deploy`; empty = all; an unknown step fails
  `apply.sh`). `AwaitApproval` is the ACTIVITY that polls the §18 decision
  channel (reusing `ReadHumanDecision` verbatim — same `konoha-decision` marker,
  same §36 validation) until `approve`/`deny` or the `poGateTimeout`; `approve`
  (and the legacy `resume`) grants, any other valid decision denies, and a
  timeout returns `TimedOut` with no error so the workflow **blocks, never
  proceeds** (§40/§44 #17). `approve`/`deny` were added to the bounded §18 action
  set. The gate is approve-once (the marker is not step-scoped): `requiredSteps`
  selects which steps wait, and one sign-off then lets the gated pipeline
  proceed — per-step sign-off would need an `approve:<step>` marker. `awaitApprovalGate` is the shared workflow-side helper (used before
  publish/merge in `IssueCycleWorkflow`, before deploy in `PostMergeWorkflow`).
  `RecordAuditEvent` appends one JSONL line (subject, step, actor, decision) to
  the append-only `audit.sink`; `audit.enabled` without a sink, or a pre-gate
  without an enabled audit, fails at load, and a write failure is fail-closed
  (an un-recorded gate breaks the guarantee — AC "every gated decision is
  recorded" holds only when the audit is on, which load-time validation forces).
  `DryRun` (§42) skips the gate (nothing mutates). Labels are only the
  `konoha:pending-approval` projection (`labels.go`).
- **Built-in security gate** (`policy.go`): `hasEscalationLabel` always treats
  `konoha:security`/`konoha:dependency` as `humanRequiredLabels`, so
  `DetectHumanEscalation` (pre-publish) and `EvaluateClosePolicy` escalate them
  even under `closePolicy: autonomous`. This is what guarantees a §52 CVE/dep
  bump is never auto-merged; a bare (non-konoha) `security` label still only
  escalates when configured. `sourceLabels()` in `labels.go` are created by
  `EnsureRepoLabels` but are NOT projectable (never stripped as stale).
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
mutating), and `./prbot release` (runs the §50 `RunRelease` activity directly
and prints the `ReleaseResult`; `OWNER`/`REPO`/`BASE_BRANCH`/`CHANGELOG_PATH`/
`VERSION_PREFIX`/`RELEASE_ENABLED`/`DRY_RUN` env overrides; no Temporal), and
`./prbot deps` (runs the §52 `ScanDependencies` activity directly and prints the
validated `DependencyReport`; `OWNER`/`REPO`/`REPO_PATH` plus `SCAN_CMD`
required; no Temporal).
- `workflow.go` owns the lifecycle state machine: `IssueCycleWorkflow` now
  wires the **SMALL autonomous path** (§46): DEVELOPING → §20 risk gate
  (suppresses publish on restricted paths/labels) → publish (idempotent PR) →
  REVIEWING → bounded repair loop (§16, `RunRepair(PreviousCommit=<reviewed
  SHA>)`, cap → BLOCKED / never close) → APPROVED (commit-bound §15) → §49 CI
  gate (`WaitForCI`) → `MergePullRequest` (idempotent, config-driven strategy;
  `--match-head-commit` binds the reviewed SHA) → CLOSED → §50
  `PostMergeWorkflow` (release when `release.enabled`; a release failure is
  logged, never changes the terminal state). A non-green CI gate or
  a merge error escalates to `HUMAN_REQUIRED` and NEVER closes — a closed issue
  always means its PR actually merged. Every terminal funnels through
  `recordTerminal` → `RecordCycleMetrics`
  (autonomous vs human/blocked, §39); a terminal `BLOCKED`/`FAILED` also starts
  the §53 `FailureFeedbackWorkflow` when `alerting.enabled`. The §55 pre-execution
  approval gate (`PENDING_APPROVAL`) runs before publish and merge when
  `approval.preGate` is on; a deny → `HUMAN_REQUIRED`, a timeout → `BLOCKED`
  (never a silent proceed). Follow-ups NOT yet wired: completion gate
  (§14/§19), disagreement (§37), diagnosis → HUMAN_REQUIRED (§17/§18), and the
  LARGE path (split/children — currently ends at the deterministic `NOT_WIRED`
  terminal, never a silent drop). All I/O stays in activities (replay-safe).
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
- `rollout.go` owns the §42 staged-rollout gate: `rolloutPhase` derives a repo's
  effective phase (shadow / phase-2 / phase-3) purely from config — unset or
  review-only mode, dryRun, or a non-risk-based close policy all resolve to
  conservative **shadow** (Phase 1); ceiling ≤ 2 is **phase-2**; ceiling 3-4 +
  risk-based is **phase-3**. `effectiveDevelopmentMax` is the phase-gated routing
  ceiling (0 in shadow), and `phaseCanDecompose` enables §8 child fan-out only
  past shadow with a ceiling ≥ 4. Every mutating activity honors `DryRun`, so
  shadow mode runs the whole pipeline with zero GitHub mutations.
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
  `SubmitReview` (activities.go) posts the verdict as a formal `gh pr review`; the
  §25 publish path authors PRs with the same App, so GitHub refuses a formal
  `--request-changes`/`--approve` review on a self-authored PR. Only that
  specific "your own pull request" error (`selfReviewError`) falls back to a
  non-blocking `--comment` review (which still clears the `ListUnreviewedPRs`
  filter); any other failure propagates (§40). It runs through `ghTokenRunner`.
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
