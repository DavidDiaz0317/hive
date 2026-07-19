package integrated

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSynchronizeNormalVisualWorkBaseFetchesExactHeadWithoutMovingCheckout(t *testing.T) {
	const repository = "owner/repo"
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	seed := filepath.Join(root, "seed")
	runIntegratedGit(t, root, "init", "--bare", remote)
	runIntegratedGit(t, root, "init", "-b", "main", seed)
	writeFixture(t, seed, "README.md", "initial\n")
	runIntegratedGit(t, seed, "add", "README.md")
	runIntegratedGit(t, seed, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "initial")
	runIntegratedGit(t, seed, "remote", "add", "origin", remote)
	runIntegratedGit(t, seed, "push", "-u", "origin", "main")
	runIntegratedGit(t, remote, "symbolic-ref", "HEAD", "refs/heads/main")
	bindTestRepositoryCloneURL(t, remote)

	checkout := filepath.Join(root, "state", "integrated", "checkout")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if branch, err := ensureCheckout(ctx, repository, checkout); err != nil || branch != "main" {
		t.Fatalf("create managed checkout = %q, %v", branch, err)
	}
	initialHead := strings.TrimSpace(integratedGitOutput(t, checkout, "rev-parse", "HEAD"))

	writeFixture(t, seed, "defect.txt", "prepared defect\n")
	runIntegratedGit(t, seed, "add", "defect.txt")
	runIntegratedGit(t, seed, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "prepared defect")
	runIntegratedGit(t, seed, "push", "origin", "main")
	verifiedHead := strings.TrimSpace(integratedGitOutput(t, seed, "rev-parse", "HEAD"))
	if initialHead == verifiedHead {
		t.Fatal("fixture did not advance the remote default branch")
	}
	if _, err := integratedGitOutputError(checkout, "cat-file", "-e", verifiedHead+"^{commit}"); err == nil {
		t.Fatal("managed checkout unexpectedly had the verified head before synchronization")
	}

	config := Config{Repository: repository, DefaultBranch: "main", CheckoutDir: checkout}
	tree, err := synchronizeNormalVisualWorkBase(ctx, config, verifiedHead)
	if err != nil {
		t.Fatal(err)
	}
	wantTree := strings.TrimSpace(integratedGitOutput(t, seed, "rev-parse", verifiedHead+"^{tree}"))
	if tree != wantTree {
		t.Fatalf("verified tree = %s, want %s", tree, wantTree)
	}
	if fetched := strings.TrimSpace(integratedGitOutput(t, checkout, "rev-parse", "refs/remotes/origin/main")); fetched != verifiedHead {
		t.Fatalf("fetched default head = %s, want %s", fetched, verifiedHead)
	}
	if current := strings.TrimSpace(integratedGitOutput(t, checkout, "rev-parse", "HEAD")); current != initialHead {
		t.Fatalf("synchronization moved checkout HEAD from %s to %s", initialHead, current)
	}
	if status := strings.TrimSpace(integratedGitOutput(t, checkout, "status", "--short")); status != "" {
		t.Fatalf("synchronization changed the checkout worktree: %q", status)
	}
	if _, err := synchronizeNormalVisualWorkBase(ctx, config, initialHead); err == nil || !strings.Contains(err.Error(), "does not match verified workflow head") {
		t.Fatalf("stale verified head did not fail closed: %v", err)
	}
}
