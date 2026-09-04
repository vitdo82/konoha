package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
)

const taskQueue = "review-queue"

func main() {
	addr := os.Getenv("TEMPORAL_ADDRESS")
	if addr == "" {
		addr = "127.0.0.1:7233"
	}
	c, err := client.Dial(client.Options{HostPort: addr})
	if err != nil {
		log.Fatalf("dial temporal %s: %v", addr, err)
	}
	defer c.Close()

	w := worker.New(c, taskQueue, worker.Options{})
	w.RegisterWorkflowWithOptions(PrReviewWorkflow, workflow.RegisterOptions{Name: "PrReviewWorkflow"})
	w.RegisterWorkflowWithOptions(ReviewAllWorkflow, workflow.RegisterOptions{Name: "ReviewAllWorkflow"})
	w.RegisterActivity(FetchDiff)
	w.RegisterActivity(ReviewWithOpencode)
	w.RegisterActivity(SubmitReview)
	w.RegisterActivity(ListUnreviewedPRs)

	log.Printf("prreview worker running on %s, queue %s. Ctrl-C to stop.", addr, taskQueue)
	for _, r := range os.Args[1:] {
		if r == "token" {
			t, err := appToken(context.Background())
			if err != nil {
				log.Fatal("appToken:", err)
			}
			fmt.Println(t[:12] + "..." + t[len(t)-8:])
			return
		}
		if r == "start" {
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
	}
	err = w.Run(worker.InterruptCh())
	if err != nil {
		log.Fatal(err)
	}
}