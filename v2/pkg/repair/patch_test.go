package repair

import (
	"context"
	"errors"
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

func TestRejectedModelPatchIsNotInfrastructureFailure(t *testing.T) {
	repository, _ := seedGitRepository(t)
	patch := "diff --git a/src/value.txt b/src/value.txt\n--- a/src/value.txt\n+++ b/src/value.txt\n@@ -1 +1 @@\n-not-the-current-value\n+fixed\n"
	err := applyModelPatch(context.Background(), repository, patch)
	if err == nil {
		t.Fatal("expected deterministic patch rejection")
	}
	if isPatchEngineInfrastructureFailure(err) {
		t.Fatalf("invalid model output was misclassified as patch-engine infrastructure: %v", err)
	}
}

func TestPatchCommandFailureClassifiesFatalRepositoryErrorsAsInfrastructure(t *testing.T) {
	exitErr := errors.New("exit status 128")
	failure := errors.New("git apply failed")
	for _, output := range []string{
		"fatal: Unable to create '.git/index.lock': Permission denied",
		"fatal: not a git repository",
		"error: unable to write new index file: No space left on device",
	} {
		classified := classifyPatchCommandFailure(context.Background(), exitErr, output, failure)
		if !isPatchEngineInfrastructureFailure(classified) {
			t.Fatalf("fatal repository failure was charged to the model: %q => %v", output, classified)
		}
	}
	classified := classifyPatchCommandFailure(context.Background(), exitErr, "error: patch failed: src/value.txt:1\nerror: src/value.txt: patch does not apply", failure)
	if isPatchEngineInfrastructureFailure(classified) {
		t.Fatalf("deterministic patch rejection was classified as infrastructure: %v", classified)
	}
}

func TestApplyIncrementalModelPatchSkipsAlreadyAppliedHunk(t *testing.T) {
	repository, _ := seedGitRepository(t)
	path := filepath.Join(repository, "src", "value.txt")
	current := "target:\n  kind: command\n  build: npm run build\n  serve: node scripts/testing/start-lhci-server.mjs\n  url: http://127.0.0.1:4173\nselectors:\n  textMustExist: []\n  textMustNotExist:\n    - visual-hive api-500 mutation\nscreenshots: []\n"
	if err := os.WriteFile(path, []byte(current), 0o600); err != nil {
		t.Fatal(err)
	}
	patch := "diff --git a/src/value.txt b/src/value.txt\n--- a/src/value.txt\n+++ b/src/value.txt\n@@ -1,5 +1,5 @@\n target:\n   kind: command\n   build: npm run build\n-  serve: node scripts/testing/start-lhci-server.mjs\n+  serve: npm run preview\n   url: http://127.0.0.1:4173\n@@ -5,5 +5,6 @@\n   url: http://127.0.0.1:4173\n selectors:\n   textMustExist: []\n-  textMustNotExist: []\n+  textMustNotExist:\n+    - visual-hive api-500 mutation\n screenshots: []\n"
	if err := applyIncrementalModelPatch(context.Background(), repository, patch); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	expected := strings.Replace(current, "serve: node scripts/testing/start-lhci-server.mjs", "serve: npm run preview", 1)
	if strings.ReplaceAll(string(data), "\r\n", "\n") != expected {
		t.Fatalf("incremental patch did not apply only the missing hunk: %q", data)
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
