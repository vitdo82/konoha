#!/usr/bin/env bash
# Applies repos.yaml to Temporal: creates/updates one <repo>-review-all and
# <repo>-issue-all schedule per repo. repos.yaml is parsed by the Go loader
# (`prbot config`), which fails fast on unknown keys / out-of-range values; the
# emitted JSON becomes the schedule workflow input, so config can't drift
# between apply.sh and the workflows (§33 lockstep). Idempotent-ish: deletes
# then recreates each schedule.
set -euo pipefail
cd "$(dirname "$0")"

queue=review-queue
interval=5m

# The loader is Go, not grep/awk; build once so repos.yaml and the workflows
# share one parser.
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