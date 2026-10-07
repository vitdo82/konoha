package konoha

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"

	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
)

func Main() {
	// Pure-CLI commands that need no Temporal connection.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "config":
			if err := runConfig(os.Args[2:]); err != nil {
				log.Fatal(err)
			}
			return
		case "doctor":
			// #99: preflight runs before bootstrap so a broken konoha.yaml is
			// reported by the doctor itself, not a bare config fatal.
			if err := runDoctor(); err != nil {
				log.Fatal(err)
			}
			return
		case "publish":
			if err := runPublish(); err != nil {
				log.Fatal(err)
			}
			return
		case "analyze":
			if err := runAnalyze(); err != nil {
				log.Fatal(err)
			}
			return
		case "estimate":
			if err := runEstimate(); err != nil {
				log.Fatal(err)
			}
			return
		case "develop":
			if err := runDevelop(); err != nil {
				log.Fatal(err)
			}
			return
		case "review":
			if err := runReview(); err != nil {
				log.Fatal(err)
			}
			return
		case "repair":
			if err := runRepair(); err != nil {
				log.Fatal(err)
			}
			return
		case "complete":
			if err := runComplete(); err != nil {
				log.Fatal(err)
			}
			return
		case "resolve":
			if err := runResolve(); err != nil {
				log.Fatal(err)
			}
			return
		case "po":
			if err := runPO(); err != nil {
				log.Fatal(err)
			}
			return
		case "split":
			if err := runSplit(); err != nil {
				log.Fatal(err)
			}
			return
		case "diagnose":
			if err := runDiagnose(); err != nil {
				log.Fatal(err)
			}
			return
		case "labels":
			if err := runLabels(); err != nil {
				log.Fatal(err)
			}
			return
		case "children":
			if err := runChildren(); err != nil {
				log.Fatal(err)
			}
			return
		case "release":
			if err := runRelease(); err != nil {
				log.Fatal(err)
			}
			return
		case "deps":
			if err := runDeps(); err != nil {
				log.Fatal(err)
			}
			return
		case "console":
			// #105: loopback-only read-only console. Dispatched BEFORE the worker
			// bootstrap (and registerOperatorSecrets) so this process reads no
			// GitHub credential; runConsole loads konoha.yaml itself.
			if err := runConsole(); err != nil {
				log.Fatal(err)
			}
			return
		case "mcp":
			// #116: loopback-only MCP surface (read-only by default). Dispatched
			// BEFORE the worker bootstrap so this process reads no GitHub
			// credential; runMCP loads konoha.yaml itself.
			if err := runMCP(); err != nil {
				log.Fatal(err)
			}
			return
		case "api":
			// #124: the generated console HTTP client (no Temporal, no GitHub
			// credential). Talks to a running console via KONOHA_CONSOLE_ADDR.
			if err := runConsoleAPI(os.Args[2:]); err != nil {
				log.Fatal(err)
			}
			return
		case "token":
			t, err := appToken(context.Background())
			if err != nil {
				log.Fatal("appToken:", err)
			}
			fmt.Println(t[:12] + "..." + t[len(t)-8:])
			return
		}
	}

	// Bootstrap application config (#48): konoha.yaml (strict) merged over
	// bundled defaults, then env overrides (env > file > default). Fixed for the
	// process lifetime so workflow/activity replay sees a stable value.
	if cfg, err := loadAppConfig(defaultAppConfigPath()); err != nil {
		log.Fatalf("config: %v", err)
	} else {
		appCfg = cfg
	}
	applyEnvOverrides()
	registerOperatorSecrets() // §23: mask operator-provisioned credentials in captured output

	// #83/#97/#99: read-only inspection surface (reads evidence.dir; no Temporal).
	if len(os.Args) > 1 {
		switch {
		case os.Args[1] == "get" && len(os.Args) > 2 && os.Args[2] == "changes":
			if err := runGetChanges(); err != nil {
				log.Fatal(err)
			}
			return
		case os.Args[1] == "describe" && len(os.Args) > 2 && os.Args[2] == "change":
			if err := runDescribeChange(os.Args[3:]); err != nil {
				log.Fatal(err)
			}
			return
		case os.Args[1] == "describe" && len(os.Args) > 2:
			// #99: summary of one cycle's outcome.json (no Temporal). A bare
			// `describe` keeps its prior behavior (falls through to the worker).
			if err := runDescribeOutcome(os.Args[2:], os.Stdout); err != nil {
				log.Fatal(err)
			}
			return
		case os.Args[1] == "logs":
			// #99: print one cycle's agent/validation evidence (no Temporal).
			if err := runLogs(os.Args[2:], os.Stdout); err != nil {
				log.Fatal(err)
			}
			return
		}
	}

	addr := os.Getenv("TEMPORAL_ADDRESS")
	if addr == "" {
		addr = "127.0.0.1:7233"
	}
	c, err := client.Dial(client.Options{HostPort: addr})
	if err != nil {
		log.Fatalf("dial temporal %s: %v", addr, err)
	}
	defer c.Close()

	// #89: print the running cycle's status snapshot as JSON.
	if len(os.Args) > 1 && os.Args[1] == "status" {
		if err := runStatus(c, os.Args[2:]); err != nil {
			log.Fatal(err)
		}
		return
	}

	w := worker.New(c, appCfg.Worker.TaskQueue, worker.Options{
		MaxConcurrentActivityExecutionSize: workerMaxConcurrentActivities(),
	})
	w.RegisterWorkflowWithOptions(PrReviewWorkflow, workflow.RegisterOptions{Name: "PrReviewWorkflow"})
	w.RegisterWorkflowWithOptions(ReviewAllWorkflow, workflow.RegisterOptions{Name: "ReviewAllWorkflow"})
	w.RegisterWorkflowWithOptions(IssueCycleWorkflow, workflow.RegisterOptions{Name: "IssueCycleWorkflow"})
	w.RegisterWorkflowWithOptions(IssueAllWorkflow, workflow.RegisterOptions{Name: "IssueAllWorkflow"})
	w.RegisterWorkflowWithOptions(PostMergeWorkflow, workflow.RegisterOptions{Name: "PostMergeWorkflow"})
	w.RegisterWorkflowWithOptions(DependencyWorkflow, workflow.RegisterOptions{Name: "DependencyWorkflow"})
	w.RegisterWorkflowWithOptions(BacklogGroomWorkflow, workflow.RegisterOptions{Name: "BacklogGroomWorkflow"})
	w.RegisterWorkflowWithOptions(AlertWorkflow, workflow.RegisterOptions{Name: "AlertWorkflow"})
	w.RegisterWorkflowWithOptions(FailureFeedbackWorkflow, workflow.RegisterOptions{Name: "FailureFeedbackWorkflow"})
	w.RegisterActivity(FetchDiff)
	w.RegisterActivity(FetchPRHead)
	w.RegisterActivity(FetchPRDiff)
	w.RegisterActivity(ReviewWithOpencode)
	w.RegisterActivity(SubmitReview)
	w.RegisterActivity(ListUnreviewedPRs)
	w.RegisterActivity(ListUnmanagedIssues)
	w.RegisterActivity(AnalyzeIssue)
	w.RegisterActivity(EstimateIssue)
	w.RegisterActivity(RunDeveloper)
	w.RegisterActivity(RunReviewer)
	w.RegisterActivity(RunRepair)
	w.RegisterActivity(RunCompletion)
	w.RegisterActivity(RunPRDescription) // #115: AI-generated PR description
	w.RegisterActivity(RunResolution)
	w.RegisterActivity(RunSplit)
	w.RegisterActivity(RunDiagnosis)
	w.RegisterActivity(EnsureRepoLabels)
	w.RegisterActivity(ApplyIssueLabels)
	w.RegisterActivity(EnsureManagedNote)
	w.RegisterActivity(EnsureChildren)
	w.RegisterActivity(RecordCycleMetrics)
	w.RegisterActivity(ReadHumanDecision)
	w.RegisterActivity(AwaitApproval)
	w.RegisterActivity(RecordAuditEvent)
	w.RegisterActivity(FetchIssueLabels)
	w.RegisterActivity(PublishDeveloperChange)
	w.RegisterActivity(EnsureIssue)
	w.RegisterActivity(EnsureIssueState)
	w.RegisterActivity(EnsureIssueLabels)
	w.RegisterActivity(WaitForCI)
	w.RegisterActivity(MergePullRequest)
	w.RegisterActivity(RunRelease)
	w.RegisterActivity(DeployRelease)
	w.RegisterActivity(VerifyDeployment)
	w.RegisterActivity(ScanDependencies)
	w.RegisterActivity(EnsureDependencyIssue)
	w.RegisterActivity(ListBacklogItems)
	w.RegisterActivity(EnsureReadyIssue)
	w.RegisterActivity(RecordGroomNote)
	w.RegisterActivity(RecordGroomMetrics)
	w.RegisterActivity(DetectAffectedDownstreams)
	w.RegisterActivity(EnsureCrossRepoIssue)
	w.RegisterActivity(EvaluateAlerts)
	w.RegisterActivity(EnsureFailureIssue)
	w.RegisterActivity(NotifyCycleEvent)     // #123: notify targets (slack/webhook/log)
	w.RegisterActivity(WriteCycleEvidence)   // #83: durable per-cycle evidence
	w.RegisterActivity(RecordActivation)     // #88: per-issue activation lineage
	w.RegisterActivity(RecordChange)         // #97: change/work-item lifecycle
	w.RegisterActivity(CleanupWorktree)      // #93: disconnect-safe worktree teardown
	w.RegisterActivity(SweepOrphanWorktrees) // #93: stranded-worktree sweep

	// §39 metrics sink (best-effort /metrics endpoint; env METRICS_ADDR, e.g. :9090)
	StartMetricsServer(os.Getenv("METRICS_ADDR"))

	// §32 webhook fast-path trigger (env GH_WEBHOOK_SECRET + WEBHOOK_ADDR).
	// Loads repos.yaml so events route to the configured lifecycle policy.
	if repos, err := loadReposConfigWithResources(defaultReposPath()); err == nil {
		StartWebhookServer(c, repos)
	} else {
		log.Printf("webhook disabled: repos.yaml: %v", err)
	}

	// #105 read-only console (env KONOHA_CONSOLE_ADDR; loopback-only).
	// Disabled by default; a non-loopback address fails fast.
	consoleStore, err := NewEvidenceStore(appCfg.Evidence, &defaultRedactor)
	if err != nil {
		log.Fatalf("console: %v", err)
	}
	if _, err := StartConsoleServer(c, consoleStore, consoleVersion()); err != nil {
		log.Fatalf("console: %v", err)
	}

	// #116 MCP surface (env KONOHA_MCP_ADDR; loopback-only, read-only by default;
	// KONOHA_MCP_CONTROL opts into the single §18 decision control tool).
	if _, err := StartMCPServer(c, consoleStore, consoleVersion()); err != nil {
		log.Fatalf("mcp: %v", err)
	}

	// #93: recover worktrees stranded by a crashed worker. workflowAlive lets the
	// sweep prove a claim's owner is gone before touching it (fail-safe: only an
	// explicit NotFound counts as gone, so a transient Temporal error never
	// sweeps a live run's worktree). Best-effort and non-blocking.
	workflowAlive = func(ctx context.Context, id string) bool {
		if _, err := c.DescribeWorkflowExecution(ctx, id, ""); err != nil {
			var notFound *serviceerror.NotFound
			return !errors.As(err, &notFound)
		}
		return true
	}
	go sweepOrphanWorktreesOnStart()

	if len(os.Args) > 1 && os.Args[1] == "start" {
		startReviewWorkflow(c)
	}

	log.Printf("prreview worker running on %s, queue %s. Ctrl-C to stop.", addr, appCfg.Worker.TaskQueue)
	err = w.Run(worker.InterruptCh())
	if err != nil {
		log.Fatal(err)
	}
}

// defaultReposPath returns the repos.yaml location: $KONOHA_CONFIG_DIR if set,
// else ./configs/repos.yaml (back-compat: ./repos.yaml was the pre-restructure
// location, but the file now lives under configs/).
func defaultReposPath() string {
	if dir := os.Getenv("KONOHA_CONFIG_DIR"); dir != "" {
		return dir + "/repos.yaml"
	}
	return "configs/repos.yaml"
}

// config prints repos.yaml-derived schedule inputs for apply.sh, which feeds
// each line to `temporal schedule create`. Path is the first non-flag arg or
// $KONOHA_CONFIG_DIR/repos.yaml (default ./configs/repos.yaml). `--deps` emits
// the §52 dependency schedules, `--alerts` the §53 alert schedules, and
// `--grooming` the §56 grooming schedules instead ("<repo>\t<interval>\t<json>").
func runConfig(args []string) error {
	path := defaultReposPath()
	deps, alerts, grooming := false, false, false
	for _, a := range args {
		switch a {
		case "--deps":
			deps = true
			continue
		case "--alerts":
			alerts = true
			continue
		case "--grooming":
			grooming = true
			continue
		}
		path = a
	}
	var lines []string
	var err error
	switch {
	case grooming:
		lines, err = groomingScheduleInputs(path)
	case alerts:
		lines, err = alertsScheduleInputs(path)
	case deps:
		lines, err = depsScheduleInputs(path)
	default:
		lines, err = scheduleInputs(path)
	}
	if err != nil {
		return err
	}
	for _, l := range lines {
		fmt.Println(l)
	}
	return nil
}

func runPublish() error {
	// debug: run PublishDeveloperChange directly (local worktree -> PR).
	// Env: REPO_PATH, BRANCH, TITLE, BODY, VALIDATION_CMD, ALLOW_ON_FAILURE, DRY_RUN.
	repoPath := "/Users/vit/projects/ducklambda"
	if p := os.Getenv("REPO_PATH"); p != "" {
		repoPath = p
	}
	res, err := PublishDeveloperChange(context.Background(), PublishInput{
		Owner: "vitdo82", Repo: "ducklambda", RepoPath: repoPath,
		Branch:         getenv("BRANCH", "konoha/dev"),
		BaseBranch:     getenv("BASE", "main"),
		Title:          getenv("TITLE", "konoha: automated change"),
		Body:           os.Getenv("BODY"),
		ValidationCmd:  os.Getenv("VALIDATION_CMD"),
		AllowOnFailure: os.Getenv("ALLOW_ON_FAILURE") == "1",
		DryRun:         os.Getenv("DRY_RUN") == "1",
	})
	if err != nil {
		log.Printf("publish: validated=%v skipped=%v pr=%d output:\n%s",
			res.Validated, res.ValidationSkipped, res.PRNumber, res.ValidationOutput)
		return err
	}
	fmt.Printf("published branch=%s pr=%d url=%s validated=%v\n",
		res.Branch, res.PRNumber, res.PRURL, res.Validated)
	return nil
}

func runAnalyze() error {
	// debug: run the Issue Analyst agent against one issue directly (no
	// Temporal), printing the validated IssueAnalysis (§4.1) as JSON.
	// Env: OWNER, REPO, REPO_PATH; issue number is arg 1 or ISSUE_NUM.
	n, err := issueNumArg()
	if err != nil {
		return err
	}
	a, err := AnalyzeIssue(context.Background(), debugIssueInput(n))
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(b))
	return nil
}

func runEstimate() error {
	// debug: run the Issue Analyst + Estimator agents against one issue
	// directly (no Temporal), printing the validated EstimateResult (§4.2)
	// as JSON. Env: OWNER, REPO, REPO_PATH; issue number is arg 1 or ISSUE_NUM.
	n, err := issueNumArg()
	if err != nil {
		return err
	}
	in := debugIssueInput(n)
	a, err := AnalyzeIssue(context.Background(), in)
	if err != nil {
		return err
	}
	e, err := EstimateIssue(context.Background(), in, a)
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(b))
	return nil
}

func runDevelop() error {
	// debug: run the Issue Analyst + Developer agents against one issue directly
	// (no Temporal), printing the validated DevelopResult (§4.4) as JSON.
	// Env: OWNER, REPO, REPO_PATH, VALIDATION_CMD, BASE_SHA;
	// issue number is arg 1 or ISSUE_NUM.
	n, err := issueNumArg()
	if err != nil {
		return err
	}
	in := debugIssueInput(n)
	a, err := AnalyzeIssue(context.Background(), in)
	if err != nil {
		return err
	}
	devModel, err := stageAgentModel(appCfg.Defaults.Agents, in.Lifecycle, stageDeveloper)
	if err != nil {
		return err
	}
	d, err := RunDeveloper(context.Background(), DevelopRequest{
		Owner: in.Owner, Repo: in.Repo, IssueNum: in.IssueNum, RepoPath: in.RepoPath,
		Model:              devModel,
		WorkflowID:         "cli-develop", // debug: single synthetic workflow
		BaseSHA:            os.Getenv("BASE_SHA"),
		Title:              a.Title,
		Body:               a.Body,
		Spec:               a.Spec,
		Requirements:       a.Requirements,
		AcceptanceCriteria: a.AcceptanceCriteria,
		Constraints:        a.Constraints,
		Assumptions:        a.Assumptions,
		OpenQuestions:      a.OpenQuestions,
		ValidationCmd:      os.Getenv("VALIDATION_CMD"),
	})
	if err != nil {
		return err
	}
	d.Evidence = nil // debug output keeps its prior shape (#83 evidence is on disk)
	b, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(b))
	return nil
}

func runReview() error {
	// debug: run the Issue Analyst + Reviewer against one PR directly (no
	// Temporal), printing the validated, commit-bound ReviewResult (§4.5/§15)
	// as JSON. PR number is arg 2 or PR_NUM; issue context for the analyst is
	// arg 2's ISSUE_NUM default (or ISSUE_NUM env). Commit HEAD is resolved
	// from the PR; override the reviewed SHA with REVIEW_SHA.
	n, err := issueNumArg()
	if err != nil {
		return err
	}
	in := debugIssueInput(n)
	a, err := AnalyzeIssue(context.Background(), in)
	if err != nil {
		return err
	}

	pr := 0
	fmt.Sscan(getenv("PR_NUM", "1"), &pr)
	if pr <= 0 {
		return fmt.Errorf("PR_NUM required for review")
	}
	diff, err := prDiff(in.Owner, in.Repo, pr)
	if err != nil {
		return err
	}
	sha := getenv("REVIEW_SHA", "")
	if sha == "" {
		sha, err = prHeadSHA(context.Background(), in.Owner, in.Repo, pr)
		if err != nil {
			return err
		}
	}

	reviewModel, err := stageAgentModel(appCfg.Defaults.Agents, in.Lifecycle, stageReviewer)
	if err != nil {
		return err
	}
	r, err := RunReviewer(context.Background(), ReviewRequest{
		Owner: in.Owner, Repo: in.Repo, PRNumber: pr, RepoPath: in.RepoPath,
		Model:     reviewModel,
		CommitSHA: sha, Diff: diff,
		Title: a.Title, Body: a.Body, Spec: a.Spec,
		Requirements: a.Requirements, AcceptanceCriteria: a.AcceptanceCriteria,
		Constraints: a.Constraints, Assumptions: a.Assumptions,
	})
	if err != nil {
		return err
	}
	r.Evidence = nil // debug output keeps its prior shape (#83 evidence is on disk)
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(b))
	return nil
}

func runRepair() error {
	// debug: run the Issue Analyst + Repair agents against one issue directly
	// (no Temporal), printing the validated DevelopResult (§4.6) as JSON.
	// Env: OWNER, REPO, REPO_PATH, VALIDATION_CMD, PRIOR_SHA (commit to continue
	// from; default repo HEAD), and optional REVIEW_BLOCKING / REVIEW_NITS
	// (newline-separated findings). Issue number is arg 2 or ISSUE_NUM.
	n, err := issueNumArg()
	if err != nil {
		return err
	}
	in := debugIssueInput(n)
	a, err := AnalyzeIssue(context.Background(), in)
	if err != nil {
		return err
	}

	blocking := linesEnv("REVIEW_BLOCKING")
	if len(blocking) == 0 {
		blocking = []string{"fix the change so it satisfies the acceptance criteria"}
	}
	repairModel, err := stageAgentModel(appCfg.Defaults.Agents, in.Lifecycle, stageRepair)
	if err != nil {
		return err
	}
	repair, err := RunRepair(context.Background(), RepairRequest{
		Develop: DevelopRequest{
			Owner: in.Owner, Repo: in.Repo, IssueNum: in.IssueNum, RepoPath: in.RepoPath,
			Model:              repairModel,
			WorkflowID:         "cli-repair", // debug: single synthetic workflow
			Title:              a.Title,
			Body:               a.Body,
			Spec:               a.Spec,
			Requirements:       a.Requirements,
			AcceptanceCriteria: a.AcceptanceCriteria,
			Constraints:        a.Constraints,
			Assumptions:        a.Assumptions,
			OpenQuestions:      a.OpenQuestions,
			ValidationCmd:      os.Getenv("VALIDATION_CMD"),
		},
		Review: ReviewResult{
			Verdict:  "REQUEST_CHANGES",
			Blocking: blocking,
			Nits:     linesEnv("REVIEW_NITS"),
		},
		PreviousCommit: os.Getenv("PRIOR_SHA"),
	})
	if err != nil {
		return err
	}
	repair.WorktreeDir = ""
	repair.Evidence = nil // debug output keeps its prior shape (#83 evidence is on disk)
	b, err := json.MarshalIndent(repair, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(b))
	return nil
}

// linesEnv splits an env var into non-empty trimmed lines (multi-finding input
// to the debug repair entrypoint).
func linesEnv(k string) []string {
	var out []string
	for _, l := range strings.Split(os.Getenv(k), "\n") {
		if t := strings.TrimSpace(l); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func runComplete() error {
	// debug: run the Issue Analyst + Completion agents against one issue directly
	// (no Temporal), printing the validated CompletionResult (§4.7/§30) as JSON.
	// Env: OWNER, REPO, REPO_PATH; issue number is arg 2 or ISSUE_NUM.
	// Optional: PR_NUM (default 0 = no PR yet), REVIEW_VERDICT/REVIEW_SUMMARY,
	// and DIFF/VALIDATION_OUTPUT for observed artifacts.
	n, err := issueNumArg()
	if err != nil {
		return err
	}
	in := debugIssueInput(n)
	a, err := AnalyzeIssue(context.Background(), in)
	if err != nil {
		return err
	}

	pr := 0
	fmt.Sscan(getenv("PR_NUM", ""), &pr)
	diff := os.Getenv("DIFF")
	if diff == "" && pr > 0 {
		if d, err := prDiff(in.Owner, in.Repo, pr); err == nil {
			diff = d
		}
	}

	compModel, err := stageAgentModel(appCfg.Defaults.Agents, in.Lifecycle, stageCompletion)
	if err != nil {
		return err
	}
	c, err := RunCompletion(context.Background(), CompletionRequest{
		Owner: in.Owner, Repo: in.Repo, IssueNum: in.IssueNum, PRNumber: pr,
		RepoPath: in.RepoPath,
		Model:    compModel,
		Title:    a.Title, Body: a.Body, Spec: a.Spec,
		Requirements:       a.Requirements,
		AcceptanceCriteria: a.AcceptanceCriteria,
		Constraints:        a.Constraints,
		Assumptions:        a.Assumptions,
		ReviewVerdict:      getenv("REVIEW_VERDICT", ""),
		ReviewSummary:      os.Getenv("REVIEW_SUMMARY"),
		Diff:               diff,
		ValidationOutput:   os.Getenv("VALIDATION_OUTPUT"),
	})
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(b))
	return nil
}

func runResolve() error {
	// debug: run the Issue Analyst + Resolution agents against one issue directly
	// (no Temporal), printing the validated ResolutionResult (§37) as JSON.
	// Env: OWNER, REPO, REPO_PATH; issue number is arg 2 or ISSUE_NUM.
	// The reviewer/completion observations come from REVIEW_VERDICT/REVIEW_SUMMARY
	// (newline-separated BLOCKING via REVIEW_BLOCKING, NITS via REVIEW_NITS),
	// COMPLETE (true/false), COMPLETION_RECOMMENDATION, and optional
	// DEVELOPER_SUMMARY.
	n, err := issueNumArg()
	if err != nil {
		return err
	}
	in := debugIssueInput(n)
	a, err := AnalyzeIssue(context.Background(), in)
	if err != nil {
		return err
	}

	criteria := []CriterionResult{}
	for _, c := range linesEnv("COMPLETION_CRITERIA") {
		criteria = append(criteria, CriterionResult{Criterion: c, Met: true, Justification: "reported"})
	}
	rec := getenv("COMPLETION_RECOMMENDATION", "close")
	complete := os.Getenv("COMPLETE") == "true"
	if rec == "reopen" || rec == "blocked" {
		complete = false
	}

	r, err := RunResolution(context.Background(), ResolutionRequest{
		Owner: in.Owner, Repo: in.Repo, IssueNum: in.IssueNum, RepoPath: in.RepoPath,
		Title: a.Title, Body: a.Body, Spec: a.Spec,
		Requirements: a.Requirements, Constraints: a.Constraints, Assumptions: a.Assumptions,
		DeveloperSummary:         os.Getenv("DEVELOPER_SUMMARY"),
		ReviewVerdict:            getenv("REVIEW_VERDICT", ""),
		ReviewBlocking:           linesEnv("REVIEW_BLOCKING"),
		ReviewNits:               linesEnv("REVIEW_NITS"),
		CompletionComplete:       complete,
		CompletionCriteria:       criteria,
		CompletionRisks:          linesEnv("COMPLETION_RISKS"),
		CompletionOpenQuestions:  linesEnv("COMPLETION_OPEN_QUESTIONS"),
		CompletionRecommendation: rec,
	})
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(b))
	return nil
}

func runPO() error {
	// debug: read + validate the human decision channel for one issue directly
	// (no Temporal), printing the validated HumanDecision (§18) as JSON. The
	// channel is the issue body/comments carrying `<!-- konoha-decision: X -->`;
	// a malformed decision fails closed (§36). Env: OWNER/REPO/REPO_PATH;
	// issue number is arg 2 or ISSUE_NUM.
	n, err := issueNumArg()
	if err != nil {
		return err
	}
	in := debugIssueInput(n)
	d, err := ReadHumanDecision(context.Background(), HumanDecisionInput{
		Owner: in.Owner, Repo: in.Repo, Num: in.IssueNum,
	})
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(b))
	return nil
}

func runSplit() error {
	// debug: run the Issue Analyst + Estimator + Splitter agents against one
	// issue directly (no Temporal), printing the validated SplitResult (§4.3)
	// as JSON. Env: OWNER, REPO, REPO_PATH; issue number is arg 2 or ISSUE_NUM.
	n, err := issueNumArg()
	if err != nil {
		return err
	}
	in := debugIssueInput(n)
	a, err := AnalyzeIssue(context.Background(), in)
	if err != nil {
		return err
	}
	e, err := EstimateIssue(context.Background(), in, a)
	if err != nil {
		return err
	}
	s, err := RunSplit(context.Background(), in, a, e)
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(b))
	return nil
}

func runDiagnose() error {
	// debug: run the Diagnosis agent against one issue directly (no Temporal),
	// printing the validated DiagnosisResult (§17) as JSON.
	// Env: OWNER, REPO, REPO_PATH, DIAG_REASON (development_failed |
	// review_loop_exhausted, default review_loop_exhausted), ATTEMPTS,
	// ATTEMPT_SUMMARIES (newline-separated), REVIEW_BLOCKING/REVIEW_NITS.
	n, err := issueNumArg()
	if err != nil {
		return err
	}
	in := debugIssueInput(n)
	a, err := AnalyzeIssue(context.Background(), in)
	if err != nil {
		return err
	}

	reason := getenv("DIAG_REASON", diagReasonReviewLoopExhausted)
	attempts := 0
	fmt.Sscan(getenv("ATTEMPTS", ""), &attempts)

	d, err := RunDiagnosis(context.Background(), DevelopmentFailure{
		Owner: in.Owner, Repo: in.Repo, IssueNum: in.IssueNum, RepoPath: in.RepoPath,
		Title: a.Title, Body: a.Body, Spec: a.Spec,
		Requirements: a.Requirements, Constraints: a.Constraints, Assumptions: a.Assumptions,
		Reason:           reason,
		Attempts:         attempts,
		AttemptSummaries: linesEnv("ATTEMPT_SUMMARIES"),
		ReviewBlocking:   linesEnv("REVIEW_BLOCKING"),
		ReviewNits:       linesEnv("REVIEW_NITS"),
	})
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(b))
	return nil
}

func runLabels() error {
	// debug: ensure the §21 label set exists in the repo and print it. Two
	// modes: `./prbot labels` creates the full set; `OWNER`/`REPO`/`REPO_PATH`
	// override, `DRY_RUN=1` for shadow mode.
	owner := getenv("OWNER", "vitdo82")
	repo := getenv("REPO", "ducklambda")
	dry := os.Getenv("DRY_RUN") == "1"
	labels := append(projectableLabels(), sourceLabels()...)
	if err := EnsureRepoLabels(context.Background(), LabelsTheirsRequest{
		Owner: owner, Repo: repo, Labels: labels, DryRun: dry,
	}); err != nil {
		return err
	}
	fmt.Println(strings.Join(labels, "\n"))
	return nil
}

func runChildren() error {
	// debug: run the Issue Analyst + Estimator + Splitter, then (unless
	// DRY_RUN=1) idempotently create the child issues, printing the validated
	// ChildIssue identities (§9). Env: OWNER, REPO, REPO_PATH; issue number is
	// arg 2 or ISSUE_NUM.
	n, err := issueNumArg()
	if err != nil {
		return err
	}
	in := debugIssueInput(n)
	a, err := AnalyzeIssue(context.Background(), in)
	if err != nil {
		return err
	}
	e, err := EstimateIssue(context.Background(), in, a)
	if err != nil {
		return err
	}
	s, err := RunSplit(context.Background(), in, a, e)
	if err != nil {
		return err
	}
	children, err := EnsureChildren(context.Background(), ChildCreateRequest{
		Owner: in.Owner, Repo: in.Repo, ParentNum: in.IssueNum,
		Tickets: s.Tickets, DryRun: os.Getenv("DRY_RUN") == "1",
	})
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(children, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(b))
	return nil
}

func runRelease() error {
	// debug: run the §50 release activity directly (no Temporal), printing the
	// ReleaseResult JSON. Env: OWNER/REPO (REPO_PATH is not needed — the
	// changelog is committed via the GitHub Contents API), BASE_BRANCH,
	// CHANGELOG_PATH, VERSION_PREFIX, DRY_RUN, RELEASE_ENABLED (default on for
	// this entrypoint).
	cfg := ReleaseConfig{
		Enabled:       os.Getenv("RELEASE_ENABLED") != "0",
		ChangelogPath: os.Getenv("CHANGELOG_PATH"),
		VersionPrefix: os.Getenv("VERSION_PREFIX"),
	}
	res, err := RunRelease(context.Background(), ReleaseRequest{
		Owner:      getenv("OWNER", "vitdo82"),
		Repo:       getenv("REPO", "ducklambda"),
		BaseBranch: os.Getenv("BASE_BRANCH"),
		Config:     cfg,
		DryRun:     os.Getenv("DRY_RUN") == "1",
	})
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(b))
	return nil
}

func runDeps() error {
	// debug: run the §52 dependency scan directly (no Temporal), printing the
	// validated DependencyReport JSON. Env: OWNER/REPO/REPO_PATH, SCAN_CMD
	// (required; must print the JSON finding array).
	report, err := ScanDependencies(context.Background(), DependencyScanRequest{
		Owner:    getenv("OWNER", "vitdo82"),
		Repo:     getenv("REPO", "ducklambda"),
		RepoPath: getenv("REPO_PATH", "/Users/vit/projects/ducklambda"),
		ScanCmd:  os.Getenv("SCAN_CMD"),
	})
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(b))
	return nil
}

func issueNumArg() (int, error) {
	var n int
	if len(os.Args) > 2 {
		fmt.Sscan(os.Args[2], &n)
	} else {
		fmt.Sscan(getenv("ISSUE_NUM", "1"), &n)
	}
	if n <= 0 {
		return 0, fmt.Errorf("issue number required (arg or ISSUE_NUM)")
	}
	return n, nil
}

func debugIssueInput(n int) IssueInput {
	return IssueInput{
		Owner:     getenv("OWNER", "vitdo82"),
		Repo:      getenv("REPO", "ducklambda"),
		IssueNum:  n,
		RepoPath:  getenv("REPO_PATH", "/Users/vit/projects/ducklambda"),
		Lifecycle: LifecycleConfig{DryRun: true},
	}
}

// runStatus is the `prbot status <repo> <issue>` reader (#89): it queries the
// stable <repo>-issue-<n> workflow's "status" handler via the Temporal client
// and prints the JSON snapshot. The projection (statusJSON) is pure and
// unit-tested without a Temporal server.
func runStatus(c client.Client, args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: prbot status <repo> <issue>")
	}
	var n int
	fmt.Sscan(args[1], &n)
	if n <= 0 {
		return fmt.Errorf("usage: prbot status <repo> <issue>")
	}
	wfID := fmt.Sprintf("%s-issue-%d", args[0], n)
	val, err := c.QueryWorkflow(context.Background(), wfID, "", "status")
	if err != nil {
		return fmt.Errorf("query status %s: %w", wfID, err)
	}
	var st StatusQuery
	if err := val.Get(&st); err != nil {
		return err
	}
	out, err := statusJSON(st)
	if err != nil {
		return err
	}
	fmt.Println(out)
	return nil
}

// runGetChanges is `prbot get changes` (#97): lists every recorded change record
// newest first. The projection (sortChangesNewestFirst/changesJSON) is pure and
// unit-tested without a Temporal server.
func runGetChanges() error {
	store, err := NewEvidenceStore(appCfg.Evidence, &defaultRedactor)
	if err != nil {
		return err
	}
	list, err := store.listChanges()
	if err != nil {
		return err
	}
	sortChangesNewestFirst(list)
	out, err := changesJSON(list)
	if err != nil {
		return err
	}
	fmt.Println(out)
	return nil
}

// runDescribeChange is `prbot describe change <repo> <issue>` (#97): prints the
// change record joined with its recomputed lineage (activations, attempts,
// totals). The projection (buildChangeView/changeViewJSON) is pure and
// unit-tested without a Temporal server.
func runDescribeChange(args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: prbot describe change <repo> <issue>")
	}
	repo := args[0]
	var n int
	fmt.Sscan(args[1], &n)
	if n <= 0 {
		return fmt.Errorf("usage: prbot describe change <repo> <issue>")
	}
	store, err := NewEvidenceStore(appCfg.Evidence, &defaultRedactor)
	if err != nil {
		return err
	}
	c, err := store.loadChange(repo, n)
	if err != nil {
		return err
	}
	var l Lineage
	if owner, repoName, ok := strings.Cut(c.Repo, "/"); ok {
		l, _ = store.loadLineage(owner, repoName, n, false)
	}
	out, err := changeViewJSON(buildChangeView(c, l))
	if err != nil {
		return err
	}
	fmt.Println(out)
	return nil
}

func startReviewWorkflow(c client.Client) {
	pr := 6
	if n := os.Getenv("PR_NUM"); n != "" {
		fmt.Sscan(n, &pr)
	}
	wo := client.StartWorkflowOptions{ID: fmt.Sprintf("pr-review-%d", pr), TaskQueue: appCfg.Worker.TaskQueue}
	exec, err := c.ExecuteWorkflow(context.Background(), wo, "PrReviewWorkflow", PrInput{
		Owner: "vitdo82", Repo: "ducklambda", PrNum: pr,
		RepoPath: "/Users/vit/projects/ducklambda",
	})
	if err != nil {
		log.Fatalf("start workflow: %v", err)
	}
	fmt.Println("started", exec.GetID())
	os.Exit(0)
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
