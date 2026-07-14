package integrated

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestBuildDistributionCreatesSelfContainedImmutableTree(t *testing.T) {
	root := t.TempDir()
	visualCommit := strings.Repeat("a", 40)
	visualDir := writeTestVisualHiveRelease(t, filepath.Join(root, "visual"), visualCommit)
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	runtimeDir := filepath.Join(root, "node-runtime")
	nodeName := "node"
	if runtime.GOOS == "windows" {
		nodeName = "node.exe"
	}
	if err := copyRegularFile(testBinary, filepath.Join(runtimeDir, nodeName), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, relative := range requiredNodeRuntimeFiles(runtime.GOOS) {
		if relative == nodeName {
			continue
		}
		target := filepath.Join(runtimeDir, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, []byte("test runtime "+relative+"\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	nodeLicense := filepath.Join(root, "NODE-LICENSE")
	if err := os.WriteFile(nodeLicense, []byte("Node.js license\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	skillDir := filepath.Join(root, "skill")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: hive\ndescription: test\n---\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(skillDir, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "agents", "openai.yaml"), []byte("name: Hive\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(root, "distribution")
	manifest, err := BuildDistribution(context.Background(), DistributionOptions{
		HiveBinary: testBinary, HiveCommit: strings.Repeat("b", 40), HiveVersion: "v0.4.1-integrated.11", VisualHiveDir: visualDir, VisualCommit: visualCommit,
		NodeBinary: testBinary, NodeRuntimeDir: runtimeDir, NodeLicense: nodeLicense, NodeVersion: "v22.23.1", SkillDir: skillDir, OutputDir: output,
	})
	if err != nil {
		t.Fatal(err)
	}
	if manifest.SchemaVersion != DistributionSchema || manifest.HiveVersion != "v0.4.1-integrated.11" || manifest.VisualHiveVersion != "0.2.0" || len(manifest.Files) < 5 {
		t.Fatalf("unexpected distribution manifest: %+v", manifest)
	}
	hiveName := "hive"
	if runtime.GOOS == "windows" {
		hiveName = "hive.exe"
	}
	for _, required := range []string{hiveName, filepath.Join("runtime", nodeName), filepath.Join("runtime", "LICENSE.node.txt"), filepath.Join("visual-hive", "visual-hive.mjs"), filepath.Join("skills", "hive", "SKILL.md"), filepath.Join("skills", "hive", "agents", "openai.yaml"), "distribution-manifest.json"} {
		if _, err := os.Stat(filepath.Join(output, required)); err != nil {
			t.Fatalf("missing distribution file %s: %v", required, err)
		}
	}
	if _, err := BuildDistribution(context.Background(), DistributionOptions{OutputDir: output}); err == nil || !strings.Contains(err.Error(), "commit") {
		t.Fatal("existing or incomplete distribution must not be overwritten")
	}
}

func TestValidateVisualHiveReleaseRejectsDigestAndTraversal(t *testing.T) {
	root := t.TempDir()
	commit := strings.Repeat("c", 40)
	visualDir := writeTestVisualHiveRelease(t, root, commit)
	entrypoint := filepath.Join(visualDir, "visual-hive.mjs")
	if _, err := ValidateVisualHiveRelease(entrypoint, commit); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(entrypoint, []byte("tampered\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateVisualHiveRelease(entrypoint, commit); err == nil {
		t.Fatal("tampered Visual Hive release was accepted")
	}

	visualDir = writeTestVisualHiveRelease(t, filepath.Join(t.TempDir(), "visual"), commit)
	manifestPath := filepath.Join(visualDir, "release-manifest.json")
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var manifest VisualHiveReleaseManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	manifest.Files[0].Path = "../escape"
	data, _ = json.Marshal(manifest)
	if err := os.WriteFile(manifestPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateVisualHiveRelease(filepath.Join(visualDir, "visual-hive.mjs"), commit); err == nil || !strings.Contains(err.Error(), "unsafe") {
		t.Fatalf("path traversal rejection = %v", err)
	}
}

func writeTestVisualHiveRelease(t *testing.T, root, commit string) string {
	t.Helper()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	entrypoint := []byte("console.log('visual-hive')\n")
	if err := os.WriteFile(filepath.Join(root, "visual-hive.mjs"), entrypoint, 0o600); err != nil {
		t.Fatal(err)
	}
	manifest := VisualHiveReleaseManifest{
		SchemaVersion: VisualHiveReleaseSchema, Name: "visual-hive", Version: "0.2.0", GitCommit: commit, Node: ">=22",
		Entrypoint: "visual-hive.mjs", PlaywrightVersion: "1.60.0",
		Files: []VisualHiveReleaseFile{{Path: "visual-hive.mjs", SHA256: fmt.Sprintf("%x", sha256.Sum256(entrypoint)), Size: int64(len(entrypoint))}},
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "release-manifest.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}
