# konoha — cross-repo PR review bot (Temporal + opencode)
BIN := /tmp/prbot
QUEUE := review-queue

.PHONY: build run apply token test clean

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

clean:
	rm -f $(BIN)