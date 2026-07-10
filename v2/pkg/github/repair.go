package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	gh "github.com/google/go-github/v72/github"
)

type RepairPullRequest struct {
	Number  int    `json:"number"`
	URL     string `json:"url"`
	HeadSHA string `json:"head_sha"`
	Created bool   `json:"created"`
}

// UpsertRepairPullRequest creates or updates exactly one open repair PR for a
// Hive-owned branch. The marker makes a retry after an ambiguous API response
// idempotent without relying on the local lifecycle transaction having
// completed.
func (c *Client) UpsertRepairPullRequest(ctx context.Context, repository, branch, base, title, body, marker string) (RepairPullRequest, error) {
	return c.upsertHivePullRequest(ctx, repository, branch, base, title, body, marker, false)
}

// UpsertReviewPullRequest creates a draft, hold-labeled PR for an operation
// that requires explicit human authority, such as adding visual baselines.
// Hive never promotes the draft or merges it automatically.
func (c *Client) UpsertReviewPullRequest(ctx context.Context, repository, branch, base, title, body, marker string) (RepairPullRequest, error) {
	pull, err := c.upsertHivePullRequest(ctx, repository, branch, base, title, body, marker, true)
	if err != nil {
		return RepairPullRequest{}, err
	}
	owner, repo, err := splitFullRepository(repository)
	if err != nil {
		return RepairPullRequest{}, err
	}
	for name, color := range map[string]string{"hold": "B60205", "hive/baseline-review": "D4C5F9"} {
		if err := c.ensureLabel(ctx, owner, repo, name, color); err != nil {
			return RepairPullRequest{}, err
		}
	}
	if _, _, err := c.client.Issues.AddLabelsToIssue(ctx, owner, repo, pull.Number, []string{"hold", "hive/baseline-review"}); err != nil {
		return RepairPullRequest{}, fmt.Errorf("label review pull request: %w", err)
	}
	return pull, nil
}

func (c *Client) upsertHivePullRequest(ctx context.Context, repository, branch, base, title, body, marker string, draft bool) (RepairPullRequest, error) {
	owner, repo, err := splitFullRepository(repository)
	if err != nil {
		return RepairPullRequest{}, err
	}
	if strings.TrimSpace(branch) == "" || strings.TrimSpace(base) == "" || strings.TrimSpace(marker) == "" {
		return RepairPullRequest{}, fmt.Errorf("repair branch, base branch, and marker are required")
	}
	pulls, _, err := c.client.PullRequests.List(ctx, owner, repo, &gh.PullRequestListOptions{
		State: "open", Head: owner + ":" + branch, Base: base, ListOptions: gh.ListOptions{PerPage: 100},
	})
	if err != nil {
		return RepairPullRequest{}, fmt.Errorf("list repair pull requests: %w", err)
	}
	var matched *gh.PullRequest
	for _, pull := range pulls {
		if strings.Contains(pull.GetBody(), marker) {
			if matched != nil {
				return RepairPullRequest{}, fmt.Errorf("multiple open repair pull requests contain marker %q", marker)
			}
			matched = pull
		}
	}
	if matched != nil {
		updated, _, err := c.client.PullRequests.Edit(ctx, owner, repo, matched.GetNumber(), &gh.PullRequest{Title: gh.Ptr(title), Body: gh.Ptr(body), Base: &gh.PullRequestBranch{Ref: gh.Ptr(base)}})
		if err != nil {
			return RepairPullRequest{}, fmt.Errorf("update repair pull request: %w", err)
		}
		return RepairPullRequest{Number: updated.GetNumber(), URL: updated.GetHTMLURL(), HeadSHA: updated.GetHead().GetSHA()}, nil
	}
	created, _, err := c.client.PullRequests.Create(ctx, owner, repo, &gh.NewPullRequest{
		Title: gh.Ptr(title), Head: gh.Ptr(branch), Base: gh.Ptr(base), Body: gh.Ptr(body), Draft: gh.Ptr(draft), MaintainerCanModify: gh.Ptr(true),
	})
	if err != nil {
		return RepairPullRequest{}, fmt.Errorf("create repair pull request: %w", err)
	}
	return RepairPullRequest{Number: created.GetNumber(), URL: created.GetHTMLURL(), HeadSHA: created.GetHead().GetSHA(), Created: true}, nil
}

func (c *Client) ensureLabel(ctx context.Context, owner, repo, name, color string) error {
	_, _, err := c.client.Issues.CreateLabel(ctx, owner, repo, &gh.Label{Name: gh.Ptr(name), Color: gh.Ptr(color)})
	if err == nil {
		return nil
	}
	var response *gh.ErrorResponse
	if errors.As(err, &response) && response.Response != nil && (response.Response.StatusCode == http.StatusUnprocessableEntity || response.Response.StatusCode == http.StatusAlreadyReported) {
		return nil
	}
	return fmt.Errorf("ensure label %q: %w", name, err)
}
