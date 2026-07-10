package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/kubestellar/hive/v2/pkg/automation"
	hivegithub "github.com/kubestellar/hive/v2/pkg/github"
	"github.com/kubestellar/hive/v2/pkg/integrated"
	"github.com/kubestellar/hive/v2/pkg/repair"
	"github.com/kubestellar/hive/v2/pkg/visualhive"
)

func runIntegratedCommand(command string, args []string) int {
	switch command {
	case "setup":
		return runSetupCommand(args)
	case "status":
		return runIntegratedStatus(args)
	case "doctor":
		return runIntegratedDoctor(args)
	case "start":
		return runIntegratedStart(args)
	case "stop":
		return runIntegratedStop(args)
	case "daemon":
		return runIntegratedDaemon(args)
	case "pause", "resume":
		return runIntegratedPause(command, args)
	case "set-coverage", "set-automation":
		return runIntegratedSetting(command, args)
	case "set-issue-limit":
		return runIntegratedIssueLimit(args)
	case "set-retry-limit":
		return runIntegratedRetryLimit(args)
	case "run":
		return runIntegratedRun(args)
	case "upgrade", "rollback", "uninstall":
		return runIntegratedManagement(command, args)
	default:
		fmt.Fprintf(os.Stderr, "unknown integrated Hive command %q\n", command)
		return 2
	}
}

func runIntegratedManagement(command string, args []string) int {
	flags := flag.NewFlagSet("hive "+command, flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	stateDir := flags.String("state-dir", defaultIntegratedStateDir(), "persistent Hive state directory")
	visualRef := ""
	flags.StringVar(&visualRef, "version", "", "immutable Visual Hive commit SHA")
	flags.StringVar(&visualRef, "visual-hive-ref", "", "immutable Visual Hive commit SHA")
	deleteState := flags.Bool("delete-state", false, "permanently delete managed local state after opening the uninstall PR")
	jsonOutput := flags.Bool("json", false, "emit machine-readable JSON")
	githubTokenEnv := flags.String("github-token-env", "HIVE_GITHUB_TOKEN", "environment variable containing GitHub token")
	githubAPIURL := flags.String("github-api-url", "", "optional GitHub Enterprise API URL")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if command != "uninstall" && visualRef == "" && command != "rollback" {
		fmt.Fprintln(os.Stderr, "--visual-hive-ref is required")
		return 2
	}
	if command == "uninstall" {
		status := readIntegratedDaemonStatus(*stateDir)
		if status.Running {
			if stopErr := terminateProcess(status.PID); stopErr != nil {
				fmt.Fprintln(os.Stderr, "uninstall could not stop the persistent scheduler:", stopErr)
				return 1
			}
			deadline := time.Now().Add(10 * time.Second)
			for time.Now().Before(deadline) && processIsAlive(status.PID) {
				time.Sleep(100 * time.Millisecond)
			}
			if processIsAlive(status.PID) {
				fmt.Fprintln(os.Stderr, "uninstall could not stop the persistent scheduler within 10 seconds")
				return 1
			}
		}
	}
	token := resolveGitHubToken(*githubTokenEnv)
	if token == "" {
		fmt.Fprintln(os.Stderr, "GitHub authorization is required")
		return 2
	}
	client := hivegithub.NewClient(token, "", nil, slog.New(slog.NewTextHandler(io.Discard, nil)), *githubAPIURL)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	result, err := integrated.RunManagement(ctx, integrated.ManagementOptions{
		Operation: integrated.ManagementOperation(command), StateDir: *stateDir, VisualHiveRef: visualRef,
		DeleteState: *deleteState, GitHub: client,
	})
	if err != nil {
		if *jsonOutput {
			_ = encodeJSON(map[string]any{"schema_version": "hive.management.v1", "operation": command, "error": err.Error(), "partial": result})
		} else {
			fmt.Fprintln(os.Stderr, command+" failed:", err)
		}
		return 1
	}
	if *jsonOutput {
		return encodeJSON(result)
	}
	if result.Idempotent && result.PRURL == "" {
		fmt.Printf("%s already at the requested state.\n", command)
	} else {
		fmt.Printf("%s PR ready: %s\n", command, result.PRURL)
	}
	return 0
}

func runIntegratedRun(args []string) int {
	flags := flag.NewFlagSet("hive run", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	stateDir := flags.String("state-dir", defaultIntegratedStateDir(), "persistent Hive state directory")
	jsonOutput := flags.Bool("json", false, "emit machine-readable JSON")
	timeout := flags.Duration("timeout", 45*time.Minute, "maximum hosted run and lifecycle duration")
	githubTokenEnv := flags.String("github-token-env", "HIVE_GITHUB_TOKEN", "environment variable containing GitHub token")
	githubAPIURL := flags.String("github-api-url", "", "optional GitHub Enterprise API URL")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	token := resolveGitHubToken(*githubTokenEnv)
	if token == "" {
		fmt.Fprintln(os.Stderr, "GitHub authorization is required")
		return 2
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	client := hivegithub.NewClient(token, "", nil, logger, *githubAPIURL)
	ctx, cancel := context.WithTimeout(context.Background(), *timeout+time.Minute)
	defer cancel()
	result, err := integrated.RunOnce(ctx, integrated.RunOptions{StateDir: *stateDir, Timeout: *timeout, GitHub: client})
	if err != nil {
		if *jsonOutput {
			_ = encodeJSON(map[string]any{"schema_version": "hive.production-run.v1", "error": err.Error(), "partial": result})
		} else {
			fmt.Fprintln(os.Stderr, "Hive production run failed:", err)
		}
		return 1
	}
	if *jsonOutput {
		return encodeJSON(result)
	}
	fmt.Printf("Run %s completed: findings=%d issues-updated=%d repairs=%d\n", result.Workflow.RunURL, result.Lifecycle.Created+result.Lifecycle.Updated, result.Outbox.Succeeded, len(result.Repairs))
	return 0
}

func runSetupCommand(args []string) int {
	flags := flag.NewFlagSet("hive setup", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	repository := flags.String("repo", "", "GitHub repository owner/name")
	coverageValue := flags.String("coverage", "", "essential, standard, comprehensive, or custom")
	automationValue := flags.String("automation", "", "advisory, issues, repair-pr, or auto-merge")
	provider := flags.String("provider", "codex", "repair model provider")
	providerCommand := flags.String("provider-command", os.Getenv("HIVE_CODEX_COMMAND"), "repair provider executable; auto-detected for Codex")
	visualHive := flags.Bool("visual-hive", true, "install Visual Hive deterministic testing")
	visualCommand := flags.String("visual-hive-command", "", "Visual Hive CLI launcher; defaults to the packaged runtime")
	visualHome := flags.String("visual-hive-home", os.Getenv("HIVE_VISUAL_HIVE_HOME"), "directory containing an immutable Visual Hive release bundle")
	visualRepo := flags.String("visual-hive-repo", valueOrEnv("VISUAL_HIVE_REPOSITORY", "DavidDiaz0317/visual-hive"), "Visual Hive source repository")
	visualRef := flags.String("visual-hive-ref", os.Getenv("VISUAL_HIVE_REF"), "immutable Visual Hive commit SHA")
	maxActiveIssues := flags.Int("max-active-issues", 5, "maximum concurrently open Hive-managed findings")
	maxRepairAttempts := flags.Int("max-repair-attempts", 3, "maximum bounded model-backed repair attempts per finding")
	stateDir := flags.String("state-dir", defaultIntegratedStateDir(), "persistent Hive state directory")
	planOnly := flags.Bool("plan", false, "produce a read-only setup plan")
	start := flags.Bool("start", false, "start after the setup PR is merged and doctor is green")
	runInterval := flags.Duration("run-interval", 15*time.Minute, "persistent production scan interval")
	jsonOutput := flags.Bool("json", false, "emit machine-readable JSON")
	githubTokenEnv := flags.String("github-token-env", "HIVE_GITHUB_TOKEN", "environment variable containing GitHub token")
	githubAPIURL := flags.String("github-api-url", "", "optional GitHub Enterprise API URL")
	var providerArgs stringListFlag
	var visualArgs stringListFlag
	flags.Var(&providerArgs, "provider-arg", "repair provider launcher argument; repeatable")
	flags.Var(&visualArgs, "visual-hive-arg", "Visual Hive launcher argument before the CLI subcommand; repeatable")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if cli := os.Getenv("VISUAL_HIVE_CLI"); cli != "" && len(visualArgs) == 0 {
		visualArgs = append(visualArgs, cli)
	}
	if *repository == "" || *coverageValue == "" || *automationValue == "" {
		if !isInteractiveTerminal() {
			fmt.Fprintln(os.Stderr, "--repo, --coverage, and --automation are required in noninteractive mode")
			return 2
		}
		reader := bufio.NewReader(os.Stdin)
		if *repository == "" {
			*repository = prompt(reader, "Repository (owner/name)")
		}
		if *coverageValue == "" {
			*coverageValue = prompt(reader, "Coverage depth (essential, standard, comprehensive, custom)")
		}
		if *automationValue == "" {
			*automationValue = prompt(reader, "Automation authority (advisory, issues, repair-pr, auto-merge)")
		}
	}
	coverage := integrated.Coverage(strings.ToLower(strings.TrimSpace(*coverageValue)))
	automationMode := integrated.Automation(strings.ToLower(strings.TrimSpace(*automationValue)))
	token := resolveGitHubToken(*githubTokenEnv)
	var client *hivegithub.Client
	if token != "" {
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		client = hivegithub.NewClient(token, "", nil, logger, *githubAPIURL)
	}
	if !*planOnly {
		if client == nil {
			fmt.Fprintf(os.Stderr, "GitHub authorization is required. Set %s or sign in once with gh auth login.\n", *githubTokenEnv)
			return 2
		}
	}
	if !*planOnly && *visualHive {
		resolvedCommand, resolvedArgs, resolveErr := resolveVisualHiveLauncher(*visualCommand, visualArgs, *visualHome)
		if resolveErr != nil {
			fmt.Fprintln(os.Stderr, "setup failed:", resolveErr)
			return 2
		}
		*visualCommand, visualArgs = resolvedCommand, resolvedArgs
		if len(visualArgs) > 0 && filepath.Base(visualArgs[0]) == "visual-hive.mjs" {
			manifest, manifestErr := integrated.ValidateVisualHiveRelease(visualArgs[0], *visualRef)
			if manifestErr != nil {
				fmt.Fprintln(os.Stderr, "setup failed:", manifestErr)
				return 2
			}
			if *visualRef == "" {
				*visualRef = manifest.GitCommit
			}
		}
	}
	if !*planOnly {
		providerCtx, providerCancel := context.WithTimeout(context.Background(), 60*time.Second)
		resolvedProviderCommand, providerErr := resolveIntegratedProvider(providerCtx, *provider, *providerCommand, providerArgs)
		providerCancel()
		if providerErr != nil {
			fmt.Fprintln(os.Stderr, "setup failed:", providerErr)
			return 2
		}
		*providerCommand = resolvedProviderCommand
	}
	acmm := acmmForIntegratedAutomation(automationMode)
	mode := automationModeForIntegrated(automationMode)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	result, err := integrated.RunSetup(ctx, integrated.SetupOptions{
		Repository: *repository, Coverage: coverage, Automation: automationMode, Provider: *provider,
		ProviderCommand: *providerCommand, ProviderArgs: append([]string(nil), providerArgs...),
		VisualHive: *visualHive, StateDir: *stateDir, Apply: !*planOnly, Start: *start,
		VisualHiveCommand: *visualCommand, VisualHiveArgs: append([]string(nil), visualArgs...),
		VisualHiveRepo: *visualRepo, VisualHiveRef: *visualRef, GitHub: client,
		MaxActiveIssues:   *maxActiveIssues,
		MaxRepairAttempts: *maxRepairAttempts,
		Policy:            automation.Policy{ACMMLevel: acmm, Mode: mode, AllowedRepositories: []string{*repository}, MaxRepairAttempts: *maxRepairAttempts},
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "setup failed:", err)
		return 1
	}
	if *start && result.Applied {
		if _, startErr := ensureIntegratedDaemonStarted(*stateDir, *runInterval); startErr != nil {
			fmt.Fprintln(os.Stderr, "setup applied but persistent startup failed:", startErr)
			return 1
		}
	}
	if *jsonOutput {
		return encodeJSON(result)
	}
	if result.Applied {
		fmt.Printf("Setup PR ready: %s\nPersistent state: %s\n", result.PRURL, *stateDir)
		if *start {
			fmt.Println("Persistent Hive started; it will retry safely until the setup PR is merged and production gates are ready.")
		}
	} else {
		fmt.Printf("Setup plan for %s: coverage=%s automation=%s ACMM=L%d\n", result.Plan.Repository, result.Plan.Coverage, result.Plan.Automation, result.Plan.ACMMLevel)
	}
	return 0
}

func runIntegratedStatus(args []string) int {
	flags := flag.NewFlagSet("hive status", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	stateDir := flags.String("state-dir", defaultIntegratedStateDir(), "persistent Hive state directory")
	jsonOutput := flags.Bool("json", false, "emit machine-readable JSON")
	githubTokenEnv := flags.String("github-token-env", "HIVE_GITHUB_TOKEN", "environment variable containing GitHub token")
	githubAPIURL := flags.String("github-api-url", "", "optional GitHub Enterprise API URL")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	store, err := integrated.NewStore(filepath.Join(*stateDir, "integrated"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	config, err := store.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "Hive is not set up:", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	var client *hivegithub.Client
	if token := resolveGitHubToken(*githubTokenEnv); token != "" {
		client = hivegithub.NewClient(token, "", nil, slog.New(slog.NewTextHandler(io.Discard, nil)), *githubAPIURL)
	}
	liveChecks := liveRepositoryChecks(ctx, client, config)
	runtimeOK, runtimeMessage := validateVisualHiveLauncher(config)
	liveChecks = append(liveChecks, doctorCheck{Name: "visual_hive_runtime", OK: runtimeOK, Message: runtimeMessage})
	ready := !config.Paused && len(config.VisualHiveRef) == 40
	for _, check := range liveChecks {
		ready = ready && check.OK
	}
	provider := repair.CodexProvider{Command: config.ProviderCommand, Prefix: config.ProviderArgs}
	providerCtx, providerCancel := context.WithTimeout(ctx, 45*time.Second)
	providerErr := provider.Health(providerCtx)
	providerCancel()
	ready = ready && providerErr == nil
	status := map[string]any{
		"schema_version": "hive.status.v1", "config": config, "paused": config.Paused, "production_ready": ready,
		"readiness_checks": liveChecks, "provider_ready": providerErr == nil, "provider_message": errorOr(providerErr, "provider authenticated"),
	}
	status["daemon"] = readIntegratedDaemonStatus(*stateDir)
	lifecyclePath := filepath.Join(*stateDir, "visual-hive", "visual-hive-lifecycle.json")
	if _, statErr := os.Stat(lifecyclePath); statErr == nil {
		if lifecycle, lifecycleErr := visualhive.NewLifecycleStore(filepath.Dir(lifecyclePath)); lifecycleErr == nil {
			snapshot := lifecycle.Snapshot()
			counts := map[visualhive.LifecycleStatus]int{}
			items := []map[string]any{}
			for _, finding := range snapshot.Findings {
				if finding == nil {
					continue
				}
				counts[finding.Status]++
				items = append(items, map[string]any{"fingerprint": finding.RepositoryFingerprint, "status": finding.Status, "issue_url": finding.IssueURL, "pr_url": finding.PRURL, "repair_attempts": finding.RepairAttempts, "human_review_required": finding.HumanReviewRequired})
			}
			status["lifecycle"] = map[string]any{"counts": counts, "findings": items, "pending_outbox": len(lifecycle.PendingOutbox())}
		}
	}
	repairPath := filepath.Join(*stateDir, "repair", "repair-worker-state.json")
	if _, statErr := os.Stat(repairPath); statErr == nil {
		if repairState, repairErr := repair.NewStore(filepath.Dir(repairPath)); repairErr == nil {
			status["repairs"] = repairState.Snapshot()
		}
	}
	if *jsonOutput {
		return encodeJSON(status)
	}
	fmt.Printf("%s: coverage=%s automation=%s ACMM=L%d paused=%t setup_pr=%s\n", config.Repository, config.Coverage, config.Automation, config.ACMMLevel, config.Paused, config.SetupPRURL)
	return 0
}

type doctorCheck struct {
	Name    string `json:"name"`
	OK      bool   `json:"ok"`
	Message string `json:"message"`
}

func runIntegratedDoctor(args []string) int {
	flags := flag.NewFlagSet("hive doctor", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	stateDir := flags.String("state-dir", defaultIntegratedStateDir(), "persistent Hive state directory")
	jsonOutput := flags.Bool("json", false, "emit machine-readable JSON")
	githubTokenEnv := flags.String("github-token-env", "HIVE_GITHUB_TOKEN", "environment variable containing GitHub token")
	githubAPIURL := flags.String("github-api-url", "", "optional GitHub Enterprise API URL")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	checks := []doctorCheck{}
	store, storeErr := integrated.NewStore(filepath.Join(*stateDir, "integrated"))
	config, configErr := integrated.Config{}, storeErr
	if storeErr == nil {
		config, configErr = store.Load()
	}
	checks = append(checks, doctorCheck{Name: "config", OK: configErr == nil, Message: errorOr(configErr, "persistent config loaded")})
	if configErr == nil {
		_, gitErr := os.Stat(filepath.Join(config.CheckoutDir, ".git"))
		checks = append(checks, doctorCheck{Name: "checkout", OK: gitErr == nil, Message: errorOr(gitErr, "managed checkout is present")})
		immutable := len(config.VisualHiveRef) == 40
		checks = append(checks, doctorCheck{Name: "visual_hive_pin", OK: immutable, Message: ternary(immutable, "Visual Hive is pinned to an immutable commit", "Visual Hive ref is not immutable")})
		runtimeOK, runtimeMessage := validateVisualHiveLauncher(config)
		checks = append(checks, doctorCheck{Name: "visual_hive_runtime", OK: runtimeOK, Message: runtimeMessage})
		provider := repair.CodexProvider{Command: config.ProviderCommand, Prefix: config.ProviderArgs}
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		providerErr := provider.Health(ctx)
		cancel()
		checks = append(checks, doctorCheck{Name: "provider", OK: providerErr == nil, Message: errorOr(providerErr, "Codex provider is authenticated")})
		var client *hivegithub.Client
		if token := resolveGitHubToken(*githubTokenEnv); token != "" {
			client = hivegithub.NewClient(token, "", nil, slog.New(slog.NewTextHandler(io.Discard, nil)), *githubAPIURL)
		}
		ctx, cancelLive := context.WithTimeout(context.Background(), 45*time.Second)
		checks = append(checks, liveRepositoryChecks(ctx, client, config)...)
		cancelLive()
	}
	ready := true
	for _, check := range checks {
		ready = ready && check.OK
	}
	output := map[string]any{"schema_version": "hive.doctor.v1", "production_ready": ready, "checks": checks}
	if *jsonOutput {
		_ = encodeJSON(output)
	} else {
		for _, check := range checks {
			fmt.Printf("[%s] %s: %s\n", ternary(check.OK, "ok", "fail"), check.Name, check.Message)
		}
	}
	if !ready {
		return 1
	}
	return 0
}

func resolveVisualHiveLauncher(command string, args []string, home string) (string, []string, error) {
	command = strings.TrimSpace(command)
	if command != "" {
		return command, append([]string(nil), args...), nil
	}
	if len(args) > 0 {
		node, err := exec.LookPath("node")
		if err != nil {
			return "", nil, fmt.Errorf("VISUAL_HIVE_CLI requires Node 22 or --visual-hive-command")
		}
		return node, append([]string(nil), args...), nil
	}

	homes := []string{}
	if strings.TrimSpace(home) != "" {
		homes = append(homes, home)
	}
	if executable, err := os.Executable(); err == nil {
		homes = append(homes, filepath.Join(filepath.Dir(executable), "visual-hive"))
	}
	for _, candidate := range homes {
		absolute, err := filepath.Abs(candidate)
		if err != nil {
			continue
		}
		cli := filepath.Join(absolute, "visual-hive.mjs")
		if info, statErr := os.Stat(cli); statErr != nil || !info.Mode().IsRegular() {
			continue
		}
		nodeName := "node"
		if runtime.GOOS == "windows" {
			nodeName = "node.exe"
		}
		node := filepath.Join(filepath.Dir(absolute), "runtime", nodeName)
		if info, statErr := os.Stat(node); statErr == nil && info.Mode().IsRegular() {
			return node, append([]string{cli}, args...), nil
		}
		if pathNode, lookErr := exec.LookPath("node"); lookErr == nil {
			return pathNode, append([]string{cli}, args...), nil
		}
	}
	if visualHive, err := exec.LookPath("visual-hive"); err == nil {
		return visualHive, append([]string(nil), args...), nil
	}
	return "", nil, fmt.Errorf("immutable Visual Hive runtime not found; install the integrated Hive bundle or pass --visual-hive-home")
}

func resolveIntegratedProvider(ctx context.Context, provider, command string, args []string) (string, error) {
	if !strings.EqualFold(strings.TrimSpace(provider), "codex") {
		if strings.TrimSpace(command) == "" {
			return "", fmt.Errorf("provider %q requires --provider-command", provider)
		}
		return command, nil
	}
	candidates := []string{}
	if strings.TrimSpace(command) != "" {
		candidates = append(candidates, command)
	}
	if resolved, err := exec.LookPath("codex"); err == nil {
		candidates = append(candidates, resolved)
	}
	homes := []string{}
	if codexHome := strings.TrimSpace(os.Getenv("CODEX_HOME")); codexHome != "" {
		homes = append(homes, codexHome)
	}
	if home, err := os.UserHomeDir(); err == nil {
		homes = append(homes, filepath.Join(home, ".codex"))
	}
	for _, home := range homes {
		name := "codex"
		if runtime.GOOS == "windows" {
			name = "codex.exe"
		}
		candidates = append(candidates, filepath.Join(home, ".sandbox-bin", name))
	}
	seen := map[string]bool{}
	errorsSeen := []string{}
	for _, candidate := range candidates {
		candidate = filepath.Clean(candidate)
		if seen[strings.ToLower(candidate)] {
			continue
		}
		seen[strings.ToLower(candidate)] = true
		resolved := candidate
		if !filepath.IsAbs(candidate) {
			if pathValue, err := exec.LookPath(candidate); err == nil {
				resolved = pathValue
			}
		}
		if info, err := os.Stat(resolved); err != nil || !info.Mode().IsRegular() {
			continue
		}
		healthCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		err := (repair.CodexProvider{Command: resolved, Prefix: append([]string(nil), args...)}).Health(healthCtx)
		cancel()
		if err == nil {
			absolute, _ := filepath.Abs(resolved)
			return absolute, nil
		}
		errorsSeen = append(errorsSeen, fmt.Sprintf("%s: %s", resolved, err))
	}
	if len(errorsSeen) > 0 {
		return "", fmt.Errorf("no usable authenticated Codex provider was found (%s)", strings.Join(errorsSeen, "; "))
	}
	return "", fmt.Errorf("Codex provider was not found; install/authenticate Codex or pass --provider-command")
}

func validateVisualHiveLauncher(config integrated.Config) (bool, string) {
	command := config.VisualHiveCommand
	if !filepath.IsAbs(command) {
		resolved, err := exec.LookPath(command)
		if err != nil {
			return false, "Visual Hive launcher is unavailable"
		}
		command = resolved
	}
	if info, err := os.Stat(command); err != nil || !info.Mode().IsRegular() {
		return false, "Visual Hive launcher is unavailable"
	}
	if len(config.VisualHiveArgs) == 0 || filepath.Base(config.VisualHiveCommand) == "visual-hive" || filepath.Base(config.VisualHiveCommand) == "visual-hive.exe" {
		return true, "Visual Hive launcher is available"
	}
	entrypoint := config.VisualHiveArgs[0]
	if info, err := os.Stat(entrypoint); err != nil || !info.Mode().IsRegular() {
		return false, "Visual Hive release entrypoint is unavailable"
	}
	return validateVisualHiveRelease(entrypoint, config.VisualHiveRef)
}

func validateVisualHiveRelease(entrypoint, expectedCommit string) (bool, string) {
	if _, err := integrated.ValidateVisualHiveRelease(entrypoint, expectedCommit); err != nil {
		return false, err.Error()
	}
	return true, "Visual Hive release manifest matches the immutable pin"
}

func liveRepositoryChecks(ctx context.Context, client *hivegithub.Client, config integrated.Config) []doctorCheck {
	if client == nil || client.GoGitHub() == nil {
		return []doctorCheck{{Name: "github_auth", OK: false, Message: "GitHub authorization is required"}}
	}
	owner, repo, ok := strings.Cut(config.Repository, "/")
	if !ok {
		return []doctorCheck{{Name: "github_auth", OK: false, Message: "configured repository is invalid"}}
	}
	checks := []doctorCheck{}
	metadata, _, repoErr := client.GoGitHub().Repositories.Get(ctx, owner, repo)
	checks = append(checks, doctorCheck{Name: "github_auth", OK: repoErr == nil, Message: errorOr(repoErr, "GitHub repository access verified")})
	if repoErr != nil {
		return checks
	}
	if metadata.GetID() > 0 && config.RepositoryID != "" && fmt.Sprintf("%d", metadata.GetID()) != config.RepositoryID {
		checks = append(checks, doctorCheck{Name: "repository_identity", OK: false, Message: "GitHub repository ID no longer matches persistent configuration"})
	} else {
		checks = append(checks, doctorCheck{Name: "repository_identity", OK: true, Message: "repository identity matches"})
	}
	setupMerged := false
	setupMessage := "setup PR is missing"
	if config.SetupPRNumber > 0 {
		pull, _, pullErr := client.GoGitHub().PullRequests.Get(ctx, owner, repo, config.SetupPRNumber)
		setupMerged = pullErr == nil && pull.GetMerged()
		setupMessage = errorOr(pullErr, ternary(setupMerged, "setup PR is merged", "setup PR has not been merged"))
	}
	checks = append(checks, doctorCheck{Name: "setup_pr_merged", OK: setupMerged, Message: setupMessage})
	content, _, _, workflowErr := client.GoGitHub().Repositories.GetContents(ctx, owner, repo, ".github/workflows/hive-visual-hive.yml", &gh.RepositoryContentGetOptions{Ref: config.DefaultBranch})
	checks = append(checks, doctorCheck{Name: "workflow_installed", OK: workflowErr == nil && content != nil, Message: errorOr(workflowErr, "production workflow is installed on the target branch")})
	visualRefErr := integrated.VerifyVisualHiveCommit(ctx, client, config.VisualHiveRepo, config.VisualHiveRef)
	checks = append(checks, doctorCheck{Name: "visual_hive_ref_exists", OK: visualRefErr == nil, Message: errorOr(visualRefErr, "Visual Hive pin resolves to the exact remote commit")})
	if config.Automation == integrated.AutomationAutoMerge {
		protection, protectionErr := client.BranchProtection(ctx, config.Repository, config.DefaultBranch)
		protected := protectionErr == nil && protection.Enabled && len(protection.RequiredChecks) > 0
		message := errorOr(protectionErr, fmt.Sprintf("protection=%t required_checks=%v required_reviews=%d", protection.Enabled, protection.RequiredChecks, protection.RequiredReviews))
		checks = append(checks, doctorCheck{Name: "branch_protection", OK: protected, Message: message})
	}
	return checks
}

func runIntegratedPause(command string, args []string) int {
	flags := flag.NewFlagSet("hive "+command, flag.ContinueOnError)
	stateDir := flags.String("state-dir", defaultIntegratedStateDir(), "persistent Hive state directory")
	jsonOutput := flags.Bool("json", false, "emit machine-readable JSON")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	store, err := integrated.NewStore(filepath.Join(*stateDir, "integrated"))
	if err != nil {
		return 1
	}
	config, err := store.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	config.Paused = command == "pause"
	if err := store.Save(config); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	store.Audit(integrated.AuditEntry{Action: command, Allowed: true, Repository: config.Repository})
	if *jsonOutput {
		return encodeJSON(map[string]any{"repository": config.Repository, "paused": config.Paused})
	}
	fmt.Printf("Hive automation for %s is %s.\n", config.Repository, ternary(config.Paused, "paused", "active"))
	return 0
}

func runIntegratedSetting(command string, args []string) int {
	flags := flag.NewFlagSet("hive "+command, flag.ContinueOnError)
	stateDir := flags.String("state-dir", defaultIntegratedStateDir(), "persistent Hive state directory")
	value := flags.String("value", "", "new setting value")
	jsonOutput := flags.Bool("json", false, "emit machine-readable JSON")
	if err := flags.Parse(args); err != nil || *value == "" {
		fmt.Fprintln(os.Stderr, "--value is required")
		return 2
	}
	store, err := integrated.NewStore(filepath.Join(*stateDir, "integrated"))
	if err != nil {
		return 1
	}
	config, err := store.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if command == "set-coverage" {
		candidate := integrated.Coverage(*value)
		if candidate != integrated.CoverageEssential && candidate != integrated.CoverageStandard && candidate != integrated.CoverageComprehensive && candidate != integrated.CoverageCustom {
			fmt.Fprintln(os.Stderr, "invalid coverage")
			return 2
		}
		config.Coverage = candidate
	} else {
		candidate := integrated.Automation(*value)
		if candidate != integrated.AutomationAdvisory && candidate != integrated.AutomationIssues && candidate != integrated.AutomationRepairPR && candidate != integrated.AutomationAutoMerge {
			fmt.Fprintln(os.Stderr, "invalid automation")
			return 2
		}
		config.Automation, config.ACMMLevel = candidate, acmmForIntegratedAutomation(candidate)
	}
	if err := store.Save(config); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	store.Audit(integrated.AuditEntry{Action: command, Allowed: true, Repository: config.Repository, Detail: *value})
	if *jsonOutput {
		return encodeJSON(config)
	}
	fmt.Printf("%s updated to %s for %s.\n", command, *value, config.Repository)
	return 0
}

func runIntegratedIssueLimit(args []string) int {
	flags := flag.NewFlagSet("hive set-issue-limit", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	stateDir := flags.String("state-dir", defaultIntegratedStateDir(), "persistent Hive state directory")
	value := flags.Int("value", 0, "maximum concurrently open Hive-managed findings (1-100)")
	jsonOutput := flags.Bool("json", false, "emit machine-readable JSON")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *value < 1 || *value > 100 {
		fmt.Fprintln(os.Stderr, "--value must be from 1 through 100")
		return 2
	}
	store, err := integrated.NewStore(filepath.Join(*stateDir, "integrated"))
	if err != nil {
		return 1
	}
	config, err := store.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	config.MaxActiveIssues = *value
	if err := store.Save(config); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	store.Audit(integrated.AuditEntry{Action: "set-issue-limit", Allowed: true, Repository: config.Repository, Detail: fmt.Sprint(*value)})
	if *jsonOutput {
		return encodeJSON(config)
	}
	fmt.Printf("Active issue limit updated to %d for %s.\n", *value, config.Repository)
	return 0
}

func runIntegratedRetryLimit(args []string) int {
	flags := flag.NewFlagSet("hive set-retry-limit", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	stateDir := flags.String("state-dir", defaultIntegratedStateDir(), "persistent Hive state directory")
	value := flags.Int("value", 0, "maximum bounded model-backed repair attempts per finding (1-10)")
	jsonOutput := flags.Bool("json", false, "emit machine-readable JSON")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *value < 1 || *value > 10 {
		fmt.Fprintln(os.Stderr, "--value must be from 1 through 10")
		return 2
	}
	store, err := integrated.NewStore(filepath.Join(*stateDir, "integrated"))
	if err != nil {
		return 1
	}
	config, err := store.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	config.MaxRepairAttempts = *value
	if err := store.Save(config); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	store.Audit(integrated.AuditEntry{Action: "set-retry-limit", Allowed: true, Repository: config.Repository, Detail: fmt.Sprint(*value)})
	if *jsonOutput {
		return encodeJSON(config)
	}
	fmt.Printf("Repair attempt limit updated to %d for %s.\n", *value, config.Repository)
	return 0
}

func resolveGitHubToken(envName string) string {
	if token := strings.TrimSpace(os.Getenv(envName)); token != "" {
		return token
	}
	command := exec.Command("gh", "auth", "token")
	command.Stderr = io.Discard
	output, err := command.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(output))
}

func defaultIntegratedStateDir() string {
	if value := os.Getenv("HIVE_STATE_DIR"); value != "" {
		return value
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".hive"
	}
	return filepath.Join(home, ".hive")
}

func isInteractiveTerminal() bool {
	info, err := os.Stdin.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

func prompt(reader *bufio.Reader, label string) string {
	fmt.Fprintf(os.Stderr, "%s: ", label)
	value, _ := reader.ReadString('\n')
	return strings.TrimSpace(value)
}

func acmmForIntegratedAutomation(value integrated.Automation) int {
	switch value {
	case integrated.AutomationAdvisory:
		return 2
	case integrated.AutomationIssues:
		return 4
	case integrated.AutomationRepairPR:
		return 5
	case integrated.AutomationAutoMerge:
		return 6
	default:
		return 1
	}
}

func automationModeForIntegrated(value integrated.Automation) automation.Mode {
	switch value {
	case integrated.AutomationIssues:
		return automation.ModeIssues
	case integrated.AutomationRepairPR:
		return automation.ModeRepairPR
	case integrated.AutomationAutoMerge:
		return automation.ModeAutoMerge
	default:
		return automation.ModeAdvisory
	}
}

func valueOrEnv(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func errorOr(err error, success string) string {
	if err != nil {
		return err.Error()
	}
	return success
}

func ternary[T any](condition bool, yes, no T) T {
	if condition {
		return yes
	}
	return no
}
