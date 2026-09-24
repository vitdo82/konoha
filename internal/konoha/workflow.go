package konoha

import (
	"fmt"
	"strings"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// batchRuns splits n items into consecutive runs of at most max. §43: a sweep
// never has more than `max` child workflows in flight at once. max<1 → 1
// (serialized).
func batchRuns(n, max int) [][2]int {
	if max < 1 {
		max = 1
	}
	var out [][2]int
	for i := 0; i < n; i += max {
		j := i + max
		if j > n {
			j = n
		}
		out = append(out, [2]int{i, j})
	}
	return out
}

// PrReviewWorkflow: fetch diff -> opencode review -> post verdict back to PR.
func PrReviewWorkflow(ctx workflow.Context, in PrInput) error {
	ao := workflow.ActivityOptions{
		StartToCloseTimeout: appCfg.Worker.startToClose(),
		RetryPolicy: &temporal.RetryPolicy{
			MaximumAttempts: int32(appCfg.Worker.RetryMaxAttempts),
		},
	}
	ctx = workflow.WithActivityOptions(ctx, ao)
	info := workflow.GetInfo(ctx)

	// #83: durable per-cycle evidence (best-effort — never fails the review).
	writeEvidence := func(req EvidenceRequest) {
		req.CycleID = info.WorkflowExecution.ID
		req.DryRun = in.Lifecycle.DryRun
		if err := workflow.ExecuteActivity(ctx, WriteCycleEvidence, req).Get(ctx, nil); err != nil {
			workflow.GetLogger(ctx).Error("evidence write failed (non-fatal)", "err", err)
		}
	}

	var diff string
	if err := workflow.ExecuteActivity(ctx, FetchDiff, in).Get(ctx, &diff); err != nil {
		return err
	}
	writeEvidence(EvidenceRequest{Diff: diff})

	var result ReviewResult
	if err := workflow.ExecuteActivity(ctx, ReviewWithOpencode, in, diff).Get(ctx, &result); err != nil {
		return err
	}
	e := evidenceOf(result.Evidence)
	writeEvidence(EvidenceRequest{
		Prompt: e.Prompt, Stdout: e.Stdout, Stderr: e.Stderr,
		Result: evidenceResultJSON(result), Diff: diff,
	})

	if err := workflow.ExecuteActivity(ctx, SubmitReview, in, result).Get(ctx, nil); err != nil {
		return err
	}

	workflow.GetLogger(ctx).Info("review done",
		"workflow", info.WorkflowExecution.ID, "verdict", result.Verdict)
	return nil
}

// ReviewAllWorkflow is cron-driven (Temporal Schedule): lists unreviewed open
// PRs in the given repo and reviews each as a child workflow with a stable,
// repo-specific ID (<repo>-review-<pr>). Reused across repos by creating one
// schedule per repo with a different RepoScope input. Best-effort per PR: a
// failure in one doesn't abort the rest; stable child ID prevents double review.
func ReviewAllWorkflow(ctx workflow.Context, scope RepoScope) error {
	ao := workflow.ActivityOptions{
		StartToCloseTimeout: appCfg.Worker.sweepWindow(),
		RetryPolicy: &temporal.RetryPolicy{
			MaximumAttempts: int32(appCfg.Worker.SweepRetryMaxAttempts),
		},
	}
	ctx = workflow.WithActivityOptions(ctx, ao)

	var pending []PrInput
	if err := workflow.ExecuteActivity(ctx, ListUnreviewedPRs, scope).Get(ctx, &pending); err != nil {
		return err
	}
	workflow.GetLogger(ctx).Info("ReviewAll sweep", "repo", scope.Owner+"/"+scope.Repo, "pendingPRs", len(pending))

	for _, in := range pending {
		cwo := workflow.ChildWorkflowOptions{
			WorkflowID: fmt.Sprintf("%s-review-%d", scope.Repo, in.PrNum), // repo-specific, stable
		}
		cctx := workflow.WithChildOptions(ctx, cwo)
		if err := workflow.ExecuteChildWorkflow(cctx, PrReviewWorkflow, in).Get(cctx, nil); err != nil {
			workflow.GetLogger(ctx).Error("child review failed, skipping", "pr", in.PrNum, "err", err)
			continue
		}
		workflow.GetLogger(ctx).Info("reviewed", "pr", in.PrNum)
	}
	return nil
}

// Issue lifecycle states (§6). Workflow history is the authoritative lifecycle
// record: each transition is logged and readable via the "state" query handler.
const (
	stateDiscovered       = "DISCOVERED"
	stateAnalyzing        = "ANALYZING"
	stateEstimating       = "ESTIMATING"
	stateSmall            = "SMALL"
	stateLarge            = "LARGE"
	stateDeveloping       = "DEVELOPING"        // §17: diagnosis said retry
	stateSplitting        = "SPLITTING"         // §17: diagnosis said repartition
	stateReviewing        = "REVIEWING"         // §22: a review is in flight (transient label)
	stateChangesRequested = "CHANGES_REQUESTED" // §22: repair loop (transient label)
	stateMerging          = "MERGING"           // §49: CI gate + PR merge before close
	statePendingApproval  = "PENDING_APPROVAL"  // §55: waiting on a pre-execution human sign-off
	stateHumanRequired    = "HUMAN_REQUIRED"    // §18: waiting on a human exception handler
	stateBlocked          = "BLOCKED"           // §18/§40: blocked; a timeout keeps it here
	stateCancelled        = "CANCELLED"         // §18: human decision "cancel"
	stateClosed           = "CLOSED"            // §14/§19: policy-permitted close
	stateFailed           = "FAILED"            // §40: terminal failure (never silent success)
	stateNotWired         = "NOT_WIRED"         // §46: branch this increment does not handle (LARGE)
)

// IssueCycleWorkflow drives one issue through the lifecycle state machine
// (§6). Deterministic: all I/O lives in activities. Foundation scope:
// DISCOVERED → ANALYZING → ESTIMATING → SMALL|LARGE. The states after the
// routing branch (DEVELOPING/SPLITTING onward, ending in CLOSED/FAILED/
// CANCELLED/HUMAN_REQUIRED) are wired in by subsequent issues — until then the
// workflow ends cleanly at the branch decision. Since label-based idempotency
// is also a later issue, the next sweep may simply run the cycle again; the
// stable <repo>-issue-<n> ID guarantees only one run is active at a time.
func IssueCycleWorkflow(ctx workflow.Context, in IssueInput) error {
	ao := workflow.ActivityOptions{
		StartToCloseTimeout: appCfg.Worker.startToClose(),
		RetryPolicy: &temporal.RetryPolicy{
			MaximumAttempts: int32(appCfg.Worker.RetryMaxAttempts),
		},
	}
	ctx = workflow.WithActivityOptions(ctx, ao)
	// §4.4: agent dev/repair run long (real opencode work), so they get a wider
	// window than the default cycle activities.
	devAO := workflow.ActivityOptions{
		StartToCloseTimeout: appCfg.Worker.devWindow(),
		RetryPolicy: &temporal.RetryPolicy{
			MaximumAttempts: int32(appCfg.Worker.RetryMaxAttempts),
		},
	}
	devCtx := workflow.WithActivityOptions(ctx, devAO)
	info := workflow.GetInfo(ctx)

	state := stateDiscovered
	if err := workflow.SetQueryHandler(ctx, "state", func() (string, error) { return state, nil }); err != nil {
		workflow.GetLogger(ctx).Error("set state query handler failed", "err", err)
	}
	// §39 observability state tracked across transitions (logged on each).
	lastComplexity, lastConfidence := 0, ""
	lastAttempts, lastReviewRounds := 0, 0
	transition := func(s string) {
		state = s
		// §39 observability: every transition emits the field set (workflow ID,
		// issue, state, repo, complexity, confidence, review rounds). Logging is
		// replay-safe, so this stays deterministic.
		logCycleEvent(workflow.GetLogger(ctx), info.WorkflowExecution.ID, in.IssueNum,
			in.Repo, state, lastComplexity, lastConfidence, lastAttempts, lastReviewRounds)
	}

	// #83: durable per-cycle evidence. writeEvidence is best-effort I/O: it
	// accumulates the first failure into evidenceErr, which the terminal outcome
	// carries, but never changes the §6 terminal state.
	evidenceErr := ""
	writeEvidence := func(req EvidenceRequest) {
		req.CycleID = info.WorkflowExecution.ID
		req.DryRun = in.Lifecycle.DryRun
		if err := workflow.ExecuteActivity(ctx, WriteCycleEvidence, req).Get(ctx, nil); err != nil {
			if evidenceErr == "" {
				evidenceErr = err.Error()
			}
			workflow.GetLogger(ctx).Error("evidence write failed (non-fatal)", "err", err)
		}
	}
	// #84: explicit three-outcome evidence tracked across the cycle. Terminals
	// inherit the latest agent/verification outcome (defaulting to SKIPPED via
	// recordTerminal's stampOutcome, never silently absent §40).
	agentOut, verifOut := AgentOutcome(""), VerificationOutcome("")
	// finish records a terminal outcome through the shared recorder, tagging it
	// with the tracked outcomes and any prior evidence-write failure.
	finish := func(out CycleOutcome) error {
		if out.AgentOutcome == "" {
			out.AgentOutcome = agentOut
		}
		if out.VerificationOutcome == "" {
			out.VerificationOutcome = verifOut
		}
		if out.EvidenceError == "" {
			out.EvidenceError = evidenceErr
		}
		return recordTerminal(ctx, info, in, out)
	}

	// projectLabels applies the §22 label projection for the current state
	// (idempotent reconcile; drops stale transients). Best-effort: a label
	// failure must not abort the workflow.
	projectLabels := func(s string, c int) {
		if in.Lifecycle.DryRun {
			return
		}
		if err := workflow.ExecuteActivity(ctx, ApplyIssueLabels, LabelsRequest{
			Owner: in.Owner, Repo: in.Repo, Num: in.IssueNum,
			Labels: ProjectLabels(in.Lifecycle, s, c, true),
		}).Get(ctx, nil); err != nil {
			workflow.GetLogger(ctx).Error("label projection failed (non-fatal)", "err", err)
		}
	}

	transition(stateAnalyzing)
	var a IssueAnalysis
	if err := workflow.ExecuteActivity(ctx, AnalyzeIssue, in).Get(ctx, &a); err != nil {
		return err
	}

	transition(stateEstimating)
	var est EstimateResult
	if err := workflow.ExecuteActivity(ctx, EstimateIssue, in, a).Get(ctx, &est); err != nil {
		return err
	}

	// The estimator only reports; the deterministic policy routes (§4.2). Record
	// complexity + confidence in history (§38): confidence is data, never proof
	// that bypasses the routing below.
	workflow.GetLogger(ctx).Info("estimate recorded",
		"workflow", info.WorkflowExecution.ID, "issue", in.IssueNum,
		"complexity", est.Complexity, "confidence", est.Confidence)

	lastAttempts, lastReviewRounds = 0, 0

	// -- routing (§11) --
	// deterministic routing policy: complexity within the phase-gated executable
	// threshold → SMALL, else LARGE.
	var route string
	if est.Complexity >= 1 && est.Complexity <= effectiveDevelopmentMax(in.Lifecycle) {
		route = stateSmall
	} else {
		route = stateLarge
	}
	transition(route)
	if route == stateSmall {
		projectLabels(route, est.Complexity) // §22: managed + complexity label early
	}

	// §22: explain the ownership claim (unmanaged → managed) exactly once, deduped
	// by the deterministic `<!-- konoha-managed: <workflowId> -->` marker. Covers
	// both routes: an LARGE parent is claimed here, and each split child posts its
	// own note when its cycle runs. Best-effort like the label projection: Temporal
	// retries the activity, and an exhausted retry must not abort the cycle.
	model := in.Lifecycle.Model
	if strings.TrimSpace(model) == "" {
		model = defaultAgentModel
	}
	if err := workflow.ExecuteActivity(ctx, EnsureManagedNote, ManagedNoteRequest{
		Owner: in.Owner, Repo: in.Repo, IssueNum: in.IssueNum,
		WorkflowID: info.WorkflowExecution.ID, Trigger: in.Trigger,
		Route: route, Complexity: est.Complexity, Model: model,
		DryRun: in.Lifecycle.DryRun,
	}).Get(ctx, nil); err != nil {
		workflow.GetLogger(ctx).Error("managed claim note failed (non-fatal)", "err", err)
	}

	if route == stateLarge {
		return runLargePath(ctx, devCtx, in, a, est, info, route, transition, projectLabels, finish)
	}

	// escalateTerminal routes the cycle to HUMAN_REQUIRED with a recorded §18
	// reason (used by the §49 CI/merge gate: a non-green gate or an exhausted
	// CI poll is never a silent success). Never closes the issue.
	escalateTerminal := func(reason string) error {
		transition(stateHumanRequired)
		projectLabels(stateHumanRequired, est.Complexity)
		return finish(CycleOutcome{
			WorkflowID: info.WorkflowExecution.ID, Issue: in.IssueNum,
			Owner: in.Owner, Repo: in.Repo,
			Route: route, Complexity: est.Complexity, Confidence: est.Confidence,
			TerminalState:    stateHumanRequired,
			HumanRequired:    true,
			EscalationReason: reason,
			Attempts:         lastAttempts, ReviewRounds: lastReviewRounds,
		})
	}

	// blockedTerminal routes an agent-REPORTED BLOCKED outcome to the human path
	// (#84): the agent could not proceed, so the workflow must never enter the
	// repair loop or silently fail. reason is the agent's own explanation.
	blockedTerminal := func(reason string) error {
		transition(stateHumanRequired)
		projectLabels(stateHumanRequired, est.Complexity)
		return finish(CycleOutcome{
			WorkflowID: info.WorkflowExecution.ID, Issue: in.IssueNum,
			Owner: in.Owner, Repo: in.Repo,
			Route: route, Complexity: est.Complexity, Confidence: est.Confidence,
			TerminalState:       stateHumanRequired,
			HumanRequired:       true,
			EscalationReason:    reasonAgentBlocked,
			AgentOutcome:        agentOutcomeBlocked,
			AgentMessage:        reason,
			VerificationOutcome: verificationSkipped,
			Attempts:            lastAttempts, ReviewRounds: lastReviewRounds,
		})
	}

	// gateStep enforces the §55 pre-execution approval gate before a mutating
	// step. proceed=false with err=nil means the gate already recorded a terminal
	// (deny → HUMAN_REQUIRED, timeout → BLOCKED) and the workflow must stop; a
	// non-nil err is a genuine failure. With approval.preGate off it is a no-op,
	// so the default pipeline is unchanged (AC #3).
	gateStep := func(step string) (bool, error) {
		if !ApprovalRequired(in.Lifecycle, step) {
			return true, nil
		}
		transition(statePendingApproval)
		projectLabels(statePendingApproval, est.Complexity)
		res, err := awaitApprovalGate(ctx, in.Owner, in.Repo, in.IssueNum, step, in.Lifecycle)
		if err != nil {
			return false, err
		}
		if res.Granted || res.Skipped {
			return true, nil
		}
		if res.Denied {
			transition(stateHumanRequired)
			projectLabels(stateHumanRequired, est.Complexity)
			return false, finish(CycleOutcome{
				WorkflowID: info.WorkflowExecution.ID, Issue: in.IssueNum,
				Owner: in.Owner, Repo: in.Repo,
				Route: route, Complexity: est.Complexity, Confidence: est.Confidence,
				TerminalState:    stateHumanRequired,
				HumanRequired:    true,
				EscalationReason: reasonApprovalDenied,
				Attempts:         lastAttempts, ReviewRounds: lastReviewRounds,
			})
		}
		// Timed out: stay BLOCKED and never proceed (§40, no silent success).
		transition(stateBlocked)
		projectLabels(stateBlocked, est.Complexity)
		return false, finish(CycleOutcome{
			WorkflowID: info.WorkflowExecution.ID, Issue: in.IssueNum,
			Owner: in.Owner, Repo: in.Repo,
			Route: route, Complexity: est.Complexity, Confidence: est.Confidence,
			TerminalState: stateBlocked,
			Attempts:      lastAttempts, ReviewRounds: lastReviewRounds,
		})
	}

	// -- §4.4 DEVELOPING --
	transition(stateDeveloping)
	var dev DevelopResult
	if err := workflow.ExecuteActivity(devCtx, RunDeveloper, DevelopRequest{
		Owner: in.Owner, Repo: in.Repo, IssueNum: in.IssueNum, RepoPath: in.RepoPath,
		Model:              in.Lifecycle.Model,
		Harness:            in.Lifecycle.Harness,
		WorkflowID:         info.WorkflowExecution.ID,
		Title:              a.Title,
		Body:               a.Body,
		Spec:               a.Spec,
		Requirements:       a.Requirements,
		AcceptanceCriteria: a.AcceptanceCriteria,
		Constraints:        a.Constraints,
		Assumptions:        a.Assumptions,
		OpenQuestions:      a.OpenQuestions,
		// §26: structured gates / legacy command resolved from lifecycle.
		Validation:    ValidationGates(in.Lifecycle),
		ValidationCmd: ValidationCommand(in.Lifecycle),
	}).Get(ctx, &dev); err != nil {
		return err
	}
	lastAttempts++
	agentOut, verifOut = agentOutcomeOf(dev), verificationOutcomeOf(dev)
	writeAgentEvidence(writeEvidence, dev.Evidence, dev, dev.ValidationOutput)
	if dev.Blocked {
		// #84: an agent-reported block is not a failed attempt — route to human.
		return blockedTerminal(dev.BlockedReason)
	}

	// -- §20 risk gate before any GitHub mutation --
	var labels []string
	if err := workflow.ExecuteActivity(ctx, FetchIssueLabels, in).Get(ctx, &labels); err != nil {
		return err
	}
	if rk := DetectHumanEscalation(in.Lifecycle, dev.ChangedFiles, labels); rk.Escalate {
		transition(stateHumanRequired)
		projectLabels(stateHumanRequired, est.Complexity)
		return finish(CycleOutcome{
			WorkflowID: info.WorkflowExecution.ID, Issue: in.IssueNum,
			Owner: in.Owner, Repo: in.Repo,
			Route: route, Complexity: est.Complexity, Confidence: est.Confidence,
			TerminalState:    stateHumanRequired,
			HumanRequired:    true,
			EscalationReason: reasonPolicyViolation,
		})
	}

	// §55: publish is gated before any GitHub mutation when configured.
	if ok, err := gateStep(approvalStepPublish); err != nil {
		return err
	} else if !ok {
		return nil
	}

	// -- publish (idempotent PR, §25) — from the MAIN repo path + the dev commit
	// (the worktree was cleaned up; its commit lives in the main repo object DB).
	var pub PublishResult
	pubIn := PublishInput{
		Owner: in.Owner, Repo: in.Repo, RepoPath: in.RepoPath,
		CommitSHA:  dev.CommitSHA,
		Branch:     fmt.Sprintf("konoha/issue-%d", in.IssueNum),
		BaseBranch: "main",
		Title:      a.Title,
		Body:       prBody(in, a),
		// §26 validation already ran in the developer activity; its aggregate is
		// carried on dev.Validated/ValidationOutput, so publish does not re-run it.
		ValidationCmd:  "",
		AllowOnFailure: false,
		DryRun:         in.Lifecycle.DryRun,
	}
	if err := workflow.ExecuteActivity(ctx, PublishDeveloperChange, pubIn).Get(ctx, &pub); err != nil {
		return err
	}

	// -- review loop (§15) --
	var approved ReviewResult
	reviewedSHA := ""
	for attempt := 0; ; attempt++ {
		transition(stateReviewing)
		projectLabels(stateReviewing, est.Complexity)
		var headSha string
		if err := workflow.ExecuteActivity(ctx, FetchPRHead, PrInput{Owner: in.Owner, Repo: in.Repo, PrNum: pub.PRNumber}).Get(ctx, &headSha); err != nil {
			return err
		}
		var diff string
		if err := workflow.ExecuteActivity(ctx, FetchPRDiff, PrInput{Owner: in.Owner, Repo: in.Repo, PrNum: pub.PRNumber}).Get(ctx, &diff); err != nil {
			return err
		}
		reviewedSHA = headSha
		var rev ReviewResult
		if err := workflow.ExecuteActivity(ctx, RunReviewer, ReviewRequest{
			Owner: in.Owner, Repo: in.Repo, PRNumber: pub.PRNumber, RepoPath: in.RepoPath,
			Model:              in.Lifecycle.Model,
			Harness:            in.Lifecycle.Harness,
			CommitSHA:          headSha,
			Diff:               diff,
			ValidationOutput:   dev.ValidationOutput,
			Title:              a.Title,
			Body:               a.Body,
			Spec:               a.Spec,
			Requirements:       a.Requirements,
			AcceptanceCriteria: a.AcceptanceCriteria,
			Constraints:        a.Constraints,
			Assumptions:        a.Assumptions,
		}).Get(ctx, &rev); err != nil {
			return err
		}
		lastReviewRounds++
		e := evidenceOf(rev.Evidence)
		writeEvidence(EvidenceRequest{Prompt: e.Prompt, Stdout: e.Stdout, Stderr: e.Stderr,
			Result: evidenceResultJSON(rev), Diff: diff})

		if rev.Verdict == "APPROVE" {
			approved = rev
			break
		}

		// REQUEST_CHANGES → bounded repair loop (§16). Evaluate against the
		// completed develop/repair attempts (dev = 1; a repair is allowed while
		// still under the cap, then the NEXT request escalates).
		if rl := EvaluateRepairLoop(in.Lifecycle, lastAttempts); !rl.Allowed {
			transition(stateBlocked)
			projectLabels(stateBlocked, est.Complexity)
			return finish(CycleOutcome{
				WorkflowID: info.WorkflowExecution.ID, Issue: in.IssueNum,
				Owner: in.Owner, Repo: in.Repo,
				Route: route, Complexity: est.Complexity, Confidence: est.Confidence,
				TerminalState: stateBlocked,
				Attempts:      lastAttempts, ReviewRounds: lastReviewRounds,
			})
		}
		lastAttempts++
		transition(stateChangesRequested)
		var repaired DevelopResult
		if err := workflow.ExecuteActivity(devCtx, RunRepair, RepairRequest{
			Develop: DevelopRequest{
				Owner: in.Owner, Repo: in.Repo, IssueNum: in.IssueNum, RepoPath: in.RepoPath,
				Model:              in.Lifecycle.Model,
				Harness:            in.Lifecycle.Harness,
				WorkflowID:         info.WorkflowExecution.ID,
				Title:              a.Title,
				Body:               a.Body,
				Spec:               a.Spec,
				Requirements:       a.Requirements,
				AcceptanceCriteria: a.AcceptanceCriteria,
				Constraints:        a.Constraints,
				Assumptions:        a.Assumptions,
				OpenQuestions:      a.OpenQuestions,
				// §26: structured gates / legacy command resolved from lifecycle.
				Validation:    ValidationGates(in.Lifecycle),
				ValidationCmd: ValidationCommand(in.Lifecycle),
			},
			Review:         rev,
			PreviousCommit: reviewedSHA, // the SHA the reviewer actually saw (§15)
		}).Get(ctx, &repaired); err != nil {
			return err
		}
		writeAgentEvidence(writeEvidence, repaired.Evidence, repaired, repaired.ValidationOutput)
		agentOut, verifOut = agentOutcomeOf(repaired), verificationOutcomeOf(repaired)
		if repaired.Blocked {
			// #84: a blocked repair must not loop — route to the human path.
			return blockedTerminal(repaired.BlockedReason)
		}
		// re-publish the repaired commit onto the same branch (idempotent).
		pubIn.CommitSHA = repaired.CommitSHA
		pubIn.RepoPath = in.RepoPath
		if err := workflow.ExecuteActivity(ctx, PublishDeveloperChange, pubIn).Get(ctx, &pub); err != nil {
			return err
		}
		dev = repaired
	}

	// -- APPROVED → close (§15/§25; completion gate is the follow-up) --
	if !ReviewAppliesTo(approved, reviewedSHA) {
		// head moved under us: re-review required — this increment escalates
		// deterministically instead of closing on a stale approval.
		transition(stateBlocked)
		return finish(CycleOutcome{
			WorkflowID: info.WorkflowExecution.ID, Issue: in.IssueNum,
			Owner: in.Owner, Repo: in.Repo,
			Route: route, Complexity: est.Complexity, Confidence: est.Confidence,
			TerminalState: stateBlocked,
			Attempts:      lastAttempts, ReviewRounds: lastReviewRounds,
		})
	}

	// -- §49 CI gate + merge: the issue may close ONLY after the PR actually
	// merges. A non-green CI gate or a merge failure escalates to
	// HUMAN_REQUIRED and never closes (no silent success, §40).
	transition(stateMerging)
	ciAO := workflow.ActivityOptions{
		StartToCloseTimeout: CIMaxWait(in.Lifecycle) + time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			MaximumAttempts: int32(appCfg.Worker.RetryMaxAttempts),
		},
	}
	ciCtx := workflow.WithActivityOptions(ctx, ciAO)
	var ci CIStatus
	if err := workflow.ExecuteActivity(ciCtx, WaitForCI, CIRequest{
		Owner: in.Owner, Repo: in.Repo, PrNum: pub.PRNumber,
		RequireCI: RequireCI(in.Lifecycle) && !in.Lifecycle.DryRun,
		MaxWait:   in.Lifecycle.Merge.CIMaxWait,
	}).Get(ciCtx, &ci); err != nil {
		// Bounded Temporal retries exhausted → permanent failure → escalate.
		workflow.GetLogger(ctx).Error("CI gate activity failed → HUMAN_REQUIRED", "pr", pub.PRNumber, "err", err)
		return escalateTerminal(reasonCIFailed)
	}
	if !ci.Green {
		workflow.GetLogger(ctx).Error("CI gate not green → HUMAN_REQUIRED", "pr", pub.PRNumber, "detail", ci.Detail)
		return escalateTerminal(reasonCIFailed)
	}

	// §55: merge is gated before the merge mutation when configured.
	if ok, err := gateStep(approvalStepMerge); err != nil {
		return err
	} else if !ok {
		return nil
	}
	transition(stateMerging)

	var merged MergeResult
	if err := workflow.ExecuteActivity(ctx, MergePullRequest, MergeRequest{
		Owner: in.Owner, Repo: in.Repo, PrNum: pub.PRNumber,
		Strategy:    MergeStrategy(in.Lifecycle),
		ExpectedSHA: reviewedSHA, // §15: refuse if the head moved past the review
		DryRun:      in.Lifecycle.DryRun,
	}).Get(ctx, &merged); err != nil || (!merged.Merged && !merged.Skipped) {
		workflow.GetLogger(ctx).Error("PR merge failed → HUMAN_REQUIRED (issue stays open)", "pr", pub.PRNumber, "err", err)
		return escalateTerminal(reasonMergeFailed)
	}

	transition(stateClosed)
	if err := workflow.ExecuteActivity(ctx, EnsureIssueState, StateRequest{
		Owner: in.Owner, Repo: in.Repo, Num: in.IssueNum, State: "closed", DryRun: in.Lifecycle.DryRun,
	}).Get(ctx, nil); err != nil {
		return err
	}
	projectLabels(stateClosed, est.Complexity)
	if err := finish(CycleOutcome{
		WorkflowID: info.WorkflowExecution.ID, Issue: in.IssueNum,
		Owner: in.Owner, Repo: in.Repo,
		Route: route, Complexity: est.Complexity, Confidence: est.Confidence,
		TerminalState:   stateClosed,
		AutonomousClose: true,
		Attempts:        lastAttempts, ReviewRounds: lastReviewRounds,
	}); err != nil {
		return err
	}
	runPostMerge(ctx, in, pub.PRNumber, reviewedSHA)
	return nil
}

// runPostMerge fires the §50 release / §51 deploy / §54 propagation after a
// successful merge, but NEVER lets a release problem change the issue's terminal
// state: the PR already merged and the issue closed, so the child's error is only
// logged (best-effort). The child is awaited (the test environment executes
// awaited children), and its ID is deterministic per repo+PR so duplicate starts
// converge (§24). sha is the reviewed/merged commit (§15) used as the §54
// propagation marker key.
func runPostMerge(ctx workflow.Context, in IssueInput, prNum int, sha string) {
	if !ReleaseEnabled(in.Lifecycle) && !DeployEnabled(in.Lifecycle) && !PropagationEnabled(in.Lifecycle) {
		return
	}
	cwo := workflow.ChildWorkflowOptions{
		WorkflowID: fmt.Sprintf("%s-postmerge-%d", in.Repo, prNum),
	}
	cctx := workflow.WithChildOptions(ctx, cwo)
	if err := workflow.ExecuteChildWorkflow(cctx, PostMergeWorkflow, PostMergeInput{
		Owner: in.Owner, Repo: in.Repo, RepoPath: in.RepoPath, PrNum: prNum,
		IssueNum:  in.IssueNum,
		Sha:       sha,
		Lifecycle: in.Lifecycle,
	}).Get(cctx, nil); err != nil {
		workflow.GetLogger(ctx).Error("post-merge release/deploy/propagation failed (non-fatal)", "pr", prNum, "err", err)
	}
}

// runLargePath implements the §8/§19 large flow: SPLITTING → RunSplit
// (validated §36) → EnsureChildren → dependency-respecting batches (§10) →
// child IssueCycleWorkflow fan-out (bounded by §43 cap) → all children complete
// → parent CLOSED. A child failure fails the whole parent (§44) and routes to
// BLOCKED — never a silent skip. Child cycles are started with the deterministic
// <repo>-issue-<childNumber> ID so sweeps/retries converge (§32/§24).
func runLargePath(ctx workflow.Context, devCtx workflow.Context, in IssueInput, a IssueAnalysis, est EstimateResult, info *workflow.Info, route string, transition func(string), projectLabels func(string, int), finish func(CycleOutcome) error) error {
	transition(stateSplitting)
	var split SplitResult
	if err := workflow.ExecuteActivity(ctx, RunSplit, in, a, est).Get(ctx, &split); err != nil {
		return err
	}
	if err := ValidateSplit(in.Lifecycle, split); err != nil {
		return err // §36 fail-closed; bounded retry, never a mutation
	}

	// Idempotently create child issues (§8/§9/§24), guarded by DryRun.
	children, err := ensureChildrenActivity(ctx, in, split)
	if err != nil {
		return err
	}

	// Dependency-respecting batches (§10), capped by §43 (default 1).
	batches, err := scheduleChildBatches(split.Tickets, in.Lifecycle.ConcurrentChildrenPerIssue())
	if err != nil {
		return err // invalid DAG → fail closed, never blind fan-out
	}

	// Execute batches in order; within a batch, run children concurrently.
	for _, batch := range batches {
		type childRun struct {
			future workflow.Future
			n      int
		}
		var runs []childRun
		for _, key := range batch {
			var num int
			for _, c := range children {
				if c.Key == key {
					num = c.GitHubIssueNumber
					break
				}
			}
			if num == 0 {
				return fmt.Errorf("child %q has no GitHub issue (EnsureChildren failed?)", key)
			}
			cwo := workflow.ChildWorkflowOptions{
				WorkflowID: fmt.Sprintf("%s-issue-%d", in.Repo, num), // deterministic (§32)
			}
			cctx := workflow.WithChildOptions(devCtx, cwo)
			childIn := IssueInput{
				Owner: in.Owner, Repo: in.Repo, IssueNum: num,
				RepoPath: in.RepoPath, Trigger: "split", Lifecycle: in.Lifecycle,
			}
			runs = append(runs, childRun{workflow.ExecuteChildWorkflow(cctx, IssueCycleWorkflow, childIn), num})
		}
		for _, r := range runs {
			if err := r.future.Get(ctx, nil); err != nil {
				workflow.GetLogger(ctx).Error("child cycle failed", "issue", r.n, "err", err)
				// §44 #1/#40: a child failure must NOT be silently skipped.
				// Aggregate into a parent BLOCKED terminal.
				transition(stateBlocked)
				return finish(CycleOutcome{
					WorkflowID: info.WorkflowExecution.ID, Issue: in.IssueNum,
					Owner: in.Owner, Repo: in.Repo,
					Route: route, Complexity: est.Complexity, Confidence: est.Confidence,
					TerminalState: stateBlocked, HumanRequired: true,
					EscalationReason: reasonAgentFailure,
				})
			}
		}
	}

	// All children complete → parent closes (completion gate is the follow-up).
	transition(stateClosed)
	if err := workflow.ExecuteActivity(ctx, EnsureIssueState, StateRequest{
		Owner: in.Owner, Repo: in.Repo, Num: in.IssueNum, State: "closed", DryRun: in.Lifecycle.DryRun,
	}).Get(ctx, nil); err != nil {
		return err
	}
	projectLabels(stateClosed, est.Complexity)
	return finish(CycleOutcome{
		WorkflowID: info.WorkflowExecution.ID, Issue: in.IssueNum,
		Owner: in.Owner, Repo: in.Repo,
		Route: route, Complexity: est.Complexity, Confidence: est.Confidence,
		TerminalState:   stateClosed,
		AutonomousClose: true,
	})
}

// ensureChildrenActivity wraps EnsureChildren so it's replay-safe (an activity),
// respecting the parent's DryRun (§42).
func ensureChildrenActivity(ctx workflow.Context, in IssueInput, split SplitResult) ([]ChildIssue, error) {
	var children []ChildIssue
	if err := workflow.ExecuteActivity(ctx, EnsureChildren, ChildCreateRequest{
		Owner: in.Owner, Repo: in.Repo, ParentNum: in.IssueNum,
		Tickets: split.Tickets, DryRun: in.Lifecycle.DryRun,
	}).Get(ctx, &children); err != nil {
		return nil, err
	}
	return children, nil
}

// awaitApprovalGate runs the §55 pre-execution approval gate for one step: it
// polls the §18 decision channel (via the AwaitApproval activity, bounded by the
// poGateTimeout) and appends the decision to the audit sink. It only does I/O;
// the caller maps the verdict to a deterministic transition. Reused by
// IssueCycleWorkflow (publish/merge) and PostMergeWorkflow (deploy).
func awaitApprovalGate(ctx workflow.Context, owner, repo string, num int, step string, lc LifecycleConfig) (ApprovalResult, error) {
	gateCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: POGateTimeout(lc) + time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			MaximumAttempts: int32(appCfg.Worker.RetryMaxAttempts),
		},
	})
	var res ApprovalResult
	if err := workflow.ExecuteActivity(gateCtx, AwaitApproval, ApprovalRequest{
		Owner: owner, Repo: repo, Num: num, Step: step,
		Timeout: POGateTimeout(lc).String(), DryRun: lc.DryRun,
	}).Get(gateCtx, &res); err != nil {
		return ApprovalResult{}, err
	}
	if err := recordAudit(ctx, lc.Audit, AuditEvent{
		Owner: owner, Repo: repo, Issue: num, Step: step,
		Actor: auditActor(res), Decision: auditDecision(res), Detail: res.Detail,
	}); err != nil {
		return ApprovalResult{}, err
	}
	return res, nil
}

// recordAudit emits one §55 audit event (activity; all I/O outside the workflow).
func recordAudit(ctx workflow.Context, cfg AuditConfig, ev AuditEvent) error {
	return workflow.ExecuteActivity(ctx, RecordAuditEvent, AuditRequest{Event: ev, Config: cfg}).Get(ctx, nil)
}

// auditDecision maps a gate verdict to the audit record's decision field.
func auditDecision(res ApprovalResult) string {
	switch {
	case res.Granted || res.Skipped:
		return "allow"
	case res.Denied:
		return "deny"
	default:
		return "timeout"
	}
}

// auditActor names who caused the decision: a human for an explicit approval/
// denial, the deterministic policy for a timeout (or a shadow-mode skip).
func auditActor(res ApprovalResult) string {
	if res.Granted || res.Denied {
		return "human"
	}
	return "policy"
}

// recordTerminal records the §39 terminal metrics for a finished cycle and
// returns nil. All terminal paths funnel through it so the sink always sees the
// run's terminal state + autonomous-vs-human outcome (no silent drops). It also
// lands the #83 outcome.json evidence (best-effort, never changes the state).
func recordTerminal(ctx workflow.Context, info *workflow.Info, in IssueInput, out CycleOutcome) error {
	// #84: fill the explicit three-outcome evidence (defaults to SKIPPED, never
	// silently absent §40) and stamp the ordered conditions deterministically.
	out = stampOutcome(out, workflow.Now(ctx))
	// #83: the cycle ID is the run's deterministic workflow ID (or the explicit
	// child ID for a sweep-escalated failure), so evidence is stable under retry.
	cycleID := out.WorkflowID
	if cycleID == "" {
		cycleID = info.WorkflowExecution.ID
	}
	if err := workflow.ExecuteActivity(ctx, WriteCycleEvidence, EvidenceRequest{
		CycleID: cycleID, DryRun: in.Lifecycle.DryRun, Outcome: evidenceJSON(out),
	}).Get(ctx, nil); err != nil {
		workflow.GetLogger(ctx).Error("outcome evidence write failed (non-fatal)", "err", err)
		if out.EvidenceError == "" {
			out.EvidenceError = err.Error()
		}
	}
	if err := workflow.ExecuteActivity(ctx, RecordCycleMetrics, out).Get(ctx, nil); err != nil {
		return err
	}
	// §53 failure feedback: a terminal BLOCKED/FAILED run files at most one
	// idempotent bug issue (only when the repo opted into alerting). Best-effort:
	// a feedback problem never changes the run's authoritative terminal state.
	startFailureFeedback(ctx, in, out)
	return nil
}

// prBody renders the pull-request body from the structured IssueAnalysis (§35)
// as markdown — summary + requirements + acceptance criteria + constraints +
// risks — instead of a single raw paragraph. Deterministic; no agent prose
// beyond the validated fields.
func prBody(in IssueInput, a IssueAnalysis) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Closes %s issue #%d\n\n", in.Repo, in.IssueNum)
	fmt.Fprintf(&b, "## Summary\n%s\n", a.Summary)
	if len(a.Requirements) > 0 {
		b.WriteString("\n## Requirements\n")
		for _, r := range a.Requirements {
			fmt.Fprintf(&b, "- %s\n", r)
		}
	}
	if len(a.AcceptanceCriteria) > 0 {
		b.WriteString("\n## Acceptance criteria\n")
		for _, c := range a.AcceptanceCriteria {
			fmt.Fprintf(&b, "- %s\n", c)
		}
	}
	if len(a.Constraints) > 0 {
		b.WriteString("\n## Constraints\n")
		for _, c := range a.Constraints {
			fmt.Fprintf(&b, "- %s\n", c)
		}
	}
	if len(a.Risks) > 0 {
		b.WriteString("\n## Risks\n")
		for _, r := range a.Risks {
			fmt.Fprintf(&b, "- %s\n", r)
		}
	}
	if len(a.Assumptions) > 0 {
		b.WriteString("\n## Assumptions\n")
		for _, x := range a.Assumptions {
			fmt.Fprintf(&b, "- %s\n", x)
		}
	}
	return b.String()
}

// IssueAllWorkflow is the issue-sweep analog of ReviewAllWorkflow (§32): lists
// open issues missing the konoha:managed label and starts an
// IssueCycleWorkflow per issue as a child with a stable, repo-specific ID
// (<repo>-issue-<n>). Stable IDs make duplicate sweep starts harmless: a
// second start of an already-running issue fails with
// WorkflowExecutionAlreadyStarted and is skipped.
func IssueAllWorkflow(ctx workflow.Context, scope RepoScope) error {
	ao := workflow.ActivityOptions{
		StartToCloseTimeout: appCfg.Worker.sweepWindow(),
		RetryPolicy: &temporal.RetryPolicy{
			MaximumAttempts: int32(appCfg.Worker.SweepRetryMaxAttempts),
		},
	}
	ctx = workflow.WithActivityOptions(ctx, ao)

	var pending []IssueInput
	if err := workflow.ExecuteActivity(ctx, ListUnmanagedIssues, scope).Get(ctx, &pending); err != nil {
		return err
	}
	workflow.GetLogger(ctx).Info("IssueAll sweep", "repo", scope.Owner+"/"+scope.Repo, "pendingIssues", len(pending))

	// §43: never more than `maxConcurrentIssueCycles` child cycles in flight per
	// repo. Stable child IDs keep duplicate sweeps harmless either way.
	type run struct {
		in     IssueInput
		cctx   workflow.Context
		future workflow.ChildWorkflowFuture
	}
	for _, b := range batchRuns(len(pending), scope.Lifecycle.ConcurrentIssueCycles()) {
		var batch []run
		for _, in := range pending[b[0]:b[1]] {
			cwo := workflow.ChildWorkflowOptions{
				WorkflowID: fmt.Sprintf("%s-issue-%d", scope.Repo, in.IssueNum), // repo-specific, stable
			}
			cctx := workflow.WithChildOptions(ctx, cwo)
			batch = append(batch, run{in: in, cctx: cctx, future: workflow.ExecuteChildWorkflow(cctx, IssueCycleWorkflow, in)})
		}
		for _, x := range batch {
			if err := x.future.Get(x.cctx, nil); err != nil {
				escalateFailedChild(ctx, x.in, err)
				continue
			}
			workflow.GetLogger(ctx).Info("cycle done", "issue", x.in.IssueNum)
		}
	}
	return nil
}

// escalateFailedChild surfaces a failed sweep child instead of silently dropping
// it (§40/§44 #1). A child cycle that errors never reached a terminal, so the
// parent projects the affected issue to the human-required/blocked terminal via
// the idempotent ApplyIssueLabels reconcile (re-running the sweep or retrying
// the child converges, §24/§31; ApplyIssueLabels is a no-op under DryRun §42),
// and starts the §53 failure-feedback path for the failed run (one deduped bug
// issue keyed by `<!-- konoha-failure: <repo>-issue-<n> -->`). Best-effort:
// neither a projection nor a feedback problem is allowed to abort the batch or
// the sweep — the remaining children still run (§40 fail-closed, not fail-fast).
func escalateFailedChild(ctx workflow.Context, in IssueInput, cause error) {
	workflow.GetLogger(ctx).Error("child cycle failed, escalating", "issue", in.IssueNum, "err", cause)
	if err := workflow.ExecuteActivity(ctx, ApplyIssueLabels, LabelsRequest{
		Owner: in.Owner, Repo: in.Repo, Num: in.IssueNum,
		Labels: ProjectLabels(in.Lifecycle, stateBlocked, 0, true),
		DryRun: in.Lifecycle.DryRun,
	}).Get(ctx, nil); err != nil {
		workflow.GetLogger(ctx).Error("child-failure label projection failed (non-fatal)", "issue", in.IssueNum, "err", err)
	}
	// §39/§53: record the failed run's terminal (metrics + failure feedback),
	// exactly like the LARGE path's recordTerminal. Best-effort so a metrics or
	// feedback problem never aborts the batch or the sweep.
	if err := recordTerminal(ctx, workflow.GetInfo(ctx), in, failedChildOutcome(in)); err != nil {
		workflow.GetLogger(ctx).Error("child-failure terminal record failed (non-fatal)", "issue", in.IssueNum, "err", err)
	}
}

// failedChildOutcome is the deterministic §40/§53 projection for a failed sweep
// child. The WorkflowID is the child's stable ID (<repo>-issue-<n>), which is
// both the §53 failure marker key and the dedupe key, so a retried sweep files
// at most one bug issue; the terminal is BLOCKED (the state the §53 feedback
// path accepts) with the human-required flag set. Pure, so the invariant test
// can assert a failure always has a concrete projection.
func failedChildOutcome(in IssueInput) CycleOutcome {
	return CycleOutcome{
		WorkflowID: fmt.Sprintf("%s-issue-%d", in.Repo, in.IssueNum), Issue: in.IssueNum,
		Owner: in.Owner, Repo: in.Repo,
		TerminalState: stateBlocked, HumanRequired: true,
		// #84: a failed child/agent never ran verification — record it as SKIPPED
		// explicitly (never silently absent, §40).
		AgentOutcome:        agentOutcomeFailed,
		VerificationOutcome: verificationSkipped,
		EscalationReason:    reasonAgentFailure,
	}
}
