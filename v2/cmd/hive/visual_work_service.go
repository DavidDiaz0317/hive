package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/kubestellar/hive/v2/pkg/agent"
	"github.com/kubestellar/hive/v2/pkg/automation"
	hivegithub "github.com/kubestellar/hive/v2/pkg/github"
	"github.com/kubestellar/hive/v2/pkg/integrated"
	"github.com/kubestellar/hive/v2/pkg/repair"
	"github.com/kubestellar/hive/v2/pkg/scheduler"
	"github.com/kubestellar/hive/v2/pkg/visualhive"
	visualcontroller "github.com/kubestellar/hive/v2/pkg/visualhive/controller"
	"github.com/kubestellar/hive/v2/pkg/visualhive/normalservice"
)

var normalVisualWorkRunner *normalservice.Service

type normalVisualArtifactSource struct {
	stateDir string
	timeout  time.Duration
	github   *hivegithub.Client
}

func (source *normalVisualArtifactSource) Fetch(ctx context.Context) (integrated.NormalVisualWork, error) {
	return integrated.FetchNormalVisualWork(ctx, source.stateDir, source.timeout, source.github)
}

func (source *normalVisualArtifactSource) Consume(workflow integrated.WorkflowRunEvidence, allowAlreadyConsumed bool) error {
	return integrated.ConsumeNormalVisualWork(source.stateDir, workflow, allowAlreadyConsumed)
}

type normalVisualRepairer struct {
	scheduler       *scheduler.Scheduler
	manager         *agent.Manager
	controller      *visualcontroller.Controller
	lifecycle       *visualhive.LifecycleStore
	github          *hivegithub.Client
	providerCommand string
	providerArgs    []string
	loadConfig      func() (integrated.Config, int, error)
	mu              sync.Mutex
}

func (runner *normalVisualRepairer) Run(ctx context.Context, supplied visualcontroller.DispatchEnvelope) (normalservice.RepairOutcome, error) {
	if runner == nil || runner.scheduler == nil || runner.manager == nil || runner.controller == nil || runner.lifecycle == nil || runner.github == nil || runner.loadConfig == nil {
		return normalservice.RepairOutcome{}, errors.New("normal Visual Hive Worker bridge is not configured")
	}
	// Worker is repository-sequential. The service already serializes cycles;
	// this lock also protects direct test/operator invocation of the bridge.
	runner.mu.Lock()
	defer runner.mu.Unlock()
	current, normalACMM, err := runner.loadConfig()
	if err != nil {
		return normalservice.RepairOutcome{}, err
	}
	if err := runner.validateRuntime(current, supplied); err != nil {
		return normalservice.RepairOutcome{}, err
	}
	envelope, err := runner.controller.RevalidateSpecialistBoundary(supplied.SourceExternalRef, "", "")
	if err != nil {
		return normalservice.RepairOutcome{}, err
	}
	if !reflect.DeepEqual(envelope, supplied) {
		return normalservice.RepairOutcome{}, errors.New("service dispatch differs from the native intake-owned envelope")
	}
	commands, err := exactWorkerCommands(envelope.ValidationCommands, current.TestCommands)
	if err != nil {
		return normalservice.RepairOutcome{}, err
	}
	evidenceSummary, err := repair.LoadEvidenceSummary(envelope.EvidenceRoot, envelope.Finding)
	if err != nil {
		return normalservice.RepairOutcome{}, err
	}
	state, err := repair.NewStore(filepath.Join(current.StateDir, "repair"))
	if err != nil {
		return normalservice.RepairOutcome{}, err
	}
	role := agent.SpecialistRole(envelope.Work.Role)
	mailbox, err := agent.NewSpecialistMailbox(filepath.Join(current.StateDir, "repair", "mailbox", string(role)), agent.SpecialistMailboxOptions{})
	if err != nil {
		return normalservice.RepairOutcome{}, err
	}
	reservedID, reservedDigest := envelope.SpecialistWorkOrderID, envelope.SpecialistRequestSHA256
	builder := func(buildCtx context.Context, worktree, baseSHA, baseTreeSHA, _ string, readiness agent.SpecialistSessionIdentity) (agent.SpecialistWorkOrderRequest, error) {
		profile := scheduler.GovernedProposalExecutorProfile{
			Backend: readiness.ExecutorBackend, ProviderSHA256: readiness.ProviderSHA256, Model: readiness.ExecutorModel,
			ConfigurationSHA256: readiness.ExecutorConfigSHA256, ContainmentProfile: readiness.ContainmentProfile,
			BackendParityClaimed: readiness.BackendParityClaimed,
		}
		if err := runner.scheduler.SetGovernedProposalExecutorProfile(profile); err != nil {
			return agent.SpecialistWorkOrderRequest{}, err
		}
		admitted, receipt, err := visualcontroller.BuildSchedulerAdmittedWork(envelope)
		if err != nil {
			return agent.SpecialistWorkOrderRequest{}, err
		}
		composer, err := repair.NewGovernedSourceContextComposer(repair.GovernedSourceContextOptions{
			Context: buildCtx, Worktree: worktree, Repository: current.Repository, BaseSHA: baseSHA, BaseTreeSHA: baseTreeSHA,
			EvidenceRoot: envelope.EvidenceRoot, AllowedPaths: envelope.AllowedRepairPaths,
			AffectedContracts: envelope.Work.AffectedContracts,
			RelevanceHints:    append([]string{envelope.Finding.Title, envelope.Finding.Body}, envelope.Work.AffectedContracts...),
			ManifestSHA256:    envelope.Evidence.ArtifactSHA256, VerificationReceiptSHA256: envelope.Evidence.VerificationReceiptSHA256,
		})
		if err != nil {
			return agent.SpecialistWorkOrderRequest{}, err
		}
		defer composer.Close()
		return runner.scheduler.BuildGovernedProposalMessage(role, admitted, receipt, composer)
	}
	reserve := func(order agent.SpecialistWorkOrder) error {
		persisted, err := runner.controller.ReserveSpecialistWorkOrderIdentity(envelope.SourceExternalRef, order.ID, order.RequestSHA256)
		if err != nil {
			return err
		}
		if persisted.SpecialistWorkOrderID != order.ID || persisted.SpecialistRequestSHA256 != order.RequestSHA256 {
			return errors.New("native intake did not persist the exact Scheduler work-order identity")
		}
		reservedID, reservedDigest = order.ID, order.RequestSHA256
		return nil
	}
	guard := func(order agent.SpecialistWorkOrder) error {
		_, err := runner.controller.RevalidateSpecialistBoundary(envelope.SourceExternalRef, order.ID, order.RequestSHA256)
		return err
	}
	provider, err := repair.NewSpecialistProvider(repair.SpecialistProviderConfig{
		Dispatcher: runner.manager, Mailbox: mailbox, Repository: current.Repository,
		RepositoryFingerprint: envelope.Work.RepositoryFingerprint,
		RecurrenceKey:         fmt.Sprintf("%s:r%d", envelope.Work.RepositoryFingerprint, envelope.Recurrence), Attempt: envelope.Attempt,
		ExpectedBaseSHA: envelope.BaseSHA, ExpectedBaseTreeSHA: envelope.BaseTreeSHA, Evidence: envelope.Evidence,
		WorkOrderKind: agent.SpecialistWorkOrderKindGovernedVisualHiveProposal, GovernedBuilder: builder,
		ReserveWorkOrder: reserve, LaunchGuard: guard, Specialist: role, RouteReason: envelope.Work.RoutingReason,
		AllowedPaths: envelope.AllowedRepairPaths, Validation: envelope.ValidationCommands, Deadline: envelope.CompositionDeadline,
	})
	if err != nil {
		return normalservice.RepairOutcome{}, err
	}
	policyLoader := func() (automation.Policy, error) {
		latest, latestACMM, err := runner.loadConfig()
		if err != nil {
			return automation.Policy{}, err
		}
		if err := runner.validateRuntime(latest, envelope); err != nil {
			return automation.Policy{}, err
		}
		policy := integrated.PolicyForConfig(latest)
		policy.Mode = automation.ModeRepairPR
		if latestACMM < policy.ACMMLevel {
			policy.ACMMLevel = latestACMM
		}
		return policy, nil
	}
	runtimeGuard := func(_ visualhive.FindingLifecycle, _ automation.Action, _ []string, _ int) error {
		_, err := runner.controller.RevalidateSpecialistBoundary(envelope.SourceExternalRef, reservedID, reservedDigest)
		return err
	}
	policy, err := policyLoader()
	if err != nil {
		return normalservice.RepairOutcome{}, err
	}
	if normalACMM < policy.ACMMLevel {
		policy.ACMMLevel = normalACMM
	}
	worker := repair.Worker{
		Config: repair.Config{
			RepositoryDir: current.CheckoutDir, WorktreeRoot: filepath.Join(current.StateDir, "repair", "worktrees"), BaseBranch: current.DefaultBranch,
			Agent: string(role), Policy: policy, PolicyLoader: policyLoader, RuntimeGuard: runtimeGuard,
			AllowedRepairPaths: envelope.AllowedRepairPaths, ValidationCommands: commands, EvidenceSummary: evidenceSummary,
			ModelTimeout: boundedVisualWorkDuration(envelope.CompositionDeadline, 20*time.Minute), CommandTimeout: 15 * time.Minute,
		},
		Provider: provider, State: state, Lifecycle: runner.lifecycle, GitHub: runner.github,
	}
	result, err := worker.Run(ctx, envelope.Finding)
	if err != nil {
		return normalservice.RepairOutcome{}, err
	}
	attempt, exists := state.Get(envelope.Work.RepositoryFingerprint)
	if !exists || attempt.ModelInvocationID == "" || attempt.PRNumber != result.PRNumber || attempt.CommitSHA != result.CommitSHA {
		return normalservice.RepairOutcome{}, errors.New("Worker PR result has no exact durable specialist checkpoint")
	}
	order, err := mailbox.LoadWorkOrder(attempt.ModelInvocationID)
	if err != nil {
		return normalservice.RepairOutcome{}, err
	}
	return normalservice.RepairOutcome{WorkOrderID: order.ID, RequestSHA256: order.RequestSHA256, Result: result}, nil
}

func (runner *normalVisualRepairer) validateRuntime(current integrated.Config, envelope visualcontroller.DispatchEnvelope) error {
	if !strings.EqualFold(current.Repository, envelope.Work.Packet.Repository) || current.RepositoryID != envelope.Work.Packet.RepositoryID ||
		current.StateDir == "" || current.CheckoutDir == "" || (current.Automation != integrated.AutomationRepairPR && current.Automation != integrated.AutomationAutoMerge) ||
		current.Paused || current.ProviderCommand != runner.providerCommand || !reflect.DeepEqual(current.ProviderArgs, runner.providerArgs) {
		return errors.New("current installed Visual Hive runtime no longer matches the configured normal Worker bridge")
	}
	return nil
}

func exactWorkerCommands(required []string, configured [][]string) ([]repair.Command, error) {
	result := make([]repair.Command, 0, len(required))
	for _, expected := range required {
		matched := false
		for _, command := range configured {
			if len(command) > 0 && strings.Join(command, " ") == expected {
				result = append(result, repair.Command{Name: command[0], Args: append([]string(nil), command[1:]...)})
				matched = true
				break
			}
		}
		if !matched {
			return nil, fmt.Errorf("verified validation command %q is not the current installed argv", expected)
		}
	}
	if len(result) == 0 {
		return nil, errors.New("normal Worker requires at least one exact installed validation command")
	}
	return result, nil
}

func boundedVisualWorkDuration(deadline time.Time, maximum time.Duration) time.Duration {
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return time.Second
	}
	if remaining < maximum {
		return remaining
	}
	return maximum
}

func configureNormalVisualWorkRunner(
	installed integrated.Config,
	controller *visualcontroller.Controller,
	lifecycle *visualhive.LifecycleStore,
	sched *scheduler.Scheduler,
	manager *agent.Manager,
	github *hivegithub.Client,
	logger *slog.Logger,
) (*normalservice.Service, error) {
	if installed.Automation != integrated.AutomationRepairPR && installed.Automation != integrated.AutomationAutoMerge {
		return nil, nil
	}
	if github == nil {
		return nil, errors.New("normal governed repair service requires the existing GitHub client")
	}
	executor, err := repair.NewCodexSpecialistChildExecutor(repair.CodexProvider{Command: installed.ProviderCommand, Prefix: installed.ProviderArgs})
	if err != nil {
		return nil, err
	}
	if err := manager.ConfigureSpecialistChildDispatcher(agent.SpecialistChildDispatcherOptions{
		RepairStateRoot: filepath.Join(installed.StateDir, "repair"), Executor: executor,
	}); err != nil {
		return nil, err
	}
	loader := func() (integrated.Config, int, error) {
		current, exists, err := loadAuthoritativeVisualWorkContract()
		if err != nil {
			return integrated.Config{}, 0, err
		}
		if !exists {
			return integrated.Config{}, 0, errors.New("authoritative installed Visual Hive contract is unavailable")
		}
		return current, manager.GetACMMLevel(), nil
	}
	source := &normalVisualArtifactSource{stateDir: installed.StateDir, timeout: 45 * time.Minute, github: github}
	repairer := &normalVisualRepairer{
		scheduler: sched, manager: manager, controller: controller, lifecycle: lifecycle, github: github,
		providerCommand: installed.ProviderCommand, providerArgs: append([]string(nil), installed.ProviderArgs...), loadConfig: loader,
	}
	verdict := &normalVisualPullRequestVerifier{github: github, lifecycle: lifecycle, loadConfig: loader}
	poll := time.Duration(installed.RunIntervalSeconds) * time.Second
	service, err := normalservice.New(normalservice.Options{
		StateDir: filepath.Join(installed.StateDir, "visual-hive"), PollInterval: poll, LeaseRetry: 30 * time.Second,
		AcquireLease: func() (func(), error) { return integrated.AcquireNormalVisualWorkLease(installed.StateDir) },
		Source:       source, Intake: controller, Repairer: repairer,
		// The verifier applies only its opaque check-evidence capability. The
		// service/controller still own completion and workflow consumption; no
		// merge, baseline, issue-resolution, or repository-write authority exists.
		Verdict: verdict, Logger: logger,
	})
	return service, err
}
