# konoha — cross-repo PR review bot (Temporal + opencode)
BIN := /tmp/prbot
QUEUE := review-queue

.PHONY: build run apply token test clean api api-ts-test

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