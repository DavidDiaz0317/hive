package github

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestInspectPullRequestGateAndMergeExactSHA(t *testing.T) {
	mergeSHASeen := ""
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/pulls/7":
			_, _ = io.WriteString(writer, `{"number":7,"state":"open","draft":false,"html_url":"https://example.test/pull/7","mergeable":true,"mergeable_state":"clean","head":{"sha":"abc"},"base":{"ref":"main"},"labels":[]}`)
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/pulls/7/files":
			_, _ = io.WriteString(writer, `[{"filename":"tests/widget.test.ts"}]`)
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/branches/main/protection":
			_, _ = io.WriteString(writer, `{"required_status_checks":{"strict":true,"contexts":["visual-hive","unit"]},"required_pull_request_reviews":{"required_approving_review_count":0}}`)
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/commits/abc/check-runs":
			_, _ = io.WriteString(writer, `{"total_count":2,"check_runs":[{"name":"visual-hive","head_sha":"abc","status":"completed","conclusion":"success","html_url":"https://example.test/run/1"},{"name":"unit","head_sha":"abc","status":"completed","conclusion":"success"}]}`)
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/commits/abc/status":
			_, _ = io.WriteString(writer, `{"state":"success","statuses":[]}`)
		case request.Method == http.MethodPut && request.URL.Path == "/repos/owner/repo/pulls/7/merge":
			var body struct {
				SHA string `json:"sha"`
			}
			_ = json.NewDecoder(request.Body).Decode(&body)
			mergeSHASeen = body.SHA
			_, _ = io.WriteString(writer, `{"merged":true,"sha":"merge-sha","message":"merged"}`)
		default:
			http.Error(writer, request.Method+" "+request.URL.Path, http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	gate, err := client.InspectPullRequestGate(context.Background(), "owner/repo", 7)
	if err != nil {
		t.Fatal(err)
	}
	if gate.HeadSHA != "abc" || !gate.VisualHiveVerdictGreen || !gate.BranchProtectionEnabled || !gate.Mergeable {
		t.Fatalf("unexpected gate: %+v", gate)
	}
	if len(gate.RequiredCheckStates) != 2 || gate.RequiredCheckStates[0] != "success" || gate.RequiredCheckStates[1] != "success" {
		t.Fatalf("required checks = %v", gate.RequiredCheckStates)
	}
	mergeSHA, err := client.MergePullRequestExact(context.Background(), "owner/repo", 7, gate.HeadSHA)
	if err != nil || mergeSHA != "merge-sha" || mergeSHASeen != "abc" {
		t.Fatalf("merge = %q expected=%q err=%v", mergeSHA, mergeSHASeen, err)
	}
}

func TestDeleteRepairBranchRequiresExactUnmovedHead(t *testing.T) {
	deleted := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/git/ref/heads/hive/repair-proof":
			_, _ = io.WriteString(writer, `{"ref":"refs/heads/hive/repair-proof","object":{"sha":"repair-head","type":"commit"}}`)
		case request.Method == http.MethodDelete && request.URL.Path == "/repos/owner/repo/git/refs/heads/hive/repair-proof":
			deleted++
			writer.WriteHeader(http.StatusNoContent)
		default:
			http.Error(writer, request.Method+" "+request.URL.Path, http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	if err := client.DeleteRepairBranchExact(context.Background(), "owner/repo", "hive/repair-proof", "different-head"); err == nil || deleted != 0 {
		t.Fatalf("moved repair branch must be preserved: deleted=%d err=%v", deleted, err)
	}
	if err := client.DeleteRepairBranchExact(context.Background(), "owner/repo", "hive/repair-proof", "repair-head"); err != nil || deleted != 1 {
		t.Fatalf("exact merged repair branch was not deleted: deleted=%d err=%v", deleted, err)
	}
	if err := client.DeleteRepairBranchExact(context.Background(), "owner/repo", "codex/not-hive", "repair-head"); err == nil {
		t.Fatal("non-Hive branch deletion must be rejected")
	}
}

func TestInspectPullRequestGateKeepsUnsafeAndPendingSignals(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/repos/owner/repo/pulls/8":
			_, _ = io.WriteString(writer, `{"number":8,"state":"open","draft":true,"mergeable":false,"head":{"sha":"def"},"base":{"ref":"main"},"labels":[{"name":"hold"}]}`)
		case "/repos/owner/repo/pulls/8/files":
			_, _ = io.WriteString(writer, `[{"filename":".github/workflows/release.yml"},{"filename":"tests/__screenshots__/home.png"},{"filename":"src/auth/session.ts"},{"filename":"deploy/app.yaml"}]`)
		case "/repos/owner/repo/branches/main/protection":
			_, _ = io.WriteString(writer, `{"required_status_checks":{"strict":true,"contexts":["Visual Hive PR"]}}`)
		case "/repos/owner/repo/commits/def/check-runs":
			_, _ = io.WriteString(writer, `{"total_count":1,"check_runs":[{"name":"Visual Hive PR","head_sha":"def","status":"in_progress"}]}`)
		case "/repos/owner/repo/commits/def/status":
			_, _ = io.WriteString(writer, `{"state":"pending","statuses":[]}`)
		default:
			http.Error(writer, "missing", http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	gate, err := client.InspectPullRequestGate(context.Background(), "owner/repo", 8)
	if err != nil {
		t.Fatal(err)
	}
	if !gate.Hold || gate.VisualHiveVerdictGreen || gate.RequiredCheckStates[0] != "pending" || !gate.WorkflowChanged || !gate.BaselineChanged || !gate.SecuritySensitive || !gate.DeploymentChanged {
		t.Fatalf("unsafe signals were lost: %+v", gate)
	}
}

func TestInspectPullRequestGateUsesNewestSameNameCheckRun(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/repos/owner/repo/pulls/10":
			_, _ = io.WriteString(writer, `{"number":10,"state":"open","mergeable":true,"head":{"sha":"same-head"},"base":{"ref":"main"},"labels":[]}`)
		case "/repos/owner/repo/pulls/10/files":
			_, _ = io.WriteString(writer, `[{"filename":"tests/widget.test.ts"}]`)
		case "/repos/owner/repo/branches/main/protection":
			_, _ = io.WriteString(writer, `{"required_status_checks":{"strict":true,"contexts":["visual-hive"]}}`)
		case "/repos/owner/repo/commits/same-head/check-runs":
			_, _ = io.WriteString(writer, `{"total_count":2,"check_runs":[{"id":200,"name":"visual-hive","status":"completed","conclusion":"success"},{"id":100,"name":"visual-hive","status":"completed","conclusion":"cancelled"}]}`)
		case "/repos/owner/repo/commits/same-head/status":
			_, _ = io.WriteString(writer, `{"state":"failure","statuses":[{"id":300,"context":"visual-hive","state":"failure"}]}`)
		default:
			http.Error(writer, "missing", http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	gate, err := client.InspectPullRequestGate(context.Background(), "owner/repo", 10)
	if err != nil {
		t.Fatal(err)
	}
	if !gate.VisualHiveVerdictGreen || len(gate.RequiredCheckStates) != 1 || gate.RequiredCheckStates[0] != "success" || len(gate.Checks) != 1 {
		t.Fatalf("newest App-backed check was not selected: %+v", gate)
	}
}

func TestBaselineImageNameDoesNotImplyAuthCodeChange(t *testing.T) {
	gate := PullRequestGate{ChangedFiles: []string{
		"visual-hive.baselines/linux/public-auth-boundary__mobile.png",
	}}
	classifyGatePaths(&gate)
	if !gate.BaselineChanged || gate.SecuritySensitive || gate.DeploymentChanged || gate.WorkflowChanged {
		t.Fatalf("reviewed baseline filename was misclassified as code risk: %+v", gate)
	}
}

func TestInspectPullRequestGateReportsExternalMerge(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/repos/owner/repo/pulls/9":
			_, _ = io.WriteString(writer, `{"number":9,"state":"closed","merged":true,"merge_commit_sha":"merge-123","head":{"sha":"head-123"},"base":{"ref":"main"},"labels":[]}`)
		case "/repos/owner/repo/pulls/9/files":
			_, _ = io.WriteString(writer, `[{"filename":"index.html"}]`)
		case "/repos/owner/repo/branches/main/protection":
			_, _ = io.WriteString(writer, `{"required_status_checks":{"strict":true,"contexts":["visual-hive"]},"required_pull_request_reviews":{"required_approving_review_count":0}}`)
		case "/repos/owner/repo/commits/head-123/check-runs":
			_, _ = io.WriteString(writer, `{"total_count":1,"check_runs":[{"name":"visual-hive","head_sha":"head-123","status":"completed","conclusion":"success"}]}`)
		case "/repos/owner/repo/commits/head-123/status":
			_, _ = io.WriteString(writer, `{"state":"success","statuses":[]}`)
		default:
			http.Error(writer, "missing", http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	gate, err := client.InspectPullRequestGate(context.Background(), "owner/repo", 9)
	if err != nil {
		t.Fatal(err)
	}
	if gate.Open || !gate.Merged || gate.MergeSHA != "merge-123" || gate.HeadSHA != "head-123" || !gate.VisualHiveVerdictGreen {
		t.Fatalf("external merge signals were lost: %+v", gate)
	}
}
