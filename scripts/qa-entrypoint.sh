#!/usr/bin/env bash
# konoha hermetic QA entrypoint (#206). DEV/QA ONLY — never a deploy path.
#
# 1. seed the fixture git remote/repo + fake GitHub/agent state (idempotent),
# 2. apply the QA Temporal schedules (needs the temporal CLI + a healthy frontend
#    — compose gates the worker on temporal: service_healthy),
# 3. exec the real worker (fake gh/opencode are on PATH).
set -euo pipefail

# git needs a writable HOME and must trust the bind-mounted /work tree (the
# container may run as the host uid, which differs from the mount's recorded
# owner under some runtimes).
mkdir -p "$HOME"
git config --global --add safe.directory '*'

/app/scripts/qa-seed.sh

# KONOHA_SKIP_BUILD: apply.sh uses the /tmp/prbot baked into the image instead of
# invoking the Go toolchain (absent from the runtime image).
KONOHA_SKIP_BUILD=1 /app/scripts/apply.sh

exec /prbot
