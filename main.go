package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strconv"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
)

const taskQueue = "review-queue"

// defaultWorkerMaxConcurrentActivities is the §43 worker-level cap on
// concurrent activity executions across all repos (env override:
// WORKER_MAX_CONCURRENT_ACTIVITIES). 10 opencode/gh subprocesses at once keeps
// a single worker from being exhausted by fan-out.
const defaultWorkerMaxConcurrentActivities = 10

func main() {
	// Pure-CLI commands that need no Temporal connection.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "config":
			if err := runConfig(os.Args[2:]); err != nil {
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
		case "token":
			t, err := appToken(context.Background())
			if err != nil {
				log.Fatal("appToken:", err)
			}
			fmt.Println(t[:12] + "..." + t[len(t)-8:])
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

	w := worker.New(c, taskQueue, worker.Options{
		MaxConcurrentActivityExecutionSize: workerMaxConcurrentActivities(),
	})
	w.RegisterWorkflowWithOptions(PrReviewWorkflow, workflow.RegisterOptions{Name: "PrReviewWorkflow"})
	w.RegisterWorkflowWithOptions(ReviewAllWorkflow, workflow.RegisterOptions{Name: "ReviewAllWorkflow"})
	w.RegisterWorkflowWithOptions(IssueCycleWorkflow, workflow.RegisterOptions{Name: "IssueCycleWorkflow"})
	w.RegisterWorkflowWithOptions(IssueAllWorkflow, workflow.RegisterOptions{Name: "IssueAllWorkflow"})
	w.RegisterActivity(FetchDiff)
	w.RegisterActivity(ReviewWithOpencode)
	w.RegisterActivity(SubmitReview)
	w.RegisterActivity(ListUnreviewedPRs)
	w.RegisterActivity(ListUnmanagedIssues)
	w.RegisterActivity(AnalyzeIssue)
	w.RegisterActivity(EstimateIssue)
	w.RegisterActivity(RunDeveloper)
	w.RegisterActivity(EnsurePullRequest)
	w.RegisterActivity(PublishDeveloperChange)
	w.RegisterActivity(EnsureIssue)
	w.RegisterActivity(EnsureIssueState)
	w.RegisterActivity(EnsureIssueLabels)

	if len(os.Args) > 1 && os.Args[1] == "start" {
		startReviewWorkflow(c)
	}

	log.Printf("prreview worker running on %s, queue %s. Ctrl-C to stop.", addr, taskQueue)
	err = w.Run(worker.InterruptCh())
	if err != nil {
		log.Fatal(err)
	}
}

// config prints repos.yaml-derived schedule inputs ("<repo>\t<json>" per line)
// for apply.sh, which feeds each line to `temporal schedule create`.
func runConfig(args []string) error {
	path := "repos.yaml"
	if len(args) > 0 {
		path = args[0]
	}
	lines, err := scheduleInputs(path)
	if err != nil {
		return err
	}
	for _, l := range lines {
		fmt.Println(l)
	}
	return nil
}

func workerMaxConcurrentActivities() int {
	if n, err := strconv.Atoi(os.Getenv("WORKER_MAX_CONCURRENT_ACTIVITIES")); err == nil && n >= 1 {
		return n
	}
	return defaultWorkerMaxConcurrentActivities
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
	d, err := RunDeveloper(context.Background(), DevelopRequest{
		Owner: in.Owner, Repo: in.Repo, IssueNum: in.IssueNum, RepoPath: in.RepoPath,
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
	b, err := json.MarshalIndent(d, "", "  ")
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

func startReviewWorkflow(c client.Client) {
	pr := 6
	if n := os.Getenv("PR_NUM"); n != "" {
		fmt.Sscan(n, &pr)
	}
	wo := client.StartWorkflowOptions{ID: fmt.Sprintf("pr-review-%d", pr), TaskQueue: taskQueue}
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
