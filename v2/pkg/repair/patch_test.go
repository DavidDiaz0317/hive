package repair

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExtractAndApplyModelPatch(t *testing.T) {
	repository, _ := seedGitRepository(t)
	response := `Patch follows.
HIVE_PATCH_BEGIN
diff --git a/src/value.txt b/src/value.txt
--- a/src/value.txt
+++ b/src/value.txt
@@ -1 +1 @@
-broken
+fixed by patch
HIVE_PATCH_END`
	patch, err := extractModelPatch(response)
	if err != nil {
		t.Fatal(err)
	}
	files, err := patchChangedFiles(patch)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0] != "src/value.txt" {
		t.Fatalf("unexpected patch files: %v", files)
	}
	if err := applyModelPatch(context.Background(), repository, patch); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(repository, "src", "value.txt"))
	if err != nil || strings.TrimSpace(string(data)) != "fixed by patch" {
		t.Fatalf("patch did not apply: %q err=%v", data, err)
	}
}

func TestModelPatchAlreadyAppliedDetectsCrashResume(t *testing.T) {
	repository, _ := seedGitRepository(t)
	patch := "diff --git a/src/value.txt b/src/value.txt\n--- a/src/value.txt\n+++ b/src/value.txt\n@@ -1 +1 @@\n-broken\n+fixed\n"
	applied, err := modelPatchAlreadyApplied(context.Background(), repository, patch)
	if err != nil || applied {
		t.Fatalf("fresh patch reported already applied: applied=%t err=%v", applied, err)
	}
	if err := applyModelPatch(context.Background(), repository, patch); err != nil {
		t.Fatal(err)
	}
	applied, err = modelPatchAlreadyApplied(context.Background(), repository, patch)
	if err != nil || !applied {
		t.Fatalf("applied patch was not recognized after resume: applied=%t err=%v", applied, err)
	}
}

func TestPatchChangedFilesRejectsUnsafeMetadata(t *testing.T) {
	for name, patch := range map[string]string{
		"traversal": "diff --git a/../outside b/../outside\n--- a/../outside\n+++ b/../outside\n@@ -0,0 +1 @@\n+x\n",
		"rename":    "diff --git a/src/a b/src/b\nrename from src/a\nrename to src/b\n",
		"binary":    "diff --git a/src/a b/src/a\nGIT binary patch\n",
		"git":       "diff --git a/.git/config b/.git/config\n--- a/.git/config\n+++ b/.git/config\n@@ -0,0 +1 @@\n+x\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := patchChangedFiles(patch); err == nil {
				t.Fatal("expected unsafe patch rejection")
			}
		})
	}
}
