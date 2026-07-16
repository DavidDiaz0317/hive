package repair

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kubestellar/hive/v2/pkg/agent"
	"github.com/kubestellar/hive/v2/pkg/automation"
	"github.com/kubestellar/hive/v2/pkg/visualhive"
)

func TestWorkerRecoversOneSpecialistInvocationIntoOnePullRequest(t *testing.T) {
	repository, _ := seedGitRepository(t)
	baseSHA := strings.TrimSpace(gitOutput(t, repository, "rev-parse", "HEAD"))
	baseTree := strings.TrimSpace(gitOutput(t, repository, "rev-parse", "HEAD^{tree}"))
	state, err := NewStore(filepath.Join(t.TempDir(), "repair-state"))
	if err != nil {
		t.Fatal(err)
	}
	clock := time.Now().UTC().Truncate(time.Second)
	anchor := clock
	roleWorkDir := t.TempDir()
	mailbox, err := agent.NewSpecialistMailbox(filepath.Join(roleWorkDir, "mailbox"), agent.SpecialistMailboxOptions{Now: func() time.Time { return clock }})
	if err != nil {
		t.Fatal(err)
	}
	identity := agent.SpecialistSessionIdentity{
		Specialist: agent.SpecialistQuality, AgentName: "quality", AgentID: "quality", Backend: "codex",
		SessionID: "hive-quality-recovery", TmuxSession: "hive-quality-recovery", WorkDir: roleWorkDir,
		ProviderSHA256: strings.Repeat("e", 64),
	}
	fingerprint := strings.Repeat("9", 64)
	providerConfig := SpecialistProviderConfig{
		Mailbox: mailbox, Repository: "owner/repo", RepositoryFingerprint: fingerprint,
		RecurrenceKey: fingerprint + ":r0", Attempt: 1,
		ExpectedBaseSHA: baseSHA, ExpectedBaseTreeSHA: baseTree,
		Evidence: agent.SpecialistEvidenceIdentity{
			BundleSchemaVersion: "visual-hive.hive-bundle.v3", BundleSHA256: strings.Repeat("b", 64),
			VerificationReceiptSHA256: strings.Repeat("c", 64), WorkflowRunID: 41, WorkflowRunAttempt: 1,
			WorkflowRunHeadSHA: baseSHA, ArtifactID: 73, ArtifactName: "visual-hive-evidence", ArtifactSHA256: strings.Repeat("d", 64),
		},
		Specialist: agent.SpecialistQuality, RouteReason: "test adequacy routes to quality",
		AllowedPaths: []string{"src/**"}, Validation: []string{"git diff --check"},
		Deadline: anchor.Add(25 * time.Minute), PollInterval: 5 * time.Millisecond, Now: func() time.Time { return clock },
	}
	lifecycle := &fakeLifecycle{}
	pulls := &fakePRClient{state: state}
	workerConfig := Config{
		RepositoryDir: repository, WorktreeRoot: filepath.Join(t.TempDir(), "worktrees"), BaseBranch: "main", Agent: "quality",
		Policy:             automation.Policy{ACMMLevel: 5, Mode: automation.ModeRepairPR, AllowedRepositories: []string{"owner/repo"}, MaxRepairAttempts: 3},
		AllowedRepairPaths: []string{"src/**"}, ValidationCommands: []Command{{Name: "git", Args: []string{"diff", "--check"}}},
		AttemptStartedAt: anchor, ModelTimeout: time.Minute, CommandTimeout: time.Minute,
	}
	finding := visualhive.FindingLifecycle{
		Repository: "owner/repo", RepositoryID: "123", RepositoryFingerprint: fingerprint, Fingerprint: "specialist-recovery",
		Status: visualhive.StatusIssueOpen, Title: "Repair the value", Body: "The verified value remains broken.",
		IssueKind: "test_adequacy", Severity: "medium", OwningAgentHint: "quality",
		AffectedContracts: []string{"src/value.txt"}, ValidationCommand: "git diff --check",
		IssueNumber: 9, IssueURL: "https://example.test/issues/9",
	}

	firstDispatcher := &fakeSpecialistDispatcher{identity: identity}
	providerConfig.Dispatcher = firstDispatcher
	firstProvider, err := NewSpecialistProvider(providerConfig)
	if err != nil {
		t.Fatal(err)
	}
	firstCtx, cancelFirst := context.WithCancel(context.Background())
	firstDispatcher.dispatch = func(agent.SpecialistDispatchRequest) { cancelFirst() }
	firstWorker := &Worker{Config: workerConfig, Provider: firstProvider, State: state, Lifecycle: lifecycle, GitHub: pulls}
	_, firstErr := firstWorker.Run(firstCtx, finding)
	if !errors.Is(firstErr, ErrSpecialistWorkPending) {
		t.Fatalf("interrupted first run = %v, want durable pending work", firstErr)
	}
	attempt, ok := state.Get(fingerprint)
	if !ok || attempt.Stage != StageModelRunning || !validSpecialistWorkOrderID(attempt.ModelInvocationID) || attempt.AttemptCounted {
		t.Fatalf("interrupted specialist checkpoint = %+v", attempt)
	}
	if len(firstDispatcher.dispatchCalls) != 1 || firstDispatcher.dispatchCalls[0].TaskID != attempt.ModelInvocationID {
		t.Fatalf("first dispatch = %+v, checkpoint=%s", firstDispatcher.dispatchCalls, attempt.ModelInvocationID)
	}
	writeSpecialistOutput(t, mailbox, attempt.ModelInvocationID, agent.SpecialistCompletionProposed, []byte(specialistProviderTestDiff), "recovered one exact proposal")

	secondDispatcher := &fakeSpecialistDispatcher{identity: identity, releaseErr: agent.ErrSpecialistTaskLeaseAbsent}
	providerConfig.Dispatcher = secondDispatcher
	secondProvider, err := NewSpecialistProvider(providerConfig)
	if err != nil {
		t.Fatal(err)
	}
	secondWorker := &Worker{Config: workerConfig, Provider: secondProvider, State: state, Lifecycle: lifecycle, GitHub: pulls}
	secondCtx, cancelSecond := context.WithTimeout(context.Background(), time.Minute)
	result, err := secondWorker.Run(secondCtx, finding)
	cancelSecond()
	if err != nil {
		t.Fatal(err)
	}
	if result.PRNumber != 17 || pulls.calls != 1 || secondDispatcher.checkCalls != 0 || secondDispatcher.inspectCalls != 1 || len(secondDispatcher.dispatchCalls) != 0 {
		t.Fatalf("recovery result=%+v pulls=%d check/inspect/dispatch=%d/%d/%d", result, pulls.calls, secondDispatcher.checkCalls, secondDispatcher.inspectCalls, len(secondDispatcher.dispatchCalls))
	}
	attempt, _ = state.Get(fingerprint)
	if attempt.Stage != StagePROpen || !attempt.AttemptCounted || attempt.Attempt != 1 || attempt.ModelInvocationID != firstDispatcher.dispatchCalls[0].TaskID {
		t.Fatalf("completed recovery checkpoint = %+v", attempt)
	}
	third, err := secondWorker.Run(context.Background(), finding)
	if err != nil || !third.Resumed || pulls.calls != 1 || len(secondDispatcher.dispatchCalls) != 0 {
		t.Fatalf("idempotent rerun = %+v, %v pulls=%d dispatch=%d", third, err, pulls.calls, len(secondDispatcher.dispatchCalls))
	}
}
