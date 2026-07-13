package integrated

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSetupCheckoutRejectsLinkedManagedParentWithoutOutsideWrite(t *testing.T) {
	checkout, outside := t.TempDir(), t.TempDir()
	sentinel := filepath.Join(outside, "sentinel.txt")
	if err := os.WriteFile(sentinel, []byte("unchanged"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(checkout, ".github")); err != nil {
		t.Skipf("directory links are unavailable: %v", err)
	}
	if err := validateOrdinarySetupCheckout(checkout); err == nil {
		t.Fatal("linked managed parent was accepted")
	}
	if err := writeSetupCheckoutFile(checkout, ".github/workflows/hive-visual-hive.yml", []byte("unsafe")); err == nil {
		t.Fatal("managed write followed a linked parent")
	}
	data, err := os.ReadFile(sentinel)
	if err != nil || string(data) != "unchanged" {
		t.Fatalf("outside sentinel changed: %q err=%v", data, err)
	}
	if _, err := os.Stat(filepath.Join(outside, "workflows", "hive-visual-hive.yml")); !os.IsNotExist(err) {
		t.Fatalf("managed file escaped checkout: %v", err)
	}
}

func TestSetupInspectionRejectsLinkedPackageAndVisualConfigLeaves(t *testing.T) {
	for _, relative := range []string{"package.json", "visual-hive.config.yaml"} {
		t.Run(strings.ReplaceAll(relative, ".", "_"), func(t *testing.T) {
			checkout, outside := t.TempDir(), t.TempDir()
			sentinel := filepath.Join(outside, "sentinel.txt")
			if err := os.WriteFile(sentinel, []byte("unchanged"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(sentinel, filepath.Join(checkout, relative)); err != nil {
				t.Skipf("file links are unavailable: %v", err)
			}
			if _, err := InspectCheckout(checkout, "main"); err == nil {
				t.Fatalf("linked setup input %s was inspected", relative)
			}
			if _, err := readSetupCheckoutFile(checkout, relative); err == nil {
				t.Fatalf("linked setup input %s was read", relative)
			}
			data, err := os.ReadFile(sentinel)
			if err != nil || string(data) != "unchanged" {
				t.Fatalf("outside sentinel changed: %q err=%v", data, err)
			}
		})
	}
}

func TestSetupStateRootRejectsCommonOrNonemptyProjectDirectories(t *testing.T) {
	common := filepath.Join(t.TempDir(), "Documents")
	if err := os.MkdirAll(common, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := validateSetupStateRoot(common, "owner/repo"); err == nil {
		t.Fatal("common Documents parent was accepted as a recursive Hive state root")
	}
	project := filepath.Join(t.TempDir(), "product-app")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(project, "README.md")
	if err := os.WriteFile(sentinel, []byte("must survive"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateSetupStateRoot(project, "owner/repo"); err == nil {
		t.Fatal("nonempty project directory was adopted as Hive state")
	}
	data, err := os.ReadFile(sentinel)
	if err != nil || string(data) != "must survive" {
		t.Fatalf("project sentinel changed: %q err=%v", data, err)
	}
}

func TestManagedCheckoutRejectsPreplacedRootAndGitLinksBeforeGit(t *testing.T) {
	for _, linkGit := range []bool{false, true} {
		name := "checkout-root"
		if linkGit {
			name = "git-dir"
		}
		t.Run(name, func(t *testing.T) {
			state, outside := t.TempDir(), t.TempDir()
			checkout := filepath.Join(state, "integrated", "checkouts", "owner--repo")
			if err := os.MkdirAll(filepath.Dir(checkout), 0o700); err != nil {
				t.Fatal(err)
			}
			sentinel := filepath.Join(outside, "sentinel.txt")
			if err := os.WriteFile(sentinel, []byte("unchanged"), 0o600); err != nil {
				t.Fatal(err)
			}
			if linkGit {
				if err := os.MkdirAll(checkout, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, filepath.Join(checkout, ".git")); err != nil {
					t.Skipf("directory links unavailable: %v", err)
				}
			} else if err := os.Symlink(outside, checkout); err != nil {
				t.Skipf("directory links unavailable: %v", err)
			}
			if _, err := validateManagedCheckoutBeforeGit(checkout, "owner/repo"); err == nil {
				t.Fatal("preplaced checkout indirection was trusted before Git")
			}
			data, err := os.ReadFile(sentinel)
			if err != nil || string(data) != "unchanged" {
				t.Fatalf("outside sentinel changed: %q err=%v", data, err)
			}
		})
	}
}

func TestManagedCheckoutRejectsLinkedMissingCheckoutAncestorBeforeClone(t *testing.T) {
	state, outside := t.TempDir(), t.TempDir()
	integrated := filepath.Join(state, "integrated")
	if err := os.MkdirAll(integrated, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(integrated, "checkouts")); err != nil {
		t.Skipf("directory links are unavailable: %v", err)
	}
	checkout := filepath.Join(integrated, "checkouts", "owner--repo")
	if _, err := validateManagedCheckoutBeforeGit(checkout, "owner/repo"); err == nil {
		t.Fatal("linked checkouts ancestor was accepted for a missing clone leaf")
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 0 {
		t.Fatalf("checkout validation wrote through linked ancestor: entries=%v err=%v", entries, err)
	}
}

func TestSetupStateRootRejectsLinkedOwnershipMarkerAndIntegratedDirectory(t *testing.T) {
	for _, linked := range []string{stateOwnershipMarkerFile, "integrated"} {
		t.Run(linked, func(t *testing.T) {
			state, outside := t.TempDir(), t.TempDir()
			if linked == stateOwnershipMarkerFile {
				target := filepath.Join(outside, "owner.json")
				if err := os.WriteFile(target, []byte(`{"schema_version":"hive.state-owner.v1","repository":"owner/repo","repository_id":"123"}`), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, filepath.Join(state, linked)); err != nil {
					t.Skipf("file links are unavailable: %v", err)
				}
			} else if err := os.Symlink(outside, filepath.Join(state, linked)); err != nil {
				t.Skipf("directory links are unavailable: %v", err)
			}
			if err := validateSetupStateRoot(state, "owner/repo"); err == nil {
				t.Fatalf("linked state entry %s was accepted", linked)
			}
		})
	}
}

func TestManagedCheckoutRejectsMarkerlessGitConfigInsteadOfAdoptingIt(t *testing.T) {
	state := t.TempDir()
	checkout := filepath.Join(state, "integrated", "checkouts", "owner--repo")
	if err := os.MkdirAll(filepath.Join(checkout, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	config := "[remote \"decoy\"]\n\turl = https://github.com/owner/repo.git\n[remote \"origin\"]\n\turl = ext::sh -c malicious\n[core]\n\tfsmonitor = malicious\n"
	if err := os.WriteFile(filepath.Join(checkout, ".git", "config"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := validateManagedCheckoutBeforeGit(checkout, "owner/repo"); err == nil || !strings.Contains(err.Error(), "no exact Hive ownership marker") {
		t.Fatalf("markerless malicious Git config was adopted: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(checkout, ".git", managedCheckoutOwnerFile)); !os.IsNotExist(err) {
		t.Fatalf("unsafe checkout received an ownership marker: %v", err)
	}
}
