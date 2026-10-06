package konoha

import (
	"mime"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
)

// Safe artifact viewer (#107). Human review must be able to read what the agent
// actually produced. Every artifact is downloadable with an attachment header;
// text/markdown (and HTML, served as TEXT and never executed) previews up to
// artifactPreviewCap (1 MiB); images preview as images; binary/oversize is
// download-only. Paths are evidence-store-relative only (traversal rejected),
// and the console's global CSP + nosniff headers apply to every response, so an
// HTML artifact can never run.

// artifactPreviewCap bounds the bytes the preview endpoint will serve (1 MiB).
const artifactPreviewCap = 1 << 20

// Artifact kinds.
const (
	artifactKindText   = "text"
	artifactKindImage  = "image"
	artifactKindBinary = "binary"
)

// artifactTextTypes maps a lowercase extension to the DOWNLOAD content type.
// Kept explicit (not mime.TypeByExtension) so classification is deterministic
// across hosts. HTML downloads as text/html but previews as text/plain.
var artifactTextTypes = map[string]string{
	".txt": "text/plain", ".log": "text/plain", ".stdout": "text/plain",
	".stderr": "text/plain", ".patch": "text/plain", ".diff": "text/plain",
	".md": "text/markdown", ".markdown": "text/markdown",
	".json": "application/json", ".yaml": "text/yaml", ".yml": "text/yaml",
	".toml": "text/toml", ".xml": "application/xml", ".csv": "text/csv",
	".html": "text/html", ".htm": "text/html",
}

// artifactImageTypes maps an image extension to its served content type. SVG is
// deliberately absent: it can carry script, so it is treated as text.
var artifactImageTypes = map[string]string{
	".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg",
	".gif": "image/gif", ".webp": "image/webp", ".bmp": "image/bmp",
	".ico": "image/x-icon",
}

// classifyArtifact is the PURE artifact-kind projection: an explicit text/image
// set; everything else (incl. SVG) is binary and download-only.
func classifyArtifact(name string) (kind, contentType string) {
	ext := strings.ToLower(path.Ext(name))
	if ct, ok := artifactImageTypes[ext]; ok {
		return artifactKindImage, ct
	}
	if ct, ok := artifactTextTypes[ext]; ok {
		return artifactKindText, ct
	}
	return artifactKindBinary, "application/octet-stream"
}

// previewContentType is the PURE preview decision: text is served as text/plain
// (HTML included — never executed), images as their image type, and binary or
// oversize artifacts are not previewable (ok=false → download only).
func previewContentType(name string, size int64) (string, bool) {
	kind, ct := classifyArtifact(name)
	switch kind {
	case artifactKindImage:
		return ct, true
	case artifactKindText:
		if size > artifactPreviewCap {
			return "", false
		}
		return "text/plain; charset=utf-8", true
	default:
		return "", false
	}
}

// ArtifactView is one artifact's metadata (pure projection; never the content).
type ArtifactView struct {
	Path        string `json:"path"`
	Size        int64  `json:"size"`
	ContentType string `json:"contentType"`
	Kind        string `json:"kind"`
	Previewable bool   `json:"previewable"`
}

// artifactView projects one artifact. Pure: the caller supplies the size, so the
// projection is unit-testable without I/O.
func artifactView(name string, size int64) ArtifactView {
	kind, ct := classifyArtifact(name)
	_, previewable := previewContentType(name, size)
	return ArtifactView{Path: name, Size: size, ContentType: ct, Kind: kind, Previewable: previewable}
}

// validArtifactPath accepts only a canonical, evidence-store-relative artifact
// path: no absolute path, no NUL, no traversal, no empty/dot segments. Pure.
func validArtifactPath(name string) bool {
	if name == "" || strings.ContainsRune(name, 0) || strings.HasPrefix(name, "/") {
		return false
	}
	if path.Clean(name) != name || name == "." || name == ".." {
		return false
	}
	for _, seg := range strings.Split(name, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

// artifactSizer is the optional sink capability for a metadata stat: the local
// sink implements it so listing never reads a large artifact's content.
type artifactSizer interface {
	ArtifactSize(relPath string) (int64, error)
}

// ArtifactSize stats one artifact (local sink; #100 optional capability).
func (s *localDirSink) ArtifactSize(relPath string) (int64, error) {
	p, err := s.abs(relPath)
	if err != nil {
		return 0, err
	}
	info, err := os.Stat(p)
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

// artifactSize returns an artifact's byte size, preferring a sink stat and
// falling back to a full read (any error → 0, an absent artifact).
func (s *EvidenceStore) artifactSize(id, name string) int64 {
	rel, err := s.relDirFor(id, false)
	if err != nil {
		return 0
	}
	full := path.Join(rel, name)
	if sz, ok := s.sink.(artifactSizer); ok {
		if n, err := sz.ArtifactSize(full); err == nil {
			return n
		}
	}
	b, err := s.sink.Read(full)
	if err != nil {
		return 0
	}
	return int64(len(b))
}

// artifactViews lists one cycle's artifacts with metadata, sorted by path.
func (s *EvidenceStore) artifactViews(id string) ([]ArtifactView, error) {
	names, err := s.listArtifacts(id)
	if err != nil {
		return nil, err
	}
	out := make([]ArtifactView, 0, len(names))
	for _, n := range names {
		out = append(out, artifactView(n, s.artifactSize(id, n)))
	}
	return out, nil
}

// readArtifactBytes reads one artifact's raw bytes; ok=false on any read error.
func (s *EvidenceStore) readArtifactBytes(id, name string) ([]byte, bool) {
	rel, err := s.relDirFor(id, false)
	if err != nil {
		return nil, false
	}
	b, err := s.sink.Read(path.Join(rel, name))
	if err != nil {
		return nil, false
	}
	return b, true
}

// contentDisposition renders a safe disposition header (filename quoted/encoded
// by stdlib mime; never interpolated by hand).
func contentDisposition(disp, filename string) string {
	return mime.FormatMediaType(disp, map[string]string{"filename": filename})
}

// handleArtifactPreview serves an artifact inline when previewable (text as
// text/plain, images as images); binary/oversize → 415 (download-only).
func (s *ConsoleServer) handleArtifactPreview(w http.ResponseWriter, r *http.Request) {
	s.serveArtifact(w, r, false)
}

// handleArtifactDownload serves an artifact as an attachment with its content
// type, for every artifact.
func (s *ConsoleServer) handleArtifactDownload(w http.ResponseWriter, r *http.Request) {
	s.serveArtifact(w, r, true)
}

// serveArtifact is the shared artifact read + serve path. It 400s a malformed
// id/path, 404s an absent artifact, and 415s a non-previewable preview.
func (s *ConsoleServer) serveArtifact(w http.ResponseWriter, r *http.Request, download bool) {
	id := r.PathValue("id")
	if !validConsoleCycleID(id) {
		writeConsoleError(w, http.StatusBadRequest, "malformed cycle id")
		return
	}
	name := r.PathValue("name")
	if !validArtifactPath(name) {
		writeConsoleError(w, http.StatusBadRequest, "malformed artifact path")
		return
	}
	data, ok := s.data.store.readArtifactBytes(id, name)
	if !ok {
		writeConsoleError(w, http.StatusNotFound, "unknown artifact")
		return
	}
	base := path.Base(name)
	if download {
		w.Header().Set("Content-Type", classifyContentType(name))
		w.Header().Set("Content-Disposition", contentDisposition("attachment", base))
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
		return
	}
	pct, ok := previewContentType(name, int64(len(data)))
	if !ok {
		kind, _ := classifyArtifact(name)
		writeConsoleError(w, http.StatusUnsupportedMediaType, kind+" artifact is download-only")
		return
	}
	w.Header().Set("Content-Type", pct)
	w.Header().Set("Content-Disposition", contentDisposition("inline", base))
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// classifyContentType is the download content type for an artifact (never "").
func classifyContentType(name string) string {
	_, ct := classifyArtifact(name)
	if ct == "" {
		return "application/octet-stream"
	}
	return ct
}
