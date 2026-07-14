package integrated

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/kubestellar/hive/v2/pkg/automation"
	hivegithub "github.com/kubestellar/hive/v2/pkg/github"
	"gopkg.in/yaml.v3"
)

const (
	checkoutActionSHA       = "9c091bb21b7c1c1d1991bb908d89e4e9dddfe3e0" // actions/checkout v7.0.0
	setupNodeActionSHA      = "48b55a011bda9f5d6aeb4c2d9c7362e8dae4041e" // actions/setup-node v6.4.0
	setupPythonActionSHA    = "a309ff8b426b58ec0e2a45f0f869d46889d02405" // actions/setup-python v6.2.0
	uploadArtifactActionSHA = "043fb46d1a93c77aae656e7c1c64a875d1fc6a0a" // actions/upload-artifact v7.0.1
)

var (
	strictPackageRunPattern = regexp.MustCompile(`^(npm|pnpm|yarn)[[:space:]]+(--silent[[:space:]]+)?run[[:space:]]+([A-Za-z0-9_.:-]+)([[:space:]]+--([[:space:]]+[A-Za-z0-9_./:@%+=,-]+)*)?$`)
	safeShellTokenPattern   = regexp.MustCompile(`^[A-Za-z0-9_./:@%+=,\\-]+$`)
	environmentTokenPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=[A-Za-z0-9_./:@%+=,\\-]+$`)
	setupRepositoryCloneURL = func(repository string) string { return "https://github.com/" + repository + ".git" }
)

type packageScriptInvocation struct {
	Name      string
	Forwarded string
}

type SetupOptions struct {
	Repository             string
	Coverage               Coverage
	Automation             Automation
	Provider               string
	ProviderCommand        string
	ProviderArgs           []string
	VisualHive             bool
	StateDir               string
	Apply                  bool
	Start                  bool
	VisualHiveCommand      string
	VisualHiveArgs         []string
	VisualHiveRepo         string
	VisualHiveRef          string
	MaxActiveIssues        int
	MaxRepairAttempts      int
	AllowedAutoMergePaths  []string
	AllowedAutoMergeRisk   []automation.RiskTier
	AutoMergePathsExplicit bool
	AutoMergeRiskExplicit  bool
	GitHub                 *hivegithub.Client
	Policy                 automation.Policy
}

type setupPRClient interface {
	UpsertRepairPullRequest(context.Context, string, string, string, string, string, string, string) (hivegithub.RepairPullRequest, error)
}

func RunSetup(ctx context.Context, options SetupOptions) (SetupResult, error) {
	stateDir, err := filepath.Abs(options.StateDir)
	if err != nil {
		return SetupResult{}, fmt.Errorf("resolve persistent state directory: %w", err)
	}
	options.StateDir = filepath.Clean(stateDir)
	if err := validateSetupOptions(options); err != nil {
		return SetupResult{}, err
	}
	if err := validateSetupStateRoot(options.StateDir, options.Repository); err != nil {
		return SetupResult{}, err
	}
	if options.Apply {
		release, leaseErr := acquireProductionRunLease(options.StateDir, 25*time.Minute)
		if leaseErr != nil {
			return SetupResult{}, fmt.Errorf("serialize setup with production runs: %w", leaseErr)
		}
		defer release()
	}
	store, err := NewStore(filepath.Join(options.StateDir, "integrated"))
	if err != nil {
		return SetupResult{}, err
	}
	prior, priorErr := store.Load()
	hasPrior := priorErr == nil
	if priorErr != nil && !errors.Is(priorErr, os.ErrNotExist) {
		return SetupResult{}, fmt.Errorf("load existing integrated configuration: %w", priorErr)
	}
	if hasPrior && !strings.EqualFold(prior.Repository, options.Repository) {
		return SetupResult{}, fmt.Errorf("state directory %s is already bound to %s; use a different --state-dir for %s", options.StateDir, prior.Repository, options.Repository)
	}
	if options.Apply {
		if transfer, exists, transferErr := store.LoadAuthorizerTransferIntent(); transferErr != nil {
			return SetupResult{}, transferErr
		} else if exists {
			return SetupResult{}, fmt.Errorf("setup authorizer transfer to %s (numeric ID %d) is pending; finish its exact next command before changing managed setup", transfer.NewAuthorizerLogin, transfer.NewAuthorizerID)
		}
	}
	if options.Apply && options.GitHub == nil {
		return SetupResult{}, fmt.Errorf("GitHub client is required to apply setup")
	}
	var legacyCheckout *legacyManagedCheckoutProof
	if hasPrior && options.GitHub != nil {
		liveRepositoryID, identityErr := verifyLiveRepositoryIdentity(ctx, options.GitHub, prior)
		if identityErr != nil {
			return SetupResult{}, fmt.Errorf("verify existing setup repository identity before checkout access: %w", identityErr)
		}
		legacyCheckout = &legacyManagedCheckoutProof{Config: prior, LiveRepositoryID: liveRepositoryID}
	}
	checkout, err := managedSetupCheckoutPath(store.Dir(), options.StateDir, options.Repository, prior, hasPrior)
	if err != nil {
		return SetupResult{}, err
	}
	defaultBranch, err := ensureCheckoutWithLegacy(ctx, options.Repository, checkout, legacyCheckout)
	if err != nil {
		return SetupResult{}, fmt.Errorf("setup checkout in state directory %s: %w", options.StateDir, err)
	}
	if err := validateOrdinarySetupCheckout(checkout); err != nil {
		return SetupResult{}, err
	}
	inspection, err := InspectCheckout(checkout, defaultBranch)
	if err != nil {
		return SetupResult{}, err
	}
	if options.GitHub != nil {
		enrichRemoteInspection(ctx, options.GitHub, options.Repository, &inspection)
	}
	if !options.Apply && strings.TrimSpace(inspection.RepositoryID) != "" {
		if err := ensureStateOwnershipMarker(options.StateDir, Config{Repository: options.Repository, RepositoryID: inspection.RepositoryID}); err != nil {
			return SetupResult{}, fmt.Errorf("bind planned state directory to exact repository identity: %w", err)
		}
	}
	options = effectiveSetupAutoMergePolicy(options, prior, hasPrior)
	plan := buildSetupPlan(options, inspection)
	result := SetupResult{Plan: plan, ProtectionActivationPending: options.Automation == AutomationAutoMerge, ActivationMessage: setupActivationMessage(options.Automation, options.Start, true)}
	if !options.Apply {
		return result, nil
	}
	if strings.TrimSpace(inspection.RepositoryID) == "" {
		return result, fmt.Errorf("GitHub did not return the immutable repository ID for %s; refusing setup mutations", options.Repository)
	}
	authorizer, err := options.GitHub.AuthenticatedNumericUser(ctx)
	if err != nil {
		return result, err
	}
	if options.VisualHive {
		if err := VerifyVisualHiveCommit(ctx, options.GitHub, options.VisualHiveRepo, options.VisualHiveRef); err != nil {
			return result, err
		}
	}
	branch := managedOperationBranch("setup", inspection.RepositoryID)
	if err := authorizeSetup(store, options.Policy, options.Repository, automation.ActionSetupBranch); err != nil {
		return result, err
	}
	if _, err := git(ctx, checkout, "fetch", "--prune", "origin", defaultBranch); err != nil {
		return result, err
	}
	if _, err := git(ctx, checkout, "switch", "-C", branch, "origin/"+defaultBranch); err != nil {
		return result, err
	}
	if err := validateOrdinarySetupCheckout(checkout); err != nil {
		return result, err
	}
	if options.VisualHive {
		if err := runVisualHiveSetup(ctx, options, checkout); err != nil {
			return result, err
		}
		if err := validateOrdinarySetupCheckout(checkout); err != nil {
			return result, fmt.Errorf("Visual Hive static setup produced an unsafe checkout entry: %w", err)
		}
	}
	setupBaselineRequired := false
	setupBaselineContractDigest := ""
	if options.VisualHive {
		requiresBaselines, contractDigest, baselinePlanErr := visualHiveScreenshotContractInventory(checkout)
		if baselinePlanErr != nil {
			return result, baselinePlanErr
		}
		// A partial baseline inventory is not sufficient: hosted capture must
		// prove every configured screenshot contract. Proposal creation later
		// filters the capture against the exact target head and reviews only the
		// missing or changed PNG delta, preserving identical reviewed bytes.
		setupBaselineRequired = requiresBaselines
		setupBaselineContractDigest = contractDigest
	}
	config := Config{
		SchemaVersion: ConfigSchema, Repository: options.Repository, RepositoryID: inspection.RepositoryID, DefaultBranch: defaultBranch,
		Coverage: options.Coverage, Automation: options.Automation, Provider: options.Provider,
		ProviderCommand: options.ProviderCommand, ProviderArgs: append([]string(nil), options.ProviderArgs...),
		ACMMLevel: acmmForAutomation(options.Automation), VisualHive: options.VisualHive,
		SetupBaselineRequired:       setupBaselineRequired,
		SetupBaselineContractDigest: setupBaselineContractDigest,
		MaxActiveIssues:             options.MaxActiveIssues,
		MaxRepairAttempts:           options.MaxRepairAttempts,
		VisualHiveRepo:              options.VisualHiveRepo, VisualHiveRef: options.VisualHiveRef,
		VisualHiveCommand: options.VisualHiveCommand, VisualHiveArgs: append([]string(nil), options.VisualHiveArgs...),
		TestCommands: testCommandsForCoverage(inspection, options.Coverage), AllowedRepairPaths: defaultAllowedRepairPaths(),
		AllowedAutoMergePaths: append([]string(nil), options.AllowedAutoMergePaths...),
		AllowedAutoMergeRisk:  append([]automation.RiskTier(nil), options.AllowedAutoMergeRisk...),
		CheckoutDir:           checkout, StateDir: options.StateDir, SetupBranch: branch,
		SetupAuthorizationActorID: authorizer.ID,
		InstalledAt:               time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if options.VisualHive {
		config.VisualHiveConfigDigest, err = visualHiveConfigDigest(checkout)
		if err != nil {
			return result, err
		}
	}
	if hasPrior {
		config.InstalledAt = prior.InstalledAt
		config.SetupBranch, config.SetupPRNumber, config.SetupPRURL, config.SetupHeadSHA = prior.SetupBranch, prior.SetupPRNumber, prior.SetupPRURL, prior.SetupHeadSHA
		config.PreviousVersion, config.Paused = prior.PreviousVersion, prior.Paused
		config.SetupBaselineInitialDigest = prior.SetupBaselineInitialDigest
		config.SetupBaselineInitialCandidates = append([]SetupBaselineCandidate(nil), prior.SetupBaselineInitialCandidates...)
		config.AllowedRepairPaths = append([]string(nil), prior.AllowedRepairPaths...)
		if prior.SetupAuthorizationActorID > 0 {
			config.SetupAuthorizationActorID = prior.SetupAuthorizationActorID
		}
	}
	if err := writeManagedFiles(checkout, config, inspection); err != nil {
		return result, err
	}
	managed := append([]string(nil), plan.FilesToManage...)
	if err := authorizeSetup(store, options.Policy, options.Repository, automation.ActionSetupCommit); err != nil {
		return result, err
	}
	if err := stageManagedPaths(ctx, checkout, managed); err != nil {
		return result, err
	}
	diff, err := git(ctx, checkout, "diff", "--cached", "--name-only")
	if err != nil {
		return result, err
	}
	// A different authenticated maintainer rerunning an otherwise identical
	// setup remains a true no-op. A substantive change keeps the human identity
	// already embedded in the default-branch verifier: pull_request workflows
	// execute from the base branch, so silently rotating that identity in the
	// proposed head would make the out-of-band status unverifiable.
	if hasPrior && prior.SetupAuthorizationActorID > 0 && authorizer.ID != prior.SetupAuthorizationActorID && strings.TrimSpace(diff) != "" {
		return result, fmt.Errorf("managed setup changes are bound to GitHub user ID %d, but the current authenticated user is %d; rerun with the recorded setup authorizer (an identical cross-user rerun remains a no-op)", prior.SetupAuthorizationActorID, authorizer.ID)
	}
	if updateSetupAuthorizationBranchForManagedChange(&config, branch, diff) {
		if err := writeManagedFiles(checkout, config, inspection); err != nil {
			return result, err
		}
		if err := stageManagedPaths(ctx, checkout, managed); err != nil {
			return result, err
		}
		diff, err = git(ctx, checkout, "diff", "--cached", "--name-only")
		if err != nil {
			return result, err
		}
	}
	idempotent := strings.TrimSpace(diff) == ""
	reuseRemoteSetup := false
	sha := ""
	if !idempotent && hasPrior && prior.SetupBranch != "" {
		matches, remoteSHA, matchErr := stagedTreeMatchesRemoteBranch(ctx, checkout, prior.SetupBranch)
		if matchErr != nil {
			return result, matchErr
		}
		if matches {
			idempotent, reuseRemoteSetup, sha, branch = true, true, remoteSHA, prior.SetupBranch
		}
	}
	knownSetupHead := config.SetupHeadSHA
	if reuseRemoteSetup {
		knownSetupHead = sha
	}
	if err := reconcileActiveSetupBaselineReconfiguration(ctx, store, config, knownSetupHead, idempotent, options.GitHub); err != nil {
		return result, err
	}
	if idempotent && hasPrior && !reuseRemoteSetup {
		sha, shaErr := git(ctx, checkout, "rev-parse", "HEAD")
		if shaErr != nil {
			return result, shaErr
		}
		if config.SetupBaselineRequired && config.SetupBaselineInitialDigest == "" {
			config.SetupBaselineInitialCandidates, config.SetupBaselineInitialDigest, err = setupBaselineInventoryAtCommit(ctx, checkout, strings.TrimSpace(sha))
			if err != nil {
				return result, fmt.Errorf("bind exact pre-setup baseline inventory: %w", err)
			}
		}
		if err := store.Save(config); err != nil {
			return result, err
		}
		if err := setSetupBaselineStatus(&result, store, config, config.SetupHeadSHA); err != nil {
			return result, err
		}
		if err := completeSetupBaselineRebind(store, config); err != nil {
			return result, err
		}
		result.Applied, result.Idempotent, result.Config = true, true, &config
		result.Branch, result.PRNumber, result.PRURL = prior.SetupBranch, prior.SetupPRNumber, prior.SetupPRURL
		result.CommitSHA = strings.TrimSpace(sha)
		if err := setSetupActivationStatus(&result, store, config, options.Start); err != nil {
			return result, err
		}
		return result, nil
	}
	if !idempotent {
		if _, err := git(ctx, checkout, "-c", "user.name=Hive Setup", "-c", "user.email=hive-setup@users.noreply.github.com", "commit", "-m", "chore: install Hive and Visual Hive", "-m", managedCommitTrailers(inspection.RepositoryID, "setup")); err != nil {
			return result, err
		}
	}
	if sha == "" {
		sha, err = git(ctx, checkout, "rev-parse", "HEAD")
		if err != nil {
			return result, err
		}
		sha = strings.TrimSpace(sha)
	}
	if config.SetupBaselineRequired && config.SetupBaselineInitialDigest == "" {
		config.SetupBaselineInitialCandidates, config.SetupBaselineInitialDigest, err = setupBaselineInventoryAtCommit(ctx, checkout, sha)
		if err != nil {
			return result, fmt.Errorf("bind exact pre-setup baseline inventory: %w", err)
		}
	}
	config.SetupBranch = branch
	if !reuseRemoteSetup {
		if err := authorizeSetup(store, options.Policy, options.Repository, automation.ActionSetupPush); err != nil {
			return result, err
		}
		if err := pushManagedBranch(ctx, checkout, branch, inspection.RepositoryID, "setup", sha); err != nil {
			return result, err
		}
	}
	// A matching remote setup tree is reused without another commit or
	// force-push, while the PR upsert still recovers an interrupted or
	// manually closed setup PR.
	if err := authorizeSetup(store, options.Policy, options.Repository, automation.ActionSetupPR); err != nil {
		return result, err
	}
	marker := "<!-- hive-setup: " + strings.ToLower(options.Repository) + " -->"
	body := setupPRBody(marker, plan)
	pull, err := options.GitHub.UpsertRepairPullRequest(ctx, options.Repository, branch, sha, defaultBranch, "Install Hive + Visual Hive production automation", body, marker)
	if err != nil {
		return result, err
	}
	if err := verifyManagedPullHead("setup", pull, sha); err != nil {
		return result, err
	}
	config.SetupPRNumber, config.SetupPRURL = pull.Number, pull.URL
	config.SetupHeadSHA = strings.ToLower(strings.TrimSpace(sha))
	authorization, err := authorizeManagedSetupPullRequest(ctx, store, options.GitHub, options.Policy, config, "setup", checkout, branch, sha, pull.URL, pull.Number)
	if err != nil {
		return result, err
	}
	if err := store.Save(config); err != nil {
		return result, err
	}
	if err := setSetupBaselineStatus(&result, store, config, config.SetupHeadSHA); err != nil {
		return result, err
	}
	if err := completeSetupBaselineRebind(store, config); err != nil {
		return result, err
	}
	result.Applied, result.Idempotent, result.Config = true, idempotent, &config
	result.Branch, result.CommitSHA, result.PRNumber, result.PRURL = branch, sha, pull.Number, pull.URL
	result.SetupAuthorizationContext, result.SetupAuthorizationStatusID = authorization.Status.Context, authorization.Status.StatusID
	result.SetupAuthorizationCreatorID, result.SetupAuthorizationReused = authorization.Status.CreatorID, authorization.Status.Reused
	if err := setSetupActivationStatus(&result, store, config, options.Start); err != nil {
		return result, err
	}
	return result, nil
}

func setSetupBaselineStatus(result *SetupResult, store *Store, config Config, setupHead string) error {
	if result == nil || !config.SetupBaselineRequired {
		return nil
	}
	baseline, err := ensureSetupBaselineIntent(store, config, strings.ToLower(strings.TrimSpace(setupHead)), config.SetupPRNumber, config.SetupPRURL)
	if err != nil {
		return err
	}
	result.SetupBaselinePending, result.SetupBaselinePhase = baseline.Phase != SetupBaselineProductionVerified || baseline.PendingAudit != nil, baseline.Phase
	result.SetupBaselineNextCommand = "hive run --state-dir " + strconv.Quote(config.StateDir) + " --json"
	return nil
}

func updateSetupAuthorizationBranchForManagedChange(config *Config, branch, stagedDiff string) bool {
	if config == nil || strings.TrimSpace(stagedDiff) == "" || strings.TrimSpace(branch) == "" || config.SetupBranch == branch {
		return false
	}
	config.SetupBranch = branch
	return true
}

func setSetupActivationStatus(result *SetupResult, store *Store, config Config, started bool) error {
	if result == nil || config.Automation != AutomationAutoMerge {
		return nil
	}
	state, exists, err := store.LoadProtectionActivation()
	if err != nil {
		return fmt.Errorf("read protection activation status: %w", err)
	}
	result.ProtectionActivationPending = !exists || !ProtectionActivationMatchesConfig(state, config)
	result.ActivationMessage = setupActivationMessage(config.Automation, started, result.ProtectionActivationPending)
	return nil
}

func setupActivationMessage(automation Automation, started, pending bool) string {
	if automation != AutomationAutoMerge {
		return ""
	}
	if !pending {
		return "Exact GitHub-Actions-App-bound PR visual-hive protection is durably activated; Hive verifies the producing PR workflow provenance before merge and hive doctor verifies live policy without changing it."
	}
	if started {
		return "Merge the exact managed setup PR and complete the trusted setup run. Hive has durably recorded the scheduler request and will activate it only after all non-scheduler doctor checks are green, before any lifecycle write."
	}
	return "Merge the exact managed setup PR, then run hive start or hive run. Hive will complete one trusted default-branch Visual Hive run and activate exact GitHub-Actions-App-bound protection before any lifecycle write."
}

func defaultAllowedRepairPaths() []string {
	return []string{"src/**", "**/src/**", "public/**", "**/public/**", "index.html", "**/index.html", "visual-hive.config.yaml", "test/**", "tests/**", "**/test/**", "**/tests/**", "**/*.test.*", "**/*.spec.*", "**/*_test.go"}
}

func defaultAllowedAutoMergePaths() []string {
	return []string{"test/**", "tests/**", "**/*.test.*", "**/*.spec.*", "**/*_test.go"}
}

func effectiveSetupAutoMergePolicy(options SetupOptions, prior Config, hasPrior bool) SetupOptions {
	if !options.AutoMergePathsExplicit {
		if hasPrior && len(prior.AllowedAutoMergePaths) > 0 {
			options.AllowedAutoMergePaths = append([]string(nil), prior.AllowedAutoMergePaths...)
		} else {
			options.AllowedAutoMergePaths = defaultAllowedAutoMergePaths()
		}
	}
	if !options.AutoMergeRiskExplicit {
		if hasPrior && len(prior.AllowedAutoMergeRisk) > 0 {
			options.AllowedAutoMergeRisk = append([]automation.RiskTier(nil), prior.AllowedAutoMergeRisk...)
		} else {
			options.AllowedAutoMergeRisk = []automation.RiskTier{automation.RiskAutomatic}
		}
	}
	options.AllowedAutoMergePaths = sortedUnique(options.AllowedAutoMergePaths)
	sort.Slice(options.AllowedAutoMergeRisk, func(i, j int) bool { return options.AllowedAutoMergeRisk[i] < options.AllowedAutoMergeRisk[j] })
	options.AllowedAutoMergeRisk = uniqueRiskTiers(options.AllowedAutoMergeRisk)
	return options
}

func uniqueRiskTiers(values []automation.RiskTier) []automation.RiskTier {
	result := values[:0]
	for _, value := range values {
		if len(result) == 0 || result[len(result)-1] != value {
			result = append(result, value)
		}
	}
	return result
}

func VerifyVisualHiveCommit(ctx context.Context, client *hivegithub.Client, repository, ref string) error {
	if client == nil || client.GoGitHub() == nil {
		return fmt.Errorf("GitHub client is required to verify the Visual Hive commit")
	}
	owner, repo, ok := strings.Cut(strings.TrimSpace(repository), "/")
	if !ok || owner == "" || repo == "" {
		return fmt.Errorf("Visual Hive repository must be owner/name")
	}
	commit, _, err := client.GoGitHub().Repositories.GetCommit(ctx, owner, repo, ref, nil)
	if err != nil {
		return fmt.Errorf("verify immutable Visual Hive commit %s in %s: %w", ref, repository, err)
	}
	if !exactCommitPin(ref, commit.GetSHA()) {
		return fmt.Errorf("Visual Hive ref resolved to %s instead of exact commit %s", commit.GetSHA(), ref)
	}
	return nil
}

type installedRepositoryConfig struct {
	SchemaVersion                     string                `json:"schema_version"`
	Repository                        string                `json:"repository"`
	RepositoryID                      string                `json:"repository_id"`
	DefaultBranch                     string                `json:"default_branch"`
	Coverage                          Coverage              `json:"coverage"`
	Automation                        Automation            `json:"automation"`
	Provider                          string                `json:"provider"`
	ACMMLevel                         int                   `json:"acmm_level"`
	MaxActiveIssues                   int                   `json:"max_active_issues"`
	MaxRepairAttempts                 int                   `json:"max_repair_attempts"`
	VisualHive                        bool                  `json:"visual_hive"`
	VisualHiveRepo                    string                `json:"visual_hive_repository"`
	VisualHiveRef                     string                `json:"visual_hive_ref"`
	VisualHiveConfigDigest            string                `json:"visual_hive_config_digest,omitempty"`
	TestCommands                      [][]string            `json:"test_commands"`
	AllowedRepairPaths                []string              `json:"allowed_repair_paths"`
	AllowedAutoMergePaths             []string              `json:"allowed_auto_merge_paths"`
	AllowedAutoMergeRisk              []automation.RiskTier `json:"allowed_auto_merge_risk"`
	SetupAuthorizationActorID         int64                 `json:"setup_authorization_actor_id"`
	SetupAuthorizationPreviousActorID int64                 `json:"setup_authorization_previous_actor_id,omitempty"`
}

// VerifyInstalledSetup proves that the exact durable policy and immutable pin
// are already present on the target branch. It is stronger than trusting a
// historical setup PR number, and makes a no-diff rerun recover cleanly when a
// redundant or stale setup PR was closed.
func VerifyInstalledSetup(ctx context.Context, client *hivegithub.Client, config Config) error {
	return verifyInstalledSetupAtRef(ctx, client, config, config.DefaultBranch)
}

// VerifyInstalledSetupAtCommit binds every managed-file read to one immutable
// target commit. Production evidence must pass this check after its hosted run
// and before any durable lifecycle application; a branch-ref preflight alone
// cannot authorize artifacts produced after an out-of-band policy change.
func VerifyInstalledSetupAtCommit(ctx context.Context, client *hivegithub.Client, config Config, commitSHA string) error {
	commitSHA = strings.ToLower(strings.TrimSpace(commitSHA))
	if !immutableCommit.MatchString(commitSHA) {
		return fmt.Errorf("exact 40-character target commit is required to verify installed setup")
	}
	return verifyInstalledSetupAtRef(ctx, client, config, commitSHA)
}

func verifyInstalledSetupAtRef(ctx context.Context, client *hivegithub.Client, config Config, ref string) error {
	if client == nil || client.GoGitHub() == nil {
		return fmt.Errorf("GitHub client is required to verify installed setup")
	}
	owner, repo, ok := strings.Cut(config.Repository, "/")
	if !ok || owner == "" || repo == "" || config.DefaultBranch == "" || strings.TrimSpace(ref) == "" {
		return fmt.Errorf("installed repository identity or default branch is incomplete")
	}
	content, _, _, err := client.GoGitHub().Repositories.GetContents(ctx, owner, repo, ".hive/integrated.json", &gh.RepositoryContentGetOptions{Ref: ref})
	if err != nil || content == nil {
		return fmt.Errorf("read managed setup from target branch: %w", err)
	}
	value, err := content.GetContent()
	if err != nil {
		return fmt.Errorf("decode managed setup from target branch: %w", err)
	}
	var installed installedRepositoryConfig
	if err := json.Unmarshal([]byte(value), &installed); err != nil {
		return fmt.Errorf("parse managed setup from target branch: %w", err)
	}
	expected := installedRepositoryConfig{
		SchemaVersion: ConfigSchema, Repository: config.Repository, RepositoryID: config.RepositoryID, DefaultBranch: config.DefaultBranch,
		Coverage: config.Coverage, Automation: config.Automation, Provider: config.Provider, ACMMLevel: config.ACMMLevel,
		MaxActiveIssues: config.MaxActiveIssues, MaxRepairAttempts: config.MaxRepairAttempts, VisualHive: config.VisualHive,
		VisualHiveRepo: config.VisualHiveRepo, VisualHiveRef: config.VisualHiveRef, VisualHiveConfigDigest: config.VisualHiveConfigDigest, TestCommands: config.TestCommands,
		AllowedRepairPaths: config.AllowedRepairPaths, AllowedAutoMergePaths: config.AllowedAutoMergePaths, AllowedAutoMergeRisk: config.AllowedAutoMergeRisk,
		SetupAuthorizationActorID:         config.SetupAuthorizationActorID,
		SetupAuthorizationPreviousActorID: config.SetupAuthorizationPreviousActorID,
	}
	if !reflect.DeepEqual(installed, expected) {
		return fmt.Errorf("target branch managed setup does not match the durable local policy; merge the current setup/upgrade PR before running Hive")
	}
	if !config.VisualHive {
		return fmt.Errorf("integrated Hive installations require Visual Hive; rerun setup with --visual-hive")
	}
	for relative, expected := range map[string]string{
		".github/workflows/hive-visual-hive.yml": workflow(config),
		".github/workflows/visual-hive-pr.yml":   pullRequestWorkflow(config),
	} {
		actual, readErr := readTargetFile(ctx, client, owner, repo, ref, relative)
		if readErr != nil {
			return fmt.Errorf("verify managed production file %s: %w", relative, readErr)
		}
		if normalizeManagedText(actual) != normalizeManagedText(expected) {
			return fmt.Errorf("managed production file %s does not match the durable immutable pin and policy; rerun setup and merge its exact reviewed PR", relative)
		}
	}
	visualConfig, readErr := readTargetFile(ctx, client, owner, repo, ref, "visual-hive.config.yaml")
	if readErr != nil {
		return fmt.Errorf("verify managed Visual Hive config: %w", readErr)
	}
	if err := verifyVisualHiveCoverageProfile([]byte(visualConfig), profileForCoverage(config.Coverage)); err != nil {
		return err
	}
	if config.VisualHiveConfigDigest != "" {
		digest := sha256.Sum256([]byte(normalizeManagedText(visualConfig)))
		if !strings.EqualFold(config.VisualHiveConfigDigest, hex.EncodeToString(digest[:])) {
			return fmt.Errorf("managed Visual Hive configuration does not match the exact repository-specific coverage plan; rerun setup and merge its reviewed PR")
		}
	}
	for _, forbidden := range []string{".github/workflows/visual-hive-issue-lifecycle.yml", ".github/workflows/visual-hive-trusted-publisher.yml"} {
		content, _, response, forbiddenErr := client.GoGitHub().Repositories.GetContents(ctx, owner, repo, forbidden, &gh.RepositoryContentGetOptions{Ref: ref})
		if forbiddenErr == nil || content != nil {
			return fmt.Errorf("standalone Visual Hive lifecycle writer %s must be removed; Hive is the sole lifecycle writer", forbidden)
		}
		if response == nil || response.StatusCode != http.StatusNotFound {
			return fmt.Errorf("verify absence of standalone lifecycle writer %s: %w", forbidden, forbiddenErr)
		}
	}
	return nil
}

func readTargetFile(ctx context.Context, client *hivegithub.Client, owner, repo, branch, relative string) (string, error) {
	content, _, _, err := client.GoGitHub().Repositories.GetContents(ctx, owner, repo, relative, &gh.RepositoryContentGetOptions{Ref: branch})
	if err != nil || content == nil {
		return "", fmt.Errorf("read %s from target branch: %w", relative, err)
	}
	value, err := content.GetContent()
	if err != nil {
		return "", fmt.Errorf("decode %s from target branch: %w", relative, err)
	}
	return value, nil
}

func normalizeManagedText(value string) string {
	return strings.TrimSpace(strings.ReplaceAll(value, "\r\n", "\n")) + "\n"
}

func verifyVisualHiveCoverageProfile(data []byte, expected string) error {
	var value struct {
		Project struct {
			SetupProfile string `yaml:"setupProfile"`
		} `yaml:"project"`
	}
	if err := yaml.Unmarshal(data, &value); err != nil {
		return fmt.Errorf("parse managed Visual Hive config: %w", err)
	}
	if value.Project.SetupProfile != expected {
		return fmt.Errorf("Visual Hive setup profile %q does not match coverage depth %q (expected %q); rerun setup and merge its exact reviewed PR", value.Project.SetupProfile, expected, expected)
	}
	return nil
}

func exactCommitPin(expected, actual string) bool {
	expected, actual = strings.ToLower(strings.TrimSpace(expected)), strings.ToLower(strings.TrimSpace(actual))
	return len(expected) == 40 && expected == actual
}

func stagedTreeMatchesRemoteBranch(ctx context.Context, checkout, branch string) (bool, string, error) {
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return false, "", nil
	}
	desiredTree, err := git(ctx, checkout, "write-tree")
	if err != nil {
		return false, "", fmt.Errorf("write desired setup tree: %w", err)
	}
	remoteRef := "refs/remotes/origin/" + branch
	remoteSHA, err := git(ctx, checkout, "rev-parse", "--verify", remoteRef)
	if err != nil {
		return false, "", nil
	}
	remoteTree, err := git(ctx, checkout, "rev-parse", "--verify", remoteRef+"^{tree}")
	if err != nil {
		return false, "", fmt.Errorf("read existing setup tree: %w", err)
	}
	return strings.TrimSpace(desiredTree) == strings.TrimSpace(remoteTree), strings.TrimSpace(remoteSHA), nil
}

func managedOperationBranch(operation, repositoryID string) string {
	operation = strings.ToLower(strings.TrimSpace(operation))
	repositoryID = regexp.MustCompile(`[^a-zA-Z0-9]+`).ReplaceAllString(strings.TrimSpace(repositoryID), "-")
	repositoryID = strings.Trim(repositoryID, "-")
	return fmt.Sprintf("hive/%s-%s", operation, repositoryID)
}

func managedCommitTrailers(repositoryID, operation string) string {
	return fmt.Sprintf("Hive-Repository-ID: %s\nHive-Operation: %s", strings.TrimSpace(repositoryID), strings.ToLower(strings.TrimSpace(operation)))
}

func pushManagedBranch(ctx context.Context, checkout, branch, repositoryID, operation, localSHA string) error {
	remoteRef := "refs/remotes/origin/" + branch
	remoteSHA, remoteErr := git(ctx, checkout, "rev-parse", "--verify", remoteRef)
	remoteSHA = strings.TrimSpace(remoteSHA)
	lease := "--force-with-lease=refs/heads/" + branch + ":"
	if remoteErr == nil {
		message, err := git(ctx, checkout, "show", "-s", "--format=%B", remoteRef)
		if err != nil {
			return fmt.Errorf("verify existing managed branch %s: %w", branch, err)
		}
		if !hasExactCommitTrailer(message, "Hive-Repository-ID", repositoryID) || !hasExactCommitTrailer(message, "Hive-Operation", operation) {
			return fmt.Errorf("refusing to overwrite remote branch %s because its latest commit lacks exact Hive ownership for repository ID %s and operation %s", branch, repositoryID, operation)
		}
		lease += remoteSHA
	}
	localSHA = strings.TrimSpace(localSHA)
	if localSHA == "" {
		return fmt.Errorf("exact local commit SHA is required before pushing managed branch %s", branch)
	}
	if _, err := git(ctx, checkout, "push", lease, "origin", localSHA+":refs/heads/"+branch); err != nil {
		return fmt.Errorf("push exact managed branch %s: %w", branch, err)
	}
	return nil
}

func hasExactCommitTrailer(message, key, value string) bool {
	want := strings.TrimSpace(key) + ": " + strings.TrimSpace(value)
	for _, line := range strings.Split(strings.ReplaceAll(message, "\r\n", "\n"), "\n") {
		if strings.EqualFold(strings.TrimSpace(line), want) {
			return true
		}
	}
	return false
}

func verifyManagedPullHead(operation string, pull hivegithub.RepairPullRequest, expectedSHA string) error {
	actual, expected := strings.TrimSpace(pull.HeadSHA), strings.TrimSpace(expectedSHA)
	if actual == "" || expected == "" || !strings.EqualFold(actual, expected) {
		return fmt.Errorf("%s PR head %q does not match exact pushed commit %q; refusing to record managed readiness", operation, actual, expected)
	}
	return nil
}

func validateSetupOptions(options SetupOptions) error {
	if !regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`).MatchString(options.Repository) {
		return fmt.Errorf("repository must be owner/name")
	}
	if options.Coverage != CoverageEssential && options.Coverage != CoverageStandard && options.Coverage != CoverageComprehensive && options.Coverage != CoverageCustom {
		return fmt.Errorf("coverage must be essential, standard, comprehensive, or custom")
	}
	if options.Automation != AutomationAdvisory && options.Automation != AutomationIssues && options.Automation != AutomationRepairPR && options.Automation != AutomationAutoMerge {
		return fmt.Errorf("automation must be advisory, issues, repair-pr, or auto-merge")
	}
	if options.StateDir == "" || options.Provider == "" || (options.Apply && options.ProviderCommand == "") {
		return fmt.Errorf("state directory and provider are required")
	}
	if options.MaxActiveIssues < 1 || options.MaxActiveIssues > 100 {
		return fmt.Errorf("maximum active issues must be from 1 through 100")
	}
	if options.MaxRepairAttempts < 1 || options.MaxRepairAttempts > 10 {
		return fmt.Errorf("maximum repair attempts must be from 1 through 10")
	}
	if options.AutoMergePathsExplicit && len(options.AllowedAutoMergePaths) == 0 {
		return fmt.Errorf("an explicit auto-merge path policy requires at least one safe repository-relative glob")
	}
	for _, pattern := range options.AllowedAutoMergePaths {
		cleaned := strings.TrimSpace(strings.ReplaceAll(pattern, "\\", "/"))
		if cleaned == "" || cleaned != pattern || strings.HasPrefix(cleaned, "/") || strings.HasPrefix(cleaned, "./") || strings.Split(cleaned, "/")[0] == ".." || strings.Contains(cleaned, "/../") {
			return fmt.Errorf("auto-merge path %q must be a normalized repository-relative glob", pattern)
		}
		if _, err := path.Match(cleaned, "hive-policy-validation-probe"); err != nil {
			return fmt.Errorf("auto-merge path %q is not a valid glob: %w", pattern, err)
		}
	}
	if options.AutoMergeRiskExplicit && len(options.AllowedAutoMergeRisk) == 0 {
		return fmt.Errorf("an explicit auto-merge risk policy requires at least one risk tier")
	}
	for _, risk := range options.AllowedAutoMergeRisk {
		if risk < automation.RiskAutomatic || risk > automation.RiskRestricted {
			return fmt.Errorf("auto-merge risk tier %d is invalid", risk)
		}
	}
	if !options.VisualHive {
		return fmt.Errorf("integrated Hive requires Visual Hive deterministic testing; disabling --visual-hive is not supported")
	}
	if options.VisualHive && (options.VisualHiveRepo == "" || !regexp.MustCompile(`^[a-f0-9]{40}$`).MatchString(options.VisualHiveRef)) {
		return fmt.Errorf("Visual Hive setup planning requires a repository and immutable 40-character commit SHA")
	}
	if options.Apply && options.VisualHive && options.VisualHiveCommand == "" {
		return fmt.Errorf("Visual Hive setup apply requires a command")
	}
	return nil
}

func buildSetupPlan(options SetupOptions, inspection RepositoryInspection) SetupPlan {
	warnings := []string{}
	if options.Automation == AutomationAutoMerge && !inspection.BranchProtection {
		warnings = append(warnings, "Default-branch protection was not detected. Hive will create its conservative exact-App-bound protection only after the setup PR is merged and one trusted default-branch Visual Hive run succeeds.")
	}
	if len(inspection.BaselineFiles) == 0 && options.VisualHive {
		warnings = append(warnings, "No reviewed visual baselines were detected; setup must prepare and review baselines before the first production scan.")
	}
	managedFiles := managedSetupFiles(options.VisualHive)
	return SetupPlan{
		SchemaVersion: PlanSchema, GeneratedAt: time.Now().UTC(), Repository: options.Repository,
		StateDir: options.StateDir,
		Coverage: options.Coverage, Automation: options.Automation, Provider: options.Provider,
		ACMMLevel: acmmForAutomation(options.Automation), VisualHive: options.VisualHive,
		VisualHiveRepository: options.VisualHiveRepo, VisualHiveRef: options.VisualHiveRef, Inspection: inspection,
		MaxActiveIssues:       options.MaxActiveIssues,
		MaxRepairAttempts:     options.MaxRepairAttempts,
		AllowedAutoMergePaths: append([]string(nil), options.AllowedAutoMergePaths...),
		AllowedAutoMergeRisk:  append([]automation.RiskTier(nil), options.AllowedAutoMergeRisk...),
		TestingLayers:         layersForCoverage(options.Coverage),
		FilesToManage:         managedFiles,
		RequiredActions:       setupRequiredActions(options.Automation),
		Warnings:              warnings, ReadOnly: true,
	}
}

func setupRequiredActions(automation Automation) []string {
	actions := []string{"Review and merge the exact setup PR"}
	if automation == AutomationAutoMerge {
		actions = append(actions, "Complete the setup PR and trusted setup run to activate exact-App-bound protection; a requested scheduler starts only after all non-scheduler doctor checks are green, or run hive start explicitly later")
	}
	return append(actions, "Run hive doctor and confirm production_ready=true")
}

func managedSetupFiles(visualHive bool) []string {
	files := []string{".hive/integrated.json", ".github/workflows/hive-visual-hive.yml", "docs/hive-quickstart.md"}
	if visualHive {
		files = append(files, "docs/visual-hive.md", "visual-hive.config.yaml", ".github/workflows/visual-hive-pr.yml", ".github/workflows/visual-hive-issue-lifecycle.yml", ".github/workflows/visual-hive-trusted-publisher.yml")
	}
	return files
}

func layersForCoverage(coverage Coverage) []string {
	essential := []string{"unit", "integration", "e2e", "visual", "accessibility"}
	if coverage == CoverageEssential {
		return essential
	}
	standard := append(essential, "api", "mutation", "security")
	if coverage == CoverageStandard {
		return standard
	}
	return append(standard, "performance", "dependency", "cross-browser", "responsive", "theme-state")
}

func acmmForAutomation(value Automation) int {
	switch value {
	case AutomationAdvisory:
		return 2
	case AutomationIssues:
		return 4
	case AutomationRepairPR:
		return 5
	case AutomationAutoMerge:
		return 6
	default:
		return 1
	}
}

func ensureCheckout(ctx context.Context, repository, checkout string) (string, error) {
	return ensureCheckoutWithLegacy(ctx, repository, checkout, nil)
}

func ensureCheckoutWithLegacy(ctx context.Context, repository, checkout string, legacy *legacyManagedCheckoutProof) (string, error) {
	exists, err := validateManagedCheckoutBeforeGitWithLegacy(checkout, repository, legacy)
	if err != nil {
		return "", err
	}
	if !exists {
		if err := os.MkdirAll(filepath.Dir(checkout), 0o700); err != nil {
			return "", err
		}
		if _, err := git(ctx, filepath.Dir(checkout), "clone", "--origin", "origin", setupRepositoryCloneURL(repository), checkout); err != nil {
			return "", fmt.Errorf("clone target repository: %w", err)
		}
		if err := writeManagedCheckoutOwner(checkout, repository); err != nil {
			return "", fmt.Errorf("bind managed checkout ownership: %w", err)
		}
	}
	if _, err := validateManagedCheckoutBeforeGitWithLegacy(checkout, repository, legacy); err != nil {
		return "", fmt.Errorf("validate exact managed checkout ownership before Git synchronization: %w", err)
	}
	if _, err := git(ctx, checkout, "fetch", "--prune", "origin"); err != nil {
		return "", err
	}
	ref, err := git(ctx, checkout, "symbolic-ref", "--short", "refs/remotes/origin/HEAD")
	if err == nil {
		if branch := strings.TrimPrefix(strings.TrimSpace(ref), "origin/"); branch != "" {
			return checkoutDefaultBranch(ctx, checkout, branch)
		}
	}
	for _, branch := range []string{"main", "master"} {
		if _, err := git(ctx, checkout, "rev-parse", "--verify", "origin/"+branch); err == nil {
			return checkoutDefaultBranch(ctx, checkout, branch)
		}
	}
	return "", fmt.Errorf("could not determine default branch")
}

func checkoutDefaultBranch(ctx context.Context, checkout, branch string) (string, error) {
	// The checkout is Hive-owned. Always inspect the current remote default
	// rather than whatever setup/repair branch a previous interrupted run left
	// checked out.
	if _, err := git(ctx, checkout, "reset", "--hard", "origin/"+branch); err != nil {
		return "", fmt.Errorf("reset exact managed checkout to origin/%s: %w", branch, err)
	}
	if _, err := git(ctx, checkout, "clean", "-ffdqx"); err != nil {
		return "", fmt.Errorf("clean exact managed checkout before static setup: %w", err)
	}
	if _, err := git(ctx, checkout, "switch", "--detach", "origin/"+branch); err != nil {
		return "", fmt.Errorf("switch managed checkout to origin/%s: %w", branch, err)
	}
	return branch, nil
}

func runVisualHiveSetup(ctx context.Context, options SetupOptions, checkout string) error {
	setupEnvironment, err := visualHiveSetupEnvironment(options)
	if err != nil {
		return err
	}
	args := append([]string(nil), options.VisualHiveArgs...)
	args = append(args, "recommend", "--repo", checkout, "--profile", profileForCoverage(options.Coverage), "--format", "json")
	configExisted := exists(filepath.Join(checkout, "visual-hive.config.yaml"))
	if !configExisted {
		args = append(args, "--write-config")
	}
	if !exists(filepath.Join(checkout, "docs", "visual-hive.md")) {
		args = append(args, "--write-docs")
	}
	command := exec.CommandContext(ctx, options.VisualHiveCommand, args...)
	command.Dir = checkout
	command.Env = setupEnvironment
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	if err := command.Run(); err != nil {
		return fmt.Errorf("generate repository-specific Visual Hive setup: %w: %s", err, safeOutput(output.String()))
	}
	if configExisted {
		if err := mergeVisualHiveRecommendation(checkout, profileForCoverage(options.Coverage)); err != nil {
			return fmt.Errorf("migrate existing Visual Hive coverage profile: %w", err)
		}
	} else if err := markVisualHiveConfigOrigin(checkout, "generated", profileForCoverage(options.Coverage)); err != nil {
		return fmt.Errorf("mark generated Visual Hive coverage ownership: %w", err)
	}
	doctorArgs := append([]string(nil), options.VisualHiveArgs...)
	doctorArgs = append(doctorArgs, "doctor", "--config", filepath.Join(checkout, "visual-hive.config.yaml"))
	doctor := exec.CommandContext(ctx, options.VisualHiveCommand, doctorArgs...)
	doctor.Dir, doctor.Env = checkout, setupEnvironment
	output.Reset()
	doctor.Stdout, doctor.Stderr = &output, &output
	if err := doctor.Run(); err != nil {
		return fmt.Errorf("validate generated Visual Hive configuration: %w: %s", err, safeOutput(output.String()))
	}
	// Local setup is a static-only trust zone. Repository install/build/serve,
	// browser installation, screenshot capture, and every target-authored script
	// execute only in the post-merge hosted baseline/production workflows.
	return nil
}

func visualHiveSetupEnvironment(options SetupOptions) ([]string, error) {
	runtimeDir, err := filepath.Abs(filepath.Dir(strings.TrimSpace(options.VisualHiveCommand)))
	if err != nil || strings.TrimSpace(options.VisualHiveCommand) == "" {
		return nil, fmt.Errorf("Visual Hive setup requires an exact runtime command")
	}
	stateDir := strings.TrimSpace(options.StateDir)
	if stateDir == "" {
		return nil, fmt.Errorf("Visual Hive setup requires a persistent state directory for runtime caches")
	}
	corepackHome, err := filepath.Abs(filepath.Join(stateDir, "runtime", "corepack"))
	if err != nil {
		return nil, fmt.Errorf("resolve corepack runtime cache: %w", err)
	}
	if err := os.MkdirAll(corepackHome, 0o700); err != nil {
		return nil, fmt.Errorf("create corepack runtime cache: %w", err)
	}
	environment := safeEnvironment()
	environment = replaceEnvironmentValue(environment, "PATH", runtimeDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	environment = replaceEnvironmentValue(environment, "COREPACK_HOME", corepackHome)
	return environment, nil
}

func replaceEnvironmentValue(environment []string, name, value string) []string {
	result := make([]string, 0, len(environment)+1)
	for _, pair := range environment {
		key, _, _ := strings.Cut(pair, "=")
		if !strings.EqualFold(key, name) {
			result = append(result, pair)
		}
	}
	return append(result, name+"="+value)
}

func visualHiveConfigRequiresBaselines(checkout string) (bool, error) {
	required, _, err := visualHiveScreenshotContractInventory(checkout)
	return required, err
}

func visualHiveScreenshotContractInventory(checkout string) (bool, string, error) {
	data, err := readSetupCheckoutFile(checkout, "visual-hive.config.yaml")
	if err != nil {
		return false, "", fmt.Errorf("read generated Visual Hive config before baseline bootstrap: %w", err)
	}
	var config struct {
		Contracts []struct {
			ID          any   `yaml:"id"`
			Name        any   `yaml:"name"`
			Path        any   `yaml:"path"`
			Screenshots []any `yaml:"screenshots"`
		} `yaml:"contracts"`
	}
	if err := yaml.Unmarshal(data, &config); err != nil {
		return false, "", fmt.Errorf("parse generated Visual Hive config before baseline bootstrap: %w", err)
	}
	type contractRecord struct {
		Index       int   `json:"index"`
		ID          any   `json:"id,omitempty"`
		Name        any   `json:"name,omitempty"`
		Path        any   `json:"path,omitempty"`
		Screenshots []any `json:"screenshots"`
	}
	records := []contractRecord{}
	for index, contract := range config.Contracts {
		if len(contract.Screenshots) > 0 {
			records = append(records, contractRecord{Index: index, ID: contract.ID, Name: contract.Name, Path: contract.Path, Screenshots: contract.Screenshots})
		}
	}
	if len(records) == 0 {
		return false, "", nil
	}
	canonical, err := json.Marshal(records)
	if err != nil {
		return false, "", fmt.Errorf("bind screenshot contract inventory: %w", err)
	}
	digest := sha256.Sum256(canonical)
	return true, hex.EncodeToString(digest[:]), nil
}

// mergeVisualHiveRecommendation reconciles every coverage-owned section to the
// deterministic recommendation. This makes upgrades and downgrades exact; the
// resulting setup PR remains the review boundary for repository-specific edits.
func mergeVisualHiveRecommendation(checkout, expectedProfile string) error {
	existingData, err := readSetupCheckoutFile(checkout, "visual-hive.config.yaml")
	if err != nil {
		return err
	}
	var current map[string]any
	if err := yaml.Unmarshal(existingData, &current); err != nil {
		return fmt.Errorf("parse existing config: %w", err)
	}
	project := ensureStringMap(current, "project")
	origin := ""
	if rawOrigin, exists := project["hiveConfigOrigin"]; exists && rawOrigin != nil {
		origin = strings.ToLower(strings.TrimSpace(fmt.Sprint(rawOrigin)))
	}
	// Legacy repository-owned plans predate Hive's origin marker. When their
	// effective coverage profile is unchanged, preserve the YAML byte-for-byte:
	// an immutable runtime/workflow upgrade must not create a cosmetic config
	// conflict with an in-flight repair. A later explicit profile change remains
	// a reviewed repository diff and records the ownership marker then.
	if origin == "" && strings.EqualFold(strings.TrimSpace(fmt.Sprint(project["setupProfile"])), strings.TrimSpace(expectedProfile)) {
		return nil
	}
	var report struct {
		RecommendedConfig map[string]any `json:"recommendedConfig"`
	}
	recommendationData, err := readSetupCheckoutFile(checkout, ".visual-hive/recommendations.json")
	if err != nil {
		return fmt.Errorf("read deterministic recommendation: %w", err)
	}
	if err := json.Unmarshal(recommendationData, &report); err != nil || len(report.RecommendedConfig) == 0 {
		return fmt.Errorf("parse deterministic recommendation: %w", err)
	}
	project["setupProfile"] = expectedProfile
	project["hiveManagedBy"] = "hive-integrated"
	if origin == "" || origin == "existing" {
		// A pre-existing Visual Hive plan belongs to the repository. Preserve its
		// domain-specific targets/contracts and record that generic profile
		// recommendations must not replace them on future coverage changes.
		project["hiveConfigOrigin"] = "existing"
		updated, err := yaml.Marshal(current)
		if err != nil {
			return err
		}
		if bytes.Equal(existingData, updated) {
			return nil
		}
		return writeSetupCheckoutFile(checkout, "visual-hive.config.yaml", updated)
	}
	project["hiveConfigOrigin"] = "generated"
	for _, key := range []string{"targets", "contracts", "viewports", "visual", "selection", "mutation", "flows"} {
		if value, exists := report.RecommendedConfig[key]; exists {
			current[key] = value
		} else {
			delete(current, key)
		}
	}
	if recommendedCost, exists := report.RecommendedConfig["costPolicy"]; exists {
		current["costPolicy"] = recommendedCost
	}
	updated, err := yaml.Marshal(current)
	if err != nil {
		return err
	}
	if bytes.Equal(existingData, updated) {
		return nil
	}
	return writeSetupCheckoutFile(checkout, "visual-hive.config.yaml", updated)
}

func markVisualHiveConfigOrigin(checkout, origin, profile string) error {
	data, err := readSetupCheckoutFile(checkout, "visual-hive.config.yaml")
	if err != nil {
		return err
	}
	var current map[string]any
	if err := yaml.Unmarshal(data, &current); err != nil {
		return err
	}
	project := ensureStringMap(current, "project")
	project["setupProfile"] = profile
	project["hiveManagedBy"] = "hive-integrated"
	project["hiveConfigOrigin"] = origin
	updated, err := yaml.Marshal(current)
	if err != nil {
		return err
	}
	return writeSetupCheckoutFile(checkout, "visual-hive.config.yaml", updated)
}

func ensureStringMap(parent map[string]any, key string) map[string]any {
	if value := anyStringMap(parent[key]); value != nil {
		parent[key] = value
		return value
	}
	value := map[string]any{}
	parent[key] = value
	return value
}

func anyStringMap(value any) map[string]any {
	if result, ok := value.(map[string]any); ok {
		return result
	}
	return nil
}

func anySlice(value any) []any {
	if result, ok := value.([]any); ok {
		return result
	}
	return nil
}

func mergeMissingMap(current, recommended map[string]any) {
	for key, value := range recommended {
		if _, exists := current[key]; !exists {
			current[key] = value
		}
	}
}

func mergeObjectListByID(current, recommended []any) []any {
	return mergeObjectListByKey(current, recommended, "id")
}

func mergeObjectListByKey(current, recommended []any, key string) []any {
	seen := map[string]bool{}
	for _, item := range current {
		if value := fmt.Sprint(anyStringMap(item)[key]); value != "" {
			seen[value] = true
		}
	}
	for _, item := range recommended {
		value := fmt.Sprint(anyStringMap(item)[key])
		if value != "" && !seen[value] {
			current = append(current, item)
			seen[value] = true
		}
	}
	return current
}

func mergeScalarList(current, recommended []any) []any {
	seen := map[string]bool{}
	for _, item := range current {
		seen[fmt.Sprint(item)] = true
	}
	for _, item := range recommended {
		value := fmt.Sprint(item)
		if !seen[value] {
			current = append(current, item)
			seen[value] = true
		}
	}
	return current
}

func profileForCoverage(coverage Coverage) string {
	if coverage == CoverageComprehensive || coverage == CoverageCustom {
		return "complex-app"
	}
	return "free-local"
}

func visualHiveConfigDigest(checkout string) (string, error) {
	data, err := readSetupCheckoutFile(checkout, "visual-hive.config.yaml")
	if err != nil {
		return "", fmt.Errorf("read repository-specific Visual Hive config for digest: %w", err)
	}
	digest := sha256.Sum256([]byte(normalizeManagedText(string(data))))
	return hex.EncodeToString(digest[:]), nil
}

func existingVisualBaselines(checkout string) []string {
	committed, ok := committedFileSet(checkout)
	if !ok {
		return nil
	}
	files := []string{}
	for path := range committed {
		clean := filepath.ToSlash(path)
		if strings.HasPrefix(strings.ToLower(clean), ".visual-hive/snapshots/") && strings.EqualFold(filepath.Ext(clean), ".png") {
			files = append(files, clean)
		}
	}
	return sortedUnique(files)
}

func setupBaselineInventoryAtCommit(ctx context.Context, checkout, commitSHA string) ([]SetupBaselineCandidate, string, error) {
	commitSHA = strings.ToLower(strings.TrimSpace(commitSHA))
	if !immutableCommit.MatchString(commitSHA) {
		return nil, "", fmt.Errorf("exact setup commit is required for baseline inventory")
	}
	command := exec.CommandContext(ctx, "git", "-C", checkout, "ls-tree", "-r", "-z", commitSHA, "--", ".visual-hive/snapshots")
	command.Env = safeEnvironment()
	output, err := command.Output()
	if err != nil {
		return nil, "", fmt.Errorf("list pre-setup baseline tree: %w", err)
	}
	candidates := []SetupBaselineCandidate{}
	for _, record := range strings.Split(string(output), "\x00") {
		if record == "" {
			continue
		}
		metadata, relative, ok := strings.Cut(record, "\t")
		fields := strings.Fields(metadata)
		relative = filepath.ToSlash(relative)
		if !ok || len(fields) != 3 || (fields[0] != "100644" && fields[0] != "100755") || fields[1] != "blob" || !immutableCommit.MatchString(strings.ToLower(fields[2])) ||
			!strings.HasPrefix(relative, ".visual-hive/snapshots/") || !strings.HasSuffix(strings.ToLower(relative), ".png") {
			return nil, "", fmt.Errorf("pre-setup baseline entry %q is not a canonical regular PNG blob", relative)
		}
		content, readErr := readSetupBaselineBlob(ctx, checkout, strings.ToLower(fields[2]))
		if readErr != nil || len(content) <= 8 || !bytes.Equal(content[:8], []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}) {
			return nil, "", fmt.Errorf("read canonical pre-setup PNG %s: %w", relative, readErr)
		}
		digest := sha256.Sum256(content)
		candidates = append(candidates, SetupBaselineCandidate{Path: relative, SHA256: hex.EncodeToString(digest[:]), Bytes: int64(len(content))})
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].Path < candidates[j].Path })
	digest, err := setupBaselineCandidateDigest(candidates)
	return candidates, digest, err
}

func readSetupBaselineBlob(ctx context.Context, checkout, objectSHA string) ([]byte, error) {
	command := exec.CommandContext(ctx, "git", "-C", checkout, "cat-file", "blob", objectSHA)
	command.Env = safeEnvironment()
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err != nil {
		return nil, fmt.Errorf("git cat-file blob %s: %w: %s", objectSHA, err, safeOutput(stderr.String()))
	}
	return stdout.Bytes(), nil
}

func writeManagedFiles(root string, config Config, inspection RepositoryInspection) error {
	repositoryConfig := map[string]any{
		"schema_version": ConfigSchema, "repository": config.Repository, "repository_id": config.RepositoryID,
		"default_branch": config.DefaultBranch, "coverage": config.Coverage, "automation": config.Automation,
		"provider": config.Provider, "acmm_level": config.ACMMLevel, "max_active_issues": config.MaxActiveIssues, "max_repair_attempts": config.MaxRepairAttempts, "visual_hive": config.VisualHive,
		"visual_hive_repository": config.VisualHiveRepo, "visual_hive_ref": config.VisualHiveRef,
		"visual_hive_config_digest": config.VisualHiveConfigDigest,
		"test_commands":             config.TestCommands, "allowed_repair_paths": config.AllowedRepairPaths,
		"allowed_auto_merge_paths": config.AllowedAutoMergePaths, "allowed_auto_merge_risk": config.AllowedAutoMergeRisk,
		"setup_authorization_actor_id": config.SetupAuthorizationActorID,
	}
	if config.SetupAuthorizationPreviousActorID > 0 {
		repositoryConfig["setup_authorization_previous_actor_id"] = config.SetupAuthorizationPreviousActorID
	}
	configData, err := json.MarshalIndent(repositoryConfig, "", "  ")
	if err != nil {
		return err
	}
	files := map[string]string{
		".hive/integrated.json":                  string(configData) + "\n",
		"docs/hive-quickstart.md":                quickstart(config, inspection),
		".github/workflows/hive-visual-hive.yml": workflow(config),
	}
	if config.VisualHive {
		files[".github/workflows/visual-hive-pr.yml"] = pullRequestWorkflow(config)
	}
	for relative, content := range files {
		if err := writeSetupCheckoutFile(root, relative, []byte(content)); err != nil {
			return err
		}
	}
	if config.VisualHive {
		for _, relative := range []string{".github/workflows/visual-hive-issue-lifecycle.yml", ".github/workflows/visual-hive-trusted-publisher.yml"} {
			if err := removeSetupCheckoutFile(root, relative); err != nil {
				return err
			}
		}
	}
	return nil
}

func testCommandsForCoverage(inspection RepositoryInspection, coverage Coverage) [][]string {
	maximumRank := 1
	if coverage == CoverageStandard {
		maximumRank = 2
	} else if coverage == CoverageComprehensive || coverage == CoverageCustom {
		maximumRank = 3
	}
	commands := make([][]string, 0, len(inspection.TestCommands))
	for _, command := range inspection.TestCommands {
		rank := commandCoverageRank(command)
		if rank > 0 && rank <= maximumRank {
			commands = append(commands, append([]string(nil), command...))
		}
	}
	commands = preferRepositoryComprehensiveSuite(inspection, commands, coverage)
	if maximumRank < 3 || !containsValue(inspection.Languages, "TypeScript/JavaScript") || hasRepositoryUnitCommand(inspection, commands) {
		return sortTestCommands(commands)
	}
	commands = append(commands, []string{"node", "--test"})
	return sortTestCommands(commands)
}

func commandCoverageRank(command []string) int {
	if len(command) == 0 {
		return 0
	}
	value := strings.ToLower(strings.Join(command, " "))
	name := commandScriptName(command)
	if strings.Contains(value, "python -m pytest") || strings.HasPrefix(value, "go test") || value == "node --test" {
		return 1
	}
	for _, unsafe := range []string{"watch", "update", "--ui", "--open", "headed", " dev", " serve", " preview"} {
		if strings.Contains(name, unsafe) || strings.Contains(value, unsafe) {
			return 0
		}
	}
	for _, deep := range []string{"performance", "perf", "load", "cross-browser", "responsive", "theme", "suite", "proof", "verify", "validation"} {
		if strings.Contains(name, deep) {
			return 3
		}
	}
	for _, standard := range []string{"integration", "e2e", "api", "visual", "a11y", "accessibility", "mutation", "mutate", "security"} {
		if strings.Contains(name, standard) {
			return 2
		}
	}
	if name == "test" || name == "build" || strings.Contains(name, "unit") || strings.Contains(name, "test:ci") || strings.Contains(name, "test:") || strings.Contains(name, "lint") || strings.Contains(name, "typecheck") || strings.Contains(name, "check") || strings.Contains(name, "build") {
		return 1
	}
	return 0
}

func hasRepositoryUnitCommand(inspection RepositoryInspection, commands [][]string) bool {
	for _, command := range commands {
		packagePath, _, script, ok := packageScriptCommandParts(command)
		if !ok {
			if invokesJavaScriptUnitRunner(command) {
				return true
			}
			continue
		}
		scripts := inspection.packageScripts[packagePath]
		terminals, safe := terminalPackageScripts(scripts, script)
		if !safe {
			continue
		}
		for invocation := range terminals {
			if invocation.Forwarded == "" && isJavaScriptUnitRunner(scripts[invocation.Name]) {
				return true
			}
		}
	}
	return false
}

// preferRepositoryComprehensiveSuite preserves the repository author's tested
// ordering when one safe aggregate provably reaches every selected leaf test.
// Orchestration wrappers (for example test:ci:lite) are not run beside that
// aggregate, avoiding repeated browser servers and overlapping smoke tests.
func preferRepositoryComprehensiveSuite(inspection RepositoryInspection, commands [][]string, coverage Coverage) [][]string {
	if coverage != CoverageComprehensive && coverage != CoverageCustom {
		return commands
	}
	replacements := map[string][]string{}
	for packagePath, scripts := range inspection.packageScripts {
		selected := map[string]bool{}
		for _, command := range commands {
			path, _, name, ok := packageScriptCommandParts(command)
			if ok && path == packagePath {
				selected[name] = true
			}
		}
		if len(selected) == 0 {
			continue
		}
		aggregate := comprehensiveAggregateScript(scripts, selected)
		if aggregate == "" {
			continue
		}
		runner := inspection.packageRunners[packagePath]
		if runner == "" {
			runner = "npm"
		}
		replacements[packagePath] = packageScriptCommand(packagePath, runner, aggregate)
	}
	if len(replacements) == 0 {
		return commands
	}
	result := make([][]string, 0, len(commands)+len(replacements))
	for _, command := range commands {
		packagePath, _, _, ok := packageScriptCommandParts(command)
		if ok && replacements[packagePath] != nil {
			continue
		}
		result = append(result, command)
	}
	paths := make([]string, 0, len(replacements))
	for packagePath := range replacements {
		paths = append(paths, packagePath)
	}
	sort.Strings(paths)
	for _, packagePath := range paths {
		result = append(result, replacements[packagePath])
	}
	return uniqueTestCommands(result)
}

func comprehensiveAggregateScript(scripts map[string]string, selected map[string]bool) string {
	for _, candidate := range []string{"test:all", "test:ci:heavy", "test:suite", "vh:suite", "test:ci"} {
		body, exists := scripts[candidate]
		if !exists || !safeAutomationScript(candidate, body) {
			continue
		}
		dependencies, strict := strictPackageRunDependencies(body)
		if !strict || len(dependencies) < 2 {
			continue
		}
		candidateTerminals, safe := terminalPackageScripts(scripts, candidate)
		if !safe || len(candidateTerminals) == 0 {
			continue
		}
		selectedTerminals := map[packageScriptInvocation]bool{}
		for name := range selected {
			if name == candidate {
				continue
			}
			terminals, selectedSafe := terminalPackageScripts(scripts, name)
			if !selectedSafe {
				selectedTerminals = nil
				break
			}
			for terminal := range terminals {
				selectedTerminals[terminal] = true
			}
		}
		for terminal := range selectedTerminals {
			if terminal.Forwarded != "" && selectedTerminals[packageScriptInvocation{Name: terminal.Name}] {
				delete(selectedTerminals, terminal)
			}
		}
		if len(selectedTerminals) == 0 {
			continue
		}
		coversSelectedTerminals := true
		for terminal := range selectedTerminals {
			if !candidateTerminals[terminal] && !equivalentVitestCoverageLeaf(scripts, terminal, candidateTerminals) {
				coversSelectedTerminals = false
				break
			}
		}
		if coversSelectedTerminals {
			return candidate
		}
	}
	return ""
}

func terminalPackageScripts(scripts map[string]string, root string) (map[packageScriptInvocation]bool, bool) {
	terminals := map[packageScriptInvocation]bool{}
	visiting := map[packageScriptInvocation]bool{}
	complete := map[packageScriptInvocation]bool{}
	var visit func(packageScriptInvocation) bool
	visit = func(invocation packageScriptInvocation) bool {
		if visiting[invocation] {
			return false
		}
		if complete[invocation] {
			return true
		}
		body, exists := scripts[invocation.Name]
		if !exists || !safeAutomationScript(invocation.Name, body) {
			return false
		}
		visiting[invocation] = true
		dependencies, strict := strictPackageRunDependencies(body)
		if !strict || invocation.Forwarded != "" {
			terminals[invocation] = true
		} else {
			for _, dependency := range dependencies {
				if !visit(dependency) {
					return false
				}
			}
		}
		delete(visiting, invocation)
		complete[invocation] = true
		return true
	}
	if !visit(packageScriptInvocation{Name: root}) {
		return nil, false
	}
	return terminals, true
}

func strictPackageRunDependencies(body string) ([]packageScriptInvocation, bool) {
	parts := strings.Split(strings.TrimSpace(body), "&&")
	if len(parts) == 0 {
		return nil, false
	}
	dependencies := make([]packageScriptInvocation, 0, len(parts))
	for _, part := range parts {
		match := strictPackageRunPattern.FindStringSubmatch(strings.TrimSpace(part))
		if len(match) < 4 {
			return nil, false
		}
		forwarded := strings.Fields(strings.TrimSpace(match[4]))
		if len(forwarded) > 0 && forwarded[0] == "--" {
			forwarded = forwarded[1:]
		}
		dependencies = append(dependencies, packageScriptInvocation{Name: match[3], Forwarded: strings.Join(forwarded, "\x00")})
	}
	return dependencies, true
}

func equivalentVitestCoverageLeaf(scripts map[string]string, selected packageScriptInvocation, candidateTerminals map[packageScriptInvocation]bool) bool {
	if selected.Forwarded != "" || !strings.Contains(strings.ToLower(selected.Name), "unit") {
		return false
	}
	selectedTokens, selectedCoverage, selectedOK := normalizedVitestCommand(scripts[selected.Name])
	if !selectedOK || selectedCoverage {
		return false
	}
	for candidate := range candidateTerminals {
		if candidate.Forwarded != "" || !strings.Contains(strings.ToLower(candidate.Name), "coverage") {
			continue
		}
		candidateTokens, candidateCoverage, candidateOK := normalizedVitestCommand(scripts[candidate.Name])
		if candidateOK && candidateCoverage && strings.Join(selectedTokens, "\x00") == strings.Join(candidateTokens, "\x00") {
			return true
		}
	}
	return false
}

func normalizedVitestCommand(body string) ([]string, bool, bool) {
	parts := strings.Fields(strings.TrimSpace(body))
	if len(parts) == 0 {
		return nil, false, false
	}
	for _, part := range parts {
		if !safeShellTokenPattern.MatchString(part) {
			return nil, false, false
		}
	}
	executable := strings.ToLower(filepath.Base(filepath.ToSlash(parts[0])))
	if executable != "vitest" && executable != "vitest.cmd" {
		return nil, false, false
	}
	normalized := make([]string, 0, len(parts))
	hadCoverage := false
	for _, part := range parts {
		lower := strings.ToLower(part)
		if lower == "--coverage" || strings.HasPrefix(lower, "--coverage=") || strings.HasPrefix(lower, "--coverage.") {
			hadCoverage = true
			continue
		}
		normalized = append(normalized, part)
	}
	return normalized, hadCoverage, true
}

func isJavaScriptUnitRunner(body string) bool {
	for _, segment := range strings.Split(strings.TrimSpace(body), "&&") {
		parts := strings.Fields(strings.TrimSpace(segment))
		if len(parts) == 0 {
			continue
		}
		valid := true
		for _, part := range parts {
			if !safeShellTokenPattern.MatchString(part) {
				valid = false
				break
			}
		}
		if valid && invokesJavaScriptUnitRunner(parts) {
			return true
		}
	}
	return false
}

func invokesJavaScriptUnitRunner(parts []string) bool {
	for len(parts) > 0 && environmentTokenPattern.MatchString(parts[0]) {
		parts = parts[1:]
	}
	if len(parts) == 0 {
		return false
	}
	executable := strings.ToLower(filepath.Base(filepath.ToSlash(parts[0])))
	parts = parts[1:]
	switch executable {
	case "vitest", "vitest.cmd", "jest", "jest.cmd", "mocha", "mocha.cmd", "ava", "ava.cmd", "tap", "tap.cmd", "uvu", "uvu.cmd":
		return !hasNoExecutionMode(parts)
	case "node", "node.exe":
		if hasNoExecutionMode(parts) {
			return false
		}
		for _, argument := range parts {
			if argument == "--test" {
				return true
			}
		}
	case "react-scripts", "react-scripts.cmd":
		return len(parts) > 0 && parts[0] == "test" && !hasNoExecutionMode(parts[1:])
	case "npx", "npx.cmd":
		if len(parts) > 0 && parts[0] == "--no-install" {
			parts = parts[1:]
		}
		return isKnownJavaScriptUnitInvocation(parts)
	case "npm", "npm.cmd":
		if len(parts) == 0 || parts[0] != "exec" {
			return false
		}
		parts = parts[1:]
		if len(parts) > 0 && parts[0] == "--" {
			parts = parts[1:]
		}
		return isKnownJavaScriptUnitInvocation(parts)
	case "pnpm", "pnpm.cmd", "yarn", "yarn.cmd":
		if len(parts) > 0 && parts[0] == "exec" {
			parts = parts[1:]
		}
		return isKnownJavaScriptUnitInvocation(parts)
	case "cross-env", "cross-env.cmd":
		for len(parts) > 0 && environmentTokenPattern.MatchString(parts[0]) {
			parts = parts[1:]
		}
		return len(parts) > 0 && invokesJavaScriptUnitRunner(parts)
	}
	return false
}

func isKnownJavaScriptUnitInvocation(parts []string) bool {
	if len(parts) == 0 {
		return false
	}
	name := strings.ToLower(filepath.Base(filepath.ToSlash(parts[0])))
	switch name {
	case "vitest", "vitest.cmd", "jest", "jest.cmd", "mocha", "mocha.cmd", "ava", "ava.cmd", "tap", "tap.cmd", "uvu", "uvu.cmd":
		return !hasNoExecutionMode(parts[1:])
	default:
		return false
	}
}

func hasNoExecutionMode(arguments []string) bool {
	for index, argument := range arguments {
		value := strings.ToLower(argument)
		for _, denied := range []string{
			"--help", "-h", "help", "--version", "-v", "version", "init", "--init",
			"list", "--list", "--listtests", "--list-tests", "--showconfig", "--show-config",
			"--clearcache", "--clear-cache", "--collect-only", "--collectonly", "--dry-run", "--dryrun",
		} {
			if value == denied || strings.HasPrefix(value, denied+"=") {
				return true
			}
		}
		if (value == "config" || value == "--config") && index == len(arguments)-1 {
			return true
		}
	}
	return false
}

func packageScriptCommandParts(command []string) (packagePath, runner, name string, ok bool) {
	if len(command) == 3 && command[1] == "run" && (command[0] == "npm" || command[0] == "pnpm" || command[0] == "yarn") {
		return ".", command[0], command[2], true
	}
	if len(command) != 5 || command[3] != "run" {
		return "", "", "", false
	}
	switch {
	case command[0] == "npm" && command[1] == "--prefix":
		return filepath.ToSlash(command[2]), "npm", command[4], true
	case command[0] == "pnpm" && command[1] == "--dir":
		return filepath.ToSlash(command[2]), "pnpm", command[4], true
	case command[0] == "yarn" && command[1] == "--cwd":
		return filepath.ToSlash(command[2]), "yarn", command[4], true
	default:
		return "", "", "", false
	}
}

func sortTestCommands(commands [][]string) [][]string {
	commands = cloneCommands(commands)
	sort.SliceStable(commands, func(i, j int) bool {
		left, right := testCommandPriority(commands[i]), testCommandPriority(commands[j])
		if left != right {
			return left < right
		}
		return strings.Join(commands[i], "\x00") < strings.Join(commands[j], "\x00")
	})
	return commands
}

func targetDependencyInstallShell() string {
	return targetPackageInstallShell() + `
          if [ -f pyproject.toml ]; then
            python -m pip install -e .
          elif [ -f requirements.txt ]; then
            python -m pip install -r requirements.txt
          fi
          tooling_playwright="$RUNNER_TEMP/visual-hive-tooling/node_modules/@playwright/test/cli.js"
          node "$tooling_playwright" install --with-deps chromium
          while IFS= read -r -d '' playwright_cli; do
            node "$playwright_cli" install chromium
          done < <(find . -path '*/node_modules/@playwright/test/cli.js' -not -path './.git/*' -print0 | sort -z)`
}

func targetPackageInstallShell() string {
	return `          corepack_enabled=0
          enable_corepack() {
            if [ "$corepack_enabled" -eq 0 ]; then
              corepack enable
              corepack_enabled=1
            fi
          }
          while IFS= read -r -d '' lockfile; do
            package_dir="$(dirname "$lockfile")"
            lock_count=0
            [ -f "$package_dir/package-lock.json" ] && lock_count=$((lock_count + 1))
            [ -f "$package_dir/pnpm-lock.yaml" ] && lock_count=$((lock_count + 1))
            [ -f "$package_dir/yarn.lock" ] && lock_count=$((lock_count + 1))
            if [ "$lock_count" -ne 1 ]; then
              echo "Package scope $package_dir must contain exactly one supported lockfile" >&2
              exit 1
            fi
          done < <(find . \( -name package-lock.json -o -name pnpm-lock.yaml -o -name yarn.lock \) -not -path '*/node_modules/*' -not -path './.git/*' -print0 | sort -z)
          while IFS= read -r -d '' lockfile; do
            package_dir="$(dirname "$lockfile")"
            test -f "$package_dir/package.json"
            test -f "$package_dir/package-lock.json"
            (cd "$package_dir" && npm ci)
          done < <(find . -name package-lock.json -not -path '*/node_modules/*' -not -path './.git/*' -print0 | sort -z)
          while IFS= read -r -d '' lockfile; do
            enable_corepack
            package_dir="$(dirname "$lockfile")"
            (cd "$package_dir" && pnpm install --frozen-lockfile)
          done < <(find . -name pnpm-lock.yaml -not -path '*/node_modules/*' -not -path './.git/*' -print0 | sort -z)
          while IFS= read -r -d '' lockfile; do
            enable_corepack
            package_dir="$(dirname "$lockfile")"
            yarn_version="$(cd "$package_dir" && yarn --version)"
            yarn_major="${yarn_version%%.*}"
            if ! [[ "$yarn_major" =~ ^[0-9]+$ ]]; then
              echo "Could not determine Yarn major version for $package_dir" >&2
              exit 1
            fi
            if [ "$yarn_major" -ge 2 ]; then
              (cd "$package_dir" && yarn install --immutable)
            else
              (cd "$package_dir" && yarn install --frozen-lockfile)
            fi
          done < <(find . -name yarn.lock -not -path '*/node_modules/*' -not -path './.git/*' -print0 | sort -z)
          has_ancestor_package_lock() {
            local candidate="$1"
            node - "$candidate" <<'NODE'
          const fs = require("fs");
          const path = require("path");
          const root = fs.realpathSync(process.cwd());
          const target = fs.realpathSync(process.argv[2]);
          const locks = ["package-lock.json", "pnpm-lock.yaml", "yarn.lock"];

          function patternsFor(scope) {
            const patterns = [];
            try {
              const manifest = JSON.parse(fs.readFileSync(path.join(scope, "package.json"), "utf8"));
              if (Array.isArray(manifest.workspaces)) patterns.push(...manifest.workspaces);
              if (manifest.workspaces && Array.isArray(manifest.workspaces.packages)) patterns.push(...manifest.workspaces.packages);
            } catch (_) {}
            try {
              const lines = fs.readFileSync(path.join(scope, "pnpm-workspace.yaml"), "utf8").split(/\r?\n/);
              let inPackages = false;
              for (const line of lines) {
                const inline = line.match(/^\s*packages\s*:\s*\[(.*)\]\s*$/);
                if (inline) {
                  for (const value of inline[1].split(",")) patterns.push(value.trim().replace(/^['"]|['"]$/g, ""));
                  inPackages = false;
                  continue;
                }
                if (/^\s*packages\s*:\s*$/.test(line)) {
                  inPackages = true;
                  continue;
                }
                if (inPackages && /^\S/.test(line)) inPackages = false;
                if (inPackages) {
                  const item = line.match(/^\s*-\s*(['"]?)(.*?)\1\s*(?:#.*)?$/);
                  if (item && item[2]) patterns.push(item[2]);
                }
              }
            } catch (_) {}
            return patterns;
          }

          function globExpression(pattern) {
            pattern = pattern.split(String.fromCharCode(92)).join("/").replace(/^\.\//, "").replace(/^\/+|\/+$/g, "");
            let source = "^";
            for (let index = 0; index < pattern.length; index += 1) {
              const character = pattern[index];
              if (character === "*") {
                if (pattern[index + 1] === "*") {
                  index += 1;
                  if (pattern[index + 1] === "/") {
                    index += 1;
                    source += "(?:.*/)?";
                  } else {
                    source += ".*";
                  }
                } else {
                  source += "[^/]*";
                }
              } else if (character === "?") {
                source += "[^/]";
              } else {
                if (".+()|[]{}^$".includes(character)) source += "\\";
                source += character;
              }
            }
            return new RegExp(source + "$");
          }

          function owns(scope) {
            const relative = path.relative(scope, target).split(path.sep).join("/");
            let matched = false;
            for (let pattern of patternsFor(scope)) {
              pattern = String(pattern).trim().split(String.fromCharCode(92)).join("/");
              const excluded = pattern.startsWith("!");
              if (excluded) pattern = pattern.slice(1);
              if (pattern && globExpression(pattern).test(relative)) matched = !excluded;
            }
            return matched;
          }

          function insideRoot(value) {
            const relative = path.relative(root, value);
            return relative === "" || (relative !== ".." && !relative.startsWith(".." + path.sep));
          }

          let scope = target;
          while (insideRoot(scope)) {
            const hasLock = locks.some((name) => fs.existsSync(path.join(scope, name)));
            if (hasLock && (scope === target || owns(scope))) process.exit(0);
            if (scope === root) break;
            const parent = path.dirname(scope);
            if (parent === scope) break;
            scope = parent;
          }
          process.exit(1);
          NODE
          }
          lockless_package_dirs=()
          while IFS= read -r -d '' manifest; do
            package_dir="$(dirname "$manifest")"
            if has_ancestor_package_lock "$package_dir"; then
              continue
            fi
            lockless_package_dirs+=("$package_dir")
          done < <(find . -name package.json -not -path '*/node_modules/*' -not -path './.git/*' -not -path '*/dist/*' -not -path '*/build/*' -not -path '*/coverage/*' -not -path '*/vendor/*' -not -path './.visual-hive/*' -print0 | sort -z)
          for package_dir in "${lockless_package_dirs[@]}"; do
            (cd "$package_dir" && npm install)
          done`
}

func repositoryTestShell(config Config) string {
	lines := []string{
		"          mkdir -p .visual-hive",
		"          repository_exit=0",
		"          printf '1\\n' > .visual-hive/pipeline-exit-code.txt",
		"          printf '1\\n' > .visual-hive/repository-test-exit-code.txt",
		"          : > .visual-hive/repository-tests.tsv",
		"          run_repository_test() {",
		"            set +e",
		"            \"$@\"",
		"            command_exit=$?",
		"            set -e",
		"            printf '%s\\t%s\\n' \"$command_exit\" \"$*\" >> .visual-hive/repository-tests.tsv",
		"            if [ \"$command_exit\" -ne 0 ] && [ \"$repository_exit\" -eq 0 ]; then",
		"              repository_exit=$command_exit",
		"            fi",
		"            return 0",
		"          }",
	}
	if len(config.TestCommands) == 0 {
		lines = append(lines, "          : # No additional repository unit-test bootstrap was required.")
	}
	for _, command := range config.TestCommands {
		quoted := make([]string, 0, len(command))
		for _, argument := range command {
			quoted = append(quoted, shellQuote(argument))
		}
		lines = append(lines, "          run_repository_test "+strings.Join(quoted, " "))
	}
	lines = append(lines, "          printf '%s\\n' \"$repository_exit\" > .visual-hive/repository-test-exit-code.txt")
	return strings.Join(lines, "\n")
}

// repositoryTestWorkflowJobs isolates every repository-authored command in its
// own GitHub-hosted job. Hive queries the exact job conclusions from GitHub's
// API; target files, step outputs, and uploaded artifact bytes are never an
// authority source for opening or resolving repository-test findings.
func repositoryTestWorkflowJobs(config Config) (string, string) {
	var jobs, needs strings.Builder
	dependencyInstall := repositoryTestDependencyInstallShell()
	jobIndex := 0
	for _, command := range config.TestCommands {
		if len(command) == 0 {
			continue
		}
		jobIndex++
		quoted := make([]string, 0, len(command))
		for _, argument := range command {
			quoted = append(quoted, shellQuote(argument))
		}
		fmt.Fprintf(&jobs, `  repository-test-%03d:
    name: Hive repository test %03d
    runs-on: ubuntu-latest
    timeout-minutes: 30
    steps:
      - uses: actions/checkout@%s
        with:
          fetch-depth: 1
          persist-credentials: false
      - uses: actions/setup-node@%s
        with:
          node-version: 22.23.1
      - uses: actions/setup-python@%s
        with:
          python-version: "3.11"
      - name: Install repository test dependencies
        shell: bash
        run: |
%s
      - name: Execute exact repository test command
        shell: bash
        run: |
          set -euo pipefail
          env -u GITHUB_TOKEN -u GH_TOKEN -u GITHUB_OUTPUT -u GITHUB_ENV -u GITHUB_PATH -u GITHUB_STEP_SUMMARY %s

`, jobIndex, jobIndex, checkoutActionSHA, setupNodeActionSHA, setupPythonActionSHA, dependencyInstall, strings.Join(quoted, " "))
		fmt.Fprintf(&needs, "      - repository-test-%03d\n", jobIndex)
	}
	if jobIndex == 0 {
		return "", "    if: ${{ always() }}"
	}
	return strings.TrimSuffix(jobs.String(), "\n"), "    needs:\n" + strings.TrimSuffix(needs.String(), "\n") + "\n    if: ${{ always() }}"
}

func repositoryTestDependencyInstallShell() string {
	return targetPackageInstallShell() + `
          if [ -f pyproject.toml ]; then
            python -m pip install -e .
          elif [ -f requirements.txt ]; then
            python -m pip install -r requirements.txt
          fi
          while IFS= read -r -d '' playwright_cli; do
            node "$playwright_cli" install --with-deps chromium
          done < <(find . -path '*/node_modules/@playwright/test/cli.js' -not -path './.git/*' -print0 | sort -z)`
}

func shellQuote(value string) string {
	if regexp.MustCompile(`^[A-Za-z0-9_./:@%+=,-]+$`).MatchString(value) {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}

func containsValue(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func quickstart(config Config, inspection RepositoryInspection) string {
	activation := ""
	if config.Automation == AutomationAutoMerge {
		activation = "\n\nAuto-merge protection uses two-phase activation: merge the exact managed setup/upgrade PR first. The next started Hive run verifies the installed files and exact production workflow path/event/head, completes both `visual-hive-production` and the actively guarded `visual-hive` eligibility seed on the current default head, and only then creates or verifies strict protection with the PR `visual-hive` context bound to the GitHub Actions App ID. Merge gating accepts that context only from `.github/workflows/visual-hive-pr.yml` on the exact pull-request head. No issue, repair, or merge lifecycle write occurs before activation."
	}
	return fmt.Sprintf("# Hive + Visual Hive\n\nThis repository is managed by Hive with `%s` coverage and `%s` automation authority. Visual Hive runs deterministic checks; Hive alone owns issues, repair branches, pull requests, merges, and closure. Hive keeps at most %d managed findings active as GitHub issues at once and permits at most %d bounded repair attempts per finding; every additional finding remains durable as a bead until capacity is available.%s\n\n## Operator commands\n\nHive automatically selects this repository's isolated persistent state. Run:\n\n```sh\nhive doctor --json\nhive status --json\nhive run --json\nhive start --json\nhive stop --json\nhive pause\nhive resume\nhive approve-merge --pr NUMBER --head EXACT_HEAD_SHA --plan --json\nhive approve-merge --pr NUMBER --head EXACT_HEAD_SHA --base EXACT_BASE_SHA --diff-digest EXACT_DIFF_SHA256 --reason \"reviewed exact path-held repair\" --json\nhive revoke-merge-approval --reason \"review withdrawn\" --json\nhive retry-repair --finding FINGERPRINT --recurrence N --attempt N --failure-class infrastructure --failure-id FAILURE_ID --reason \"dependency restored\" --json\n```\n\nIf status reports `workflow_dispatch_recovery`, use its exact `revoke_plan_command` (preferred for uncertain transport) or `retry_plan_command`; never delete the dispatch state manually.\n\nDefault branch: `%s`. Detected languages: %s.\n", config.Coverage, config.Automation, config.MaxActiveIssues, config.MaxRepairAttempts, activation, config.DefaultBranch, strings.Join(inspection.Languages, ", "))
}

func setupPRBody(marker string, plan SetupPlan) string {
	activation := ""
	if plan.Automation == AutomationAutoMerge {
		activation = " After this exact PR is merged, the next trusted `hive run` verifies these files and the exact production workflow provenance, completes the production verdict plus actively guarded PR-context eligibility seed, and only then activates exact GitHub-Actions-App-bound branch protection. A scheduler requested during setup remains deferred until those non-scheduler doctor gates are green. Merge gating still accepts `visual-hive` only from the exact PR workflow; no lifecycle write occurs before activation."
	}
	return fmt.Sprintf("%s\n\nInstalls Hive + Visual Hive as one production testing and repair experience.\n\n- Coverage: **%s**\n- Automation authority: **%s**\n- ACMM enforcement: **L%d**\n- Active issue WIP limit: **%d**\n- Repair attempt limit: **%d**\n- Provider: **%s**\n- Visual Hive source: `%s@%s`\n- Detected languages: %s\n- Testing layers: %s\n\nThe Visual workflow is read-only and uploads provenance-bound evidence. Hive is the only GitHub lifecycle writer.%s", marker, plan.Coverage, plan.Automation, plan.ACMMLevel, plan.MaxActiveIssues, plan.MaxRepairAttempts, plan.Provider, plan.VisualHiveRepository, plan.VisualHiveRef, strings.Join(plan.Inspection.Languages, ", "), strings.Join(plan.TestingLayers, ", "), activation)
}

func authorizeSetup(store *Store, policy automation.Policy, repository string, action automation.Action) error {
	decision := policy.Authorize(automation.ActionRequest{Action: action, Agent: "setup", Repository: repository, SetupApproved: true})
	if err := store.AuditStrict(AuditEntry{Action: string(action), Allowed: decision.Allowed, Repository: repository, Detail: strings.Join(decision.Reasons, "; ")}); err != nil {
		return fmt.Errorf("persist setup authorization audit: %w", err)
	}
	if !decision.Allowed {
		return fmt.Errorf("%s denied: %s", action, strings.Join(decision.Reasons, "; "))
	}
	return nil
}

func enrichRemoteInspection(ctx context.Context, client *hivegithub.Client, repository string, inspection *RepositoryInspection) {
	owner, repo, ok := strings.Cut(repository, "/")
	if !ok || client.GoGitHub() == nil {
		return
	}
	metadata, _, err := client.GoGitHub().Repositories.Get(ctx, owner, repo)
	if err == nil {
		inspection.RepositoryID = fmt.Sprintf("%d", metadata.GetID())
		if metadata.GetDefaultBranch() != "" {
			inspection.DefaultBranch = metadata.GetDefaultBranch()
		}
		for key, value := range metadata.GetPermissions() {
			inspection.Permissions[key] = value
		}
	}
	if _, _, err := client.GoGitHub().Repositories.GetBranchProtection(ctx, owner, repo, inspection.DefaultBranch); err == nil {
		inspection.BranchProtection = true
	}
}

func git(ctx context.Context, dir string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, "git", args...)
	command.Dir, command.Env = dir, safeEnvironment()
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	if err := command.Run(); err != nil {
		return output.String(), fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, safeOutput(output.String()))
	}
	return output.String(), nil
}

func safeEnvironment() []string {
	secret := regexp.MustCompile(`(?i)(TOKEN|SECRET|PASSWORD|PRIVATE_KEY|API_KEY)`)
	executionAffecting := regexp.MustCompile(`(?i)^(NODE_OPTIONS|NODE_PATH|BASH_ENV|ENV|SHELLOPTS|CDPATH|GIT_CONFIG.*|GIT_EXTERNAL_DIFF|GIT_SSH|GIT_SSH_COMMAND|NPM_CONFIG_.*|COREPACK_.*|PNPM_HOME|YARN_.*)$`)
	result := []string{"GIT_TERMINAL_PROMPT=0"}
	for _, pair := range os.Environ() {
		name, _, _ := strings.Cut(pair, "=")
		if !secret.MatchString(name) && !executionAffecting.MatchString(name) && !strings.EqualFold(name, "GH_TOKEN") && !strings.EqualFold(name, "GITHUB_TOKEN") {
			result = append(result, pair)
		}
	}
	return result
}

func safeOutput(value string) string {
	value = regexp.MustCompile(`(?i)(github_pat_[A-Za-z0-9_]{10,}|gh[pousr]_[A-Za-z0-9]{10,}|sk-[A-Za-z0-9_-]{10,})`).ReplaceAllString(value, "[REDACTED]")
	value = strings.TrimSpace(value)
	if len(value) > 2048 {
		value = value[len(value)-2048:]
	}
	return value
}

func safeRepoName(repository string) string {
	return strings.ReplaceAll(strings.ToLower(repository), "/", "-")
}

func managedSetupCheckoutPath(storeDir, stateDir, repository string, prior Config, hasPrior bool) (string, error) {
	current := filepath.Join(storeDir, "checkout")
	if !hasPrior {
		return current, nil
	}
	if strings.TrimSpace(prior.StateDir) == "" || !sameFilesystemPath(prior.StateDir, stateDir) {
		return "", fmt.Errorf("durable setup state path does not match selected state directory %s", stateDir)
	}
	candidate := strings.TrimSpace(prior.CheckoutDir)
	legacy := filepath.Join(storeDir, "checkouts", safeRepoName(repository))
	if candidate == "" || (!sameFilesystemPath(candidate, current) && !sameFilesystemPath(candidate, legacy)) {
		return "", fmt.Errorf("durable setup checkout path is outside the supported bounded or legacy state layout")
	}
	return filepath.Clean(candidate), nil
}

func cloneCommands(values [][]string) [][]string {
	result := make([][]string, len(values))
	for index := range values {
		result[index] = append([]string(nil), values[index]...)
	}
	return result
}

func stageManagedPaths(ctx context.Context, checkout string, paths []string) error {
	stageable := make([]string, 0, len(paths))
	for _, relative := range paths {
		if _, err := os.Lstat(filepath.Join(checkout, filepath.FromSlash(relative))); err == nil {
			stageable = append(stageable, relative)
			continue
		} else if !os.IsNotExist(err) {
			return err
		}
		tracked, err := git(ctx, checkout, "ls-files", "--", relative)
		if err != nil {
			return err
		}
		if strings.TrimSpace(tracked) != "" {
			stageable = append(stageable, relative)
		}
	}
	if len(stageable) == 0 {
		return nil
	}
	args := append([]string{"add", "-A", "--"}, stageable...)
	_, err := git(ctx, checkout, args...)
	return err
}
