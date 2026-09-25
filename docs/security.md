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

## Tests

- `TestHarnessEnvStripsCloudCredentials`, `TestValidationGateScrubsCloudCredentials`
  (`harness_test.go`) — cloud/provider vars absent; allowlist is the only opt-in.
- `TestHarnessEnvAllowlist` — `GH_*` always absent.
- `TestValidationGateArgvNeverInvokesShell` (`validation_test.go`) — argv
  dispatched literally.
- `TestPromptInjectionConfinedToOwnedWorktree` (`developer_test.go`) — injection
  stays in the owned worktree; no Konoha-side `gh` mutation; main checkout clean.
