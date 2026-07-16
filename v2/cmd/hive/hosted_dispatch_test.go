package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/kubestellar/hive/v2/pkg/hostedstatus"
	"github.com/kubestellar/hive/v2/pkg/integrated"
)

func TestHostedStatusAndDoctorUseRemoteReceiptNotStaleLocalPauseState(t *testing.T) {
	localActive := integrated.Config{Paused: false, ExecutionMode: integrated.ExecutionHosted}
	remotePaused := hostedstatus.Result{ProductionReady: false, Paused: true, Checks: []hostedstatus.Check{{Name: hostedstatus.CheckRemotePauseState, Details: "paused"}}}
	status, ready := hostedStatusDocument("state", localActive, remotePaused)
	if ready || status["paused"] != true || status["production_ready"] != false {
		t.Fatalf("status trusted stale active local config: %#v", status)
	}
	doctor, _, ready := hostedDoctorDocument(remotePaused, nil)
	if ready || doctor["production_ready"] != false {
		t.Fatalf("doctor trusted stale active local config: %#v", doctor)
	}

	localPaused := integrated.Config{Paused: true, ExecutionMode: integrated.ExecutionHosted}
	remoteActive := hostedstatus.Result{ProductionReady: true, Paused: false, Checks: []hostedstatus.Check{{Name: hostedstatus.CheckRemotePauseState, Passed: true, Details: "active"}}}
	status, ready = hostedStatusDocument("state", localPaused, remoteActive)
	if !ready || status["paused"] != false || status["production_ready"] != true {
		t.Fatalf("status ignored authoritative remote resume: %#v", status)
	}
	doctor, _, ready = hostedDoctorDocument(remoteActive, nil)
	if !ready || doctor["production_ready"] != true {
		t.Fatalf("doctor ignored authoritative remote resume: %#v", doctor)
	}
}

func TestDispatchHostedOperationUsesManagedWorkflowAndExactIdempotencyKey(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	store, err := integrated.NewStore(filepath.Join(stateDir, "integrated"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(integrated.Config{
		Repository: "owner/repository", RepositoryID: "42", DefaultBranch: "main",
		ExecutionMode: integrated.ExecutionHosted, HostedStateBranch: "hive/state-42",
	}); err != nil {
		t.Fatal(err)
	}
	var method, path string
	var request struct {
		Ref    string                 `json:"ref"`
		Inputs map[string]interface{} `json:"inputs"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, incoming *http.Request) {
		method, path = incoming.Method, incoming.URL.Path
		if err := json.NewDecoder(incoming.Body).Decode(&request); err != nil {
			t.Errorf("decode dispatch request: %v", err)
		}
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	t.Setenv("HIVE_GITHUB_TOKEN", "test-token")

	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	prior := os.Stdout
	os.Stdout = writer
	code := dispatchHostedOperation(stateDir, "pause", "operator-pause-1", "HIVE_GITHUB_TOKEN", server.URL, true)
	_ = writer.Close()
	os.Stdout = prior
	output, readErr := io.ReadAll(reader)
	_ = reader.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	if code != 0 {
		t.Fatalf("dispatch exit=%d output=%s", code, output)
	}
	var result hostedDispatchResult
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("decode dispatch result: %v: %s", err, output)
	}
	if !result.Accepted || result.RequestID != "operator-pause-1" || result.Operation != "pause" || result.Ref != "main" {
		t.Fatalf("unexpected hosted dispatch result: %+v", result)
	}
	if method != http.MethodPost || request.Ref != "main" || request.Inputs["operation"] != "pause" || request.Inputs["request_id"] != "operator-pause-1" {
		t.Fatalf("unexpected hosted dispatch request: method=%s path=%s body=%+v", method, path, request)
	}
	if path != "/repos/owner/repository/actions/workflows/"+integrated.HostedControllerWorkflowPath+"/dispatches" {
		t.Fatalf("dispatch used unexpected workflow path %q", path)
	}
}

func TestDispatchHostedOperationReportsAmbiguousRetryIdentity(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	store, err := integrated.NewStore(filepath.Join(stateDir, "integrated"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(integrated.Config{Repository: "owner/repository", RepositoryID: "42", DefaultBranch: "main", ExecutionMode: integrated.ExecutionHosted}); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, incoming *http.Request) {
		http.Error(writer, `{"message":"ambiguous"}`, http.StatusInternalServerError)
	}))
	defer server.Close()
	t.Setenv("HIVE_GITHUB_TOKEN", "test-token")

	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	prior := os.Stdout
	os.Stdout = writer
	code := dispatchHostedOperation(stateDir, "cycle", "same-cycle-id", "HIVE_GITHUB_TOKEN", server.URL, true)
	_ = writer.Close()
	os.Stdout = prior
	output, _ := io.ReadAll(reader)
	_ = reader.Close()
	if code == 0 {
		t.Fatalf("ambiguous transport unexpectedly succeeded: %s", output)
	}
	var result hostedDispatchResult
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("decode failure result: %v: %s", err, output)
	}
	if result.RequestID != "same-cycle-id" || result.RetryCommand == "" || result.Error == "" || result.Accepted {
		t.Fatalf("ambiguous dispatch lost its exact recovery identity: %+v", result)
	}
}
