# ESF-vs-Konoha non-goals (decision record)

Status: **accepted.** This record captures the ESF-vs-Konoha comparison's
residual gap as two intentional lists so it is reviewable rather than drifting.
It is **documentation only** — no production behavior changed (ticket [#101]).

Konoha is **local-first**: a single Go binary (Temporal SDK + `yaml.v3`) that
shells out to `gh` and a local `opencode`/`generic` harness in operator-owned git
worktrees. Its control surface is the CLI + GitHub. The comparison produced two
classes of gap: features Konoha will not adopt, and features once labelled
"by design non-goals" that are in fact implementable **with explicit
boundaries** (now ticketed).

- **A. Hard non-goals** — revisit only on a change of premise.
- **B. Implementable with caveats** — not non-goals; each carries a boundary
  that must be stated, and is ticketed ([#102]/[#103]/[#104]).

[#91] and [#92] are the **conditional exceptions** referenced throughout: they
are deferred, not refused, and each unblocks a different class of work.

## A. Hard non-goals

Revisit each only if its **premise** changes (stated per entry) — not merely
because effort is available.

### A1. MicroVM / sandbox execution fabric

- **ESF does:** executes every agent/gate in an owned guest kernel (microVM) with
  deny-by-default egress and no host reachability.
- **Konoha does:** runs validation gates and agents **on the operator host** as
  the operator's OS user. The controls it relies on are *environmental and
  structural* — scrubbed env (no `GH_*`, no cloud creds), argv-only gates, an
  owned worktree, no GitHub write credential — **not OS confinement**
  ([`docs/security.md`](security.md) → "Host-execution trade-off").
- **Revisit when:** multi-tenant **execution isolation** becomes a requirement —
  i.e. the threat model starts including intentionally hostile repo build code or
  a hosted/multi-tenant worker. The sandbox spike is [#92]; see
  [`docs/security.md`](security.md) → "Sandboxed execution evaluation" for the
  full option comparison and revisit triggers.

### A2. Web UI / authenticated control plane

- **ESF does:** exposes an authenticated web control plane.
- **Konoha does:** control is the **CLI + GitHub** — Temporal schedules, the
  operator CLI (`doctor`/`status`/`logs`/`describe`), and GitHub issues/PRs/labels
  as the human surface. There is no server-side console.
- **Revisit when:** an operator workflow genuinely cannot be expressed through
  the CLI + GitHub (e.g. multi-operator RBAC). The operator CLI lands in the
  `v4.2-console` milestone; that is the current answer to "a UI".

### A3. OpenTelemetry

- **ESF does:** exports OpenTelemetry traces/metrics.
- **Konoha does:** exposes stdlib `/metrics` with **stable §39 names** (no
  `prometheus`/OTel dependency — see `observability.go`, `METRICS_ADDR`). This is
  a deliberate zero-dependency choice.
- **Revisit when:** a downstream observability stack *requires* OTLP wire format
  rather than scrape-time exposition. Until then, scraping `/metrics` covers it.

### A4. Enforced network egress + L7 credential header injection

- **ESF does:** enforces a network egress policy and injects credentials at L7.
- **Konoha does:** neither. Enforcement is **impossible for a host process** —
  agents run as the operator's OS user with the operator's network access, and
  any in-process proxy is bypassable. Host behaviour is **advisory only**
  ([#103]); real enforcement belongs to the sandbox path ([#92]).
- **Revisit when:** the isolation mechanism from [#92] is adopted (a container
  executor with policyable egress). Until then, only the advisory
  [`HTTPS_PROXY` allowlist](#b2-network-egress) is offered, never claimed as
  enforcement.

### A5. Speculative fan-out

- **ESF does:** prepares an environment once, snapshots it, and runs N agents at
  the same task, picking the strongest by deterministic ranking.
- **Konoha does:** **decomposition** fan-out only (Splitter → child DAG, one
  attempt per sub-task). Speculation is designed but deferred (§58,
  `docs/new-design-v2.md`) — it is blocked on the LARGE path + completion gate and
  on an isolation/snapshot mechanism.
- **Revisit when:** the preconditions in [#91] hold (LARGE path + completion gate
  wired, and an isolation provider that can snapshot/clone is chosen). Do not
  declare a half-built `Snapshotter` or a no-op provider.

### A6. Sandbox execution

- **Konoha does:** nothing sandboxed — evaluation only.
- **Revisit when:** see A1/[#92]. This is the same premise as A1 stated from the
  "execution" angle; both are governed by the [#92] decision.

## B. Implementable with caveats (ticketed — not non-goals)

These were previously dismissed as "by design non-goals". They are implementable
**within a stated boundary**. Each boundary is what keeps them from masquerading
as a stronger guarantee than they are (§40: never a silent over-claim).

| Feature | Boundary that must be stated | Ticket |
|---|---|---|
| Host resource ceilings (CPU/mem/disk/TTL) | Linux best-effort **anti-accident**, **not** a security boundary; no-op elsewhere | [#102] |
| Network egress | Advisory `HTTPS_PROXY` allowlist, **bypassable**; enforcement deferred to [#92] | [#103] |
| Policy tenancy / scopes | Narrow-only **policy** tenancy, **not** execution isolation; no per-tenant GitHub App credentials (`GH_*` contract unchanged) | [#104] |

### B1. Host resource ceilings (CPU/mem/disk/TTL) — [#102]

- **Boundary:** applied best-effort on Linux (rlimits/cgroup-style) to stop a
  runaway *accident*; explicitly **not** a security boundary — a determined
  process is not confined, and the controls are a no-op on platforms that lack
  them. Never reported as an enforced sandbox.
- **Upgrade condition:** becomes enforcement only when a sandbox/container
  executor ([#92]) applies the ceilings at the isolation layer.

### B2. Network egress — [#103]

- **Boundary:** an **advisory** `HTTPS_PROXY` allowlist that shapes well-behaved
  agent traffic; **bypassable** by a process that ignores it. Enforcement is
  deferred to [#92] (see [A4](#a4-enforced-network-egress--l7-credential-header-injection)).
- **Upgrade condition:** [#92] adopts a container executor with policyable
  egress; then the advisory proxy is replaced by (or backed by) real enforcement.

### B3. Policy tenancy / scopes — [#104]

- **Boundary:** **narrow-only** *policy* tenancy (a tenant may only narrow the
  effective policy, never widen it). It is **not** execution isolation and does
  **not** introduce per-tenant GitHub App credentials — the `GH_*` contract and
  the single-App model are unchanged (§23).
- **Upgrade condition:** if [#92] adopts a container executor, tenancy graduates
  from policy scoping to **execution isolation** (per-tenant boundary), at which
  point the credential model is revisited separately.

## Conditional exceptions

- **[#91] Speculative candidate fan-out** — deferred, not refused. Unblocks when
  the LARGE path + completion gate are wired **and** an isolation provider that
  can snapshot/clone is selected. (§58, `docs/new-design-v2.md`.)
- **[#92] Sandboxed execution evaluation** — the **deferred** path that, if
  adopted, upgrades [A1](#a1-microvm--sandbox-execution-fabric),
  [A4](#a4-enforced-network-egress--l7-credential-header-injection),
  [B1](#b1-host-resource-ceilings-cpumemdisktl--102),
  [B2](#b2-network-egress--103), and [B3](#b3-policy-tenancy--104) from
  advisory/best-effort to enforced.

## Consequences

- The remaining gap is **intentional and reviewable**: each hard non-goal names
  the premise that would reopen it, and each caveated feature names the boundary
  that keeps it honest.
- No new code, config keys, or dependencies; `make test` is unchanged.
- [A1]/[A4]/[A6] and [B1]/[B2]/[B3] all trace their upgrade path to [#92]; that
  decision is the single hinge for "advisory/best-effort → enforced".

[#91]: https://github.com/vitdo82/konoha/issues/91
[#92]: https://github.com/vitdo82/konoha/issues/92
[#101]: https://github.com/vitdo82/konoha/issues/101
[#102]: https://github.com/vitdo82/konoha/issues/102
[#103]: https://github.com/vitdo82/konoha/issues/103
[#104]: https://github.com/vitdo82/konoha/issues/104
