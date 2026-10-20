package konoha

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"
)

// #212: local sequential runtime. Temporal remains the default durable path; the
// local runtime drives the SAME activity functions and pure policy functions
// in-process, in the same SMALL-path order as IssueCycleWorkflow, so a host can
// run Konoha headless with no Temporal server. It has no durability or replay:
// see docs/runtime.md for the non-guarantees.

// localActivities is the injectable seam over the real activity functions. The
// local runtime never shells out itself; every I/O step is one of these calls,
// so tests drive the whole cycle without gh/opencode/PATH (mirrors ghRunner).
type localActivities struct {
	analyze           func(context.Context, IssueInput) (IssueAnalysis, error)
	estimate          func(context.Context, IssueInput, IssueAnalysis) (EstimateResult, error)
	ensureManagedNote func(context.Context, ManagedNoteRequest) error
	applyLabels       func(context.Context, LabelsRequest) error
	fetchSnapshot     func(context.Context, IssueInput) (IssueSnapshot, error)
	fetchLabels       func(context.Context, IssueInput) ([]string, error)
	developer         func(context.Context, DevelopRequest) (DevelopResult, error)
	repair            func(context.Context, RepairRequest) (DevelopResult, error)
	publish           func(context.Context, PublishInput) (PublishResult, error)
	prHead            func(context.Context, PrInput) (string, error)
	prDiff            func(context.Context, PrInput) (string, error)
	reviewer          func(context.Context, ReviewRequest) (ReviewResult, error)
	prDescription     func(context.Context, PRDescriptionRequest) (PRDescriptionResult, error)
	waitCI            func(context.Context, CIRequest) (CIStatus, error)
	merge             func(context.Context, MergeRequest) (MergeResult, error)
	ensureIssueState  func(context.Context, StateRequest) error
	awaitApproval     func(context.Context, ApprovalRequest) (ApprovalResult, error)
	recordAudit       func(context.Context, AuditRequest) error
	cleanup           func(context.Context, CleanupRequest) (CleanupResult, error)
	writeEvidence     func(context.Context, EvidenceRequest) error
	recordActivation  func(context.Context, ActivationRequest) error
	recordChange      func(context.Context, ChangeRequest) error
	recordMetrics     func(context.Context, CycleOutcome) error
}

// realLocalActivities wires the production activity functions. Names are the
// registered Temporal activity funcs (never renamed, so the type names stay put).
func realLocalActivities() localActivities {
	return localActivities{
		analyze:           AnalyzeIssue,
		estimate:          EstimateIssue,
		ensureManagedNote: EnsureManagedNote,
		applyLabels:       ApplyIssueLabels,
		fetchSnapshot:     FetchIssueSnapshot,
		fetchLabels:       FetchIssueLabels,
		developer:         RunDeveloper,
		repair:            RunRepair,
		publish:           PublishDeveloperChange,
		prHead:            FetchPRHead,
		prDiff:            FetchPRDiff,
		reviewer:          RunReviewer,
		prDescription:     RunPRDescription,
		waitCI:            WaitForCI,
		merge:             MergePullRequest,
		ensureIssueState:  EnsureIssueState,
		awaitApproval:     AwaitApproval,
		recordAudit:       RecordAuditEvent,
		cleanup:           CleanupWorktree,
		writeEvidence:     WriteCycleEvidence,
		recordActivation:  RecordActivation,
		recordChange:      RecordChange,
		recordMetrics:     RecordCycleMetrics,
	}
}

// localActivitiesFor is the test seam for the whole activity set.
var localActivitiesFor = realLocalActivities

// localRuntime executes cycles sequentially in-process.
type localRuntime struct {
	cfg  RuntimeConfig
	acts localActivities
	now  func() time.Time
	logf func(string, ...interface{})
}

func newLocalRuntime(cfg AppConfig, acts localActivities) *localRuntime {
	return &localRuntime{cfg: cfg.Runtime, acts: acts, now: time.Now, logf: log.Printf}
}

// localCycleID is the deterministic per-issue run ID, identical to the Temporal
// child workflow ID, so evidence/lineage/change records and idempotent GitHub
// mutations converge between the two runtimes.
func localCycleID(in IssueInput) string {
	return fmt.Sprintf("%s-issue-%d", in.Repo, in.IssueNum)
}

// call applies the #95 retry class in-process: Temporal retries are driven by the
// server, so the local runtime reproduces the attempt count/backoff from the same
// retryPolicyFor contract. An agent class stays at one attempt (never replay an
// LLM), a deterministic class at one (never double-count), infrastructure and
// verification retry with backoff.
func (r *localRuntime) call(ctx context.Context, c retryClass, op func() error) error {
	p := retryPolicyFor(c)
	attempts := 1
	interval := time.Second
	if p != nil {
		if p.MaximumAttempts > 0 {
			attempts = int(p.MaximumAttempts)
		}
		if p.InitialInterval > 0 {
			interval = p.InitialInterval
		}
	}
	var err error
	for i := 0; i < attempts; i++ {
		if err = op(); err == nil {
			return nil
		}
		if i == attempts-1 {
			break
		}
		if err := localRetryWait(ctx, interval); err != nil {
			return err
		}
		if p != nil && p.BackoffCoefficient > 1 {
			interval = time.Duration(float64(interval) * p.BackoffCoefficient)
		}
		if p != nil && p.MaximumInterval > 0 && interval > p.MaximumInterval {
			interval = p.MaximumInterval
		}
	}
	return err
}

// localRetryWait is the backoff sleep seam (tests shrink it without waiting).
var localRetryWait = func(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

// callValue is call for a value-returning activity.
func callValue[T any](r *localRuntime, ctx context.Context, c retryClass, op func() (T, error)) (T, error) {
	var out T
	err := r.call(ctx, c, func() error {
		v, e := op()
		if e != nil {
			return e
		}
		out = v
		return nil
	})
	return out, err
}

// localLogger adapts r.logf to the narrow §39 workflowLogger surface.
type localLogger func(string, ...interface{})

func (f localLogger) Info(msg string, kv ...interface{}) { f("%s %v", msg, kv) }

// runCycle executes one SMALL-path cycle. Every best-effort write (evidence,
// lineage, change, labels, managed note) logs and continues; a failed worktree
// teardown is recorded but never changes the terminal state (§40).
func (r *localRuntime) runCycle(ctx context.Context, in IssueInput) error {
	id := localCycleID(in)
	start := r.now()
	resources, err := resolveResources(appCfg.Defaults.Agents, in.Lifecycle)
	if err != nil {
		return err
	}

	lastComplexity, lastConfidence := 0, ""
	lastAttempts, lastReviewRounds := 0, 0
	transition := func(s string) {
		logCycleEvent(localLogger(r.logf), id, in.IssueNum, in.Repo, s, lastComplexity, lastConfidence, lastAttempts, lastReviewRounds)
	}

	evidenceErr := ""
	writeEvidence := func(req EvidenceRequest) {
		req.CycleID = id
		req.DryRun = in.Lifecycle.DryRun
		if err := r.call(ctx, retryInfrastructure, func() error { return r.acts.writeEvidence(ctx, req) }); err != nil {
			if evidenceErr == "" {
				evidenceErr = err.Error()
			}
			r.logf("local: evidence write failed (non-fatal): %v", err)
		}
	}
	recordActivation := func(act LineageActivation) {
		req := ActivationRequest{Owner: in.Owner, Repo: in.Repo, Issue: in.IssueNum, Activation: act, DryRun: in.Lifecycle.DryRun}
		if err := r.call(ctx, retryInfrastructure, func() error { return r.acts.recordActivation(ctx, req) }); err != nil {
			r.logf("local: lineage activation write failed (non-fatal): %v", err)
		}
	}
	recordChange := func(req ChangeRequest) {
		req.Owner, req.Repo, req.Issue = in.Owner, in.Repo, in.IssueNum
		req.WorkflowID = id
		req.DryRun = in.Lifecycle.DryRun
		if err := r.call(ctx, retryInfrastructure, func() error { return r.acts.recordChange(ctx, req) }); err != nil {
			r.logf("local: change record write failed (non-fatal): %v", err)
		}
	}
	projectLabels := func(s string, c int) {
		if in.Lifecycle.DryRun {
			return
		}
		req := LabelsRequest{Owner: in.Owner, Repo: in.Repo, Num: in.IssueNum, Labels: ProjectLabels(in.Lifecycle, s, c, true)}
		if err := r.call(ctx, retryInfrastructure, func() error { return r.acts.applyLabels(ctx, req) }); err != nil {
			r.logf("local: label projection failed (non-fatal): %v", err)
		}
	}

	// §87/#102/#94: per-cycle aggregates carried into the terminal record.
	var usage HarnessUsage
	var limitsEv *LimitEvidence
	verificationMutated := false
	agentOut, verifOut := AgentOutcome(""), VerificationOutcome("")

	cleanupDone := false
	var cleanup *CleanupResult
	runCleanup := func() {
		if cleanupDone {
			return
		}
		cleanupDone = true
		var res CleanupResult
		req := CleanupRequest{RepoPath: in.RepoPath, Repo: in.Repo, IssueNum: in.IssueNum, WorkflowID: id}
		if err := r.call(context.Background(), retryDeterministic, func() error {
			var e error
			res, e = r.acts.cleanup(context.Background(), req)
			return e
		}); err != nil {
			res = CleanupResult{Attempted: true, Outcome: cleanupFailed, Error: Redact(err.Error())}
		}
		cleanup = &res
		if res.Error != "" || res.Outcome == cleanupFailed {
			r.logf("local: worktree cleanup failed (non-fatal): outcome=%s remaining=%d err=%s", res.Outcome, res.Remaining, res.Error)
		}
	}
	defer runCleanup()

	finish := func(out CycleOutcome) error {
		elapsed := r.now().Sub(start)
		runCleanup()
		out = assembleTerminal(in, out, terminalInputs{
			AgentOutcome:        agentOut,
			VerificationOutcome: verifOut,
			Usage:               usage,
			Limits:              limitsEv,
			VerificationMutated: verificationMutated,
			EvidenceError:       evidenceErr,
			Cleanup:             cleanup,
			Elapsed:             elapsed,
			Resources:           resources,
		})
		return r.recordTerminal(ctx, in, id, out)
	}

	transition(stateAnalyzing)
	var a IssueAnalysis
	if a, err = callValue(r, ctx, retryAgent, func() (IssueAnalysis, error) { return r.acts.analyze(ctx, in) }); err != nil {
		return err
	}

	transition(stateEstimating)
	var est EstimateResult
	if est, err = callValue(r, ctx, retryAgent, func() (EstimateResult, error) { return r.acts.estimate(ctx, in, a) }); err != nil {
		return err
	}
	lastComplexity, lastConfidence = est.Complexity, est.Confidence

	// -- routing (§11) --
	route := stateLarge
	if est.Complexity >= 1 && est.Complexity <= effectiveDevelopmentMax(in.Lifecycle) {
		route = stateSmall
	}
	transition(route)
	if route == stateLarge {
		return finish(CycleOutcome{
			WorkflowID: id, Issue: in.IssueNum, Owner: in.Owner, Repo: in.Repo,
			Route: route, Complexity: est.Complexity, Confidence: est.Confidence,
			TerminalState: stateNotWired,
		})
	}
	projectLabels(route, est.Complexity)

	model := resources.modelFor(stageDeveloper).AgentModel()
	if model == "" {
		model = defaultAgentModel
	}
	devModel := resources.modelFor(stageDeveloper)
	repairModel := resources.modelFor(stageRepair)
	reviewModel := resources.modelFor(stageReviewer)
	effHarness := effectiveHarnessName(in.Lifecycle.Harness)
	// claimReleased is the #141 checkpoint: a closed/labeled issue stops the run
	// before a GitHub mutation. A dryRun cycle publishes nothing, so it is skipped.
	claimReleased := func() (bool, error) {
		if in.Lifecycle.DryRun {
			return false, nil
		}
		snap, err := callValue(r, ctx, retryInfrastructure, func() (IssueSnapshot, error) { return r.acts.fetchSnapshot(ctx, in) })
		if err != nil {
			return false, err
		}
		return releasedClaim(snap.State, snap.Labels, in.Lifecycle.Reconcile), nil
	}
	cancelledTerminal := func(reason string) error {
		transition(stateCancelled)
		projectLabels(stateCancelled, est.Complexity)
		return finish(CycleOutcome{
			WorkflowID: id, Issue: in.IssueNum, Owner: in.Owner, Repo: in.Repo,
			Route: route, Complexity: est.Complexity, Confidence: est.Confidence,
			TerminalState: stateCancelled, CancelReason: reason,
			Attempts: lastAttempts, ReviewRounds: lastReviewRounds,
		})
	}
	escalateTerminal := func(reason string) error {
		transition(stateHumanRequired)
		projectLabels(stateHumanRequired, est.Complexity)
		return finish(CycleOutcome{
			WorkflowID: id, Issue: in.IssueNum, Owner: in.Owner, Repo: in.Repo,
			Route: route, Complexity: est.Complexity, Confidence: est.Confidence,
			TerminalState: stateHumanRequired, HumanRequired: true,
			EscalationReason: reason,
			Attempts:         lastAttempts, ReviewRounds: lastReviewRounds,
		})
	}
	blockedTerminal := func(reason string) error {
		transition(stateHumanRequired)
		projectLabels(stateHumanRequired, est.Complexity)
		return finish(CycleOutcome{
			WorkflowID: id, Issue: in.IssueNum, Owner: in.Owner, Repo: in.Repo,
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
	// gateStep enforces the §55 pre-execution gate with the local approvalWait.
	gateStep := func(step string) (bool, *ApprovalResult, error) {
		if !ApprovalRequired(in.Lifecycle, step) {
			return true, nil, nil
		}
		transition(statePendingApproval)
		projectLabels(statePendingApproval, est.Complexity)
		recordChange(ChangeRequest{State: statePendingApproval})
		req := ApprovalRequest{
			Owner: in.Owner, Repo: in.Repo, Num: in.IssueNum, Step: step,
			Timeout: r.cfg.Local.approvalWait().String(), DryRun: in.Lifecycle.DryRun,
		}
		res, err := callValue(r, ctx, retryInfrastructure, func() (ApprovalResult, error) { return r.acts.awaitApproval(ctx, req) })
		if err != nil {
			return false, nil, err
		}
		if err := r.call(ctx, retryDeterministic, func() error {
			return r.acts.recordAudit(ctx, AuditRequest{Event: AuditEvent{
				Owner: in.Owner, Repo: in.Repo, Issue: in.IssueNum, Step: step,
				Actor: auditActor(res), Decision: auditDecision(res), Detail: res.Detail,
			}, Config: in.Lifecycle.Audit})
		}); err != nil {
			return false, nil, err
		}
		if res.Granted || res.Skipped {
			return true, nil, nil
		}
		if res.Rework {
			return false, &res, nil
		}
		if res.Denied {
			recordChange(ChangeRequest{ReworkNote: res.Detail})
			transition(stateHumanRequired)
			projectLabels(stateHumanRequired, est.Complexity)
			return false, nil, finish(CycleOutcome{
				WorkflowID: id, Issue: in.IssueNum, Owner: in.Owner, Repo: in.Repo,
				Route: route, Complexity: est.Complexity, Confidence: est.Confidence,
				TerminalState: stateHumanRequired, HumanRequired: true,
				EscalationReason: reasonApprovalDenied,
				Attempts:         lastAttempts, ReviewRounds: lastReviewRounds,
			})
		}
		transition(stateBlocked)
		projectLabels(stateBlocked, est.Complexity)
		return false, nil, finish(CycleOutcome{
			WorkflowID: id, Issue: in.IssueNum, Owner: in.Owner, Repo: in.Repo,
			Route: route, Complexity: est.Complexity, Confidence: est.Confidence,
			TerminalState: stateBlocked,
			Attempts:      lastAttempts, ReviewRounds: lastReviewRounds,
		})
	}

	noteReq := ManagedNoteRequest{
		Owner: in.Owner, Repo: in.Repo, IssueNum: in.IssueNum,
		WorkflowID: id, Trigger: in.Trigger, Route: route, Complexity: est.Complexity,
		Model: model, DryRun: in.Lifecycle.DryRun,
	}
	if err := r.call(ctx, retryInfrastructure, func() error { return r.acts.ensureManagedNote(ctx, noteReq) }); err != nil {
		r.logf("local: managed claim note failed (non-fatal): %v", err)
	}

	developReq := DevelopRequest{
		Owner: in.Owner, Repo: in.Repo, IssueNum: in.IssueNum, RepoPath: in.RepoPath,
		Model:              devModel.AgentModel(),
		Harness:            in.Lifecycle.Harness,
		WorkflowID:         id,
		Title:              a.Title,
		Body:               a.Body,
		Spec:               a.Spec,
		Requirements:       a.Requirements,
		AcceptanceCriteria: a.AcceptanceCriteria,
		Constraints:        a.Constraints,
		Assumptions:        a.Assumptions,
		OpenQuestions:      a.OpenQuestions,
		Validation:         ValidationGates(in.Lifecycle),
		ValidationCmd:      ValidationCommand(in.Lifecycle),
		Limits:             in.Lifecycle.Limits,
		Tooling:            in.Lifecycle.Tooling,
	}

	// -- §4.4 DEVELOPING --
	transition(stateDeveloping)
	var dev DevelopResult
	if dev, err = callValue(r, ctx, retryAgent, func() (DevelopResult, error) { return r.acts.developer(ctx, developReq) }); err != nil {
		return err
	}
	lastAttempts++
	if dev.Limits != nil {
		limitsEv = dev.Limits
	}
	agentOut, verifOut = agentOutcomeOf(dev), verificationOutcomeOf(dev)
	verificationMutated = dev.VerificationMutated
	usage.add(dev.Usage)
	recordActivation(LineageActivation{
		Attempt: lastAttempts, WorkflowID: id,
		Reason: activationReasonInitial, Stage: stageDeveloper, Harness: effHarness, Model: devModel.AgentModel(),
		ModelDigest: devModel.Digest,
		BaseSHA:     dev.BaseSHA, CommitSHA: dev.CommitSHA, Result: string(agentOut),
		TokensIn: dev.Usage.TokensIn, TokensOut: dev.Usage.TokensOut, CostUSD: dev.Usage.CostUSD,
	})
	writeAgentEvidence(writeEvidence, dev.Evidence, dev, dev.ValidationOutput)
	if dev.Blocked {
		return blockedTerminal(dev.BlockedReason)
	}
	// repairWith runs a fresh repair on top of the reviewed/previous commit and
	// tracks it exactly like the workflow (usage, limits, lineage, evidence).
	repairWith := func(previous string, rev ReviewResult, explanation string) (DevelopResult, error) {
		req := developReq
		req.Model = repairModel.AgentModel()
		rr := RepairRequest{Develop: req, Review: rev, PreviousCommit: previous}
		res, err := callValue(r, ctx, retryAgent, func() (DevelopResult, error) { return r.acts.repair(ctx, rr) })
		if err != nil {
			return DevelopResult{}, err
		}
		if res.Limits != nil {
			limitsEv = res.Limits
		}
		usage.add(res.Usage)
		agentOut, verifOut = agentOutcomeOf(res), verificationOutcomeOf(res)
		verificationMutated = res.VerificationMutated
		recordActivation(LineageActivation{
			Attempt: lastAttempts, WorkflowID: fmt.Sprintf("%s-repair-%d", id, lastAttempts),
			Reason: activationReasonRepair, Stage: stageRepair, Harness: effHarness, Model: repairModel.AgentModel(),
			ModelDigest: repairModel.Digest, BaseSHA: res.BaseSHA, CommitSHA: res.CommitSHA,
			Result: string(agentOut), ReworkNote: explanation,
			TokensIn: res.Usage.TokensIn, TokensOut: res.Usage.TokensOut, CostUSD: res.Usage.CostUSD,
		})
		writeAgentEvidence(writeEvidence, res.Evidence, res, res.ValidationOutput)
		return res, nil
	}

	// -- #141 claim checkpoint before any GitHub mutation --
	if released, err := claimReleased(); err != nil {
		return err
	} else if released {
		return cancelledTerminal(reasonClaimReleased)
	}

	// -- §20 risk gate before any GitHub mutation --
	var labels []string
	if labels, err = callValue(r, ctx, retryInfrastructure, func() ([]string, error) { return r.acts.fetchLabels(ctx, in) }); err != nil {
		return err
	}
	if rk := DetectHumanEscalation(in.Lifecycle, dev.ChangedFiles, labels); rk.Escalate {
		transition(stateHumanRequired)
		projectLabels(stateHumanRequired, est.Complexity)
		return finish(CycleOutcome{
			WorkflowID: id, Issue: in.IssueNum, Owner: in.Owner, Repo: in.Repo,
			Route: route, Complexity: est.Complexity, Confidence: est.Confidence,
			TerminalState: stateHumanRequired, HumanRequired: true,
			EscalationReason: reasonPolicyViolation,
			Attempts:         lastAttempts, ReviewRounds: lastReviewRounds,
		})
	}

	// -- §55 publish gate (a request-changes decision repairs and re-gates) --
	for {
		ok, rw, err := gateStep(approvalStepPublish)
		if err != nil {
			return err
		}
		if ok {
			break
		}
		if rw == nil {
			return nil // deny/timeout already recorded a terminal
		}
		if rl := EvaluateRepairLoop(in.Lifecycle, lastAttempts); !rl.Allowed {
			transition(stateBlocked)
			projectLabels(stateBlocked, est.Complexity)
			return finish(CycleOutcome{
				WorkflowID: id, Issue: in.IssueNum, Owner: in.Owner, Repo: in.Repo,
				Route: route, Complexity: est.Complexity, Confidence: est.Confidence,
				TerminalState: stateBlocked,
				Attempts:      lastAttempts, ReviewRounds: lastReviewRounds,
			})
		}
		lastAttempts++
		transition(stateChangesRequested)
		projectLabels(stateChangesRequested, est.Complexity)
		feedback := strings.TrimSpace(rw.Feedback)
		if feedback == "" {
			feedback = strings.TrimSpace(rw.Detail)
		}
		if feedback == "" {
			feedback = "human requested changes at the publish gate"
		}
		recordChange(ChangeRequest{
			ReworkNote: feedback,
			Feedback: []ChangeFeedback{{
				Key: fmt.Sprintf("%s/human-rework/%d", id, lastAttempts), Kind: feedbackKindReworkNote,
				Body: feedback, At: r.now(), Author: "human",
			}},
		})
		reworked, err := repairWith(dev.CommitSHA, ReviewResult{Verdict: "REQUEST_CHANGES", Blocking: reworkBlocking(feedback, rw.Comments)}, feedback)
		if err != nil {
			return err
		}
		if reworked.Blocked {
			return blockedTerminal(reworked.BlockedReason)
		}
		dev = reworked
	}

	// -- #141 the gate can wait for a long time; re-check the claim --
	if released, err := claimReleased(); err != nil {
		return err
	} else if released {
		return cancelledTerminal(reasonClaimReleased)
	}

	// -- publish (idempotent PR, §25) --
	prBodyText := prBody(in, a)
	if AIDescriptionEnabled(in.Lifecycle) {
		descReq := PRDescriptionRequest{
			Owner: in.Owner, Repo: in.Repo, IssueNum: in.IssueNum, RepoPath: in.RepoPath,
			Model: devModel.AgentModel(), Harness: in.Lifecycle.Harness,
			Title: a.Title, Body: a.Body, Spec: a.Spec,
			Requirements: a.Requirements, AcceptanceCriteria: a.AcceptanceCriteria, Constraints: a.Constraints,
			DeveloperSummary: dev.Summary, Concerns: dev.Concerns, ChangedFiles: dev.ChangedFiles,
			ValidationOutput: dev.ValidationOutput, Limits: in.Lifecycle.Limits, Tooling: in.Lifecycle.Tooling,
		}
		if desc, err := callValue(r, ctx, retryAgent, func() (PRDescriptionResult, error) { return r.acts.prDescription(ctx, descReq) }); err != nil {
			r.logf("local: PR description generation failed; using deterministic body (§25): %v", err)
		} else if desc.Body != "" {
			prBodyText = desc.Body
			writeAgentEvidence(writeEvidence, desc.Evidence, desc, "")
		}
	}

	pubIn := PublishInput{
		Owner: in.Owner, Repo: in.Repo, RepoPath: in.RepoPath,
		CommitSHA:      dev.CommitSHA,
		Branch:         fmt.Sprintf("konoha/issue-%d", in.IssueNum),
		BaseBranch:     "main",
		Title:          a.Title,
		Body:           prBodyText,
		ValidationCmd:  "",
		AllowOnFailure: false,
		DryRun:         in.Lifecycle.DryRun,
	}
	var pub PublishResult
	if pub, err = callValue(r, ctx, retryInfrastructure, func() (PublishResult, error) { return r.acts.publish(ctx, pubIn) }); err != nil {
		return err
	}

	// -- review (§15) --
	var approved ReviewResult
	reviewedSHA := ""
	for {
		transition(stateReviewing)
		projectLabels(stateReviewing, est.Complexity)
		headIn := PrInput{Owner: in.Owner, Repo: in.Repo, PrNum: pub.PRNumber}
		headSha, err := callValue(r, ctx, retryInfrastructure, func() (string, error) { return r.acts.prHead(ctx, headIn) })
		if err != nil {
			return err
		}
		diff, err := callValue(r, ctx, retryInfrastructure, func() (string, error) { return r.acts.prDiff(ctx, headIn) })
		if err != nil {
			return err
		}
		reviewedSHA = headSha
		revReq := ReviewRequest{
			Owner: in.Owner, Repo: in.Repo, PRNumber: pub.PRNumber, RepoPath: in.RepoPath,
			Model:              reviewModel.AgentModel(),
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
			Limits:             in.Lifecycle.Limits,
			Tooling:            in.Lifecycle.Tooling,
		}
		var rev ReviewResult
		if rev, err = callValue(r, ctx, retryAgent, func() (ReviewResult, error) { return r.acts.reviewer(ctx, revReq) }); err != nil {
			return err
		}
		lastReviewRounds++
		if rev.Limits != nil {
			limitsEv = rev.Limits
		}
		usage.add(rev.Usage)
		e := evidenceOf(rev.Evidence)
		writeEvidence(EvidenceRequest{Prompt: e.Prompt, Stdout: e.Stdout, Stderr: e.Stderr,
			Result: evidenceResultJSON(rev), Diff: diff})
		rework := ""
		if rev.Verdict != "APPROVE" {
			rework = reworkInstruction(rev)
		}
		recordChange(ChangeRequest{
			Feedback:   reviewFeedback(id, lastReviewRounds, rev),
			ReworkNote: rework,
		})
		if rev.Verdict == "APPROVE" {
			approved = rev
			break
		}
		if rl := EvaluateRepairLoop(in.Lifecycle, lastAttempts); !rl.Allowed {
			transition(stateBlocked)
			projectLabels(stateBlocked, est.Complexity)
			return finish(CycleOutcome{
				WorkflowID: id, Issue: in.IssueNum, Owner: in.Owner, Repo: in.Repo,
				Route: route, Complexity: est.Complexity, Confidence: est.Confidence,
				TerminalState: stateBlocked,
				Attempts:      lastAttempts, ReviewRounds: lastReviewRounds,
			})
		}
		lastAttempts++
		transition(stateChangesRequested)
		repaired, err := repairWith(reviewedSHA, rev, rework)
		if err != nil {
			return err
		}
		if repaired.Blocked {
			return blockedTerminal(repaired.BlockedReason)
		}
		if released, err := claimReleased(); err != nil {
			return err
		} else if released {
			return cancelledTerminal(reasonClaimReleased)
		}
		pubIn.CommitSHA = repaired.CommitSHA
		if pub, err = callValue(r, ctx, retryInfrastructure, func() (PublishResult, error) { return r.acts.publish(ctx, pubIn) }); err != nil {
			return err
		}
		dev = repaired
	}

	// -- APPROVED → §49 CI gate + merge → CLOSED --
	if !ReviewAppliesTo(approved, reviewedSHA) {
		transition(stateBlocked)
		return finish(CycleOutcome{
			WorkflowID: id, Issue: in.IssueNum, Owner: in.Owner, Repo: in.Repo,
			Route: route, Complexity: est.Complexity, Confidence: est.Confidence,
			TerminalState: stateBlocked,
			Attempts:      lastAttempts, ReviewRounds: lastReviewRounds,
		})
	}

	transition(stateMerging)
	var ci CIStatus
	ciReq := CIRequest{
		Owner: in.Owner, Repo: in.Repo, PrNum: pub.PRNumber,
		RequireCI: RequireCI(in.Lifecycle) && !in.Lifecycle.DryRun,
		MaxWait:   in.Lifecycle.Merge.CIMaxWait,
	}
	if ci, err = callValue(r, ctx, retryVerification, func() (CIStatus, error) { return r.acts.waitCI(ctx, ciReq) }); err != nil || !ci.Green {
		return escalateTerminal(reasonCIFailed)
	}

	// §55: merge is gated before the merge mutation. A request-changes decision
	// here has no in-place repair path, so it fails closed to a human.
	if ok, rw, err := gateStep(approvalStepMerge); err != nil {
		return err
	} else if rw != nil {
		recordChange(ChangeRequest{ReworkNote: rw.Feedback})
		return escalateTerminal(reasonApprovalDenied)
	} else if !ok {
		return nil
	}
	if released, err := claimReleased(); err != nil {
		return err
	} else if released {
		return cancelledTerminal(reasonClaimReleased)
	}
	transition(stateMerging)

	var merged MergeResult
	mergeReq := MergeRequest{
		Owner: in.Owner, Repo: in.Repo, PrNum: pub.PRNumber,
		Strategy:    MergeStrategy(in.Lifecycle),
		ExpectedSHA: reviewedSHA,
		DryRun:      in.Lifecycle.DryRun,
	}
	if merged, err = callValue(r, ctx, retryInfrastructure, func() (MergeResult, error) { return r.acts.merge(ctx, mergeReq) }); err != nil || (!merged.Merged && !merged.Skipped) {
		return escalateTerminal(reasonMergeFailed)
	}

	transition(stateClosed)
	closeReq := StateRequest{Owner: in.Owner, Repo: in.Repo, Num: in.IssueNum, State: "closed", DryRun: in.Lifecycle.DryRun}
	if err := r.call(ctx, retryInfrastructure, func() error { return r.acts.ensureIssueState(ctx, closeReq) }); err != nil {
		return err
	}
	projectLabels(stateClosed, est.Complexity)
	return finish(CycleOutcome{
		WorkflowID: id, Issue: in.IssueNum, Owner: in.Owner, Repo: in.Repo,
		Route: route, Complexity: est.Complexity, Confidence: est.Confidence,
		TerminalState:   stateClosed,
		AutonomousClose: true,
		Attempts:        lastAttempts, ReviewRounds: lastReviewRounds,
	})
}

// recordTerminal writes the runtime-neutral terminal records: outcome.json, the
// §39 metrics (once), and the #97 change status. The §6 state stays authoritative.
func (r *localRuntime) recordTerminal(ctx context.Context, in IssueInput, cycleID string, out CycleOutcome) error {
	out = prepareTerminalOutcome(in, out, r.now())
	if err := r.call(ctx, retryInfrastructure, func() error {
		return r.acts.writeEvidence(ctx, EvidenceRequest{CycleID: cycleID, DryRun: in.Lifecycle.DryRun, Outcome: evidenceJSON(out)})
	}); err != nil {
		if out.EvidenceError == "" {
			out.EvidenceError = err.Error()
		}
		r.logf("local: outcome evidence write failed (non-fatal): %v", err)
	}
	if err := r.call(ctx, retryDeterministic, func() error { return r.acts.recordMetrics(ctx, out) }); err != nil {
		return err
	}
	if err := r.call(ctx, retryDeterministic, func() error {
		return r.acts.recordChange(ctx, ChangeRequest{
			Owner: in.Owner, Repo: in.Repo, Issue: in.IssueNum,
			WorkflowID: cycleID, State: out.TerminalState, DryRun: in.Lifecycle.DryRun,
		})
	}); err != nil {
		r.logf("local: change status write failed (non-fatal): %v", err)
	}
	return nil
}
