package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestResolveVisualHiveLauncherUsesPackagedRuntime(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "visual-hive")
	runtimeDir := filepath.Join(root, "runtime")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	entrypoint := filepath.Join(home, "visual-hive.mjs")
	if err := os.WriteFile(entrypoint, []byte("console.log('ok')\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	nodeName := "node"
	if runtime.GOOS == "windows" {
		nodeName = "node.exe"
	}
	node := filepath.Join(runtimeDir, nodeName)
	if err := os.WriteFile(node, []byte("runtime"), 0o700); err != nil {
		t.Fatal(err)
	}

	command, args, err := resolveVisualHiveLauncher("", nil, home)
	if err != nil {
		t.Fatal(err)
	}
	if command != node || len(args) != 1 || args[0] != entrypoint {
		t.Fatalf("packaged launcher = %q %v", command, args)
	}
}

func TestValidateVisualHiveReleaseRejectsTampering(t *testing.T) {
	root := t.TempDir()
	entrypoint := filepath.Join(root, "visual-hive.mjs")
	contents := []byte("console.log('ok')\n")
	if err := os.WriteFile(entrypoint, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	commit := strings.Repeat("a", 40)
	manifest := map[string]any{
		"schemaVersion":     "visual-hive.release.v1",
		"name":              "visual-hive",
		"version":           "0.2.0",
		"gitCommit":         commit,
		"node":              ">=22",
		"entrypoint":        "visual-hive.mjs",
		"playwrightVersion": "1.60.0",
		"files": []map[string]any{{
			"path": "visual-hive.mjs", "sha256": fmt.Sprintf("%x", sha256.Sum256(contents)), "size": len(contents),
		}},
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "release-manifest.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if ok, message := validateVisualHiveRelease(entrypoint, commit); !ok {
		t.Fatalf("valid release rejected: %s", message)
	}
	if err := os.WriteFile(entrypoint, []byte("tampered\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if ok, message := validateVisualHiveRelease(entrypoint, commit); ok || !strings.Contains(message, "inventory") {
		t.Fatalf("tampered release = %t, %q", ok, message)
	}
}
