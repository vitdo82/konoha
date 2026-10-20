#!/usr/bin/env bash
# Drives one scripted scenario in the running hermetic QA stack (#206).
# DEV/QA ONLY — never a deploy path. Requires `make qa` to be up.
#
#   scripts/qa-scenario.sh <scenario> [issue]
#
# Scenarios that work with the committed QA config (configs/qa/repos.yaml):
#   happy     reviewer APPROVE              -> CLOSED
#   repair    REQUEST_CHANGES then APPROVE  -> CLOSED (one repair round)
#   reject    reviewer always REQUEST_CHANGES -> BLOCKED (never closes)
#   ci-fail   APPROVE but CI bucket=fail     -> HUMAN_REQUIRED (no merge)
#
# `issue` defaults to the fake store's next number. Each scenario needs a FRESH
# issue number: the workflow ID is deterministic (<repo>-issue-<n>), so a reused
# number is a no-op. Full reset: `make qa-down && make qa`.
#
# NOT supported here (need a config change, which is bootstrap-only):
#   risk (§20 gate: humanRequiredPaths), ci-skip (merge.requireCI:false).
set -euo pipefail

scenario="${1:-}"
issue="${2:-}"
work="${KONOHA_QA_WORK:-$(cd "$(dirname "$0")/.." && pwd)/.qa/work}"
agents="$work/agents"
store="$work/fakegh/store.json"

[ -n "$scenario" ] || { echo "usage: $0 <happy|repair|reject|ci-fail> [issue]" >&2; exit 2; }
[ -f "$store" ] || { echo "no QA state at $work — run 'make qa' first" >&2; exit 1; }

reviewer="VERDICT:APPROVE\nBLOCKING:\n- none\nNITS:\n- none\n"
repair='{"summary":"fixed","concerns":[]}'
ci_bucket=""
case "$scenario" in
  happy)  ;;
  repair) reviewer="VERDICT:REQUEST_CHANGES\nBLOCKING:\n- auth gap\nNITS:\n- none\n\nVERDICT:APPROVE\nBLOCKING:\n- none\nNITS:\n- none\n" ;;
  reject) reviewer="VERDICT:REQUEST_CHANGES\nBLOCKING:\n- nope\nNITS:\n- none\n" ;;
  ci-fail) ci_bucket="fail" ;;
  *) echo "unknown scenario: $scenario" >&2; exit 2 ;;
esac

# Add an open issue (fresh number) + set the CI bucket; print the number used.
issue="$(ISSUE="$issue" SCENARIO="$scenario" CI_BUCKET="$ci_bucket" python3 - "$store" <<'PY'
import json, os, sys
p = sys.argv[1]
s = json.load(open(p))
n = int(os.environ["ISSUE"]) if os.environ.get("ISSUE") else s.get("next", 1)
s["issues"].append({"number": n, "state": "open", "title": os.environ["SCENARIO"],
                    "body": "qa scenario " + os.environ["SCENARIO"], "labels": ["bug"], "isPr": False})
s["next"] = max(s.get("next", 1), n + 1)
s["ciBucket"] = os.environ["CI_BUCKET"]
json.dump(s, open(p, "w"))
print(n)
PY
)"

mkdir -p "$agents"
printf '%s' '{"summary":"s","spec":"generated-temp-spec","requirements":["r"],"acceptanceCriteria":["Given x When y Then z"]}' > "$agents/analyst.txt"
printf '%s' '{"complexity":3,"confidence":"high","rationale":"small"}' > "$agents/estimator.txt"
printf '%s' '{"summary":"the change","concerns":[]}' > "$agents/developer.txt"
printf '%s' "$repair" > "$agents/repair.txt"
printf '%b' "$reviewer" > "$agents/reviewer.txt"

echo "==> scenario=$scenario issue=$issue (ciBucket=${ci_bucket:-pass})"
docker compose exec -T worker /prbot cycle r "$issue"
echo "==> started r-issue-$issue; watch: docker compose logs -f worker"
