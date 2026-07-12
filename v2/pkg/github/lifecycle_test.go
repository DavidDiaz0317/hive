package github

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
)

func TestLifecycleIssueUpsertDeduplicatesByMarker(t *testing.T) {
	marker := "<!-- hive-visual-fingerprint: abc -->"
	created := false
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if serveLifecycleWriter(writer, request) {
			return
		}
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/issues":
			if created {
				_, _ = io.WriteString(writer, `[{"number":7,"html_url":"https://github.test/owner/repo/issues/7","body":"`+marker+`","user":{"login":"hive-writer"}}]`)
			} else {
				_, _ = io.WriteString(writer, `[]`)
			}
		case request.Method == http.MethodPost && request.URL.Path == "/repos/owner/repo/issues":
			created = true
			assertIssueRequest(t, request, "open")
			writer.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(writer, `{"number":7,"html_url":"https://github.test/owner/repo/issues/7","body":"`+marker+`\nbody","user":{"login":"hive-writer"}}`)
		case request.Method == http.MethodPatch && request.URL.Path == "/repos/owner/repo/issues/7":
			assertIssueRequest(t, request, "open")
			_, _ = io.WriteString(writer, `{"number":7,"html_url":"https://github.test/owner/repo/issues/7"}`)
		default:
			http.Error(writer, request.Method+" "+request.URL.Path, http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())

	firstNumber, _, firstCreated, err := client.UpsertLifecycleIssue(context.Background(), "owner/repo", marker, "Finding", marker+"\nbody", []string{"hive/active"})
	if err != nil {
		t.Fatal(err)
	}
	secondNumber, _, secondCreated, err := client.UpsertLifecycleIssue(context.Background(), "owner/repo", marker, "Finding", marker+"\nupdated", []string{"hive/active"})
	if err != nil {
		t.Fatal(err)
	}
	if !firstCreated || secondCreated || firstNumber != 7 || secondNumber != 7 {
		t.Fatalf("upsert did not deduplicate: first=%d/%t second=%d/%t", firstNumber, firstCreated, secondNumber, secondCreated)
	}
}

func TestLifecycleIssueUpsertAdoptsLegacyVisualHiveFingerprint(t *testing.T) {
	marker := "<!-- hive-visual-fingerprint: repository-hash -->"
	legacy := "<!-- visual-hive-issue dedupe:visual-hive:owner:repo:finding -->"
	created, adopted := false, false
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if serveLifecycleWriter(writer, request) {
			return
		}
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/issues":
			body := legacy
			if adopted {
				body = marker + "\n" + legacy
			}
			body = strings.ReplaceAll(body, "\n", `\n`)
			_, _ = io.WriteString(writer, `[{"number":11,"html_url":"https://github.test/owner/repo/issues/11","body":"`+body+`","user":{"login":"hive-writer"}}]`)
		case request.Method == http.MethodPatch && request.URL.Path == "/repos/owner/repo/issues/11":
			assertIssueRequest(t, request, "open")
			adopted = true
			_, _ = io.WriteString(writer, `{"number":11,"html_url":"https://github.test/owner/repo/issues/11"}`)
		case request.Method == http.MethodPost:
			created = true
			http.Error(writer, "must adopt", http.StatusInternalServerError)
		default:
			http.Error(writer, request.Method+" "+request.URL.Path, http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())

	number, _, wasCreated, err := client.UpsertLifecycleIssue(context.Background(), "owner/repo", marker, "Finding", marker+"\n"+legacy+"\nbody", []string{"hive/active"})

	if err != nil || number != 11 || wasCreated || created {
		t.Fatalf("legacy issue was not adopted: number=%d created=%t post=%t err=%v", number, wasCreated, created, err)
	}
}

func TestUpdateLifecycleIssueClosesExplicitly(t *testing.T) {
	marker := "<!-- hive-visual-fingerprint: close-proof -->"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if serveLifecycleWriter(writer, request) {
			return
		}
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/issues/9":
			_, _ = io.WriteString(writer, `{"number":9,"title":"Resolved","html_url":"https://github.test/owner/repo/issues/9","body":"`+marker+`","user":{"login":"hive-writer"}}`)
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/issues":
			_, _ = io.WriteString(writer, `[{"number":9,"title":"Resolved","html_url":"https://github.test/owner/repo/issues/9","body":"`+marker+`","user":{"login":"hive-writer"}}]`)
		case request.Method == http.MethodPatch && request.URL.Path == "/repos/owner/repo/issues/9":
			assertIssueRequest(t, request, "closed")
			_, _ = io.WriteString(writer, `{"number":9,"html_url":"https://github.test/owner/repo/issues/9"}`)
		default:
			http.Error(writer, "unexpected request", http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	number, _, err := client.UpdateLifecycleIssue(context.Background(), "owner/repo", 9, "Resolved", marker+"\nevidence", "closed", []string{"hive/resolved"})
	if err != nil || number != 9 {
		t.Fatalf("close failed: issue=%d err=%v", number, err)
	}
}

func TestLifecycleIssueUpsertReconcilesConcurrentDuplicateCreate(t *testing.T) {
	marker := "<!-- hive-visual-fingerprint: concurrent-proof -->"
	states := map[int]string{7: "open", 8: "open"}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if serveLifecycleWriter(writer, request) {
			return
		}
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/issues":
			_, _ = io.WriteString(writer, `[{"number":8,"title":"Finding duplicate","html_url":"https://github.test/owner/repo/issues/8","body":"`+marker+`","user":{"login":"hive-writer"}},{"number":7,"title":"Finding","html_url":"https://github.test/owner/repo/issues/7","body":"`+marker+`","user":{"login":"hive-writer"}}]`)
		case request.Method == http.MethodPatch && (request.URL.Path == "/repos/owner/repo/issues/7" || request.URL.Path == "/repos/owner/repo/issues/8"):
			var body struct {
				State string `json:"state"`
			}
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			number := 7
			if strings.HasSuffix(request.URL.Path, "/8") {
				number = 8
			}
			states[number] = body.State
			_, _ = fmt.Fprintf(writer, `{"number":%d,"html_url":"https://github.test/owner/repo/issues/%d"}`, number, number)
		default:
			http.Error(writer, request.Method+" "+request.URL.Path, http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())

	number, _, created, err := client.UpsertLifecycleIssue(context.Background(), "owner/repo", marker, "Finding", marker+"\nbody", []string{"hive/managed", "hive/active"})
	if err != nil {
		t.Fatal(err)
	}
	if created || number != 7 || states[7] != "open" || states[8] != "closed" {
		t.Fatalf("duplicate issue race did not converge to lowest canonical issue: number=%d created=%t states=%v", number, created, states)
	}
}

func TestLifecycleIssueConcurrentListThenCreateRaceConverges(t *testing.T) {
	marker := "<!-- hive-visual-fingerprint: simultaneous-proof -->"
	type raceIssue struct {
		Number  int               `json:"number"`
		Title   string            `json:"title"`
		Body    string            `json:"body"`
		State   string            `json:"state"`
		HTMLURL string            `json:"html_url"`
		Labels  []string          `json:"labels,omitempty"`
		User    map[string]string `json:"user,omitempty"`
	}
	var mu sync.Mutex
	issues := map[int]*raceIssue{}
	initialGets, creates := 0, 0
	initialReady, createsReady := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if serveLifecycleWriter(writer, request) {
			return
		}
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/issues":
			mu.Lock()
			if initialGets < 2 && len(issues) == 0 {
				initialGets++
				if initialGets == 2 {
					close(initialReady)
				}
				mu.Unlock()
				<-initialReady
				_, _ = io.WriteString(writer, `[]`)
				return
			}
			values := make([]*raceIssue, 0, len(issues))
			for _, issue := range issues {
				copy := *issue
				copy.Labels = nil
				values = append(values, &copy)
			}
			mu.Unlock()
			if err := json.NewEncoder(writer).Encode(values); err != nil {
				t.Error(err)
			}
		case request.Method == http.MethodPost && request.URL.Path == "/repos/owner/repo/issues":
			var body raceIssue
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			number := 7 + creates
			creates++
			body.Number, body.HTMLURL = number, fmt.Sprintf("https://github.test/owner/repo/issues/%d", number)
			body.User = map[string]string{"login": "hive-writer"}
			issues[number] = &body
			if creates == 2 {
				close(createsReady)
			}
			mu.Unlock()
			<-createsReady
			writer.WriteHeader(http.StatusCreated)
			response := body
			response.Labels = nil
			_ = json.NewEncoder(writer).Encode(response)
		case request.Method == http.MethodPatch && strings.HasPrefix(request.URL.Path, "/repos/owner/repo/issues/"):
			var body raceIssue
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Error(err)
				return
			}
			number := 7
			if strings.HasSuffix(request.URL.Path, "/8") {
				number = 8
			}
			body.User = map[string]string{"login": "hive-writer"}
			mu.Lock()
			body.Number, body.HTMLURL = number, fmt.Sprintf("https://github.test/owner/repo/issues/%d", number)
			issues[number] = &body
			mu.Unlock()
			body.Labels = nil
			_ = json.NewEncoder(writer).Encode(body)
		default:
			http.Error(writer, request.Method+" "+request.URL.Path, http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	type result struct {
		number  int
		created bool
		err     error
	}
	results := make(chan result, 2)
	for range 2 {
		go func() {
			number, _, created, err := client.UpsertLifecycleIssue(context.Background(), "owner/repo", marker, "Finding", marker+"\nbody", []string{"hive/managed", "hive/active"})
			results <- result{number: number, created: created, err: err}
		}()
	}
	createdCount := 0
	for range 2 {
		result := <-results
		if result.err != nil || result.number != 7 {
			t.Fatalf("concurrent lifecycle create did not converge: number=%d created=%t err=%v", result.number, result.created, result.err)
		}
		if result.created {
			createdCount++
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if createdCount != 1 || issues[7].State != "open" || issues[8].State != "closed" {
		t.Fatalf("concurrent lifecycle race left duplicates: created=%d issues=%+v", createdCount, issues)
	}
}

func TestLifecycleIssueIgnoresForeignMarkerPreclaim(t *testing.T) {
	marker := "<!-- hive-visual-fingerprint: public-preclaim-proof -->"
	created, foreignTouched := false, false
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if serveLifecycleWriter(writer, request) {
			return
		}
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/issues":
			foreign := `{"number":1,"title":"spoof","body":"` + marker + `","user":{"login":"attacker"}}`
			if created {
				_, _ = io.WriteString(writer, `[`+foreign+`,{"number":7,"title":"Finding","html_url":"https://github.test/owner/repo/issues/7","body":"`+marker+`\nbody","user":{"login":"hive-writer"}}]`)
			} else {
				_, _ = io.WriteString(writer, `[`+foreign+`]`)
			}
		case request.Method == http.MethodPost && request.URL.Path == "/repos/owner/repo/issues":
			created = true
			assertIssueRequest(t, request, "open")
			writer.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(writer, `{"number":7,"html_url":"https://github.test/owner/repo/issues/7","body":"`+marker+`\nbody","user":{"login":"hive-writer"}}`)
		case request.Method == http.MethodPatch && request.URL.Path == "/repos/owner/repo/issues/7":
			assertIssueRequest(t, request, "open")
			_, _ = io.WriteString(writer, `{"number":7,"html_url":"https://github.test/owner/repo/issues/7"}`)
		case request.Method == http.MethodPatch && request.URL.Path == "/repos/owner/repo/issues/1":
			foreignTouched = true
			http.Error(writer, "foreign issue must not be mutated", http.StatusInternalServerError)
		default:
			http.Error(writer, request.Method+" "+request.URL.Path, http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	number, _, wasCreated, err := client.UpsertLifecycleIssue(context.Background(), "owner/repo", marker, "Finding", marker+"\nbody", []string{"hive/managed", "hive/active"})
	if err != nil || !wasCreated || number != 7 || foreignTouched {
		t.Fatalf("foreign marker preclaim controlled lifecycle issue: number=%d created=%t foreign_touched=%t err=%v", number, wasCreated, foreignTouched, err)
	}
}

func TestUpdateLifecycleIssueRejectsForeignAuthoredStateTarget(t *testing.T) {
	marker := "<!-- hive-visual-fingerprint: foreign-state-proof -->"
	patched := false
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if serveLifecycleWriter(writer, request) {
			return
		}
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/issues/4":
			_, _ = io.WriteString(writer, `{"number":4,"body":"`+marker+`","user":{"login":"attacker"}}`)
		case request.Method == http.MethodPatch:
			patched = true
			http.Error(writer, "must not patch foreign issue", http.StatusInternalServerError)
		default:
			http.Error(writer, request.Method+" "+request.URL.Path, http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	_, _, err := client.UpdateLifecycleIssue(context.Background(), "owner/repo", 4, "Resolved", marker+"\nevidence", "closed", []string{"hive/resolved"})
	if err == nil || !strings.Contains(err.Error(), "not authored") || patched {
		t.Fatalf("foreign lifecycle target was not rejected before mutation: patched=%t err=%v", patched, err)
	}
}

func serveLifecycleWriter(writer http.ResponseWriter, request *http.Request) bool {
	if request.Method != http.MethodGet || request.URL.Path != "/user" {
		return false
	}
	_, _ = io.WriteString(writer, `{"login":"hive-writer"}`)
	return true
}

func assertIssueRequest(t *testing.T, request *http.Request, expectedState string) {
	t.Helper()
	var body struct {
		Title  string   `json:"title"`
		Body   string   `json:"body"`
		State  string   `json:"state"`
		Labels []string `json:"labels"`
	}
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.State != expectedState || strings.TrimSpace(body.Title) == "" || len(body.Labels) == 0 {
		t.Fatalf("unexpected issue request: %+v", body)
	}
}
