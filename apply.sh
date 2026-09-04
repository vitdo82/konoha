#!/usr/bin/env bash
# Applies repos.yaml to Temporal: creates/updates one <repo>-review-all schedule
# per repo. Idempotent-ish: deletes then recreates each schedule.
set -euo pipefail
cd "$(dirname "$0")"

queue=review-queue
interval=5m

# minimal YAML parse: only reads top-level `repo:`, `owner:`, `repoPath:` under repos
while IFS= read -r repo; do
  owner=$(awk -v k="owner:" '$1==k{print $2}' repos.yaml)
  repopath=$(awk -v r="repo: $repo" '$0==r{f=1} f&&$1=="repoPath:"{print $2; exit}' repos.yaml)
  [ -z "$repo" ] && continue
  [ -z "$repopath" ] && { echo "skip $repo: no repoPath"; continue; }

  id="${repo}-review-all"
  echo "==> $id ($owner/$repo @ $repopath)"
  temporal schedule delete --schedule-id "$id" >/dev/null 2>&1 || true
  temporal schedule create \
    --schedule-id "$id" \
    --workflow-id "$id" \
    --interval "$interval" \
    --type ReviewAllWorkflow \
    --task-queue "$queue" \
    --input "{\"owner\":\"$owner\",\"repo\":\"$repo\",\"repoPath\":\"$repopath\"}"

  id="${repo}-issue-all"
  echo "==> $id ($owner/$repo @ $repopath)"
  temporal schedule delete --schedule-id "$id" >/dev/null 2>&1 || true
  temporal schedule create \
    --schedule-id "$id" \
    --workflow-id "$id" \
    --interval "$interval" \
    --type IssueAllWorkflow \
    --task-queue "$queue" \
    --input "{\"owner\":\"$owner\",\"repo\":\"$repo\",\"repoPath\":\"$repopath\"}"
done < <(grep -E '^\s+- repo:' repos.yaml | awk '{print $3}')