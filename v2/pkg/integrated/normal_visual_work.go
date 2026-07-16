package integrated

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	hivegithub "github.com/kubestellar/hive/v2/pkg/github"
)

// NormalVisualWork is the sealed production artifact returned to the normal
// Hive service. Fetching it performs the existing exact dispatch, job,
// artifact, producer, source-index, and live-head verification, but deliberately
// performs no legacy lifecycle, bead, repair-manager, baseline, or merge work.
type NormalVisualWork struct {
	Config   Config
	Workflow WorkflowRunEvidence
	Artifact hivegithub.VerifiedVisualHiveArtifact
}

// FetchNormalVisualWork reuses the released integrated transport/verifier as a
// narrow source for the normal service. The caller must own the shared
// production-run lease for its complete lifetime.
func FetchNormalVisualWork(ctx context.Context, stateDir string, timeout time.Duration, client *hivegithub.Client) (NormalVisualWork, error) {
	if client == nil || stateDir == "" {
		return NormalVisualWork{}, errors.New("normal Visual Hive fetch requires GitHub and persistent state")
	}
	if timeout <= 0 {
		timeout = 45 * time.Minute
	}
	store, err := NewStore(filepath.Join(stateDir, "integrated"))
	if err != nil {
		return NormalVisualWork{}, err
	}
	config, err := store.Load()
	if err != nil {
		return NormalVisualWork{}, err
	}
	if config.ExecutionMode == ExecutionHosted {
		return NormalVisualWork{}, errors.New("hosted installation remains owned by its GitHub controller")
	}
	if config.Automation != AutomationRepairPR && config.Automation != AutomationAutoMerge {
		return NormalVisualWork{}, errors.New("normal governed repair service requires repair-pr authority")
	}
	pauseRequested, err := PauseRequested(stateDir)
	if err != nil {
		return NormalVisualWork{}, fmt.Errorf("read pause request: %w", err)
	}
	if config.Paused || pauseRequested {
		return NormalVisualWork{}, errors.New("repository automation is paused")
	}
	if _, err := verifyLiveRepositoryIdentity(ctx, client, config); err != nil {
		return NormalVisualWork{}, err
	}
	if err := VerifyInstalledSetup(ctx, client, config); err != nil {
		return NormalVisualWork{}, err
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	workflow, err := dispatchAndWait(runCtx, client, config)
	if err != nil {
		return NormalVisualWork{}, err
	}
	_, verified, err := client.FetchAndVerifyVisualHiveBundle(runCtx, hivegithub.VisualHiveArtifactRequest{
		Repository: config.Repository, WorkflowRunID: workflow.RunID, ArtifactID: workflow.BundleArtifact,
		SourceArtifactID: workflow.EvidenceArtifact, DestinationDir: filepath.Join(stateDir, "visual-hive", "artifacts"),
		FetchSourceArtifact: true, TargetRef: config.DefaultBranch, MaxACMM: config.ACMMLevel,
		ExpectedWorkflowName: visualHiveProductionWorkflowName, ExpectedWorkflowPath: visualHiveProductionWorkflowPath,
		ExpectedEvent: visualHiveProductionWorkflowEvent, ExpectedRunName: workflowDispatchDisplayTitle(workflow.CorrelationID),
		ExpectedProducerGitCommit: config.VisualHiveRef, AllowFailedWorkflowRun: workflow.RepositoryTestOverall != 0,
	})
	if err != nil {
		return NormalVisualWork{}, err
	}
	if _, err := requireLiveInstalledWorkflowHead(runCtx, client, config, workflow.HeadSHA); err != nil {
		var stale *staleWorkflowHeadError
		if errors.As(err, &stale) {
			if discardErr := discardStaleWorkflowDispatch(stateDir, config, workflow, stale); discardErr != nil {
				return NormalVisualWork{}, discardErr
			}
		}
		return NormalVisualWork{}, err
	}
	return NormalVisualWork{Config: config, Workflow: workflow, Artifact: verified}, nil
}

// ConsumeNormalVisualWork retires only the exact durable workflow dispatch.
// allowAlreadyConsumed is used solely for crash recovery after the service
// durably recorded its final exact-head receipt before deleting the intent.
func ConsumeNormalVisualWork(stateDir string, workflow WorkflowRunEvidence, allowAlreadyConsumed bool) error {
	store, err := NewStore(filepath.Join(stateDir, "integrated"))
	if err != nil {
		return err
	}
	_, exists, err := store.LoadWorkflowDispatchIntent()
	if err != nil {
		return err
	}
	if !exists && allowAlreadyConsumed {
		return nil
	}
	return consumeWorkflowDispatch(stateDir, workflow)
}

// AcquireNormalVisualWorkLease claims the same OS-backed lifetime ownership as
// legacy RunOnce. Its long metadata horizon is informational; the kernel lock
// and legacy compatibility sentinel remain authoritative until release.
func AcquireNormalVisualWorkLease(stateDir string) (func(), error) {
	return acquireProductionRunLease(stateDir, 10*365*24*time.Hour)
}
