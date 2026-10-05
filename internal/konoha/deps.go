package konoha

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"go.temporal.io/sdk/workflow"
)

// §52 dependency + security automation. A schedule-driven DependencyWorkflow
// scans a repo for outdated/vulnerable dependencies (config `scanCmd`), then
// files one idempotent issue per actionable finding. The issue carries the
// `konoha:dependency` label (and `konoha:security` for a known vulnerability),
// so the §20 risk gate escalates it to a human before any PR is published. The
// issue itself then flows through the normal IssueCycleWorkflow via the existing
// sweep/webhook — Konoha never starts that cycle here, so there is no coupling
// and no double-start.
//
// All I/O lives in ScanDependencies / EnsureDependencyIssue; DependencyWorkflow
// is deterministic. Disabled/absent config is a no-op (no scan, no issues, §42
// honors DryRun for every mutation).

// DependencyConfig is the per-repo §52 policy (repos.yaml `lifecycle.deps`).
// AutoFileIssues is a pointer so "unset" (file issues when enabled) is
// distinguishable from an explicit false (scan/report only).
type DependencyConfig struct {
	Enabled        bool   `yaml:"enabled" json:"enabled,omitempty"`
	ScanCmd        string `yaml:"scanCmd" json:"scanCmd,omitempty"`   // must print the JSON finding array
	Schedule       string `yaml:"schedule" json:"schedule,omitempty"` // per-repo schedule interval (Go duration)
	Label          string `yaml:"label" json:"label,omitempty"`       // base label (default konoha:dependency)
	AutoFileIssues *bool  `yaml:"autoFileIssues" json:"autoFileIssues,omitempty"`
}

const defaultDependencySchedule = "24h"

// validate gates the §52 deps block: an enabled scan needs a command and the
// schedule (when set) is a positive duration. Unknown keys fail at decode.
func (dc DependencyConfig) validate(prefix string) error {
	if dc.Enabled && strings.TrimSpace(dc.ScanCmd) == "" {
		return fmt.Errorf("%s.scanCmd: required when deps.enabled", prefix)
	}
	if dc.Schedule != "" {
		d, err := time.ParseDuration(dc.Schedule)
		if err != nil || d <= 0 {
			return fmt.Errorf("%s.schedule: %q is not a positive duration", prefix, dc.Schedule)
		}
	}
	return nil
}

// DependencyEnabled reports whether the repo opted into §52 dependency
// automation. Absent/false is a no-op everywhere.
func DependencyEnabled(lc LifecycleConfig) bool { return lc.Deps.Enabled }

// DependencyScanCmd returns the configured scan command (trimmed).
func DependencyScanCmd(lc LifecycleConfig) string { return strings.TrimSpace(lc.Deps.ScanCmd) }

// DependencyLabel resolves the base label applied to filed dependency issues.
func DependencyLabel(lc LifecycleConfig) string {
	if l := strings.TrimSpace(lc.Deps.Label); l != "" {
		return l
	}
	return labelDependency
}

// DependencySchedule resolves the per-repo scan cadence (Go duration string).
func DependencySchedule(lc LifecycleConfig) string {
	if s := strings.TrimSpace(lc.Deps.Schedule); s != "" {
		return s
	}
	return defaultDependencySchedule
}

// AutoFileDependencyIssues resolves whether scan findings become GitHub issues.
// Enabled + unset → true (the point of the feature); an explicit false keeps the
// scan report-only.
func AutoFileDependencyIssues(lc LifecycleConfig) bool {
	if lc.Deps.AutoFileIssues != nil {
		return *lc.Deps.AutoFileIssues
	}
	return true
}

// DependencyFinding is one actionable scan result. A non-empty ID marks a known
// vulnerability (advisory/CVE); otherwise it is an outdated-version bump.
type DependencyFinding struct {
	Package  string `json:"package"`
	Version  string `json:"version"`
	ID       string `json:"id"`
	Severity string `json:"severity"`
	Summary  string `json:"summary"`
}

// Vulnerability reports whether the finding is a known vulnerability.
func (f DependencyFinding) Vulnerability() bool { return strings.TrimSpace(f.ID) != "" }

// Identity is the deterministic marker key: <package>@<id|version>.
func (f DependencyFinding) Identity() string {
	if id := strings.TrimSpace(f.ID); id != "" {
		return f.Package + "@" + id
	}
	return f.Package + "@" + strings.TrimSpace(f.Version)
}

// DependencyReport is the validated scan result.
type DependencyReport struct {
	Findings []DependencyFinding `json:"findings"`
}

// DependencyScanRequest parameterizes ScanDependencies.
type DependencyScanRequest struct {
	Owner    string `json:"owner"`
	Repo     string `json:"repo"`
	RepoPath string `json:"repoPath"`
	ScanCmd  string `json:"scanCmd"`
}

// ScanDependencies runs the configured scan command from the repo checkout and
// parses its output into a validated report. The contract is a JSON array (or an
// object with `findings`) of {package,version,id,severity,summary}. A command
// failure or a malformed entry is an error (fail-closed, §36/§40): a broken scan
// never masquerades as "no findings".
func ScanDependencies(ctx context.Context, in DependencyScanRequest) (DependencyReport, error) {
	cmd := strings.TrimSpace(in.ScanCmd)
	if cmd == "" {
		return DependencyReport{}, fmt.Errorf("ScanDependencies: no scanCmd configured")
	}
	c := exec.CommandContext(ctx, "sh", "-c", cmd)
	c.Dir = in.RepoPath
	out, err := c.CombinedOutput()
	if err != nil {
		return DependencyReport{}, fmt.Errorf("dependency scan %q failed: %s: %w", cmd, strings.TrimSpace(string(out)), err)
	}
	report, err := parseDependencyReport(out)
	if err != nil {
		return DependencyReport{}, fmt.Errorf("dependency scan %q: %w", cmd, err)
	}
	return report, nil
}

// parseDependencyReport parses scan output and normalizes/validates each entry.
// Empty output means no findings. Malformed JSON or an entry missing
// package/version|id fails closed.
func parseDependencyReport(out []byte) (DependencyReport, error) {
	s := strings.TrimSpace(string(out))
	if s == "" {
		return DependencyReport{}, nil
	}
	var findings []DependencyFinding
	if err := json.Unmarshal([]byte(s), &findings); err != nil {
		var doc struct {
			Findings []DependencyFinding `json:"findings"`
		}
		if err2 := json.Unmarshal([]byte(s), &doc); err2 != nil {
			return DependencyReport{}, fmt.Errorf("output is not a JSON finding array: %v", err)
		}
		findings = doc.Findings
	}
	for i := range findings {
		f := &findings[i]
		f.Package = strings.TrimSpace(f.Package)
		f.Version = strings.TrimSpace(f.Version)
		f.ID = strings.TrimSpace(f.ID)
		f.Severity = normalizeSeverity(f.Severity)
		f.Summary = strings.TrimSpace(f.Summary)
		if f.Package == "" || (f.Version == "" && f.ID == "") {
			return DependencyReport{}, fmt.Errorf("finding %d invalid: package and version|id are required", i)
		}
	}
	return DependencyReport{Findings: findings}, nil
}

// normalizeSeverity maps a scanner severity onto the controlled set, defaulting
// to "unknown" (fail-closed: never silently "low").
func normalizeSeverity(s string) string {
	switch v := strings.ToLower(strings.TrimSpace(s)); v {
	case "critical", "high", "medium", "low":
		return v
	default:
		return "unknown"
	}
}

// depMarkerOpen is the §24 idempotency marker for a dependency issue:
// `<!-- konoha-dep: <package>@<id|version> -->`, matched byte-exactly.
const depMarkerOpen = "<!-- konoha-dep: "

func depMarker(f DependencyFinding) string {
	return depMarkerOpen + f.Identity() + " -->"
}

// DepIssueRequest targets one finding's idempotent issue.
type DepIssueRequest struct {
	Owner     string            `json:"owner"`
	Repo      string            `json:"repo"`
	Finding   DependencyFinding `json:"finding"`
	BaseLabel string            `json:"baseLabel,omitempty"`
	DryRun    bool              `json:"dryRun,omitempty"`
}

// DepIssueResult reports the filed (or matched) issue. Number is 0 under DryRun.
type DepIssueResult struct {
	Number  int    `json:"number,omitempty"`
	Created bool   `json:"created,omitempty"`
	Marker  string `json:"marker,omitempty"`
}

// EnsureDependencyIssue files one issue per finding, idempotent under retry
// (§24): it scans repo issues for the deterministic marker first and returns the
// existing number instead of creating a duplicate. A vulnerability also gets the
// built-in `konoha:security` label so the §20 risk gate blocks its auto-merge.
func EnsureDependencyIssue(ctx context.Context, in DepIssueRequest) (DepIssueResult, error) {
	if in.DryRun {
		return DepIssueResult{}, nil // §42: shadow mode mutates nothing
	}
	marker := depMarker(in.Finding)
	items, err := listIssues(ctx, in.Owner, in.Repo)
	if err != nil {
		return DepIssueResult{}, err
	}
	if n := findMarkerIssue(items, marker); n != 0 {
		return DepIssueResult{Number: n, Marker: marker}, nil
	}
	labels := []string{in.BaseLabel}
	if in.Finding.Vulnerability() {
		labels = append(labels, labelSecurity)
	}
	args := []string{"issue", "create", "-R", in.Owner + "/" + in.Repo,
		"--title", depIssueTitle(in.Finding), "--body", depIssueBody(in.Finding, marker)}
	for _, l := range dedupeLabels(labels) {
		if l == "" {
			continue
		}
		args = append(args, "--label", l)
	}
	out, err := ghRunner(ctx, args...)
	if err != nil {
		return DepIssueResult{}, fmt.Errorf("gh issue create (dependency): %s: %w", strings.TrimSpace(string(out)), err)
	}
	n, err := parseIssueNumber(string(out))
	if err != nil {
		return DepIssueResult{}, err
	}
	return DepIssueResult{Number: n, Created: true, Marker: marker}, nil
}

// findMarkerIssue returns the first non-PR issue whose body carries the marker.
func findMarkerIssue(items []ghItem, marker string) int {
	for _, it := range items {
		if isPR(it) {
			continue
		}
		if strings.Contains(it.Body, marker) {
			return it.Number
		}
	}
	return 0
}

// depIssueTitle renders a deterministic issue title per finding kind.
func depIssueTitle(f DependencyFinding) string {
	if f.Vulnerability() {
		return fmt.Sprintf("Security: update %s (%s)", f.Package, f.ID)
	}
	return fmt.Sprintf("Dependency: update %s to %s", f.Package, f.Version)
}

// depIssueBody renders the issue body with the idempotency marker first.
func depIssueBody(f DependencyFinding, marker string) string {
	var b strings.Builder
	b.WriteString(marker + "\n\n")
	fmt.Fprintf(&b, "Package: `%s`\n", f.Package)
	if f.Version != "" {
		fmt.Fprintf(&b, "Current version: `%s`\n", f.Version)
	}
	if f.ID != "" {
		fmt.Fprintf(&b, "Advisory: `%s`\n", f.ID)
		fmt.Fprintf(&b, "Severity: `%s`\n", f.Severity)
	}
	if f.Summary != "" {
		fmt.Fprintf(&b, "\n%s\n", f.Summary)
	}
	b.WriteString("\nFiled by Konoha dependency automation (§52). This issue runs through the normal lifecycle.")
	if f.Vulnerability() {
		b.WriteString(" The `konoha:security` label forces human sign-off before any merge.")
	}
	b.WriteString("\n")
	return b.String()
}

// DependencyWorkflow is the schedule-driven §52 entrypoint: scan the repo and
// file one idempotent issue per actionable finding. Disabled config is a no-op.
// The filed issues then flow through IssueCycleWorkflow via the existing
// sweep/webhook — this workflow never starts that cycle, so overlapping schedule
// runs (marker dedupe) and the sweep never double-start.
func DependencyWorkflow(ctx workflow.Context, scope RepoScope) error {
	lc := scope.Lifecycle
	if !DependencyEnabled(lc) {
		return nil
	}
	ctx = workflow.WithActivityOptions(ctx, activityOptionsFor(retryInfrastructure, appCfg.Worker.startToClose()))

	var report DependencyReport
	if err := workflow.ExecuteActivity(ctx, ScanDependencies, DependencyScanRequest{
		Owner: scope.Owner, Repo: scope.Repo, RepoPath: scope.RepoPath,
		ScanCmd: DependencyScanCmd(lc),
	}).Get(ctx, &report); err != nil {
		return err
	}
	workflow.GetLogger(ctx).Info("dependency scan",
		"repo", scope.Owner+"/"+scope.Repo, "findings", len(report.Findings))

	if !AutoFileDependencyIssues(lc) {
		return nil
	}
	// §21: the labels we are about to apply must exist (idempotent create).
	if err := workflow.ExecuteActivity(ctx, EnsureRepoLabels, LabelsTheirsRequest{
		Owner: scope.Owner, Repo: scope.Repo,
		Labels: dedupeLabels([]string{DependencyLabel(lc), labelSecurity}),
		DryRun: lc.DryRun,
	}).Get(ctx, nil); err != nil {
		return err
	}
	for _, f := range report.Findings {
		var res DepIssueResult
		if err := workflow.ExecuteActivity(ctx, EnsureDependencyIssue, DepIssueRequest{
			Owner: scope.Owner, Repo: scope.Repo, Finding: f,
			BaseLabel: DependencyLabel(lc), DryRun: lc.DryRun,
		}).Get(ctx, &res); err != nil {
			return err
		}
		workflow.GetLogger(ctx).Info("dependency issue",
			"package", f.Package, "issue", res.Number, "created", res.Created)
	}
	return nil
}
