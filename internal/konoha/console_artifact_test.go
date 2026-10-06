package konoha

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// #107: the artifact kind + preview decision is a PURE projection (no I/O): an
// explicit text/image set, everything else binary; HTML previews as text/plain
// and oversize text is download-only.
func TestClassifyAndPreviewProjection(t *testing.T) {
	cases := []struct {
		name        string
		wantKind    string
		wantCt      string
		previewable bool
	}{
		{"agent.stdout", artifactKindText, "text/plain", true},
		{"result.json", artifactKindText, "application/json", true},
		{"diff.patch", artifactKindText, "text/plain", true},
		{"page.html", artifactKindText, "text/html", true},
		{"notes.md", artifactKindText, "text/markdown", true},
		{"shot.png", artifactKindImage, "image/png", true},
		{"icon.svg", artifactKindBinary, "application/octet-stream", false},
		{"archive.zip", artifactKindBinary, "application/octet-stream", false},
		{"noext", artifactKindBinary, "application/octet-stream", false},
	}
	for _, tc := range cases {
		kind, ct := classifyArtifact(tc.name)
		if kind != tc.wantKind || ct != tc.wantCt {
			t.Fatalf("classifyArtifact(%q) = (%q,%q), want (%q,%q)", tc.name, kind, ct, tc.wantKind, tc.wantCt)
		}
		pct, ok := previewContentType(tc.name, 10)
		if ok != tc.previewable {
			t.Fatalf("previewContentType(%q,10) previewable=%v, want %v", tc.name, ok, tc.previewable)
		}
		if tc.wantKind == artifactKindText && ok && pct != "text/plain; charset=utf-8" {
			t.Fatalf("text preview of %q = %q, want text/plain", tc.name, pct)
		}
	}

	// HTML is always text/plain (never text/html → never executed).
	if ct, ok := previewContentType("evil.html", 100); !ok || ct != "text/plain; charset=utf-8" {
		t.Fatalf("HTML preview = (%q,%v), want text/plain", ct, ok)
	}
	// The cap is enforced on text only; oversize text is not previewable.
	if _, ok := previewContentType("big.txt", artifactPreviewCap+1); ok {
		t.Fatal("oversize text must be download-only")
	}
	if _, ok := previewContentType("big.txt", artifactPreviewCap); !ok {
		t.Fatal("text at the cap must be previewable")
	}
	// Images preview regardless of size (no text cap).
	if _, ok := previewContentType("big.png", artifactPreviewCap*10); !ok {
		t.Fatal("images must preview")
	}
}

// #107 AC: path traversal in an artifact path is rejected (pure guard).
func TestValidArtifactPath(t *testing.T) {
	ok := []string{"outcome.json", "verification/unit.stdout", "a/b/c.txt"}
	for _, n := range ok {
		if !validArtifactPath(n) {
			t.Fatalf("validArtifactPath(%q) = false, want true", n)
		}
	}
	bad := []string{"", "/etc/passwd", "../x", "..", ".", "a/../b", "a/./b", "a//b", "a/../../b", "a/"}
	for _, n := range bad {
		if validArtifactPath(n) {
			t.Fatalf("validArtifactPath(%q) = true, want false", n)
		}
	}
}

func newArtifactConsole(t *testing.T) (*ConsoleServer, *EvidenceStore, *httptest.Server) {
	t.Helper()
	store, err := NewEvidenceStore(EvidenceConfig{Dir: t.TempDir()}, &defaultRedactor)
	if err != nil {
		t.Fatalf("evidence store: %v", err)
	}
	o := CycleOutcome{WorkflowID: "duck-issue-7", Owner: "acme", Repo: "duck", Issue: 7, TerminalState: stateClosed}
	b, _ := json.Marshal(o)
	if err := store.Write("duck-issue-7", evidenceOutcome, string(b), false); err != nil {
		t.Fatal(err)
	}
	srv, err := newConsoleServer(ConsoleConfig{Addr: "127.0.0.1:0", Token: "secret-token"}, &consoleData{store: store}, "test-1")
	if err != nil {
		t.Fatalf("new console: %v", err)
	}
	hs := httptest.NewServer(srv)
	t.Cleanup(hs.Close)
	return srv, store, hs
}

func artifactURL(hs *httptest.Server, kind, name string) string {
	return hs.URL + "/api/v1/cycles/duck-issue-7/" + kind + "/" + name
}

// #107 AC1: an HTML artifact is previewed as text/plain and never rendered as
// text/html (the CSP + nosniff baseline also applies to every response).
func TestArtifactHTMLPreviewedAsText(t *testing.T) {
	_, store, hs := newArtifactConsole(t)
	html := "<html><body><script>alert(1)</script></body></html>"
	if err := store.Write("duck-issue-7", "page.html", html, false); err != nil {
		t.Fatal(err)
	}

	res := getURL(t, artifactURL(hs, "artifacts", "page.html"), "secret-token", "")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("preview status = %d, want 200", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("HTML preview Content-Type = %q, want text/plain (never executed)", ct)
	}
	if csp := res.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") {
		t.Fatalf("artifact response missing CSP baseline: %q", csp)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if string(body) != html {
		t.Fatalf("preview body = %q, want the raw artifact text", body)
	}
}

// #107 AC4/AC5: every artifact downloads as an attachment with its content type,
// read only from the evidence store.
func TestArtifactDownload(t *testing.T) {
	_, store, hs := newArtifactConsole(t)
	for name, content := range map[string]string{
		"page.html": "<h1>hi</h1>",
		"shot.png":  "\x89PNG\r\n\x1a\nDATA",
		"raw.bin":   "\x00\x01\x02",
	} {
		if err := store.Write("duck-issue-7", name, content, false); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct{ name, wantCt string }{
		{"page.html", "text/html"},
		{"shot.png", "image/png"},
		{"raw.bin", "application/octet-stream"},
	}
	for _, tc := range cases {
		res := getURL(t, artifactURL(hs, "download", tc.name), "secret-token", "")
		if res.StatusCode != http.StatusOK {
			t.Fatalf("download %s status = %d, want 200", tc.name, res.StatusCode)
		}
		cd := res.Header.Get("Content-Disposition")
		if !strings.HasPrefix(cd, "attachment") || !strings.Contains(cd, tc.name) {
			t.Fatalf("download %s Content-Disposition = %q, want attachment + filename", tc.name, cd)
		}
		if ct := res.Header.Get("Content-Type"); ct != tc.wantCt {
			t.Fatalf("download %s Content-Type = %q, want %q", tc.name, ct, tc.wantCt)
		}
		res.Body.Close()
	}
}

// #107 AC2/AC3: the preview cap forces oversize text to download-only, binary
// artifacts are download-only, and traversal is rejected.
func TestArtifactPreviewLimitsAndTraversal(t *testing.T) {
	srv, store, hs := newArtifactConsole(t)
	if err := store.Write("duck-issue-7", "big.txt", strings.Repeat("a", artifactPreviewCap+1), false); err != nil {
		t.Fatal(err)
	}
	if err := store.Write("duck-issue-7", "raw.bin", "\x00\x01", false); err != nil {
		t.Fatal(err)
	}

	res := getURL(t, artifactURL(hs, "artifacts", "big.txt"), "secret-token", "")
	if res.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("oversize preview = %d, want 415", res.StatusCode)
	}
	res.Body.Close()

	res = getURL(t, artifactURL(hs, "artifacts", "raw.bin"), "secret-token", "")
	if res.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("binary preview = %d, want 415", res.StatusCode)
	}
	res.Body.Close()

	res = getURL(t, artifactURL(hs, "download", "big.txt"), "secret-token", "")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("oversize download = %d, want 200 (download-only)", res.StatusCode)
	}
	res.Body.Close()

	res = getURL(t, artifactURL(hs, "artifacts", "ghost.txt"), "secret-token", "")
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("absent artifact = %d, want 404", res.StatusCode)
	}
	res.Body.Close()

	// Traversal is rejected at the handler boundary (the mux would clean a real
	// request; SetPathValue exercises the guard directly).
	req := httptest.NewRequest(http.MethodGet, "/api/v1/cycles/duck-issue-7/artifacts/x", nil)
	req.SetPathValue("id", "duck-issue-7")
	req.SetPathValue("name", "../secret")
	w := httptest.NewRecorder()
	srv.handleArtifactPreview(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("traversal preview = %d, want 400", w.Code)
	}
}

// #107 AC6: the artifact-listing projection carries path/size/type without a
// browser, and the list endpoint serves it.
func TestArtifactListView(t *testing.T) {
	_, store, hs := newArtifactConsole(t)
	if err := store.Write("duck-issue-7", "page.html", "<h1>hi</h1>", false); err != nil {
		t.Fatal(err)
	}
	if err := store.Write("duck-issue-7", "shot.png", "\x89PNGdata", false); err != nil {
		t.Fatal(err)
	}
	if err := store.Write("duck-issue-7", "raw.bin", "\x00\x01\x02", false); err != nil {
		t.Fatal(err)
	}
	if err := store.Write("duck-issue-7", "verification/unit.stdout", "ok", false); err != nil {
		t.Fatal(err)
	}

	res := getURL(t, hs.URL+"/api/v1/cycles/duck-issue-7/artifacts", "secret-token", "")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d, want 200", res.StatusCode)
	}
	var body struct {
		Artifacts []ArtifactView `json:"artifacts"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	res.Body.Close()

	byPath := map[string]ArtifactView{}
	for _, a := range body.Artifacts {
		byPath[a.Path] = a
	}
	if a := byPath["page.html"]; a.Size != int64(len("<h1>hi</h1>")) || a.Kind != artifactKindText ||
		a.ContentType != "text/html" || !a.Previewable {
		t.Fatalf("page.html view = %+v, want text/text/html previewable", a)
	}
	if a := byPath["shot.png"]; a.Kind != artifactKindImage || !a.Previewable {
		t.Fatalf("shot.png view = %+v, want image previewable", a)
	}
	if a := byPath["raw.bin"]; a.Kind != artifactKindBinary || a.Previewable {
		t.Fatalf("raw.bin view = %+v, want binary download-only", a)
	}
	if a := byPath["verification/unit.stdout"]; !a.Previewable || a.Kind != artifactKindText {
		t.Fatalf("verification stream view = %+v, want text previewable", a)
	}
}
