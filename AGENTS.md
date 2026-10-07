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

Security model — trusted vs untrusted inputs, the agent/validation env
allowlist, argv-only gates, worktree isolation, and the host-execution trade-off
— lives in `docs/security.md` (§23/§27/#90).

Console threat model — the local review console's bind boundary (loopback-only),
Origin allowlist + 403, bearer/CSRF auth, untrusted-artifact handling, the no-
GitHub-credential rule, local evidence-file permissions, and the §55 audit, each
mapped to its test — lives in `docs/security-console.md` (#111). It is
**documentation plus doc-enforced assertions**: no production behavior changed.

Non-goals decision record — the ESF-vs-Konoha **hard non-goals** (sandbox, web
UI, OpenTelemetry, enforced egress, speculative fan-out) and the caveated
features that are implementable with stated boundaries (host resource ceilings,
advisory egress, policy tenancy) — lives in `docs/non-goals.md` (#101). It is
**documentation only**: no build instruction, and no production behavior.

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
- **OpenAPI-first console API + generated code (#124)**: `api/openapi.yaml`
  (OpenAPI 3.1) is the canonical contract for the console/worker API. Regenerate
  the Go types/client (`internal/konoha/consoleapi/api.gen.go`, oapi-codegen) and
  the TS types (`api/generated/ts/api.gen.ts`, openapi-typescript) with `make api`
  (`api/generate.sh`) — **never hand-edit a generated file** (they carry a
  `Code generated … DO NOT EDIT` header); change the spec and regenerate.
  `internal/konoha/console_routes.go` is the single route-registration source and
  `console_contract_test.go` fails if a route/method/auth tier or a response
  field drifts from the spec. `api/generated/ts/client.ts` is a thin dependency-free
  `fetch` client typed by the generated spec (`make api-ts-test` typechecks +
  smokes it). The CLI reads the console through the generated Go client
  (`prbot api <status|metrics|cycles|cycle|artifacts|queue>` in `console_client.go`)
  rather than ad-hoc HTTP.
- **Workflows vs activities** (`internal/konoha/workflow.go` vs `activities.go`): Temporal workflows
  must be deterministic (no I/O, no time in logic). All I/O — shelling out to `gh`
  and `opencode` — lives in activities. Keep it that way.
- **Typed activity retries + agent attempt provenance (`retry.go`, #95)**:
  `retryPolicyFor`/`activityOptionsFor` assign one of four retry classes at the
  call site — `deterministic` (`MaximumAttempts: 1`; pure/derived records where a
  retry would double-count), `infrastructure` (`worker.retryMaxAttempts`, default
  5, with exponential backoff), `agent` (exactly 1; a transport-loss retry must
  not replay an LLM) and `verification` (3; CI/deploy gates). Long agent
  activities also get a `HeartbeatTimeout`; `startAgentHeartbeat`/`runAgentCombined`
  beat while the child runs so a hung agent surfaces as a timeout, not a
  `StartToCloseTimeout` stall. Agent activities stamp the Temporal
  `activity.GetInfo(ctx).Attempt` onto their result (`activityAttempt`, safe
  outside Temporal) so the attempt lands in `result.json`. The sweeps keep their
  own `sweepRetryMaxAttempts` policy (`ReviewAllWorkflow`/`IssueAllWorkflow`).
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
- `harness.go` owns the §85 name-based agent harness registry: the `Harness`
  interface (`Name`/`Executable`/`Args`/`Env`/`PromptMode`/`ResolveModel`), a
  name-keyed `Registry` (sorted `Names()`, `Lookup`, best-effort `Availability`),
  and the built-ins `opencode` and `generic`. `opencode` is byte-for-byte
  today's `agentArgs`/`cmdEnv`; `generic` runs an operator-declared bare
  `executable` with `args`/`modelFlag`/`passEnv`/`promptOnStdin` from the
  top-level `harness:` block in konoha.yaml (`{{model_args}}`/`{{prompt}}`
  expand). **A repo names a harness, never a path**: a path-like `executable` is
  rejected at load, as is an unknown `lifecycle.harness` /
  `defaults.lifecycle.harness`; `GH_*` is stripped for every harness and
  `generic` forwards only `passEnv`-listed vars (§23). Every agent exec routes
  through `agentCommand`, which resolves the harness + an explicit model
  (malformed → clear error, never adopted silently). The registry is internal,
  so no exported activity/workflow func is renamed (Temporal type names).
  **Optional provisioning capabilities (#96)**: `agentCommand` first runs
  `prepareAgent`, which type-asserts optional interfaces (`Provisioner`/
  `CatalogCacher`/`Versioner`/`MCPConfigurer`) plus a `capabilityDeclarer`
  (`generic` enables them from config; `opencode` has none). `binary` +
  `binary_sha256` stage the pinned binary into the agent's git-admin staging dir
  after verifying the digest (mismatch fails closed); `catalog_cache` stages a
  model catalog and `resolveModelFromCatalog` selects offline; `prompt_mode:
  stdin_file` writes the prompt outside the tracked tree and redirects stdin
  (never argv); an `mcp` block writes the worktree-relative config and
  git-excludes it. Every capability records `APPLIED`/`SKIPPED`/`FAILED`
  (`CapabilityOutcome`, never a false success §40) into `provisioning.json`
  evidence; `--version` is best-effort and never fails a run.
- `modelprovider.go` + `modelprovider_adapters.go` own the #118 model-provider
  protocol abstraction — a **standalone foundation**, NOT wired into the agent
  harness (agents still run through `harness.go`; this feeds #PL1 profiles and
  #PL6 secrets). `ModelProvider` (`Name`/`Protocol`/`Invoke(ctx, Envelope)
  (Envelope, error)`) is implemented by three wire adapters — OpenAI Responses,
  OpenAI Chat Completions, Anthropic Messages — over a canonical `Envelope`
  (messages/tools in; text/tool-calls/`HarnessUsage` out). Protocols are the
  closed set `openai-responses|openai-chat|anthropic-messages`; gateways
  (`openrouter|litellm|ollama`) are **OpenAI-chat base-URL presets**, not a fourth
  adapter. Config is `defaults.providers.<name>` (`protocol`|`gateway`,
  `baseUrl?`, `apiKeyEnv?`); an unknown protocol/gateway, a gateway/protocol
  mismatch, or a non-bare `apiKeyEnv` fails at load (`AppConfig.validate`), and
  again in `newProviderRegistry`. A `retryingProvider` decorator applies the #95
  contract: `transportError`/5xx retried with backoff, a 4xx `httpStatusError`
  fails closed in one attempt. Credentials are resolved **by name** at Invoke
  time via `SecretResolver` (default env), registered with the process redactor
  (#82), and never placed in the envelope, an error, a log, or a
  `ProviderDescriptor` (which records the `apiKeyEnv` NAME only). Adds no
  exported activity/workflow func (no Temporal type-name change).
- `machine.go` owns the #121 machine-provider abstraction — the execution fabric
  the #92 evaluation deferred. It exists only because #92 returned **do not adopt
  a sandbox now** (`docs/security.md` → "Sandboxed execution evaluation");
  `docs/non-goals.md` → A1 is the revisit trigger. `Provider`
  (`Name`/`Capabilities`/`Create`/`Reattach`/`Destroy`/`List`/`Ping`) is the
  **workflow-only** infrastructure handle; `Machine`
  (`Execute`/`ReadFile`/`WriteFile`/`Destroy`) is the **agent-facing** handle.
  `Command{Argv, Dir, Env, StdinPath, Timeout}` is **structured argv only** —
  there is deliberately no shell-string field. Capability negotiation
  (`negotiateCapabilities`/`requireCapabilities`, closed set
  `snapshot|clone|suspend|preview`) records a missing capability `SKIPPED` (the
  #96 `CapabilityOutcome` vocabulary) and fails closed on a required-but-missing
  one (§40). The built-in `local` provider (the default, selected by NAME via
  `machineRegistry`) reproduces today's host/worktree behavior: `Create` is the
  §4.4-owned `ensureWorktree`, `Execute` runs a bare argv child with the §23
  scrubbed env + #103 advisory egress + #102 ceilings (a caller env can never
  re-add a stripped credential), `Destroy` honors the claim via `cleanupWorktree`.
  Ownership/adaptor boundary (#121/#142): only the workflow holds a `Provider`;
  an agent operates inside its `Machine` and never creates/reattaches/destroys
  one. `machine.go` references **no** GitHub runner, policy gate, label
  projection or reconciliation, so an adapter cannot bypass claim authority,
  workflow policy, review gates or tracker reconciliation. It is a **standalone
  foundation, NOT wired into the agent path yet** (agents still run through
  `harness.go`), and adds no exported activity/workflow func (no Temporal
  type-name change). No sandbox provider is added until #92 is re-opened.
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
- **Resource catalogs (`resources.go`, §98)**: `defaults.agents.models:<name>
  {provider, model, baseUrl?, apiKeyEnv?}` and `defaults.agents.budgets:<name>
  {maxCostUsd?, maxTokens?, maxWallClock?}`. A repo/stage selects by NAME
  (`lifecycle.models.<stage>` for analyst/estimator/developer/reviewer/repair/
  completion, `lifecycle.budget`), falling back to the legacy `lifecycle.model`
  raw id. The resolved provider/model + a content `Digest` land in
  `outcome.json` (`CycleOutcome.Resources`) and each develop/repair activation
  records `Stage`/`ModelDigest` in lineage; unknown names and unknown stage keys
  fail at load (`loadReposConfigWithResources` / `validateResourceRefs`) and
  again closed at resolve time. `apiKeyEnv` is a NAME only — a credential value
  is never read or recorded. Budget crossings are computed from #87 usage +
  elapsed time and are EVIDENCE ONLY: they never rewrite a verdict or shorten a
  configured timeout (§40). Egress policies and workspaces are deliberately not
  modeled — there is no sandbox (docs/security.md), so they are documented as
  not-applicable, never stubbed.
- **Policy tenancy scopes (`scopes.go`, #104)**: an optional `scopes:` block in
  `konoha.yaml` (Product Line → Department, plus a top-level `facility:` label)
  is a **narrow-only** policy layer, resolved **once at load** by
  `resolveScopes` inside `loadReposConfigWithResources` (`prbot config` /
  `apply.sh` / worker bootstrap). A scope may only reference repos already in
  `repos.yaml`, built-in harness names, and `defaults.agents` model/budget
  names — an unknown reference fails at load; `closePolicy` may only become more
  human-gated and `approval` may only turn the §55 gate ON (which still requires
  `lifecycle.audit.enabled`). A `repos.yaml` entry is mapped by its
  `productLine`/`department` or, by default, by repository membership in the
  scopes' `repositories` lists. The resolved scope name lands on
  `LifecycleConfig.Scope` (yaml-only-resolved, `yaml:"-"`), travels in the
  schedule input (no workflow ever resolves a scope name), is recorded in
  `outcome.json` (`CycleOutcome.Scope`), and is part of the #88 policy digest, so
  a scope change is a visible diff. This is **policy** tenancy, **not execution
  isolation**, and introduces no per-tenant GitHub App credential — `GH_*` is
  unchanged (§23); see `docs/non-goals.md` → B3 (#101). `scopes.go` adds no
  exported activity/workflow func, so no Temporal type name changes.
- **Declarative agent profiles (`profiles.go`, #117)**: a top-level `agents:`
  block in `konoha.yaml` bundles the per-agent layers — harness (#85), model
  (#98 catalog NAME), limits (#102), plus `tools`/`mcp`/`promptMode` — into named
  profiles. `repos.yaml` selects one per stage via `lifecycle.agents.<stage>`
  (`analyst|estimator|developer|reviewer|repair|completion`) with a repo-level
  `lifecycle.agents.default`. `resolveAgentProfiles` runs **once at load** inside
  `loadReposConfigWithResources` (before `resolveScopes`, so a scope narrows the
  profile-resolved harness/model), flattening the selection into the existing
  `LifecycleConfig.Harness`/`Models`/`Limits` so **no workflow-side name
  resolution** ever occurs. The resolved stage→profile map + a content digest
  land on `LifecycleConfig.Profiles`/`ProfileDigest` (`yaml:"-"`, carried in the
  schedule input), in `outcome.json` (`CycleOutcome.Profiles`/`ProfileDigest`),
  and in the #88 policy digest. Unknown profile/harness/model names, an empty or
  duplicate tool/MCP entry, two selected profiles that disagree on the single
  harness/limits the pipeline can express, and a **conflicting** legacy
  `lifecycle.harness`/`model`/`models`/`limits` key all fail at load. A repo with
  no `lifecycle.agents` is byte-for-byte the legacy behavior (the implicit
  profile). `tools`/`mcp` tool/MCP references are validated against the #119
  catalogs at load (see the tool-registry note below). `profiles.go` adds
  no exported activity/workflow func (no Temporal type name changes).
- **Tool registry + permissions + MCP registry (`tools.go`, #119)**: the
  top-level `tools:` block declares `custom` tools (id → description) with the
  built-ins code-level (`builtinToolCatalog`), and `mcpServers:` names HTTP MCP
  servers (`{url, headers}`). A profile's `tools` is an advisory **allow** list;
  `deny` is a hard restriction. `resolveAgentProfiles` validates every ref
  against the catalogs (an unknown tool/server **fails at load**), rejects a
  tool both allowed and denied, and fails closed on any `deny` entry (the bundled
  harnesses receive no tool allowlist, so a denied tool cannot be removed —
  running anyway would be broader capability than declared, §40). The resolved
  policy is the union of the selected profiles, stored on
  `LifecycleConfig.Tooling` (`yaml:"-"`, carried in the schedule input), handed
  to the agent path via `AgentTooling` on the request structs, and recorded in
  `outcome.json` (`CycleOutcome.Tooling`) + the #88 profile digest. `agentCommand`
  enforces it: a deny list fails closed and MCP server names resolve from the
  process registry (`resolveMCPServerSpecs`), unknown failing closed. A
  supporting harness (`MCPConfigurer`, #96) receives only the resolved set and
  renders it into its git-excluded config (`renderMCPConfig`); a harness without
  the capability records `mcp` `SKIPPED` — never a false success. Resolution
  reads **only** konoha.yaml/repos.yaml, so issue text can never grant a tool or
  server (§27). `tools.go` adds no exported activity/workflow func (no Temporal
  type name changes).
- Child workflow IDs are stable per repo+PR (`<repo>-review-<n>`) — that's what makes
  sweeps idempotent/review-once. Don't break the shape.
- **Idempotent GitHub mutation** (`reconcile.go`): the `Ensure*` activities are
  search-before-create and rely on deterministic HTML-comment markers
  (`konoha-parent`, `konoha-child`, `konoha-managed`) matched byte-exactly in
  issue/PR bodies.
  `ghRunner` is a package var so tests simulate GitHub; never call
  `activity.GetLogger` inside these activities (it panics on non-Temporal ctx, which
  the unit tests use).
- PR idempotency for the publish path lives in `publish.go` (`EnsurePullRequest`,
  search by head branch, not by marker). `publish.go` also owns the §26
  verification runner: `validateRepo` runs the repo's structured
  `lifecycle.validation` gates as bare argv (never a shell string), in order and
  with no early-exit, and `Passed` is true iff every **mandatory** gate exits 0
  while any gate that cannot start records `ERROR` and fails closed (§40). Gates
  run with the scrubbed agent env (§23, no `GH_*`) in a repo-relative `dir`
  (traversal rejected at load and again at run). Each gate's streams land in the
  cycle's `verification/<id>.stdout|stderr` evidence (redacted). `validateRepoWithDrift`
  (#94) snapshots `git status --porcelain` before/after the run: a gate that
  rewrites a tracked file is recorded as `verificationMutated=true` on
  `outcome.json` plus a `verification/post-verification.patch` (a warning is
  logged). Drift is EVIDENCE ONLY — it never changes the delivered commit SHA or
  the terminal §6 state; both the structured-gate and legacy `validationCmd`
  paths are covered. The legacy
  `validationCmd` shell string is the fallback; declaring both is a load-time
  error (`config.go` `validateGates`), and an absent profile behaves exactly as
  before (skipped). `ValidationGates`/`ValidationCommand` (policy.go) resolve the
  per-repo list, falling back to `defaults.lifecycle`. The workflow threads the
  resolved profile into the dev/repair activities only, so `dev.Validated` feeds
  the §84 `verificationOutcome`; publish keeps its existing policy gate.
- `prdesc.go` owns the #115 AI-generated PR description (§25), an opt-in
  (`lifecycle.publish.aiDescription`, default off) bounded agent step.
  `RunPRDescription` runs a fresh opencode execution over the already-validated
  artifacts (issue spec, developer summary, changed files, §26 validation
  evidence) — never the developer/reviewer transcript — embedding issue-derived
  text in the `UNTRUSTED` markers (§27), and returns a structured
  `PRDescription` (summary/changes/risks/validation) parsed/validated
  fail-closed (`parsePRDescriptionJSON`/`ValidatePRDescription`). `renderPRDescription`
  prepends the deterministic `Closes` line, clamps fields, redacts (#82), and
  length-bounds the body. The workflow calls it before `EnsurePullRequest`; any
  failure falls back to the deterministic `prBody` and the PR still opens
  (§40 fail-open to the safe default). The body is EVIDENCE ONLY — the
  reviewer/§20 gate/§49 merge/§19 close read `a`/`dev`, never the body, so it
  can never change a verdict (`RetryClass` is `agent`, exactly 1). It adds one
  registered activity (`RunPRDescription`), no renamed funcs.
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
  (single-worker deployment model); a target delivery failure (#123) is
  fail-closed and the key is not recorded, so the retry re-attempts. Alerting
  NEVER resolves
  a stuck run (§40/§44 #1). The same flag gates failure feedback: a terminal
  `BLOCKED`/`FAILED` cycle runs `FailureFeedbackWorkflow` (`<runId>-failure`)
  whose I/O is `EnsureFailureIssue` — idempotent by the byte-exact
  `<!-- konoha-failure: <runId> -->` marker (search-before-create, §24), filing
  at most one bug issue per run with the structured `CycleOutcome` +
  `DevelopmentFailure` — plus, when a #123 target is configured, a
  `NotifyCycleEvent` delivery for the filing. The issue carries NO
  `konoha:managed` label, so it stays
  a normal issue and the existing sweep/webhook drives it back through the
  lifecycle (self-healing stream, no double-start, §32). `DryRun` logs alerts
  but never POSTs and files nothing (§42).
- `notify.go` owns the #123 notification providers on top of §53: a `Notifier`
  interface (`Notify(ctx, NotifyEvent) error`) with three implementations —
  `slackNotifier` (Slack incoming webhook, `{"text": …}`), `webhookNotifier`
  (generic JSON `{kind,repo,text,dedupeKey}`), and `logNotifier` — so a
  delivery target is decoupled from the §39 metrics layer (which is untouched).
  Event kinds are the closed set `alert|terminal|failure`: §53 metric alerts
  (`EvaluateAlerts` → `deliverEvent`), terminal cycle outcomes
  (`notifyTerminal`, called from `recordTerminal` for every
  `CLOSED`/`BLOCKED`/`FAILED`/`HUMAN_REQUIRED`), and failure-feedback filings
  (`FailureFeedbackWorkflow`). Targets are `lifecycle.alerting.targets:
  [{type,url}]` (the legacy single `notify:` URL folds in as a Slack target);
  an unknown type, a missing `url` for slack/webhook, or a `url` on a log target
  fails at load (`AlertingConfig.validate`), and `newNotifier` fails closed
  again. Delivery is fail-closed + idempotent (§40/§24): the first target
  error is returned so the activity (retry class `infrastructure`) retries, the
  failed target's dedupe key is NOT recorded, and a delivered
  `(target, dedupeKey)` is skipped — a retry can never double-notify.
  `NotifyCycleEvent` is the single workflow-side delivery activity (registered
  in `cli.go`); `DryRun` (§42) logs and sends nothing; a notification problem
  only logs and never changes the §6 terminal state. No exported
  activity/workflow func is renamed (no Temporal type-name change beyond the
  added `NotifyCycleEvent`).
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
  set; #108 adds `request-changes` (`EvaluateApproval` → `approvalRework`,
  `ApprovalResult.Rework`/`Feedback`), which `IssueCycleWorkflow` handles at the
  publish gate by running a bounded `RunRepair` carrying the human feedback and
  re-gating, and fails closed (HUMAN_REQUIRED) at merge/deploy where there is no
  in-place repair path. The gate is approve-once (the marker is not step-scoped):
  `requiredSteps` selects which steps wait, and one sign-off then lets the gated
  pipeline proceed — per-step sign-off would need an `approve:<step>` marker.
  `awaitApprovalGate` is the shared workflow-side helper (used before
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
required; no Temporal), and `./prbot status <repo> <issue>` (queries the running
cycle's `status` handler via the Temporal client and prints the JSON snapshot),
and `./prbot doctor` (operator preflight: `konoha.yaml`/`repos.yaml` load,
Temporal reachability, `gh` present + authenticated, the selected harness on
PATH, and the evidence dir writable; exits non-zero with a specific message per
failure; no Temporal needed for the readers), and `./prbot logs <repo> <issue>`
/ `./prbot describe <repo> <issue>` (print the cycle's agent streams +
`validation.txt`, and a human-readable `outcome.json` summary — state, agent/
verification outcomes, conditions, usage — from `evidence.dir`; no Temporal; the
#97 `./prbot describe change <repo> <issue>` view is unchanged).
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
  `status.go` adds the read-only `status` query handler to both
  `IssueCycleWorkflow` and `PrReviewWorkflow`, returning
  `{state, currentStep, conditions, agentOutcome, verificationOutcome}`; it is
  additive to the unchanged `state` handler, and the per-step `conditions` are
  appended in workflow code (never activities) so replay reconstructs them.
  `prbot status <repo> <issue>` queries it via the Temporal client; the pure
  projection (`statusJSON`) is unit-tested without a server.
- `IssueAllWorkflow` is the issue-sweep analog of `ReviewAllWorkflow` (§32):
  lists open issues lacking `konoha:managed` and starts a stable
  `<repo>-issue-<n>` child `IssueCycleWorkflow` per issue, capped by
  `ConcurrentIssueCycles()` (§43). A failing child is NEVER silently skipped
  (§40/§44 #1): `escalateFailedChild` projects the affected issue to the
  human-required/blocked terminal via the idempotent `ApplyIssueLabels`
  reconcile and starts the §53 `FailureFeedbackWorkflow` for the run (marker key
  = the child's stable ID, so a retried sweep files at most one bug issue, §24),
  honoring `DryRun` (§42). Both are best-effort and per-child, so one failing
  child neither aborts the batch nor fails the sweep — the siblings still run
  (§40 fail-closed, not fail-fast). `failedChildOutcome` is the pure, invariant-
  testable projection.
- `observability.go` owns the §39 metrics sink + structured workflow logging:
  `RecordCycleMetrics` is an ACTIVITY (metrics are incremented once, never in
  replay-prone workflow code) that applies a `CycleOutcome` against a
  sync-guarded registry and recomputes the derived §39 gauges:
  `autonomous_completion_rate = autonomous closes / total completes`,
  `human_escalation_rate`, `split_rate`, `review_disagreement_rate`.
  `MetricsText`/`StartMetricsServer` export the registry with STABLE §39 names
  (no prometheus dep in go.mod — stdlib `/metrics` only; METRICS_ADDR env);
  `logCycleEvent` emits the §39 field set on every workflow transition. It also
  mirrors the #84 explicit three-outcome counts (`agent_outcome_<lower>` /
  `verification_outcome_<lower>`) from `CycleOutcome`; existing derived gauges
  are unchanged (only a populated outcome is counted). It also accumulates the
  #87 `agent_tokens_in_total`/`agent_tokens_out_total`/`agent_cost_usd_total`
  counters and the derived `cost_per_completed_issue` gauge (total reported cost
  / total completes) from `CycleOutcome.Usage`.
- `usage.go` owns #87 agent token/cost accounting: `HarnessUsage`
  (`TokensIn`/`TokensOut`/`CostUSD`) is the optional usage a harness may report,
  parsed best-effort by `parseHarnessUsage` from a `KONOHA_USAGE {…}` line on the
  agent stream. Absence/malformed/negative values contribute ZERO — never
  estimated or inferred (a fabricated number is worse than a missing one). The
  developer/repair and reviewer results carry the per-run `Usage`, the workflow
  aggregates it into `CycleOutcome.Usage` (→ `outcome.json`), and usage is
  observability only: no verdict, route, or terminal-state path reads it.
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
  `EnsureManagedNote` backs the ownership claim: when a cycle claims an
  unmanaged issue (route transition, both SMALL and LARGE — LARGE children post
  their own note when their cycle runs), it leaves ONE explanatory comment whose
  first line is the byte-exact `<!-- konoha-managed: <workflowId> -->` marker,
  idempotent by search-before-create (§24) and a no-op under `DryRun` (§42). The
  body is deterministic GitHub-flavored Markdown (`### Konoha took ownership of
  this issue` heading + a lead + a bulleted `Repo` / `Workflow / run id` /
  `Route / estimated complexity` (omitted when unknown) / `Agent model` /
  `How to intervene` list); the workflow passes `Trigger` (`sweep|webhook|split`)
  and the resolved `Model`. Interpolated ids/model/trigger are sanitized
  (backticks, newlines, `<`/`>` stripped) so they cannot break Markdown or inject
  content; raw issue text is never embedded. A gh failure is retried by the
  activity and, once exhausted, logged non-fatal (the cycle never aborts, §22).
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
  reused. `cleanupWorktree` runs deferred so it fires on failure too — but the
  WORKFLOW is the authoritative teardown owner (`cleanup.go`, #93): a deferred
  `workflow.NewDisconnectedContext` step in `IssueCycleWorkflow` invokes the
  idempotent `CleanupWorktree` activity on every exit path (success/failure/
  timeout/cancellation). The agent receives structured spec artifacts (§35); raw
  issue text is JSON-escaped inside `UNTRUSTED ISSUE DATA` markers (§27). The
  activity returns only a local commit artifact (never pushes/PRs) and records
  the §26 validation result.
- `limits.go` owns the #102 best-effort host resource ceilings. A global
  `limits:` block in konoha.yaml plus a per-repo `lifecycle.limits` override
  (`cpuMilli`/`memoryMiB`/`diskMiB`/`maxWallClock`, resolved by `EffectiveLimits`,
  validated at load incl. `maxWallClock <= worker.devTimeout`) are applied to
  every agent child (`agentCommand` → `enforceCommand`) and every §26 validation
  gate/legacy command (`runGate`/`validateRepo`). Enforcement is **Linux-only**
  (systemd-run `--user --scope` cgroup, else a POSIX `sh`+`ulimit` rlimit
  wrapper + `timeout`); off Linux it is a documented no-op. `LimitEvidence`
  (applied values + `ENFORCED`/`SKIPPED` reason + any crossed ceiling) lands on
  `DevelopResult`/`ReviewResult` and `outcome.json` (`CycleOutcome.Limits`,
  `failedChildOutcome`). It is **anti-accident, not a security boundary**, and
  evidence only: a ceiling never rewrites a deterministic verdict except by
  killing the process that would have produced it (§40). The wrapper always uses
  the caller's scrubbed env, so it cannot re-introduce a `GH_*` credential (§23).
- `urlpolicy.go` owns the #122 outbound-request (SSRF) URL policy + referenced
  secret names. A per-repo `lifecycle.urlPolicy` (`allow`/`deny` host globs and
  CIDRs) is resolved by `EffectiveURLPolicy`; `EvaluateURLPolicy` is the pure
  decision (**deny wins**; a non-empty allow list is default-deny) and
  `GuardOutbound` fails closed only where enforcement is possible. On the host a
  child's outbound cannot be blocked (no sandbox, docs/security.md), so the
  policy is **advisory** — `URLPolicyEvidence` records `status: "advisory"` +
  the resolved rules and no host path may ever record `"enforced"` (§40).
  `RequireHTTPSOrLoopback` is the scheme guard for URLs Konoha itself dials.
  `lifecycle.secrets` lists #CU1 *reference names* (`op`/`vault`/`env`/`file`,
  validated at load); only the NAMES are recorded in `outcome.json`
  (`CycleOutcome.SecretNames`), never a value — resolution is #CU1. Recorded by
  `recordTerminal` (`CycleOutcome.URLPolicy`). Adds no exported activity/workflow
  func (no Temporal type-name change).
- `cleanup.go` owns the #93 disconnect-safe worktree teardown + durable cleanup
  record. `CleanupWorktree` is the thin, idempotent wrapper over `removeWorktree`:
  it removes ONLY a dir whose `.owner` claim matches the requesting `WorkflowID`
  (foreign/unclaimed is never touched) and then independently verifies via
  `git worktree list` + an on-disk stat, returning
  `CleanupResult{Attempted,Outcome,Verified,Remaining,Error}` (outcome ∈
  `removed|absent|skipped|failed`; `Remaining` = owned worktrees still present).
  Removal retries (configurable `worker.cleanupMaxAttempts`, backoff) and always
  returns a result — a failure is recorded in `outcome.json`'s `cleanup` section
  but NEVER changes the terminal §6 state (§40). `IssueCycleWorkflow` runs it
  before `recordTerminal` (so the record lands) and again via a deferred
  disconnected-context call as the cancellation backstop (§44 #20). The startup
  orphan sweep (`SweepOrphanWorktrees`, run once from `Main`) removes a
  konoha-owned worktree only when it is older than `worker.orphanWorktreeTTL`
  AND `workflowAlive` proves the owning workflow is gone; with no probe it never
  sweeps (fail-safe). `KONOHA_DISABLE_WORKTREE_CLEANUP=1` is the test/debug
  escape hatch (mirrors `KONOHA_TOKEN_FIXED`; never set in a real deploy) and
  disables both. Claim-vs-tracker reconciliation is #141, not this file.
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
- `redact.go` owns the §23/§27 secret redaction layer: a concurrency-safe
  `Redactor` (`Add(secrets...)`/`Redact(string)`) masks registered values
  (longest-first, ≥4 chars) and credential-shaped patterns (bearer,
  `Authorization:`, `sk-`, `gh[pousr]_`, `xox[baprs]-`, `AKIA`, `AIza`, URL
  userinfo, `*_PASSWORD|*_TOKEN|*_SECRET=` assignments) with the byte-exact
  `[REDACTED]` marker. Operator-provisioned credential env is registered once at
  worker bootstrap (`registerOperatorSecrets`, values only — the App PEM file
  contents are never read); `pushBranch` registers the exact installation token
  and scrubs its git error via the shared redactor. Every place raw agent/child
  output becomes an error string (`developer.go`/`activities.go` `opencode run`)
  goes through `Redact`. Best-effort but unconditional; a redaction failure never
  changes a verdict, and the `VERDICT:`/marker parser contract is untouched.
  `RegisterSecrets` is also the #CU1 hook: an injected child-env value is
  registered for the run so it masks even without a pattern match.
- `evidence.go` owns the #83 durable per-cycle evidence store: `EvidenceStore`
  (rooted at `evidence.dir`, resolved once at bootstrap via `appCfg`) writes a
  redacted, atomic (temp+rename, `0600`), traversal-validated record under
  `cycles/<repo>-issue-<n>/` (or `<repo>-review-<n>`) with the byte-exact
  artifact set `prompt.txt`/`agent.stdout`/`agent.stderr`/`result.json`/
  `diff.patch`/`validation.txt`/`outcome.json`. All raw I/O goes through the
  #100 pluggable `EvidenceSink` seam (`Write`/`Read`/`List` by root-relative
  slash path): the store owns redaction, path validation, and the layout, while
  a sink only moves opaque bytes. `evidence.sink` selects the backend —
  `local` (default) is `localDirSink`, byte-identical to the #83 layout; an
  S3/MinIO sink is a **documented deferred** implementation (YAGNI), an unknown
  value fails at config load (`EvidenceConfig.validate`), and `newEvidenceSink`
  builds only `local` (constructor also fails closed). `WriteCycleEvidence` is
  the single evidence I/O activity; the workflow invokes it after
  develop/repair/review and at the terminal via `recordTerminal`. Evidence is
  BEST-EFFORT and never authoritative over §6 Temporal state (§40/§44 #20): a
  write failure is logged and recorded as `CycleOutcome.EvidenceError`, never a
  terminal change. Cycle IDs must be plain path segments (`..`/separators/
  absolute rejected); `DryRun` (§42) namespaces under `dryrun/` and still writes
  (evidence is a record, not a mutation). The agent activities capture
  prompt/stdout/stderr into `AgentEvidence` on their result types for this.
  `outcome.json` also carries the #93 `cleanup` section
  (`attempted`/`outcome`/`verified`/`remaining`; see `cleanup.go`).
  `WriteVerification` lands each §26 gate's streams under
  `cycles/<id>/verification/<gate-id>.stdout|stderr` (redacted, plain-segment
  gate id), written best-effort by the validation runner via `persistGateEvidence`;
  the #94 drift patch reuses it as `verification/post-verification.patch`.
  Never call `activity.GetLogger` in these activities (unit tests use a
  non-Temporal ctx, which panics).
- `outcome.go` owns the #84 explicit three-outcome evidence record written into
  `outcome.json`: `AgentOutcome ∈ {SUCCESS,FAILED,TIMED_OUT,SKIPPED,BLOCKED}`
  and `VerificationOutcome ∈ {SUCCESS,FAILED,SKIPPED,ERROR}` plus the ordered
  `Condition{Type,Status,Reason,Message,At}` list (agent → verification →
  terminal). The §6 `terminalState` stays authoritative (§44 #20); outcomes are
  descriptive. `stampOutcome` (called by `recordTerminal`) defaults an absent
  agent/verification outcome to `SKIPPED` — never silently absent (§40) — and
  attaches the conditions with the deterministic `workflow.Now(ctx)` timestamp.
  A `BLOCKED` agent is agent-reported ("cannot proceed / needs input"), distinct
  from a process failure: it routes to `HUMAN_REQUIRED` (`reasonAgentBlocked`)
  and never the repair loop. The Developer/Repair output contract gained a
  `{"blocked":true,"blockedReason":"..."}` variant (validated fail-closed in
   `ValidateDeveloperReport`), parsed into `DevelopResult.Blocked`.
- `lineage.go` owns the #88 per-issue activation lineage + policy digest: per
  `(repo, issue)` it writes `lineage/<owner>-<repo>-issue-<n>.json` under the
  evidence root (`dryrun/lineage/` under §42) via the `RecordActivation`
  activity, whose `LineageActivation` entries record attempt/workflowId/
  reason(`initial|repair`)/harness/model/baseSha/commitSha/result + #87 usage.
  `upsertActivation` is idempotent by `WorkflowID` (a duplicate REPLACES, never
  appends, §24) and `recomputeLineage` derives `attempts`/`totalTokens`/
  `totalCostUsd` from recorded usage only (missing = zero, never estimated); a
  repair gets a distinct per-attempt workflowId so develop + repair are two
  ordered activations. Recording is best-effort (a write failure never changes
  the §6 terminal). `policyDigest` = SHA-256 of the effective `LifecycleConfig`
  + resolved harness/model + `taskHash(owner,repo,issue)`, canonical JSON
  (sorted map keys) so it is stable across processes; `recordTerminal` stamps it
   onto `CycleOutcome.PolicyDigest` (→ `outcome.json`). `DevelopResult.BaseSHA`
   carries the resolved branch-from commit for lineage.
- `change.go` owns the #97 change/work-item lifecycle on top of evidence + lineage:
  per `(repo, issue)` it writes `changes/<repo>-issue-<n>.json` (`dryrun/changes/`
  under §42) via the `RecordChange` activity. `deriveChangeStatus` maps the §6
  state → status (`CLOSED`→`DONE`, `CANCELLED`→`ABANDONED`, a human-decision wait
  `PENDING_APPROVAL`/`HUMAN_REQUIRED`→`IN_REVIEW`, else `OPEN`) — status is
  DERIVED, never set directly. The record carries a redacted `reworkNote` (a
  rejected review's blocking instruction / a §55 denial detail) and the full
  ORDERED feedback thread (`reviewFeedback` keys each entry by workflow+revision+
  kind+index; `upsertFeedback` REPLACES by key, never appends, §24). `recordTerminal`
  records the terminal status; the workflow records `IN_REVIEW` on gate entry,
  review comments + the rework note per revision, and stamps `ReworkNote` onto the
  repair `LineageActivation`. Recording is best-effort and idempotent by
  `(repo, issue)`; CLI `prbot get changes` (newest first) / `prbot describe change
  <repo> <issue>` (joins change + recomputed lineage via pure
  `buildChangeView`/`changeViewJSON`, no Temporal). Never call
  `activity.GetLogger` in `RecordChange` (non-Temporal ctx panics).
- `console.go` owns the #105/#108 local review console: an opt-in, **loopback-only**
  HTTP server + embedded SPA (read + the #108 decisions mutation). `StartConsoleServer`
  (worker; no-op when `KONOHA_CONSOLE_ADDR` is unset) and the `prbot console`
  entrypoint (`runConsole`) share `newConsoleServer`; a non-loopback bind address
  (`validateLoopbackAddr`/`isLoopbackHost`) **fails fast**, and disabling by
  default is explicit (`ConsoleConfig.enabled`). Auth is `authorized`: a
  constant-time bearer token (`KONOHA_CONSOLE_TOKEN`, random+logged when unset)
  for CLI/worker, or the per-process CSRF token issued by `GET /api/v1/status`
  for browsers; `mutationAllowed` (CSRF + loopback `Origin`) guards the single
  mutation route `POST /api/v1/cycles/{id}/decisions` (#108).
  Read routes: `GET /api/v1/status|cycles|cycles/{id}|cycles/{id}/artifacts`
  (method-qualified; unknown id → 404 via `validConsoleCycleID`, malformed → 400;
  `http.Server` timeouts bound every handler). `consoleData` reads finished
  cycles from the evidence store (`listCycleIDs`/`loadOutcomeOptional`/
  `listArtifacts` on `*EvidenceStore`) and running cycles best-effort from the
  #89 status query (`temporalStatusQuery`/`temporalRunningLister`, nil client ⇒
  evidence-only). The SPA (`consoleassets/`, `//go:embed`) is served with a
  strict CSP (`consoleCSP`, no inline script) and never executes artifact
  content. `consoleConfigFromEnv` reads **only** `KONOHA_CONSOLE_*`; the console
  still reads no GitHub credential env, but a #108 decision shells `gh` (the
  operator's auth) to post the §18 comment — the read-only paths stay
  credential-free and make no outbound calls beyond the optional Temporal status
  query. Adding the console renames no exported activity/workflow func (no
  Temporal type change).
- `mcp.go` owns the #116 local MCP surface: an opt-in, **loopback-only**,
  **read-only** Model Context Protocol server (Streamable HTTP, JSON-RPC 2.0
  over `POST /mcp`) started by `StartMCPServer` (worker; no-op when
  `KONOHA_MCP_ADDR` is unset) or the `prbot mcp` entrypoint. It reuses the
  console's `validateLoopback`/`bearerToken`/`constantTimeEqual` and the same
  `consoleData` projections, so `list_cycles`/`get_cycle`/`get_evidence` return
  the SAME data as the CLI (#99) and console (#105); `environment_list/log/diff`
  read the cycle's recorded evidence (same as `prbot logs`). Every read is
  redacted (#82) and size-bounded, `get_evidence` paths are store-relative only
  (#83), and `mcpConfigFromEnv` reads **only** `KONOHA_MCP_*` (never `GH_*`). No
  mutating tool is exposed by default; `KONOHA_MCP_CONTROL` adds exactly one
  opt-in tool, `post_decision`, which routes through the #108 `submitDecision`
  path (the authoritative §18 channel) — never a parallel write. Adding the MCP
  surface renames no exported activity/workflow func (no Temporal type change).
- `decisions.go` owns the #108 console decision surface, built on the §55 gate
  and #97 change record. **It is not a second source of truth**: `SubmitHumanDecision`
  writes the byte-exact `<!-- konoha-decision: <action> -->` marker (plus a
  base64 `decision-id`/`payload` marker) as ONE issue comment via `ghRunner`,
  search-before-create so a repeated submit is a no-op (§24); `ReadHumanDecision`
  parses the payload back into `HumanDecision.Feedback`/`Comments`.
  `ValidateDecisionRequest` enforces the §108 rules (action set
  `approve|request-changes|deny` — plus `resume` for the #120 clarification form,
  which carries only feedback — non-empty size-bounded request-changes
  feedback, diff-anchored comments bound to `(sha, file, line, side)` — a
  comment on any other SHA, an unknown side, or a negative line is stale/
  malformed and fails closed). `consoleData.submitDecision` binds the submission
  to the
  LIVE gate (`gateSnapshot`: owner from `repos.yaml`/outcome, attempt+SHA from
  the #88 lineage, `Open` iff the running status is `PENDING_APPROVAL`) — a
  superseded attempt/SHA or a closed gate is `409`. `applyDecisionEvidence`
  records the derived records for request-changes: a #97 rework activation
  carrying the feedback, the change feedback thread (status back to OPEN) with
  one entry per anchored comment, and the cycle's `comments.json` (rendered on
  the next review), REDACTED (#82) and MERGED by the content-addressed stable id
  `diffCommentID` so carry-forward/replay never duplicates (§24). The #113
  delivery path is the PURE `reworkBlocking(feedback, comments)` projection —
  freeform feedback lines then `file:line (side): body` anchored findings,
  deterministic order — threaded into the repair `Review.Blocking` by
  `IssueCycleWorkflow`. Stale comments (superseded SHA) are retained and rendered
  but refused as a re-submission; the capture UI lives in the console Diff tab
  (`consoleassets/app.js`: staged drafts submitted with request-changes). Audit
  still goes
  through `RecordAuditEvent` (`auditDecisionForAction`: allow/deny/request_changes).
  A `POST` never proceeds on a fail-closed path; the §6 state stays authoritative
  (§44 #20).
- `forms.go` owns the #120 typed interaction forms for human input, built ON TOP
  of the #108/#97 decision surface (never a second source of truth, §44 #20).
  `decisionFormSchemas` is the closed, deterministic catalog — `approve` (no
  fields), `request-changes` (`summary` text required + `severity` choice +
  diff-anchored comments), `clarification` (`answer` text required + `confidence`
  number 0–1) — each naming the EXISTING §18 action it maps onto
  (`approve`/`request-changes`/`resume`). `ValidateFormSubmission` is the PURE,
  fail-closed §36 gate: an unknown form/field, missing required value, wrong type,
  a value outside a choice's closed set, and an over-cap (`MaxLength`) or
  out-of-range (`Min`/`Max`) value are all rejected and never become a decision.
  `FormDecision.decisionRequest` maps a validated form onto a `DecisionRequest`
  (binding the gate attempt/SHA + anchored-comment SHA) and applies the #82
  redactor to the feedback/comment bodies; the console's
  `POST /api/v1/cycles/{id}/forms` handler (`handleFormDecision`) and the SPA
  panel (`consoleassets/app.js`, schemas advertised in `GET /api/v1/status.forms`)
  funnel it through `consoleData.submitBoundDecision` — the SAME live-gate bind +
  `ValidateDecisionRequest` + `d.decide` path as a raw #108 decision, so audit
  (§55) and per-gate idempotency (§24) are inherited unchanged. A clarification
  form is answered at the existing §55 gate: the cycle waits in
  `PENDING_APPROVAL`, a valid answer maps to `resume` (`EvaluateApproval`/
  `ApplyHumanDecision` treat it as continue), and a timeout keeps the existing
  `BLOCKED` semantics (§40). `forms.go` adds no exported activity/workflow func
  (no Temporal type-name change).
- `queue.go` owns the #109 needs-attention queue + recovery actions (Machinist
  model: **retry preserves earlier attempts, no silent replay**). `attentionItems`
  is the PURE projection (awaiting-approval / blocked / failed / interrupted,
  deterministic newest-first) over a neutral `queueCycle`; the console resolves
  the live facts (`consoleData.queueCycleFor`: outcome + the Temporal running/
  status/execution probes) and `listQueue` serves `GET /api/v1/queue`. `retry`/
  `cancel` are the ONLY authoritatives actions: `cancel` goes through
  `client.CancelWorkflow` (the workflow's disconnect-safe teardown still runs) and
  is idempotent (a stopped cycle is `NOOP`); `retry` starts a NEW run under the
  same deterministic `<repo>-issue-<n>` id (`ALLOW_DUPLICATE` reuse +
  `USE_EXISTING` conflict policy, so a repeated retry converges, §24) and never
  deletes the #88 lineage / #97 change history. An **interrupted** cycle (no
  outcome and no live worker — a running status the worker cannot answer, or a
  failed/timed-out execution) is never auto-retried: `ConfirmStopped` is required
  and the writer best-effort stops any still-registered execution first (no
  silent replay). `attach`/`pause`/`resume` are #136/#144 session verbs — with no
  provider they are disabled and report `SKIPPED` (§40, never a false success).
  Every accepted action is `RecordAuditEvent`-audited (§55, steps `queue-retry`/
  `queue-cancel`) and a `DryRun` (§42) is a `PREVIEW` with no mutation.
  `consoleQueueWriter` is the writer; `temporalCancelWorkflow`/`temporalStartCycle`/
  `temporalExecStatus` are the Temporal seams (nil → the surface fails closed
  with 501). This file adds no exported activity/workflow func (no Temporal type
  name changes).


- `stream.go` owns the #110 live progress streaming (SSE) endpoint on the
  console: `GET /api/v1/cycles/{id}/stream` (same bearer/CSRF auth as every
  route, read-only). `consoleStreamSource` is the seam — production resolves a
  bounded snapshot from the #89 status query (running) or `outcome.json`
  (finished) plus a bounded tail of the recorded `agent.stdout`; tests inject a
  simulated environment. `streamHub` is the process-local fan-out: a per-cycle
  producer polls the source and publishes monotonic-id `state`/`condition`/
  `output`/`end` events; `?watch=1` adds the command/output tail (bounded by
  `streamOutputCap`, 64 KiB) and a non-watch subscriber never sees output. Replay
  is bounded by `streamOutputCap` bytes + `streamBufferEvents`, so memory is
  O(cap) regardless of stream length. A reconnect's `Last-Event-ID` (or
  `?lastEventId=`) replays only buffered events after the cursor — no duplicates;
  a lagged subscriber gets a `reset` and resumes. A true §6 terminal state emits
  `end` and closes; a `PENDING_APPROVAL`/gate wait stays open. The events are
  PROJECTIONS of the durable status/evidence records — the §6 state stays
  authoritative (§44 #20) and no live output is ever buffered unbounded. This
  file adds no exported activity/workflow func (no Temporal type name changes).
