# konoha — cross-repo PR review bot (Temporal + opencode)
BIN := /tmp/prbot
QUEUE := review-queue

# The hermetic QA stack (#206) runs the worker as the invoking host user so the
# bind-mounted ./.qa/work is writable and its 0600 evidence is host-readable.
export QA_UID := $(shell id -u)
export QA_GID := $(shell id -g)

.PHONY: build run apply token test clean api api-ts-test qa qa-down qa-describe qa-export

# Build the worker binary (entrypoint lives under cmd/konoha)
build:
	go build -o $(BIN) ./cmd/konoha

# Run the worker (needs GH_APP_ID, GH_APP_INSTALL_ID, GH_APP_PEM env)
run: build
	$(BIN)

# Apply/recreate Temporal schedules from configs/repos.yaml
apply:
	./scripts/apply.sh

# Print a masked app installation token (smoke-test auth)
token: build
	$(BIN) token

# Build sanity + basic Go checks + unit tests
test:
	go vet ./...
	go test ./...
	go build ./...

# Regenerate the console API clients from api/openapi.yaml (#124). Generated
# files under internal/konoha/consoleapi and api/generated are never hand-edited.
api:
	./api/generate.sh

# Typecheck + smoke the generated TypeScript client (needs node/npm).
api-ts-test:
	cd api/ts && npm install --silent && npm run --silent typecheck && npm run --silent smoke

clean:
	rm -f $(BIN)

# Hermetic Docker Compose QA environment (#206). DEV/QA ONLY — fake gh/opencode,
# a local fixture git remote, zero GitHub credentials, no network egress. Brings
# the stack up (fail-closed on unhealthy Temporal) and seeds one cycle.
qa:
	mkdir -p .qa/work
	docker compose up --wait --build
	docker compose exec -T worker /prbot cycle r 1

qa-down:
	docker compose down -v --remove-orphans

# Host-side readers over the bind-mounted evidence (run `make build` first). The
# evidence root is relative to the worker CWD, so read from .qa/work.
qa-describe:
	cd .qa/work && KONOHA_CONFIG_DIR="$(CURDIR)/configs/qa" $(BIN) describe r 1

qa-export:
	cd .qa/work && KONOHA_CONFIG_DIR="$(CURDIR)/configs/qa" $(BIN) export --format jsonl