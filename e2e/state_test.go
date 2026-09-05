//go:build e2e

package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// fakeGhItem mirrors a subset of the fake-gh store for scenario assertions.
type fakeGhItem struct {
	Number  int      `json:"number"`
	State   string   `json:"state"`
	Title   string   `json:"title"`
	Body    string   `json:"body"`
	Labels  []string `json:"labels"`
	IsPR    bool     `json:"isPr"`
	HeadRef string   `json:"headRef"`
}

type fakeStore struct {
	Issues []fakeGhItem `json:"issues"`
	Labels []string     `json:"labels"`
	Next   int          `json:"next"`
}

func statePath() string { return filepath.Join(fakeStateDir, "store.json") }

// seedState writes the fake-gh store file (equivalent to pre-existing GitHub
// state).
func seedState(t *testing.T, jsonBody string) {
	t.Helper()
	if err := os.WriteFile(statePath(), []byte(jsonBody), 0o644); err != nil {
		t.Fatalf("seed state: %v", err)
	}
}

// readState returns the fake-gh store for assertions.
func readState(t *testing.T) *fakeStore {
	t.Helper()
	b, err := os.ReadFile(statePath())
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	var s fakeStore
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatalf("parse state: %v", err)
	}
	return &s
}
