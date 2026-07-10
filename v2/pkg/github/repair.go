package github

import (
	"context"
	"encoding/hex"
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

type MergedBaselineBranch struct {
	PRNumber              int    `json:"pr_number"`
	PRURL                 string `json:"pr_url"`
	RepositoryFingerprint string `json:"repository_fingerprint"`
	Branch                string `json:"branch"`
	HeadSHA               string `json:"head_sha"`
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
	return c.listPullRequestsByState(ctx, owner, repo, base, "open")
}

// ListMergedBaselineBranches returns only still-existing Hive baseline refs
// that are proven to be the exact heads of merged, labeled baseline-review PRs.
// This recovers branches created by older Hive versions even if their local
// repair checkpoint was later superseded.
func (c *Client) ListMergedBaselineBranches(ctx context.Context, repository, base string) ([]MergedBaselineBranch, error) {
	owner, repo, err := splitFullRepository(repository)
	if err != nil {
		return nil, err
	}
	refs := map[string]string{}
	refOptions := &gh.ReferenceListOptions{Ref: "heads/hive/baseline-", ListOptions: gh.ListOptions{PerPage: 100}}
	for {
		page, response, err := c.client.Git.ListMatchingRefs(ctx, owner, repo, refOptions)
		if err != nil {
			return nil, fmt.Errorf("list Hive baseline refs: %w", err)
		}
		for _, ref := range page {
			branch := strings.TrimPrefix(ref.GetRef(), "refs/heads/")
			if strings.HasPrefix(branch, "hive/baseline-") && ref.GetObject().GetSHA() != "" {
				refs[branch] = ref.GetObject().GetSHA()
			}
		}
		if response.NextPage == 0 {
			break
		}
		refOptions.Page = response.NextPage
	}
	if len(refs) == 0 {
		return nil, nil
	}

	pulls, err := c.listPullRequestsByState(ctx, owner, repo, base, "closed")
	if err != nil {
		return nil, fmt.Errorf("list closed baseline review pull requests: %w", err)
	}
	byBranch := map[string]MergedBaselineBranch{}
	for _, summary := range pulls {
		branch := summary.GetHead().GetRef()
		expectedSHA, exists := refs[branch]
		if !exists || !hasExactLabel(summary.Labels, "hive/baseline-review") {
			continue
		}
		pull, _, err := c.client.PullRequests.Get(ctx, owner, repo, summary.GetNumber())
		if err != nil {
			return nil, fmt.Errorf("get baseline review pull request #%d: %w", summary.GetNumber(), err)
		}
		fingerprint, marked := baselineReviewFingerprint(pull.GetBody())
		if !pull.GetMerged() || pull.GetHead().GetRef() != branch || pull.GetHead().GetSHA() != expectedSHA || !hasExactLabel(pull.Labels, "hive/baseline-review") || !marked {
			continue
		}
		if _, duplicate := byBranch[branch]; duplicate {
			return nil, fmt.Errorf("multiple merged baseline review PRs claim branch %s", branch)
		}
		byBranch[branch] = MergedBaselineBranch{PRNumber: pull.GetNumber(), PRURL: pull.GetHTMLURL(), RepositoryFingerprint: fingerprint, Branch: branch, HeadSHA: expectedSHA}
	}
	if len(byBranch) != len(refs) {
		return nil, fmt.Errorf("a live Hive baseline branch lacks an exact merged baseline-review PR")
	}
	branches := make([]MergedBaselineBranch, 0, len(byBranch))
	for _, branch := range byBranch {
		branches = append(branches, branch)
	}
	sort.Slice(branches, func(i, j int) bool { return branches[i].PRNumber < branches[j].PRNumber })
	return branches, nil
}

func (c *Client) listPullRequestsByState(ctx context.Context, owner, repo, base, state string) ([]*gh.PullRequest, error) {
	result := make([]*gh.PullRequest, 0)
	options := &gh.PullRequestListOptions{State: state, Base: base, ListOptions: gh.ListOptions{PerPage: 100}}
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

func hasExactLabel(labels []*gh.Label, expected string) bool {
	for _, label := range labels {
		if strings.EqualFold(strings.TrimSpace(label.GetName()), expected) {
			return true
		}
	}
	return false
}

func baselineReviewFingerprint(body string) (string, bool) {
	const prefix = "<!-- hive-baseline-review: "
	line := strings.TrimSpace(strings.SplitN(body, "\n", 2)[0])
	if !strings.HasPrefix(line, prefix) || !strings.HasSuffix(line, " -->") {
		return "", false
	}
	payload := strings.TrimSuffix(strings.TrimPrefix(line, prefix), " -->")
	parts := strings.Split(payload, ":")
	if len(parts) != 2 || len(parts[0]) != 64 || len(parts[1]) != 40 {
		return "", false
	}
	if _, err := hex.DecodeString(parts[0]); err != nil {
		return "", false
	}
	if _, err := hex.DecodeString(parts[1]); err != nil {
		return "", false
	}
	return parts[0], true
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
