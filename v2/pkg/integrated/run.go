package integrated

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/kubestellar/hive/v2/pkg/automation"
	"github.com/kubestellar/hive/v2/pkg/beads"
	hivegithub "github.com/kubestellar/hive/v2/pkg/github"
	"github.com/kubestellar/hive/v2/pkg/repair"
	"github.com/kubestellar/hive/v2/pkg/visualhive"
)

type RunOptions struct {
	StateDir string
	Timeout  time.Duration
	GitHub   *hivegithub.Client
}

type WorkflowRunEvidence struct {
	RunID            int64  `json:"run_id"`
	RunURL           string `json:"run_url"`
	HeadSHA          string `json:"head_sha"`
	Conclusion       string `json:"conclusion"`
	EvidenceArtifact int64  `json:"evidence_artifact_id"`
	BundleArtifact   int64  `json:"bundle_artifact_id"`
}

type RunResult struct {
	SchemaVersion      string                           `json:"schema_version"`
	Repository         string                           `json:"repository"`
	Workflow           WorkflowRunEvidence              `json:"workflow"`
	PostMergeWorkflow  *WorkflowRunEvidence             `json:"post_merge_workflow,omitempty"`
	Validation         visualhive.Validation            `json:"validation"`
	Lifecycle          visualhive.ApplyLifecycleResult  `json:"lifecycle"`
	PostMergeLifecycle *visualhive.ApplyLifecycleResult `json:"post_merge_lifecycle,omitempty"`
	Outbox             visualhive.OutboxProcessorResult `json:"outbox"`
	Repairs            []repair.Result                  `json:"repairs,omitempty"`
	Gates              []GateEvaluation                 `json:"gates,omitempty"`
	StartedAt          time.Time                        `json:"started_at"`
	CompletedAt        time.Time                        `json:"completed_at"`
}

type GateEvaluation struct {
	RepositoryFingerprint string                     `json:"repository_fingerprint"`
	Purpose               string                     `json:"purpose,omitempty"`
	Gate                  hivegithub.PullRequestGate `json:"gate"`
	Decision              *automation.Decision       `json:"merge_decision,omitempty"`
	MergeSHA              string                     `json:"merge_sha,omitempty"`
}

func RunOnce(ctx context.Context, options RunOptions) (RunResult, error) {
	result := RunResult{SchemaVersion: "hive.production-run.v1", StartedAt: time.Now().UTC()}
	if options.GitHub == nil || options.StateDir == "" {
		return result, fmt.Errorf("GitHub client and persistent state directory are required")
	}
	store, err := NewStore(filepath.Join(options.StateDir, "integrated"))
	if err != nil {
		return result, err
	}
	config, err := store.Load()
	if err != nil {
		return result, err
	}
	result.Repository = config.Repository
	policy := automation.Policy{
		ACMMLevel: config.ACMMLevel, Mode: automationMode(config.Automation), Paused: config.Paused,
		AllowedRepositories: []string{config.Repository}, MaxRepairAttempts: repairAttemptLimit(config),
		AllowedAutoMergePaths: config.AllowedAutoMergePaths, AllowedAutoMergeRisk: config.AllowedAutoMergeRisk,
	}
	if config.Paused {
		store.Audit(AuditEntry{Action: "run", Allowed: false, Repository: config.Repository, Detail: "repository automation is paused"})
		return result, fmt.Errorf("repository automation is paused")
	}
	if options.Timeout <= 0 {
		options.Timeout = 45 * time.Minute
	}
	releaseRun, err := acquireProductionRunLease(options.StateDir, options.Timeout+2*time.Minute)
	if err != nil {
		store.Audit(AuditEntry{Action: "run", Allowed: false, Repository: config.Repository, Detail: err.Error()})
		return result, err
	}
	defer releaseRun()
	store.Audit(AuditEntry{Action: "run", Allowed: true, Repository: config.Repository})
	runCtx, cancel := context.WithTimeout(ctx, options.Timeout)
	defer cancel()
	lifecycle, err := visualhive.NewLifecycleStore(filepath.Join(options.StateDir, "visual-hive"))
	if err != nil {
		return result, err
	}
	if _, err := recoverCompletedPostMergeVerification(lifecycle); err != nil {
		return result, err
	}
	if err := reconcileApprovedBaselineBranches(runCtx, options.StateDir, config, lifecycle, options.GitHub, policy); err != nil {
		return result, err
	}
	if err := reconcileOpenRepairDuplicates(runCtx, config, lifecycle, options.GitHub, policy); err != nil {
		return result, err
	}
	externalMerge, err := reconcileExternallyMergedRepair(runCtx, lifecycle, options.GitHub)
	if err != nil {
		return result, err
	}
	workflow, err := dispatchAndWait(runCtx, options.GitHub, config)
	if err != nil {
		return result, err
	}
	result.Workflow = workflow
	if externalMerge {
		finding, ok := mergedRepairFinding(lifecycle.Snapshot())
		if !ok || finding.MergeSHA != workflow.HeadSHA {
			return result, fmt.Errorf("post-merge verification ran at %s without a matching reconciled merge", workflow.HeadSHA)
		}
		if err := lifecycle.MarkPostMergeVerifying(finding.RepositoryFingerprint, fmt.Sprintf("%d", workflow.RunID), workflow.RunURL); err != nil {
			return result, err
		}
	}
	beadStore, err := beads.NewStore(filepath.Join(options.StateDir, "beads", "quality"))
	if err != nil {
		return result, err
	}
	validation, apply, outbox, verifiedArtifact, err := applyWorkflowEvidence(runCtx, options.StateDir, config, workflow, lifecycle, beadStore, options.GitHub, policy, visualhive.ApplyLifecycleOptions{})
	if err != nil {
		return result, err
	}
	result.Validation, result.Lifecycle, result.Outbox = validation, apply, outbox
	if externalMerge {
		workflowCopy, lifecycleCopy := workflow, apply
		result.PostMergeWorkflow, result.PostMergeLifecycle = &workflowCopy, &lifecycleCopy
		if _, err := recoverCompletedPostMergeVerification(lifecycle); err != nil {
			return result, err
		}
	}
	if config.Automation == AutomationRepairPR || config.Automation == AutomationAutoMerge {
		orchestration, orchestrationErr := orchestrateRepairs(runCtx, options.StateDir, config, lifecycle, beadStore, options.GitHub, policy, verifiedArtifact.SourceArtifactPath)
		result.Repairs, result.Gates = orchestration.Repairs, orchestration.Gates
		result.PostMergeWorkflow, result.PostMergeLifecycle = orchestration.PostMergeWorkflow, orchestration.PostMergeLifecycle
		result.Outbox.Succeeded += orchestration.Outbox.Succeeded
		result.Outbox.Failed += orchestration.Outbox.Failed
		result.Outbox.Processed += orchestration.Outbox.Processed
		result.Outbox.Denied += orchestration.Outbox.Denied
		result.Outbox.StaleSkipped += orchestration.Outbox.StaleSkipped
		result.Outbox.Errors = append(result.Outbox.Errors, orchestration.Outbox.Errors...)
		if orchestrationErr != nil {
			return result, orchestrationErr
		}
	}
	result.CompletedAt = time.Now().UTC()
	return result, nil
}

func reconcileApprovedBaselineBranches(ctx context.Context, stateDir string, config Config, lifecycle *visualhive.LifecycleStore, client *hivegithub.Client, policy automation.Policy) error {
	state, err := repair.NewStore(filepath.Join(stateDir, "repair"))
	if err != nil {
		return err
	}
	snapshot := state.Snapshot()
	keys := make([]string, 0, len(snapshot.Attempts))
	for key := range snapshot.Attempts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		attempt := snapshot.Attempts[key]
		if attempt == nil || attempt.BaselineReview == nil || attempt.BaselineReview.Status != repair.BaselineReviewApproved || attempt.BaselineReview.ProposalBranch == "" || attempt.BaselineReview.ProposalCommitSHA == "" {
			continue
		}
		finding, exists := lifecycle.Finding(key)
		if !exists {
			return fmt.Errorf("approved baseline branch lost its finding lifecycle")
		}
		decision := policy.Authorize(automation.ActionRequest{Action: automation.ActionDeleteBaselineBranch, Agent: repairActor(finding.OwningAgentHint), Repository: config.Repository, RepairAttempts: finding.RepairAttempts})
		lifecycle.RecordAuthorization(key, string(automation.ActionDeleteBaselineBranch), decision.Allowed, strings.Join(decision.Reasons, "; "))
		if !decision.Allowed {
			return fmt.Errorf("delete approved baseline branch %s denied: %s", attempt.BaselineReview.ProposalBranch, strings.Join(decision.Reasons, "; "))
		}
		deleted, err := client.DeleteBaselineBranchExact(ctx, config.Repository, attempt.BaselineReview.ProposalBranch, attempt.BaselineReview.ProposalCommitSHA)
		if err != nil {
			return err
		}
		if deleted {
			lifecycle.RecordAuthorization(key, "approved_baseline_branch_reconciled", true, fmt.Sprintf("deleted %s at exact reviewed head %s from proposal PR #%d", attempt.BaselineReview.ProposalBranch, attempt.BaselineReview.ProposalCommitSHA, attempt.BaselineReview.ProposalPRNumber))
		}
	}
	legacy, err := client.ListMergedBaselineBranches(ctx, config.Repository, config.DefaultBranch)
	if err != nil {
		return err
	}
	for _, proposal := range legacy {
		actor, attempts := "quality", 0
		if finding, exists := lifecycle.Finding(proposal.RepositoryFingerprint); exists {
			actor, attempts = repairActor(finding.OwningAgentHint), finding.RepairAttempts
		}
		decision := policy.Authorize(automation.ActionRequest{Action: automation.ActionDeleteBaselineBranch, Agent: actor, Repository: config.Repository, RepairAttempts: attempts})
		lifecycle.RecordAuthorization(proposal.RepositoryFingerprint, string(automation.ActionDeleteBaselineBranch), decision.Allowed, fmt.Sprintf("legacy proposal PR #%d: %s", proposal.PRNumber, strings.Join(decision.Reasons, "; ")))
		if !decision.Allowed {
			return fmt.Errorf("delete legacy baseline branch %s denied: %s", proposal.Branch, strings.Join(decision.Reasons, "; "))
		}
		deleted, err := client.DeleteBaselineBranchExact(ctx, config.Repository, proposal.Branch, proposal.HeadSHA)
		if err != nil {
			return err
		}
		if deleted {
			lifecycle.RecordAuthorization(proposal.RepositoryFingerprint, "legacy_baseline_branch_reconciled", true, fmt.Sprintf("deleted %s at exact merged proposal head %s from PR #%d", proposal.Branch, proposal.HeadSHA, proposal.PRNumber))
		}
	}
	return nil
}

func reconcileOpenRepairDuplicates(ctx context.Context, config Config, lifecycle *visualhive.LifecycleStore, client *hivegithub.Client, policy automation.Policy) error {
	state := lifecycle.Snapshot()
	keys := make([]string, 0, len(state.Findings))
	for key := range state.Findings {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		finding := state.Findings[key]
		if finding == nil || finding.PRNumber <= 0 || finding.RepairCommitSHA == "" || finding.Branch == "" || finding.MergeSHA != "" {
			continue
		}
		marker := fmt.Sprintf("<!-- hive-repair: %s -->", finding.RepositoryFingerprint)
		pulls, err := client.ListOpenRepairPullRequests(ctx, config.Repository, config.DefaultBranch, marker)
		if err != nil {
			return err
		}
		if len(pulls) <= 1 {
			continue
		}
		currentFound := false
		for _, pull := range pulls {
			if pull.Number == finding.PRNumber && pull.Branch == finding.Branch && pull.HeadSHA == finding.RepairCommitSHA {
				currentFound = true
				break
			}
		}
		if !currentFound {
			return fmt.Errorf("multiple open repair PRs exist for %s but none matches the exact durable current repair", finding.RepositoryFingerprint)
		}
		for _, pull := range pulls {
			if pull.Number == finding.PRNumber {
				continue
			}
			actor := repairActor(finding.OwningAgentHint)
			closeDecision := policy.Authorize(automation.ActionRequest{Action: automation.ActionCloseRepairPR, Agent: actor, Repository: config.Repository, RepairAttempts: finding.RepairAttempts})
			lifecycle.RecordAuthorization(finding.RepositoryFingerprint, string(automation.ActionCloseRepairPR), closeDecision.Allowed, fmt.Sprintf("superseded PR #%d: %s", pull.Number, strings.Join(closeDecision.Reasons, "; ")))
			if !closeDecision.Allowed {
				return fmt.Errorf("close superseded repair PR #%d denied: %s", pull.Number, strings.Join(closeDecision.Reasons, "; "))
			}
			if err := client.CloseRepairPullRequestExact(ctx, config.Repository, pull.Number, marker, pull.Branch, pull.HeadSHA); err != nil {
				return err
			}
			deleteDecision := policy.Authorize(automation.ActionRequest{Action: automation.ActionDeleteRepairBranch, Agent: actor, Repository: config.Repository, RepairAttempts: finding.RepairAttempts})
			lifecycle.RecordAuthorization(finding.RepositoryFingerprint, string(automation.ActionDeleteRepairBranch), deleteDecision.Allowed, fmt.Sprintf("superseded PR #%d branch %s: %s", pull.Number, pull.Branch, strings.Join(deleteDecision.Reasons, "; ")))
			if !deleteDecision.Allowed {
				return fmt.Errorf("delete superseded repair branch %s denied: %s", pull.Branch, strings.Join(deleteDecision.Reasons, "; "))
			}
			if err := client.DeleteRepairBranchExact(ctx, config.Repository, pull.Branch, pull.HeadSHA); err != nil {
				return err
			}
			lifecycle.RecordAuthorization(finding.RepositoryFingerprint, "duplicate_repair_reconciled", true, fmt.Sprintf("closed superseded PR #%d and deleted %s at %s; retained PR #%d", pull.Number, pull.Branch, pull.HeadSHA, finding.PRNumber))
		}
	}
	return nil
}

func repairAttemptLimit(config Config) int {
	if config.MaxRepairAttempts < 1 {
		return 3
	}
	return config.MaxRepairAttempts
}

type repairOrchestrationResult struct {
	Repairs            []repair.Result
	Gates              []GateEvaluation
	PostMergeWorkflow  *WorkflowRunEvidence
	PostMergeLifecycle *visualhive.ApplyLifecycleResult
	Outbox             visualhive.OutboxProcessorResult
}

func applyWorkflowEvidence(ctx context.Context, stateDir string, config Config, workflow WorkflowRunEvidence, lifecycle *visualhive.LifecycleStore, beadStore *beads.Store, client *hivegithub.Client, policy automation.Policy, applyOptions visualhive.ApplyLifecycleOptions) (visualhive.Validation, visualhive.ApplyLifecycleResult, visualhive.OutboxProcessorResult, hivegithub.VerifiedVisualHiveArtifact, error) {
	bundle, verified, err := client.FetchAndVerifyVisualHiveBundle(ctx, hivegithub.VisualHiveArtifactRequest{
		Repository: config.Repository, WorkflowRunID: workflow.RunID, ArtifactID: workflow.BundleArtifact,
		SourceArtifactID: workflow.EvidenceArtifact, DestinationDir: filepath.Join(stateDir, "visual-hive", "artifacts"),
		FetchSourceArtifact: config.Automation == AutomationRepairPR || config.Automation == AutomationAutoMerge,
		TargetRef:           config.DefaultBranch, MaxACMM: config.ACMMLevel,
	})
	if err != nil {
		return visualhive.Validation{}, visualhive.ApplyLifecycleResult{}, visualhive.OutboxProcessorResult{}, hivegithub.VerifiedVisualHiveArtifact{}, err
	}
	applyOptions.TargetRef = config.DefaultBranch
	applyOptions.VerificationRunID = fmt.Sprintf("%d", workflow.RunID)
	applyOptions.VerificationURL = workflow.RunURL
	applyOptions.VerificationCommitSHA = workflow.HeadSHA
	applyOptions.MaxActiveIssues = config.MaxActiveIssues
	applyOptions.PreferRepairable = config.Automation == AutomationRepairPR || config.Automation == AutomationAutoMerge
	apply, err := lifecycle.ApplyBundle(bundle, beadStore, applyOptions)
	if err != nil {
		return bundle.Validation, visualhive.ApplyLifecycleResult{}, visualhive.OutboxProcessorResult{}, verified, err
	}
	outbox := visualhive.ProcessOutbox(ctx, lifecycle, beadStore, policy, client)
	if outbox.Failed > 0 {
		return bundle.Validation, apply, outbox, verified, fmt.Errorf("GitHub lifecycle outbox failed: %s", strings.Join(outbox.Errors, "; "))
	}
	return bundle.Validation, apply, outbox, verified, nil
}

func orchestrateRepairs(ctx context.Context, stateDir string, config Config, lifecycle *visualhive.LifecycleStore, beadStore *beads.Store, client *hivegithub.Client, policy automation.Policy, evidenceRoot string) (repairOrchestrationResult, error) {
	result := repairOrchestrationResult{}
	resumed, baselineGate, pendingReview, err := reconcileBaselineReview(ctx, stateDir, config, lifecycle, client, policy)
	if baselineGate != nil {
		result.Gates = append(result.Gates, *baselineGate)
	}
	if resumed != nil {
		result.Repairs = append(result.Repairs, *resumed)
	}
	if err != nil || pendingReview {
		return result, err
	}
	maxAttempts := policy.MaxRepairAttempts
	if maxAttempts <= 0 {
		maxAttempts = 3
	}
	for cycle := 0; cycle <= maxAttempts; cycle++ {
		repairs, err := runEligibleRepairs(ctx, config, lifecycle, client, policy, evidenceRoot)
		result.Repairs = append(result.Repairs, repairs...)
		if err != nil {
			finding, ok := activeRepairFinding(lifecycle.Snapshot())
			if repair.IsRetryableAttemptError(err) && ok {
				spent, countErr := durableRepairAttempts(stateDir, finding)
				if countErr != nil {
					return result, countErr
				}
				if spent < maxAttempts {
					continue
				}
				return result, fmt.Errorf("repair attempt budget exhausted for %s after %d attempts and retryable failure: %w", finding.RepositoryFingerprint, spent, err)
			}
			return result, err
		}
		finding, ok := activeRepairFinding(lifecycle.Snapshot())
		if !ok {
			return result, nil
		}
		if finding.Status == visualhive.StatusMerged || finding.Status == visualhive.StatusPostMergeVerifying {
			postMerge, postApply, postOutbox, verifyErr := verifyMergedFinding(ctx, stateDir, config, finding, lifecycle, beadStore, client, policy)
			result.PostMergeWorkflow, result.PostMergeLifecycle, result.Outbox = &postMerge, &postApply, postOutbox
			return result, verifyErr
		}
		if finding.Status == visualhive.StatusIssueOpen || finding.Status == visualhive.StatusFixQueued || finding.Status == visualhive.StatusRepairRunning {
			if finding.RepairAttempts >= maxAttempts {
				return result, fmt.Errorf("repair attempt budget exhausted for %s", finding.RepositoryFingerprint)
			}
			continue
		}
		if finding.Status == visualhive.StatusNeedsRevision {
			pending, pendingErr := hasPendingBaselineCandidate(stateDir, finding.RepositoryFingerprint)
			if pendingErr != nil {
				return result, pendingErr
			}
			if !pending {
				if finding.RepairAttempts >= maxAttempts {
					return result, fmt.Errorf("repair attempt budget exhausted for %s", finding.RepositoryFingerprint)
				}
				continue
			}
		}
		gate, err := waitForPullRequestGate(ctx, client, config.Repository, finding)
		if err != nil {
			return result, err
		}
		green := gateChecksGreen(gate)
		checkEvidence := make([]visualhive.CheckEvidence, 0, len(gate.Checks))
		for _, check := range gate.Checks {
			checkEvidence = append(checkEvidence, visualhive.CheckEvidence{Name: check.Name, State: check.State, URL: check.URL})
		}
		summary := checkSummary(gate)
		evaluation := GateEvaluation{RepositoryFingerprint: finding.RepositoryFingerprint, Purpose: "repair", Gate: gate}
		if !green {
			attemptState, stateErr := repair.NewStore(filepath.Join(stateDir, "repair"))
			if stateErr != nil {
				return result, stateErr
			}
			attempt, exists := attemptState.Get(finding.RepositoryFingerprint)
			if exists && attempt.BaselineReview != nil && attempt.BaselineReview.Status == repair.BaselineReviewCandidateReady {
				decision := policy.Authorize(automation.ActionRequest{
					Action: automation.ActionCreateBaselineReview, Agent: repairActor(finding.OwningAgentHint), Repository: config.Repository,
					Risk: automation.RiskRestricted, ChangedFiles: baselineCandidatePaths(attempt.BaselineReview.Candidates), RepairAttempts: finding.RepairAttempts,
				})
				lifecycle.RecordAuthorization(finding.RepositoryFingerprint, string(automation.ActionCreateBaselineReview), decision.Allowed, strings.Join(decision.Reasons, "; "))
				if !decision.Allowed {
					return result, fmt.Errorf("baseline review proposal denied: %s", strings.Join(decision.Reasons, "; "))
				}
				verified, fetchErr := client.FetchAndVerifyPullRequestArtifact(ctx, hivegithub.PullRequestArtifactRequest{
					Repository: config.Repository, ExpectedHeadSHA: attempt.CommitSHA, ExpectedHeadBranch: attempt.Branch,
					ExpectedWorkflowPath: ".github/workflows/visual-hive-pr.yml", ArtifactName: "visual-hive-pr",
					DestinationDir: filepath.Join(stateDir, "repair", "baseline-artifacts", repairStateKey(finding.RepositoryFingerprint)),
				})
				if fetchErr != nil {
					return result, fetchErr
				}
				hosted, recognized, reviewErr := repair.ReadHostedBaselineReview(verified.ArtifactRoot)
				if reviewErr != nil {
					return result, reviewErr
				}
				if !recognized {
					return result, fmt.Errorf("failed exact-head PR run did not contain an exclusive missing-baseline review")
				}
				updated, proposalErr := repair.CreateBaselineProposal(ctx, baselineProposalConfig(config, stateDir), finding, repair.BaselineProposalSource{
					WorkflowRunID: verified.WorkflowRunID, ArtifactID: verified.ArtifactID, RunURL: verified.RunURL,
				}, hosted, attemptState, client)
				if proposalErr != nil {
					return result, proposalErr
				}
				reason := fmt.Sprintf("Visual baseline proposal %s requires full-resolution review of %d exact candidate image(s)", updated.BaselineReview.ProposalPRURL, len(updated.BaselineReview.Candidates))
				if err := lifecycle.MarkManualReviewRequired(finding.RepositoryFingerprint, "visual_baseline", reason); err != nil {
					return result, err
				}
				replaceRepairResult(&result.Repairs, repairResultFromAttempt(updated, true))
				result.Gates = append(result.Gates, evaluation)
				return result, nil
			}
			if finding.Status != visualhive.StatusReady {
				if err := lifecycle.MarkChecksWithEvidence(finding.RepositoryFingerprint, gate.HeadSHA, false, summary, checkEvidence); err != nil {
					return result, err
				}
			}
			result.Gates = append(result.Gates, evaluation)
			if finding.RepairAttempts >= maxAttempts {
				return result, fmt.Errorf("hosted checks remain red after %d attempts: %s", finding.RepairAttempts, summary)
			}
			continue
		}
		if finding.Status != visualhive.StatusReady {
			if err := lifecycle.MarkChecksWithEvidence(finding.RepositoryFingerprint, gate.HeadSHA, true, summary, checkEvidence); err != nil {
				return result, err
			}
		}
		if gate.Merged {
			if strings.TrimSpace(gate.MergeSHA) == "" {
				return result, fmt.Errorf("merged pull request #%d did not report a merge commit", finding.PRNumber)
			}
			if err := lifecycle.MarkMerged(finding.RepositoryFingerprint, gate.MergeSHA); err != nil {
				return result, err
			}
			cleanupMergedRepairBranch(ctx, lifecycle, client, config.Repository, finding, gate.HeadSHA)
			result.Gates = append(result.Gates, evaluation)
			finding.MergeSHA, finding.Status = gate.MergeSHA, visualhive.StatusMerged
			postMerge, postApply, postOutbox, verifyErr := verifyMergedFinding(ctx, stateDir, config, finding, lifecycle, beadStore, client, policy)
			result.PostMergeWorkflow, result.PostMergeLifecycle, result.Outbox = &postMerge, &postApply, postOutbox
			return result, verifyErr
		}
		if !gate.Open {
			return result, fmt.Errorf("repair pull request #%d was closed without merging", finding.PRNumber)
		}
		if config.Automation == AutomationRepairPR {
			result.Gates = append(result.Gates, evaluation)
			return result, nil
		}
		decision := policy.Authorize(automation.ActionRequest{
			Action: automation.ActionMergePR, Agent: repairActor(finding.OwningAgentHint), Repository: config.Repository,
			Risk: mergeRisk(gate.ChangedFiles), ChangedFiles: gate.ChangedFiles, RepairAttempts: finding.RepairAttempts,
			ExpectedHeadSHA: finding.RepairCommitSHA, TestedHeadSHA: gate.HeadSHA,
			MergeableKnown: gate.MergeableKnown, Mergeable: gate.Mergeable, VisualHiveVerdictGreen: gate.VisualHiveVerdictGreen,
			RequiredCheckStates: gate.RequiredCheckStates, BranchProtectionEnabled: gate.BranchProtectionEnabled,
			Hold: gate.Hold, HumanReviewRequired: gate.HumanReviewRequired, BaselineChanged: gate.BaselineChanged,
			WorkflowChanged: gate.WorkflowChanged, SecuritySensitive: gate.SecuritySensitive, DeploymentChanged: gate.DeploymentChanged,
		})
		lifecycle.RecordAuthorization(finding.RepositoryFingerprint, string(automation.ActionMergePR), decision.Allowed, strings.Join(decision.Reasons, "; "))
		evaluation.Decision = &decision
		if !decision.Allowed {
			if err := markMergePolicyHold(lifecycle, finding, decision); err != nil {
				return result, err
			}
			result.Gates = append(result.Gates, evaluation)
			return result, nil
		}
		mergeSHA, err := client.MergePullRequestExact(ctx, config.Repository, finding.PRNumber, gate.HeadSHA)
		if err != nil {
			return result, err
		}
		evaluation.MergeSHA = mergeSHA
		result.Gates = append(result.Gates, evaluation)
		if err := lifecycle.MarkMerged(finding.RepositoryFingerprint, mergeSHA); err != nil {
			return result, err
		}
		cleanupMergedRepairBranch(ctx, lifecycle, client, config.Repository, finding, gate.HeadSHA)
		finding.MergeSHA, finding.Status = mergeSHA, visualhive.StatusMerged
		postMerge, postApply, postOutbox, verifyErr := verifyMergedFinding(ctx, stateDir, config, finding, lifecycle, beadStore, client, policy)
		result.PostMergeWorkflow, result.PostMergeLifecycle, result.Outbox = &postMerge, &postApply, postOutbox
		return result, verifyErr
	}
	return result, fmt.Errorf("repair orchestration exceeded its bounded iteration budget")
}

func durableRepairAttempts(stateDir string, finding visualhive.FindingLifecycle) (int, error) {
	spent := finding.RepairAttempts
	state, err := repair.NewStore(filepath.Join(stateDir, "repair"))
	if err != nil {
		return 0, err
	}
	attempt, exists := state.Get(finding.RepositoryFingerprint)
	if exists && attempt.Recurrence == finding.Recurrences && attempt.Attempt > spent {
		spent = attempt.Attempt
	}
	return spent, nil
}

func markMergePolicyHold(lifecycle *visualhive.LifecycleStore, finding visualhive.FindingLifecycle, decision automation.Decision) error {
	reason := fmt.Sprintf("Hive cannot auto-merge pull request #%d under the configured policy: %s. Review and merge the exact tested head manually, or change repository authority explicitly.", finding.PRNumber, strings.Join(decision.Reasons, "; "))
	return lifecycle.MarkManualReviewRequired(finding.RepositoryFingerprint, "merge_policy", reason)
}

func cleanupMergedRepairBranch(ctx context.Context, lifecycle *visualhive.LifecycleStore, client *hivegithub.Client, repository string, finding visualhive.FindingLifecycle, expectedHeadSHA string) {
	err := client.DeleteRepairBranchExact(ctx, repository, finding.Branch, expectedHeadSHA)
	if err != nil {
		lifecycle.RecordAuthorization(finding.RepositoryFingerprint, "delete_repair_branch", false, err.Error())
		return
	}
	lifecycle.RecordAuthorization(finding.RepositoryFingerprint, "delete_repair_branch", true, fmt.Sprintf("deleted %s at exact merged head %s", finding.Branch, expectedHeadSHA))
}

func hasPendingBaselineCandidate(stateDir, repositoryFingerprint string) (bool, error) {
	state, err := repair.NewStore(filepath.Join(stateDir, "repair"))
	if err != nil {
		return false, err
	}
	attempt, exists := state.Get(repositoryFingerprint)
	return exists && attempt.BaselineReview != nil && attempt.BaselineReview.Status == repair.BaselineReviewCandidateReady, nil
}

func reconcileBaselineReview(ctx context.Context, stateDir string, config Config, lifecycle *visualhive.LifecycleStore, client *hivegithub.Client, policy automation.Policy) (*repair.Result, *GateEvaluation, bool, error) {
	state, err := repair.NewStore(filepath.Join(stateDir, "repair"))
	if err != nil {
		return nil, nil, false, err
	}
	snapshot := state.Snapshot()
	keys := make([]string, 0, len(snapshot.Attempts))
	for key := range snapshot.Attempts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		attempt := snapshot.Attempts[key]
		if attempt == nil || attempt.BaselineReview == nil || attempt.BaselineReview.Status != repair.BaselineReviewProposalOpen || attempt.BaselineReview.ProposalPRNumber <= 0 {
			continue
		}
		finding, exists := lifecycle.Finding(key)
		if !exists || finding.PRNumber != attempt.PRNumber {
			return nil, nil, false, fmt.Errorf("baseline proposal lost its originating repair lifecycle")
		}
		gate, err := client.InspectPullRequestGate(ctx, config.Repository, attempt.BaselineReview.ProposalPRNumber)
		if err != nil {
			return nil, nil, false, err
		}
		evaluation := &GateEvaluation{RepositoryFingerprint: key, Purpose: "baseline_review", Gate: gate}
		expected := baselineCandidatePaths(attempt.BaselineReview.Candidates)
		if gate.HeadSHA != attempt.BaselineReview.ProposalCommitSHA || !sameStringSet(gate.ChangedFiles, expected) || !gate.BaselineChanged || gate.WorkflowChanged || gate.SecuritySensitive || gate.DeploymentChanged {
			return nil, evaluation, false, fmt.Errorf("baseline proposal #%d was tampered or changed outside its exact candidate set", gate.Number)
		}
		reason := fmt.Sprintf("Visual baseline proposal %s requires full-resolution review of %d exact candidate image(s)", attempt.BaselineReview.ProposalPRURL, len(attempt.BaselineReview.Candidates))
		if gate.Open {
			if err := lifecycle.MarkManualReviewRequired(key, "visual_baseline", reason); err != nil {
				return nil, evaluation, false, err
			}
			return nil, evaluation, true, nil
		}
		if !gate.Merged {
			attempt.BaselineReview.Status = repair.BaselineReviewRejected
			attempt.BaselineReview.RejectionReason = "baseline proposal was closed without merging"
			if err := state.Put(*attempt); err != nil {
				return nil, evaluation, false, err
			}
			return nil, evaluation, false, fmt.Errorf("baseline proposal #%d was rejected", gate.Number)
		}
		if !approvedMergedBaselineProposal(gate) {
			return nil, evaluation, false, fmt.Errorf("merged baseline proposal #%d lacks ready-for-review human approval, green exact-head checks, or merge SHA", gate.Number)
		}
		decision := policy.Authorize(automation.ActionRequest{
			Action: automation.ActionApplyBaselineReview, Agent: repairActor(finding.OwningAgentHint), Repository: config.Repository,
			Risk: automation.RiskRestricted, ChangedFiles: expected, RepairAttempts: finding.RepairAttempts,
		})
		lifecycle.RecordAuthorization(key, string(automation.ActionApplyBaselineReview), decision.Allowed, strings.Join(decision.Reasons, "; "))
		if !decision.Allowed {
			return nil, evaluation, false, fmt.Errorf("apply reviewed baseline denied: %s", strings.Join(decision.Reasons, "; "))
		}
		updated, err := repair.ResumeAfterBaselineApproval(ctx, baselineProposalConfig(config, stateDir), finding, gate.MergeSHA, state, client)
		if err != nil {
			return nil, evaluation, false, err
		}
		if err := lifecycle.MarkPRHeadUpdated(key, updated.CommitSHA); err != nil {
			return nil, evaluation, false, err
		}
		if err := lifecycle.MarkManualReviewComplete(key, "visual_baseline"); err != nil {
			return nil, evaluation, false, err
		}
		result := repairResultFromAttempt(updated, true)
		return &result, evaluation, false, nil
	}
	return nil, nil, false, nil
}

// approvedMergedBaselineProposal treats the explicit merge of Hive's exact,
// non-draft baseline-only proposal as the human approval. UpsertReviewPullRequest
// deliberately applies a durable hold label, and GitHub retains that label after
// a reviewer marks the PR ready and merges it. The label must continue blocking
// an open proposal, but cannot make an otherwise exact reviewed merge impossible
// to consume. Exact head, candidate paths, and risk classification are verified
// immediately before this helper is called.
func approvedMergedBaselineProposal(gate hivegithub.PullRequestGate) bool {
	return gate.Merged && !gate.Draft && !gate.HumanReviewRequired && gateChecksGreen(gate) && strings.TrimSpace(gate.MergeSHA) != ""
}

func baselineProposalConfig(config Config, stateDir string) repair.BaselineProposalConfig {
	commands := make([]repair.Command, 0, len(config.TestCommands))
	for _, parts := range config.TestCommands {
		if len(parts) > 0 {
			commands = append(commands, repair.Command{Name: parts[0], Args: append([]string(nil), parts[1:]...)})
		}
	}
	return repair.BaselineProposalConfig{
		RepositoryDir: config.CheckoutDir, WorktreeRoot: filepath.Join(stateDir, "repair", "baseline-worktrees"), BaseBranch: config.DefaultBranch,
		ValidationCommands: commands, Environment: repairValidationEnvironment(config), CommandTimeout: 15 * time.Minute,
	}
}

func baselineCandidatePaths(candidates []repair.BaselineCandidate) []string {
	result := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		result = append(result, candidate.BaselinePath)
	}
	sort.Strings(result)
	return result
}

func sameStringSet(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	counts := map[string]int{}
	for _, value := range left {
		counts[filepath.ToSlash(strings.TrimPrefix(value, "./"))]++
	}
	for _, value := range right {
		key := filepath.ToSlash(strings.TrimPrefix(value, "./"))
		if counts[key] == 0 {
			return false
		}
		counts[key]--
	}
	return true
}

func repairResultFromAttempt(attempt repair.Attempt, resumed bool) repair.Result {
	review := attempt.BaselineReview
	return repair.Result{
		RepositoryFingerprint: attempt.RepositoryFingerprint, Branch: attempt.Branch, CommitSHA: attempt.CommitSHA,
		PRNumber: attempt.PRNumber, PRURL: attempt.PRURL, ChangedFiles: append([]string(nil), attempt.ChangedFiles...), BaselineReview: review, Resumed: resumed,
	}
}

func replaceRepairResult(results *[]repair.Result, replacement repair.Result) {
	for index := range *results {
		if (*results)[index].RepositoryFingerprint == replacement.RepositoryFingerprint {
			(*results)[index] = replacement
			return
		}
	}
	*results = append(*results, replacement)
}

func repairStateKey(value string) string {
	digest := sha256.Sum256([]byte(value))
	return fmt.Sprintf("%x", digest[:6])
}

func reconcileExternallyMergedRepair(ctx context.Context, lifecycle *visualhive.LifecycleStore, client *hivegithub.Client) (bool, error) {
	finding, ok := repairPullRequestFinding(lifecycle.Snapshot())
	if !ok {
		return false, nil
	}
	gate, err := client.InspectPullRequestGate(ctx, finding.Repository, finding.PRNumber)
	if err != nil {
		return false, err
	}
	if gate.HeadSHA != finding.RepairCommitSHA {
		return false, fmt.Errorf("pull request #%d head %s does not match Hive repair commit %s", finding.PRNumber, gate.HeadSHA, finding.RepairCommitSHA)
	}
	if !gate.Merged {
		if !gate.Open {
			return false, fmt.Errorf("repair pull request #%d was closed without merging", finding.PRNumber)
		}
		return false, nil
	}
	if strings.TrimSpace(gate.MergeSHA) == "" {
		return false, fmt.Errorf("merged pull request #%d did not report a merge commit", finding.PRNumber)
	}
	green := gateChecksGreen(gate)
	if !green {
		return false, fmt.Errorf("externally merged pull request #%d lacks green exact-head gates", finding.PRNumber)
	}
	if finding.Status == visualhive.StatusResolved || finding.Status == visualhive.StatusIssueClosed {
		if err := lifecycle.MarkRecoveredMerge(finding.RepositoryFingerprint, gate.MergeSHA); err != nil {
			return false, err
		}
		cleanupMergedRepairBranch(ctx, lifecycle, client, finding.Repository, finding, gate.HeadSHA)
		return false, nil
	}
	if finding.Status != visualhive.StatusReady {
		checkEvidence := make([]visualhive.CheckEvidence, 0, len(gate.Checks))
		for _, check := range gate.Checks {
			checkEvidence = append(checkEvidence, visualhive.CheckEvidence{Name: check.Name, State: check.State, URL: check.URL})
		}
		if err := lifecycle.MarkChecksWithEvidence(finding.RepositoryFingerprint, gate.HeadSHA, true, checkSummary(gate), checkEvidence); err != nil {
			return false, err
		}
	}
	if finding.ManualReviewKind == "merge_policy" {
		if err := lifecycle.MarkManualReviewComplete(finding.RepositoryFingerprint, "merge_policy"); err != nil {
			return false, err
		}
	}
	if err := lifecycle.MarkMerged(finding.RepositoryFingerprint, gate.MergeSHA); err != nil {
		return false, err
	}
	cleanupMergedRepairBranch(ctx, lifecycle, client, finding.Repository, finding, gate.HeadSHA)
	return true, nil
}

func repairPullRequestFinding(state visualhive.LifecycleState) (visualhive.FindingLifecycle, bool) {
	keys := make([]string, 0, len(state.Findings))
	for key := range state.Findings {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		finding := state.Findings[key]
		if finding == nil || (finding.HumanReviewRequired && finding.ManualReviewKind != "merge_policy") || finding.PRNumber <= 0 || finding.RepairCommitSHA == "" || finding.MergeSHA != "" {
			continue
		}
		switch finding.Status {
		case visualhive.StatusPROpen, visualhive.StatusChecksRunning, visualhive.StatusNeedsRevision, visualhive.StatusReady, visualhive.StatusResolved, visualhive.StatusIssueClosed:
			return *finding, true
		}
	}
	return visualhive.FindingLifecycle{}, false
}

func mergedRepairFinding(state visualhive.LifecycleState) (visualhive.FindingLifecycle, bool) {
	for _, finding := range state.Findings {
		if finding != nil && finding.Status == visualhive.StatusMerged && finding.MergeSHA != "" {
			return *finding, true
		}
	}
	return visualhive.FindingLifecycle{}, false
}

func recoverCompletedPostMergeVerification(lifecycle *visualhive.LifecycleStore) (bool, error) {
	state := lifecycle.Snapshot()
	keys := make([]string, 0, len(state.Findings))
	for key := range state.Findings {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		finding := state.Findings[key]
		if finding == nil || finding.Status != visualhive.StatusPostMergeVerifying || finding.ValidationRunID == "" || finding.LastWorkflowRunID != finding.ValidationRunID {
			continue
		}
		summary := fmt.Sprintf("finding remained present after authoritative target-branch verification %s", finding.ValidationRunURL)
		if err := lifecycle.MarkPostMergeFailed(finding.RepositoryFingerprint, summary); err != nil {
			return false, err
		}
		return true, nil
	}
	return false, nil
}

func verifyMergedFinding(ctx context.Context, stateDir string, config Config, finding visualhive.FindingLifecycle, lifecycle *visualhive.LifecycleStore, beadStore *beads.Store, client *hivegithub.Client, policy automation.Policy) (WorkflowRunEvidence, visualhive.ApplyLifecycleResult, visualhive.OutboxProcessorResult, error) {
	postMerge, err := dispatchAndWait(ctx, client, config)
	if err != nil {
		return postMerge, visualhive.ApplyLifecycleResult{}, visualhive.OutboxProcessorResult{}, err
	}
	applyOptions, err := verifyPostMergeTarget(ctx, stateDir, config, finding, postMerge.HeadSHA, client)
	if err != nil {
		lifecycle.RecordAuthorization(finding.RepositoryFingerprint, "post_merge_descendant", false, err.Error())
		return postMerge, visualhive.ApplyLifecycleResult{}, visualhive.OutboxProcessorResult{}, err
	}
	if postMerge.HeadSHA != finding.MergeSHA {
		lifecycle.RecordAuthorization(finding.RepositoryFingerprint, "post_merge_descendant", true, fmt.Sprintf("repair merge %s is an unchanged-file ancestor of target head %s", finding.MergeSHA, postMerge.HeadSHA))
	}
	if err := lifecycle.MarkPostMergeVerifying(finding.RepositoryFingerprint, fmt.Sprintf("%d", postMerge.RunID), postMerge.RunURL); err != nil {
		return postMerge, visualhive.ApplyLifecycleResult{}, visualhive.OutboxProcessorResult{}, err
	}
	_, postApply, postOutbox, _, err := applyWorkflowEvidence(ctx, stateDir, config, postMerge, lifecycle, beadStore, client, policy, applyOptions)
	if err != nil {
		return postMerge, postApply, postOutbox, err
	}
	verified, exists := lifecycle.Finding(finding.RepositoryFingerprint)
	if !exists || verified.Status != visualhive.StatusIssueClosed {
		if _, recoveryErr := recoverCompletedPostMergeVerification(lifecycle); recoveryErr != nil {
			return postMerge, postApply, postOutbox, recoveryErr
		}
		return postMerge, postApply, postOutbox, fmt.Errorf("post-merge verification did not resolve and close finding %s", finding.RepositoryFingerprint)
	}
	return postMerge, postApply, postOutbox, nil
}

func verifyPostMergeTarget(ctx context.Context, stateDir string, config Config, finding visualhive.FindingLifecycle, headSHA string, client *hivegithub.Client) (visualhive.ApplyLifecycleOptions, error) {
	options := visualhive.ApplyLifecycleOptions{}
	if headSHA == finding.MergeSHA {
		return options, nil
	}
	owner, repo, ok := strings.Cut(config.Repository, "/")
	if !ok || owner == "" || repo == "" {
		return options, fmt.Errorf("invalid configured repository")
	}
	state, err := repair.NewStore(filepath.Join(stateDir, "repair"))
	if err != nil {
		return options, err
	}
	attempt, exists := state.Get(finding.RepositoryFingerprint)
	if !exists || attempt.CommitSHA != finding.RepairCommitSHA || attempt.PRNumber != finding.PRNumber || len(attempt.ChangedFiles) == 0 {
		return options, fmt.Errorf("cannot verify descendant target for %s without the exact durable repair attempt", finding.RepositoryFingerprint)
	}
	comparison, _, err := client.GoGitHub().Repositories.CompareCommits(ctx, owner, repo, finding.MergeSHA, headSHA, &gh.ListOptions{PerPage: 100})
	if err != nil {
		return options, fmt.Errorf("compare repair merge to target head: %w", err)
	}
	if comparison.GetStatus() != "ahead" || comparison.GetMergeBaseCommit().GetSHA() != finding.MergeSHA || comparison.GetTotalCommits() < 1 || comparison.GetTotalCommits() > 250 {
		return options, fmt.Errorf("target head %s is not a bounded descendant of repair merge %s", headSHA, finding.MergeSHA)
	}
	if len(comparison.Files) == 0 || len(comparison.Files) >= 300 {
		return options, fmt.Errorf("target descendant comparison did not provide a complete bounded file inventory")
	}
	repairFiles := make(map[string]bool, len(attempt.ChangedFiles))
	for _, file := range attempt.ChangedFiles {
		repairFiles[filepath.ToSlash(strings.TrimSpace(file))] = true
	}
	for _, file := range comparison.Files {
		if repairFiles[filepath.ToSlash(file.GetFilename())] || repairFiles[filepath.ToSlash(file.GetPreviousFilename())] {
			return options, fmt.Errorf("target descendant changed repair file %s after merge", file.GetFilename())
		}
	}
	options.VerifiedMergeAncestorFingerprint = finding.RepositoryFingerprint
	options.VerifiedMergeAncestorSHA = finding.MergeSHA
	return options, nil
}

func dispatchAndWait(ctx context.Context, client *hivegithub.Client, config Config) (WorkflowRunEvidence, error) {
	owner, repo, ok := strings.Cut(config.Repository, "/")
	if !ok {
		return WorkflowRunEvidence{}, fmt.Errorf("invalid configured repository")
	}
	const workflowFile = "hive-visual-hive.yml"
	const dispatchAttemptLimit = 3
	for attempt := 1; attempt <= dispatchAttemptLimit; attempt++ {
		selected, err := dispatchAndWaitAttempt(ctx, client, owner, repo, workflowFile, config.DefaultBranch)
		if err != nil {
			return WorkflowRunEvidence{}, err
		}
		if retryCancelledDispatch(selected.GetConclusion(), attempt, dispatchAttemptLimit) {
			continue
		}
		if selected.GetConclusion() != "success" {
			return WorkflowRunEvidence{}, fmt.Errorf("Visual Hive workflow %s concluded %s", selected.GetHTMLURL(), selected.GetConclusion())
		}
		artifacts, _, err := client.GoGitHub().Actions.ListWorkflowRunArtifacts(ctx, owner, repo, selected.GetID(), &gh.ListOptions{PerPage: 100})
		if err != nil {
			return WorkflowRunEvidence{}, fmt.Errorf("list production evidence artifacts: %w", err)
		}
		workflow := WorkflowRunEvidence{RunID: selected.GetID(), RunURL: selected.GetHTMLURL(), HeadSHA: selected.GetHeadSHA(), Conclusion: selected.GetConclusion()}
		for _, artifact := range artifacts.Artifacts {
			switch {
			case strings.HasPrefix(artifact.GetName(), "visual-hive-evidence-"):
				workflow.EvidenceArtifact = artifact.GetID()
			case strings.HasPrefix(artifact.GetName(), "visual-hive-bundle-"):
				workflow.BundleArtifact = artifact.GetID()
			}
		}
		if workflow.EvidenceArtifact <= 0 || workflow.BundleArtifact <= 0 {
			return WorkflowRunEvidence{}, fmt.Errorf("workflow did not publish both evidence and provenance-bound bundle artifacts")
		}
		return workflow, nil
	}
	return WorkflowRunEvidence{}, fmt.Errorf("Visual Hive workflow was cancelled by concurrency %d consecutive times", dispatchAttemptLimit)
}

func dispatchAndWaitAttempt(ctx context.Context, client *hivegithub.Client, owner, repo, workflowFile, ref string) (*gh.WorkflowRun, error) {
	started := time.Now().UTC().Add(-5 * time.Second)
	_, err := client.GoGitHub().Actions.CreateWorkflowDispatchEventByFileName(ctx, owner, repo, workflowFile, gh.CreateWorkflowDispatchEventRequest{Ref: ref})
	if err != nil {
		return nil, fmt.Errorf("dispatch Visual Hive production workflow: %w", err)
	}
	var selected *gh.WorkflowRun
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for selected == nil {
		runs, _, listErr := client.GoGitHub().Actions.ListWorkflowRunsByFileName(ctx, owner, repo, workflowFile, &gh.ListWorkflowRunsOptions{
			Branch: ref, Event: "workflow_dispatch", ExcludePullRequests: true, ListOptions: gh.ListOptions{PerPage: 20},
		})
		if listErr == nil {
			for _, candidate := range runs.WorkflowRuns {
				if candidate.GetCreatedAt().Time.Before(started) {
					continue
				}
				if selected == nil || candidate.GetCreatedAt().Time.After(selected.GetCreatedAt().Time) {
					selected = candidate
				}
			}
		}
		if selected != nil {
			break
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("wait for dispatched workflow: %w", ctx.Err())
		case <-ticker.C:
		}
	}
	for selected.GetStatus() != "completed" {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("wait for workflow completion: %w", ctx.Err())
		case <-ticker.C:
		}
		current, _, getErr := client.GoGitHub().Actions.GetWorkflowRunByID(ctx, owner, repo, selected.GetID())
		if getErr != nil {
			continue
		}
		selected = current
	}
	return selected, nil
}

func retryCancelledDispatch(conclusion string, attempt, limit int) bool {
	return strings.EqualFold(strings.TrimSpace(conclusion), "cancelled") && attempt < limit
}

func runEligibleRepairs(ctx context.Context, config Config, lifecycle *visualhive.LifecycleStore, client *hivegithub.Client, policy automation.Policy, evidenceRoot string) ([]repair.Result, error) {
	snapshot := lifecycle.Snapshot()
	key := selectedRepairKey(snapshot)
	if key == "" {
		return nil, nil
	}
	for _, key := range []string{key} {
		finding := snapshot.Findings[key]
		if finding == nil || finding.HumanReviewRequired || (finding.Status != visualhive.StatusIssueOpen && finding.Status != visualhive.StatusFixQueued && finding.Status != visualhive.StatusNeedsRevision && finding.Status != visualhive.StatusRepairRunning) || finding.IssueNumber <= 0 {
			continue
		}
		state, err := repair.NewStore(filepath.Join(config.StateDir, "repair"))
		if err != nil {
			return nil, err
		}
		commands := make([]repair.Command, 0, len(config.TestCommands))
		for _, parts := range config.TestCommands {
			if len(parts) > 0 {
				commands = append(commands, repair.Command{Name: parts[0], Args: append([]string(nil), parts[1:]...)})
			}
		}
		if len(commands) == 0 {
			return nil, fmt.Errorf("validated repository test plan has no executable commands")
		}
		evidenceSummary, err := repair.LoadEvidenceSummary(evidenceRoot, *finding)
		if err != nil {
			if errors.Is(err, repair.ErrNoActionableEvidence) {
				reason := "Verified evidence does not contain a safe, repository-scoped repair contribution; keep the issue for operator review without dispatching a model or PR."
				if reviewErr := lifecycle.MarkManualReviewRequired(finding.RepositoryFingerprint, "repair_scope", reason); reviewErr != nil {
					return nil, reviewErr
				}
				return nil, nil
			}
			return nil, err
		}
		worker := repair.Worker{
			Config: repair.Config{
				RepositoryDir: config.CheckoutDir, WorktreeRoot: filepath.Join(config.StateDir, "repair", "worktrees"), BaseBranch: config.DefaultBranch,
				Policy: policy, AllowedRepairPaths: config.AllowedRepairPaths, PreparationCommands: repairPreparationCommands(config.CheckoutDir), ValidationCommands: commands,
				Environment: repairValidationEnvironment(config), EvidenceSummary: evidenceSummary,
				ModelTimeout: 20 * time.Minute, CommandTimeout: 15 * time.Minute,
			},
			Provider: repair.CodexProvider{Command: config.ProviderCommand, Prefix: config.ProviderArgs}, State: state, Lifecycle: lifecycle, GitHub: client,
		}
		result, err := worker.Run(ctx, *finding)
		if err != nil {
			return nil, err
		}
		return []repair.Result{result}, nil // repository concurrency budget defaults to one repair
	}
	return nil, nil
}

// selectedRepairKey enforces the repository-wide concurrency budget before a
// worker is constructed. Any existing branch/PR/merge lifecycle blocks a new
// issue from starting; only the already-active repair may resume or revise.
func selectedRepairKey(state visualhive.LifecycleState) string {
	keys := make([]string, 0, len(state.Findings))
	for key := range state.Findings {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	resumeKey := ""
	for _, key := range keys {
		finding := state.Findings[key]
		if finding == nil {
			continue
		}
		switch finding.Status {
		case visualhive.StatusRepairRunning, visualhive.StatusNeedsRevision:
			if finding.HumanReviewRequired {
				return ""
			}
			if resumeKey == "" {
				resumeKey = key
			}
		case visualhive.StatusPROpen, visualhive.StatusChecksRunning, visualhive.StatusReady,
			visualhive.StatusMerged, visualhive.StatusPostMergeVerifying:
			return ""
		}
	}
	if resumeKey != "" {
		return resumeKey
	}
	for _, key := range keys {
		finding := state.Findings[key]
		if finding != nil && !finding.HumanReviewRequired && finding.IssueNumber > 0 &&
			(finding.Status == visualhive.StatusIssueOpen || finding.Status == visualhive.StatusFixQueued) {
			return key
		}
	}
	return ""
}

func repairPreparationCommands(checkout string) []repair.Command {
	type lockfile struct {
		path string
		name string
	}
	locks := []lockfile{}
	_ = filepath.WalkDir(checkout, func(filePath string, entry os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", ".visual-hive", "node_modules", "dist", "build", "coverage", "vendor":
				if filePath != checkout {
					return filepath.SkipDir
				}
			}
			return nil
		}
		switch entry.Name() {
		case "package-lock.json", "pnpm-lock.yaml", "yarn.lock", "bun.lock", "bun.lockb":
			if len(locks) < 100 {
				locks = append(locks, lockfile{path: filePath, name: entry.Name()})
			}
		}
		return nil
	})
	sort.Slice(locks, func(i, j int) bool { return locks[i].path < locks[j].path })
	commands := make([]repair.Command, 0, len(locks)+1)
	for _, lock := range locks {
		dir, _ := filepath.Rel(checkout, filepath.Dir(lock.path))
		dir = filepath.ToSlash(dir)
		if dir == "" {
			dir = "."
		}
		switch lock.name {
		case "package-lock.json":
			args := []string{"ci"}
			if dir != "." {
				args = []string{"--prefix", dir, "ci"}
			}
			commands = append(commands, repair.Command{Name: "npm", Args: args})
		case "pnpm-lock.yaml":
			args := []string{"install", "--frozen-lockfile"}
			if dir != "." {
				args = []string{"--dir", dir, "install", "--frozen-lockfile"}
			}
			commands = append(commands, repair.Command{Name: "pnpm", Args: args})
		case "yarn.lock":
			args := []string{"install", "--immutable"}
			if dir != "." {
				args = []string{"--cwd", dir, "install", "--immutable"}
			}
			commands = append(commands, repair.Command{Name: "yarn", Args: args})
		case "bun.lock", "bun.lockb":
			args := []string{"install", "--frozen-lockfile"}
			if dir != "." {
				args = []string{"--cwd", dir, "install", "--frozen-lockfile"}
			}
			commands = append(commands, repair.Command{Name: "bun", Args: args})
		}
	}
	if _, err := os.Stat(filepath.Join(checkout, "pyproject.toml")); err == nil {
		commands = append(commands, repair.Command{Name: "python", Args: []string{"-m", "pip", "install", "."}})
	} else if _, err := os.Stat(filepath.Join(checkout, "requirements.txt")); err == nil {
		commands = append(commands, repair.Command{Name: "python", Args: []string{"-m", "pip", "install", "-r", "requirements.txt"}})
	}
	return commands
}

func repairValidationEnvironment(config Config) map[string]string {
	for _, argument := range config.VisualHiveArgs {
		trimmed := strings.TrimSpace(argument)
		if filepath.IsAbs(trimmed) && (strings.HasSuffix(strings.ToLower(trimmed), ".js") || strings.HasSuffix(strings.ToLower(trimmed), ".mjs") || strings.HasSuffix(strings.ToLower(trimmed), ".cjs")) {
			return map[string]string{"VISUAL_HIVE_CLI": trimmed}
		}
	}
	return nil
}

func activeRepairFinding(state visualhive.LifecycleState) (visualhive.FindingLifecycle, bool) {
	keys := make([]string, 0, len(state.Findings))
	for key := range state.Findings {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		finding := state.Findings[key]
		if finding == nil || finding.IssueNumber <= 0 || finding.HumanReviewRequired {
			continue
		}
		switch finding.Status {
		case visualhive.StatusIssueOpen, visualhive.StatusFixQueued, visualhive.StatusRepairRunning,
			visualhive.StatusPROpen, visualhive.StatusChecksRunning, visualhive.StatusNeedsRevision,
			visualhive.StatusReady, visualhive.StatusMerged, visualhive.StatusPostMergeVerifying:
			return *finding, true
		}
	}
	return visualhive.FindingLifecycle{}, false
}

func waitForPullRequestGate(ctx context.Context, client *hivegithub.Client, repository string, finding visualhive.FindingLifecycle) (hivegithub.PullRequestGate, error) {
	if finding.PRNumber <= 0 || finding.RepairCommitSHA == "" {
		return hivegithub.PullRequestGate{}, fmt.Errorf("finding %s has no persisted repair PR and exact head SHA", finding.RepositoryFingerprint)
	}
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		gate, err := client.InspectPullRequestGate(ctx, repository, finding.PRNumber)
		if err != nil {
			return gate, err
		}
		if gate.HeadSHA != finding.RepairCommitSHA {
			return gate, fmt.Errorf("pull request #%d head %s does not match Hive repair commit %s", finding.PRNumber, gate.HeadSHA, finding.RepairCommitSHA)
		}
		pending := !hasVisualHiveCheck(gate)
		for _, state := range gate.RequiredCheckStates {
			pending = pending || state == "pending" || state == "queued" || state == "in_progress"
		}
		if !pending {
			return gate, nil
		}
		select {
		case <-ctx.Done():
			return gate, fmt.Errorf("wait for exact-head pull request gates: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func hasVisualHiveCheck(gate hivegithub.PullRequestGate) bool {
	for _, check := range gate.Checks {
		name := strings.NewReplacer("-", " ", "_", " ").Replace(strings.ToLower(check.Name))
		if strings.Contains(name, "visual hive") && check.State != "pending" && check.State != "queued" && check.State != "in_progress" {
			return true
		}
	}
	return false
}

func gateChecksGreen(gate hivegithub.PullRequestGate) bool {
	if !gate.VisualHiveVerdictGreen {
		return false
	}
	for _, state := range gate.RequiredCheckStates {
		if strings.ToLower(strings.TrimSpace(state)) != "success" {
			return false
		}
	}
	return true
}

func checkSummary(gate hivegithub.PullRequestGate) string {
	parts := make([]string, 0, len(gate.Checks))
	for _, check := range gate.Checks {
		parts = append(parts, fmt.Sprintf("%s=%s", check.Name, check.State))
	}
	if len(parts) == 0 {
		return "no hosted checks observed"
	}
	return strings.Join(parts, ", ")
}

func mergeRisk(files []string) automation.RiskTier {
	for _, file := range files {
		normalized := "/" + strings.Trim(strings.ToLower(strings.ReplaceAll(file, "\\", "/")), "/") + "/"
		if strings.Contains(normalized, "/src/") {
			return automation.RiskLow
		}
	}
	return automation.RiskAutomatic
}

func repairActor(hint string) string {
	lower := strings.ToLower(hint)
	if strings.Contains(lower, "security") || strings.Contains(lower, "sec-check") {
		return "sec-check"
	}
	if strings.Contains(lower, "ci") {
		return "ci-maintainer"
	}
	return "quality"
}

func automationMode(value Automation) automation.Mode {
	switch value {
	case AutomationIssues:
		return automation.ModeIssues
	case AutomationRepairPR:
		return automation.ModeRepairPR
	case AutomationAutoMerge:
		return automation.ModeAutoMerge
	default:
		return automation.ModeAdvisory
	}
}
