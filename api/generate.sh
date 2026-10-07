#!/usr/bin/env bash
# Regenerate the Konoha Console API clients from the canonical spec (#124).
#
#   api/openapi.yaml  ->  internal/konoha/consoleapi/api.gen.go  (Go types+client)
#   api/openapi.yaml  ->  api/generated/ts/api.gen.ts            (TS types)
#
# The generated Go/TS files are checked in — never hand-edit them; change the
# spec and re-run `make api` (or this script). The TypeScript fetch wrapper in
# api/generated/ts/client.ts is written against the generated types.
set -euo pipefail

cd "$(dirname "$0")/.."

OAPI_VERSION="v2.8.0"
TS_VERSION="7"

echo "generating Go client (oapi-codegen ${OAPI_VERSION})…"
go run "github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@${OAPI_VERSION}" \
  -config api/oapi-codegen.yaml \
  -o internal/konoha/consoleapi/api.gen.go \
  api/openapi.yaml

echo "generating TypeScript types (openapi-typescript ${TS_VERSION})…"
npx --yes "openapi-typescript@${TS_VERSION}" api/openapi.yaml -o api/generated/ts/api.gen.ts

echo "done."
