# Contributing to Konoha

Thanks for taking the time to contribute. This document covers the legal
process, the development loop, and where the open-core boundary sits.

## Contributor sign-off (DCO)

Every commit must carry a `Signed-off-by:` line. This is the
[Developer Certificate of Origin](https://developercertificate.org/) (DCO) 1.1:
you certify that you wrote the change, or have the right to submit it under the
project's license.

Sign off when you commit:

```bash
git commit -s -m "fix: correct the CI gate timeout"
```

If you already committed without it:

```bash
git commit --amend -s --no-edit          # last commit
git rebase --signoff <base-branch>       # every commit on your branch
```

The `.github/workflows/dco.yml` workflow checks every non-merge commit in a pull
request and fails the PR when a sign-off is missing. Merge commits are ignored.
The sign-off identity must match a real name and email; use `git config
user.name` / `user.email`, not a placeholder.

**DCO is not a CLA.** Under the DCO, contributions are licensed
inbound = outbound under Apache-2.0 only. The project therefore cannot
relicense or dual-license existing contributions yet. Before any relicensing or
dual-licensed release, a CLA must be adopted and existing contributors asked to
sign it. See `docs/licensing.md` for the license and open-core decision record.

## Development loop

Requirements: Go (see `go.mod` for the toolchain version), `git`, and the `gh`
CLI. A local Temporal server is only needed for the durable runtime; the
`runtime.mode: local` path runs without it.

```bash
make test        # go vet ./... + go test ./... + go build ./...
make build       # build the binary to /tmp/prbot
make api         # regenerate the OpenAPI-derived Go/TS clients after a spec change
```

Before opening a pull request:

- `make test` passes.
- Generated files (`internal/konoha/consoleapi/api.gen.go`,
  `api/generated/ts/api.gen.ts`) are regenerated with `make api`, never
  hand-edited.
- Behaviour changes that touch a documented invariant also update
  `AGENTS.md` and the relevant `docs/` record.
- Commits are signed off (see above).

`AGENTS.md` lists the engineering invariants the codebase enforces — stable
Temporal activity/workflow names, byte-exact idempotency markers, the
`VERDICT:` parser contract, all I/O in activities, and the no-`GH_*`-to-agents
rule. Read it before a non-trivial change.

## Commercial surface

This repository is the Apache-2.0 open-source core. Features intended for the
commercial add-on (SSO/RBAC, hosted audit export, enterprise support) are
developed outside this repository and consume the core through its CLI, HTTP
API, and configuration — never by importing `internal/` packages. Do not add
proprietary code or a proprietary license header to this repository; see
`docs/licensing.md`.

## Security

Do not open a public issue for a vulnerability. Report it privately through
GitHub's private vulnerability reporting on this repository, and include the
affected version or commit.
