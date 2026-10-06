package konoha

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// #114: the per-cycle preview pane is deferred/blocked, not implemented. These
// doc-asserts pin the recorded blocker so the absence of an implementation is a
// stated decision (never a silent over-claim, §40), mirroring #103/#111.

func readRepoDoc(t *testing.T, parts ...string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(append([]string{"..", ".."}, parts...)...))
	if err != nil {
		t.Fatalf("read %v: %v", parts, err)
	}
	return string(b)
}

func TestPreviewPaneBlockedRecorded(t *testing.T) {
	readme := readRepoDoc(t, "README.md")
	for _, want := range []string{
		"blocked on isolation #114",
		"not available",
		"no preview proxy, preview route, or dev-server",
		"execution-isolation boundary",
	} {
		if !strings.Contains(readme, want) {
			t.Fatalf("README must record the preview-pane blocker (#114): missing %q", want)
		}
	}
	// No preview route may be advertised as available.
	if strings.Contains(readme, "/api/v1/cycles/{id}/preview") {
		t.Fatal("README must not advertise a preview route while #114 is blocked")
	}

	nonGoals := readRepoDoc(t, "docs", "non-goals.md")
	for _, want := range []string{"[#114] Per-cycle preview pane", "execution isolation", "#92"} {
		if !strings.Contains(nonGoals, want) {
			t.Fatalf("docs/non-goals.md must record the #114 blocker: missing %q", want)
		}
	}

	secConsole := readRepoDoc(t, "docs", "security-console.md")
	for _, want := range []string{"preview proxy", "origin-isolated", "loopback-only"} {
		if !strings.Contains(secConsole, want) {
			t.Fatalf("docs/security-console.md must cover the future preview-proxy origin: missing %q", want)
		}
	}
}
