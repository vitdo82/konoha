package konoha

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// §50: conventional-commit classification matrix (feat→minor, fix/perf→patch,
// breaking→major, no match→patch).
func TestConventionalCommitBump(t *testing.T) {
	cases := []struct {
		msg  string
		want string
	}{
		{"feat: add login", bumpMinor},
		{"feat(api): add endpoint", bumpMinor},
		{"fix: handle nil", bumpPatch},
		{"perf: cache lookups", bumpPatch},
		{"feat!: drop v1", bumpMajor},
		{"feat(api)!: drop v1", bumpMajor},
		{"fix: x\n\nBREAKING CHANGE: config moved", bumpMajor},
		{"refactor: y\n\nBREAKING-CHANGE: rename", bumpMajor},
		{"chore: tidy", bumpPatch},
		{"docs: readme", bumpPatch},
		{"no conventional type at all", bumpPatch},
		{"", bumpPatch},
	}
	for _, c := range cases {
		if got := ConventionalCommitBump(c.msg); got != c.want {
			t.Errorf("ConventionalCommitBump(%q) = %q, want %q", c.msg, got, c.want)
		}
	}
}

// §50: the highest bump across the whole commit set wins; empty → patch.
func TestDetermineBump(t *testing.T) {
	cases := []struct {
		msgs []string
		want string
	}{
		{[]string{"fix: a", "feat: b"}, bumpMinor},
		{[]string{"fix: a"}, bumpPatch},
		{[]string{"chore: a", "feat!: b"}, bumpMajor},
		{[]string{"feat: a", "fix!: b"}, bumpMajor},
		{nil, bumpPatch},
	}
	for _, c := range cases {
		if got := DetermineBump(c.msgs); got != c.want {
			t.Errorf("DetermineBump(%v) = %q, want %q", c.msgs, got, c.want)
		}
	}
}

func TestParseVersion(t *testing.T) {
	maj, min, pat, ok := ParseVersion("v1.2.3", "v")
	if !ok || maj != 1 || min != 2 || pat != 3 {
		t.Fatalf("ParseVersion v1.2.3 = %d.%d.%d ok=%v", maj, min, pat, ok)
	}
	for _, bad := range []string{"", "v1.2", "v1", "v1.2.3-beta", "release-1.2.3", "vx.y.z"} {
		if _, _, _, ok := ParseVersion(bad, "v"); ok {
			t.Fatalf("ParseVersion(%q) must fail", bad)
		}
	}
}

// §50: next tag = latest + bump, with empty latest treated as 0.0.0.
func TestNextVersion(t *testing.T) {
	cases := []struct {
		latest, bump, want string
	}{
		{"v1.2.3", bumpMinor, "v1.3.0"},
		{"v1.2.3", bumpPatch, "v1.2.4"},
		{"v1.2.3", bumpMajor, "v2.0.0"},
		{"", bumpMinor, "v0.1.0"},
		{"", bumpPatch, "v0.0.1"},
	}
	for _, c := range cases {
		got, err := NextVersion(c.latest, c.bump, "v")
		if err != nil || got != c.want {
			t.Errorf("NextVersion(%q,%q) = %q err=%v, want %q", c.latest, c.bump, got, err, c.want)
		}
	}
	if _, err := NextVersion("v1.2.3", "banana", "v"); err == nil {
		t.Fatal("unknown bump must fail closed")
	}
}

// §50 AC: the rendered changelog contains the merged PRs since the last tag.
func TestRenderChangelogListsMergedPRs(t *testing.T) {
	out := RenderChangelog("v1.3.0", []MergedPR{
		{Number: 12, Title: "feat: add login"},
		{Number: 15, Title: "fix: crash"},
	}, "2026-09-13")
	for _, want := range []string{"## v1.3.0 - 2026-09-13", "#12 feat: add login", "#15 fix: crash"} {
		if !strings.Contains(out, want) {
			t.Fatalf("changelog missing %q:\n%s", want, out)
		}
	}
}

// §50/§42: disabled release mutates nothing.
func TestRunReleaseDisabledNoop(t *testing.T) {
	useFakeToken(t)
	withTokenRunner(t, func(context.Context, string, ...string) ([]byte, error) {
		t.Fatal("disabled release must not call gh")
		return nil, nil
	})
	res, err := RunRelease(context.Background(), ReleaseRequest{
		Owner: "o", Repo: "r", Config: ReleaseConfig{Enabled: false},
	})
	if err != nil || !res.Skipped || !strings.Contains(res.Reason, "disabled") {
		t.Fatalf("want disabled skip, got %+v err=%v", res, err)
	}
}

// §50/§42: dry-run mutates nothing.
func TestRunReleaseDryRunSkips(t *testing.T) {
	useFakeToken(t)
	withTokenRunner(t, func(context.Context, string, ...string) ([]byte, error) {
		t.Fatal("dry run must not call gh")
		return nil, nil
	})
	res, err := RunRelease(context.Background(), ReleaseRequest{
		Owner: "o", Repo: "r", BaseBranch: "main",
		Config: ReleaseConfig{Enabled: true}, DryRun: true,
	})
	if err != nil || !res.Skipped || res.Reason != "dryRun" {
		t.Fatalf("want dryRun skip, got %+v err=%v", res, err)
	}
}

// releaseFake is a stateful gh seam for the §50 activity: it answers tag/commit/
// PR reads and records the mutations.
type releaseFake struct {
	tags      []string
	commits   []string
	prs       []MergedPR
	existing  string // existing CHANGELOG content ("" = no file)
	released  bool
	release   int
	lastRel   []string
	commitMsg string
	content   string // decoded content written by the PUT
}

func (f *releaseFake) run(_ context.Context, _ string, args ...string) ([]byte, error) {
	line := strings.Join(args, " ")
	switch {
	case strings.HasPrefix(line, "api repos/o/r/tags"):
		type row struct {
			Name string `json:"name"`
		}
		rows := make([]row, 0, len(f.tags))
		for _, t := range f.tags {
			rows = append(rows, row{Name: t})
		}
		return json.Marshal(rows)
	case strings.HasPrefix(line, "api repos/o/r/commits?sha="):
		if f.released {
			return []byte("[]"), nil
		}
		return json.Marshal(commitRows(f.commits))
	case strings.HasPrefix(line, "api repos/o/r/commits/"):
		return []byte(`{"commit":{"committer":{"date":"2026-01-01T00:00:00Z"}}}`), nil
	case strings.HasPrefix(line, "pr list"):
		return json.Marshal(f.prs)
	case strings.HasPrefix(line, "api -X PUT repos/o/r/contents/"):
		for _, a := range args {
			if strings.HasPrefix(a, "content=") {
				if dec, err := base64.StdEncoding.DecodeString(a[len("content="):]); err == nil {
					f.content = string(dec)
				}
			}
			if strings.HasPrefix(a, "message=") {
				f.commitMsg = a[len("message="):]
			}
		}
		return []byte(`{"commit":{"sha":"new"}}`), nil
	case strings.HasPrefix(line, "api repos/o/r/contents/"):
		if f.existing == "" {
			return nil, errors.New("HTTP 404: Not Found")
		}
		return json.Marshal(map[string]string{
			"sha":      "abc",
			"content":  base64.StdEncoding.EncodeToString([]byte(f.existing)),
			"encoding": "base64",
		})
	case strings.HasPrefix(line, "release create"):
		f.release++
		f.released = true
		f.lastRel = args
		f.tags = append(f.tags, args[2])
		return []byte("https://github.com/o/r/releases/tag/" + args[2]), nil
	}
	return nil, fmt.Errorf("unexpected gh call: %s", line)
}

func commitRows(msgs []string) []map[string]any {
	rows := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		rows = append(rows, map[string]any{"commit": map[string]string{"message": m}})
	}
	return rows
}

// §50: an enabled repo gets the right semver tag, a changelog containing the
// merged PRs, and exactly one release; the second run (retry/overlap) is a no-op.
func TestRunReleaseCreatesOnceAndIsIdempotent(t *testing.T) {
	useFakeToken(t)
	f := &releaseFake{
		tags:    []string{"v1.0.0"},
		commits: []string{"feat: add login"},
		prs:     []MergedPR{{Number: 12, Title: "feat: add login", MergedAt: "2026-02-01T00:00:00Z"}},
	}
	withTokenRunner(t, f.run)
	req := ReleaseRequest{
		Owner: "o", Repo: "r", BaseBranch: "main",
		Config: ReleaseConfig{Enabled: true, VersionPrefix: "v"},
	}
	res, err := RunRelease(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Tag != "v1.1.0" || res.Bump != bumpMinor || res.PreviousTag != "v1.0.0" {
		t.Fatalf("unexpected release result: %+v", res)
	}
	if f.release != 1 {
		t.Fatalf("release created %d times, want 1", f.release)
	}
	if !strings.Contains(f.content, "#12 feat: add login") {
		t.Fatalf("committed changelog missing merged PR:\n%s", f.content)
	}
	if !strings.Contains(strings.Join(f.lastRel, " "), "--target main") {
		t.Fatalf("release must target the base branch: %v", f.lastRel)
	}

	// Retry: tag v1.1.0 now exists and there are no new commits → no second release.
	res2, err := RunRelease(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !res2.Skipped || f.release != 1 {
		t.Fatalf("retry must be a no-op: res=%+v releases=%d", res2, f.release)
	}
}

// §50: no commits since the last tag → no spurious patch release.
func TestRunReleaseNoCommitsNoop(t *testing.T) {
	useFakeToken(t)
	f := &releaseFake{tags: []string{"v1.0.0"}}
	withTokenRunner(t, f.run)
	res, err := RunRelease(context.Background(), ReleaseRequest{
		Owner: "o", Repo: "r", BaseBranch: "main", Config: ReleaseConfig{Enabled: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Skipped || f.release != 0 {
		t.Fatalf("no commits must skip with no release: res=%+v releases=%d", res, f.release)
	}
}
