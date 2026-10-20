# Runtime modes

Konoha has two runtime drivers over the same activities, policy, and evidence.
The active runtime owns lifecycle state (§44 #1); Temporal is the default
durable implementation.

| | Temporal (default) | Local headless |
|---|---|---|
| Config | `runtime.mode: temporal` (or absent) | `runtime.mode: local` |
| Entrypoint | worker (`prbot` / `make run`) | `prbot run` |
| Server required | Temporal (+ its datastore) | none |
| Durability / replay | yes | no |
| Schedules (deps/grooming/alerts) | yes, via `./scripts/apply.sh` | not started |
| Webhook fast-path | optional | not started |
| LARGE (split/children) | supported | ends `NOT_WIRED` |
| Post-merge release/deploy/propagation | supported | not run |
| Queue retry/cancel | supported | unavailable |
| Crash recovery | Temporal replay | none; restart starts a new cycle |
| Human gate wait | `poGateTimeout` | `runtime.local.approvalWait` |

Both drivers call the same activity functions, pure policy functions, #95 retry
classes, evidence writers, and §6 state strings. The deterministic cycle ID
`<repo>-issue-<n>` is shared, so evidence, lineage, change records, and
idempotent GitHub mutations converge.

## Configuration

```yaml
runtime:
  mode: local              # temporal | local (closed set; unknown fails at load)
  local:
    sweepInterval: 5m      # idle delay between sweeps
    maxCycles: 1           # cycles per sweep per repo, >= 1
    once: false            # run one sweep and exit
    approvalWait: 10m      # max wait at a §55 gate; timeout => BLOCKED
```

An absent `runtime:` block preserves the Temporal behavior byte-for-byte.

## Running locally

```bash
prbot run                          # sweep every repo until SIGTERM/SIGINT
prbot run --once                   # one sweep, then exit 0
prbot run --repo ducklambda        # one repo only
prbot run --repo ducklambda --issue 42   # exactly one cycle
```

`prbot run` never dials `TEMPORAL_ADDRESS`, never starts schedules or the
webhook server, and logs to stderr. A signal stops the loop; an in-flight cycle
finishes before the process exits. Metrics stay opt-in (`METRICS_ADDR`); the
console and MCP surfaces remain separate opt-in processes. `prbot doctor` skips
the Temporal check in local mode.

Temporal-only features are reported inactive once at startup; they are never
silently treated as active.

## OS service

systemd:

```ini
[Unit]
Description=Konoha local runtime
After=network-online.target

[Service]
Type=simple
WorkingDirectory=/opt/konoha
Environment=KONOHA_CONFIG_DIR=/etc/konoha
EnvironmentFile=/etc/konoha/konoha.env
ExecStart=/usr/local/bin/prbot run
Restart=on-failure
KillSignal=SIGTERM
# Let the in-flight cycle finish before the service is killed.
TimeoutStopSec=3600

[Install]
WantedBy=multi-user.target
```

launchd (`~/Library/LaunchAgents/io.konoha.runtime.plist`):

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>io.konoha.runtime</string>
  <key>ProgramArguments</key>
  <array>
    <string>/usr/local/bin/prbot</string>
    <string>run</string>
  </array>
  <key>WorkingDirectory</key><string>/opt/konoha</string>
  <key>EnvironmentVariables</key>
  <dict>
    <key>KONOHA_CONFIG_DIR</key><string>/etc/konoha</string>
  </dict>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardErrorPath</key><string>/var/log/konoha.err</string>
  <key>StandardOutPath</key><string>/var/log/konoha.out</string>
</dict>
</plist>
```

Provide `GH_APP_ID`/`GH_APP_INSTALL_ID`/`GH_APP_PEM` through the unit's env file
(the App credential is read by `gh`-mutating activities only; agents never see
`GH_*`, §23). Installer/unit packaging is tracked by #148.

## Reading evidence

The readers work offline in both modes:

```bash
prbot describe <repo> <issue>
prbot logs <repo> <issue>
prbot export --format jsonl
```

`prbot status` and queue retry/cancel require Temporal and are unavailable in
local mode.

## Rollback to Temporal

1. Stop `prbot run`.
2. Set `runtime.mode: temporal` (or remove the `runtime:` block).
3. Start the worker (`make run`) and apply the schedules (`./scripts/apply.sh`).

The cycle ID and evidence layout are unchanged, so a repo can switch modes
without breaking existing records.
