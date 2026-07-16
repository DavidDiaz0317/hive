package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/kubestellar/hive/v2/pkg/checkpoint"
	hivegithub "github.com/kubestellar/hive/v2/pkg/github"
	"github.com/kubestellar/hive/v2/pkg/hostedcontrol"
	"github.com/kubestellar/hive/v2/pkg/hostedstate"
	"github.com/kubestellar/hive/v2/pkg/integrated"
)

const hostedRequestSchema = "hive.hosted-request-ledger.v1"

var hostedRequestPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

type hostedCommandResult struct {
	SchemaVersion string                `json:"schema_version"`
	Repository    string                `json:"repository"`
	Operation     string                `json:"operation"`
	RequestID     string                `json:"request_id"`
	StateBranch   string                `json:"state_branch"`
	StateBefore   string                `json:"state_before,omitempty"`
	StateAfter    string                `json:"state_after,omitempty"`
	Sequence      uint64                `json:"sequence"`
	Paused        bool                  `json:"paused"`
	Skipped       bool                  `json:"skipped,omitempty"`
	Duplicate     bool                  `json:"duplicate,omitempty"`
	Run           *integrated.RunResult `json:"run,omitempty"`
	Doctor        any                   `json:"doctor,omitempty"`
	Status        any                   `json:"status,omitempty"`
	Error         string                `json:"error,omitempty"`
}

type hostedRequestLedger struct {
	SchemaVersion string                `json:"schema_version"`
	Requests      []hostedRequestRecord `json:"requests"`
}

type hostedRequestRecord struct {
	ID          string    `json:"id"`
	Operation   string    `json:"operation"`
	Status      string    `json:"status"`
	StartedAt   time.Time `json:"started_at"`
	CompletedAt time.Time `json:"completed_at,omitempty"`
}

func runHostedCycleCommand(args []string) int {
	flags := flag.NewFlagSet("hive hosted-cycle", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	repository := flags.String("repo", "", "exact GitHub repository owner/name")
	repositoryID := flags.String("repository-id", "", "exact numeric GitHub repository ID")
	stateBranch := flags.String("state-branch", "", "dedicated hosted state branch")
	stateCheckout := flags.String("state-dir", "", "checked-out hosted state branch")
	targetCheckout := flags.String("checkout-dir", "", "ephemeral default-branch target checkout")
	stateKeyEnv := flags.String("state-key-env", "HIVE_HOSTED_STATE_KEY", "environment containing the hosted state key")
	operation := flags.String("operation", "cycle", "cycle, status, doctor, pause, resume, or recover")
	requestID := flags.String("request-id", "", "bounded idempotency key")
	restoreVersion := flags.String("restore-release-version", "", "exact predecessor release version for one transition")
	restoreHiveCommit := flags.String("restore-hive-commit", "", "exact predecessor Hive commit for one transition")
	restoreVisualCommit := flags.String("restore-visual-commit", "", "exact predecessor Visual Hive commit for one transition")
	restoreManifestSHA256 := flags.String("restore-manifest-sha256", "", "exact predecessor manifest digest for one transition")
	jsonOutput := flags.Bool("json", false, "emit one machine-readable JSON document")
	if err := parseExactFlags(flags, args); err != nil {
		return 2
	}
	result := hostedCommandResult{SchemaVersion: "hive.hosted-cycle.v1", Repository: *repository, Operation: *operation, RequestID: *requestID, StateBranch: *stateBranch}
	fail := func(err error) int {
		result.Error = err.Error()
		_ = encodeJSON(result)
		return 1
	}
	if !*jsonOutput {
		return fail(errors.New("hosted-cycle is an internal JSON-only controller command"))
	}
	if err := validateHostedInvocation(*repository, *repositoryID, *stateBranch, *stateKeyEnv, *operation, *requestID); err != nil {
		return fail(err)
	}
	identity, err := loadInstalledReleaseIdentity()
	if err != nil {
		return fail(err)
	}
	restoreRelease, restoreManifest, err := hostedRestoreRelease(*restoreVersion, *restoreHiveCommit, *restoreVisualCommit, *restoreManifestSHA256, identity)
	if err != nil {
		return fail(err)
	}
	numericRepositoryID, _ := strconv.ParseInt(*repositoryID, 10, 64)
	runID, _ := strconv.ParseUint(os.Getenv("GITHUB_RUN_ID"), 10, 64)
	attempt, _ := strconv.ParseUint(os.Getenv("GITHUB_RUN_ATTEMPT"), 10, 64)
	controller := hostedstate.ControllerIdentity{
		RunID: runID, Attempt: attempt, Event: os.Getenv("GITHUB_EVENT_NAME"),
		WorkflowPath: integrated.HostedControllerWorkflowPath,
		WorkflowSHA:  strings.ToLower(os.Getenv("GITHUB_SHA")), DefaultHeadSHA: strings.ToLower(os.Getenv("GITHUB_SHA")),
	}
	runnerTemp := strings.TrimSpace(os.Getenv("RUNNER_TEMP"))
	if runnerTemp == "" {
		return fail(errors.New("GitHub runner temporary directory is unavailable"))
	}
	runtimeRoot, err := os.MkdirTemp(runnerTemp, "hive-hosted-runtime-")
	if err != nil {
		return fail(fmt.Errorf("create hosted runtime state: %w", err))
	}
	defer os.RemoveAll(runtimeRoot)
	if err := os.Chmod(runtimeRoot, 0o700); err != nil {
		return fail(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 58*time.Minute)
	defer cancel()
	if err := validateHostedTargetCheckout(ctx, *targetCheckout, os.Getenv("GITHUB_SHA")); err != nil {
		return fail(err)
	}
	manager, err := hostedcontrol.Open(ctx, hostedcontrol.Config{
		CheckoutDir: *stateCheckout, RuntimeRoot: runtimeRoot, StateBranch: *stateBranch,
		Secret: []byte(os.Getenv(*stateKeyEnv)), Repository: hostedstate.RepositoryIdentity{FullName: *repository, ID: numericRepositoryID},
		Release:        hostedstate.ReleaseIdentity{Version: identity.Version, HiveCommit: identity.HiveCommit, VisualHiveCommit: identity.VisualHiveCommit},
		RestoreRelease: restoreRelease,
		Controller:     controller, TrustedWorkflowPath: integrated.HostedControllerWorkflowPath,
	})
	if err != nil {
		return fail(fmt.Errorf("open authenticated hosted state: %w", err))
	}
	result.StateBefore = manager.HeadSHA()
	installRoot := filepath.Dir(identity.ManifestPath)
	node := filepath.Join(installRoot, "runtime", "node")
	visual := filepath.Join(installRoot, "visual-hive", "visual-hive.mjs")
	requireProvider := *operation == "cycle" || *operation == "recover"
	config, err := integrated.PrepareHostedRuntime(ctx, runtimeRoot, *targetCheckout, node, []string{visual}, strings.TrimSpace(os.Getenv("HIVE_HOSTED_CODEX")), nil, requireProvider,
		integrated.HostedReleaseIdentity{Version: identity.Version, HiveCommit: identity.HiveCommit, VisualHiveCommit: identity.VisualHiveCommit, DistributionManifestSHA256: identity.ManifestSHA256}, restoreManifest)
	if err != nil {
		return fail(err)
	}
	if config.Repository != *repository || config.RepositoryID != *repositoryID || config.ExecutionMode != integrated.ExecutionHosted ||
		config.HostedStateBranch != *stateBranch || config.HiveReleaseVersion != identity.Version || config.HiveCommit != identity.HiveCommit ||
		config.VisualHiveRef != identity.VisualHiveCommit || config.DistributionManifestSHA256 != identity.ManifestSHA256 {
		return fail(errors.New("signed hosted policy does not match controller repository and immutable release identity"))
	}
	if err := validateHostedPolicyContext(config); err != nil {
		return fail(err)
	}
	// Every runner receipt carries the pause state restored from authenticated
	// hosted state. This remains explicit when false so external status never
	// has to infer "active" from a missing JSON field or stale desktop config.
	result.Paused = config.Paused
	mutating := *operation == "cycle" || *operation == "pause" || *operation == "resume" || *operation == "recover"
	if mutating {
		duplicate, ledgerErr := beginHostedRequest(runtimeRoot, *requestID, *operation)
		if ledgerErr != nil {
			return fail(ledgerErr)
		}
		if duplicate {
			result.Duplicate, result.Skipped, result.StateAfter, result.Sequence = true, true, manager.HeadSHA(), manager.Sequence()
			return encodeJSON(result)
		}
		if err := manager.Checkpoint(ctx, "accept request "+*requestID); err != nil {
			return fail(err)
		}
	}
	operationCtx := checkpoint.WithCallback(ctx, manager.Callback)
	var operationErr error
	if *operation == "cycle" || *operation == "recover" {
		removeGitCredential, credentialErr := installHostedGitTransportCredential(operationCtx, *targetCheckout, os.Getenv("HIVE_GITHUB_TOKEN"))
		if credentialErr != nil {
			return fail(credentialErr)
		}
		defer removeGitCredential()
	}
	switch *operation {
	case "cycle", "recover":
		if config.Paused && *operation == "cycle" {
			result.Paused, result.Skipped = true, true
			break
		}
		run, runErr := integrated.RunOnce(operationCtx, integrated.RunOptions{StateDir: runtimeRoot, Timeout: 50 * time.Minute, GitHub: hostedGitHubClient()})
		result.Run, operationErr = &run, runErr
	case "pause", "resume":
		updated, pauseErr := integrated.SetPaused(operationCtx, runtimeRoot, *operation == "pause")
		result.Paused, operationErr = updated.Paused, pauseErr
	case "doctor":
		checks := collectIntegratedDoctorChecks(runtimeRoot, "HIVE_GITHUB_TOKEN", "", false)
		result.Doctor = map[string]any{"schema_version": "hive.doctor.v1", "production_ready": doctorChecksReady(checks), "checks": checks}
	case "status":
		result.Status = map[string]any{"schema_version": "hive.status.v1", "repository": config.Repository, "config": config, "paused": config.Paused, "execution_mode": config.ExecutionMode}
	default:
		operationErr = fmt.Errorf("unsupported hosted operation %q", *operation)
	}
	if mutating {
		if operationErr == nil {
			operationErr = completeHostedRequest(runtimeRoot, *requestID, *operation)
		}
		checkpointCtx, checkpointCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		checkpointErr := manager.Checkpoint(checkpointCtx, "finish request "+*requestID)
		checkpointCancel()
		if operationErr == nil {
			operationErr = checkpointErr
		} else if checkpointErr != nil {
			operationErr = fmt.Errorf("%v; final hosted checkpoint failed: %w", operationErr, checkpointErr)
		}
	}
	result.StateAfter, result.Sequence = manager.HeadSHA(), manager.Sequence()
	if operationErr != nil {
		return fail(operationErr)
	}
	return encodeJSON(result)
}

func validateHostedTargetCheckout(ctx context.Context, checkout, expectedSHA string) error {
	checkout = strings.TrimSpace(checkout)
	expectedSHA = strings.ToLower(strings.TrimSpace(expectedSHA))
	if checkout == "" || !regexp.MustCompile(`^[a-f0-9]{40}$`).MatchString(expectedSHA) {
		return errors.New("hosted target checkout identity is invalid")
	}
	command := exec.CommandContext(ctx, "git", "-C", checkout, "rev-parse", "HEAD")
	command.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	output, err := command.Output()
	if err != nil || strings.ToLower(strings.TrimSpace(string(output))) != expectedSHA {
		return errors.New("hosted target checkout does not match the exact workflow SHA")
	}
	return nil
}

func hostedRestoreRelease(version, hiveCommit, visualCommit, manifestSHA256 string, current installedReleaseIdentity) (*hostedstate.ReleaseIdentity, *integrated.HostedReleaseIdentity, error) {
	values := []string{strings.TrimSpace(version), strings.ToLower(strings.TrimSpace(hiveCommit)), strings.ToLower(strings.TrimSpace(visualCommit)), strings.ToLower(strings.TrimSpace(manifestSHA256))}
	empty := 0
	for _, value := range values {
		if value == "" {
			empty++
		}
	}
	if empty == len(values) {
		return nil, nil, nil
	}
	if empty != 0 || !regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+-integrated\.[0-9]+$`).MatchString(values[0]) ||
		!regexp.MustCompile(`^[a-f0-9]{40}$`).MatchString(values[1]) || !regexp.MustCompile(`^[a-f0-9]{40}$`).MatchString(values[2]) ||
		!regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(values[3]) {
		return nil, nil, errors.New("hosted predecessor release identity is incomplete or invalid")
	}
	if values[0] == current.Version || values[1] == current.HiveCommit {
		return nil, nil, errors.New("hosted predecessor release must differ from the installed release")
	}
	restore := &hostedstate.ReleaseIdentity{Version: values[0], HiveCommit: values[1], VisualHiveCommit: values[2]}
	manifest := &integrated.HostedReleaseIdentity{Version: values[0], HiveCommit: values[1], VisualHiveCommit: values[2], DistributionManifestSHA256: values[3]}
	return restore, manifest, nil
}

func validateHostedInvocation(repository, repositoryID, stateBranch, stateKeyEnv, operation, requestID string) error {
	if os.Getenv("GITHUB_ACTIONS") != "true" || os.Getenv("GITHUB_REPOSITORY") != repository || os.Getenv("GITHUB_REF") == "" ||
		os.Getenv("GITHUB_SHA") == "" || os.Getenv("GITHUB_RUN_ID") == "" || os.Getenv("GITHUB_RUN_ATTEMPT") == "" {
		return errors.New("hosted-cycle requires the bound GitHub Actions controller context")
	}
	if !regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`).MatchString(repository) || !regexp.MustCompile(`^[1-9][0-9]{0,19}$`).MatchString(repositoryID) ||
		!strings.HasPrefix(stateBranch, "hive/state-") || strings.Contains(stateBranch, "..") {
		return errors.New("hosted-cycle repository or state-branch identity is invalid")
	}
	event := os.Getenv("GITHUB_EVENT_NAME")
	if event != "schedule" && event != "workflow_dispatch" {
		return errors.New("hosted-cycle event is not trusted")
	}
	workflowRef := os.Getenv("GITHUB_WORKFLOW_REF")
	if !strings.HasPrefix(workflowRef, repository+"/"+integrated.HostedControllerWorkflowPath+"@refs/heads/") {
		return errors.New("hosted-cycle workflow ref is not the managed controller on a branch")
	}
	if !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(strings.ToLower(os.Getenv("GITHUB_SHA"))) {
		return errors.New("hosted-cycle workflow SHA is invalid")
	}
	runID, runErr := strconv.ParseUint(os.Getenv("GITHUB_RUN_ID"), 10, 64)
	attempt, attemptErr := strconv.ParseUint(os.Getenv("GITHUB_RUN_ATTEMPT"), 10, 64)
	if runErr != nil || attemptErr != nil || runID == 0 || attempt == 0 {
		return errors.New("hosted-cycle workflow run identity is invalid")
	}
	if !regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`).MatchString(stateKeyEnv) || len(os.Getenv(stateKeyEnv)) < 32 {
		return errors.New("hosted state authentication secret is missing or invalid")
	}
	if strings.TrimSpace(os.Getenv("HIVE_GITHUB_TOKEN")) == "" {
		return errors.New("hosted-cycle GitHub lifecycle token is missing")
	}
	if !hostedRequestPattern.MatchString(requestID) {
		return errors.New("hosted request ID is invalid")
	}
	allowed := map[string]bool{"cycle": true, "status": true, "doctor": true, "pause": true, "resume": true, "recover": true}
	if !allowed[operation] || event == "schedule" && operation != "cycle" {
		return errors.New("hosted operation is not permitted for this event")
	}
	return nil
}

func validateHostedPolicyContext(config integrated.Config) error {
	branch := strings.TrimSpace(config.DefaultBranch)
	if branch == "" || os.Getenv("GITHUB_REF") != "refs/heads/"+branch {
		return errors.New("hosted-cycle ref does not match the signed default branch")
	}
	wantWorkflowRef := config.Repository + "/" + integrated.HostedControllerWorkflowPath + "@refs/heads/" + branch
	if os.Getenv("GITHUB_WORKFLOW_REF") != wantWorkflowRef {
		return errors.New("hosted-cycle workflow ref does not match the signed managed controller and default branch")
	}
	return nil
}

func hostedGitHubClient() *hivegithub.Client {
	return hivegithub.NewClient(os.Getenv("HIVE_GITHUB_TOKEN"), "", nil, slog.New(slog.NewTextHandler(io.Discard, nil)), "")
}

// installHostedGitTransportCredential gives only Hive's ephemeral target Git
// common directory an authenticated transport header. It does not affect the
// separately checked-out state branch, and target code is never executed.
func installHostedGitTransportCredential(ctx context.Context, checkout, token string) (func(), error) {
	token = strings.TrimSpace(token)
	checkout = strings.TrimSpace(checkout)
	if token == "" || checkout == "" {
		return nil, errors.New("hosted Git transport token is missing")
	}
	const key = "http.https://github.com/.extraheader"
	query := exec.CommandContext(ctx, "git", "-C", checkout, "config", "--local", "--get-all", key)
	if output, err := query.Output(); err == nil || len(output) != 0 {
		return nil, errors.New("hosted target checkout already contains a Git transport credential")
	} else if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 1 {
		return nil, errors.New("inspect hosted target Git transport")
	}
	header := "AUTHORIZATION: basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token))
	install := exec.CommandContext(ctx, "git", "-C", checkout, "config", "--local", "--add", key, header)
	if err := install.Run(); err != nil {
		return nil, errors.New("configure ephemeral hosted Git transport")
	}
	return func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = exec.CommandContext(cleanupCtx, "git", "-C", checkout, "config", "--local", "--unset-all", key).Run()
	}, nil
}

func beginHostedRequest(root, id, operation string) (bool, error) {
	ledger, err := loadHostedRequestLedger(root)
	if err != nil {
		return false, err
	}
	for _, record := range ledger.Requests {
		if record.ID == id {
			if record.Operation != operation {
				return false, errors.New("hosted request ID was already bound to a different operation")
			}
			return record.Status == "completed", nil
		}
	}
	ledger.Requests = append(ledger.Requests, hostedRequestRecord{ID: id, Operation: operation, Status: "started", StartedAt: time.Now().UTC()})
	if len(ledger.Requests) > 100 {
		ledger.Requests = append([]hostedRequestRecord(nil), ledger.Requests[len(ledger.Requests)-100:]...)
	}
	return false, saveHostedRequestLedger(root, ledger)
}

func completeHostedRequest(root, id, operation string) error {
	ledger, err := loadHostedRequestLedger(root)
	if err != nil {
		return err
	}
	for index := range ledger.Requests {
		if ledger.Requests[index].ID == id && ledger.Requests[index].Operation == operation {
			ledger.Requests[index].Status = "completed"
			ledger.Requests[index].CompletedAt = time.Now().UTC()
			return saveHostedRequestLedger(root, ledger)
		}
	}
	return errors.New("hosted request ledger lost the active request")
}

func loadHostedRequestLedger(root string) (hostedRequestLedger, error) {
	path := filepath.Join(root, "integrated", "hosted-requests.json")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return hostedRequestLedger{SchemaVersion: hostedRequestSchema}, nil
	}
	if err != nil {
		return hostedRequestLedger{}, err
	}
	var ledger hostedRequestLedger
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&ledger); err != nil || ledger.SchemaVersion != hostedRequestSchema || len(ledger.Requests) > 100 {
		return hostedRequestLedger{}, errors.New("hosted request ledger is invalid")
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return hostedRequestLedger{}, errors.New("hosted request ledger has trailing content")
	}
	return ledger, nil
}

func saveHostedRequestLedger(root string, ledger hostedRequestLedger) error {
	directory := filepath.Join(root, "integrated")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(ledger, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(directory, "hosted-requests.json")
	temporary, err := os.CreateTemp(directory, ".hosted-requests-*.tmp")
	if err != nil {
		return err
	}
	name := temporary.Name()
	defer os.Remove(name)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(append(data, '\n')); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}
