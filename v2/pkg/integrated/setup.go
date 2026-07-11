package integrated

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/kubestellar/hive/v2/pkg/automation"
	hivegithub "github.com/kubestellar/hive/v2/pkg/github"
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
	UpsertRepairPullRequest(context.Context, string, string, string, string, string, string) (hivegithub.RepairPullRequest, error)
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
	store, err := NewStore(filepath.Join(options.StateDir, "integrated"))
	if err != nil {
		return SetupResult{}, err
	}
	prior, priorErr := store.Load()
	hasPrior := priorErr == nil && strings.EqualFold(prior.Repository, options.Repository)
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
	result := SetupResult{Plan: plan}
	if !options.Apply {
		return result, nil
	}
	if options.GitHub == nil {
		return result, fmt.Errorf("GitHub client is required to apply setup")
	}
	if options.VisualHive {
		if err := VerifyVisualHiveCommit(ctx, options.GitHub, options.VisualHiveRepo, options.VisualHiveRef); err != nil {
			return result, err
		}
	}
	branch := "hive/setup"
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
	if hasPrior {
		config.InstalledAt = prior.InstalledAt
		config.SetupPRNumber, config.SetupPRURL = prior.SetupPRNumber, prior.SetupPRURL
		config.PreviousVersion, config.Paused = prior.PreviousVersion, prior.Paused
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
	idempotent := strings.TrimSpace(diff) == ""
	reuseRemoteSetup := false
	sha := ""
	if !idempotent && hasPrior && prior.SetupBranch != "" {
		matches, remoteSHA, matchErr := stagedTreeMatchesRemoteBranch(ctx, checkout, prior.SetupBranch)
		if matchErr != nil {
			return result, matchErr
		}
		if matches {
			idempotent, reuseRemoteSetup, sha = true, true, remoteSHA
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
		return result, nil
	}
	if !idempotent {
		if _, err := git(ctx, checkout, "-c", "user.name=Hive Setup", "-c", "user.email=hive-setup@users.noreply.github.com", "commit", "-m", "chore: install Hive and Visual Hive"); err != nil {
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
	if !reuseRemoteSetup {
		if err := authorizeSetup(store, options.Policy, options.Repository, automation.ActionSetupPush); err != nil {
			return result, err
		}
		if _, err := git(ctx, checkout, "push", "--force-with-lease", "origin", "HEAD:refs/heads/"+branch); err != nil {
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
	pull, err := options.GitHub.UpsertRepairPullRequest(ctx, options.Repository, branch, defaultBranch, "Install Hive + Visual Hive production automation", body, marker)
	if err != nil {
		return result, err
	}
	config.SetupPRNumber, config.SetupPRURL = pull.Number, pull.URL
	if err := store.Save(config); err != nil {
		return result, err
	}
	result.Applied, result.Idempotent, result.Config = true, idempotent, &config
	result.Branch, result.CommitSHA, result.PRNumber, result.PRURL = branch, sha, pull.Number, pull.URL
	return result, nil
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
	if options.Apply && options.VisualHive && (options.VisualHiveCommand == "" || options.VisualHiveRepo == "" || !regexp.MustCompile(`^[a-f0-9]{40}$`).MatchString(options.VisualHiveRef)) {
		return fmt.Errorf("Visual Hive setup requires a command, repository, and immutable 40-character commit SHA")
	}
	return nil
}

func buildSetupPlan(options SetupOptions, inspection RepositoryInspection) SetupPlan {
	warnings := []string{}
	if !inspection.BranchProtection {
		warnings = append(warnings, "Default-branch protection was not detected; auto-merge remains technically disabled until protection or an equivalent ruleset is verified.")
	}
	if len(inspection.BaselineFiles) == 0 && options.VisualHive {
		warnings = append(warnings, "No reviewed visual baselines were detected; setup must prepare and review baselines before the first production scan.")
	}
	managedFiles := []string{".hive/integrated.json", ".github/workflows/hive-visual-hive.yml", "docs/hive-quickstart.md"}
	if options.VisualHive {
		managedFiles = append(managedFiles, "docs/visual-hive.md", "visual-hive.config.yaml", ".github/workflows/visual-hive-pr.yml", ".github/workflows/visual-hive-issue-lifecycle.yml", ".github/workflows/visual-hive-trusted-publisher.yml")
	}
	return SetupPlan{
		SchemaVersion: PlanSchema, GeneratedAt: time.Now().UTC(), Repository: options.Repository,
		Coverage: options.Coverage, Automation: options.Automation, Provider: options.Provider,
		ACMMLevel: acmmForAutomation(options.Automation), VisualHive: options.VisualHive, Inspection: inspection,
		MaxActiveIssues:   options.MaxActiveIssues,
		MaxRepairAttempts: options.MaxRepairAttempts,
		TestingLayers:     layersForCoverage(options.Coverage),
		FilesToManage:     managedFiles,
		RequiredActions:   []string{"Review and merge the setup PR", "Run hive doctor", "Launch the first complete production scan"},
		Warnings:          warnings, ReadOnly: true,
	}
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
	if !exists(filepath.Join(checkout, "visual-hive.config.yaml")) {
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
	doctorArgs := append([]string(nil), options.VisualHiveArgs...)
	doctorArgs = append(doctorArgs, "doctor", "--config", filepath.Join(checkout, "visual-hive.config.yaml"))
	doctor := exec.CommandContext(ctx, options.VisualHiveCommand, doctorArgs...)
	doctor.Dir, doctor.Env = checkout, safeEnvironment()
	output.Reset()
	doctor.Stdout, doctor.Stderr = &output, &output
	if err := doctor.Run(); err != nil {
		return fmt.Errorf("validate generated Visual Hive configuration: %w: %s", err, safeOutput(output.String()))
	}
	return nil
}

func profileForCoverage(coverage Coverage) string {
	if coverage == CoverageComprehensive || coverage == CoverageCustom {
		return "complex-app"
	}
	return "free-local"
}

func writeManagedFiles(root string, config Config, inspection RepositoryInspection) error {
	repositoryConfig := map[string]any{
		"schema_version": ConfigSchema, "repository": config.Repository, "repository_id": config.RepositoryID,
		"default_branch": config.DefaultBranch, "coverage": config.Coverage, "automation": config.Automation,
		"provider": config.Provider, "acmm_level": config.ACMMLevel, "max_active_issues": config.MaxActiveIssues, "max_repair_attempts": config.MaxRepairAttempts, "visual_hive": config.VisualHive,
		"visual_hive_repository": config.VisualHiveRepo, "visual_hive_ref": config.VisualHiveRef,
		"test_commands": config.TestCommands, "allowed_repair_paths": config.AllowedRepairPaths,
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
  schedule:
    - cron: "17 4 * * *"
  push:
    branches: [%s]

permissions:
  contents: read
  actions: read

concurrency:
  group: hive-visual-hive-production
  cancel-in-progress: false

jobs:
  visual-hive:
    runs-on: ubuntu-latest
    timeout-minutes: 45
    steps:
      - uses: actions/checkout@%s
        with:
          fetch-depth: 0
      - uses: actions/checkout@%s
        with:
          repository: %s
          ref: %s
          path: .hive-visual-tooling
      - uses: actions/setup-node@%s
        with:
          node-version: 22
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
`, config.DefaultBranch, checkoutActionSHA, checkoutActionSHA, config.VisualHiveRepo, config.VisualHiveRef, setupNodeActionSHA, setupPythonActionSHA, targetDependencies, repositoryTests, uploadArtifactActionSHA, config.ACMMLevel, uploadArtifactActionSHA)
}

func pullRequestWorkflow(config Config) string {
	repositoryTests := repositoryTestShell(config)
	targetDependencies := targetDependencyInstallShell()
	return fmt.Sprintf(`name: Visual Hive PR

on:
  pull_request:
  workflow_dispatch:

permissions:
  contents: read
  actions: read

concurrency:
  group: visual-hive-pr-${{ github.event.pull_request.number || github.ref }}
  cancel-in-progress: true

jobs:
  visual-hive:
    runs-on: ubuntu-latest
    timeout-minutes: 45
    steps:
      - uses: actions/checkout@%s
        with:
          fetch-depth: 0
      - uses: actions/checkout@%s
        with:
          repository: %s
          ref: %s
          path: .hive-visual-tooling
      - uses: actions/setup-node@%s
        with:
          node-version: 22
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
        run: |
          mkdir -p .visual-hive
          if [ "${{ github.event_name }}" = "pull_request" ]; then
            git diff --name-only "${{ github.event.pull_request.base.sha }}" "${{ github.event.pull_request.head.sha }}" > .visual-hive/changed-files.pr.txt
          else
            git fetch origin %s --depth=1
            git diff --name-only "origin/%s"...HEAD > .visual-hive/changed-files.pr.txt
          fi
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
`, checkoutActionSHA, checkoutActionSHA, config.VisualHiveRepo, config.VisualHiveRef, setupNodeActionSHA, setupPythonActionSHA, targetDependencies, config.DefaultBranch, config.DefaultBranch, repositoryTests, uploadArtifactActionSHA)
}

func testCommandsForCoverage(inspection RepositoryInspection, coverage Coverage) [][]string {
	commands := cloneCommands(inspection.TestCommands)
	if coverage != CoverageComprehensive && coverage != CoverageCustom {
		return commands
	}
	if !containsValue(inspection.Languages, "TypeScript/JavaScript") || hasRepositoryUnitCommand(commands) {
		return commands
	}
	commands = append(commands, []string{"node", "--test"})
	return sortTestCommands(commands)
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
	return fmt.Sprintf("# Hive + Visual Hive\n\nThis repository is managed by Hive with `%s` coverage and `%s` automation authority. Visual Hive runs deterministic checks; Hive alone owns issues, repair branches, pull requests, merges, and closure. Hive keeps at most %d managed findings active as GitHub issues at once and permits at most %d bounded repair attempts per finding; every additional finding remains durable as a bead until capacity is available.\n\n## Operator commands\n\nSet `HIVE_STATE_DIR` to the persistent Hive data directory, then run:\n\n```sh\nhive doctor --json\nhive status --json\nhive run --json\nhive pause\nhive resume\n```\n\nDefault branch: `%s`. Detected languages: %s.\n", config.Coverage, config.Automation, config.MaxActiveIssues, config.MaxRepairAttempts, config.DefaultBranch, strings.Join(inspection.Languages, ", "))
}

func setupPRBody(marker string, plan SetupPlan) string {
	return fmt.Sprintf("%s\n\nInstalls Hive + Visual Hive as one production testing and repair experience.\n\n- Coverage: **%s**\n- Automation authority: **%s**\n- ACMM enforcement: **L%d**\n- Active issue WIP limit: **%d**\n- Repair attempt limit: **%d**\n- Provider: **%s**\n- Detected languages: %s\n- Testing layers: %s\n\nThe Visual workflow is read-only and uploads provenance-bound evidence. Hive is the only GitHub lifecycle writer. Review tracked baselines and branch protection before enabling auto-merge.", marker, plan.Coverage, plan.Automation, plan.ACMMLevel, plan.MaxActiveIssues, plan.MaxRepairAttempts, plan.Provider, strings.Join(plan.Inspection.Languages, ", "), strings.Join(plan.TestingLayers, ", "))
}

func authorizeSetup(store *Store, policy automation.Policy, repository string, action automation.Action) error {
	decision := policy.Authorize(automation.ActionRequest{Action: action, Agent: "setup", Repository: repository, SetupApproved: true})
	store.Audit(AuditEntry{Action: string(action), Allowed: decision.Allowed, Repository: repository, Detail: strings.Join(decision.Reasons, "; ")})
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
