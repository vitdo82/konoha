package konoha

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// #147: the license/CLA/open-core decision is documentation plus the assertions
// below, so a missing artifact or an unclassified top-level directory fails a
// test instead of drifting silently. See docs/licensing.md.
func TestLicensingBoundaryDocs(t *testing.T) {
	root := filepath.Join("..", "..")
	read := func(rel string) string {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("required licensing artifact %s: %v", rel, err)
		}
		return string(b)
	}

	license := read("LICENSE")
	if !strings.Contains(license, "Apache License") ||
		!strings.Contains(license, "Version 2.0, January 2004") {
		t.Fatal("LICENSE is not the Apache-2.0 text")
	}
	notice := read("NOTICE")
	if !strings.Contains(notice, "Konoha") || !strings.Contains(notice, "Copyright 2026 Vitali R") {
		t.Fatal("NOTICE must name the project and the copyright holder")
	}
	readme := read("README.md")
	if !strings.Contains(readme, "Apache License 2.0") || !strings.Contains(readme, "(LICENSE)") {
		t.Fatal("README must state the license and link LICENSE")
	}

	contributing := read("CONTRIBUTING.md")
	if !strings.Contains(contributing, "Signed-off-by:") || !strings.Contains(contributing, "DCO") {
		t.Fatal("CONTRIBUTING.md must document the DCO sign-off")
	}
	read(".github/workflows/dco.yml")

	record := read(filepath.Join("docs", "licensing.md"))
	classified := map[string]bool{}
	for _, line := range strings.Split(record, "\n") {
		if !strings.HasPrefix(line, "| `") {
			continue
		}
		field := strings.SplitN(strings.TrimPrefix(line, "| `"), "`", 2)[0]
		if strings.HasSuffix(field, "/") {
			classified[strings.TrimSuffix(field, "/")] = true
		}
	}
	if len(classified) == 0 {
		t.Fatal("docs/licensing.md must classify top-level directories in a table")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read repo root: %v", err)
	}
	onDisk := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() && e.Name() != ".git" {
			onDisk[e.Name()] = true
			if !classified[e.Name()] {
				t.Errorf("top-level directory %q is unclassified in docs/licensing.md (#147)", e.Name())
			}
		}
	}
	for dir := range classified {
		if !onDisk[dir] {
			t.Errorf("docs/licensing.md classifies %q, but that directory does not exist", dir)
		}
	}
}
