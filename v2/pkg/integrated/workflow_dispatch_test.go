package integrated

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	hivegithub "github.com/kubestellar/hive/v2/pkg/github"
)

func TestDispatchConsumesOnlyExactCorrelationAndArtifacts(t *testing.T) {
	var correlation string
	dispatches := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodPost && request.URL.Path == "/repos/owner/repo/actions/workflows/hive-visual-hive.yml/dispatches":
			var body struct {
				Ref    string         `json:"ref"`
				Inputs map[string]any `json:"inputs"`
			}
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			correlation, _ = body.Inputs[workflowDispatchInput].(string)
			if body.Ref != "main" || !workflowDispatchCorrelationPattern.MatchString(correlation) {
				t.Errorf("dispatch was not exactly correlated: ref=%q correlation=%q", body.Ref, correlation)
			}
			dispatches++
			writer.WriteHeader(http.StatusNoContent)
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/actions/workflows/hive-visual-hive.yml/runs":
			manual := `{"id":22,"display_title":"manual production run","event":"workflow_dispatch","head_branch":"main","status":"completed","conclusion":"success","html_url":"https://example.test/runs/22","head_sha":"manual"}`
			exact := fmt.Sprintf(`{"id":21,"display_title":%q,"event":"workflow_dispatch","head_branch":"main","status":"completed","conclusion":"success","html_url":"https://example.test/runs/21","head_sha":"exact-head"}`, workflowDispatchDisplayTitle(correlation))
			_, _ = fmt.Fprintf(writer, `{"total_count":2,"workflow_runs":[%s,%s]}`, manual, exact)
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/actions/runs/21/artifacts":
			_, _ = io.WriteString(writer, `{"total_count":4,"artifacts":[{"id":901,"name":"visual-hive-evidence-decoy"},{"id":902,"name":"visual-hive-bundle-decoy"},{"id":101,"name":"visual-hive-evidence-21"},{"id":102,"name":"visual-hive-bundle-21"}]}`)
		default:
			http.Error(writer, request.Method+" "+request.URL.Path, http.StatusNotFound)
		}
	}))
	defer server.Close()

	stateDir := t.TempDir()
	config := dispatchTestConfig(stateDir)
	client := hivegithub.NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	workflow, err := dispatchAndWait(context.Background(), client, config)
	if err != nil {
		t.Fatal(err)
	}
	if dispatches != 1 || workflow.RunID != 21 || workflow.HeadSHA != "exact-head" || workflow.CorrelationID != correlation {
		t.Fatalf("wrong workflow consumed: dispatches=%d workflow=%+v", dispatches, workflow)
	}
	if workflow.EvidenceArtifact != 101 || workflow.BundleArtifact != 102 {
		t.Fatalf("artifact selection was not exact-run-bound: %+v", workflow)
	}
	store, err := NewStore(stateDir + "/integrated")
	if err != nil {
		t.Fatal(err)
	}
	intent, exists, err := store.LoadWorkflowDispatchIntent()
	if err != nil || !exists || intent.RunID != workflow.RunID || intent.CorrelationID != workflow.CorrelationID {
		t.Fatalf("exact dispatch binding was not durable: exists=%t intent=%+v err=%v", exists, intent, err)
	}
	if err := consumeWorkflowDispatch(stateDir, workflow); err != nil {
		t.Fatal(err)
	}
	if _, exists, err := store.LoadWorkflowDispatchIntent(); err != nil || exists {
		t.Fatalf("consumed dispatch intent remains: exists=%t err=%v", exists, err)
	}
}

func TestDispatchRecoveryReusesCorrelationWithoutRedispatch(t *testing.T) {
	var mu sync.Mutex
	correlation := ""
	dispatches := 0
	exposeRun := false
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		mu.Lock()
		defer mu.Unlock()
		switch {
		case request.Method == http.MethodPost && request.URL.Path == "/repos/owner/repo/actions/workflows/hive-visual-hive.yml/dispatches":
			var body struct {
				Inputs map[string]any `json:"inputs"`
			}
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			correlation, _ = body.Inputs[workflowDispatchInput].(string)
			dispatches++
			writer.WriteHeader(http.StatusNoContent)
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/actions/workflows/hive-visual-hive.yml/runs":
			if !exposeRun {
				_, _ = io.WriteString(writer, `{"total_count":0,"workflow_runs":[]}`)
				return
			}
			_, _ = fmt.Fprintf(writer, `{"total_count":1,"workflow_runs":[{"id":31,"display_title":%q,"event":"workflow_dispatch","head_branch":"main","status":"completed","conclusion":"success","html_url":"https://example.test/runs/31","head_sha":"head"}]}`, workflowDispatchDisplayTitle(correlation))
		default:
			http.Error(writer, request.Method+" "+request.URL.Path, http.StatusNotFound)
		}
	}))
	defer server.Close()

	stateDir := t.TempDir()
	config := dispatchTestConfig(stateDir)
	store, err := NewStore(stateDir + "/integrated")
	if err != nil {
		t.Fatal(err)
	}
	client := hivegithub.NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	firstCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, _, err := dispatchAndWaitAttempt(firstCtx, client, store, config, "owner", "repo", "hive-visual-hive.yml", "main"); err == nil || !strings.Contains(err.Error(), "exactly correlated") {
		t.Fatalf("unobserved dispatch should retain a recoverable error, got %v", err)
	}
	intent, exists, err := store.LoadWorkflowDispatchIntent()
	if err != nil || !exists || intent.CorrelationID != correlation || intent.RunID != 0 || intent.DispatchAcknowledgedAt.IsZero() {
		t.Fatalf("unobserved dispatch was not durably recoverable: exists=%t intent=%+v err=%v", exists, intent, err)
	}

	mu.Lock()
	exposeRun = true
	mu.Unlock()
	selected, recovered, err := dispatchAndWaitAttempt(context.Background(), client, store, config, "owner", "repo", "hive-visual-hive.yml", "main")
	if err != nil {
		t.Fatal(err)
	}
	if dispatches != 1 || selected.GetID() != 31 || recovered.CorrelationID != correlation || recovered.RunID != 31 {
		t.Fatalf("recovery redispatched or selected the wrong run: dispatches=%d selected=%+v intent=%+v", dispatches, selected, recovered)
	}
}

func TestDispatchUsesReturnedRunIDWithoutRecencySearch(t *testing.T) {
	var correlation string
	listCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodPost && request.URL.Path == "/repos/owner/repo/actions/workflows/hive-visual-hive.yml/dispatches":
			if version := request.Header.Get("X-GitHub-Api-Version"); version != "2022-11-28" {
				t.Errorf("self-hosted test endpoint API version = %q", version)
			}
			var body struct {
				Inputs map[string]any `json:"inputs"`
			}
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			correlation, _ = body.Inputs[workflowDispatchInput].(string)
			_, _ = io.WriteString(writer, `{"workflow_run_id":51,"run_url":"https://api.example.test/runs/51","html_url":"https://example.test/runs/51"}`)
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/actions/runs/51":
			_, _ = fmt.Fprintf(writer, `{"id":51,"display_title":%q,"event":"workflow_dispatch","head_branch":"main","status":"completed","conclusion":"success","html_url":"https://example.test/runs/51","head_sha":"exact"}`, workflowDispatchDisplayTitle(correlation))
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/actions/workflows/hive-visual-hive.yml/runs":
			listCalls++
			http.Error(writer, "run search must not be used when dispatch returns an exact ID", http.StatusInternalServerError)
		default:
			http.Error(writer, request.Method+" "+request.URL.Path, http.StatusNotFound)
		}
	}))
	defer server.Close()

	stateDir := t.TempDir()
	config := dispatchTestConfig(stateDir)
	store, err := NewStore(stateDir + "/integrated")
	if err != nil {
		t.Fatal(err)
	}
	client := hivegithub.NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	selected, intent, err := dispatchAndWaitAttempt(context.Background(), client, store, config, "owner", "repo", "hive-visual-hive.yml", "main")
	if err != nil {
		t.Fatal(err)
	}
	if listCalls != 0 || selected.GetID() != 51 || intent.RunID != 51 || intent.CorrelationID != correlation {
		t.Fatalf("dispatch response was not bound directly: listCalls=%d selected=%+v intent=%+v", listCalls, selected, intent)
	}
}

func TestWorkflowDispatchAPIVersionSelection(t *testing.T) {
	for _, test := range []struct {
		host string
		want bool
	}{
		{host: "api.github.com", want: true},
		{host: "API.GITHUB.COM", want: true},
		{host: "api.enterprise.ghe.com", want: true},
		{host: "github.example.test", want: false},
		{host: "127.0.0.1", want: false},
	} {
		if got := usesCurrentWorkflowDispatchAPI(test.host); got != test.want {
			t.Errorf("usesCurrentWorkflowDispatchAPI(%q) = %t, want %t", test.host, got, test.want)
		}
	}
}

func TestDuplicateExactWorkflowCorrelationFailsClosed(t *testing.T) {
	config := dispatchTestConfig(t.TempDir())
	intent, err := newWorkflowDispatchIntent(config, "hive-visual-hive.yml", "main")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(writer, `{"total_count":2,"workflow_runs":[{"id":41,"display_title":%q,"event":"workflow_dispatch","head_branch":"main"},{"id":42,"display_title":%q,"event":"workflow_dispatch","head_branch":"main"}]}`, intent.ExpectedDisplayTitle, intent.ExpectedDisplayTitle)
	}))
	defer server.Close()
	client := hivegithub.NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	if selected, err := findCorrelatedWorkflowRun(context.Background(), client, "owner", "repo", "hive-visual-hive.yml", "main", intent); err == nil || selected != nil || !strings.Contains(err.Error(), "multiple run IDs") {
		t.Fatalf("duplicate exact correlation was not rejected: selected=%+v err=%v", selected, err)
	}
}

func TestProductionWorkflowRequiresExactDispatchCorrelation(t *testing.T) {
	value := workflow(Config{VisualHiveRepo: "owner/visual-hive", VisualHiveRef: strings.Repeat("a", 40), ACMMLevel: 4})
	for _, required := range []string{
		"hive_dispatch_id:",
		"required: true",
		"type: string",
		`run-name: "Hive Visual Hive Production [${{ inputs.hive_dispatch_id }}]"`,
		"  visual-hive-production:",
		"  visual-hive:",
		`test "$HIVE_EVENT_NAME" = "workflow_dispatch"`,
		`test "$HIVE_REF" = "refs/heads/$HIVE_DEFAULT_BRANCH"`,
		`ref: ${{ github.event.repository.default_branch }}`,
		`test "$(git rev-parse HEAD)" = "$HIVE_DISPATCH_SHA"`,
	} {
		if !strings.Contains(value, required) {
			t.Fatalf("production workflow missing exact dispatch correlation %q:\n%s", required, value)
		}
	}
	if strings.Count(value, "  visual-hive:\n") != 1 {
		t.Fatalf("production workflow must contain exactly one guarded PR-context seed:\n%s", value)
	}
	seed := value[strings.Index(value, "  # GitHub allows an App-bound context"):]
	if strings.Contains(seed, "\n    if:") || strings.Contains(seed, "\n      if:") {
		t.Fatalf("activation seed must fail actively instead of being skipped:\n%s", seed)
	}
}

func TestPullRequestWorkflowHasNoManualDispatchAndOwnsProtectedContext(t *testing.T) {
	value := pullRequestWorkflow(Config{VisualHiveRepo: "owner/visual-hive", VisualHiveRef: strings.Repeat("a", 40)})
	if strings.Contains(value, "workflow_dispatch") || !strings.Contains(value, "on:\n  pull_request:") || !strings.Contains(value, "  visual-hive:\n") {
		t.Fatalf("PR workflow triggers or check context are unsafe:\n%s", value)
	}
	for _, forbidden := range []string{"github.event_name", "github.ref }}", "origin/$HIVE_DEFAULT_BRANCH", "visual-hive-production:"} {
		if strings.Contains(value, forbidden) {
			t.Fatalf("PR workflow retains manual/production fallback %q:\n%s", forbidden, value)
		}
	}
}

func dispatchTestConfig(stateDir string) Config {
	return Config{
		Repository: "owner/repo", RepositoryID: "123", DefaultBranch: "main", StateDir: stateDir,
	}
}
