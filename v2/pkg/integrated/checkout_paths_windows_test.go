//go:build windows

package integrated

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func createTestJunction(t *testing.T, link, target string) {
	t.Helper()
	if output, err := exec.Command("cmd.exe", "/d", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
		t.Skipf("directory junctions are unavailable: %v: %s", err, output)
	}
}

func TestWindowsJunctionAncestorsAreRejectedBeforeStateOrGitWrites(t *testing.T) {
	t.Run("state-integrated", func(t *testing.T) {
		state, outside := t.TempDir(), t.TempDir()
		sentinel := filepath.Join(outside, "sentinel.txt")
		if err := os.WriteFile(sentinel, []byte("unchanged"), 0o600); err != nil {
			t.Fatal(err)
		}
		createTestJunction(t, filepath.Join(state, "integrated"), outside)
		if err := validateSetupStateRoot(state, "owner/repo"); err == nil {
			t.Fatal("state integrated junction was accepted")
		}
		if data, err := os.ReadFile(sentinel); err != nil || string(data) != "unchanged" {
			t.Fatalf("outside sentinel changed: %q err=%v", data, err)
		}
	})

	t.Run("checkout-parent", func(t *testing.T) {
		state, outside := t.TempDir(), t.TempDir()
		integrated := filepath.Join(state, "integrated")
		if err := os.MkdirAll(integrated, 0o700); err != nil {
			t.Fatal(err)
		}
		createTestJunction(t, filepath.Join(integrated, "checkouts"), outside)
		checkout := filepath.Join(integrated, "checkouts", "owner--repo")
		if _, err := validateManagedCheckoutBeforeGit(checkout, "owner/repo"); err == nil {
			t.Fatal("checkout parent junction was accepted")
		}
		entries, err := os.ReadDir(outside)
		if err != nil || len(entries) != 0 {
			t.Fatalf("checkout validation wrote through junction: entries=%v err=%v", entries, err)
		}
	})
}

func TestLegacyManagedCheckoutMigrationRejectsNestedJunction(t *testing.T) {
	_, checkout, proof := createLegacyManagedCheckout(t)
	outside := t.TempDir()
	junction := filepath.Join(checkout, ".git", "objects", "attacker")
	createTestJunction(t, junction, outside)
	if _, err := validateManagedCheckoutBeforeGitWithLegacy(checkout, "owner/repo", &proof); err == nil {
		t.Fatal("markerless legacy checkout with a nested Git junction was migrated")
	}
	if _, err := os.Lstat(filepath.Join(checkout, ".git", managedCheckoutOwnerFile)); !os.IsNotExist(err) {
		t.Fatalf("junction-containing checkout received an ownership marker: %v", err)
	}
}
