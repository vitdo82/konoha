package konoha

import "testing"

// #141/AC1: an issue releases its claim when it is no longer open or carries a
// human-applied release label. The predicate is pure and fail-safe: only an
// explicitly-closed issue or an explicit release label stops a run, never a
// missing signal.
func TestReleasedClaim(t *testing.T) {
	managed := []string{labelManaged, "bug"}
	cases := []struct {
		name   string
		state  string
		labels []string
		cfg    ReconcileConfig
		want   bool
	}{
		{"open issue keeps its claim", "open", managed, ReconcileConfig{}, false},
		{"closed issue releases", "closed", managed, ReconcileConfig{}, true},
		{"default duplicate label releases", "open", []string{labelManaged, "duplicate"}, ReconcileConfig{}, true},
		{"default wontfix label releases", "open", []string{labelManaged, "wontfix"}, ReconcileConfig{}, true},
		{"unrelated label keeps the claim", "open", []string{labelManaged, "enhancement"}, ReconcileConfig{}, false},
		{"custom label releases", "open", []string{labelManaged, "superseded"}, ReconcileConfig{ReleasedLabels: []string{"superseded"}}, true},
		{"custom list overrides the default", "open", []string{labelManaged, "duplicate"}, ReconcileConfig{ReleasedLabels: []string{"superseded"}}, false},
		{"empty state is not a release", "", managed, ReconcileConfig{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := releasedClaim(c.state, c.labels, c.cfg)
			if got != c.want {
				t.Fatalf("releasedClaim(state=%q, labels=%v) = %v, want %v", c.state, c.labels, got, c.want)
			}
		})
	}
}
