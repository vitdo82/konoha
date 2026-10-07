#!/usr/bin/env bash
# Seeds the hermetic QA fixture state (#206). DEV/QA ONLY — never a deploy path.
#
# Resets and recreates the `/work` state (bind-mounted from the host at
# ./.qa/work) so every `docker compose up` starts from a byte-identical state
# (AC5 idempotency):
#   - /work/fixture/r       a real git checkout (the worker's repoPath)
#   - /work/git-base/o/r.git a local bare remote (KONOHA_GIT_BASE/<owner>/<repo>.git)
#   - /work/fakegh/store.json one open, unmanaged issue #1 (the sweep input)
#   - /work/agents/*.txt     canned fake-opencode role outputs (e2e/README.md)
set -euo pipefail

work="${KONOHA_QA_WORK:-/work}"
fixture="$work/fixture/r"
git_base="$work/git-base"
state="$work/fakegh"
agents="$work/agents"

# Reset only our own state; never touch the bind-mount root itself.
rm -rf "$work/fixture" "$work/git-base" "$work/fakegh" "$work/agents" "$work/.konoha"
mkdir -p "$fixture" "$git_base/o" "$state" "$agents" "$work/.konoha"

git -C "$fixture" init -q -b main
git -C "$fixture" config user.email "qa@konoha"
git -C "$fixture" config user.name "qa"
printf '# fixture repository\n' > "$fixture/README.md"
git -C "$fixture" add .
git -C "$fixture" commit -qm "init"
# Bare remote mirroring KONOHA_GIT_BASE/<owner>/<repo>.git (see publish.go).
git clone -q --bare "$fixture" "$git_base/o/r.git"

# One open, unmanaged issue: the §32 sweep / Analyst inputs.
cat > "$state/store.json" <<'JSON'
{"issues":[{"number":1,"state":"open","title":"Add a greeting to the fixture","body":"The fixture repo should greet the reader.","labels":["enhancement"],"isPr":false}],"labels":["enhancement"],"next":2}
JSON

printf '%s' '{"summary":"add a greeting","spec":"generated-temp-spec","requirements":["greet the reader"],"acceptanceCriteria":["Given the fixture When read Then it greets"]}' > "$agents/analyst.txt"
printf '%s' '{"complexity":3,"confidence":"high","rationale":"small change"}' > "$agents/estimator.txt"
printf '%s' '{"summary":"the change","concerns":[]}' > "$agents/developer.txt"
printf '%s\n' 'VERDICT:APPROVE' 'BLOCKING:' '- none' 'NITS:' '- none' 'CONCLUDE: ok' > "$agents/reviewer.txt"

echo "qa fixtures seeded at $work"
