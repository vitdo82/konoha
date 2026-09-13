package konoha

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// §51 deployment + post-deploy verification. After §50 publishes a release tag,
// the post-merge workflow runs the repo's deploy hook against that tag and then
// a bounded smoke/health check. A permanent verification failure is never a
// silent success: it emits a rollback signal and escalates to HUMAN_REQUIRED
// (§40). Both stages are opt-in (`lifecycle.deploy.enabled`) and honor `DryRun`
// (§42); the deploy is guard-railed by the released tag so a retry never
// re-deploys the same release (§24).
//
// All I/O lives in the DeployRelease / VerifyDeployment activities. The
// PostMergeWorkflow only calls them (deterministic).

// DeployConfig is the per-repo §51 deploy + verify policy (repos.yaml
// `lifecycle.deploy`). RollbackOnFail is a pointer so "unset" is distinguishable
// from an explicit false: unset falls back to the configured default (true).
type DeployConfig struct {
	Enabled        bool   `yaml:"enabled" json:"enabled,omitempty"`
	Command        string `yaml:"command" json:"command,omitempty"`               // deploy hook, run with cwd=repoPath
	Env            string `yaml:"env" json:"env,omitempty"`                       // deployment environment (GitHub deployments + KONOHA_DEPLOY_ENV)
	Verify         string `yaml:"verify" json:"verify,omitempty"`                 // smoke/health command; exit 0 = healthy
	MaxWait        string `yaml:"maxWait" json:"maxWait,omitempty"`               // bounded verify poll window (Go duration)
	RollbackOnFail *bool  `yaml:"rollbackOnFail" json:"rollbackOnFail,omitempty"` // emit a rollback signal on failure
}

const (
	defaultDeployEnv   = "production"
	defaultDeployWait  = 5 * time.Minute
	deployPollInterval = 5 * time.Second
)

// validate gates the §51 deploy block: an enabled deploy needs a command, the
// verify window (when set) is a positive duration, and the environment name is a
// safe fragment.
func (dc DeployConfig) validate(prefix string) error {
	if dc.Enabled && strings.TrimSpace(dc.Command) == "" {
		return fmt.Errorf("%s.command: required when deploy.enabled", prefix)
	}
	if dc.MaxWait != "" {
		if d, err := time.ParseDuration(dc.MaxWait); err != nil || d <= 0 {
			return fmt.Errorf("%s.maxWait: %q is not a positive duration", prefix, dc.MaxWait)
		}
	}
	if strings.ContainsAny(dc.Env, " \t\n/") || strings.Contains(dc.Env, "..") {
		return fmt.Errorf("%s.env: %q contains invalid characters", prefix, dc.Env)
	}
	return nil
}

// DeployEnabled reports whether the repo opted into §51 deploy automation.
// Absent/false is a no-op everywhere (config-driven, fail-closed).
func DeployEnabled(lc LifecycleConfig) bool { return lc.Deploy.Enabled }

// DeployEnvironment resolves the deployment environment name (default
// "production"), used for the GitHub deployment record and KONOHA_DEPLOY_ENV.
func DeployEnvironment(lc LifecycleConfig) string {
	if strings.TrimSpace(lc.Deploy.Env) != "" {
		return lc.Deploy.Env
	}
	return defaultDeployEnv
}

// DeployMaxWait resolves the §51 bounded verification poll window.
func DeployMaxWait(lc LifecycleConfig) time.Duration {
	if d, err := time.ParseDuration(lc.Deploy.MaxWait); err == nil && d > 0 {
		return d
	}
	if d, err := time.ParseDuration(appCfg.Defaults.Lifecycle.Deploy.MaxWait); err == nil && d > 0 {
		return d
	}
	return defaultDeployWait
}

// RollbackOnFail resolves whether a failed deploy/verify emits a rollback
// signal. An explicit per-repo value wins; otherwise the configured default;
// otherwise true (fail-closed: surface the rollback recommendation).
func RollbackOnFail(lc LifecycleConfig) bool {
	if lc.Deploy.RollbackOnFail != nil {
		return *lc.Deploy.RollbackOnFail
	}
	if d := appCfg.Defaults.Lifecycle.Deploy.RollbackOnFail; d != nil {
		return *d
	}
	return true
}

// DeployRequest parameterizes DeployRelease. Tag is the §50 release tag the
// deploy targets (empty → nothing to deploy, fail-closed skip).
type DeployRequest struct {
	Owner    string       `json:"owner"`
	Repo     string       `json:"repo"`
	RepoPath string       `json:"repoPath"`
	Tag      string       `json:"tag,omitempty"`
	Config   DeployConfig `json:"config,omitempty"`
	DryRun   bool         `json:"dryRun,omitempty"`
}

// DeployResult reports the deploy outcome. Skipped covers disabled, dry-run, and
// "tag already successfully deployed" (idempotent no-op). Rollback is the §51
// recommendation emitted when the command fails.
type DeployResult struct {
	Skipped      bool   `json:"skipped,omitempty"`
	Reason       string `json:"reason,omitempty"`
	Deployed     bool   `json:"deployed,omitempty"`
	Rollback     bool   `json:"rollback,omitempty"`
	Tag          string `json:"tag,omitempty"`
	Environment  string `json:"environment,omitempty"`
	DeploymentID int64  `json:"deploymentId,omitempty"`
	Detail       string `json:"detail,omitempty"`
}

// DeployRelease is the §51 deploy activity: it records a GitHub deployment for
// the released tag (the tag-based guard: a tag whose latest deployment already
// succeeded is never re-deployed, §24) and runs the configured hook from the
// main checkout — never inside the ephemeral agent worktree — with the tag and
// environment exported. A failing hook returns Rollback=true (no error: the
// failure is a business outcome, retried verification is the activity's job),
// and marks the deployment failed.
func DeployRelease(ctx context.Context, in DeployRequest) (DeployResult, error) {
	if !in.Config.Enabled {
		return DeployResult{Skipped: true, Reason: "deploy disabled"}, nil
	}
	if in.DryRun {
		return DeployResult{Skipped: true, Reason: "dryRun"}, nil
	}
	if strings.TrimSpace(in.Tag) == "" {
		return DeployResult{Skipped: true, Reason: "no release tag"}, nil
	}
	env := in.Config.Env
	if strings.TrimSpace(env) == "" {
		env = defaultDeployEnv
	}
	tok, err := appToken(ctx)
	if err != nil {
		return DeployResult{}, err
	}
	repo := in.Owner + "/" + in.Repo

	// §24 tag guard: a tag whose latest deployment already succeeded is done.
	state, _, found, err := deploymentState(ctx, tok, repo, in.Tag)
	if err != nil {
		return DeployResult{}, err
	}
	if found && state == deployStateSuccess {
		return DeployResult{Skipped: true, Reason: "tag " + in.Tag + " already deployed", Tag: in.Tag}, nil
	}

	id, err := createDeployment(ctx, tok, repo, in.Tag, env)
	if err != nil {
		return DeployResult{}, err
	}
	if out, err := runDeployHook(ctx, in, env); err != nil {
		detail := fmt.Sprintf("deploy hook failed: %s: %s", strings.TrimSpace(string(out)), err)
		if err := setDeploymentStatus(ctx, tok, repo, id, deployStateFailure, "deploy failed: "+truncate(detail, 120)); err != nil {
			return DeployResult{}, err
		}
		return DeployResult{Rollback: true, Tag: in.Tag, Environment: env, DeploymentID: id, Detail: detail}, nil
	}
	if err := setDeploymentStatus(ctx, tok, repo, id, deployStateSuccess, "deployed "+in.Tag); err != nil {
		return DeployResult{}, err
	}
	return DeployResult{Deployed: true, Tag: in.Tag, Environment: env, DeploymentID: id}, nil
}

// runDeployHook runs the configured deploy command in the main checkout with the
// release tag + environment exported. It is a plain `sh -c` so the hook is
// config-driven (a CD webhook, a container push, a git checkout of the tag…).
func runDeployHook(ctx context.Context, in DeployRequest, env string) ([]byte, error) {
	c := exec.CommandContext(ctx, "sh", "-c", in.Config.Command)
	c.Dir = in.RepoPath
	c.Env = append(append([]string{}, os.Environ()...),
		"KONOHA_RELEASE_TAG="+in.Tag, "KONOHA_DEPLOY_ENV="+env)
	return c.CombinedOutput()
}

// VerifyRequest parameterizes VerifyDeployment. DeploymentID (from DeployResult)
// lets a permanent failure mark the deployment for rollback.
type VerifyRequest struct {
	Owner          string       `json:"owner"`
	Repo           string       `json:"repo"`
	RepoPath       string       `json:"repoPath"`
	Tag            string       `json:"tag,omitempty"`
	DeploymentID   int64        `json:"deploymentId,omitempty"`
	Config         DeployConfig `json:"config,omitempty"`
	RollbackOnFail bool         `json:"rollbackOnFail,omitempty"`
	DryRun         bool         `json:"dryRun,omitempty"`
}

// VerifyResult is the §51 smoke/health verdict. Skipped means no verify command
// was configured (deploy success is terminal); Verified is only true when the
// command exited 0.
type VerifyResult struct {
	Skipped  bool   `json:"skipped,omitempty"`
	Verified bool   `json:"verified,omitempty"`
	Rollback bool   `json:"rollback,omitempty"`
	Detail   string `json:"detail,omitempty"`
	Attempts int    `json:"attempts,omitempty"`
	MaxWait  string `json:"maxWait,omitempty"`
}

// VerifyDeployment runs the configured smoke/health command with bounded
// retries until it exits 0 or the window expires. A command failure is retried
// (transient) until MaxWait; expiry is a permanent failure → Verified=false,
// never a silent success (§40). When RollbackOnFail, the deployment is marked
// inactive as the rollback signal.
func VerifyDeployment(ctx context.Context, in VerifyRequest) (VerifyResult, error) {
	if !in.Config.Enabled {
		return VerifyResult{Skipped: true, Detail: "deploy disabled"}, nil
	}
	if in.DryRun {
		return VerifyResult{Skipped: true, Detail: "dryRun"}, nil
	}
	if strings.TrimSpace(in.Config.Verify) == "" {
		return VerifyResult{Skipped: true, Detail: "no verify configured"}, nil
	}
	maxWait := defaultDeployWait
	if d, err := time.ParseDuration(in.Config.MaxWait); err == nil && d > 0 {
		maxWait = d
	}
	deadline := time.Now().Add(maxWait)
	attempts := 0
	detail := ""
	for {
		attempts++
		out, err := runVerify(ctx, in)
		if err == nil {
			return VerifyResult{Verified: true, Attempts: attempts, Detail: "verify passed"}, nil
		}
		detail = fmt.Sprintf("verify failed: %s: %s", strings.TrimSpace(string(out)), err)
		rem := time.Until(deadline)
		if rem <= 0 {
			res := VerifyResult{Verified: false, Attempts: attempts,
				MaxWait: maxWait.String(), Detail: detail + " (permanent: window expired)"}
			if in.RollbackOnFail {
				res.Rollback = true
				if in.DeploymentID > 0 {
					tok, e := appToken(ctx)
					if e != nil {
						return VerifyResult{}, e
					}
					if e := setDeploymentStatus(ctx, tok, in.Owner+"/"+in.Repo, in.DeploymentID,
						deployStateInactive, "rollback recommended: post-deploy verification failed"); e != nil {
						return VerifyResult{}, e
					}
				}
			}
			return res, nil
		}
		if rem > deployPollInterval {
			rem = deployPollInterval
		}
		select {
		case <-ctx.Done():
			return VerifyResult{}, ctx.Err()
		case <-time.After(rem):
		}
	}
}

// runVerify runs the configured verify command in the main checkout with the
// release tag + environment exported.
func runVerify(ctx context.Context, in VerifyRequest) ([]byte, error) {
	c := exec.CommandContext(ctx, "sh", "-c", in.Config.Verify)
	c.Dir = in.RepoPath
	env := in.Config.Env
	if strings.TrimSpace(env) == "" {
		env = defaultDeployEnv
	}
	c.Env = append(append([]string{}, os.Environ()...),
		"KONOHA_RELEASE_TAG="+in.Tag, "KONOHA_DEPLOY_ENV="+env)
	return c.CombinedOutput()
}

// §51 GitHub deployment states (the tag-based idempotency + rollback signal).
const (
	deployStateSuccess  = "success"
	deployStateFailure  = "failure"
	deployStateInactive = "inactive"
)

// deploymentState returns the latest deployment status for a ref. found=false
// means the ref has never been deployed.
func deploymentState(ctx context.Context, tok, repo, ref string) (state string, id int64, found bool, err error) {
	b, err := ghTokenRunner(ctx, tok, "api", "repos/"+repo+"/deployments?ref="+ref+"&per_page=1")
	if err != nil {
		return "", 0, false, fmt.Errorf("list deployments %s@%s: %s: %w", repo, ref, strings.TrimSpace(string(b)), err)
	}
	var rows []struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(b, &rows); err != nil {
		return "", 0, false, fmt.Errorf("parse deployments %s@%s: %w", repo, ref, err)
	}
	if len(rows) == 0 {
		return "", 0, false, nil
	}
	id = rows[0].ID
	sb, err := ghTokenRunner(ctx, tok, "api", "repos/"+repo+"/deployments/"+strconv.FormatInt(id, 10)+"/statuses?per_page=1")
	if err != nil {
		return "", id, true, fmt.Errorf("deployment status %s/%d: %s: %w", repo, id, strings.TrimSpace(string(sb)), err)
	}
	var sts []struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal(sb, &sts); err != nil {
		return "", id, true, fmt.Errorf("parse deployment status %s/%d: %w", repo, id, err)
	}
	if len(sts) == 0 {
		return "", id, true, nil
	}
	return sts[0].State, id, true, nil
}

// createDeployment records a new GitHub deployment for the ref + environment and
// returns its id.
func createDeployment(ctx context.Context, tok, repo, ref, environment string) (int64, error) {
	b, err := ghTokenRunner(ctx, tok, "api", "-X", "POST", "repos/"+repo+"/deployments",
		"-f", "ref="+ref, "-f", "environment="+environment, "-f", "auto_merge=false")
	if err != nil {
		return 0, fmt.Errorf("create deployment %s@%s: %s: %w", repo, ref, strings.TrimSpace(string(b)), err)
	}
	var doc struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return 0, fmt.Errorf("parse deployment %s@%s: %w", repo, ref, err)
	}
	return doc.ID, nil
}

// setDeploymentStatus posts a deployment status (the human-visible deploy +
// rollback signal).
func setDeploymentStatus(ctx context.Context, tok, repo string, id int64, state, description string) error {
	b, err := ghTokenRunner(ctx, tok, "api", "-X", "POST",
		"repos/"+repo+"/deployments/"+strconv.FormatInt(id, 10)+"/statuses",
		"-f", "state="+state, "-f", "description="+description)
	if err != nil {
		return fmt.Errorf("deployment status %s/%d %s: %s: %w", repo, id, state, strings.TrimSpace(string(b)), err)
	}
	return nil
}

// truncate caps a string at n bytes for a deployment description.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
