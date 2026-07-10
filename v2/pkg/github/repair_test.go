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
	"testing"
)

func TestUpsertRepairPullRequestCreatesThenUpdates(t *testing.T) {
	marker := "<!-- hive-repair: owner/repo:fingerprint -->"
	listCalls, createCalls, editCalls := 0, 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/pulls":
			listCalls++
			if createCalls == 0 {
				_, _ = io.WriteString(writer, `[]`)
			} else {
				_, _ = io.WriteString(writer, fmt.Sprintf(`[{"number":7,"html_url":"https://example.test/pull/7","body":%q,"head":{"sha":"abc","ref":"hive/repair"},"base":{"ref":"main"}}]`, marker))
			}
		case request.Method == http.MethodPost && request.URL.Path == "/repos/owner/repo/pulls":
			createCalls++
			_, _ = io.WriteString(writer, `{"number":7,"html_url":"https://example.test/pull/7","head":{"sha":"abc"}}`)
		case request.Method == http.MethodPatch && request.URL.Path == "/repos/owner/repo/pulls/7":
			editCalls++
			_, _ = io.WriteString(writer, `{"number":7,"html_url":"https://example.test/pull/7","head":{"sha":"def"}}`)
		default:
			http.Error(writer, request.Method+" "+request.URL.Path, http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	first, err := client.UpsertRepairPullRequest(context.Background(), "owner/repo", "hive/repair", "main", "Fix", marker+"\nRefs #3", marker)
	if err != nil || !first.Created || first.Number != 7 {
		t.Fatalf("first upsert = %+v, %v", first, err)
	}
	second, err := client.UpsertRepairPullRequest(context.Background(), "owner/repo", "hive/repair", "main", "Fix again", marker, marker)
	if err != nil || second.Created || second.HeadSHA != "def" {
		t.Fatalf("second upsert = %+v, %v", second, err)
	}
	if listCalls != 2 || createCalls != 1 || editCalls != 1 {
		t.Fatalf("unexpected calls list=%d create=%d edit=%d", listCalls, createCalls, editCalls)
	}
}

func TestUpsertReviewPullRequestIsDraftAndHoldLabeled(t *testing.T) {
	draft := false
	labelsAdded := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/pulls":
			_, _ = io.WriteString(writer, `[]`)
		case request.Method == http.MethodPost && request.URL.Path == "/repos/owner/repo/pulls":
			var body struct {
				Draft bool `json:"draft"`
			}
			_ = json.NewDecoder(request.Body).Decode(&body)
			draft = body.Draft
			_, _ = io.WriteString(writer, `{"number":7,"html_url":"https://example.test/pull/7","head":{"sha":"abc"}}`)
		case request.Method == http.MethodPost && request.URL.Path == "/repos/owner/repo/labels":
			_, _ = io.WriteString(writer, `{"name":"created"}`)
		case request.Method == http.MethodPost && request.URL.Path == "/repos/owner/repo/issues/7/labels":
			_ = json.NewDecoder(request.Body).Decode(&labelsAdded)
			_, _ = io.WriteString(writer, `[]`)
		default:
			http.Error(writer, request.Method+" "+request.URL.Path, http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	pull, err := client.UpsertReviewPullRequest(context.Background(), "owner/repo", "hive/baseline", "main", "Review baseline", "body", "<!-- marker -->")
	if err != nil {
		t.Fatal(err)
	}
	if !draft || pull.Number != 7 || len(labelsAdded) != 2 || labelsAdded[0] != "hold" || labelsAdded[1] != "hive/baseline-review" {
		t.Fatalf("review PR was not held: draft=%t pull=%+v labels=%v", draft, pull, labelsAdded)
	}
}

func TestUpsertRepairPullRequestRejectsDuplicateMarker(t *testing.T) {
	marker := "<!-- hive-repair: duplicate -->"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, fmt.Sprintf(`[{"number":1,"body":%q},{"number":2,"body":%q}]`, marker, marker))
	}))
	defer server.Close()
	client := NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	_, err := client.UpsertRepairPullRequest(context.Background(), "owner/repo", "branch", "main", "title", marker, marker)
	if err == nil || !strings.Contains(err.Error(), "multiple") {
		t.Fatalf("expected duplicate rejection, got %v", err)
	}
}

func TestUpsertRepairPullRequestRejectsMarkerOnAnotherBranch(t *testing.T) {
	marker := "<!-- hive-repair: stable -->"
	createCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if request.Method == http.MethodGet {
			_, _ = io.WriteString(writer, fmt.Sprintf(`[{"number":7,"body":%q,"head":{"ref":"hive/repair-original"},"base":{"ref":"main"}}]`, marker))
			return
		}
		createCalls++
		http.Error(writer, "must not create", http.StatusInternalServerError)
	}))
	defer server.Close()
	client := NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	_, err := client.UpsertRepairPullRequest(context.Background(), "owner/repo", "hive/repair-retry", "main", "title", marker, marker)
	if err == nil || !strings.Contains(err.Error(), "refusing duplicate branch") || createCalls != 0 {
		t.Fatalf("cross-branch duplicate was not rejected: calls=%d err=%v", createCalls, err)
	}
}

func TestListMergedBaselineBranchesRequiresLiveExactLabeledMerge(t *testing.T) {
	fingerprint := strings.Repeat("a", 64)
	repairHead := strings.Repeat("b", 40)
	marker := fmt.Sprintf("<!-- hive-baseline-review: %s:%s -->", fingerprint, repairHead)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/git/matching-refs/heads/hive/baseline-":
			_, _ = io.WriteString(writer, `[{"ref":"refs/heads/hive/baseline-proof","object":{"sha":"baseline-head","type":"commit"}}]`)
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/pulls" && request.URL.Query().Get("state") == "closed":
			_, _ = io.WriteString(writer, fmt.Sprintf(`[{"number":137,"body":%q,"head":{"ref":"hive/baseline-proof","sha":"baseline-head"},"base":{"ref":"main"},"labels":[{"name":"hive/baseline-review"}]}]`, marker))
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/pulls/137":
			_, _ = io.WriteString(writer, fmt.Sprintf(`{"number":137,"html_url":"https://example.test/pull/137","state":"closed","merged":true,"body":%q,"head":{"ref":"hive/baseline-proof","sha":"baseline-head"},"base":{"ref":"main"},"labels":[{"name":"hive/baseline-review"}]}`, marker))
		default:
			http.Error(writer, request.Method+" "+request.URL.Path, http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	branches, err := client.ListMergedBaselineBranches(context.Background(), "owner/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	if len(branches) != 1 || branches[0].PRNumber != 137 || branches[0].RepositoryFingerprint != fingerprint || branches[0].Branch != "hive/baseline-proof" || branches[0].HeadSHA != "baseline-head" {
		t.Fatalf("unexpected merged baseline branches: %+v", branches)
	}
}
