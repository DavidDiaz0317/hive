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
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
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

type SetupOptions struct {
	Repository        string
	Coverage          Coverage
	Automation        Automation
	Provider          string
	ProviderCommand   string
	ProviderArgs      []string
	VisualHive        bool
	StateDir          string
	Apply             bool
	Start             bool
	VisualHiveCommand string
	VisualHiveArgs    []string
	VisualHiveRepo    string
	VisualHiveRef     string
	MaxActiveIssues   int
	MaxRepairAttempts int
	GitHub            *hivegithub.Client
	Policy            automation.Policy
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
	checkout := filepath.Join(store.Dir(), "checkouts", safeRepoName(options.Repository))
	defaultBranch, err := ensureCheckout(ctx, options.Repository, checkout)
	if err != nil {
		return SetupResult{}, err
	}
	inspection, err := InspectCheckout(checkout, defaultBranch)
	if err != nil {
		return SetupResult{}, err
	}
	if options.GitHub != nil {
		enrichRemoteInspection(ctx, options.GitHub, options.Repository, &inspection)
	}
	plan := buildSetupPlan(options, inspection)
	result := SetupResult{Plan: plan, ProtectionActivationPending: options.Automation == AutomationAutoMerge, ActivationMessage: setupActivationMessage(options.Automation, options.Start, true)}
	if !options.Apply {
		return result, nil
	}
	if options.GitHub == nil {
		return result, fmt.Errorf("GitHub client is required to apply setup")
	}
	if strings.TrimSpace(inspection.RepositoryID) == "" {
		return result, fmt.Errorf("GitHub did not return the immutable repository ID for %s; refusing setup mutations", options.Repository)
	}
	if hasPrior {
		if _, err := verifyLiveRepositoryIdentity(ctx, options.GitHub, prior); err != nil {
			return result, fmt.Errorf("verify existing setup repository identity before mutation: %w", err)
		}
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
	if options.VisualHive {
		if err := runVisualHiveSetup(ctx, options, checkout); err != nil {
			return result, err
		}
	}
	config := Config{
		SchemaVersion: ConfigSchema, Repository: options.Repository, RepositoryID: inspection.RepositoryID, DefaultBranch: defaultBranch,
		Coverage: options.Coverage, Automation: options.Automation, Provider: options.Provider,
		ProviderCommand: options.ProviderCommand, ProviderArgs: append([]string(nil), options.ProviderArgs...),
		ACMMLevel: acmmForAutomation(options.Automation), VisualHive: options.VisualHive,
		MaxActiveIssues:   options.MaxActiveIssues,
		MaxRepairAttempts: options.MaxRepairAttempts,
		VisualHiveRepo:    options.VisualHiveRepo, VisualHiveRef: options.VisualHiveRef,
		VisualHiveCommand: options.VisualHiveCommand, VisualHiveArgs: append([]string(nil), options.VisualHiveArgs...),
		TestCommands: testCommandsForCoverage(inspection, options.Coverage), AllowedRepairPaths: defaultAllowedRepairPaths(),
		AllowedAutoMergePaths: defaultAllowedAutoMergePaths(),
		AllowedAutoMergeRisk:  []automation.RiskTier{automation.RiskAutomatic},
		CheckoutDir:           checkout, StateDir: options.StateDir, SetupBranch: branch,
		InstalledAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if options.VisualHive {
		config.VisualHiveConfigDigest, err = visualHiveConfigDigest(checkout)
		if err != nil {
			return result, err
		}
	}
	if hasPrior {
		config.InstalledAt = prior.InstalledAt
		config.SetupBranch, config.SetupPRNumber, config.SetupPRURL = prior.SetupBranch, prior.SetupPRNumber, prior.SetupPRURL
		config.PreviousVersion, config.Paused = prior.PreviousVersion, prior.Paused
		config.AllowedRepairPaths = append([]string(nil), prior.AllowedRepairPaths...)
		config.AllowedAutoMergePaths = append([]string(nil), prior.AllowedAutoMergePaths...)
		config.AllowedAutoMergeRisk = append([]automation.RiskTier(nil), prior.AllowedAutoMergeRisk...)
	}
	if err := writeManagedFiles(checkout, config, inspection); err != nil {
		return result, err
	}
	managed := append([]string(nil), plan.FilesToManage...)
	baselineFiles, baselineErr := visualBaselineFiles(checkout)
	if baselineErr != nil {
		return result, baselineErr
	}
	if len(baselineFiles) > 0 {
		plan.FilesToManage = sortedUnique(append(plan.FilesToManage, baselineFiles...))
		result.Plan = plan
	}
	if err := authorizeSetup(store, options.Policy, options.Repository, automation.ActionSetupCommit); err != nil {
		return result, err
	}
	if err := stageManagedPaths(ctx, checkout, managed); err != nil {
		return result, err
	}
	if len(baselineFiles) > 0 {
		args := append([]string{"add", "-f", "--"}, baselineFiles...)
		if _, err := git(ctx, checkout, args...); err != nil {
			return result, fmt.Errorf("stage explicitly reviewed baseline candidates: %w", err)
		}
	}
	diff, err := git(ctx, checkout, "diff", "--cached", "--name-only")
	if err != nil {
		return result, err
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
	if idempotent && hasPrior && !reuseRemoteSetup {
		sha, shaErr := git(ctx, checkout, "rev-parse", "HEAD")
		if shaErr != nil {
			return result, shaErr
		}
		if err := store.Save(config); err != nil {
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
	if err := store.Save(config); err != nil {
		return result, err
	}
	result.Applied, result.Idempotent, result.Config = true, idempotent, &config
	result.Branch, result.CommitSHA, result.PRNumber, result.PRURL = branch, sha, pull.Number, pull.URL
	if err := setSetupActivationStatus(&result, store, config, options.Start); err != nil {
		return result, err
	}
	return result, nil
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
		return "Merge the exact managed setup PR. The already-started Hive scheduler will verify the installed files, complete one trusted default-branch Visual Hive run, and then activate exact GitHub-Actions-App-bound protection before any lifecycle write."
	}
	return "Merge the exact managed setup PR, then run hive start or hive run. Hive will complete one trusted default-branch Visual Hive run and activate exact GitHub-Actions-App-bound protection before any lifecycle write."
}

func defaultAllowedRepairPaths() []string {
	return []string{"src/**", "**/src/**", "public/**", "**/public/**", "index.html", "**/index.html", "visual-hive.config.yaml", "test/**", "tests/**", "**/test/**", "**/tests/**", "**/*.test.*", "**/*.spec.*", "**/*_test.go"}
}

func defaultAllowedAutoMergePaths() []string {
	return []string{"test/**", "tests/**", "**/*.test.*", "**/*.spec.*", "**/*_test.go"}
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
	SchemaVersion          string                `json:"schema_version"`
	Repository             string                `json:"repository"`
	RepositoryID           string                `json:"repository_id"`
	DefaultBranch          string                `json:"default_branch"`
	Coverage               Coverage              `json:"coverage"`
	Automation             Automation            `json:"automation"`
	Provider               string                `json:"provider"`
	ACMMLevel              int                   `json:"acmm_level"`
	MaxActiveIssues        int                   `json:"max_active_issues"`
	MaxRepairAttempts      int                   `json:"max_repair_attempts"`
	VisualHive             bool                  `json:"visual_hive"`
	VisualHiveRepo         string                `json:"visual_hive_repository"`
	VisualHiveRef          string                `json:"visual_hive_ref"`
	VisualHiveConfigDigest string                `json:"visual_hive_config_digest,omitempty"`
	TestCommands           [][]string            `json:"test_commands"`
	AllowedRepairPaths     []string              `json:"allowed_repair_paths"`
	AllowedAutoMergePaths  []string              `json:"allowed_auto_merge_paths"`
	AllowedAutoMergeRisk   []automation.RiskTier `json:"allowed_auto_merge_risk"`
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
	if !options.VisualHive {
		return fmt.Errorf("integrated Hive requires Visual Hive deterministic testing; disabling --visual-hive is not supported")
	}
	if options.Apply && options.VisualHive && (options.VisualHiveCommand == "" || options.VisualHiveRepo == "" || !regexp.MustCompile(`^[a-f0-9]{40}$`).MatchString(options.VisualHiveRef)) {
		return fmt.Errorf("Visual Hive setup requires a command, repository, and immutable 40-character commit SHA")
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
		Coverage: options.Coverage, Automation: options.Automation, Provider: options.Provider,
		ACMMLevel: acmmForAutomation(options.Automation), VisualHive: options.VisualHive, Inspection: inspection,
		MaxActiveIssues:   options.MaxActiveIssues,
		MaxRepairAttempts: options.MaxRepairAttempts,
		TestingLayers:     layersForCoverage(options.Coverage),
		FilesToManage:     managedFiles,
		RequiredActions:   setupRequiredActions(options.Automation),
		Warnings:          warnings, ReadOnly: true,
	}
}

func setupRequiredActions(automation Automation) []string {
	actions := []string{"Review and merge the exact setup PR"}
	if automation == AutomationAutoMerge {
		actions = append(actions, "Leave the started scheduler running, or run hive start/hive run; Hive will complete one trusted scan and activate exact-App-bound protection before lifecycle writes")
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
	if !exists(filepath.Join(checkout, ".git")) {
		if err := os.MkdirAll(filepath.Dir(checkout), 0o700); err != nil {
			return "", err
		}
		if _, err := git(ctx, filepath.Dir(checkout), "clone", "--origin", "origin", "https://github.com/"+repository+".git", checkout); err != nil {
			return "", fmt.Errorf("clone target repository: %w", err)
		}
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
	if _, err := git(ctx, checkout, "switch", "--detach", "origin/"+branch); err != nil {
		return "", fmt.Errorf("switch managed checkout to origin/%s: %w", branch, err)
	}
	return branch, nil
}

func runVisualHiveSetup(ctx context.Context, options SetupOptions, checkout string) error {
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
	command.Env = safeEnvironment()
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
	doctor.Dir, doctor.Env = checkout, safeEnvironment()
	output.Reset()
	doctor.Stdout, doctor.Stderr = &output, &output
	if err := doctor.Run(); err != nil {
		return fmt.Errorf("validate generated Visual Hive configuration: %w: %s", err, safeOutput(output.String()))
	}
	if len(existingVisualBaselines(checkout)) == 0 {
		bootstrapArgs := append([]string(nil), options.VisualHiveArgs...)
		bootstrapArgs = append(bootstrapArgs, "pipeline", "--config", filepath.Join(checkout, "visual-hive.config.yaml"), "--mode", "pr", "--bootstrap-baselines", "--continue-on-error", "--format", "json")
		bootstrap := exec.CommandContext(ctx, options.VisualHiveCommand, bootstrapArgs...)
		bootstrap.Dir, bootstrap.Env = checkout, safeEnvironment()
		output.Reset()
		bootstrap.Stdout, bootstrap.Stderr = &output, &output
		// A target defect may make the bootstrap pipeline non-zero. Any created
		// snapshots are still explicit setup-PR review candidates; the first
		// production run will turn the remaining deterministic defect into a
		// Hive finding after setup is merged.
		_ = bootstrap.Run()
	}
	return nil
}

// mergeVisualHiveRecommendation reconciles every coverage-owned section to the
// deterministic recommendation. This makes upgrades and downgrades exact; the
// resulting setup PR remains the review boundary for repository-specific edits.
func mergeVisualHiveRecommendation(checkout, expectedProfile string) error {
	configPath := filepath.Join(checkout, "visual-hive.config.yaml")
	recommendationPath := filepath.Join(checkout, ".visual-hive", "recommendations.json")
	existingData, err := os.ReadFile(configPath)
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
	recommendationData, err := os.ReadFile(recommendationPath)
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
		return os.WriteFile(configPath, updated, 0o600)
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
	return os.WriteFile(configPath, updated, 0o600)
}

func markVisualHiveConfigOrigin(checkout, origin, profile string) error {
	path := filepath.Join(checkout, "visual-hive.config.yaml")
	data, err := os.ReadFile(path)
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
	return os.WriteFile(path, updated, 0o600)
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
	data, err := os.ReadFile(filepath.Join(checkout, "visual-hive.config.yaml"))
	if err != nil {
		return "", fmt.Errorf("read repository-specific Visual Hive config for digest: %w", err)
	}
	digest := sha256.Sum256([]byte(normalizeManagedText(string(data))))
	return hex.EncodeToString(digest[:]), nil
}

func existingVisualBaselines(checkout string) []string {
	files, _ := visualBaselineFiles(checkout)
	return files
}

func visualBaselineFiles(checkout string) ([]string, error) {
	root := filepath.Join(checkout, ".visual-hive", "snapshots")
	files := []string{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			if os.IsNotExist(walkErr) {
				return nil
			}
			return walkErr
		}
		if entry.IsDir() || !strings.EqualFold(filepath.Ext(entry.Name()), ".png") {
			return nil
		}
		relative, err := filepath.Rel(checkout, path)
		if err != nil || strings.HasPrefix(relative, "..") {
			return fmt.Errorf("baseline path escaped the managed checkout")
		}
		files = append(files, filepath.ToSlash(relative))
		return nil
	})
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list Visual Hive baseline candidates: %w", err)
	}
	return sortedUnique(files), nil
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
		target := filepath.Join(root, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(target, []byte(content), 0o600); err != nil {
			return err
		}
	}
	if config.VisualHive {
		for _, relative := range []string{".github/workflows/visual-hive-issue-lifecycle.yml", ".github/workflows/visual-hive-trusted-publisher.yml"} {
			if err := os.Remove(filepath.Join(root, filepath.FromSlash(relative))); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	}
	return nil
}

func workflow(config Config) string {
	repositoryTests := repositoryTestShell(config)
	targetDependencies := targetDependencyInstallShell()
	return fmt.Sprintf(`name: Hive Visual Hive Production

on:
  workflow_dispatch:
    inputs:
      hive_dispatch_id:
        description: Opaque durable Hive dispatch correlation
        required: true
        type: string

run-name: "Hive Visual Hive Production [${{ inputs.hive_dispatch_id }}]"

permissions:
  contents: read
  actions: read

concurrency:
  group: hive-visual-hive-production
  cancel-in-progress: false

jobs:
  visual-hive-production:
    runs-on: ubuntu-latest
    timeout-minutes: 45
    steps:
      - uses: actions/checkout@%s
        with:
          fetch-depth: 0
          persist-credentials: false
      - uses: actions/checkout@%s
        with:
          repository: %s
          ref: %s
          path: .hive-visual-tooling
          persist-credentials: false
      - uses: actions/setup-node@%s
        with:
          node-version: 22.23.1
          cache: npm
          cache-dependency-path: .hive-visual-tooling/package-lock.json
      - uses: actions/setup-python@%s
        with:
          python-version: "3.11"
      - name: Build immutable Visual Hive tooling
        working-directory: .hive-visual-tooling
        run: npm ci && npm run build
      - name: Move trusted tooling outside target tree
        shell: bash
        run: |
          mv .hive-visual-tooling "$RUNNER_TEMP/visual-hive-tooling"
          echo "VISUAL_HIVE_CLI=$RUNNER_TEMP/visual-hive-tooling/packages/cli/dist/index.js" >> "$GITHUB_ENV"
      - name: Install target dependencies and matching Playwright browser
        shell: bash
        run: |
%s
      - name: Run complete deterministic production scan
        shell: bash
        run: |
%s
          set +e
          node "$VISUAL_HIVE_CLI" pipeline --config visual-hive.config.yaml --mode full --ci --continue-on-error --skip-install --github-step-summary
          pipeline_exit=$?
          set -e
          printf '%%s\n' "$pipeline_exit" > .visual-hive/pipeline-exit-code.txt
          echo "Visual Hive deterministic pipeline exit: $pipeline_exit"
          node "$VISUAL_HIVE_CLI" issues --config visual-hive.config.yaml --write
          # Writes .visual-hive/baselines.json before artifact upload.
          node "$VISUAL_HIVE_CLI" baselines list --config visual-hive.config.yaml --write
          node "$VISUAL_HIVE_CLI" hive integration-smoke --config visual-hive.config.yaml --mode measured
          node <<'NODE'
          const fs = require("fs");
          const candidates = [".visual-hive/plan.full.json", ".visual-hive/plan.json"];
          const planPath = candidates.find(fs.existsSync);
          const plan = planPath ? JSON.parse(fs.readFileSync(planPath, "utf8")) : {};
          const rows = Array.isArray(plan.items) ? plan.items : [];
          const layerPath = ".visual-hive/testing-layers.json";
          const layerReport = fs.existsSync(layerPath) ? JSON.parse(fs.readFileSync(layerPath, "utf8")) : {};
          const layers = Array.isArray(layerReport.layers) ? layerReport.layers : [];
          const systemScopes = [
            ["workflow-safety", ".visual-hive/workflows.json"],
            ["provider-governance", ".visual-hive/provider-results.json"]
          ].filter(([, artifact]) => fs.existsSync(artifact)).map(([scope]) => scope);
          const ids = [...new Set([
            ...rows.map((item) => item.contractId || item.id),
            ...layers.map((layer) => Number.isInteger(layer.id) ? "testing-layer:" + layer.id : null),
            ...systemScopes
          ].filter(Boolean))].sort();
          fs.writeFileSync(".visual-hive/evaluated-contracts.txt", ids.join("\n") + "\n");
          NODE
      - name: Upload independently verifiable evidence
        id: evidence
        uses: actions/upload-artifact@%s
        with:
          name: visual-hive-evidence-${{ github.run_id }}
          path: |
            .visual-hive
            !.visual-hive/bundles/**
          if-no-files-found: error
          include-hidden-files: true
          retention-days: 14
      - name: Build Hive lifecycle bundle bound to evidence artifact
        shell: bash
        env:
          VISUAL_HIVE_WORKFLOW_ARTIFACT_ID: ${{ steps.evidence.outputs.artifact-id }}
        run: |
          args=()
          while IFS= read -r contract; do
            if [ -n "$contract" ]; then args+=(--evaluated-contract "$contract"); fi
          done < .visual-hive/evaluated-contracts.txt
          node "$VISUAL_HIVE_CLI" hive bundle --config visual-hive.config.yaml --issues .visual-hive/issues.json --acmm-request %d --scan-scope full --authoritative-for-resolution "${args[@]}"
      - name: Upload trusted Hive bundle
        id: bundle
        uses: actions/upload-artifact@%s
        with:
          name: visual-hive-bundle-${{ github.run_id }}
          path: .visual-hive/bundles
          if-no-files-found: error
          include-hidden-files: true
          retention-days: 14

  # GitHub allows an App-bound context to become required only after that
  # context has completed successfully in the repository within seven days. This
  # seed uses the future PR context, but actively fails on every non-default or
  # stale dispatch. Hive independently verifies both jobs before protection.
  visual-hive:
    runs-on: ubuntu-latest
    timeout-minutes: 10
    steps:
      - name: Reject a non-default production dispatch
        shell: bash
        env:
          HIVE_EVENT_NAME: ${{ github.event_name }}
          HIVE_REF: ${{ github.ref }}
          HIVE_DEFAULT_BRANCH: ${{ github.event.repository.default_branch }}
        run: |
          set -euo pipefail
          test "$HIVE_EVENT_NAME" = "workflow_dispatch"
          test "$HIVE_REF" = "refs/heads/$HIVE_DEFAULT_BRANCH"
      - uses: actions/checkout@%s
        with:
          ref: ${{ github.event.repository.default_branch }}
          fetch-depth: 1
          persist-credentials: false
      - name: Verify dispatch SHA is the current default head
        shell: bash
        env:
          HIVE_DISPATCH_SHA: ${{ github.sha }}
        run: |
          set -euo pipefail
          test "$(git rev-parse HEAD)" = "$HIVE_DISPATCH_SHA"
`, checkoutActionSHA, checkoutActionSHA, config.VisualHiveRepo, config.VisualHiveRef, setupNodeActionSHA, setupPythonActionSHA, targetDependencies, repositoryTests, uploadArtifactActionSHA, config.ACMMLevel, uploadArtifactActionSHA, checkoutActionSHA)
}

func pullRequestWorkflow(config Config) string {
	repositoryTests := repositoryTestShell(config)
	targetDependencies := targetDependencyInstallShell()
	return fmt.Sprintf(`name: Visual Hive PR

on:
  pull_request:

permissions:
  contents: read
  actions: read

concurrency:
  group: visual-hive-pr-${{ github.event.pull_request.number }}
  cancel-in-progress: true

jobs:
  visual-hive:
    runs-on: ubuntu-latest
    timeout-minutes: 45
    steps:
      - uses: actions/checkout@%s
        with:
          fetch-depth: 0
          persist-credentials: false
      - uses: actions/checkout@%s
        with:
          repository: %s
          ref: %s
          path: .hive-visual-tooling
          persist-credentials: false
      - uses: actions/setup-node@%s
        with:
          node-version: 22.23.1
          cache: npm
          cache-dependency-path: .hive-visual-tooling/package-lock.json
      - uses: actions/setup-python@%s
        with:
          python-version: "3.11"
      - name: Build immutable Visual Hive tooling
        working-directory: .hive-visual-tooling
        run: npm ci && npm run build
      - name: Move trusted tooling outside target tree
        shell: bash
        run: |
          mv .hive-visual-tooling "$RUNNER_TEMP/visual-hive-tooling"
          echo "VISUAL_HIVE_CLI=$RUNNER_TEMP/visual-hive-tooling/packages/cli/dist/index.js" >> "$GITHUB_ENV"
      - name: Install target dependencies and matching Playwright browser
        shell: bash
        run: |
%s
      - name: Capture pull request scope
        shell: bash
        env:
          HIVE_BASE_SHA: ${{ github.event.pull_request.base.sha }}
          HIVE_HEAD_SHA: ${{ github.event.pull_request.head.sha }}
        run: |
          mkdir -p .visual-hive
          git diff --name-only "$HIVE_BASE_SHA" "$HIVE_HEAD_SHA" > .visual-hive/changed-files.pr.txt
      - name: Run deterministic repository and Visual Hive gates
        shell: bash
        run: |
%s
          set +e
          node "$VISUAL_HIVE_CLI" pipeline --config visual-hive.config.yaml --mode pr --changed-files .visual-hive/changed-files.pr.txt --ci --continue-on-error --skip-install --github-step-summary
          pipeline_exit=$?
          set -e
          printf '%%s\n' "$pipeline_exit" > .visual-hive/pipeline-exit-code.txt
          node "$VISUAL_HIVE_CLI" issues --config visual-hive.config.yaml --write
          # Writes .visual-hive/baselines.json before artifact upload.
          node "$VISUAL_HIVE_CLI" baselines list --config visual-hive.config.yaml --write
          node "$VISUAL_HIVE_CLI" hive integration-smoke --config visual-hive.config.yaml --mode measured
      - name: Upload review evidence
        if: always()
        uses: actions/upload-artifact@%s
        with:
          name: visual-hive-pr
          path: .visual-hive
          if-no-files-found: error
          include-hidden-files: true
          retention-days: 14
      - name: Enforce deterministic verdict
        if: always()
        shell: bash
        run: exit "$(cat .visual-hive/pipeline-exit-code.txt)"
`, checkoutActionSHA, checkoutActionSHA, config.VisualHiveRepo, config.VisualHiveRef, setupNodeActionSHA, setupPythonActionSHA, targetDependencies, repositoryTests, uploadArtifactActionSHA)
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
	if maximumRank < 3 || !containsValue(inspection.Languages, "TypeScript/JavaScript") || hasRepositoryUnitCommand(commands) {
		return commands
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

func hasRepositoryUnitCommand(commands [][]string) bool {
	for _, command := range commands {
		value := strings.ToLower(strings.Join(command, " "))
		if value == "node --test" || strings.Contains(value, "vitest") || strings.Contains(value, "jest") || strings.HasSuffix(value, " run test") || strings.Contains(value, "test:unit") || strings.Contains(value, "test:ci") {
			return true
		}
	}
	return false
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
	return `          npm_lock_count=0
          while IFS= read -r lockfile; do
            package_dir="$(dirname "$lockfile")"
            npm --prefix "$package_dir" ci
            npm_lock_count=$((npm_lock_count + 1))
          done < <(find . -name package-lock.json -not -path '*/node_modules/*' -not -path './.git/*' -print | sort)
          if [ "$npm_lock_count" -eq 0 ]; then
            while IFS= read -r manifest; do
              npm --prefix "$(dirname "$manifest")" install
            done < <(find . -name package.json -not -path '*/node_modules/*' -not -path './.git/*' -print | sort)
          fi
          while IFS= read -r lockfile; do
            corepack enable
            pnpm --dir "$(dirname "$lockfile")" install --frozen-lockfile
          done < <(find . -name pnpm-lock.yaml -not -path '*/node_modules/*' -not -path './.git/*' -print | sort)
          while IFS= read -r lockfile; do
            corepack enable
            yarn --cwd "$(dirname "$lockfile")" install --immutable
          done < <(find . -name yarn.lock -not -path '*/node_modules/*' -not -path './.git/*' -print | sort)
          if [ -f pyproject.toml ]; then
            python -m pip install -e .
          elif [ -f requirements.txt ]; then
            python -m pip install -r requirements.txt
          fi
          tooling_playwright="$RUNNER_TEMP/visual-hive-tooling/node_modules/@playwright/test/cli.js"
          node "$tooling_playwright" install --with-deps chromium
          while IFS= read -r playwright_cli; do
            node "$playwright_cli" install chromium
          done < <(find . -path '*/node_modules/@playwright/test/cli.js' -not -path './.git/*' -print | sort)`
}

func repositoryTestShell(config Config) string {
	if len(config.TestCommands) == 0 {
		return "          : # No additional repository unit-test bootstrap was required."
	}
	lines := make([]string, 0, len(config.TestCommands))
	for _, command := range config.TestCommands {
		quoted := make([]string, 0, len(command))
		for _, argument := range command {
			quoted = append(quoted, shellQuote(argument))
		}
		lines = append(lines, "          "+strings.Join(quoted, " "))
	}
	return strings.Join(lines, "\n")
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
		activation = " After this exact PR is merged, the already-started scheduler (or the next `hive start`/`hive run`) verifies these files and the exact production workflow provenance, completes the production verdict plus actively guarded PR-context eligibility seed, and only then activates exact GitHub-Actions-App-bound branch protection. Merge gating still accepts `visual-hive` only from the exact PR workflow; no lifecycle write occurs before activation."
	}
	return fmt.Sprintf("%s\n\nInstalls Hive + Visual Hive as one production testing and repair experience.\n\n- Coverage: **%s**\n- Automation authority: **%s**\n- ACMM enforcement: **L%d**\n- Active issue WIP limit: **%d**\n- Repair attempt limit: **%d**\n- Provider: **%s**\n- Detected languages: %s\n- Testing layers: %s\n\nThe Visual workflow is read-only and uploads provenance-bound evidence. Hive is the only GitHub lifecycle writer.%s", marker, plan.Coverage, plan.Automation, plan.ACMMLevel, plan.MaxActiveIssues, plan.MaxRepairAttempts, plan.Provider, strings.Join(plan.Inspection.Languages, ", "), strings.Join(plan.TestingLayers, ", "), activation)
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
	result := []string{"GIT_TERMINAL_PROMPT=0"}
	for _, pair := range os.Environ() {
		name, _, _ := strings.Cut(pair, "=")
		if !secret.MatchString(name) && !strings.EqualFold(name, "GH_TOKEN") && !strings.EqualFold(name, "GITHUB_TOKEN") {
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
