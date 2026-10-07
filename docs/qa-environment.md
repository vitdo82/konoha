# Hermetic QA environment (Docker Compose, #206)

> **Dev/QA only — never a deploy path.** The image bakes fake `gh`/`opencode`
> and the stack honors the non-prod `KONOHA_TOKEN_FIXED`/`KONOHA_GIT_BASE` test
> seams. It is a sibling of the tier-1 hermetic suite (`e2e/README.md`, #47) and
> is explicitly scoped apart from the self-hosted install story (#148).

One command brings up an isolated stack with **zero secrets**:

```bash
make qa        # docker compose up --wait + seeds one cycle
make qa-down   # docker compose down -v
```

`make qa` is `docker compose up --wait --build` followed by
`docker compose exec -T worker /prbot cycle r 1` (starts the seeded cycle). The
stack runs with **no GitHub credentials and no network egress**: GitHub is a
stateful fake, opencode is a fake, and git pushes go to a local bare remote.

## Services

| Service | Image | Role |
|---|---|---|
| `postgres` | `postgres:16-alpine` | Temporal persistence |
| `temporal` | `temporalio/auto-setup:1.29.7` | Temporal frontend (`:7233`) |
| `temporal-ui` | `temporalio/ui:2.55.0` | schedules/executions UI (`:8080`) |
| `worker` | `Dockerfile` (multi-stage) | runs `/prbot`; hosts the console + metrics |
| `console-bridge` | `alpine/socat` | forwards the container's `:8766` → worker loopback `:8765` (C1) |

Host ports (all bound to `127.0.0.1`):

| Host | Service | Notes |
|---|---|---|
| `7233` | Temporal gRPC | |
| `8080` | Temporal UI | |
| `8765` | console | via the C1 socat bridge |
| `9090` | §39 metrics (`/metrics`) | |

The worker's entrypoint (`scripts/qa-entrypoint.sh`) seeds the fixture state
(`scripts/qa-seed.sh`), applies the QA Temporal schedules (`scripts/apply.sh`
with `KONOHA_SKIP_BUILD=1`), then execs `/prbot`. The `apply` step is folded
into the worker entrypoint rather than a one-shot `apply` service so
`docker compose up --wait` reports on a long-lived, healthy worker (a one-shot
container would make `--wait` fail).

## C1 — console/MCP stay loopback-only

`validateLoopbackAddr`/`validateLoopback` (`internal/konoha/console.go`) reject
any non-loopback bind, for **both** the console and the MCP surface. Binding
`127.0.0.1:8765` *inside* a container is not reachable via Docker's `-p`
port DNAT (it targets the container's `eth0`). This environment **does not
weaken the guard**; instead a `console-bridge` sidecar shares the worker's
network namespace (`network_mode: "service:worker"`) and a `socat` bridge
forwards `:8766` on the container interface to the worker's loopback
`127.0.0.1:8765`. The worker still binds loopback only. `make qa` then reaches
the console at `http://127.0.0.1:8765`.

The `network_mode: host` alternative is Linux-only and not portable; a new
opt-in env to relax the loopback check is a security-boundary change and is out
of scope (see `docs/security-console.md`).

## Configuration overlay

The stack uses a dedicated overlay via `KONOHA_CONFIG_DIR=/app/configs/qa`
(mounted read-only):

- `configs/qa/konoha.yaml` — QA runtime overrides (evidence root `.konoha/evidence`).
- `configs/qa/repos.yaml` — the single fixture repo `o/r`, `repoPath`
  `/work/fixture/r`.

The committed prod `configs/konoha.yaml` and `configs/repos.yaml` are
byte-unchanged (AC6).

## Hermeticity seams

- `KONOHA_TOKEN_FIXED=qa-token` — fixed installation token (no App).
- `KONOHA_GIT_BASE=/work/git-base` — git pushes target
  `/work/git-base/o/r.git` (`scripts/qa-seed.sh` creates it).
- `FAKE_GH_STATE` / `FAKE_OPENCODE_DIR` — fake `gh` JSON store + canned agent
  outputs.
- The image builds with `-tags=e2e` so the fake `gh`/`opencode` shims (behind
  `//go:build e2e`) are baked onto `PATH`.

`/work` is a host bind mount (`./.qa/work`) so the worker's checkout, sibling
`.konoha/` worktree base, and `evidence.dir` are all visible from the host. The
worker runs as the invoking host user (Makefile exports `QA_UID`/`QA_GID`) so
the bind mount is writable and the `0600` evidence files are host-readable.

## Reading evidence from the host

```bash
make build      # host /tmp/prbot (readers need no Temporal)
make qa         # bring the stack up + start the seeded cycle
make qa-describe   # -> cd .qa/work && prbot describe r 1
make qa-export     # -> cd .qa/work && prbot export --format jsonl
```

Equivalently, from `.qa/work`:

```bash
cd .qa/work
KONOHA_CONFIG_DIR="$(git rev-parse --show-toplevel)/configs/qa" /tmp/prbot describe r 1
KONOHA_CONFIG_DIR="$(git rev-parse --show-toplevel)/configs/qa" /tmp/prbot export --format jsonl
```

The reader resolves `evidence.dir` relative to its CWD, so it must run from
`.qa/work` to match the container's `/work`.

## Fail-closed (AC8)

`worker` has `depends_on: temporal: condition: service_healthy` and Temporal has
a `temporal operator cluster health` healthcheck. If Temporal never becomes
healthy, `docker compose up --wait` **fails** instead of starting a worker that
can never poll — no silent success (§40/§44 #17).

## Teardown / idempotency

`docker compose down -v` removes all containers and named volumes. The seed step
resets `./.qa/work` on every `up`, so re-running reproduces an identical result.
`./.qa/` is git-ignored.

## Out of scope

- Real-GitHub staging against a live App install (see #148).
- CI integration / shared hosting.
- Any change to the console/MCP loopback guard.
- Snapshots beyond the Postgres named volume.
