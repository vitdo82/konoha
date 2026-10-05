# Konoha

Cross-repo PR review orchestrator. Runs a Temporal worker that sweeps repos for
open PRs without a `kakashi-82[bot]` review and reviews each via a local opencode
agent, posting the verdict back to the PR.

## Configuration

Precedence: **env > file > bundled default**. Per-repo policy is file-authoritative
(`repos.yaml`); runtime/worker values live in `konoha.yaml`; only deployment/host
keys are env-overridable (policy/timeouts are NOT — a host env must not fork
deterministic behavior).

### `configs/repos.yaml` — per-repo lifecycle policy (source of truth)

```yaml
repos:
  - repo: my-repo
    owner: my-org
    repoPath: /path/to/repo
    schedule: "*/5 * * * *"
    lifecycle:
      mode: supervised-autonomous      # review-only | supervised-autonomous
      harness: opencode                # §85 agent harness by NAME (never a path)
      developmentMaxComplexity: 4      # §11 executable ceiling
      maxDevReviewAttempts: 2          # §16 repair-loop cap
      autonomy:
        default: autonomous
        closePolicy: risk-based        # autonomous | risk-based | human-required
        humanRequiredPaths: [".github/**", "infra/**", "migrations/**"]   # §20
        humanRequiredLabels: ["security", "breaking-change"]              # §20
      merge:                           # §49 merge + CI gate
        strategy: squash               # squash | rebase | merge
        requireCI: true                # gate the merge on green CI (default true)
        ciMaxWait: 10m                 # bounded CI poll window
      release:                         # §50 post-merge release + changelog
        enabled: false                 # opt-in; false/unset = no-op
        changelogPath: CHANGELOG.md    # repo-relative changelog file
        versionPrefix: v               # tag prefix (default v)
      deploy:                          # §51 deploy + post-deploy verification
        enabled: false                 # opt-in; false/unset = no-op
        command: ""                    # deploy hook (sh -c) run in the main checkout
        env: production                # deployment environment (KONOHA_DEPLOY_ENV)
        verify: ""                     # smoke/health command; exit 0 = healthy
        maxWait: 5m                    # bounded verify poll window
        rollbackOnFail: true           # emit a rollback signal on failure
      validation:                      # §26 structured verification gates (optional)
        - id: unit-tests               # required, unique per profile
          argv: ["go", "test", "./..."]  # structured argv, never a shell string
          mandatory: true              # default true; advisory gate failures don't fail
          timeout: 20m                 # per-gate window (default 10m)
          dir: backend                 # repo-relative; no ".." / leading "/"
      # validationCmd: "make check"   # §26 legacy shell-string form; mutually exclusive
      deps:                            # §52 dependency + security automation
        enabled: false                 # opt-in; false/unset = no scan, no issues
        scanCmd: ""                    # required when enabled; prints the JSON finding array
        schedule: 24h                  # per-repo scan cadence (<repo>-deps schedule)
        label: konoha:dependency       # base label; vulnerabilities also get konoha:security
        autoFileIssues: true           # false = scan/report only
      grooming:                        # §56 backlog grooming pre-stage
        enabled: false                 # opt-in; false/unset = no groom, no ready issues
        selector: backlog              # label selecting raw backlog items
        schedule: 24h                  # per-repo cadence (<repo>-groom schedule)
        repoLabel: ""                  # optional extra provenance label on the ready issue
        readyLabel: konoha:ready       # label marking a groomed ready issue
      alerting:                        # §53 rate-drift alerts + failure feedback
        enabled: false                 # opt-in; false/unset = no alerts, no bug issues
        notify: ""                     # webhook URL (slack-compatible); empty = log only
        schedule: 24h                  # evaluation cadence (<repo>-alerts schedule)
        onRates: []                    # optional subset; empty = every configured threshold
        thresholds:                    # autonomous rate alerts below; all others above
          autonomous_completion_rate: 0.7
          human_escalation_rate: 0.3
      propagation:                     # §54 cross-repo change propagation
        enabled: false                 # opt-in; false/unset = no downstream issues
        maxPerMerge: 3                 # fan-out cap per merge (default 3, §43-style)
        downstream:                    # downstream repo → {trigger, type}
          downstream-repo:
            trigger: "pkg/api/**"      # changed-file glob that makes a merge affect it
            type: breaking             # change-kind tag on the filed issue
      approval:                        # §55 pre-execution approval gate
        preGate: false                 # opt-in; false/unset = fully autonomous (no change)
        requiredSteps: [publish, merge, deploy]  # closed set; empty = all steps
      audit:                           # §55 append-only audit of gated decisions
        enabled: false                 # required (with sink) when approval.preGate is on
        sink: /var/log/konoha-audit.jsonl  # required when enabled (JSONL append)
      poGateTimeout: 168h              # §18/§33 human-response window (also the gate wait)
      dryRun: false                    # §42 shadow mode: true = zero mutations
      maxConcurrentIssueCycles: 1      # §43
      maxConcurrentChildrenPerIssue: 3 # §43
```
Unknown keys / out-of-range values fail fast at `./scripts/apply.sh` (the Go
loader `prbot config`).

### `configs/konoha.yaml` — runtime + code-level defaults

```yaml
worker:
  taskQueue: review-queue
  maxConcurrentActivities: 10     # env: WORKER_MAX_CONCURRENT_ACTIVITIES
  startToCloseTimeout: 15m        # cycle/PR activity window
  sweepTimeout: 10m               # sweep activity window
  retryMaxAttempts: 5             # #95 infrastructure-class retries (gh/git/evidence, backoff)
  sweepRetryMaxAttempts: 2        # sweep activity retries (separate policy)
  cleanupMaxAttempts: 5           # #93 worktree teardown retries (idempotent)
  orphanWorktreeTTL: 24h          # #93 sweep a stranded owned worktree after this age
evidence:
  dir: .konoha/evidence           # durable per-cycle evidence root (see below)
harness:                          # §85 generic harness (used when a repo selects `generic`)
  executable: ""                  # bare executable NAME (no path separators)
  args: ["run", "{{model_args}}", "{{prompt}}"]  # {{model_args}}/{{prompt}} expand
  modelFlag: "--model"
  passEnv: ["PATH", "HOME", "LANG"]  # explicit allowlist; GH_* is always stripped
  promptOnStdin: false            # legacy: true = prompt via stdin, not argv
  prompt_mode: ""                 # #96: arg | stdin | stdin_file (overrides promptOnStdin)
  binary: ""                      # #96: operator path to a standalone binary to stage
  binary_sha256: ""               # #96: required sha256 of `binary`; mismatch fails closed
  catalog_cache: ""               # #96: operator path to a model catalog for offline resolution
  mcp:                            # #96: MCP server config for supporting harnesses (optional)
    path: ".mcp.json"             # worktree-relative location to write (git-excluded)
    config: "{}"                  # config content
defaults:
  lifecycle:
    harness: opencode             # §85 fallback harness name when a repo leaves it unset
    developmentMaxComplexity: 4   # fallback when repos.yaml leaves it unset
    maxDevReviewAttempts: 2
    maxResolutionAttempts: 1      # §37 disagreement-resolution cap
    poGateTimeout: 168h
    merge:                        # §49 fallback merge policy
      strategy: squash
      requireCI: true             # fail-closed: gate on green CI unless opted out
      ciMaxWait: 10m
    deploy:                       # §51 fallback deploy/verify policy
      env: production
      maxWait: 5m
      rollbackOnFail: true        # fail-closed: emit a rollback signal on failure
    validation:                   # §26 fallback verification gates
      - id: test
        argv: ["make", "test"]
  agents:                        # §98 named resource catalog
    models:                      # selectable per stage: analyst/estimator/developer/reviewer/repair/completion
      cheap:
        provider: anthropic
        model: claude-haiku
        baseUrl: ""               # optional provider endpoint override
        apiKeyEnv: ANTHROPIC_API_KEY  # credential NAME only; the value is never read or recorded
      strong:
        provider: anthropic
        model: claude-sonnet
    budgets:                     # recorded as evidence; never gate a verdict or shorten a timeout
      default:
        maxCostUsd: 5.0           # crossing computed from #87 reported usage
        maxTokens: 200000
        maxWallClock: 30m         # crossing computed from elapsed cycle time
```

A repo (or a single stage) selects a model by **name** in `repos.yaml`:

```yaml
lifecycle:
  models:                        # §98 per-stage catalog name (falls back to `model`)
    analyst: cheap
    developer: strong
    reviewer: strong
  budget: default                # §98 named budget (optional)
```

An unknown model/budget name, or an unknown stage key, **fails at load**
(`prbot config` / worker startup) — never mid-cycle. The resolved
provider/model plus a content **digest** land in `outcome.json` under
`resources`, and each develop/repair activation records its stage + model
digest in `lineage.json`, so "same config, different result" is investigable
and a catalog edit is a reviewable diff. Budget crossings (`maxCostUsd` /
`maxTokens` / `maxWallClock`) are **evidence only** (§40): they are computed
from the harness's reported #87 usage and the elapsed cycle time, and they never
rewrite a verdict or shorten a configured timeout. Credential values never reach
evidence — only the `apiKeyEnv` **name** is recorded.

**Egress and workspaces are intentionally omitted (no sandbox).** Konoha has no
sandbox (see `docs/security.md`), so there is nothing to name: there is no
egress to policy and no isolated workspace to attach. They are documented as
not-applicable rather than stubbed, so the catalog never advertises a capability
that does not exist.

A missing `konoha.yaml` is fine (bundled defaults). `KONOHA_CONFIG_DIR` overrides
the config directory (default `configs/`).

**Typed activity retries (#95).** `worker.retryMaxAttempts` is the
**infrastructure** class — gh/git/evidence I/O, retried with exponential
backoff. The other classes are fixed in code, not config-driven: deterministic
record/policy activities get `MaximumAttempts: 1` (a retry would double-count),
agent runs get exactly 1 (a transport-loss retry must not replay an LLM) plus a
`HeartbeatTimeout` so a hung agent surfaces as a timeout, and verification
activities (CI/deploy gates) get 3. Sweeps keep their own
`sweepRetryMaxAttempts` policy.

**Agent harness (§85).** A repo selects an agent harness by **name**
(`lifecycle.harness`, fallback `defaults.lifecycle.harness`, default
`opencode`) — config never names an executable path. Built-ins: `opencode`
(byte-for-byte today's argv/env) and `generic` (the `harness:` block above, whose
bare `executable` plus `args`/`modelFlag`/`passEnv` are operator-declared). An
unknown harness name fails at load; `GH_*` is stripped for every harness and
`generic` forwards only `passEnv`-listed variables.

**Harness provisioning (§96).** Independently of the name, a harness may expose
optional capabilities, detected per run: `binary` + `binary_sha256` stage a
standalone binary into the agent's PATH after verifying its digest (a mismatch
**fails closed**); `catalog_cache` stages a model catalog so `--model` resolution
is deterministic and **offline** (a model absent from the catalog fails closed);
`prompt_mode: stdin_file` writes the prompt to a file and redirects the child's
stdin from it (never argv); and an `mcp` block writes the operator-declared MCP
server config into the worktree (git-excluded) for harnesses that use MCP. Each
capability is operator configuration — a caller can never supply a path. The
`<executable> --version` is captured **best-effort** (never fails a run). Every
capability records `APPLIED` or `SKIPPED` (a harness without it is never a false
success) in the cycle's `provisioning.json` evidence.

**Validation (§26).** A repo may declare `lifecycle.validation` as an ordered
list of structured gates. Each gate runs as a bare argv in `dir` (repo-relative,
validated at load: no `..` / leading `/`), with a bounded `timeout` and the
scrubbed agent env (no `GH_*`). All gates run — never early-exit — and the
aggregate passes iff every **mandatory** gate exits 0; an advisory failure is
recorded but tolerated, while a gate that cannot start records `ERROR` and fails
closed. Each gate's `stdout`/`stderr` lands under the cycle's
`verification/<id>.stdout|stderr` evidence (redacted). A gate that rewrites a
tracked file is recorded as **verification drift**: `verificationMutated=true`
in `outcome.json` plus a saved `verification/post-verification.patch` and a
warning log — evidence only, never folded into the deliverable (the delivered
commit SHA and terminal state are unchanged). The legacy
`lifecycle.validationCmd` shell string still works; setting both is a load-time
error (ambiguous). A repo that declares neither inherits
`defaults.lifecycle.validation`.

### Durable per-cycle evidence (`evidence.dir`)

Every cycle writes a redacted, path-validated, append-only record under
`evidence.dir` (default `.konoha/evidence`), so auditability survives Temporal
history retention:

```
cycles/<repo>-issue-<n>/     (and <repo>-review-<n> for PR cycles)
  prompt.txt        # what the last agent was asked
  agent.stdout      # its captured stdout (redacted)
  agent.stderr      # its captured stderr (redacted)
  result.json       # the validated agent result
  diff.patch        # the reviewed PR diff
  validation.txt    # the §26 validation output
  provisioning.json # #96 optional harness-capability outcomes (APPLIED/SKIPPED)
  verification/     # §26 per-gate streams (one pair per configured gate)
    <id>.stdout     # redacted gate stdout
    <id>.stderr     # redacted gate stderr
    post-verification.patch   # #94 drift: a gate rewrote a tracked file
  outcome.json      # the terminal §6 outcome
lineage/<owner>-<repo>-issue-<n>.json   # #88 per-issue activation lineage (below)
```

Evidence is **best-effort**: a write failure is recorded as `evidenceError` on
the outcome and logged, but never changes the authoritative workflow state
(§40). Cycle IDs must be plain path segments (traversal rejected); writes are
redacted via the §23 `Redactor` and atomic (temp file + rename, owner-only
perms). Under `dryRun` (§42) evidence is namespaced under `dryrun/` — it is a
record, not a mutation.

`outcome.json`'s `cleanup` section records the #93 worktree teardown that runs on
**every** exit path (including cancellation, via a disconnected workflow
context):

```json
"cleanup": {"attempted": true, "outcome": "removed", "verified": true, "remaining": 0}
```

`outcome` is one of `removed`/`absent`/`skipped`/`failed`; `verified` is an
independent `git worktree list` + on-disk check, and `remaining` counts
konoha-owned worktrees still present (a leftover is reported, never hidden).
Teardown removes only a worktree whose `.owner` claim matches the run — a
foreign/unclaimed directory is never touched. A cleanup failure is logged and
recorded but never changes the terminal state (§40). Once per worker start an
orphan sweep removes an owned worktree whose workflow is gone and whose age
exceeds `worker.orphanWorktreeTTL`; `KONOHA_DISABLE_WORKTREE_CLEANUP=1`
(test/debug only) disables teardown and the sweep.

### Agent token/cost accounting (§39)

A harness may report per-step usage on its output stream as a line starting
with `KONOHA_USAGE` followed by `{"tokensIn":…,"tokensOut":…,"costUSD":…}`.
Parsing is **best-effort**: when a harness reports nothing (the default for
`opencode`) the contribution is zero and no number is ever estimated or
inferred. Per-agent usage lands in the cycle's `result.json`, the cycle
aggregate in `outcome.json`, and the §39 sink accumulates
`agent_tokens_in_total`, `agent_tokens_out_total`, `agent_cost_usd_total` plus a
derived `cost_per_completed_issue` gauge (total reported cost / total completes).
Usage is observability only — no verdict or terminal state reads it.

### Per-issue activation lineage + policy digest (#88)

Per `(repo, issue)` Konoha keeps `lineage/<owner>-<repo>-issue-<n>.json`: every
develop/repair **activation** with its attempt number, workflowId, reason
(`initial`/`repair`), harness/model, base/commit SHA, result and reported usage,
plus `attempts`/`totalTokens`/`totalCostUsd` **recomputed** from the recorded
activations (missing usage contributes zero, never estimated). Recording is
idempotent by `workflowId` — a retried activity replaces, never appends — so a
develop + repair are two ordered activations and replays never duplicate.
`outcome.json` additionally carries `policyDigest`, a SHA-256 of the effective
lifecycle policy + resolved harness/model + task identity (canonical JSON, so it
is stable across processes and changes when any policy/model/task field
changes). Why: the same config can produce different results, and the digest
plus the lineage make that difference investigable. Lineage is best-effort and
never changes the terminal state. Under `dryRun` it is namespaced under
`dryrun/lineage/` and mutates nothing external.

### Env override matrix

| Setting | Env override | Notes |
|---|---|---|
| Temporal address | `TEMPORAL_ADDRESS` | host-specific |
| Config dir | `KONOHA_CONFIG_DIR` | default `configs/` |
| Metrics listener | `METRICS_ADDR` | default off |
| Webhook | `WEBHOOK_ADDR` + `GH_WEBHOOK_SECRET` | optional §32 fast-path |
| GitHub App | `GH_APP_ID` / `GH_APP_INSTALL_ID` / `GH_APP_PEM` | secrets, never in file |
| Max concurrent activities | `WORKER_MAX_CONCURRENT_ACTIVITIES` | host capacity |
| Task queue | `KONOHA_TASK_QUEUE` | rare per-env rename |

Deliberately NOT configurable (contract/invariant): `konoha-parent`/`konoha-child`/
`konoha-decision` markers, the `VERDICT:`/`BLOCKING:`/`NITS:`/`CONCLUDE:` agent
parser contract, §6 workflow state names, the secret-redaction patterns and the
`[REDACTED]` marker, and GitHub API base.

## Architecture

- **Worker** (Go, Temporal SDK): `ReviewAllWorkflow` (cron-driven per repo via
  Temporal Schedule) → child `PrReviewWorkflow` (`<repo>-review-<n>`) →
  activities `ListUnreviewedPRs` → `FetchDiff` → `ReviewWithOpencode` →
  `SubmitReview`. `IssueAllWorkflow` sweeps unmanaged issues into
  `IssueCycleWorkflow` (`<repo>-issue-<n>`), capped by
  `maxConcurrentIssueCycles`. On a commit-bound approval the issue only closes
  after the §49 pipeline: `WaitForCI` (when `merge.requireCI`) →
  `MergePullRequest` (idempotent, config-driven strategy) →
  `EnsureIssueState(CLOSED)`. A failing CI gate or merge error escalates to
  `HUMAN_REQUIRED` and never closes the issue. When `release.enabled`, the close
  fires `PostMergeWorkflow` (`<repo>-postmerge-<pr>`): `RunRelease` derives a
  semver bump from conventional-commit messages since the last tag
  (`feat`→minor, `fix`/`perf`→patch, `BREAKING CHANGE`/`feat!`→major, no
  match→patch), prepends `CHANGELOG.md`, and creates the tag + GitHub Release
  (existing tag → no-op, idempotent; a release problem never changes the
  issue's terminal state). When `deploy.enabled`, the same workflow then runs
  `DeployRelease` (records a GitHub Deployment for the tag, runs the hook from
  the main checkout — a tag whose latest deployment succeeded is never
  re-deployed) and `VerifyDeployment` (bounded smoke/health poll). A permanent
  verification failure emits a rollback signal, reopens the issue, and projects
  `konoha:human-required` — never a silent "deployed" (§40). `DependencyWorkflow`
  (per-repo `<repo>-deps` schedule when `deps.enabled`) runs `ScanDependencies`
  (config `scanCmd`) and files one idempotent issue per finding
  (`EnsureDependencyIssue`, marker `<!-- konoha-dep: … -->`); those issues then
  flow through the normal lifecycle via the existing sweep/webhook. A filed issue
  carries `konoha:dependency`, plus `konoha:security` for a known vulnerability —
  both are built-in §20 escalation labels, so a dependency/CVE update always
  routes to `HUMAN_REQUIRED` before a PR is published, even under
  `closePolicy: autonomous`. When `alerting.enabled`, a per-repo `<repo>-alerts`
  schedule runs `AlertWorkflow` → `EvaluateAlerts`, which reads the §39 rate
  gauges and emits one alert per violated metric per window (deduped; log +
  optional `notify` webhook). The same flag makes a terminal `BLOCKED`/`FAILED`
  cycle start `FailureFeedbackWorkflow`, which files at most one bug issue per
  run (`EnsureFailureIssue`, marker `<!-- konoha-failure: … -->`) carrying the
  structured failure; the filed issue is a normal issue (no `konoha:managed`
  label) so the sweep drives it back through the lifecycle. Alerting never
  resolves a stuck run — Temporal state stays authoritative. When
  `propagation.enabled`, a successful merge also runs the §54 stage:
  `DetectAffectedDownstreams` (reads the PR's changed files via `gh pr diff
  --name-only`) matches them against the configured `downstream` map, and
  `EnsureCrossRepoIssue` files one issue per affected repo (marker
  `<!-- konoha-cross: <owner>/<repo>#<sha> -->`, label `konoha:cross-repo`,
  fan-out capped by `maxPerMerge`). The filed issue carries no `konoha:managed`
  label, so the downstream repo's own sweep/webhook drives it through the
  downstream lifecycle — the downstream phase/autonomy/§20 gates apply
  unchanged and no cycle is double-started. When `approval.preGate` is on, each
  listed step (`publish`/`merge`/`deploy`) first enters `PENDING_APPROVAL` and
  the `AwaitApproval` activity polls the §18 decision channel (bounded by
  `poGateTimeout`): `approve` proceeds, `deny` escalates to `HUMAN_REQUIRED`, and
  a timeout stays `BLOCKED` — never a silent proceed. Each gated decision is
  appended by `RecordAuditEvent` to the append-only `audit.sink` JSONL file
  (subject, step, actor, allow/deny/timeout). When `grooming.enabled`, a per-repo
  `<repo>-groom` schedule runs `BacklogGroomWorkflow`: each open issue carrying
  `selector` is analysed in DRY (`AnalyzeIssue` + `EstimateIssue`), and an
  actionable item is filed as a fully-specified ready issue
  (`EnsureReadyIssue`, marker `<!-- konoha-groomed: <sha> -->`, label
  `readyLabel`) that the normal sweep consumes; an unactionable item is left open
  with an idempotent note (`RecordGroomNote`, marker
  `<!-- konoha-groom-rejected: <sha> -->`) and never auto-closed. The run records
  the §39-style `groomed_count` / `groom_reject_rate` gauges.
- **Config**: `repos.yaml` — one entry per repo (source of truth); per-repo
  `lifecycle:` policy (§33/§43 of `docs/new-design-v2.md`): mode, complexity,
  review attempts, autonomy/escalation, `poGateTimeout`, `dryRun`, §43
  concurrency caps, §52 `deps`, §53 `alerting`, §54 `propagation`, §55
  `approval`/`audit`, and §56 `grooming`.
- **Runtime config**: `konoha.yaml` — worker/runtime values + code-level
  `defaults.lifecycle` fallbacks (see Configuration below).
- **Apply**: `scripts/apply.sh` builds the worker and asks `prbot config` (the
  shared Go loader) for each repo's schedule input JSON — unknown keys / bad
  values in `repos.yaml` fail fast. It then (re)creates `<repo>-review-all` and
  `<repo>-issue-all` schedules per repo, plus a `<repo>-deps` schedule for each
  `deps.enabled` repo (`prbot config --deps`), a `<repo>-alerts` schedule for
  each `alerting.enabled` repo (`prbot config --alerts`), and a `<repo>-groom`
  schedule for each `grooming.enabled` repo (`prbot config --grooming`).

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
(`DRY_RUN=1` to shadow-run), and `/tmp/prbot release` (runs the §50 release
activity for `OWNER`/`REPO`; `RELEASE_ENABLED=0`, `CHANGELOG_PATH`,
`VERSION_PREFIX`, `BASE_BRANCH`, `DRY_RUN=1` override), and `/tmp/prbot deps`
(runs the §52 dependency scan for `OWNER`/`REPO`/`REPO_PATH` and prints the
validated `DependencyReport`; `SCAN_CMD` required), and `/tmp/prbot status
<repo> <issue>` (queries the running issue cycle's `status` handler and prints
the `{state, currentStep, conditions, agentOutcome, verificationOutcome}` JSON;
needs a running worker + Temporal), and `/tmp/prbot get changes` (lists every
recorded change/work-item record newest first) and `/tmp/prbot describe change
<repo> <issue>` (prints the record's status, rework note, full ordered feedback
thread, and recomputed activations/attempts/totals; both read `evidence.dir` and
need no Temporal). `prbot config --deps`
prints the §52 dependency schedule inputs for `apply.sh`, `prbot config
--alerts` prints the §53 alert schedule inputs, and `prbot config --grooming`
prints the §56 grooming schedule inputs.

## Add a repo

Add one entry to `configs/repos.yaml`, then `./scripts/apply.sh`.