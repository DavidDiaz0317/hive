package integrated

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	hivegithub "github.com/kubestellar/hive/v2/pkg/github"
	"github.com/kubestellar/hive/v2/pkg/visualhive"
)

func TestActiveRepairFindingSkipsHumanReviewOnlyIssue(t *testing.T) {
	state := visualhive.LifecycleState{Findings: map[string]*visualhive.FindingLifecycle{
		"a-baseline": {
			RepositoryFingerprint: "a-baseline", Status: visualhive.StatusIssueOpen,
			IssueNumber: 100, HumanReviewRequired: true,
		},
		"b-console": {
			RepositoryFingerprint: "b-console", Status: visualhive.StatusIssueOpen,
			IssueNumber: 101,
		},
	}}

	finding, ok := activeRepairFinding(state)
	if !ok || finding.RepositoryFingerprint != "b-console" {
		t.Fatalf("expected actionable finding, got ok=%t finding=%+v", ok, finding)
	}
}

func TestRepairPreparationAndPinnedCLIEnvironment(t *testing.T) {
	checkout := t.TempDir()
	if err := os.WriteFile(filepath.Join(checkout, "package-lock.json"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	commands := repairPreparationCommands(checkout)
	if len(commands) != 1 || commands[0].Name != "npm" || len(commands[0].Args) != 1 || commands[0].Args[0] != "ci" {
		t.Fatalf("unexpected preparation commands: %+v", commands)
	}
	cli := filepath.Join(checkout, "visual-hive", "dist", "index.js")
	environment := repairValidationEnvironment(Config{VisualHiveArgs: []string{cli}})
	if environment["VISUAL_HIVE_CLI"] != cli {
		t.Fatalf("pinned CLI environment = %v", environment)
	}
}

func TestDispatchRetryIsBoundedToConcurrencyCancellation(t *testing.T) {
	if !retryCancelledDispatch("cancelled", 1, 3) || retryCancelledDispatch("failure", 1, 3) || retryCancelledDispatch("cancelled", 3, 3) {
		t.Fatal("dispatch retry classification must be cancellation-only and bounded")
	}
}

func TestReconcileExternallyMergedRepairPersistsExactMerge(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/repos/owner/repo/pulls/12":
			_, _ = io.WriteString(writer, `{"number":12,"state":"closed","merged":true,"merge_commit_sha":"merge-sha","head":{"sha":"repair-sha"},"base":{"ref":"main"},"labels":[]}`)
		case "/repos/owner/repo/pulls/12/files":
			_, _ = io.WriteString(writer, `[{"filename":"index.html"}]`)
		case "/repos/owner/repo/branches/main/protection":
			_, _ = io.WriteString(writer, `{"required_status_checks":{"strict":true,"contexts":["visual-hive"]},"required_pull_request_reviews":{"required_approving_review_count":0}}`)
		case "/repos/owner/repo/commits/repair-sha/check-runs":
			_, _ = io.WriteString(writer, `{"total_count":1,"check_runs":[{"name":"visual-hive","head_sha":"repair-sha","status":"completed","conclusion":"success"}]}`)
		case "/repos/owner/repo/commits/repair-sha/status":
			_, _ = io.WriteString(writer, `{"state":"success","statuses":[]}`)
		default:
			http.Error(writer, "missing", http.StatusNotFound)
		}
	}))
	defer server.Close()

	dir := t.TempDir()
	state := visualhive.LifecycleState{
		SchemaVersion: visualhive.LifecycleSchema,
		Findings: map[string]*visualhive.FindingLifecycle{
			"earlier-open-issue": {
				Repository: "owner/repo", RepositoryFingerprint: "earlier-open-issue", Status: visualhive.StatusIssueOpen, IssueNumber: 9,
			},
			"finding": {
				Repository: "owner/repo", RepositoryFingerprint: "finding", Status: visualhive.StatusReady,
				IssueNumber: 10, PRNumber: 12, RepairCommitSHA: "repair-sha",
			},
		},
		ReplayKeys: map[string]string{}, Outbox: []*visualhive.OutboxEntry{},
	}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "visual-hive-lifecycle.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	lifecycle, err := visualhive.NewLifecycleStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	client := hivegithub.NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	reconciled, err := reconcileExternallyMergedRepair(context.Background(), lifecycle, client)
	if err != nil {
		t.Fatal(err)
	}
	finding, exists := lifecycle.Finding("finding")
	if !reconciled || !exists || finding.Status != visualhive.StatusMerged || finding.MergeSHA != "merge-sha" {
		t.Fatalf("external merge was not persisted: reconciled=%t finding=%+v", reconciled, finding)
	}
}
