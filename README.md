# Konoha

Cross-repo PR review orchestrator. Runs a Temporal worker that sweeps repos for
open PRs without a `kakashi-82[bot]` review and reviews each via a local opencode
agent, posting the verdict back to the PR.

## Architecture

- **Worker** (Go, Temporal SDK): `ReviewAllWorkflow` (cron-driven per repo) →
  child `PrReviewWorkflow` (`<repo>-review-<n>`) → activities
  `FetchDiff` → `ReviewWithOpencode` → `SubmitReview`.
- **Config**: `repos.yaml` — one entry per repo (source of truth).
- **Apply**: `apply.sh` (or `temporal schedule create`) creates a
  `<repo>-review-all` schedule per repo, every 5 min.

## Run

```bash
go build -o /tmp/prbot .
GH_APP_ID=<id> GH_APP_INSTALL_ID=<id> GH_APP_PEM=<path> /tmp/prbot
# apply schedules
./apply.sh
```

Env: `GH_APP_ID`, `GH_APP_INSTALL_ID`, `GH_APP_PEM` (GitHub App auth, never
committed). Temporal on `127.0.0.1:7233` (override with `TEMPORAL_ADDRESS`).

## Add a repo

Add one entry to `repos.yaml`, then `./apply.sh`.