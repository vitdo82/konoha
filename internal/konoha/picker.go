package konoha

import (
	"fmt"
	"strings"
)

// Agent profile picker at cycle start (#145). This is the operator-facing
// selection UX over #117's declarative profiles: list the named profiles (with
// their harness + model) and pick one when starting a cycle, via the picker
// projection or the `--profile` CLI flag.
//
// The choice is operator-supplied and resolved against the `agents:` catalog
// ONCE at start, then flattened into the lifecycle exactly as load does, so it
// lands in outcome.json (`profiles`) and the #88 policy digest. Issue text never
// participates (§27): resolution reads only konoha.yaml/repos.yaml.

// ProfileOption is one row of the picker: a named #117 profile with the
// effective harness + model it selects, so the operator can see the choice.
type ProfileOption struct {
	Name    string `json:"name"`
	Harness string `json:"harness"`
	Model   string `json:"model,omitempty"`
}

// profilePickerOptions projects the `agents:` catalog into the sorted option
// list the picker renders (harness falls back to the effective default). Pure;
// no terminal, no config mutation.
func profilePickerOptions(cfg AppConfig) []ProfileOption {
	def := strings.TrimSpace(cfg.Defaults.Lifecycle.Harness)
	if def == "" {
		def = defaultHarnessName
	}
	out := make([]ProfileOption, 0, len(cfg.Agents))
	for _, name := range sortedProfileNames(cfg.Agents) {
		p := cfg.Agents[name]
		h := strings.TrimSpace(p.Harness)
		if h == "" {
			h = def
		}
		out = append(out, ProfileOption{Name: name, Harness: h, Model: strings.TrimSpace(p.Model)})
	}
	return out
}

// renderProfilePicker is the PURE text projection of the picker: one aligned
// line per profile, `*` marking the selected one. A TUI overlay (#143) and the
// unit tests share it — the interactive layer only moves the cursor, so the
// projection is testable without a real terminal.
func renderProfilePicker(options []ProfileOption, selected string) string {
	if len(options) == 0 {
		return "no agent profiles configured\n"
	}
	w := 0
	for _, o := range options {
		if len(o.Name) > w {
			w = len(o.Name)
		}
	}
	var b strings.Builder
	b.WriteString("Agent profiles:\n")
	for _, o := range options {
		mark := " "
		if o.Name == selected {
			mark = "*"
		}
		model := o.Model
		if model == "" {
			model = "-"
		}
		fmt.Fprintf(&b, "  %s %-*s  harness=%s model=%s\n", mark, w, o.Name, o.Harness, model)
	}
	return b.String()
}

// selectCycleProfile resolves the operator's profile choice for one repo's
// cycle start. An empty name keeps the repo's existing (default) selection; a
// non-empty name must exist in the operator's `agents:` catalog or it fails
// closed (an unknown profile is never adopted). The chosen profile becomes the
// repo-level default, then #117 resolves it once — flattening harness/model/
// limits, recording the stage→profile map, and computing the content digest —
// so the recorded lifecycle matches the scheduled path byte-for-byte.
func selectCycleProfile(cfg AppConfig, repo RepoConfig, name string) (LifecycleConfig, error) {
	var lc LifecycleConfig
	if repo.Lifecycle != nil {
		lc = *repo.Lifecycle
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return lc, nil // no pick: the repos.yaml selection (or legacy path) stands
	}
	if _, ok := cfg.Agents[name]; !ok {
		return LifecycleConfig{}, fmt.Errorf("unknown agent profile %q (known: %s)", name, strings.Join(sortedProfileNames(cfg.Agents), ", "))
	}
	if lc.Agents == nil {
		lc.Agents = map[string]string{}
	}
	lc.Agents[profileDefaultStage] = name
	repos := []RepoConfig{{Owner: repo.Owner, Repo: repo.Repo, RepoPath: repo.RepoPath, Lifecycle: &lc}}
	if err := resolveAgentProfiles(repos, cfg); err != nil {
		return LifecycleConfig{}, err
	}
	return lc, nil
}
