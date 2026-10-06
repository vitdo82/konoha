# Console security threat model (decision record)

Status: **accepted.** This is the reference for the local review console
(`console.go`, `decisions.go`, `console_view.go`, `console_artifact.go`,
`consoleassets/`, tickets [#105]/[#106]/[#107]/[#108]/[#109]/[#110]) and the
ticket [#111] deliverable. It states the control-plane boundary explicitly,
maps each control to the test that enforces it, and records the residual risk.

It is **documentation plus doc-enforced assertions**: no production behavior
changed. The console is a **loopback-only, single-operator local view**. It is
not a hosted control plane — see [`docs/non-goals.md`](non-goals.md) → A2.

The threat surface is new: an HTTP listener is a network-reachable process, and
the console reads untrusted agent artifacts. Konoha's own model (Machinist)
draws this boundary explicitly; this document does the same.

## Trusted vs untrusted

| Trusted (operator-controlled) | Untrusted (never authoritative) |
|---|---|
| The loopback host + the operator's browser / CLI | Agent stdout/stderr, `result.json`, `diff.patch` |
| `KONOHA_CONSOLE_TOKEN` (bearer), the per-process CSRF token | Issue text and comments rendered in the board/detail |
| `configs/konoha.yaml`, `configs/repos.yaml` (policy only) | Artifact file **names and bytes** (uploaded by the agent) |
| Temporal status of a running cycle | Any HTTP `Origin` header |

Untrusted content is **data, never instructions**: no artifact is executed, no
artifact HTML is rendered, and issue text is inserted as text nodes.

## Controls

Each control below names the enforcing test. Where a control is a manual
review discipline rather than a runtime guarantee, it is marked a **review
checklist** item.

### 1. Bind boundary — loopback only (default)

- **Control.** The console starts only when `KONOHA_CONSOLE_ADDR` is set
  (`consoleConfigFromEnv`, `ConsoleConfig.enabled`); the default is **disabled**
  (no listener). A configured address is validated by `validateLoopbackAddr`
  (`console.go:95`): the host must be `localhost` or a loopback IP
  (`isLoopbackHost`); an empty host (all interfaces) and any routable IP/host are
  rejected. The check runs at `newConsoleServer` (`console.go:266`), at
  `StartConsoleServer` (`console.go:925`), and again at the `prbot console`
  entrypoint `runConsole` (`console.go:1026`) — before any listener binds.
- **Non-loopback policy.** Konoha **explicitly forbids** a non-loopback bind
  (the stronger branch permitted by [#111] AC4). The "permit a non-loopback bind
  once TLS + an explicit allowed-origins list are configured" branch is **not
  implemented** and is not a planned feature: a remote control plane is an A2
  non-goal. Stating it as an unconditional refusal keeps the guarantee honest
  rather than aspirational (§40, never a silent over-claim). If a future ticket
  ever permits non-loopback, it must add TLS + `KONOHA_CONSOLE_ALLOWED_ORIGINS`
  and re-open this record.
- **Tests.** `TestConsoleAddrLoopbackOnly` (rejects `0.0.0.0`, `:8080`,
  routable IPs, `example.com`, missing port; accepts `127.0.0.1`,
  `[::1]`, `localhost`); `TestConsoleDisabledAndFailClosed`;
  `TestStartConsoleServerEnv` (non-loopback env fails before listening).

### 2. Origin allowlist (CSRF origin binding)

- **Control.** Mutating routes (`POST …/decisions`, `POST …/actions`) sit behind
  `requireMutation` (`console.go:544`), which rejects with **403** unless the
  request is a CLI/worker bearer token, or a browser request carrying the CSRF
  token **and** a loopback `Origin` (`originAllowed`, `console.go:582`). A
  missing `Origin` is allowed (non-browser client). The allowlist is therefore
  the loopback origin set; any other `Origin` is denied. `GET`/`HEAD`/`OPTIONS`
  are never mutations.
- **Tests.** `TestConsoleForeignOriginForbidden` (HTTP-level: foreign `Origin` +
  valid CSRF → 403; loopback `Origin` → not 403; bearer ignores `Origin`);
  `TestConsoleCSRFPlumbing` (the `mutationAllowed` matrix);
  `TestLoopbackOriginRule`.

### 3. Auth — bearer + CSRF, constant-time

- **Control.** Read routes require `authorized` (`console.go:556`): a valid
  bearer token (`KONOHA_CONSOLE_TOKEN`, API/CLI/worker) **or** the per-process
  CSRF token. The CSRF token is issued by the unauthenticated bootstrap
  `GET /api/v1/status` (`handleStatus`) and is independent of the bearer token.
  A bearer token is generated (32 bytes, `crypto/rand`) and logged when
  `KONOHA_CONSOLE_TOKEN` is unset, so the console is never unauthenticated.
  Every credential comparison is `constantTimeEqual` → `crypto/subtle`
  (`console.go:125`).
- **Tests.** `TestConsoleBearerAuth` (401 + `WWW-Authenticate` challenge on
  missing/wrong token); `TestConsoleCSRFPlumbing` (CSRF issued, independent of
  the bearer, unlocks reads); `TestConsoleGeneratesTokenWhenUnset`.

### 4. Untrusted content — never executed

- **Control.** Artifacts (agent stdout/stderr, `result.json`, `diff.patch`,
  anything the agent wrote) are **untrusted bytes**. The API classifies each
  artifact and previews it as **`text/plain`** (HTML included), or streams it as
  an attachment — it is never served with an executable content type and never
  rendered (`console_artifact.go` `previewContentType`; the SPA inserts all
  dynamic values via `textContent`, never `innerHTML`). Every response carries
  the strict baseline CSP (`consoleCSP`, `console.go:66`): `default-src 'none'`,
  `script-src 'self'`, `connect-src 'self'`, `frame-ancestors 'none'`,
  `form-action 'none'`, plus `X-Content-Type-Options: nosniff` and
  `X-Frame-Options: DENY` (`ServeHTTP`, `console.go:351`). The SPA index has no
  inline `<script>`.
- **Tests.** `TestArtifactHTMLPreviewedAsText`; `TestArtifactDownload`;
  `TestArtifactPreviewLimitsAndTraversal`; `TestValidArtifactPath`;
  `TestArtifactListView`; `TestConsoleSecurityHeadersAndSPA`.
- **Review checklist.** `consoleassets/app.js` must keep using `textContent`/
  `createElement` and never `innerHTML`/`insertAdjacentHTML`. (No runtime test
  asserts this today; it is a review-checklist control.)

### 5. Credentials — the console holds no GitHub App credential

- **Control.** The console process reads **only** `KONOHA_CONSOLE_*`
  (`consoleConfigFromEnv`); it never reads `GH_*`/`GITHUB_TOKEN` and never
  proxies a GitHub mutation itself. A #108 decision is written to the existing
  §18 authoritative channel (`SubmitHumanDecision`, the `konoha-decision`
  marker) by the decision writer, reusing the operator's `gh` auth — the
  read-only paths make no outbound calls beyond the optional Temporal status
  query. `GH_*` is stripped for every agent/validation child by the §23 env
  contract.
- **Tests.** `TestConsoleReadsNoGitHubCredential` (asserts the env reads and
  greps `console.go` for `GH_*`); `TestAgentEnvHasNoGitHubCredential`.

### 6. Local data — evidence store stays user-readable only

- **Control.** The evidence store (`evidence.dir`) is the console's data
  source; it may contain prompts, agent output, diffs and repository data.
  Writes are redacted (registered secrets + credential-shaped patterns →
  `[REDACTED]`), atomic (temp+rename), and `0600` — readable only by the
  operator's user. The console adds no broader filesystem access.
- **Tests.** `TestWriteCycleEvidenceFullArtifactSet` (asserts `0600`
  owner-only perms); `TestEvidenceRedactsRegisteredSecret`;
  `TestEvidenceRejectsTraversal`.

### 7. Audit — every gated decision is recorded

- **Control.** A #108 decision (`approve`/`request-changes`/`deny`) is audited
  through the §55 audit sink (`RecordAuditEvent`; action mapping in
  `decisions.go` `auditDecisionForAction`) when the repo enables
  `lifecycle.audit`. A pre-gate without an enabled audit fails at config load,
  so a gated decision cannot be taken un-audited. A write failure is
  fail-closed.
- **Tests.** `TestRecordAuditEvent`; `TestApprovalAuditConfig`.

## Residual risk (accepted, not eliminated)

- **No execution isolation.** The console shares the operator host and
  filesystem with the worker; loopback-only + auth is a **control-plane**
  boundary, not a sandbox. A local process running as the operator could reach
  the listener or read the evidence files directly. This is the same accepted
  posture as the rest of Konoha — see
  [`docs/security.md`](security.md) → "Host-execution trade-off" and
  [`docs/non-goals.md`](non-goals.md) → A1/#92.
- **Local data is not encrypted at rest.** The evidence store is protected by
  OS file permissions (`0600`), nothing more. Protect the host account.
- **The CSRF/Origin check trusts the browser's `Origin`.** A non-browser client
  that presents the bearer token bypasses it by design (CLI/worker use). The
  bearer token is the credential to protect.
- **No `Host` header check.** A DNS-rebinding page that resolves its own
  hostname to `127.0.0.1` could reach the read routes and read the CSRF token
  from `GET /api/v1/status`; the **mutation** routes stay protected because a
  rebound page's `Origin` is non-loopback → 403. A future ticket may add a
  `Host` allowlist; today this is a documented residual risk, not a claimed
  control.
- **Single-worker model.** Idempotency keys (e.g. #109 queue actions) are
  process-local; a hosted multi-worker deployment would need a shared store.

## Revisit triggers (any one re-opens this record)

- A non-loopback bind is ever proposed (requires TLS + allowed-origins and a
  credential model for remote operators).
- The console gains a **new mutation** or a **new data source** outside the
  evidence store / §18 channel.
- Konoha moves to a hosted/multi-operator worker (see `docs/non-goals.md` A2,
  #92 isolation).

## Tests

- `TestConsoleAddrLoopbackOnly`, `TestConsoleDisabledAndFailClosed`,
  `TestStartConsoleServerEnv` (`console_test.go`) — bind boundary.
- `TestConsoleForeignOriginForbidden`, `TestConsoleCSRFPlumbing`,
  `TestLoopbackOriginRule` (`decisions_test.go`, `console_test.go`) — Origin
  binding / 403.
- `TestConsoleBearerAuth`, `TestConsoleGeneratesTokenWhenUnset` — auth.
- `TestArtifactHTMLPreviewedAsText`, `TestArtifactDownload`,
  `TestArtifactPreviewLimitsAndTraversal`, `TestConsoleSecurityHeadersAndSPA` —
  untrusted content.
- `TestConsoleReadsNoGitHubCredential`, `TestAgentEnvHasNoGitHubCredential` —
  credentials.
- `TestWriteCycleEvidenceFullArtifactSet`, `TestEvidenceRedactsRegisteredSecret`
  — local data.
- `TestRecordAuditEvent`, `TestApprovalAuditConfig` — audit.

[#105]: https://github.com/vitdo82/konoha/issues/105
[#106]: https://github.com/vitdo82/konoha/issues/106
[#107]: https://github.com/vitdo82/konoha/issues/107
[#108]: https://github.com/vitdo82/konoha/issues/108
[#109]: https://github.com/vitdo82/konoha/issues/109
[#110]: https://github.com/vitdo82/konoha/issues/110
[#111]: https://github.com/vitdo82/konoha/issues/111
