# Temporal-Optional Runtime (Local Headless Mode) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a `runtime.mode: local` sequential runner that executes the SMALL issue lifecycle in-process (no Temporal server, no client dial) behind a headless `prbot run` entrypoint.

**Architecture:** Keep the Temporal workflow byte-identical in behavior and add a second driver, `local.go`, that calls the same activity functions in the same SMALL-path order and the same pure policy functions. Extract the runtime-neutral terminal assembly out of `IssueCycleWorkflow` into pure helpers so both drivers write the same `outcome.json` shape. Config selects the mode; `prbot run` dispatches before the Temporal client is dialed.

**Tech Stack:** Go 1.27, Temporal Go SDK v1.34 (workflow side unchanged), `gopkg.in/yaml.v3` strict config, existing evidence/lineage/change stores, `gh`/`opencode` child processes.

**Spec:** `https://github.com/vitdo82/konoha/issues/212` (issue text; design doc updates land in Task 8)

## Global Constraints

- Never rename or change the signature of an exported activity/workflow function (Temporal type names). Added helpers stay unexported.
- Markers (`konoha-managed`, `konoha-decision`, `konoha-parent`, `konoha-child`, …), the `VERDICT:`/`BLOCKING:`/`NITS:`/`CONCLUDE:` parser contract, and §6 state strings are byte-exact and non-configurable.
- An absent `runtime:` block preserves today's behavior exactly. `runtime.mode` accepts only `temporal` and `local`.
- Local mode never dials `TEMPORAL_ADDRESS` and never starts schedules or the webhook server.
- Local mode reuses §23 env scrubbing, §24 idempotent GitHub mutations, §40 fail-closed routing, and the §6 terminal states. Approval timeout is `BLOCKED`; deny is `HUMAN_REQUIRED`.
- `make test` (vet + build + test) and `gofmt` stay clean.
- Local mode is non-durable: no replay, crash recovery, schedules, webhooks, child fan-out, queue verbs, or post-merge workflows (documented, never reported active).

## Review Focus

- Unknown `runtime.mode` or malformed `runtime.local.*` value must fail at load, never silently fall back to `temporal`.
- A host with no Temporal (and an unreachable `TEMPORAL_ADDRESS`) must still complete `prbot run`: no dial, no fatal.
- A local §55 gate that never receives a decision must end `BLOCKED` with terminal evidence and must never proceed to a mutation.
- An LARGE issue in local mode must end at `NOT_WIRED` with no split/child activity call and no GitHub mutation.
- A local cycle that errors mid-path (and a SIGTERM during a cycle) must still tear down the worktree and leave a terminal outcome, not a silent drop.

---

### Task 1: `runtime:` config block, validation, and doctor awareness

**Files:**
- Create: `internal/konoha/runtime.go`
- Modify: `internal/konoha/appconfig.go` (`AppConfig`, `defaultAppConfig`, `validate`)
- Modify: `internal/konoha/operator.go` (`runDoctorChecks`)
- Test: `internal/konoha/runtime_test.go`
- Test: `internal/konoha/operator_test.go`
- Modify: `configs/konoha.yaml` (commented block)

**Interfaces:**
- Consumes: `AppConfig.validate`, `defaultAppConfig`.
- Produces:
  - `type RuntimeConfig struct { Mode string; Local LocalRuntimeConfig }` with yaml keys `mode`, `local`.
  - `type LocalRuntimeConfig struct { SweepInterval string; MaxCycles int; Once bool; ApprovalWait string }` with yaml keys `sweepInterval`, `maxCycles`, `once`, `approvalWait`.
  - Constants `runtimeModeTemporal = "temporal"`, `runtimeModeLocal = "local"`.
  - Methods: `(RuntimeConfig) mode() string`, `(LocalRuntimeConfig) sweepInterval() time.Duration`, `(LocalRuntimeConfig) maxCycles() int`, `(LocalRuntimeConfig) once() bool`, `(LocalRuntimeConfig) approvalWait() time.Duration`, `(RuntimeConfig) validate() error`.
  - `AppConfig.Runtime RuntimeConfig` (yaml `runtime`).

- [ ] **Step 1: Write the failing config tests**

In `runtime_test.go`, table-drive `loadAppConfig` on temp files:

```go
// unknown mode fails at load
// runtime:\n  mode: localish\n  -> err contains `runtime.mode`
// valid local block loads with sweepInterval/maxCycles/once/approvalWait resolved
// maxCycles: 0 -> err contains `maxCycles`
// sweepInterval: nope -> err contains `sweepInterval`
// approvalWait: nope -> err contains `approvalWait`
// no runtime block -> mode()=="temporal", sweepInterval()==5*time.Minute,
//   maxCycles()==1, once()==false, approvalWait()==10*time.Minute
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/konoha -run TestRuntimeConfig -v`
Expected: FAIL (`undefined: RuntimeConfig`).

- [ ] **Step 3: Implement `runtime.go` and wire validation**

`validate` rejects anything but empty/`temporal`/`local` after `strings.ToLower(strings.TrimSpace(...))`, requires a positive `sweepInterval` and `approvalWait`, and `maxCycles >= 1`. Add `Runtime: RuntimeConfig{Mode: runtimeModeTemporal, Local: LocalRuntimeConfig{SweepInterval: "5m", MaxCycles: 1, ApprovalWait: "10m"}}` to `defaultAppConfig`, and call `c.Runtime.validate()` first in `AppConfig.validate`.

Document the block in `configs/konoha.yaml` as a commented example with the closed mode set.

- [ ] **Step 4: Add the doctor test and behavior**

Test: write a konoha.yaml with `runtime: {mode: local}` and call `runDoctorChecks` with stubs; assert `findCheck(checks, "temporal")` is nil and a check named `temporal (not required: local runtime)` exists with no error. Keep the existing default/temporal behavior test passing.

In `runDoctorChecks`, after loading `cfg`: local mode appends the non-required check and skips `doctorTemporalHealth`; otherwise append the existing `"temporal"` check.

- [ ] **Step 5: Run tests**

Run: `go test ./internal/konoha -run 'TestRuntimeConfig|TestDoctor' -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/konoha/runtime.go internal/konoha/runtime_test.go internal/konoha/appconfig.go internal/konoha/operator.go internal/konoha/operator_test.go configs/konoha.yaml
git commit -m "feat: add runtime mode config and local-mode doctor awareness (#212)"
```

---

### Task 2: Make activity logging safe outside a Temporal activity context

**Files:**
- Create: `internal/konoha/logger.go`
- Modify: `internal/konoha/activities.go`, `cleanup.go`, `decisions.go`, `approval.go`, `publish.go`, `change.go`, `evidence.go`, `lineage.go` (replace `activity.GetLogger(ctx)` with `activityLogger(ctx)`)
- Test: `internal/konoha/logger_test.go`

**Interfaces:**
- Produces: `func activityLogger(ctx context.Context) log.Logger` where `log` is `go.temporal.io/sdk/log`; returns `activity.GetLogger(ctx)` when `activity.IsActivity(ctx)`, else a process fallback logger whose `Info`/`Warn`/`Error` write to stderr via the stdlib `log` package (`Debug` is a no-op).
- Consumes: nothing new.

- [ ] **Step 1: Write the failing test**

```go
func TestActivityLoggerWorksOutsideActivity(t *testing.T) {
    activityLogger(context.Background()).Info("outside", "k", "v") // must not panic
    // SubmitReview logs before its dryRun early return: proves a real activity path
    if err := SubmitReview(context.Background(), PrInput{Lifecycle: LifecycleConfig{DryRun: true}}, ReviewResult{}); err != nil {
        t.Fatalf("dryRun SubmitReview: %v", err)
    }
}
```

- [ ] **Step 2: Run it to verify it panics**

Run: `go test ./internal/konoha -run TestActivityLoggerWorksOutsideActivity -v`
Expected: FAIL with `getActivityEnv: Not an activity context`.

- [ ] **Step 3: Add `logger.go` and replace every call site**

Add the helper, then run:

```bash
grep -rl 'activity.GetLogger(ctx)' internal/konoha | xargs sed -i '' 's/activity\.GetLogger(ctx)/activityLogger(ctx)/g'
```

Keep the Temporal activity-context behavior identical (`activity.IsActivity` gate). Update the `activity.GetLogger` panic notes in comments to name the helper (do not delete the notes' intent).

- [ ] **Step 4: Run tests**

Run: `go test ./internal/konoha -run 'TestActivityLogger|TestEvidence|TestCleanup|TestApproval' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/konoha/logger.go internal/konoha/logger_test.go internal/konoha/activities.go internal/konoha/cleanup.go internal/konoha/decisions.go internal/konoha/approval.go internal/konoha/publish.go internal/konoha/change.go internal/konoha/evidence.go internal/konoha/lineage.go
git commit -m "refactor: activity logger tolerates non-Temporal contexts (#212)"
```

---

### Task 3: Extract runtime-neutral terminal assembly

**Files:**
- Modify: `internal/konoha/outcome.go` (new helpers)
- Modify: `internal/konoha/workflow.go` (`finish`, `recordTerminal`)
- Test: `internal/konoha/outcome_test.go`
- Test: existing `internal/konoha/workflow_suite_test.go` (must stay green)

**Interfaces:**
- Consumes: `stampOutcome`, `policyDigest`, `taskHash`, `stampProfiles`, `urlPolicyEvidence`, `EffectiveURLPolicy`, `OutboundSecretNames`, `egressEvidence`, `limitsEvidence`, `EffectiveLimits`, `budgetCrossings`.
- Produces:
  - `type terminalInputs struct { AgentOutcome AgentOutcome; VerificationOutcome VerificationOutcome; Usage HarnessUsage; Limits *LimitEvidence; VerificationMutated bool; EvidenceError string; Cleanup *CleanupResult; Elapsed time.Duration; Resources *ResourceEvidence }`
  - `func assembleTerminal(in IssueInput, out CycleOutcome, ti terminalInputs) CycleOutcome` — applies the defaulting/budget/limits/egress/duration/resource/cleanup logic currently inside the workflow `finish` closure.
  - `func prepareTerminalOutcome(in IssueInput, out CycleOutcome, now time.Time) CycleOutcome` — applies `stampOutcome`, `DryRun`, policy digest, scope, profiles/tooling, URL policy, secret names.

- [ ] **Step 1: Write the failing helper test**

```go
func TestAssembleTerminalDefaults(t *testing.T) {
    lc := LifecycleConfig{DryRun: true}
    in := IssueInput{Owner: "o", Repo: "r", IssueNum: 7, Lifecycle: lc}
    got := assembleTerminal(in, CycleOutcome{WorkflowID: "r-issue-7", TerminalState: stateFailed},
        terminalInputs{AgentOutcome: agentOutcomeFailed, Usage: HarnessUsage{TokensIn: 3}, Elapsed: 2 * time.Second})
    // AgentOutcome preserved, Usage copied, VerificationOutcome == SKIPPED after
    // prepareTerminalOutcome, DryRun true, PolicyDigest non-empty, DurationSeconds == 2
    got = prepareTerminalOutcome(in, got, time.Unix(0, 0))
    ...
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/konoha -run TestAssembleTerminalDefaults -v`
Expected: FAIL (`undefined: assembleTerminal`).

- [ ] **Step 3: Implement the helpers and rewire the workflow**

Move the body of `finish` (outcome defaulting, limits, egress, duration, resources/budget crossings, `Cleanup`) into `assembleTerminal`. Move the first block of `recordTerminal` (outcome stamping, DryRun, policy digest, scope, profiles, URL policy, secret names) into `prepareTerminalOutcome`. `finish` becomes: set `terminalRecorded`, compute `elapsed`, `runCleanup(ctx)`, call `assembleTerminal`, `status.recordTerminal`, `recordTerminal`. `recordTerminal` starts with `out = prepareTerminalOutcome(in, out, workflow.Now(ctx))`.

- [ ] **Step 4: Run the full workflow + outcome suites**

Run: `go test ./internal/konoha -run 'TestWorkflow|TestAssembleTerminal|TestStampOutcome' -v`
Expected: PASS with unchanged assertions (the refactor is behavior-preserving).

- [ ] **Step 5: Commit**

```bash
git add internal/konoha/outcome.go internal/konoha/outcome_test.go internal/konoha/workflow.go
git commit -m "refactor: share terminal outcome assembly across runtimes (#212)"
```

---

### Task 4: Local sequential SMALL path — happy path

**Files:**
- Create: `internal/konoha/local.go`
- Test: `internal/konoha/local_test.go`

**Interfaces:**
- Consumes: every existing activity function listed below, plus `resolveResources`, `effectiveDevelopmentMax`, `EnsureManagedNote`, `ProjectLabels`, `prBody`, `AIDescriptionEnabled`, `writeAgentEvidence`, `evidenceResultJSON`, `evidenceOf`, `activationReasonInitial`, `stageDeveloper`, `effectiveHarnessName`, `assembleTerminal`, `prepareTerminalOutcome`, `evidenceJSON`, `logCycleEvent`.
- Produces:
  - `type localActivities struct` with one func field per activity (listed in Step 3).
  - `func realLocalActivities() localActivities`.
  - `var localActivitiesFor = realLocalActivities` (test seam, mirrors `ghRunner`).
  - `type localRuntime struct { cfg RuntimeConfig; acts localActivities; now func() time.Time; logf func(string, ...any) }`.
  - `func newLocalRuntime(cfg AppConfig, acts localActivities) *localRuntime`.
  - `func localCycleID(in IssueInput) string` → `fmt.Sprintf("%s-issue-%d", in.Repo, in.IssueNum)`.
  - `func (r *localRuntime) runCycle(ctx context.Context, in IssueInput) error`.
  - `func (r *localRuntime) recordTerminal(ctx context.Context, in IssueInput, cycleID string, out CycleOutcome) error`.
  - `func (r *localRuntime) call(ctx context.Context, c retryClass, op func() error) error` — applies `retryPolicyFor(c)` in-process (attempt count, `InitialInterval`, `BackoffCoefficient`, `MaximumInterval`); every activity invocation goes through it.

- [ ] **Step 1: Write the failing happy-path test**

Use `localActivitiesFor` to inject fakes (no PATH/GitHub). Assert:
- terminal state `CLOSED`, `AutonomousClose` true, `Attempts == 1`, `ReviewRounds == 1`
- one developer call, one publish call, one reviewer call, one merge call, one close call
- the evidence fake received an `EvidenceRequest{Outcome: ...}` whose JSON decodes to a `CycleOutcome` with `TerminalState == stateClosed`, non-empty `PolicyDigest`, `Cleanup.Outcome == cleanupAbsent`
- the cleanup fake was called exactly once and after the merge

```go
func TestLocalRuntimeSmallHappyPath(t *testing.T) {
    appCfg.Evidence.Dir = t.TempDir()
    // ... fakes: analysis SMALL complexity 3, dev Validated/CommitSHA "devsha",
    // publish PR 101, head "headsha", reviewer APPROVE, CI green, merge merged,
    // cleanup absent, evidence capture slice
    err := rt.runCycle(context.Background(), IssueInput{Owner: "o", Repo: "r", IssueNum: 7, RepoPath: t.TempDir(),
        Lifecycle: LifecycleConfig{Mode: modeSupervisedAutonomous, DevelopmentMaxComplexity: 4}})
    ...
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/konoha -run TestLocalRuntimeSmallHappyPath -v`
Expected: FAIL (`undefined: localRuntime`).

- [ ] **Step 3: Implement `local.go` happy path**

`localActivities` fields (all unexported, exact parameter/return types):

```go
analyze           func(context.Context, IssueInput) (IssueAnalysis, error)                  // AnalyzeIssue
estimate          func(context.Context, IssueInput, IssueAnalysis) (EstimateResult, error)  // EstimateIssue
ensureManagedNote func(context.Context, ManagedNoteRequest) error                           // EnsureManagedNote
applyLabels       func(context.Context, LabelsRequest) error                                // ApplyIssueLabels
fetchSnapshot     func(context.Context, IssueInput) (IssueSnapshot, error)                  // FetchIssueSnapshot
fetchLabels       func(context.Context, IssueInput) ([]string, error)                       // FetchIssueLabels
developer         func(context.Context, DevelopRequest) (DevelopResult, error)              // RunDeveloper
repair            func(context.Context, RepairRequest) (DevelopResult, error)               // RunRepair
publish           func(context.Context, PublishInput) (PublishResult, error)                // PublishDeveloperChange
prHead            func(context.Context, PrInput) (string, error)                            // FetchPRHead
prDiff            func(context.Context, PrInput) (string, error)                            // FetchPRDiff
reviewer          func(context.Context, ReviewRequest) (ReviewResult, error)                // RunReviewer
prDescription     func(context.Context, PRDescriptionRequest) (PRDescriptionResult, error)  // RunPRDescription
waitCI            func(context.Context, CIRequest) (CIStatus, error)                        // WaitForCI
merge             func(context.Context, MergeRequest) (MergeResult, error)                  // MergePullRequest
ensureIssueState  func(context.Context, StateRequest) error                                 // EnsureIssueState
awaitApproval     func(context.Context, ApprovalRequest) (ApprovalResult, error)            // AwaitApproval
recordAudit       func(context.Context, AuditRequest) error                                 // RecordAuditEvent
cleanup           func(context.Context, CleanupRequest) (CleanupResult, error)              // CleanupWorktree
writeEvidence     func(context.Context, EvidenceRequest) error                              // WriteCycleEvidence
recordActivation  func(context.Context, ActivationRequest) error                            // RecordActivation
recordChange      func(context.Context, ChangeRequest) error                                // RecordChange
recordMetrics     func(context.Context, CycleOutcome) error                                 // RecordCycleMetrics
```

`runCycle` order (mirror `IssueCycleWorkflow` lines 146–1008): resolve resources → `AnalyzeIssue` → `EstimateIssue` → route (`est.Complexity >= 1 && <= effectiveDevelopmentMax`) → LARGE returns `NOT_WIRED` (Task 5) → ApplyIssueLabels(SMALL) → `EnsureManagedNote` (model = developer stage model) → `RunDeveloper` with `WorkflowID: cycleID` → track outcomes/limits/usage/lineage/evidence → risk/claim/publish/review/CI/merge/close per Tasks 4–5 → `recordTerminal`. Teardown is a `defer` calling the cleanup activity once with `CleanupRequest{RepoPath, Repo, IssueNum, WorkflowID: cycleID}`; a cleanup error records `CleanupResult{Attempted: true, Outcome: cleanupFailed, Error: Redact(err.Error())}` and never changes the state.

Every best-effort write (evidence, lineage, change, labels, managed note) logs via `r.logf` and never aborts the cycle, exactly like the workflow. Every activity invocation goes through `r.call` with its retry class, so an infrastructure/verification failure retries like Temporal and an agent call never replays.

- [ ] **Step 4: Run the happy-path test**

Run: `go test ./internal/konoha -run TestLocalRuntimeSmallHappyPath -v`
Expected: PASS.

- [ ] **Step 5: Add the retry-class tests and helper**

```go
// TestLocalCallRetriesInfrastructure: op fails twice then succeeds;
//   appCfg.Worker.RetryMaxAttempts=3; assert 3 calls, nil error.
// TestLocalCallAgentNeverRetries: op always fails in a retryAgent call;
//   assert exactly 1 call and the error propagates.
// TestLocalCallDeterministicNeverRetries: same for retryDeterministic.
// TestLocalCallVerificationThreeAttempts: retryVerification fails always;
//   assert exactly 3 calls.
```

`call` uses `retryPolicyFor(c).MaximumAttempts`, and sleeps `InitialInterval * BackoffCoefficient^(attempt-1)` capped at `MaximumInterval` (abort when `ctx.Done()`). Map each activity to its class exactly as `IssueCycleWorkflow` does: analyze/estimate/developer/repair/reviewer/prDescription = `retryAgent`; publish/labels/managed note/claims/head/diff/merge/issue-state/evidence/lineage/change = `retryInfrastructure`; awaitApproval = `retryInfrastructure`; waitCI = `retryVerification`; cleanup/metrics/audit = `retryDeterministic`.

Run: `go test ./internal/konoha -run 'TestLocalCall|TestLocalRuntimeSmallHappyPath' -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/konoha/local.go internal/konoha/local_test.go
git commit -m "feat: local sequential SMALL-path runner (#212)"
```

---

### Task 5: Local gates, repair loop, LARGE terminal, and conformance

**Files:**
- Modify: `internal/konoha/local.go`
- Test: `internal/konoha/local_test.go`
- Test: `internal/konoha/local_conformance_test.go`

**Interfaces:**
- Consumes: `EvaluateRepairLoop`, `DetectHumanEscalation`, `releasedClaim`, `EvaluateApproval`-derived `ApprovalResult`, `ReviewAppliesTo`, `reworkBlocking`, `reworkInstruction`, `reviewFeedback`, `failedChildOutcome`, `ValidationGates`, `ValidationCommand`, `RequireCI`, `MergeStrategy`, `CIMaxWait`, `reason*` constants.
- Produces: `func (r *localRuntime) gateStep(step string) (proceed bool, rework *ApprovalResult, err error)`; no exported API change.

- [ ] **Step 1: Write the failing policy-matrix tests**

One test each, driving `runCycle` with fakes and asserting the terminal state + the decisive side effect:

| Test | Setup | Expect |
|---|---|---|
| `TestLocalRouteLargeNotWired` | estimate complexity 5 (ceiling 4) | `NOT_WIRED`, zero split/child/developer/publish calls |
| `TestLocalRepairLoopCapBlocks` | reviewer always `REQUEST_CHANGES`, `MaxDevReviewAttempts: 2` | 1 repair, then `BLOCKED`, never closes |
| `TestLocalRiskGateEscalates` | dev changed `infra/x`, `humanRequiredPaths: ["infra/**"]` | `HUMAN_REQUIRED`, `reasonPolicyViolation`, zero publish |
| `TestLocalApprovalTimeoutBlocks` | `approval.preGate` with publish step, `awaitApproval` returns `TimedOut` | `BLOCKED`, zero publish, outcome written |
| `TestLocalApprovalDenyHumanRequired` | `awaitApproval` returns `Denied` | `HUMAN_REQUIRED`, zero publish |
| `TestLocalApprovalReworkRepairs` | publish gate returns `Rework` once (feedback "tighten"), then Granted | 2 developer-ish attempts (dev + repair), `CLOSED` |
| `TestLocalCIUnhealthyEscalates` | `waitCI` returns `CIStatus{Green:false}` | `HUMAN_REQUIRED`, `reasonCIFailed`, zero merge/close |
| `TestLocalMergeFailureEscalates` | `merge` returns error | `HUMAN_REQUIRED`, `reasonMergeFailed`, zero close |
| `TestLocalClaimReleasedCancels` | `fetchSnapshot` returns closed issue | `CANCELLED`, `reasonClaimReleased`, zero publish |

Each test must also assert `recordMetrics` saw the same terminal state and the evidence fake saw the outcome.

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/konoha -run TestLocal -v`
Expected: FAIL on the unimplemented branches.

- [ ] **Step 3: Implement the gates**

Mirror the workflow:
- LARGE → finish `CycleOutcome{TerminalState: stateNotWired, Route: stateLarge, ...}` with no activity call beyond analyze/estimate.
- claim checkpoint before publish, after a repair, and before merge (`releasedClaim`, skipped under `DryRun`).
- risk gate after `FetchIssueLabels`: `reasonPolicyViolation`.
- `gateStep(step)`: `!ApprovalRequired` → proceed; `DryRun` → proceed; else `awaitApproval` with `Timeout: r.cfg.Local.approvalWait().String()`; record audit with `auditActor`/`auditDecision`; `Granted||Skipped` proceed; `Rework` returned to caller; `Denied` → `HUMAN_REQUIRED`/`reasonApprovalDenied`; timeout → `BLOCKED`.
- publish-gate rework: `EvaluateRepairLoop`, `RunRepair` with `PreviousCommit: dev.CommitSHA`, changed feedback, then re-gate; cap → `BLOCKED`.
- review loop: head/diff/review; `REQUEST_CHANGES` → `EvaluateRepairLoop`; at cap → `BLOCKED`; else `RunRepair` with `PreviousCommit: reviewedSHA`, re-publish, loop.
- approved → `ReviewAppliesTo(approved, reviewedSHA)`; false → `BLOCKED`.
- CI → `reasonCIFailed` on error/not green; merge gate (rework → `HUMAN_REQUIRED`/`reasonApprovalDenied`); merge error/not merged → `reasonMergeFailed`; only then `EnsureIssueState(closed)` + `CLOSED`.
- a blocked developer/repair → `HUMAN_REQUIRED` with `reasonAgentBlocked`, `AgentOutcome: BLOCKED`, `VerificationOutcome: SKIPPED`.

- [ ] **Step 4: Run the policy matrix**

Run: `go test ./internal/konoha -run TestLocal -v`
Expected: PASS.

- [ ] **Step 5: Write the conformance test**

`local_conformance_test.go` runs the same fixture twice:
1. `IssueCycleWorkflow` through `testsuite` with the deterministic activity mocks (`mockSmallPath`), capturing the `EvidenceRequest.Outcome` passed to a mocked `WriteCycleEvidence`.
2. `localRuntime.runCycle` with fake activities returning the same values, capturing the same evidence request.

Then decode both `CycleOutcome` JSON values and assert equality of the spec-listed fields: `TerminalState`, `AgentOutcome`, `VerificationOutcome`, `PolicyDigest`, `Profiles`, `ProfileDigest`, `Tooling`, `Limits`, `Egress`, `URLPolicy`, `SecretNames`, `Cleanup`, `Route`, `Complexity`, `Confidence`, `Attempts`, `ReviewRounds`, and the ordered `Conditions` types/statuses/reasons (ignore `At` and `DurationSeconds`).

- [ ] **Step 6: Run the conformance test and full suite**

Run: `go test ./internal/konoha -run 'TestLocalConformance|TestLocal|TestWorkflow' -v`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/konoha/local.go internal/konoha/local_test.go internal/konoha/local_conformance_test.go
git commit -m "feat: local runtime gates, repair loop, and conformance (#212)"
```

---

### Task 6: Headless `prbot run` loop, signals, and no-dial guarantee

**Files:**
- Modify: `internal/konoha/local.go`
- Modify: `internal/konoha/cli.go` (`Main` dispatch)
- Test: `internal/konoha/local_run_test.go`

**Interfaces:**
- Consumes: `loadAppConfig`, `loadReposConfigWithResources`, `repoScope`, `registerOperatorSecrets`, `StartMetricsServer`, `sweepOrphanWorktreesOnStart`, `workflowAlive`, `ListUnmanagedIssues`/`failedChildOutcome`.
- Produces:
  - `func runLocal(args []string) error` (CLI; loads config itself, like `runConsole`).
  - `func parseLocalArgs(args []string) (localOptions, error)` with `type localOptions struct { Once bool; Repo string; Issue int }`.
  - `func (r *localRuntime) run(ctx context.Context, repos []RepoConfig, opts localOptions) error`.
  - `func (r *localRuntime) runSweep(ctx context.Context, repos []RepoConfig, opts localOptions) error`.
  - `func localModeWarning() string` (one line naming schedules, webhook fast-path, child fan-out, post-merge workflows, queue retry/cancel).

- [ ] **Step 1: Write the failing tests**

```go
// TestLocalRunDoesNotDialTemporal: control listener on 127.0.0.1:0 counts
//   accepts; TEMPORAL_ADDRESS points at it; localActivitiesFor returns fakes for
//   a --once --repo r --issue 7 run; assert runLocal returns nil and accepts == 0.
// TestLocalRunOncePerformsOneSweep: listUnmanagedIssues returns issues 1 and 2,
//   fake activity counts analyze calls; run once with maxCycles 1;
//   assert exactly 1 analyze call and nil error.
// TestLocalRunSigtermFinishesInflightCycle: a fake analyze blocks until released;
//   start runLocal in a goroutine; wait for a "started" signal; send
//   syscall.Kill(os.Getpid(), syscall.SIGTERM); release the cycle; assert the
//   cycle completed (CLOSED) and runLocal returned nil.
// TestLocalModeWarningNamesDisabledFeatures: assert the string contains
//   "schedules", "webhook", "child", "post-merge", "queue".
// TestParseLocalArgs: --once/--repo r/--issue 7; unknown flag and --issue
//   without --repo are errors.
```

Tests set `KONOHA_CONFIG_DIR` to a temp dir with `konoha.yaml` (`runtime.mode: local`, local `once`/`maxCycles`, absolute temp evidence dir) and `repos.yaml` (repo `r`, `repoPath` temp).

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/konoha -run 'TestLocalRun|TestParseLocalArgs|TestLocalModeWarning' -v`
Expected: FAIL (`undefined: runLocal`).

- [ ] **Step 3: Implement the entrypoint and loop**

`runLocal`:
1. `loadAppConfig(defaultAppConfigPath())` → set `appCfg`; `registerOperatorSecrets()`.
2. `parseLocalArgs`; require `appCfg.Runtime.mode() == runtimeModeLocal` (otherwise `runtime.mode is "temporal"; start the worker instead of prbot run`).
3. `loadReposConfigWithResources`; select `--repo` (unknown name fails).
4. `log.Printf("%s", localModeWarning())`.
5. `StartMetricsServer(os.Getenv("METRICS_ADDR"))`.
6. Set `workflowAlive = func(context.Context, string) bool { return false }` (no Temporal workflow owns a local worktree) and run `sweepOrphanWorktreesOnStart()` once.
7. `ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)`; `defer stop()`; `return newLocalRuntime(appCfg, localActivitiesFor()).run(ctx, repos, opts)`.

`run`:
- If `opts.Issue > 0`: one cycle for that issue; a returned error is recorded as a failed terminal (`failedChildOutcome` + blocked label projection) and then returned (non-zero exit).
- Else loop: `runSweep` (for each repo: `listUnmanagedIssues`, cap at `maxCycles`, run each sequentially; a cycle error records the failed terminal and continues); exit after one sweep when `opts.Once || r.cfg.Local.once()` is true; otherwise `select { case <-ctx.Done(): return nil; case <-time.After(r.cfg.Local.sweepInterval()): }`.
- The cycle itself runs with `context.Background()` so a signal never kills an in-flight agent; the signal stops the loop after the cycle (spec: finish the in-flight cycle).
- No webhook server, console, MCP, schedules, or Temporal client is started.

`Main` adds, in the pre-bootstrap switch (before `loadAppConfig`/`client.Dial`):

```go
case "run":
    if err := runLocal(os.Args[2:]); err != nil {
        log.Fatal(err)
    }
    return
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/konoha -run 'TestLocalRun|TestParseLocalArgs|TestLocalModeWarning' -v`
Expected: PASS, including `accepts == 0`.

- [ ] **Step 5: Commit**

```bash
git add internal/konoha/local.go internal/konoha/local_run_test.go internal/konoha/cli.go
git commit -m "feat: headless prbot run local runtime loop (#212)"
```

---

### Task 7: Hermetic e2e scenario with no Temporal dev-server

**Files:**
- Create: `e2e/local/local_test.go` (`//go:build e2e`)
- Modify: `e2e/fakeopencode/main.go` (write the child env once to `$FAKE_OPENCODE_DIR/agent.env`)
- Test: `e2e/local/local_test.go`

**Interfaces:**
- Consumes: `e2e/fakegh`, `e2e/fakeopencode`, `KONOHA_TOKEN_FIXED`, `KONOHA_GIT_BASE`, `FAKE_GH_STATE`, `FAKE_OPENCODE_DIR`, `KONOHA_CONFIG_DIR`.
- Produces: `TestMain` for package `local` that starts no Temporal server; `TestScenarioLocalSmallHappyPath`.

- [ ] **Step 1: Write the failing local e2e test**

`TestMain`:
1. Build `gh`/`opencode` fakes into a temp bin dir (`go build -tags=e2e ./e2e/fakegh`, `./e2e/fakeopencode`).
2. Create a fixture git repo + bare remote (`KONOHA_GIT_BASE` layout `<base>/o/r.git`).
3. Write a temp `KONOHA_CONFIG_DIR` with `konoha.yaml` (`evidence.dir` absolute temp, `runtime: {mode: local}`, `runtime.local: {once: true, maxCycles: 1, approvalWait: 1s}`) and `repos.yaml` (repo `r`, `repoPath` fixture, `developmentMaxComplexity: 4`, `autonomy.closePolicy: risk-based`).
4. Seed fake state: issue 1 open; agent fixtures for analyst/estimator/developer/reviewer.
5. Run `go run ./cmd/konoha run --once --repo r --issue 1` with `TEMPORAL_ADDRESS=127.0.0.1:1` (proves no dial), fakes on PATH, `KONOHA_TOKEN_FIXED=e2e-token`, `KONOHA_GIT_BASE`, `FAKE_*`.

The test asserts:
- exit code 0
- issue 1 `closed` and exactly one PR `merged` in the fake store
- `evidence.dir/cycles/r-issue-1/outcome.json` decodes with `terminalState == "CLOSED"` and non-empty `agentOutcome`/`verificationOutcome`/`policyDigest`
- `$FAKE_OPENCODE_DIR/agent.env` contains no `GH_`, `GITHUB_TOKEN`, or `GH_TOKEN` line (§23)
- no Temporal dev-server was started (the test package never calls `testsuite.StartDevServer`)

- [ ] **Step 2: Run it to verify it fails**

Run: `go test -tags=e2e ./e2e/local -run TestScenarioLocal -v`
Expected: FAIL (no `run` command / no env dump).

- [ ] **Step 3: Add the env dump to the fake and run the scenario**

In `e2e/fakeopencode/main.go`, on startup write `os.Environ()` joined by newlines to `filepath.Join(os.Getenv("FAKE_OPENCODE_DIR"), "agent.env")` (best-effort; ignore error). Keep the existing role-based output unchanged.

- [ ] **Step 4: Run the scenario**

Run: `go test -tags=e2e ./e2e/local -run TestScenarioLocal -v`
Expected: PASS.

- [ ] **Step 5: Confirm the Temporal e2e suite still passes**

Run: `go test -tags=e2e ./e2e/...`
Expected: PASS (the fake env dump is additive).

- [ ] **Step 6: Commit**

```bash
git add e2e/local/local_test.go e2e/fakeopencode/main.go
git commit -m "test: hermetic local-runtime e2e without Temporal (#212)"
```

---

### Task 8: Documentation

**Files:**
- Modify: `docs/new-design-v2.md` (§2, §44 #1/#20, §45, §57, new §59)
- Create: `docs/runtime.md`
- Modify: `README.md`
- Modify: `AGENTS.md`
- Modify: `docs/qa-environment.md`

**Interfaces:** none (documentation only).

- [ ] **Step 1: Update the design record**

- §2: replace “Temporal is the durable conductor.” with “The active runtime is the conductor. Temporal is the default durable implementation; local headless mode is the non-durable alternative.” Update the header tagline (“Temporal enforces policy and coordination” → “the active runtime enforces policy and coordination”) and add a `runtime (Temporal | local)` box to the diagram; keep policy authority deterministic.
- §44: #1 → “The active runtime owns lifecycle state; Temporal is the default durable implementation.” #20 → “Labels are projections; the active runtime is authoritative.” Keep the numbering/count at 20.
- §45: add one runtime-neutral sentence (the lifecycle is not defined as Temporal-only).
- §57: replace “rather than from Temporal history” with “rather than from runtime history”.
- State-authority statements outside those sections (AC: none may claim Temporal is mandatory): reword line-level “Temporal workflow history is the authoritative record.”, “Temporal remains authoritative.”, “keeps its authoritative Temporal state”, and “history remains the authoritative lifecycle record” to name the active runtime.
- New §59 `Optional runtime: local headless mode`: status implemented by #212; one active runtime per cycle; parity guarantees (activities, pure policy, retry classes, evidence artifacts, §6 states, idempotent mutations, §23 scrubbing); non-guarantees (durability, replay, schedules, webhooks, child fan-out, queue verbs, post-merge workflows); the mode matrix from the issue; fail-closed rules; revisit trigger (adopt the full engine seam if crash recovery/schedules are needed).

Run: `grep -n "Temporal is the durable conductor\|Temporal is authoritative\|rather than from Temporal history\|Temporal workflow history is the authoritative\|Temporal remains authoritative\|authoritative Temporal state" docs/new-design-v2.md`
Expected: no matches.

- [ ] **Step 2: Write `docs/runtime.md`**

Sections: runtime modes; mode matrix (same table as §59); `prbot run` usage and flags; systemd unit and launchd plist examples; rollback to Temporal (`runtime.mode: temporal`, run the worker, schedules via `./scripts/apply.sh`); local-mode limits (no durability/replay/schedules/webhooks/children/queue/post-merge); evidence reads (`prbot describe/logs/export`) work offline.

- [ ] **Step 3: Update README, AGENTS.md, and qa docs**

- `README.md`: add “Runtime modes” (Temporal default, `runtime.mode: local`, `prbot run [--once] [--repo r] [--issue n]`, local-mode limits, pointer to `docs/runtime.md`).
- `AGENTS.md`: add an architecture bullet for `runtime.go`/`local.go` — config `runtime.mode`, in-process activities with the retry classes, shared terminal assembly, no Temporal type-name changes, local-mode non-guarantees.
- `docs/qa-environment.md`: add the no-Temporal test path (`go test ./internal/konoha`, `go test -tags=e2e ./e2e/local`), noting the compose stack remains the Temporal path.

- [ ] **Step 4: Verify the docs statements and full suite**

Run: `make test`
Expected: `go vet`, `go build`, and every test pass.

Run: `gofmt -l internal/konoha cmd e2e | grep -v '^$'`
Expected: no output.

- [ ] **Step 5: Commit**

```bash
git add docs/new-design-v2.md docs/runtime.md docs/qa-environment.md README.md AGENTS.md
git commit -m "docs: document optional local runtime mode (#212)"
```

---

## Final verification

- [ ] `make test` green.
- [ ] `go test -tags=e2e ./e2e/...` green (Temporal scenarios + local scenario).
- [ ] `gofmt -l internal/konoha cmd e2e` prints nothing.
- [ ] `git diff --stat` shows no renamed exported activity/workflow function (review `git diff | grep '^[-+]func '`).
- [ ] `prbot run --once --repo <name> --issue <n>` with `TEMPORAL_ADDRESS=127.0.0.1:1` completes and produces `outcome.json`.
