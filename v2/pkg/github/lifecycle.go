package github

import (
	"context"
	"fmt"
	"sort"
	"strings"

	gh "github.com/google/go-github/v72/github"
)

// UpsertLifecycleIssue creates or updates the one GitHub issue carrying the
// repository-scoped lifecycle marker. Searching by marker before creation makes
// retries safe even when GitHub accepted a write before Hive persisted it.
func (c *Client) UpsertLifecycleIssue(ctx context.Context, repository, marker, title, body string, labels []string) (int, string, bool, error) {
	owner, repo, err := splitFullRepository(repository)
	if err != nil {
		return 0, "", false, err
	}
	if strings.TrimSpace(marker) == "" || strings.TrimSpace(title) == "" || strings.TrimSpace(body) == "" {
		return 0, "", false, fmt.Errorf("lifecycle marker, title, and body are required")
	}
	writer, err := c.AuthenticatedLogin(ctx)
	if err != nil {
		return 0, "", false, err
	}
	issueNumber, err := c.findLifecycleIssue(ctx, owner, repo, marker, writer)
	if err != nil {
		return 0, "", false, err
	}
	if issueNumber == 0 {
		if legacyMarker := legacyVisualHiveMarker(body); legacyMarker != "" {
			issueNumber, err = c.findLifecycleIssue(ctx, owner, repo, legacyMarker, writer)
			if err != nil {
				return 0, "", false, err
			}
		}
	}
	request := &gh.IssueRequest{Title: gh.Ptr(title), Body: gh.Ptr(body), Labels: &labels, State: gh.Ptr("open")}
	if issueNumber > 0 {
		issue, _, err := c.client.Issues.Edit(ctx, owner, repo, issueNumber, request)
		if err != nil {
			return 0, "", false, fmt.Errorf("update lifecycle issue #%d: %w", issueNumber, err)
		}
		canonical, err := c.reconcileLifecycleIssues(ctx, owner, repo, marker, writer, request, issue)
		if err != nil {
			return 0, "", false, err
		}
		return canonical.GetNumber(), canonical.GetHTMLURL(), false, nil
	}
	issue, _, err := c.client.Issues.Create(ctx, owner, repo, request)
	if err != nil {
		return 0, "", false, fmt.Errorf("create lifecycle issue: %w", err)
	}
	canonical, err := c.reconcileLifecycleIssues(ctx, owner, repo, marker, writer, request, issue)
	if err != nil {
		return 0, "", false, err
	}
	return canonical.GetNumber(), canonical.GetHTMLURL(), canonical.GetNumber() == issue.GetNumber(), nil
}

func legacyVisualHiveMarker(body string) string {
	const prefix = "<!-- visual-hive-issue dedupe:"
	start := strings.Index(body, prefix)
	if start < 0 {
		return ""
	}
	end := strings.Index(body[start:], "-->")
	if end < 0 {
		return ""
	}
	return strings.TrimSpace(body[start : start+end+3])
}

func (c *Client) UpdateLifecycleIssue(ctx context.Context, repository string, number int, title, body, state string, labels []string) (int, string, error) {
	owner, repo, err := splitFullRepository(repository)
	if err != nil {
		return 0, "", err
	}
	if number <= 0 || (state != "open" && state != "closed") {
		return 0, "", fmt.Errorf("valid lifecycle issue number and state are required")
	}
	marker := lifecycleMarkerFromBody(body)
	if marker == "" {
		return 0, "", fmt.Errorf("lifecycle issue update is missing its exact Hive marker")
	}
	writer, err := c.AuthenticatedLogin(ctx)
	if err != nil {
		return 0, "", err
	}
	existing, _, err := c.client.Issues.Get(ctx, owner, repo, number)
	if err != nil {
		return 0, "", fmt.Errorf("read lifecycle issue #%d before update: %w", number, err)
	}
	if !isOwnedLifecycleIssue(existing, marker, writer) {
		return 0, "", fmt.Errorf("lifecycle issue #%d is not authored by the authenticated Hive writer with the exact marker", number)
	}
	request := &gh.IssueRequest{Title: gh.Ptr(title), Body: gh.Ptr(body), Labels: &labels, State: gh.Ptr(state)}
	issue, _, err := c.client.Issues.Edit(ctx, owner, repo, number, request)
	if err != nil {
		return 0, "", fmt.Errorf("update lifecycle issue #%d: %w", number, err)
	}
	canonical, err := c.reconcileLifecycleIssues(ctx, owner, repo, marker, writer, request, issue)
	if err != nil {
		return 0, "", err
	}
	return canonical.GetNumber(), canonical.GetHTMLURL(), nil
}

func (c *Client) findLifecycleIssue(ctx context.Context, owner, repo, marker, writer string) (int, error) {
	issues, err := c.findLifecycleIssues(ctx, owner, repo, marker, writer)
	if err != nil || len(issues) == 0 {
		return 0, err
	}
	return issues[0].GetNumber(), nil
}

func (c *Client) findLifecycleIssues(ctx context.Context, owner, repo, marker, writer string) ([]*gh.Issue, error) {
	matches := []*gh.Issue{}
	page := 1
	for page > 0 {
		issues, response, err := c.client.Issues.ListByRepo(ctx, owner, repo, &gh.IssueListByRepoOptions{
			State: "all", ListOptions: gh.ListOptions{Page: page, PerPage: 100},
		})
		if err != nil {
			return nil, fmt.Errorf("list lifecycle issues: %w", err)
		}
		for _, issue := range issues {
			if issue.IsPullRequest() {
				continue
			}
			if isOwnedLifecycleIssue(issue, marker, writer) {
				matches = append(matches, issue)
			}
		}
		if response == nil {
			break
		}
		page = response.NextPage
	}
	sort.Slice(matches, func(i, j int) bool { return matches[i].GetNumber() < matches[j].GetNumber() })
	return matches, nil
}

// reconcileLifecycleIssues is the compensating half of issue creation. GitHub
// has no conditional-create primitive for issues, so two independent Hive
// state directories can both win a list-then-create race. Every create/update
// deterministically elects the lowest exact-marker issue, reopens or closes it
// to the requested state, and closes every higher-numbered duplicate. A later
// production update repeats this reconciliation, covering API list visibility
// lag after concurrent creates.
func (c *Client) reconcileLifecycleIssues(ctx context.Context, owner, repo, marker, writer string, desired *gh.IssueRequest, fallback *gh.Issue) (*gh.Issue, error) {
	matches, err := c.findLifecycleIssues(ctx, owner, repo, marker, writer)
	if err != nil {
		return nil, err
	}
	if len(matches) == 0 {
		if fallback == nil || fallback.GetNumber() <= 0 || !isOwnedLifecycleIssue(fallback, marker, writer) {
			return nil, fmt.Errorf("GitHub did not return the lifecycle issue after mutation")
		}
		return fallback, nil
	}
	canonical := matches[0]
	canonical, _, err = c.client.Issues.Edit(ctx, owner, repo, canonical.GetNumber(), desired)
	if err != nil {
		return nil, fmt.Errorf("reconcile canonical lifecycle issue #%d: %w", matches[0].GetNumber(), err)
	}
	for _, duplicate := range matches[1:] {
		duplicateBody := duplicate.GetBody()
		if start := strings.Index(duplicateBody, "\n\n---\nHive automatically closed this duplicate lifecycle issue."); start >= 0 {
			duplicateBody = duplicateBody[:start]
		}
		duplicateBody = strings.TrimSpace(duplicateBody) + fmt.Sprintf("\n\n---\nHive automatically closed this duplicate lifecycle issue. Canonical issue: #%d.\n<!-- hive-duplicate-of: %d -->", canonical.GetNumber(), canonical.GetNumber())
		labels := duplicateLifecycleLabels(desired.GetLabels())
		request := &gh.IssueRequest{
			Title: gh.Ptr(duplicate.GetTitle()), Body: gh.Ptr(duplicateBody), Labels: &labels, State: gh.Ptr("closed"),
		}
		if _, _, err := c.client.Issues.Edit(ctx, owner, repo, duplicate.GetNumber(), request); err != nil {
			return nil, fmt.Errorf("close duplicate lifecycle issue #%d in favor of #%d: %w", duplicate.GetNumber(), canonical.GetNumber(), err)
		}
		if c.logger != nil {
			c.logger.Warn("closed duplicate Hive lifecycle issue", "canonical", canonical.GetNumber(), "duplicate", duplicate.GetNumber())
		}
	}
	return canonical, nil
}

func isOwnedLifecycleIssue(issue *gh.Issue, marker, writer string) bool {
	if issue == nil || issue.IsPullRequest() || !strings.EqualFold(strings.TrimSpace(issue.GetUser().GetLogin()), strings.TrimSpace(writer)) {
		return false
	}
	count := 0
	for _, line := range strings.Split(strings.ReplaceAll(issue.GetBody(), "\r\n", "\n"), "\n") {
		if strings.TrimSpace(line) == marker {
			count++
		}
	}
	return count == 1
}

func lifecycleMarkerFromBody(body string) string {
	const prefix = "<!-- hive-visual-fingerprint:"
	start := strings.Index(body, prefix)
	if start < 0 {
		return ""
	}
	end := strings.Index(body[start:], "-->")
	if end < 0 {
		return ""
	}
	marker := strings.TrimSpace(body[start : start+end+3])
	if strings.Count(body, marker) != 1 {
		return ""
	}
	return marker
}

func duplicateLifecycleLabels(desired []string) []string {
	values := map[string]bool{"hive/managed": true, "hive/resolved": true}
	for _, label := range desired {
		label = strings.TrimSpace(label)
		if label != "" && label != "hive/active" && label != "visual-hive/ready-for-hive" && label != "visual-hive/still-active" {
			values[label] = true
		}
	}
	labels := make([]string, 0, len(values))
	for label := range values {
		labels = append(labels, label)
	}
	sort.Strings(labels)
	return labels
}

func splitFullRepository(repository string) (string, string, error) {
	parts := strings.Split(strings.TrimSpace(repository), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("repository must be owner/name")
	}
	return parts[0], parts[1], nil
}
