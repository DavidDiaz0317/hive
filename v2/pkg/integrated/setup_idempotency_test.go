package integrated

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStagedTreeMatchesExistingSetupBranch(t *testing.T) {
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	seed := filepath.Join(root, "seed")
	checkout := filepath.Join(root, "checkout")

	runIntegratedGit(t, root, "init", "--bare", remote)
	runIntegratedGit(t, root, "init", "-b", "main", seed)
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("seed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runIntegratedGit(t, seed, "add", ".")
	runIntegratedGit(t, seed, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "seed")
	runIntegratedGit(t, seed, "remote", "add", "origin", remote)
	runIntegratedGit(t, seed, "push", "-u", "origin", "main")
	runIntegratedGit(t, remote, "symbolic-ref", "HEAD", "refs/heads/main")
	runIntegratedGit(t, root, "clone", remote, checkout)

	runIntegratedGit(t, checkout, "switch", "-C", "hive/setup", "origin/main")
	setupPath := filepath.Join(checkout, ".hive", "integrated.json")
	if err := os.MkdirAll(filepath.Dir(setupPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(setupPath, []byte("{\"version\":1}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runIntegratedGit(t, checkout, "add", ".hive/integrated.json")
	runIntegratedGit(t, checkout, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "setup")
	runIntegratedGit(t, checkout, "push", "-u", "origin", "hive/setup")
	remoteSHA := strings.TrimSpace(integratedGitOutput(t, checkout, "rev-parse", "origin/hive/setup"))

	runIntegratedGit(t, checkout, "switch", "-C", "proof", "origin/main")
	if err := os.MkdirAll(filepath.Dir(setupPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(setupPath, []byte("{\"version\":1}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runIntegratedGit(t, checkout, "add", ".hive/integrated.json")

	matches, sha, err := stagedTreeMatchesRemoteBranch(context.Background(), checkout, "hive/setup")
	if err != nil {
		t.Fatal(err)
	}
	if !matches || sha != remoteSHA {
		t.Fatalf("matching setup tree = %t, sha = %q; want true, %q", matches, sha, remoteSHA)
	}

	if err := os.WriteFile(setupPath, []byte("{\"version\":2}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runIntegratedGit(t, checkout, "add", ".hive/integrated.json")
	matches, _, err = stagedTreeMatchesRemoteBranch(context.Background(), checkout, "hive/setup")
	if err != nil {
		t.Fatal(err)
	}
	if matches {
		t.Fatal("changed setup tree must not be treated as idempotent")
	}
}
