#!/usr/bin/env bash
# Applies repos.yaml to Temporal: creates/updates one <repo>-review-all and
# <repo>-issue-all schedule per repo, plus a <repo>-deps schedule (§52) when
# lifecycle.deps.enabled. repos.yaml is parsed by the Go loader (`prbot config`),
# which fails fast on unknown keys / out-of-range values; the emitted JSON
# becomes the schedule workflow input, so config can't drift between apply.sh and
# the workflows (§33 lockstep). Idempotent-ish: deletes then recreates each
# schedule.
set -euo pipefail
cd "$(dirname "$0")/.."   # repo root (script moved to scripts/ in the layout refactor)

queue=review-queue
interval=5m

# The loader is Go, not grep/awk; build once so repos.yaml and the workflows
# share one parser. The entrypoint moved under cmd/konoha.
make build --no-print-directory

while IFS=$'\t' read -r repo input; do
  [ -z "$repo" ] && continue

  id="${repo}-review-all"
  echo "==> $id ($repo)"
  temporal schedule delete --schedule-id "$id" >/dev/null 2>&1 || true
  temporal schedule create \
    --schedule-id "$id" \
    --workflow-id "$id" \
    --interval "$interval" \
    --type ReviewAllWorkflow \
    --task-queue "$queue" \
    --input "$input"

  id="${repo}-issue-all"
  echo "==> $id ($repo)"
  temporal schedule delete --schedule-id "$id" >/dev/null 2>&1 || true
  temporal schedule create \
    --schedule-id "$id" \
    --workflow-id "$id" \
    --interval "$interval" \
    --type IssueAllWorkflow \
    --task-queue "$queue" \
    --input "$input"
done < <(/tmp/prbot config)

# §52 dependency + security scan schedules. Only repos with lifecycle.deps.enabled
# emit a line here, so a disabled repo gets no scan and no issues. The per-repo
# cadence is lifecycle.deps.schedule (default 24h).
while IFS=$'\t' read -r repo deps_interval input; do
  [ -z "$repo" ] && continue

  id="${repo}-deps"
  echo "==> $id ($repo, every $deps_interval)"
  temporal schedule delete --schedule-id "$id" >/dev/null 2>&1 || true
  temporal schedule create \
    --schedule-id "$id" \
    --workflow-id "$id" \
    --interval "$deps_interval" \
    --type DependencyWorkflow \
    --task-queue "$queue" \
    --input "$input"
done < <(/tmp/prbot config --deps)

# §53 alert schedules. Only repos with lifecycle.alerting.enabled emit a line
# here, so a disabled repo is never evaluated. The per-repo cadence is
# lifecycle.alerting.schedule (default 24h).
while IFS=$'\t' read -r repo alert_interval input; do
  [ -z "$repo" ] && continue

  id="${repo}-alerts"
  echo "==> $id ($repo, every $alert_interval)"
  temporal schedule delete --schedule-id "$id" >/dev/null 2>&1 || true
  temporal schedule create \
    --schedule-id "$id" \
    --workflow-id "$id" \
    --interval "$alert_interval" \
    --type AlertWorkflow \
    --task-queue "$queue" \
    --input "$input"
done < <(/tmp/prbot config --alerts)