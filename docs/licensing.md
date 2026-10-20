# Licensing and open-core boundary (decision record)

Status: **accepted.** Ticket [#147]. This record fixes the project's license,
the contributor agreement, and the OSS-core vs commercial-add-on boundary. It is
**documentation plus doc-enforced assertions** — no production behavior changed.

## Decision

1. The repository is licensed **Apache-2.0** (`LICENSE`), with attribution in
   `NOTICE`. Everything committed here is Apache-2.0: there is no mixed-license
   directory, no proprietary file, and no per-file license header scheme.
2. Contributions require a **DCO 1.1 sign-off** (`Signed-off-by:`) enforced by
   `.github/workflows/dco.yml`; the process is documented in `CONTRIBUTING.md`.
3. The commercial product is a **separate, proprietary add-on** that consumes
   this core through its process boundaries (CLI, HTTP console/MCP API,
   `konoha.yaml`/`repos.yaml`) and is **not** part of this repository.
4. The layout is classified below. `licensing_test.go` fails when a top-level
   directory appears without a classification, when a classification names a
   directory that no longer exists, or when `LICENSE`/`NOTICE`/the README
   license statement/the DCO process goes missing.

## Why Apache-2.0

Apache-2.0 gives users a patent grant, an explicit trademark carve-out, and a
`NOTICE` propagation rule, which is what an operator running a worker against
their own repositories needs. It also keeps the core adoptable without a
sales conversation, which is the premise of an open-core model. A copyleft
license was rejected because the paid surface must link the core without
infecting its own terms.

## OSS core vs commercial add-on

The boundary is drawn at **deployment scope**, not at which feature looks
"advanced": anything an operator can run on their own host against their own
repositories is OSS. Anything that requires a hosted, multi-tenant service or a
commercial relationship is the add-on.

| Surface | Where | License |
| --- | --- | --- |
| Temporal worker, sweeps, schedules, webhooks | `internal/konoha` | Apache-2.0 |
| Lifecycle state machine + policy engine (`policy.go`, `rollout.go`) | `internal/konoha` | Apache-2.0 |
| Agent harnesses, profiles, tool/MCP registries, model-provider layer | `internal/konoha` | Apache-2.0 |
| Evidence store, lineage, change records, JSONL export (`prbot export`) | `internal/konoha` | Apache-2.0 |
| Local review console (#105), local MCP surface (#116), TUI (#143) | `internal/konoha` | Apache-2.0 |
| CLI (`prbot …`), configs, scripts, OpenAPI spec | `cmd/`, `configs/`, `scripts/`, `api/` | Apache-2.0 |
| Hosted multi-tenant control plane | separate repository/module | commercial |
| SSO / RBAC / per-tenant credential brokering | separate repository/module | commercial |
| Hosted audit retention, compliance reporting, long-term evidence archive | separate repository/module | commercial |
| Enterprise support, SLAs, managed deployment | commercial contract | commercial |

Two consequences worth stating explicitly:

- **The local console is OSS.** The console surface in this repository (#105) is
  Apache-2.0. The commercial console offering is the hosted, multi-tenant
  deployment with SSO/RBAC and retained audit — not the code here. This
  deliberately narrows the sketch in [#147] ("console" as a commercial feature),
  because the console already shipped in the open repository and relicensing it
  retroactively is not possible under the DCO.
- **Local feature parity is not a boundary.** A feature is not commercial
  because it is complex or operator-facing; it is commercial only when it needs
  the hosted service. Moving a feature out later means deleting it from this
  repository, never adding a proprietary license line to an existing file.

## Layout classification

Every top-level directory is classified here. Adding a directory means adding a
row in the same change.

| Path | License | Contents |
| --- | --- | --- |
| `.github/` | Apache-2.0 | DCO workflow and repository automation |
| `api/` | Apache-2.0 | OpenAPI contract, generated clients, TS client |
| `cmd/` | Apache-2.0 | `konoha` entrypoint |
| `configs/` | Apache-2.0 | bundled `konoha.yaml`/`repos.yaml` defaults |
| `docs/` | Apache-2.0 | design, security, and decision records |
| `e2e/` | Apache-2.0 | hermetic tier-1 end-to-end suite |
| `internal/` | Apache-2.0 | worker, policy engine, agents, console, evidence |
| `scripts/` | Apache-2.0 | apply/QA/schedule helpers |

Root files (`README.md`, `LICENSE`, `NOTICE`, `CONTRIBUTING.md`, `AGENTS.md`,
`Makefile`, `Dockerfile`, `docker-compose.yaml`, `go.mod`, `go.sum`) are
Apache-2.0 under the root `LICENSE`.

## Mechanical rules

- **No proprietary code here.** The DCO workflow plus the classification test
  keep the repository single-license. A pull request adding a file with a
  non-Apache license header is rejected on review.
- **No import across the boundary.** The add-on may not import
  `prreview/internal/...` (Go enforces `internal/` for foreign modules anyway).
  The supported seams are the console/MCP HTTP API, the CLI JSON output
  (`prbot api …`, `prbot export`), and the YAML configuration files. Those seams
  are versioned contracts: `api/openapi.yaml` has a generated client and
  `console_contract_test.go` guards it, and `prbot export` carries
  `exportSchemaVersion`.
- **No open-core hooks in the core.** The core contains no license-key check,
  no feature flag that gates an OSS feature behind a purchase, and no phone-home
  call. The paid surface is additive and self-hosted deployments are never
  crippled.
- **Go module metadata.** Go modules have no license field in `go.mod`; the
  root `LICENSE` governs, and `NOTICE` is the attribution file required by
  Apache-2.0 §4(d). `go list -m -json all` reports the dependency graph for
  third-party notice collection.

## Enforcement

`internal/konoha/licensing_test.go` asserts:

- `LICENSE` is the Apache-2.0 text and `NOTICE` carries the project name and
  copyright holder.
- `README.md` states the license and links `LICENSE`.
- `docs/licensing.md` classifies every existing top-level directory and every
  classified directory exists.
- `CONTRIBUTING.md` documents the `Signed-off-by:` sign-off and the DCO
  workflow file exists.

These checks are documentation assertions; they do not prove legal compliance.
A relicensing or dual-license decision still requires the CLA described below.

## Revisit triggers

- **Any relicensing or dual-licensed release.** Adopt a CLA first, collect
  signatures from existing contributors, and update `CONTRIBUTING.md`,
  `docs/licensing.md`, and `NOTICE` together. DCO-only contributions cannot be
  relicensed.
- **A commercial feature that needs core changes** — for example a policy
  engine hook that only the hosted product can supply. Prefer a configuration
  seam over a new in-core conditional; if a new seam is unavoidable, document it
  in this record before merging.
- **Trademark use** beyond reproducing `NOTICE` (product name, logo, domain)
  needs a separate trademark policy; Apache-2.0 §6 grants no trademark rights.

[#147]: https://github.com/vitdo82/konoha/issues/147
