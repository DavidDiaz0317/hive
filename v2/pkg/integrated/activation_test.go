package integrated

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kubestellar/hive/v2/pkg/automation"
	hivegithub "github.com/kubestellar/hive/v2/pkg/github"
)

type activationRepositoryServer struct {
	mu             sync.Mutex
	installed      bool
	protectionMode string
	protectionPuts int
	url            string
	config         Config
	head           string
	runName        string
	runPath        string
	runEvent       string
	runBranch      string
	seedConclusion string
}

func newActivationRepositoryServer(t *testing.T, installed bool, protectionMode string) (*activationRepositoryServer, *httptest.Server) {
	t.Helper()
	config := Config{
		Repository: "owner/repo", RepositoryID: "123", DefaultBranch: "main", StateDir: t.TempDir(),
		Coverage: CoverageComprehensive, Automation: AutomationAutoMerge, Provider: "codex", ACMMLevel: 6,
		MaxActiveIssues: 5, MaxRepairAttempts: 4, VisualHive: true,
		VisualHiveRepo: "owner/visual", VisualHiveRef: strings.Repeat("a", 40),
		TestCommands: [][]string{{"npm", "test"}}, AllowedRepairPaths: []string{"tests/**"},
		AllowedAutoMergePaths: []string{"tests/**"}, AllowedAutoMergeRisk: []automation.RiskTier{automation.RiskAutomatic},
	}
	fixture := &activationRepositoryServer{
		installed: installed, protectionMode: protectionMode, config: config, head: strings.Repeat("b", 40),
		runName: visualHiveProductionWorkflowName, runPath: visualHiveProductionWorkflowPath,
		runEvent: visualHiveProductionWorkflowEvent, runBranch: "main", seedConclusion: "success",
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		fixture.serve(t, writer, request)
	}))
	fixture.url = server.URL
	return fixture, server
}

func (s *activationRepositoryServer) serve(t *testing.T, writer http.ResponseWriter, request *http.Request) {
	t.Helper()
	writer.Header().Set("Content-Type", "application/json")
	switch request.URL.Path {
	case "/apps/github-actions":
		_, _ = io.WriteString(writer, `{"id":42,"slug":"github-actions"}`)
	case "/repos/owner/repo/branches/main":
		_, _ = io.WriteString(writer, fmt.Sprintf(`{"name":"main","commit":{"sha":%q}}`, s.head))
	case "/repos/owner/repo/actions/runs/77":
		_, _ = io.WriteString(writer, fmt.Sprintf(`{"id":77,"name":%q,"path":%q,"event":%q,"head_branch":%q,"head_sha":%q,"display_title":%q,"status":"completed","conclusion":"success","repository":{"id":123,"full_name":"owner/repo"}}`, s.runName, s.runPath, s.runEvent, s.runBranch, s.head, workflowDispatchDisplayTitle(strings.Repeat("c", 64))))
	case "/repos/owner/repo/actions/runs/77/jobs":
		_, _ = io.WriteString(writer, fmt.Sprintf(`{"total_count":2,"jobs":[{"id":800,"run_id":77,"head_branch":%q,"head_sha":%q,"status":"completed","conclusion":"success","name":"visual-hive-production","workflow_name":"Hive Visual Hive Production","check_run_url":%q},{"id":801,"run_id":77,"head_branch":%q,"head_sha":%q,"status":"completed","conclusion":%q,"name":"visual-hive","workflow_name":"Hive Visual Hive Production","check_run_url":%q}]}`, s.runBranch, s.head, s.url+"/repos/owner/repo/check-runs/900", s.runBranch, s.head, s.seedConclusion, s.url+"/repos/owner/repo/check-runs/901"))
	case "/repos/owner/repo/check-runs/900":
		_, _ = io.WriteString(writer, fmt.Sprintf(`{"id":900,"name":"visual-hive-production","head_sha":%q,"status":"completed","conclusion":"success","app":{"id":42}}`, s.head))
	case "/repos/owner/repo/check-runs/901":
		_, _ = io.WriteString(writer, fmt.Sprintf(`{"id":901,"name":"visual-hive","head_sha":%q,"status":"completed","conclusion":%q,"app":{"id":42}}`, s.head, s.seedConclusion))
	case "/repos/owner/repo/contents/.hive/integrated.json":
		if !s.installed {
			http.Error(writer, "missing", http.StatusNotFound)
			return
		}
		installed := installedRepositoryConfig{
			SchemaVersion: ConfigSchema, Repository: s.config.Repository, RepositoryID: s.config.RepositoryID, DefaultBranch: s.config.DefaultBranch,
			Coverage: s.config.Coverage, Automation: s.config.Automation, Provider: s.config.Provider, ACMMLevel: s.config.ACMMLevel,
			MaxActiveIssues: s.config.MaxActiveIssues, MaxRepairAttempts: s.config.MaxRepairAttempts, VisualHive: true,
			VisualHiveRepo: s.config.VisualHiveRepo, VisualHiveRef: s.config.VisualHiveRef, TestCommands: s.config.TestCommands,
			AllowedRepairPaths: s.config.AllowedRepairPaths, AllowedAutoMergePaths: s.config.AllowedAutoMergePaths, AllowedAutoMergeRisk: s.config.AllowedAutoMergeRisk,
		}
		data, _ := json.Marshal(installed)
		writeActivationContent(writer, data)
	case "/repos/owner/repo/contents/.github/workflows/hive-visual-hive.yml":
		writeActivationContent(writer, []byte(workflow(s.config)))
	case "/repos/owner/repo/contents/.github/workflows/visual-hive-pr.yml":
		writeActivationContent(writer, []byte(pullRequestWorkflow(s.config)))
	case "/repos/owner/repo/contents/visual-hive.config.yaml":
		writeActivationContent(writer, []byte("project:\n  setupProfile: complex-app\n"))
	case "/repos/owner/repo/contents/.github/workflows/visual-hive-issue-lifecycle.yml", "/repos/owner/repo/contents/.github/workflows/visual-hive-trusted-publisher.yml":
		http.Error(writer, "missing", http.StatusNotFound)
	case "/repos/owner/repo/branches/main/protection":
		if request.Method == http.MethodPut {
			var body struct {
				RequiredStatusChecks struct {
					Strict bool                               `json:"strict"`
					Checks []hivegithub.RequiredCheckIdentity `json:"checks"`
				} `json:"required_status_checks"`
				EnforceAdmins bool `json:"enforce_admins"`
			}
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil || !body.EnforceAdmins || !body.RequiredStatusChecks.Strict || len(body.RequiredStatusChecks.Checks) != 1 || body.RequiredStatusChecks.Checks[0].Context != "visual-hive" || body.RequiredStatusChecks.Checks[0].AppID != 42 {
				t.Fatalf("unsafe activation protection request: %+v err=%v", body, err)
			}
			s.mu.Lock()
			s.protectionPuts++
			s.protectionMode = "exact"
			s.mu.Unlock()
			_, _ = io.WriteString(writer, `{"required_status_checks":{"strict":true,"checks":[{"context":"visual-hive","app_id":42}]},"enforce_admins":{"enabled":true}}`)
			return
		}
		s.mu.Lock()
		mode := s.protectionMode
		s.mu.Unlock()
		switch mode {
		case "exact":
			_, _ = io.WriteString(writer, `{"required_status_checks":{"strict":true,"checks":[{"context":"visual-hive","app_id":42}]},"enforce_admins":{"enabled":true}}`)
		case "name-only":
			_, _ = io.WriteString(writer, `{"required_status_checks":{"strict":true,"contexts":["visual-hive"]},"enforce_admins":{"enabled":true}}`)
		default:
			http.Error(writer, "missing", http.StatusNotFound)
		}
	case "/repos/owner/repo/rules/branches/main":
		http.Error(writer, "missing", http.StatusNotFound)
	default:
		http.Error(writer, request.Method+" "+request.URL.Path, http.StatusNotFound)
	}
}

func writeActivationContent(writer http.ResponseWriter, data []byte) {
	_, _ = io.WriteString(writer, `{"type":"file","encoding":"base64","content":"`+base64.StdEncoding.EncodeToString(data)+`"}`)
}

func activationWorkflow(fixture *activationRepositoryServer) WorkflowRunEvidence {
	return WorkflowRunEvidence{CorrelationID: strings.Repeat("c", 64), RunID: 77, RunURL: fixture.url + "/actions/runs/77", HeadSHA: fixture.head, Conclusion: "success", EvidenceArtifact: 88, BundleArtifact: 89}
}

func saveActivationDispatch(t *testing.T, store *Store, config Config, workflow WorkflowRunEvidence) {
	t.Helper()
	now := time.Now().UTC()
	intent := WorkflowDispatchIntent{
		SchemaVersion: WorkflowDispatchSchema, Repository: config.Repository, RepositoryID: config.RepositoryID,
		WorkflowFile: "hive-visual-hive.yml", Ref: config.DefaultBranch, CorrelationID: workflow.CorrelationID,
		ExpectedDisplayTitle: workflowDispatchDisplayTitle(workflow.CorrelationID), PreparedAt: now,
		DispatchAttemptedAt: now, DispatchAcknowledgedAt: now, RunID: workflow.RunID, RunURL: workflow.RunURL, MatchedAt: now,
	}
	if err := store.SaveWorkflowDispatchIntent(intent); err != nil {
		t.Fatal(err)
	}
}

func TestProtectionActivationDoesNotMutateBeforeManagedSetupIsMerged(t *testing.T) {
	fixture, server := newActivationRepositoryServer(t, false, "none")
	defer server.Close()
	store, err := NewStore(filepath.Join(fixture.config.StateDir, "integrated"))
	if err != nil {
		t.Fatal(err)
	}
	client := hivegithub.NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	_, err = ActivateAutoMergeProtection(context.Background(), store, client, fixture.config, activationWorkflow(fixture))
	if !errors.Is(err, ErrProtectionActivationRunStale) {
		t.Fatalf("pre-merge activation did not fail closed: %v", err)
	}
	if fixture.protectionPuts != 0 {
		t.Fatalf("pre-merge activation mutated protection %d time(s)", fixture.protectionPuts)
	}
	if _, exists, loadErr := store.LoadProtectionActivation(); loadErr != nil || exists {
		t.Fatalf("pre-merge activation persisted readiness: exists=%t err=%v", exists, loadErr)
	}
}

func TestProtectionActivationSeedsPRContextWithoutPriorPRCheckAndRerunsAsNoop(t *testing.T) {
	fixture, server := newActivationRepositoryServer(t, true, "none")
	defer server.Close()
	store, err := NewStore(filepath.Join(fixture.config.StateDir, "integrated"))
	if err != nil {
		t.Fatal(err)
	}
	client := hivegithub.NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	workflow := activationWorkflow(fixture)
	saveActivationDispatch(t, store, fixture.config, workflow)
	first, err := ActivateAutoMergeProtection(context.Background(), store, client, fixture.config, workflow)
	if err != nil || !first.Required || !first.Activated || first.Idempotent || !first.ProtectionCreated || first.State == nil {
		t.Fatalf("fresh activation = %+v err=%v", first, err)
	}
	second, err := ActivateAutoMergeProtection(context.Background(), store, client, fixture.config, workflow)
	if err != nil || !second.Activated || !second.Idempotent || second.ProtectionCreated || second.State == nil {
		t.Fatalf("idempotent activation = %+v err=%v", second, err)
	}
	if fixture.protectionPuts != 1 {
		t.Fatalf("protection mutations=%d want=1", fixture.protectionPuts)
	}
	persisted, exists, err := store.LoadProtectionActivation()
	if err != nil || !exists || !ProtectionActivationMatchesConfig(persisted, fixture.config) || persisted.WorkflowRunID != 77 || persisted.CheckRunID != 900 || persisted.SeedCheckRunID != 901 || persisted.CheckAppID != 42 ||
		persisted.CheckContext != visualHiveProductionCheckContext || persisted.RequiredContext != visualHivePRCheckContext || persisted.WorkflowPath != visualHiveProductionWorkflowPath {
		t.Fatalf("durable activation = %+v exists=%t err=%v", persisted, exists, err)
	}
	audit, err := os.ReadFile(filepath.Join(store.Dir(), "audit.jsonl"))
	if err != nil || strings.Count(string(audit), `"action":"activate_branch_protection","allowed":true`) != 1 {
		t.Fatalf("activation audit was not exactly-once: %q err=%v", audit, err)
	}
}

func TestProtectionActivationRejectsWrongProductionWorkflowProvenance(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*activationRepositoryServer)
	}{
		{name: "PR workflow path", mutate: func(f *activationRepositoryServer) { f.runPath = ".github/workflows/visual-hive-pr.yml" }},
		{name: "PR workflow name", mutate: func(f *activationRepositoryServer) { f.runName = "Visual Hive PR" }},
		{name: "pull request event", mutate: func(f *activationRepositoryServer) { f.runEvent = "pull_request" }},
		{name: "off-default dispatch with spoofed green seed", mutate: func(f *activationRepositoryServer) { f.runBranch = "hive/repair-one" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture, server := newActivationRepositoryServer(t, true, "none")
			defer server.Close()
			test.mutate(fixture)
			store, err := NewStore(filepath.Join(fixture.config.StateDir, "integrated"))
			if err != nil {
				t.Fatal(err)
			}
			workflow := activationWorkflow(fixture)
			saveActivationDispatch(t, store, fixture.config, workflow)
			client := hivegithub.NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
			if _, err := ActivateAutoMergeProtection(context.Background(), store, client, fixture.config, workflow); err == nil || !strings.Contains(err.Error(), "managed production workflow path/name/event/ref/head") {
				t.Fatalf("spoofed production provenance was accepted: %v", err)
			}
			if fixture.protectionPuts != 0 {
				t.Fatalf("spoofed activation mutated protection %d time(s)", fixture.protectionPuts)
			}
		})
	}
}

func TestProtectionActivationRejectsRedPRContextSeed(t *testing.T) {
	fixture, server := newActivationRepositoryServer(t, true, "none")
	defer server.Close()
	fixture.seedConclusion = "failure"
	store, err := NewStore(filepath.Join(fixture.config.StateDir, "integrated"))
	if err != nil {
		t.Fatal(err)
	}
	workflow := activationWorkflow(fixture)
	saveActivationDispatch(t, store, fixture.config, workflow)
	client := hivegithub.NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	if _, err := ActivateAutoMergeProtection(context.Background(), store, client, fixture.config, workflow); err == nil || !strings.Contains(err.Error(), "actively guarded") {
		t.Fatalf("red PR-context eligibility seed was accepted: %v", err)
	}
	if fixture.protectionPuts != 0 {
		t.Fatalf("red seed mutated protection %d time(s)", fixture.protectionPuts)
	}
}

func TestProtectionActivationRejectsExistingRepositoryPolicyWithoutReplacingIt(t *testing.T) {
	fixture, server := newActivationRepositoryServer(t, true, "name-only")
	defer server.Close()
	store, err := NewStore(filepath.Join(fixture.config.StateDir, "integrated"))
	if err != nil {
		t.Fatal(err)
	}
	client := hivegithub.NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	workflow := activationWorkflow(fixture)
	saveActivationDispatch(t, store, fixture.config, workflow)
	_, err = ActivateAutoMergeProtection(context.Background(), store, client, fixture.config, workflow)
	if err == nil || !strings.Contains(err.Error(), "did not replace repository-owned policy") || !strings.Contains(err.Error(), "App ID 42") {
		t.Fatalf("existing policy rejection was not actionable: %v", err)
	}
	if fixture.protectionPuts != 0 {
		t.Fatalf("existing repository policy was replaced %d time(s)", fixture.protectionPuts)
	}
	if _, exists, loadErr := store.LoadProtectionActivation(); loadErr != nil || exists {
		t.Fatalf("rejected policy persisted activation: exists=%t err=%v", exists, loadErr)
	}
	if intent, exists, loadErr := store.LoadWorkflowDispatchIntent(); loadErr != nil || !exists || intent.RunID != workflow.RunID {
		t.Fatalf("failed activation did not preserve exact dispatch for retry: exists=%t intent=%+v err=%v", exists, intent, loadErr)
	}
}
