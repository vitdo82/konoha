# Security model

Konoha executes repository validation and AI agents on the **operator host**
(no microVM / sandbox). This document states the trust boundary the code
enforces, what it deliberately does *not* enforce, and the host-execution
trade-off. It is the reference for §23/§27 and ticket #90.

## Trusted vs untrusted

| Trusted (operator-controlled) | Untrusted (never authoritative) |
|---|---|
| `configs/konoha.yaml`, `configs/repos.yaml` | GitHub issue text and comments |
| Harness selection + `passEnv` allowlist | PR comments / review bodies |
| Structured validation gates (`argv`) | `README`, `openspec`, source files |
| Workflow/activity code + contract strings | External dependencies |
| | Agent stdout/stderr and its JSON report |

Untrusted content is always **data, never instructions**. It is embedded after
the compiled-in system instruction block, JSON-escaped, inside explicit
`UNTRUSTED ISSUE DATA START/END` markers (`developer.go`, `reviewer.go`,
`analyst.go`). The system block stays a verbatim constant; nothing in the
untrusted region can rewrite it (§27).

## Environment allowlist

Every agent and validation child gets a scrubbed environment:

- **GitHub credentials are stripped unconditionally** — `GH_*` and
  `GITHUB_TOKEN` — for every harness and every validation gate. No allowlist can
  re-enable them (§23).
- **Common cloud/provider credentials are stripped by default** — `AWS_*`,
  `GOOGLE_*`, `AZURE_*`, `OPENAI_API_KEY`, `ANTHROPIC_API_KEY`, `NPM_TOKEN`
  (§90(a)).
- The **`opencode` harness forwards the rest of the host env** minus the above
  (today's behavior). Its minimum viable env is `PATH`, `HOME`, and its own
  provider/model configuration; it needs no cloud credential.
- The **`generic` harness forwards only `passEnv`-listed variables**. This is an
  explicit allowlist: listing a cloud variable (`AWS_PROFILE`,
  `AWS_ACCESS_KEY_ID`, …) is the documented, deliberate way to opt that one in.
  GitHub credentials still cannot be allowlisted.

The choke point is `agentEnv()` (`activities.go`) for the `opencode` harness and
every validation gate, and `genericHarness.Env` (`harness.go`) for the generic
harness. See `isGitHubCredentialEnv` / `isCloudCredentialEnv`.

## Validation gates

Structured gates (`lifecycle.validation`) are dispatched as **bare argv via
`exec.CommandContext`** (`publish.go` `runGate`), one argument per `argv` entry.
No shell is involved, so shell metacharacters in an argument are literal data —
a config value cannot inject a shell command into a structured gate (§90(b)).

`lifecycle.validationCmd` is the explicit, documented **`sh -c` escape hatch**
for repos that genuinely need a shell pipeline. It is mutually exclusive with
`validation` and rejected at config load if both are declared (`config.go`
`validateGates`). Prefer structured `argv`; reach for `sh -c` only when a shell
is unavoidable, and treat that command as trusted operator input.

## Worktree isolation

Each development/repair attempt runs in a deterministic, workflow-owned sibling
git worktree (`developer.go`). The activity:

- refuses to reuse a directory without a matching ownership claim, or one
  claimed by another workflow;
- runs the agent with `cwd` = the owned worktree, and captures the commit SHA
  and changed files **deterministically from git** — never from agent prose;
- never pushes, opens a PR, or edits issues/labels from an agent result (§16,
  §25): those are separate idempotent activities the workflow controls.

A prompt-injection fixture therefore cannot make Konoha perform a mutation
outside the owned worktree: the injected text stays data, the agent's only
output is a local commit, and the main checkout is never touched by the activity.

## Host-execution trade-off

Agents and gates run as the **operator's OS user with the operator's filesystem
privileges**. The controls above are *environmental and structural* — scrubbed
env, argv-only gates, owned worktree, no GitHub write credential — **not a
sandbox**. A sufficiently determined process could still touch host paths it has
permission to write; Konoha does not claim OS-level confinement. To reduce the
blast radius, run the worker as a dedicated user with the least privilege it
needs, keep secrets out of the host environment it inherits, and prefer the
`generic` harness's allowlist over inheriting the full host env.

## Sandboxed execution evaluation (#92, ADR-style)

**Status: accepted — do not adopt a sandbox in this milestone.** This section is
the whole deliverable of the [#92] evaluation spike; **no production behavior
changed**. [T9] (the controls above) is the **accepted mitigation** for the
current posture.

### Context

Konoha runs as a single Go binary (deps: Temporal SDK + `yaml.v3`) on the
operator host, and both untrusted halves of the pipeline execute there:

- **Validation gates** run the repository's own bytes (`npm test`, `make test`, …)
  as bare `argv`, as the operator's OS user.
- **Agents** run the `opencode`/`generic` harness in an owned worktree.

T9's controls are *environmental and structural* — scrubbed env (no `GH_*`, no
cloud creds), argv-only gates, owned worktree, no GitHub write credential. They
defend against *injected content causing Konoha to mutate something*. They are
**not** OS-level confinement: a malicious repo's build/test code still runs with
the operator user's filesystem and network access. That residual risk is what
this spike weighs.

### Options compared

| Option | Isolation | Effort (CP / days) | Ops cost | Security delta vs today |
|---|---|---|---|---|
| **Do nothing (T9)** | none (env/structural only) | 0 / 0 | none | baseline; untrusted repo code runs on host |
| **Rootless container** (podman/`bwrap`/`nsjail`) | namespaces (fs/PID/IPC/mount), policyable egress; shares host kernel | 6 / ~3d | runtime on every worker host; image/toolchain; worktree bind-mount; uid mapping; macOS semantics differ | confines filesystem blast radius to the worktree; no host paths; kernel-LPE escapes remain possible |
| **microVM** (Firecracker/CubeSandbox) | owned guest kernel, deny-by-default egress, no host reachability | 9 / ~2–3wk | KVM + jailer + kernel/rootfs build, vsock/net, snapshot/reattach lifecycle; Linux bare-metal only | strongest: kernel exploit contained; ESF's model |

### Decision

**Do not adopt a sandbox now.** Accept T9 as the mitigation. Rationale, against
Konoha's local-first constraints:

1. **Zero-dependency, single-worker posture** — no container runtime or KVM is
   in today's deployment; adopting one is an ops/deployment change, not a code
   change.
2. **Dev/prod parity** — the operator runs `temporal server start-dev` + `gh` +
   `opencode` locally (often macOS). microVMs are Linux/KVM-only, so local runs
   would be impossible or diverge; rootless containers behave differently across
   host OSes.
3. **Single-tenant threat model** — the operator chooses the repos. T9 does not
   cover *intentionally hostile repo build code*, but that presupposes running
   repos whose content the operator does not control, which is not the current
   model.
4. **Agent egress** — agents must reach their model provider; microVM
   deny-by-default egress needs an allowlist/proxy, i.e. more moving parts for
   the common case.
5. **YAGNI (§58 precedent)** — a `Sandbox` interface with only a no-op
   implementation is the speculative abstraction §58 warns against. Build the
   seam when a second provider is actually justified, not before.

### Revisit triggers (any one re-opens this decision)

- Konoha is pointed at **third-party/untrusted repos** the operator does not
  control; or
- Konoha moves from single-operator local to a **hosted/multi-tenant worker**; or
- a real incident where a gate or agent escaped the worktree / exfiltrated host
  data.

When triggered, implement the **rootless-container** provider first
(best security-per-ops, fits the existing worktree model, no KVM). Reach for a
**microVM** only if the threat model includes kernel-level escape or the workload
is genuinely multi-tenant.

### Provider boundary (shape to hold when built)

- `Create` / `Reattach` / `Destroy` / `List` / `Ping` plus
  `Execute(Command{Argv, Dir, Env, StdinPath, Timeout})`.
- `Argv` is **structured — no shell-string escape hatch** for sandboxed
  execution. `lifecycle.validationCmd` (`sh -c`) stays a host-only *trusted*
  escape hatch; under a sandbox it must be refused, never silently run on the host.
- `Env` is the already-scrubbed agent env; GH/cloud stripping still applies inside.
- The no-op (worktree) provider stays the default; a stronger provider is
  opt-in and an unavailable one **fails closed** — never a silent fallback to
  host execution (§40).

Implemented in [#121]: `machine.go` holds these interfaces and the default
`local` provider, with capability negotiation (a missing capability is recorded
`SKIPPED`, a required-but-missing one fails closed). **No sandbox provider is
added** — this decision stands until one of the revisit triggers above fires.

[#121]: https://github.com/vitdo82/konoha/issues/121

### Consequences

- No new code, config keys, or dependencies; `make test` is unchanged.
- Residual risk is accepted and recorded: untrusted repo build/test code runs as
  the operator user. Mitigate operationally (dedicated least-privilege user,
  secrets out of the inherited env, prefer the `generic` harness allowlist) as
  stated in [Host-execution trade-off](#host-execution-trade-off).
- [#91] (§58 speculative fan-out) stays blocked on "isolation mechanism chosen";
  this record defers that choice until a revisit trigger fires.

[T9]: https://github.com/vitdo82/konoha/issues/90
[#91]: https://github.com/vitdo82/konoha/issues/91
[#92]: https://github.com/vitdo82/konoha/issues/92

## Tests

- `TestHarnessEnvStripsCloudCredentials`, `TestValidationGateScrubsCloudCredentials`
  (`harness_test.go`) — cloud/provider vars absent; allowlist is the only opt-in.
- `TestHarnessEnvAllowlist` — `GH_*` always absent.
- `TestValidationGateArgvNeverInvokesShell` (`validation_test.go`) — argv
  dispatched literally.
- `TestPromptInjectionConfinedToOwnedWorktree` (`developer_test.go`) — injection
  stays in the owned worktree; no Konoha-side `gh` mutation; main checkout clean.
