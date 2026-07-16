package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
	hivegithub "github.com/kubestellar/hive/v2/pkg/github"
	"github.com/kubestellar/hive/v2/pkg/hostedstatus"
	"github.com/kubestellar/hive/v2/pkg/integrated"
)

type hostedDispatchResult struct {
	SchemaVersion string `json:"schema_version"`
	Repository    string `json:"repository"`
	Workflow      string `json:"workflow"`
	Ref           string `json:"ref"`
	Operation     string `json:"operation"`
	RequestID     string `json:"request_id"`
	Accepted      bool   `json:"accepted"`
	Error         string `json:"error,omitempty"`
	RetryCommand  string `json:"retry_command,omitempty"`
}

func inspectHostedController(config integrated.Config, githubTokenEnv, githubAPIURL string) (hostedstatus.Result, error) {
	token := resolveGitHubToken(githubTokenEnv)
	if token == "" {
		return hostedstatus.Result{}, errors.New("GitHub authorization is required for hosted status")
	}
	client := hivegithub.NewClient(token, "", nil, slog.New(slog.NewTextHandler(io.Discard, nil)), githubAPIURL)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	return hostedstatus.Inspect(ctx, client.GoGitHub(), config, time.Now().UTC())
}

func runHostedStatusCommand(stateDir string, config integrated.Config, githubTokenEnv, githubAPIURL string, jsonOutput bool) int {
	controller, err := inspectHostedController(config, githubTokenEnv, githubAPIURL)
	if err != nil {
		if jsonOutput {
			_ = encodeJSON(map[string]any{"schema_version": "hive.status.v1", "state_dir": stateDir, "config": config, "production_ready": false, "error": err.Error()})
		} else {
			fmt.Println("Hosted Hive status failed:", err)
		}
		return 1
	}
	status, ready := hostedStatusDocument(stateDir, config, controller)
	if jsonOutput {
		return encodeJSON(status)
	}
	fmt.Printf("%s: hosted production_ready=%t latest_cycle=%v\n", config.Repository, ready, controller.LatestCompletedCycle)
	if !ready {
		return 1
	}
	return 0
}

func hostedStatusDocument(stateDir string, config integrated.Config, controller hostedstatus.Result) (map[string]any, bool) {
	ready := controller.ProductionReady && !controller.Paused
	return map[string]any{
		"schema_version": "hive.status.v1", "state_dir": stateDir, "config": config,
		"execution_mode": integrated.ExecutionHosted, "paused": controller.Paused, "production_ready": ready,
		"hosted_controller": controller, "daemon_ready": true, "daemon": map[string]any{"required": false, "reason": "repository-owned hosted cadence"},
	}, ready
}

func runHostedDoctorCommand(config integrated.Config, githubTokenEnv, githubAPIURL string, jsonOutput bool) int {
	controller, err := inspectHostedController(config, githubTokenEnv, githubAPIURL)
	output, checks, ready := hostedDoctorDocument(controller, err)
	if jsonOutput {
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

func hostedDoctorDocument(controller hostedstatus.Result, inspectErr error) (map[string]any, []doctorCheck, bool) {
	checks := []doctorCheck{{Name: "config", OK: true, Message: "persistent hosted config loaded"}}
	if inspectErr != nil {
		checks = append(checks, doctorCheck{Name: "hosted_controller", OK: false, Message: inspectErr.Error()})
	} else {
		for _, check := range controller.Checks {
			checks = append(checks, doctorCheck{Name: "hosted_" + check.Name, OK: check.Passed, Message: check.Details})
		}
	}
	paused := true
	if inspectErr == nil {
		paused = controller.Paused
	}
	checks = append(checks,
		doctorCheck{Name: "automation_active", OK: !paused, Message: ternary(paused, "hosted automation is paused or its remote state is unverified", "hosted automation is active")},
		doctorCheck{Name: "persistent_scheduler", OK: true, Message: "repository-owned GitHub cadence is installed; no bootstrap computer or local daemon is required"},
	)
	ready := inspectErr == nil && controller.ProductionReady && !paused && doctorChecksReady(checks)
	output := map[string]any{"schema_version": "hive.doctor.v1", "production_ready": ready, "checks": checks, "hosted_controller": controller}
	return output, checks, ready
}

func hostedConfig(stateDir string) (integrated.Config, bool, error) {
	store, err := integrated.NewStore(filepath.Join(stateDir, "integrated"))
	if err != nil {
		return integrated.Config{}, false, err
	}
	config, err := store.Load()
	if err != nil {
		return integrated.Config{}, false, err
	}
	return config, config.ExecutionMode == integrated.ExecutionHosted, nil
}

func dispatchHostedOperation(stateDir, operation, requestID, githubTokenEnv, githubAPIURL string, jsonOutput bool) int {
	config, hosted, err := hostedConfig(stateDir)
	if err != nil {
		return emitHostedDispatchFailure(hostedDispatchResult{SchemaVersion: "hive.hosted-dispatch.v1", Operation: operation}, jsonOutput, err)
	}
	if !hosted {
		return emitHostedDispatchFailure(hostedDispatchResult{SchemaVersion: "hive.hosted-dispatch.v1", Repository: config.Repository, Operation: operation}, jsonOutput, errors.New("installation uses the local runtime"))
	}
	if requestID == "" {
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			return emitHostedDispatchFailure(hostedDispatchResult{SchemaVersion: "hive.hosted-dispatch.v1", Repository: config.Repository, Operation: operation}, jsonOutput, errors.New("generate hosted request identity"))
		}
		requestID = "cli-" + operation + "-" + hex.EncodeToString(random[:])
	}
	result := hostedDispatchResult{
		SchemaVersion: "hive.hosted-dispatch.v1", Repository: config.Repository,
		Workflow: integrated.HostedControllerWorkflowPath, Ref: config.DefaultBranch,
		Operation: operation, RequestID: requestID,
	}
	if !hostedRequestPattern.MatchString(requestID) {
		return emitHostedDispatchFailure(result, jsonOutput, errors.New("hosted request ID is invalid"))
	}
	if operation != "cycle" && operation != "pause" && operation != "resume" && operation != "recover" && operation != "status" && operation != "doctor" {
		return emitHostedDispatchFailure(result, jsonOutput, errors.New("hosted operation is invalid"))
	}
	token := resolveGitHubToken(githubTokenEnv)
	if token == "" {
		return emitHostedDispatchFailure(result, jsonOutput, errors.New("GitHub authorization is required"))
	}
	owner, repository, ok := strings.Cut(config.Repository, "/")
	if !ok || owner == "" || repository == "" {
		return emitHostedDispatchFailure(result, jsonOutput, errors.New("hosted repository identity is invalid"))
	}
	client := hivegithub.NewClient(token, "", nil, slog.New(slog.NewTextHandler(io.Discard, nil)), githubAPIURL)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	_, err = client.GoGitHub().Actions.CreateWorkflowDispatchEventByFileName(ctx, owner, repository, integrated.HostedControllerWorkflowPath, gh.CreateWorkflowDispatchEventRequest{
		Ref: config.DefaultBranch,
		Inputs: map[string]interface{}{
			"operation":  operation,
			"request_id": requestID,
		},
	})
	if err != nil {
		result.RetryCommand = fmt.Sprintf("rerun the same command with --request-id %s", requestID)
		return emitHostedDispatchFailure(result, jsonOutput, fmt.Errorf("dispatch hosted controller (the request may have been accepted; retry only with the same request ID): %w", err))
	}
	result.Accepted = true
	if jsonOutput {
		return encodeJSON(result)
	}
	fmt.Printf("Hosted Hive %s accepted for %s (request %s).\n", operation, config.Repository, requestID)
	return 0
}

func emitHostedDispatchFailure(result hostedDispatchResult, jsonOutput bool, err error) int {
	result.Error = err.Error()
	if jsonOutput {
		_ = encodeJSON(result)
	} else {
		fmt.Println("Hosted Hive request failed:", err)
	}
	return 1
}
