package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	gh "github.com/google/go-github/v72/github"
)

type RepairPullRequest struct {
	Number  int    `json:"number"`
	URL     string `json:"url"`
	HeadSHA string `json:"head_sha"`
	Created bool   `json:"created"`
}

type OpenRepairPullRequest struct {
	Number  int    `json:"number"`
	URL     string `json:"url"`
	Branch  string `json:"branch"`
	HeadSHA string `json:"head_sha"`
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
	pulls, err := c.listOpenPullRequests(ctx, owner, repo, base)
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
		if matched.GetHead().GetRef() != branch {
			return RepairPullRequest{}, fmt.Errorf("open repair pull request #%d already owns marker %q on branch %s; refusing duplicate branch %s", matched.GetNumber(), marker, matched.GetHead().GetRef(), branch)
		}
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

// ListOpenRepairPullRequests returns every open PR carrying one exact Hive
// fingerprint marker, regardless of branch. Callers use this to repair legacy
// duplicate state and to prove the one-active-PR invariant.
func (c *Client) ListOpenRepairPullRequests(ctx context.Context, repository, base, marker string) ([]OpenRepairPullRequest, error) {
	owner, repo, err := splitFullRepository(repository)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(base) == "" || strings.TrimSpace(marker) == "" {
		return nil, fmt.Errorf("base branch and repair marker are required")
	}
	pulls, err := c.listOpenPullRequests(ctx, owner, repo, base)
	if err != nil {
		return nil, err
	}
	result := make([]OpenRepairPullRequest, 0)
	for _, pull := range pulls {
		if strings.Contains(pull.GetBody(), marker) {
			result = append(result, OpenRepairPullRequest{Number: pull.GetNumber(), URL: pull.GetHTMLURL(), Branch: pull.GetHead().GetRef(), HeadSHA: pull.GetHead().GetSHA()})
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Number < result[j].Number })
	return result, nil
}

// CloseRepairPullRequestExact closes only an open Hive repair PR whose marker,
// branch, and head still match the caller's durable observation.
func (c *Client) CloseRepairPullRequestExact(ctx context.Context, repository string, number int, marker, expectedBranch, expectedHeadSHA string) error {
	owner, repo, err := splitFullRepository(repository)
	if err != nil {
		return err
	}
	if number <= 0 || strings.TrimSpace(marker) == "" || strings.TrimSpace(expectedBranch) == "" || strings.TrimSpace(expectedHeadSHA) == "" {
		return fmt.Errorf("repair PR number, marker, branch, and exact head are required")
	}
	pull, _, err := c.client.PullRequests.Get(ctx, owner, repo, number)
	if err != nil {
		return fmt.Errorf("get repair pull request #%d: %w", number, err)
	}
	if !strings.Contains(pull.GetBody(), marker) || pull.GetHead().GetRef() != expectedBranch || pull.GetHead().GetSHA() != expectedHeadSHA {
		return fmt.Errorf("refusing to close repair pull request #%d because its marker, branch, or head changed", number)
	}
	if pull.GetState() != "open" {
		return nil
	}
	if _, _, err := c.client.PullRequests.Edit(ctx, owner, repo, number, &gh.PullRequest{State: gh.Ptr("closed")}); err != nil {
		return fmt.Errorf("close repair pull request #%d: %w", number, err)
	}
	return nil
}

func (c *Client) listOpenPullRequests(ctx context.Context, owner, repo, base string) ([]*gh.PullRequest, error) {
	result := make([]*gh.PullRequest, 0)
	options := &gh.PullRequestListOptions{State: "open", Base: base, ListOptions: gh.ListOptions{PerPage: 100}}
	for {
		pulls, response, err := c.client.PullRequests.List(ctx, owner, repo, options)
		if err != nil {
			return nil, err
		}
		result = append(result, pulls...)
		if response.NextPage == 0 {
			return result, nil
		}
		options.Page = response.NextPage
	}
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
