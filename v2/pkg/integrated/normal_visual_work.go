package integrated

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
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

// UsesNormalHiveRuntime reports whether the existing ordinary Hive/dashboard
// process owns local Visual Hive repair cadence. Advisory and issues-only local
// installations retain the legacy scheduler, while hosted installations remain
// owned by their repository controller.
func UsesNormalHiveRuntime(config Config) bool {
	return config.ExecutionMode == ExecutionLocal && config.VisualHive &&
		(config.Automation == AutomationRepairPR || config.Automation == AutomationAutoMerge)
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
	if _, err := synchronizeNormalVisualWorkBase(runCtx, config, workflow.HeadSHA); err != nil {
		return NormalVisualWork{}, err
	}
	return NormalVisualWork{Config: config, Workflow: workflow, Artifact: verified}, nil
}

// synchronizeNormalVisualWorkBase makes the already verified live workflow
// head available to the normal repair controller without moving or cleaning
// the Hive-owned checkout. The explicit refspec prevents a target-controlled
// remote name or ref from widening this authenticated transport boundary.
func synchronizeNormalVisualWorkBase(ctx context.Context, config Config, expectedHead string) (string, error) {
	expectedHead = strings.ToLower(strings.TrimSpace(expectedHead))
	if !immutableCommit.MatchString(expectedHead) {
		return "", errors.New("normal Visual Hive work requires an immutable verified workflow head")
	}
	branch := strings.TrimSpace(config.DefaultBranch)
	if !validLegacyBranchName(branch) {
		return "", errors.New("normal Visual Hive work requires a safe installed default branch")
	}
	exists, err := validateManagedCheckoutBeforeGit(config.CheckoutDir, config.Repository)
	if err != nil {
		return "", fmt.Errorf("validate managed checkout before normal Visual Hive synchronization: %w", err)
	}
	if !exists {
		return "", errors.New("normal Visual Hive managed checkout is unavailable")
	}
	remoteURL := RepositoryCloneURL(config.Repository)
	remoteRef := "refs/remotes/origin/" + branch
	refspec := "+refs/heads/" + branch + ":" + remoteRef
	if _, err := gitTransport(ctx, config.CheckoutDir, remoteURL, "fetch", "--no-tags", "--no-recurse-submodules", remoteURL, refspec); err != nil {
		return "", fmt.Errorf("fetch verified normal Visual Hive base: %w", err)
	}
	fetchedHead, err := git(ctx, config.CheckoutDir, "rev-parse", "--verify", remoteRef+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("resolve fetched normal Visual Hive head: %w", err)
	}
	fetchedHead = strings.ToLower(strings.TrimSpace(fetchedHead))
	if !immutableCommit.MatchString(fetchedHead) || fetchedHead != expectedHead {
		return "", fmt.Errorf("fetched default-branch head %s does not match verified workflow head %s", fetchedHead, expectedHead)
	}
	tree, err := git(ctx, config.CheckoutDir, "rev-parse", "--verify", expectedHead+"^{tree}")
	if err != nil {
		return "", fmt.Errorf("resolve verified normal Visual Hive base tree: %w", err)
	}
	tree = strings.ToLower(strings.TrimSpace(tree))
	if !validNormalVisualGitObjectID(tree) {
		return "", errors.New("verified normal Visual Hive base tree identity is invalid")
	}
	return tree, nil
}

func validNormalVisualGitObjectID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
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
