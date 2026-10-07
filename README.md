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
      publish:                         # §25/#115 PR description policy
        aiDescription: false           # opt-in AI PR body; failure → deterministic body
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
      limits:                          # #102 per-repo host ceilings (overrides konoha.yaml limits)
        cpuMilli: 2000                 # Linux best-effort, NOT a security boundary; no-op elsewhere
        memoryMiB: 2048
        diskMiB: 1024
        maxWallClock: 30m              # hard kill; must be <= worker.devTimeout
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
        notify: ""                     # legacy single webhook URL (slack-compatible); empty = log only
        targets: []                    # #123 targets: {type: slack|webhook|log, url}
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
  sink: local                     # storage backend; `local` only — S3/MinIO is a deferred seam
# limits:                         # #102 host resource ceilings (Linux best-effort; no-op elsewhere)
#   cpuMilli: 2000                # unset/absent = no ceilings (per-repo lifecycle.limits overrides)
#   memoryMiB: 2048
#   diskMiB: 1024
#   maxWallClock: 30m             # hard kill; must not exceed worker.devTimeout
# egress:                         # #103 ADVISORY proxy allowlist (bypassable; NOT a security boundary)
#   proxy: http://127.0.0.1:8888  # sets HTTPS_PROXY/HTTP_PROXY/NO_PROXY on agent + validation children
#   allowOut: ["api.anthropic.com", "registry.npmjs.org"]  # recorded in outcome.json only
# #122 per-repo outbound URL policy lives in repos.yaml `lifecycle.urlPolicy`:
#   allow/deny host globs (`*.example.com`) or CIDRs (`10.0.0.0/8`); deny wins.
#   ADVISORY on the host (bypassable; never "enforced") and recorded in outcome.json.
# lifecycle.urlPolicy also pairs with `lifecycle.secrets: [op://…]`: only the
#   referenced NAMES are recorded; values are resolved by the #CU1 secrets store.
# facility: acme                  # #104 Facility label (recorded; not an auth boundary — single-operator)
# scopes:                         # #104 narrow-only policy tenancy (NOT execution isolation; see below)
#   payments:                     # Product Line
#     repositories: ["acme/payments", "acme/payments-infra"]  # the repos this scope covers
#     harnesses: ["opencode"]     # allowlists may only reference names already in the global catalogs
#     models: ["cheap", "strong"]
#     budgets: ["small"]
#     approval: true              # may only turn the §55 gate ON
#     closePolicy: risk-based     # may only narrow (risk-based or stricter — never autonomous)
#     departments:                # optional functional areas that may only narrow further
#       checkout:
#         repositories: ["acme/payments-checkout"]
#         models: ["strong"]
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
# agents:                         # #117 declarative agent profiles (see below)
#   reviewer:
#     harness: opencode           # §85 harness NAME (fallback: defaults.lifecycle.harness)
#     model: strong               # §98 catalog NAME from defaults.agents.models (never a raw id)
#     tools: [read, grep]         # #119 advisory allow list (recorded + digested + handed to a supporting harness)
#     deny: []                    # #119 hard deny list — fails closed (see tool registry below)
#     mcp: [github]               # #119 named MCP servers from `mcpServers:` (resolved + played to a supporting harness)
#     limits: { memoryMiB: 2048 } # #102 override; agrees with every other selected profile
#     promptMode: arg             # recorded + digested (harness prompt transport)
# tools:                          # #119 tool catalog (built-ins are code-level)
#   custom:
#     deploy-check: { description: "Verify a deploy" }
# mcpServers:                     # #119 named HTTP MCP servers
#   github:
#     url: "https://api.githubcopilot.com/mcp/"
#     headers: { Authorization: "Bearer ${GITHUB_MCP_TOKEN}" }  # written to the git-excluded config only
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

**Declarative agent profiles (#117).** A profile bundles the per-agent layers
(harness #85, model #98, limits #102, plus the tool/MCP references and prompt
transport) into one named unit declared in `konoha.yaml` `agents:`. A repo
selects a profile per stage in `repos.yaml` — `lifecycle.agents.<stage>` for
`analyst|estimator|developer|reviewer|repair|completion`, with a repo-level
`lifecycle.agents.default` fallback:

```yaml
lifecycle:
  agents:                        # #117: stage → profile name (default = repo fallback)
    default: cheap
    developer: strong
    reviewer: strong
```

Resolution happens **once at load** (`prbot config` / worker startup): the
selected profile is flattened into the repo's effective harness/model/limits, so
no workflow ever resolves a profile name. The resolved stage→profile map and a
content **digest** land in `outcome.json` (`profiles` / `profileDigest`) and are
covered by the #88 policy digest, so a profile edit is a reviewable diff. An
unknown profile, harness, or model name **fails at load**, and two selected
profiles that disagree on the single harness/limits the pipeline can express
fail closed.

**Backward compatibility.** A repo with no `lifecycle.agents` keeps today's
behavior exactly — the legacy `lifecycle.harness`/`model`/`models`/`limits` keys
are the implicit profile. Declaring both a profile and a **conflicting** legacy
key (e.g. a profile harness of `opencode` alongside `lifecycle.harness: generic`)
fails at load with a clear error; agreeing values are allowed.

**Tool registry + permissions (#119).** A tool catalog and named MCP server
registry let an operator declare exactly what an agent may use. Built-in tool
ids (`read`, `grep`, `glob`, `edit`, `write`, `bash`, `webfetch`, `task`,
`todowrite`, `skill`) are code-level; custom tools are declared in
`tools.custom.<id>` with a description. A profile's `tools` is an advisory
**allow** list: it is recorded in `outcome.json` and covered by the profile
digest. A profile's `deny` is a **hard** restriction: the bundled harnesses
receive no tool allowlist, so a denied tool cannot be removed — konoha **fails
the run closed** rather than silently running with broader capability (§40). An
unknown tool reference **fails at load**.

Named HTTP MCP servers live in `mcpServers.<name>`; a profile references them by
name in `mcp`. Only the resolved set is written into the supporting harness's
git-excluded config (generic harnesses, via the #96 `MCPConfigurer`); a harness
without the capability records the `mcp` surface `SKIPPED` — never a false
success. The resolved names are recorded + digested, and an unknown server
reference **fails at load**. Nothing an issue, PR, diff, or spec says can widen
the tool/MCP set (§27): resolution reads only `konoha.yaml` and `repos.yaml`.

**Model providers — protocols + gateways (#118).** A **foundation** layer, not
yet wired to the agent harness. A provider names a wire **protocol** — or a
**gateway** preset — and the credential **name** (never a value):

```yaml
defaults:
  providers:
    anthropic:
      protocol: anthropic-messages          # openai-responses | openai-chat | anthropic-messages
      apiKeyEnv: ANTHROPIC_API_KEY          # NAME only; the value is never read or recorded
    openai:
      protocol: openai-responses
      apiKeyEnv: OPENAI_API_KEY
    openrouter:
      gateway: openrouter                   # implies openai-chat + a default baseUrl
      apiKeyEnv: OPENROUTER_API_KEY
    local-ollama:
      gateway: ollama                       # no key needed
```

Gateways are OpenAI-chat-compatible base-URL presets — `openrouter`, `litellm`,
`ollama` — so a gateway is config, not a fourth adapter. A `baseUrl` overrides
the default. Every provider implements the same
`Invoke(ctx, Envelope) (Envelope, error)` contract over a canonical envelope
(messages/tools in, text/tool-calls/usage out); no wire type leaks past it.

**Typed retry (#95).** A transport failure or a **5xx** is retried with
exponential backoff (`worker.retryMaxAttempts`); a **4xx** model/policy error is
**not** retried and fails closed. Response usage is the #87 `HarnessUsage`, so a
provider response feeds the §39 accounting directly.

**Credentials by name (#82).** `apiKeyEnv` is an env-var **name**. The value is
resolved only at `Invoke` time, sent in the auth header, and registered with the
process redactor so it is masked in every log/evidence surface; only the name is
ever recorded (e.g. a `ProviderDescriptor`). An unknown protocol/gateway, a
gateway/protocol mismatch, or a non-bare `apiKeyEnv` **fails at load**.

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

**AI PR description (§25/#115).** Opt in with
`lifecycle.publish.aiDescription: true` (default off). Before opening the PR the
worker runs one bounded agent step over the already-validated artifacts (issue
spec, developer summary, changed files, §26 validation evidence) and posts a
structured body — Summary / Changes / Risks / Validation — bounded in length and
redacted. The body is **evidence only**: it never feeds the reviewer, the §20
risk gate, the §49 merge, or the §19 close policy, so it can never change a
verdict. Malformed output or a generation failure (or the default off) falls
back to the deterministic body and the PR still opens — never a broken PR. No
raw issue text is treated as instructions (§27).

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

All evidence I/O goes through the pluggable `EvidenceSink` seam (#100): the
store owns redaction, path validation, and the artifact layout, while a sink
only moves opaque bytes by root-relative path (`Write`/`Read`/`List`). The
`evidence.sink` key selects the backend — `local` (the default) is the
local-directory sink and is byte-identical to the #83 layout. An
S3/MinIO sink is a **documented, deferred** implementation (YAGNI until
retention/durability needs it), not a stub; an unknown `evidence.sink` value
fails fast at config load, and `localDirSink` is the only backend the factory
builds today.

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

### Host resource ceilings (#102)

Konoha runs agent and validation child processes directly on the operator host
(there is no sandbox — see `docs/security.md`), so a runaway agent can consume
the machine. The optional `limits:` block (global in `konoha.yaml`, overridable
per repo under `lifecycle.limits`) applies **best-effort anti-accident ceilings**
around those processes:

```yaml
limits:
  cpuMilli: 2000        # 2000m = 2 cores
  memoryMiB: 2048
  diskMiB: 1024
  maxWallClock: 30m     # hard kill; must not exceed worker.devTimeout
```

These are explicitly **not a security boundary** — a host process can still see
the host. Enforcement is **Linux-only**: Konoha prefers a `systemd-run --user
--scope` cgroup (`MemoryMax`/`CPUQuota`/`RuntimeMaxSec`), falling back to a
POSIX `sh`+`ulimit` wrapper (`RLIMIT_AS`/`RLIMIT_CPU`/`RLIMIT_FSIZE`, plus
`timeout` for the wall clock). On any other OS (e.g. macOS, the dev host) it is a
documented **no-op**. The applied ceilings and whether they were
`ENFORCED`/`SKIPPED` (with a reason) are recorded in `outcome.json`:

```json
"limits": {"cpuMilli": 2000, "memoryMiB": 2048, "status": "ENFORCED", "mechanism": "systemd-run"}
```

A ceiling kill is a recorded agent failure (`outcome.json` → `agentOutcome:
FAILED`, reason `agent_failure`), never a silent pass; it never changes a
deterministic gate verdict except by killing the process that would have
produced it (§40). Invalid/negative ceilings, or a `maxWallClock` greater than
the agent activity window, fail at config load.

### Network egress — advisory only (#103)

Konoha has **no sandbox** (`docs/security.md`), so it cannot enforce a network
policy: agents and gates run as the operator's OS user with the operator's
network access, and any in-process proxy would be trivially bypassed. The
optional `egress:` block is therefore **advisory**: when set, Konoha exports
`HTTPS_PROXY`/`HTTP_PROXY` (and a loopback `NO_PROXY`) on agent and validation
child processes so **well-behaved** tools honour the operator's allowlisting
proxy.

```yaml
egress:
  proxy: http://127.0.0.1:8888
  allowOut: ["api.anthropic.com", "registry.npmjs.org"]
```

> **This is bypassable and is not a security boundary.** A process that ignores
> the proxy env or opens a raw socket is not stopped. Konoha never labels host
> egress as "enforced" and performs **no L7 credential header injection** on the
> host — there is no trusted mandatory proxy to carry it. `outcome.json` records
> the advisory status + allowlist, never an enforcement claim:

```json
"egress": {"status": "advisory", "proxy": "http://127.0.0.1:8888", "allowOut": ["api.anthropic.com"]}
```

Absent/empty `egress.proxy` is the default and sets **no** proxy env (today's
behavior). Enforced egress (deny-by-default + L7 credential header injection) belongs to the
container/microVM executor ([#92](https://github.com/vitdo82/konoha/issues/92)),
where it becomes mandatory; see
[`docs/non-goals.md`](docs/non-goals.md) → A4/B2.

### Outbound URL policy (SSRF) + referenced secrets (#122)

Each repo may declare an outbound **URL policy** for its agent and validation
children — an SSRF control that allow/denies by **host** (`*.example.com`) or
**CIDR** (`10.0.0.0/8`, `169.254.169.254`). **Deny wins**, and a non-empty
`allow` list means **default-deny**:

```yaml
# repos.yaml
lifecycle:
  urlPolicy:
    allow: ["api.anthropic.com", "registry.npmjs.org", "*.githubusercontent.com"]
    deny: ["169.254.169.254", "10.0.0.0/8", "metadata.internal"]
  secrets: ["env://ANTHROPIC_API_KEY", "op://vault/github-token"]
```

> **This is advisory on the host and is not a security boundary.** Konoha cannot
> intercept a child's sockets (no sandbox — `docs/security.md`), so a denied host
> is **recorded, not blocked**; a process that opens a raw socket is not stopped.
> Konoha never labels the host policy "enforced". A denial only fails closed
> where enforcement is possible (inside a sandbox), which the ticket defers to
> [#92](https://github.com/vitdo82/konoha/issues/92). `outcome.json` records the
> resolved rules + status:

```json
"urlPolicy": {"status": "advisory", "allow": ["api.anthropic.com"], "deny": ["10.0.0.0/8"]}
```

**Referenced secrets are names only.** `lifecycle.secrets` lists *reference
names* (`op://`, `vault://`, `env://`, `file://`); resolution is delegated to the
#CU1 secrets store (resolved out-of-process, model-invisible). Only the names
land in evidence — never a value:

```json
"secretNames": ["env://ANTHROPIC_API_KEY", "op://vault/github-token"]
```

Konoha also ships `RequireHTTPSOrLoopback` — the SSRF scheme guard for a URL
Konoha itself dials: absolute, no userinfo/fragment, HTTPS unless loopback. Like
the policy engine, it is unit-tested and wired by the enforcement path rather
than a host child. See [`docs/security.md`](docs/security.md) →
"Outbound-request (SSRF) URL policy".

### Machine providers (#121) — local default, no sandbox

Konoha's execution fabric is a `Provider`/`Machine` seam, added after the
[#92](https://github.com/vitdo82/konoha/issues/92) evaluation returned **do not
adopt a sandbox now** (`docs/security.md` → "Sandboxed execution evaluation";
`docs/non-goals.md` → A1 is the revisit trigger). There is **no sandbox
provider**: the built-in `local` provider is the default and reproduces today's
host/worktree behavior exactly.

- `Provider` (workflow-only): `Name`/`Capabilities`/`Create`/`Reattach`/
  `Destroy`/`List`/`Ping`.
- `Machine` (agent-facing): `Execute(Command{Argv, Dir, Env, StdinPath,
  Timeout})`, `ReadFile`, `WriteFile`, `Destroy`. **Structured argv only** — the
  interface has no shell-string form.

The `local` provider:

- `Create` materializes the deterministic, workflow-owned git worktree (the same
  §4.4 claim as the developer path); a foreign/unclaimed directory is refused.
- `Execute` runs a bare `argv` child on the host with the §23 scrubbed env (no
  `GH_*`/cloud credentials), the #103 advisory egress proxy, and the #102
  best-effort ceilings; a caller-supplied env can never re-add a stripped
  credential.
- `Destroy` removes only a worktree whose claim matches the requesting workflow.

**Ownership rule:** the workflow owns infrastructure; an agent only operates
inside the machine it was given and never creates/reattaches/destroys one. The
`local` provider advertises **no** snapshot/clone/suspend/preview capability, so
a requested capability negotiates to `SKIPPED` — never a false success (§40).
An adapter cannot bypass claim authority, workflow policy, review gates or
tracker reconciliation: `machine.go` references no GitHub runner, policy gate,
label projection or reconciliation (the [#142] adapter boundary). This is a
**foundation, not wired into the agent path yet**; a sandbox provider is added
only after a #92 revisit trigger fires (rootless container first, microVM only
for a kernel-level/multi-tenant threat model).

[#142]: https://github.com/vitdo82/konoha/issues/142

### Policy tenancy / scopes — narrow-only, not isolation (#104)

Several teams can share one Konoha worker by assigning repositories to **scopes**.
The tenancy shape is Paddock's **Facility → Product Line → Department**: the
Facility is the whole `scopes:` block (the optional top-level `facility:` label),
a **Product Line** groups repositories for one product, and an optional
**Department** is a functional area within it.

```yaml
facility: acme
scopes:
  payments:                       # Product Line
    repositories: ["acme/payments", "acme/payments-infra"]
    harnesses: ["opencode"]
    models: ["cheap", "strong"]
    budgets: ["small"]
    approval: true                # optional per-scope gate override (may only turn it ON)
    closePolicy: risk-based       # optional; may only narrow
    departments:                  # optional; may only narrow the product line
      checkout:
        repositories: ["acme/payments-checkout"]
        models: ["strong"]
```

A `repos.yaml` entry maps to a Product Line **by default** (the scope whose
`repositories` lists it); it may name one explicitly with `productLine:` and an
optional `department:`. Resolution runs **exactly once at load**
(`prbot config` / `apply.sh` / worker bootstrap): the resolved scope name and the
narrowed effective policy travel in the schedule input, so **no workflow ever
resolves a scope name** and replay stays deterministic. The resolved scope is
recorded in `outcome.json` and folded into the #88 policy digest, so a change of
scope is a visible diff.

A scope can only ever **narrow**:

- a **repository**, **harness**, **model**, or **budget** reference not covered
  by the global allowlist (repos.yaml entries, the built-in harness names, the
  `defaults.agents` catalog) **fails at load** — never a silent drop;
- `closePolicy` may only become **more** human-gated (`risk-based` or
  `human-required`; never `autonomous`);
- `approval: true` may only turn the §55 gate **on** (and still requires
  `lifecycle.audit.enabled`, since every gated decision must be recorded);
- a department's lists must be subsets of its product line's, and its
  `closePolicy` at least as strict.

> **This is policy tenancy, not execution isolation.** Scopes constrain what a
> `repos.yaml` entry is *allowed* to do; they share the host filesystem and
> process space. There is **no per-tenant GitHub App credential** — Konoha still
> uses one App via `GH_APP_ID`/`GH_INSTALL_ID`/`GH_APP_PEM`, and `GH_*` remains a
> non-configurable contract (§23). A credential-shaped key in a scope is
> rejected at load. Execution isolation (per-tenant boundary) is the sandbox
> path ([#92](https://github.com/vitdo82/konoha/issues/92)); see
> [`docs/non-goals.md`](docs/non-goals.md) → B3.

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
| Console | `KONOHA_CONSOLE_ADDR` + `KONOHA_CONSOLE_TOKEN` | optional #105/#108 console (read + decisions); **loopback only** |
| MCP | `KONOHA_MCP_ADDR` + `KONOHA_MCP_TOKEN` (+ `KONOHA_MCP_CONTROL`) | optional #116 read-only MCP surface; control opt-in; **loopback only** |
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
  configured notification targets). The same flag makes a terminal
  `BLOCKED`/`FAILED`
  cycle start `FailureFeedbackWorkflow`, which files at most one bug issue per
  run (`EnsureFailureIssue`, marker `<!-- konoha-failure: … -->`) carrying the
  structured failure; the filed issue is a normal issue (no `konoha:managed`
  label) so the sweep drives it back through the lifecycle. Alerting never
  resolves a stuck run — Temporal state stays authoritative.

  ### Notification targets (#123)

  Notification delivery is pluginized behind a `Notifier` interface
  (`Notify(ctx, NotifyEvent) error`), decoupled from the §39 metrics layer.
  Delivered event kinds are §53 metric alerts, terminal cycle outcomes
  (`CLOSED`/`BLOCKED`/`FAILED`/`HUMAN_REQUIRED`), and failure-feedback filings.
  Configure one or more targets under `lifecycle.alerting.targets`:

  ```yaml
      alerting:
        enabled: true
        targets:
          - type: slack               # Slack incoming webhook ({"text": …})
            url: https://hooks.slack.com/services/T…/B…/…
          - type: webhook             # generic JSON webhook ({kind, repo, text, dedupeKey})
            url: https://example.com/konoha-hook
          - type: log                 # log-only; no url
        thresholds:
          autonomous_completion_rate: 0.7
  ```

  An unknown `type` (or a missing/forbidden `url`) fails at config load. The
  legacy single `notify:` URL still works and is treated as a Slack target.
  Delivery is fail-closed and idempotent: a failed delivery is returned so the
  activity retries, its dedupe key is not recorded (so the retry re-attempts),
  and a `(target, dedupeKey)` already delivered is skipped — a retry can never
  double-notify. Under `DryRun` (§42) notifications are logged, never sent. When
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
docs/non-goals.md        # ESF-vs-Konoha non-goals + caveated features (decision record, #101)
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

## Operator CLI

Operator-facing inspection over the durable evidence store (`evidence.dir`),
plus a `doctor` preflight. Only `status` needs Temporal; the readers work
offline against `evidence.dir`:

```bash
/tmp/prbot doctor                  # preflight; exits non-zero with a specific
                                   # message per failure: konoha.yaml/repos.yaml
                                   # load, Temporal reachable, gh present +
                                   # authenticated, harness on PATH, evidence dir writable
/tmp/prbot status <repo> <issue>   # running cycle's status snapshot (needs Temporal;
                                   # degrades with a clear message when none is running)
/tmp/prbot logs <repo> <issue>     # agent.stdout / agent.stderr / validation.txt (no Temporal)
/tmp/prbot describe <repo> <issue> # outcome.json summary: state, agent/verification
                                   # outcomes, conditions, usage (no Temporal)
```

## Local review console (#105, board + detail #106, decisions #108, queue #109, forms #120)

An opt-in, **loopback-only** HTTP console for the board, cycle detail, the human
decision gate (#108), schema-driven interaction forms (#120), and the
needs-attention queue + recovery actions (#109).
Read-only except for the decisions and queue-action endpoints. It is
**disabled by default** — no listener starts unless `KONOHA_CONSOLE_ADDR` is set
— and start a dedicated process (or let the worker start it when the same env
var is set):

```bash
KONOHA_CONSOLE_ADDR=127.0.0.1:8765 /tmp/prbot console   # standalone, no Temporal required
KONOHA_CONSOLE_ADDR=127.0.0.1:8765 make run             # or alongside the worker
```

> **Loopback only.** A non-loopback bind address (e.g. `0.0.0.0:8765`, a LAN IP,
> or a bare `:8765`) is **rejected at startup** with a clear error. The console
> is not an authenticated remote control plane; it is a local operator surface.

Auth:

- **CLI/worker API clients** present a bearer token (`KONOHA_CONSOLE_TOKEN`). If
  unset, a per-process token is generated and logged at startup.
- **Browsers** fetch a per-process **CSRF token** from `GET /api/v1/status` and
  send it on every API call; browser **mutations** (the decisions endpoint) also
  require a loopback `Origin`. All token comparisons are constant-time.

Endpoints:

| Endpoint | Returns |
|---|---|
| `GET /api/v1/status` | `{version, readOnly, decisions, queue, stream, csrfHeader, csrfToken, forms}` (unauthenticated bootstrap; `forms` is the #120 schema catalog when decisions are enabled) |
| `GET /api/v1/metrics` | #112 metrics dashboard: the existing §39 rate gauges + throughput + #87 cost/tokens, each optional (a missing figure renders "no data") |
| `GET /api/v1/cycles` | board rows (finished cycles from evidence + running from Temporal) **and** the grouped `columns` |
| `GET /api/v1/cycles/{id}` | cycle detail: outcome/status, ordered conditions, the outcome-conflict flag, the Diff/Validation/Metadata projections, and the open `gate` identity |
| `GET /api/v1/cycles/{id}/stream` | **live SSE** (#110): condition transitions (+ `?watch=1` for a bounded agent command/output tail); resumable via `Last-Event-ID` |
| `GET /api/v1/cycles/{id}/artifacts` | artifact list with `{path, size, contentType, kind, previewable}` |
| `GET /api/v1/cycles/{id}/artifacts/{name}` | **preview** (inline): text/HTML as `text/plain`, images as images; binary/oversize → 415 |
| `GET /api/v1/cycles/{id}/download/{name}` | **download** any artifact (`Content-Disposition: attachment`, correct content type) |
| `POST /api/v1/cycles/{id}/decisions` | **the only mutation** (#108): `approve` / `request-changes` / `deny` |
| `POST /api/v1/cycles/{id}/forms` | #120 typed interaction form: a schema-validated submission mapped onto an existing §18 decision action (no new state) |
| `GET /api/v1/queue` | the #109 needs-attention queue: awaiting-approval / blocked / failed / interrupted cycles with their available recovery actions |
| `POST /api/v1/cycles/{id}/actions` | #109 recovery mutation: `retry` / `cancel` (session verbs `attach` / `pause` / `resume` report `SKIPPED` without a provider, #136/#144) |

### Canonical API contract (#124)

The API is **OpenAPI-first**: `api/openapi.yaml` (OpenAPI 3.1) is the source of
truth for the routes, auth and schemas above. The Go server/client types and the
TypeScript types are generated from it:

```bash
make api          # regenerate Go (oapi-codegen) + TS (openapi-typescript) clients
make api-ts-test  # typecheck + smoke the generated TS client (needs node/npm)
```

- **Go**: `internal/konoha/consoleapi/api.gen.go` (types + client). The console's
  route table (`internal/konoha/console_routes.go`) is the single registration
  source, and `console_contract_test.go` fails if a route, method, auth tier or
  response field drifts from the spec.
- **TypeScript**: `api/generated/ts/api.gen.ts` (types) + `client.ts` (a thin,
  dependency-free `fetch` client typed by them).
- **Never hand-edit generated files** — edit `api/openapi.yaml` and run
  `make api` (see `AGENTS.md`).

The CLI can read the running console through the generated client (no ad-hoc
HTTP/JSON):

```bash
KONOHA_CONSOLE_ADDR=127.0.0.1:8765 KONOHA_CONSOLE_TOKEN=<token> /tmp/prbot api cycles
# prbot api <status|metrics|cycles|cycle <id>|artifacts <id>|queue>
```

### Board + cycle detail (#106)

- **Board** groups cycles by the authoritative §6 state into five fixed columns —
  **Running**, **Needs attention** (awaiting approval / blocked), **Human
  required**, **Closed**, **Failed** — with a count per column and newest-first
  (by latest condition timestamp, ties by descending id) ordering. A card shows
  priority and assignee/owner when recorded on the outcome (projected from issue
  intent) and simply omits them when absent.
- **Cycle detail** has four tabs:
  - **Result** — terminal §6 state, `agentOutcome`, `verificationOutcome`, and the
    ordered condition list (#84).
  - **Diff** — changed files + the patch (from `diff.patch`), with line-anchored
    comment threads (an optional `comments.json`; absent → the diff renders with
    no threads). When a gate is open the reviewer anchors a comment to a
    `file:line` (a staged draft, submitted with request-changes, #113); carried
    comments from an earlier revision are rendered and marked **stale** when
    their SHA is not the current reviewed SHA.
  - **Validation** — the deterministic verification outcome, the combined
    `validation.txt`, and each §26 gate's captured stdout/stderr.
  - **Artifacts** — every evidence artifact with size/type; **Preview** (text up
    to 1 MiB; HTML shown as text, never executed; images as images) and
    **Download** (any artifact, as an attachment).
  - **Metadata** — repo/issue, harness/model + policy digest (#88), usage/cost
    (#87), and attempt number (#95).
- **"Summaries are not proof":** agent outcomes and deterministic verification
  outcomes are separate, non-collapsible fields. A **conflict** (agent `SUCCESS`
  while verification `FAILED`/`ERROR`) is highlighted and never masked by the
  agent's prose. The board/outcome/diff/comments projections are pure and
  unit-tested without a browser or Temporal.

### Safe artifact viewer (#107)

Human review must read what the agent actually produced, without the console
ever executing it:

- **List** every artifact for a cycle with its path, size, content type, and
  whether it is previewable.
- **Download** *any* artifact with `Content-Disposition: attachment` and a
  correct `Content-Type`.
- **Preview** is capped at **1 MiB** and served **inline**: text/markdown/JSON
  as `text/plain`, and **HTML is displayed as text, never executed**
  (`text/plain` + the global CSP + `X-Content-Type-Options: nosniff`); images
  render as images; binary or oversize artifacts are **download-only**.
- **Safety:** artifact paths are evidence-store-relative only — traversal is
  rejected (`400`); the viewer reads only the configured evidence store, never
  an arbitrary host path. Untrusted issue/agent text is data in the UI too (it
  is rendered via `textContent`, never `innerHTML`). The pure listing/preview
  projection is unit-tested without a browser.

### Human decisions: approve / request-changes / deny (#108)

When a repo has the §55 pre-execution gate on, a cycle waits in
`PENDING_APPROVAL` and the console detail shows a **Decision** panel bound to the
exact attempt + reviewed SHA the view rendered. The panel is a thin client over
the **existing authoritative channel** — it never becomes a second source of
truth:

- **Approve** writes `<!-- konoha-decision: approve -->` to the issue (the §18
  decision channel the CLI/GitHub path already uses), so `AwaitApproval`/
  `ReadHumanDecision` resume the cycle deterministically.
- **Deny** writes `deny` → `HUMAN_REQUIRED` (unchanged). A **timeout** still
  stays `BLOCKED`; the UI never implies approval.
- **Request changes** writes `request-changes` plus a size-bounded feedback body
  and optional diff-anchored comments. It records a #97 **rework activation**
  carrying the feedback and returns the record to the working path; the
  diff-anchored comments are persisted as the cycle's `comments.json` and shown
  on the next review. At the publish gate the workflow runs a bounded repair
  with the feedback and re-gates (capped by `maxDevReviewAttempts`).
- **Diff-anchored comments (#113):** a comment is stored anchored to
  `(sha, file, line, side)`; its body is redacted (#82) before persistence. The
  bundle is delivered to the repair agent as deterministic, parseable BLOCKING
  findings — `file:line (side): body`, ordered by file/line/side — appended
  after the freeform feedback (`reworkBlocking`). Comments carry forward across
  revisions through the #97 feedback thread and `comments.json` (merged by a
  content-addressed stable id, so a replay/overlap never duplicates, §24); a
  comment bound to a superseded SHA is **stale** — retained and visibly marked,
  but the server refuses it as a re-submission. The comment→feedback projection
  is pure and unit-tested without a browser or Temporal (§44 #20: no new source
  of truth — everything flows through the existing decision/feedback channel).
- **Authority + idempotency:** only the **current** gate may be actioned — a
  superseded attempt/SHA is refused with `409`, and a repeated submit is a no-op
  (one decision per gate, keyed by a byte-exact id marker). Diff-anchored
  comments bound to a superseded SHA are **stale** and cannot be re-submitted.
- **Audit:** every accepted decision is appended by the existing §55
  `RecordAuditEvent` with the actor, step, and decision (`allow` / `deny` /
  `request_changes`).
- No new database, no parallel state machine: the §6 workflow state remains
  authoritative (§44 #20) and the decision is observable via the existing
  `konoha-decision` marker.

Writing a decision posts an issue comment, so the console shells `gh` (the
operator's GitHub auth) **only** on a decision — the read-only paths still touch
no GitHub credential. `repos.yaml` is read (policy only) to resolve the repo
owner and the audit sink.

### Typed interaction forms (#120)

The Decision panel is **schema-driven**: `GET /api/v1/status` advertises a closed
form catalog (also rendered as typed inputs by the SPA), and a submission goes to
`POST /api/v1/cycles/{id}/forms`. A form is a typed, validated wrapper around an
**existing** §18 decision action — it introduces no new state and no parallel
writer (§44 #20):

| Form | Mapped §18 action | Fields |
|---|---|---|
| `approve` | `approve` | — |
| `request-changes` | `request-changes` | `summary` text (required, ≤8 KiB); `severity` choice (`low`/`medium`/`high`, optional); diff-anchored comments |
| `clarification` | `resume` | `answer` text (required, ≤8 KiB); `confidence` number 0–1 (optional) |

- **Validation fails closed (§36).** An unknown form or field, a missing required
  value, a wrong type, a value outside a choice's closed set, and an over-cap or
  out-of-range value are all rejected (`400`) and **never** become a decision. The
  validator is pure and unit-tested without a browser or Temporal.
- **Mapping only.** A valid submission is mapped to its existing §18 action and
  written through the **same** gate/validation/writer as a raw #108 decision
  (`SubmitHumanDecision`/`WriteConsoleDecision`), so the `konoha-decision` marker
  remains the single source of truth and the GitHub channel accepts it unchanged.
- **Clarification pauses and resumes.** A clarification form is answered at the
  §55 gate: the cycle waits in `PENDING_APPROVAL`, a valid answer maps to `resume`
  (which `EvaluateApproval`/`ApplyHumanDecision` treat as continue), and a timeout
  keeps the existing `BLOCKED` semantics (§40 — never a silent proceed).
- **Redaction, audit, idempotency (§82/§55/§24).** Freeform form text and anchored
  comment bodies are redacted before they become a decision; every accepted form
  is appended to the §55 audit sink with the actor + mapped decision; and a
  repeated submission for the same gate is a no-op (one decision per gate).

The SPA is served from embedded assets with a strict `Content-Security-Policy`
(`default-src 'none'; script-src 'self'; …`), no inline script, and it never
executes artifact content. The read-only paths make **no outbound network calls**
beyond the operator's own Temporal server (optional; finished cycles render
without it). Malformed cycle ids are `400`, unknown cycles are `404`, a stale
gate is `409`, and every handler is time-bounded. This is policy-read over the
same evidence store the CLI reads; see `docs/non-goals.md` → A2.


### Needs-attention queue + recovery (#109)

The console surfaces a **Needs attention** queue of everything waiting on a
human: **awaiting approval** (`PENDING_APPROVAL`), **blocked**, **failed**, and
**interrupted** cycles. It is the same pure projection the tests exercise
(`GET /api/v1/queue`), deterministic and newest-first.

Recovery follows Machinist's rules — **retry preserves earlier attempts, and
there is no silent replay of external effects**:

- **Retry** re-runs the cycle as a **new attempt** (Temporal starts a fresh run
  under the same deterministic `<repo>-issue-<n>` id, so prior runs and the #88
  lineage / #97 change history are preserved). It does not discard successful
  earlier work (the branch/PR are idempotent).
- **Cancel** asks a running cycle to stop through the authoritative Temporal
  lifecycle (the disconnect-safe worktree teardown still runs). The stopped
  state is reflected before a retry is allowed — a cancel is idempotent (a
  stopped cycle is a `NOOP`) and never auto-replays.
- **Interrupted** (the worker/lease was lost) is never auto-queued for retry:
  the operator must tick an explicit **"process stopped"** confirmation, and the
  retry best-effort stops any still-registered execution before starting a new
  run.
- **Attach / pause / resume** are session verbs (#136/#144). With no session
  provider wired they render **disabled** and report `SKIPPED` — never a false
  success (§40).
- **Authoritative + audited + idempotent:** every accepted retry/cancel goes
  through the authoritative channel and is appended to the §55 audit sink with
  the actor and step (`queue-retry` / `queue-cancel`); a repeated action is a
  `NOOP`, never a second mutation (§24).
- **DryRun (§42):** with the repo's `dryRun` set, retry/cancel render disabled
  and return a `PREVIEW` with **no mutation**.

### Live progress stream (#110)

`GET /api/v1/cycles/{id}/stream` is a read-only **server-sent events** stream so
the console can show progress between §6 transitions without polling. Same bearer
auth as every other route; it never mutates anything and it is not a second
source of truth — the events are projections of the durable status/evidence
records.

- **Condition transitions** are always streamed: a `state` event per §6 state
  change and a `condition` event per new ordered condition (#84). A terminal
  cycle ends with an `end` event and the stream closes.
- **Watch mode** (`?watch=1`) additionally tails the cycle's agent
  command/output activity as `output` events, read from the recorded (redacted)
  agent stdout artifact — so it is bounded and never bypasses the §23/§27
  redaction layer. The tail is capped (`streamOutputCap`, 64 KiB) so a huge agent
  log can never grow console memory; the full artifact stays in the evidence
  store. The source is a seam (`consoleStreamSource`), so a redaction-aware live
  environment tee can feed the same stream without changing the wire.
- **Resumable:** every event carries a monotonic `id`; a reconnect sends
  `Last-Event-ID` (or `?lastEventId=`) and the server replays only the buffered
  events after that cursor — no duplicates. A client that falls behind is sent a
  `reset` event and reconnects to resume.
- **Bounds:** buffered output is capped by bytes and the replay buffer by event
  count; heartbeats (`: ping`) keep idle connections alive; a
  `PENDING_APPROVAL`/gate wait stays open (only true §6 terminal states close).

### Metrics dashboard (#112)

`GET /api/v1/metrics` surfaces the observability figures the worker already
computes — no new instrumentation. It is a **pure read-only projection** over the
in-process §39 registry snapshot plus the durable per-cycle `outcome.json`
records, so it works both when the worker hosts the console (live figures) and
standalone (`prbot console`, derived from evidence):

- **Rates** — `autonomous_completion_rate`, `human_escalation_rate`,
  `split_rate`, `review_disagreement_rate` (the existing §39 gauges).
- **Throughput** — completed cycles per day over the observed window.
- **Cost/tokens** (#87) — per completed issue, plus totals.

Every figure is optional: when nothing has been reported it renders **"no data"**
and is `null` on the wire — a missing number is never shown as a fabricated zero
(the §87 rule). The projection is unit-tested without a browser or Temporal. This
is policy-read over the same evidence store as the rest of the console; see
`docs/non-goals.md` → A2.

### Preview pane (not implemented — blocked on isolation #114)

A per-cycle **dev-server + embedded browser preview** (Vibe-Kanban style) is
**not available** and there is **no preview proxy, preview route, or dev-server
lifecycle**. It is deferred until Konoha gains an execution-isolation boundary
(#92): the preview serves a repository's own bytes, and the #102 resource
ceilings and #103 egress policy are **advisory/best-effort on the host, not
confinement** (`docs/non-goals.md` → B1/B2). On the current shared host a preview
would be a second, unpoliced execution surface, so it stays out.

When (and only when) isolation lands, the preview is planned as
**default-closed and operator-configured** — a repo names a run command, a caller
can never supply one — **loopback-only and origin-checked** like the rest of the
console (#105/#111), bounded by #102, torn down with the cycle (#93), and
**origin-isolated** from the console origin (separate origin or sandboxed iframe)
so untrusted served content is never treated as trusted console content. See
`docs/non-goals.md` → conditional exceptions and
[#114](https://github.com/vitdo82/konoha/issues/114).

## MCP server (#116)

An opt-in, **loopback-only**, **read-only** Model Context Protocol surface so an
operator's local agent can ask "what needs attention?", "what is the status of
cycle X?", "show the latest failure" without scraping the evidence store. It is
**disabled by default** — no listener starts unless `KONOHA_MCP_ADDR` is set —
and reads no GitHub credential. Start a dedicated process, or let the worker
start it when the same env var is set:

```bash
KONOHA_MCP_ADDR=127.0.0.1:8766 /tmp/prbot mcp   # standalone, no Temporal required
KONOHA_MCP_ADDR=127.0.0.1:8766 make run         # or alongside the worker
```

> **Loopback only.** A non-loopback bind address (e.g. `0.0.0.0:8766`, a LAN IP,
> or a bare `:8766`) is **rejected at startup**, exactly like the console.

Transport is MCP **Streamable HTTP** (JSON-RPC 2.0 over `POST /mcp`); auth is a
bearer token (`KONOHA_MCP_TOKEN`, generated and logged when unset), compared
constant-time. Tools are thin projections over the **same data as the CLI (#99)
and console (#105)**: every read is redacted (#82) and size-bounded, and
`get_evidence` paths are evidence-store-relative only (traversal rejected, #83).

| Tool | Returns |
|---|---|
| `list_cycles` | board rows (finished from evidence + running from Temporal), optionally filtered by `repo` / `state`, plus the grouped `columns` |
| `get_cycle` | one cycle's detail: state, agent/verification outcomes, ordered conditions, artifacts, diff/validation projections |
| `get_evidence` | one cycle's artifact list, or one artifact's redacted, path-validated content |
| `environment_list` | one cycle's recorded evidence artifacts (its inspectable environment) |
| `environment_log` | one cycle's agent stdout/stderr + validation output (same data as `prbot logs`) |
| `environment_diff` | one cycle's recorded `diff.patch` |

There are **no mutating tools by default**. Setting `KONOHA_MCP_CONTROL=1` adds
exactly one opt-in control tool, `post_decision` (`approve` /
`request-changes` / `deny`), which binds to the live gate attempt+SHA and writes
**only** through the authoritative §18 decision channel (#108) — never a parallel
path. It is default-closed; the mutating environment operations (create/write/
exec/delete) of Container Use's MCP tools are deliberately **not** exposed, and
`request_retry` is deferred because it would need a new §18 action outside the
#108 closed set (the queue retry/cancel surface stays console-only, #109). See
`docs/non-goals.md` for the no-sandbox boundary.

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