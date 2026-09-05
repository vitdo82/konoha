# E2E suite (tier 1, hermetic)

Scheduler-driven / client-driven end-to-end tests for the issue lifecycle
workflow (#47). Runs the **real** worker + Temporal dev-server with **fake**
`gh`/`opencode` on PATH and a **local fixture git repo + bare remote** — fully
network-independent (no GitHub, no opencode).

```bash
go test -tags=e2e ./e2e/...
```

## What it does

`TestMain` (harness_test.go):

1. Starts a real Temporal dev-server (SDK auto-downloads the temporal CLI).
2. Builds fake `gh` + fake `opencode` binaries into a temp `bin/` dir.
3. Creates a fixture git repo (`main` @ one commit) + a bare remote at
   `<tmp>/o/r.git`.
4. Starts the real worker (`go run ./cmd/konoha`) subprocess with:
   - `PATH` prefixed with the fake binaries,
   - `TEMPORAL_ADDRESS` → the dev-server,
   - `KONOHA_TOKEN_FIXED` (appToken override; hermetic — never set in prod),
   - `KONOHA_GIT_BASE=<tmp>` (git pushes go to the local bare remote),
   - `FAKE_GH_STATE` / `FAKE_OPENCODE_DIR` (fake state + canned agent output).
5. Runs the scenarios; each seeds fake-GitHub state + agent fixtures, starts
   `IssueCycleWorkflow` (direct-sweep mode) or `IssueAllWorkflow` (sweep mode),
   waits, and asserts terminal state / PR / issue-state through the fake store.

## Scenarios

| File | Scenario | Status |
|---|---|---|
| `scenario_01` | SMALL happy path → CLOSED, exactly 1 PR | ✅ |
| `scenario_02` | scripted REQUEST_CHANGES → repair → APPROVE → CLOSED | ✅ |
| `scenario_03` | reviewer always REQUEST_CHANGES → BLOCKED, never closes | ✅ |
| `scenario_04` | dev writes `infra/**` → §20 gate, publish suppressed | ✅ |
| `scenario_11` | dual sweep → one workflow, one PR (convergence §32) | ✅ |
| `scenario_12` | sweep retry → no duplicate PR (§24) | ✅ |
| `scenario_05..10` | completion/disagreement, human/PO, LARGE children | ⏭ pending wiring |

## Fakes

- `fakegh`: minimal `gh` CLI with a JSON store (`FAKE_GH_STATE`). Handles
  `issue list/create/edit/view`, `pr list/create/edit/view/diff/review`, `api`
  (get + PATCH issues, comments), `label list/create`. `pr view --jq .headRefOid`
  resolves the real pushed SHA from the fixture bare remote so `RunRepair`'s
  `git reset --hard <sha>` gets a real object.
- `fakeopencode`: role-detected from the prompt (Issue Analyst/Estimator/
  Developer/Reviewer/Repair/…). Developer/repair write a real file + `git commit`
  in the worktree (so the "agent must commit" invariant holds); reviewer emits
  `$FAKE_OPENCODE_DIR/reviewer.txt` one paragraph per call (pop semantics →
  scripted REQUEST_CHANGES→APPROVE). A `devpath` fixture steers which file the
  dev writes (used by the §20 risk scenario).

## Requirements

`go` + `git` on PATH; the temporal CLI is auto-downloaded by the SDK (may take
~1 min first run). No GitHub credentials needed.

## Not covered here

Tier 2 (real opencode + real GitHub App) is out of scope (see AGENTS.md). The
`KONOHA_TOKEN_FIXED`/`KONOHA_GIT_BASE` overrides are test-only seams and are
never honored in a real deploy (documented in the code).