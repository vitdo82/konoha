# syntax=docker/dockerfile:1
#
# konoha hermetic QA image (#206). DEV/QA ONLY — never a deploy path.
#
# Builds the worker with `-tags=e2e` and bakes the tier-1 fake `gh`/`opencode`
# shims (e2e/fakegh, e2e/fakeopencode) plus the `temporal` CLI (for
# scripts/apply.sh), so the compose stack runs with zero GitHub credentials and
# no network egress. The `KONOHA_TOKEN_FIXED`/`KONOHA_GIT_BASE` test seams are
# runtime-only (not build-tagged) and are set by docker-compose.yaml.

# --- build ------------------------------------------------------------------
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -tags=e2e -trimpath -o /out/prbot ./cmd/konoha \
 && CGO_ENABLED=0 go build -tags=e2e -trimpath -o /out/gh ./e2e/fakegh \
 && CGO_ENABLED=0 go build -tags=e2e -trimpath -o /out/opencode ./e2e/fakeopencode

# --- runtime ----------------------------------------------------------------
FROM alpine:3.22
RUN apk add --no-cache bash git socat ca-certificates \
 && adduser -D -H -u 10001 konoha

# Temporal CLI copied from the matching auto-setup image so its version tracks
# the server (scripts/apply.sh shells out to `temporal schedule ...`).
COPY --from=temporalio/auto-setup:1.29.7 /usr/local/bin/temporal /usr/local/bin/temporal

COPY --from=build /out/prbot /prbot
COPY --from=build /out/prbot /tmp/prbot
COPY --from=build /out/gh /usr/local/bin/gh
COPY --from=build /out/opencode /usr/local/bin/opencode

COPY configs /app/configs
COPY scripts /app/scripts

# The worker resolves relative evidence/worktree paths against this CWD, which
# compose bind-mounts from the host (./.qa/work) so `prbot describe`/`export`
# read the evidence from the host.
WORKDIR /work
ENV HOME=/work/.home
ENTRYPOINT ["/app/scripts/qa-entrypoint.sh"]
