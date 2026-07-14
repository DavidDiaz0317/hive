package integrated

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	hivegithub "github.com/kubestellar/hive/v2/pkg/github"
)

func TestVisualHiveSetupIsStaticOnlyAndNeverExecutesTargetOrBrowserBootstrap(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node is required for the static Visual Hive setup test")
	}
	checkout, stateDir := t.TempDir(), t.TempDir()
	sentinel := filepath.Join(checkout, "target-command-executed")
	preloadSentinel := filepath.Join(checkout, "environment-preload-executed")
	preload := filepath.Join(t.TempDir(), "preload.cjs")
	if err := os.WriteFile(preload, []byte(`require("fs").writeFileSync(`+strconv.Quote(preloadSentinel)+`, "executed")`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NODE_OPTIONS", "--require="+preload)
	packageJSON := map[string]any{"scripts": map[string]string{
		"preinstall":  "node -e \"require('fs').writeFileSync('target-command-executed','preinstall')\"",
		"postinstall": "node -e \"require('fs').writeFileSync('target-command-executed','postinstall')\"",
		"test":        "node -e \"require('fs').writeFileSync('target-command-executed','test')\"",
	}}
	data, _ := json.Marshal(packageJSON)
	if err := os.WriteFile(filepath.Join(checkout, "package.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(t.TempDir(), "static-visual-hive.cjs")
	scriptBody := `const fs = require("fs");
const path = require("path");
const command = process.argv[2];
const args = process.argv.slice(3);
const value = (name) => { const index = args.indexOf(name); return index >= 0 ? args[index + 1] : ""; };
if (command === "recommend") {
  const repo = value("--repo");
  fs.mkdirSync(path.join(repo, "docs"), { recursive: true });
  fs.writeFileSync(path.join(repo, "docs", "visual-hive.md"), "# Visual Hive\n");
  fs.writeFileSync(path.join(repo, "visual-hive.config.yaml"), "project:\n  name: static-only\n  setupProfile: complex-app\ntargets:\n  local:\n    kind: command\n    serve: npm run malicious-serve\n    url: http://127.0.0.1:4173\ncontracts:\n  - id: home\n    target: local\n    screenshots:\n      - name: home\n        route: /\n        viewport: desktop\n");
  fs.writeFileSync(path.join(repo, "static-environment.json"), JSON.stringify({ corepackHome: process.env.COREPACK_HOME, path: process.env.PATH }));
} else if (command === "doctor") {
  process.exit(0);
} else {
  fs.writeFileSync(path.join(process.cwd(), "target-command-executed"), command);
  process.exit(91);
}`
	if err := os.WriteFile(script, []byte(scriptBody), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runVisualHiveSetup(context.Background(), SetupOptions{Coverage: CoverageComprehensive, StateDir: stateDir, VisualHiveCommand: node, VisualHiveArgs: []string{script}}, checkout); err != nil {
		t.Fatalf("static Visual Hive setup failed: %v", err)
	}
	if _, err := os.Stat(sentinel); !os.IsNotExist(err) {
		t.Fatalf("local setup executed a target-authored command: %v", err)
	}
	if _, err := os.Stat(preloadSentinel); !os.IsNotExist(err) {
		t.Fatalf("static Visual Hive launcher inherited NODE_OPTIONS preload: %v", err)
	}
	if _, err := os.Stat(filepath.Join(checkout, ".visual-hive", "snapshots", "win32")); !os.IsNotExist(err) {
		t.Fatalf("local setup created a platform snapshot tree: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "runtime", "playwright-browsers")); !os.IsNotExist(err) {
		t.Fatalf("local setup provisioned a browser cache: %v", err)
	}
	environmentData, err := os.ReadFile(filepath.Join(checkout, "static-environment.json"))
	if err != nil {
		t.Fatal(err)
	}
	var environment struct {
		CorepackHome string `json:"corepackHome"`
		Path         string `json:"path"`
	}
	if err := json.Unmarshal(environmentData, &environment); err != nil {
		t.Fatal(err)
	}
	wantCorepack := filepath.Clean(filepath.Join(stateDir, "runtime", "corepack"))
	if filepath.Clean(environment.CorepackHome) != wantCorepack {
		t.Fatalf("COREPACK_HOME=%q, want %q", environment.CorepackHome, wantCorepack)
	}
	if !strings.HasPrefix(environment.Path, filepath.Dir(node)+string(os.PathListSeparator)) {
		t.Fatalf("bundled runtime was not prepended to static setup PATH: %q", environment.Path)
	}
}

func TestExistingVisualBaselinesIgnoresStaleUntrackedSnapshots(t *testing.T) {
	checkout := t.TempDir()
	runGit := func(arguments ...string) {
		t.Helper()
		command := exec.Command("git", append([]string{"-C", checkout}, arguments...)...)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", arguments, err, output)
		}
	}
	runGit("init", "-b", "main")
	runGit("config", "user.name", "Hive test")
	runGit("config", "user.email", "hive@example.invalid")
	if err := os.WriteFile(filepath.Join(checkout, "README.md"), []byte("fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit("add", "README.md")
	runGit("commit", "-m", "fixture")
	stale := filepath.Join(checkout, ".visual-hive", "snapshots", "linux", "stale.png")
	if err := os.MkdirAll(filepath.Dir(stale), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale, []byte("not trusted hosted evidence"), 0o600); err != nil {
		t.Fatal(err)
	}
	if baselines := existingVisualBaselines(checkout); len(baselines) != 0 {
		t.Fatalf("untracked local snapshot was treated as a reviewed baseline: %v", baselines)
	}
	runGit("add", "-f", ".visual-hive/snapshots/linux/stale.png")
	runGit("commit", "-m", "review fixture baseline")
	if baselines := existingVisualBaselines(checkout); len(baselines) != 1 || baselines[0] != ".visual-hive/snapshots/linux/stale.png" {
		t.Fatalf("committed reviewed baseline was not detected: %v", baselines)
	}
}

func TestCreateSetupBaselineProposalRejectsArtifactMutationAfterDurableSave(t *testing.T) {
	root := t.TempDir()
	content := append([]byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}, []byte("hosted-linux-baseline")...)
	path := ".visual-hive/snapshots/linux/home.png"
	absolute := filepath.Join(root, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(absolute), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(absolute, content, 0o600); err != nil {
		t.Fatal(err)
	}
	fileHash := sha256.Sum256(content)
	candidates := []SetupBaselineCandidate{{Path: path, SHA256: hex.EncodeToString(fileHash[:]), Bytes: int64(len(content))}}
	candidateDigest, err := setupBaselineCandidateDigest(candidates)
	if err != nil {
		t.Fatal(err)
	}
	head := strings.Repeat("a", 40)
	correlation := strings.Repeat("b", 64)
	manifest := setupBaselineArtifactManifest{
		SchemaVersion: setupBaselineArtifactSchema, Repository: "owner/repo", RepositoryID: "123", CaptureCorrelation: correlation,
		CaptureHead: head, WorkflowRunID: "77", WorkflowName: visualHiveProductionWorkflowName, WorkflowPath: visualHiveProductionWorkflowPath,
		Event: "workflow_dispatch", Platform: "linux", Runner: "ubuntu-latest", CandidateDigest: candidateDigest,
		FileCount: 1, TotalBytes: int64(len(content)), Files: candidates,
	}
	data, _ := json.Marshal(manifest)
	if err := os.WriteFile(filepath.Join(root, "setup-baseline-manifest.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	intent := SetupBaselineIntent{
		SchemaVersion: SetupBaselineSchema, Phase: SetupBaselineArtifactVerified, Repository: "owner/repo", RepositoryID: "123", DefaultBranch: "main",
		AuthorizerID: 42, SetupPRNumber: 1, SetupPRURL: "https://example.test/pull/1", SetupHeadSHA: head,
		CaptureCorrelation: correlation, CaptureHeadSHA: head, CaptureRunID: 77, CaptureRunURL: "https://example.test/run/77",
		ArtifactID: 88, ArtifactName: "hive-setup-baselines-" + correlation, ArtifactRoot: root, CaptureCandidateDigest: candidateDigest, CaptureCandidates: candidates,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(), DispatchAttemptedAt: time.Now().UTC(), DispatchAcknowledgedAt: time.Now().UTC(),
	}
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(absolute, append(content, byte('x')), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := createSetupBaselineProposal(context.Background(), store, Config{}, intent, nil); err == nil || !strings.Contains(err.Error(), "re-verify hosted") {
		t.Fatalf("mutated artifact reached proposal creation: %v", err)
	}
}

func TestVerifySetupBaselineArtifactAcceptsOnlyExactExtractionCompletionMarker(t *testing.T) {
	root := t.TempDir()
	content := append([]byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}, []byte("hosted-linux-baseline")...)
	path := ".visual-hive/snapshots/linux/home.png"
	absolute := filepath.Join(root, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(absolute), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(absolute, content, 0o600); err != nil {
		t.Fatal(err)
	}
	fileHash := sha256.Sum256(content)
	candidates := []SetupBaselineCandidate{{Path: path, SHA256: hex.EncodeToString(fileHash[:]), Bytes: int64(len(content))}}
	candidateDigest, err := setupBaselineCandidateDigest(candidates)
	if err != nil {
		t.Fatal(err)
	}
	head, correlation := strings.Repeat("a", 40), strings.Repeat("b", 64)
	manifest := setupBaselineArtifactManifest{
		SchemaVersion: setupBaselineArtifactSchema, Repository: "owner/repo", RepositoryID: "123", CaptureCorrelation: correlation,
		CaptureHead: head, WorkflowRunID: "77", WorkflowName: visualHiveProductionWorkflowName, WorkflowPath: visualHiveProductionWorkflowPath,
		Event: "workflow_dispatch", Platform: "linux", Runner: "ubuntu-latest", CandidateDigest: candidateDigest,
		FileCount: 1, TotalBytes: int64(len(content)), Files: candidates,
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "setup-baseline-manifest.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, ".hive-extraction-complete")
	if err := os.WriteFile(marker, []byte("88\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	intent := SetupBaselineIntent{
		Repository: "owner/repo", RepositoryID: "123", CaptureCorrelation: correlation, CaptureHeadSHA: head,
		CaptureRunID: 77, ArtifactID: 88,
	}
	verified, digest, err := verifySetupBaselineArtifact(root, intent)
	if err != nil || len(verified) != 1 || digest != candidateDigest {
		t.Fatalf("exact extracted setup baseline artifact was rejected: verified=%+v digest=%s err=%v", verified, digest, err)
	}
	if err := os.WriteFile(marker, []byte("89\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := verifySetupBaselineArtifact(root, intent); err == nil || !strings.Contains(err.Error(), "extraction completion marker") {
		t.Fatalf("artifact with a mismatched extraction marker was accepted: %v", err)
	}
}

func TestEnsureSetupBaselineDestinationRejectsSymlinkParentEscape(t *testing.T) {
	checkout, outside := t.TempDir(), t.TempDir()
	link := filepath.Join(checkout, ".visual-hive")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("directory symlinks unavailable: %v", err)
	}
	if _, err := ensureSetupBaselineDestination(checkout, ".visual-hive/snapshots/linux/home.png"); err == nil {
		t.Fatal("symlink parent was accepted")
	}
	if _, err := os.Stat(filepath.Join(outside, "snapshots")); !os.IsNotExist(err) {
		t.Fatalf("destination path was created outside checkout: %v", err)
	}
}

func TestMergedSetupBaselineHeadDriftResetsForHostedRecapture(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	oldHead, newHead := strings.Repeat("a", 40), strings.Repeat("b", 40)
	mergeSHA := strings.Repeat("d", 40)
	rawDiff := "diff --git a/.visual-hive/snapshots/linux/home.png b/.visual-hive/snapshots/linux/home.png\nnew baseline\n"
	approvedCandidates := []SetupBaselineCandidate{{Path: ".visual-hive/snapshots/linux/home.png", SHA256: strings.Repeat("e", 64), Bytes: 9}}
	approvedDigest, err := setupBaselineCandidateDigest(approvedCandidates)
	if err != nil {
		t.Fatal(err)
	}
	intent := SetupBaselineIntent{
		SchemaVersion: SetupBaselineSchema, Phase: SetupBaselineMerged, Repository: "owner/repo", RepositoryID: "123", DefaultBranch: "main",
		AuthorizerID: 42, SetupPRNumber: 1, SetupPRURL: "https://example.test/pull/1", SetupHeadSHA: oldHead,
		VisualHiveConfigDigest: strings.Repeat("8", 64), ScreenshotContractDigest: strings.Repeat("9", 64), InitialBaselineDigest: emptySetupBaselineCandidateDigest(t),
		CaptureCorrelation: strings.Repeat("c", 64), CaptureHeadSHA: oldHead, CaptureRunID: 77, CaptureRunURL: "https://example.test/run/77",
		ArtifactID: 88, ArtifactName: "artifact", ArtifactRoot: "root", CaptureCandidateDigest: approvedDigest, CaptureCandidates: approvedCandidates, CandidateDigest: approvedDigest,
		Candidates: approvedCandidates,
		Branch:     "hive/setup-baseline-123", CommitSHA: strings.Repeat("f", 40), Marker: setupBaselineMarker("owner/repo", strings.Repeat("c", 64)),
		PRNumber: 2, PRURL: "https://example.test/pull/2", BaseSHA: oldHead, DiffDigest: digestTestDiff(rawDiff),
		ApprovalPlanDigest: strings.Repeat("2", 64), ApprovalActorID: 42, ApprovalReason: "reviewed", MergeAttemptedAt: time.Now().UTC(), MergeSHA: mergeSHA,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(), DispatchAttemptedAt: time.Now().UTC(), DispatchAcknowledgedAt: time.Now().UTC(),
	}
	deletes, failures := 0, 1
	server := newMergedSetupBaselineRetirementServer(t, intent, rawDiff, intent.CommitSHA, &deletes, &failures)
	defer server.Close()
	client := hivegithub.NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	config := Config{Repository: "owner/repo", RepositoryID: "123"}
	if _, err := resetSetupBaselineAfterMergedHeadDrift(context.Background(), store, config, client, intent, newHead); err == nil {
		t.Fatal("transient merged baseline branch deletion did not preserve drift recovery state")
	}
	reset, err := resetSetupBaselineAfterMergedHeadDrift(context.Background(), store, config, client, intent, newHead)
	if err != nil {
		t.Fatal(err)
	}
	if deletes != 2 {
		t.Fatalf("merged drift branch retirement attempts=%d, want 2", deletes)
	}
	if audit, err := os.ReadFile(filepath.Join(store.Dir(), "audit.jsonl")); err != nil || !bytes.Contains(audit, []byte("authorize_setup_baseline_merged_branch_retirement")) || !bytes.Contains(audit, []byte("setup_baseline_merged_head_drift_recapture")) {
		t.Fatalf("merged drift retirement audit is incomplete: %q err=%v", audit, err)
	}
	if reset.Phase != SetupBaselinePending || reset.CaptureRunID != 0 || reset.PRNumber != 0 || reset.MergeSHA != "" {
		t.Fatalf("merged head drift did not require clean hosted recapture: %+v", reset)
	}
	if len(reset.PreviouslyApprovedCandidates) != 1 || reset.PreviouslyApprovedDigest != intent.CandidateDigest {
		t.Fatalf("merged head drift did not preserve the exact approved candidate identity: %+v", reset)
	}
}

func TestUninstallDrainsMergedAndProductionVerifiedSetupBaselines(t *testing.T) {
	for _, phase := range []string{SetupBaselineMerged, SetupBaselineProductionVerified} {
		t.Run(phase, func(t *testing.T) {
			intent, config, rawDiff := mergedSetupBaselineIntentFixture(t, phase)
			store, err := NewStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if err := store.SaveSetupBaselineIntent(intent); err != nil {
				t.Fatal(err)
			}
			deletes, failures := 0, 0
			server := newMergedSetupBaselineRetirementServer(t, intent, rawDiff, intent.CommitSHA, &deletes, &failures)
			defer server.Close()
			client := hivegithub.NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
			if err := drainSetupBaselineForUninstall(context.Background(), store, config, client); err != nil {
				t.Fatal(err)
			}
			if deletes != 1 {
				t.Fatalf("merged baseline branch delete count=%d, want 1", deletes)
			}
			if _, exists, err := store.LoadSetupBaselineIntent(); err != nil || exists {
				t.Fatalf("merged baseline intent survived uninstall drain: exists=%t err=%v", exists, err)
			}
			if audit, err := os.ReadFile(filepath.Join(store.Dir(), "audit.jsonl")); err != nil || !bytes.Contains(audit, []byte("authorize_setup_baseline_merged_branch_retirement")) || !bytes.Contains(audit, []byte("uninstall_setup_baseline_drained")) {
				t.Fatalf("merged uninstall retirement audit is incomplete: %q err=%v", audit, err)
			}
		})
	}
}

func TestUninstallMergedSetupBaselineMissingRefIsIdempotentAndMovedRefFailsClosed(t *testing.T) {
	for _, test := range []struct {
		name      string
		refHead   string
		wantError bool
	}{
		{name: "missing ref", refHead: ""},
		{name: "moved non-descendant ref", refHead: strings.Repeat("9", 40), wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			intent, config, rawDiff := mergedSetupBaselineIntentFixture(t, SetupBaselineProductionVerified)
			store, err := NewStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if err := store.SaveSetupBaselineIntent(intent); err != nil {
				t.Fatal(err)
			}
			deletes, failures := 0, 0
			server := newMergedSetupBaselineRetirementServer(t, intent, rawDiff, test.refHead, &deletes, &failures)
			defer server.Close()
			client := hivegithub.NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
			err = drainSetupBaselineForUninstall(context.Background(), store, config, client)
			if (err != nil) != test.wantError {
				t.Fatalf("drain error=%v, want error=%t", err, test.wantError)
			}
			_, exists, loadErr := store.LoadSetupBaselineIntent()
			if loadErr != nil || exists != test.wantError {
				t.Fatalf("intent retention mismatch: exists=%t want=%t err=%v", exists, test.wantError, loadErr)
			}
		})
	}
}

func mergedSetupBaselineIntentFixture(t *testing.T, phase string) (SetupBaselineIntent, Config, string) {
	t.Helper()
	baseSHA, commitSHA, mergeSHA := strings.Repeat("a", 40), strings.Repeat("f", 40), strings.Repeat("d", 40)
	candidate := SetupBaselineCandidate{Path: ".visual-hive/snapshots/linux/home.png", SHA256: strings.Repeat("e", 64), Bytes: 9}
	digest, err := setupBaselineCandidateDigest([]SetupBaselineCandidate{candidate})
	if err != nil {
		t.Fatal(err)
	}
	emptyDigest, err := setupBaselineCandidateDigest(nil)
	if err != nil {
		t.Fatal(err)
	}
	rawDiff := "diff --git a/.visual-hive/snapshots/linux/home.png b/.visual-hive/snapshots/linux/home.png\nnew baseline\n"
	now := time.Now().UTC()
	intent := SetupBaselineIntent{
		SchemaVersion: SetupBaselineSchema, Phase: phase, Repository: "owner/repo", RepositoryID: "123", DefaultBranch: "main",
		AuthorizerID: 42, SetupPRNumber: 1, SetupPRURL: "https://example.test/pull/1", SetupHeadSHA: baseSHA,
		VisualHiveConfigDigest: strings.Repeat("8", 64), ScreenshotContractDigest: strings.Repeat("9", 64), InitialBaselineDigest: emptyDigest,
		CaptureCorrelation: strings.Repeat("c", 64), CaptureHeadSHA: baseSHA, CaptureRunID: 77, CaptureRunURL: "https://example.test/run/77",
		ArtifactID: 88, ArtifactName: "artifact", ArtifactRoot: "root", CaptureCandidateDigest: digest, CaptureCandidates: []SetupBaselineCandidate{candidate},
		CandidateDigest: digest, Candidates: []SetupBaselineCandidate{candidate}, Branch: "hive/setup-baseline-123", CommitSHA: commitSHA,
		Marker: setupBaselineMarker("owner/repo", strings.Repeat("c", 64)), PRNumber: 2, PRURL: "https://example.test/pull/2", BaseSHA: baseSHA,
		DiffDigest: digestTestDiff(rawDiff), ApprovalPlanDigest: strings.Repeat("2", 64), ApprovalActorID: 42, ApprovalReason: "reviewed",
		MergeAttemptedAt: now, MergeSHA: mergeSHA, CreatedAt: now, UpdatedAt: now, DispatchAttemptedAt: now, DispatchAcknowledgedAt: now,
	}
	if phase == SetupBaselineProductionVerified {
		intent.ProductionRunID, intent.ProductionHeadSHA = 99, mergeSHA
	}
	config := Config{Repository: "owner/repo", RepositoryID: "123", SetupAuthorizationActorID: 42}
	return intent, config, rawDiff
}

func TestEmptySetupBaselineInventoryHasOneCanonicalDigestAfterJSONRoundTrip(t *testing.T) {
	nilDigest, err := setupBaselineCandidateDigest(nil)
	if err != nil {
		t.Fatal(err)
	}
	emptyDigest, err := setupBaselineCandidateDigest([]SetupBaselineCandidate{})
	if err != nil {
		t.Fatal(err)
	}
	if nilDigest != emptyDigest {
		t.Fatalf("nil and empty setup baseline inventories have different digests: nil=%s empty=%s", nilDigest, emptyDigest)
	}
	now := time.Now().UTC()
	intent := SetupBaselineIntent{
		SchemaVersion: SetupBaselineSchema, Phase: SetupBaselinePending, Repository: "owner/repo", RepositoryID: "123", DefaultBranch: "main",
		AuthorizerID: 42, SetupPRNumber: 1, SetupPRURL: "https://example.test/pull/1", SetupHeadSHA: strings.Repeat("a", 40),
		VisualHiveConfigDigest: strings.Repeat("b", 64), ScreenshotContractDigest: strings.Repeat("c", 64), InitialBaselineDigest: emptyDigest,
		InitialBaselineCandidates: []SetupBaselineCandidate{}, CreatedAt: now, UpdatedAt: now,
	}
	data, err := json.Marshal(intent)
	if err != nil {
		t.Fatal(err)
	}
	var roundTripped SetupBaselineIntent
	if err := json.Unmarshal(data, &roundTripped); err != nil {
		t.Fatal(err)
	}
	if roundTripped.InitialBaselineCandidates != nil {
		t.Fatalf("omitempty round trip unexpectedly retained empty inventory: %#v", roundTripped.InitialBaselineCandidates)
	}
	if err := validateSetupBaselineIntent(roundTripped); err != nil {
		t.Fatalf("empty setup baseline inventory became invalid after durable JSON round trip: %v", err)
	}
}

func newMergedSetupBaselineRetirementServer(t *testing.T, intent SetupBaselineIntent, rawDiff, refHead string, deletes, failures *int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo":
			_, _ = io.WriteString(writer, `{"id":123,"full_name":"owner/repo"}`)
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/pulls/2" && strings.Contains(request.Header.Get("Accept"), "diff"):
			writer.Header().Set("Content-Type", "application/vnd.github.v3.diff")
			_, _ = io.WriteString(writer, rawDiff)
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/pulls/2":
			_, _ = fmt.Fprintf(writer, `{"number":2,"changed_files":%d,"html_url":"https://example.test/pull/2","state":"closed","merged":true,"merge_commit_sha":%q,"body":%q,"head":{"ref":%q,"sha":%q,"repo":{"id":123,"full_name":"owner/repo"}},"base":{"ref":"main","sha":%q,"repo":{"id":123,"full_name":"owner/repo"}}}`, len(intent.Candidates), intent.MergeSHA, intent.Marker, intent.Branch, intent.CommitSHA, intent.BaseSHA)
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/pulls/2/files":
			files := make([]map[string]string, 0, len(intent.Candidates))
			for _, candidate := range intent.Candidates {
				files = append(files, map[string]string{"filename": candidate.Path})
			}
			if err := json.NewEncoder(writer).Encode(files); err != nil {
				t.Fatal(err)
			}
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/git/ref/heads/"+intent.Branch:
			if refHead == "" {
				http.Error(writer, "missing", http.StatusNotFound)
				return
			}
			_, _ = fmt.Fprintf(writer, `{"ref":%q,"object":{"sha":%q,"type":"commit"}}`, "refs/heads/"+intent.Branch, refHead)
		case request.Method == http.MethodGet && strings.Contains(request.URL.Path, "/compare/"):
			_, _ = io.WriteString(writer, `{"status":"diverged","ahead_by":1,"behind_by":1}`)
		case request.Method == http.MethodDelete && request.URL.Path == "/repos/owner/repo/git/refs/heads/"+intent.Branch:
			*deletes++
			if *failures > 0 {
				*failures--
				http.Error(writer, "transient", http.StatusServiceUnavailable)
				return
			}
			writer.WriteHeader(http.StatusNoContent)
		default:
			http.Error(writer, request.Method+" "+request.URL.Path, http.StatusNotFound)
		}
	}))
}

func TestUninstallMergedSetupBaselineTransientDeleteRetriesExactly(t *testing.T) {
	intent, config, rawDiff := mergedSetupBaselineIntentFixture(t, SetupBaselineProductionVerified)
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSetupBaselineIntent(intent); err != nil {
		t.Fatal(err)
	}
	deletes, failures := 0, 1
	server := newMergedSetupBaselineRetirementServer(t, intent, rawDiff, intent.CommitSHA, &deletes, &failures)
	defer server.Close()
	client := hivegithub.NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	if err := drainSetupBaselineForUninstall(context.Background(), store, config, client); err == nil {
		t.Fatal("transient merged baseline deletion did not preserve retryable intent")
	}
	if _, exists, err := store.LoadSetupBaselineIntent(); err != nil || !exists {
		t.Fatalf("transient deletion lost intent: exists=%t err=%v", exists, err)
	}
	if err := drainSetupBaselineForUninstall(context.Background(), store, config, client); err != nil {
		t.Fatal(err)
	}
	if deletes != 2 {
		t.Fatalf("delete attempts=%d, want 2", deletes)
	}
}

func TestInitialPendingExternalBaselinesCannotBypassSpecializedApproval(t *testing.T) {
	intent := SetupBaselineIntent{
		Candidates:      []SetupBaselineCandidate{{Path: ".visual-hive/snapshots/linux/external.png", SHA256: strings.Repeat("a", 64), Bytes: 9}},
		CandidateDigest: strings.Repeat("b", 64),
	}
	matched, err := canReusePreviouslyApprovedSetupBaseline(context.Background(), nil, Config{}, intent, strings.Repeat("c", 40))
	if err != nil || matched {
		t.Fatalf("initial unapproved candidates were accepted as previously reviewed: matched=%t err=%v", matched, err)
	}
}

func TestUninstallDrainsPendingSetupBaselineBeforeRecoveryCheck(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	head := strings.Repeat("a", 40)
	config := Config{Repository: "owner/repo", RepositoryID: "123", DefaultBranch: "main", SetupAuthorizationActorID: 42, SetupHeadSHA: head,
		VisualHiveConfigDigest: strings.Repeat("8", 64), SetupBaselineContractDigest: strings.Repeat("9", 64), SetupBaselineInitialDigest: emptySetupBaselineCandidateDigest(t)}
	if _, err := ensureSetupBaselineIntent(store, config, head, 1, "https://example.test/pull/1"); err != nil {
		t.Fatal(err)
	}
	if err := verifyUninstallRecoveryState(store); err == nil {
		t.Fatal("pending setup baseline was omitted from uninstall recovery state")
	}
	if err := drainSetupBaselineForUninstall(context.Background(), store, config, nil); err != nil {
		t.Fatal(err)
	}
	if _, exists, err := store.LoadSetupBaselineIntent(); err != nil || exists {
		t.Fatalf("pending setup baseline survived uninstall drain: exists=%t err=%v", exists, err)
	}
	if err := verifyUninstallRecoveryState(store); err != nil {
		t.Fatalf("uninstall recovery state remained blocked after drain: %v", err)
	}
}

func TestIdempotentSetupRecreatesMissingRequiredBaselineIntent(t *testing.T) {
	stateDir := t.TempDir()
	store, err := NewStore(filepath.Join(stateDir, "integrated"))
	if err != nil {
		t.Fatal(err)
	}
	head := strings.Repeat("a", 40)
	config := Config{
		SchemaVersion: ConfigSchema, Repository: "owner/repo", RepositoryID: "123", DefaultBranch: "main", StateDir: stateDir,
		SetupBranch: "hive/setup-123", SetupPRNumber: 7, SetupPRURL: "https://example.test/owner/repo/pull/7", SetupHeadSHA: head,
		SetupAuthorizationActorID: 42, SetupBaselineRequired: true, VisualHiveConfigDigest: strings.Repeat("c", 64), SetupBaselineContractDigest: strings.Repeat("d", 64), SetupBaselineInitialDigest: emptySetupBaselineCandidateDigest(t),
	}
	// This is the crash boundary: config is durable but setup-baseline.json is
	// not. The idempotent setup completion helper must restore it.
	if err := store.Save(config); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load()
	if err != nil || !loaded.SetupBaselineRequired {
		t.Fatalf("durable baseline-required bit was lost: config=%+v err=%v", loaded, err)
	}
	first := SetupResult{}
	if err := setSetupBaselineStatus(&first, store, loaded, head); err != nil {
		t.Fatal(err)
	}
	intent, exists, err := store.LoadSetupBaselineIntent()
	if err != nil || !exists || intent.Phase != SetupBaselinePending || intent.SetupPRNumber != 7 || intent.SetupHeadSHA != head {
		t.Fatalf("idempotent setup did not restore exact pending intent: exists=%t intent=%+v err=%v", exists, intent, err)
	}
	second := SetupResult{}
	if err := setSetupBaselineStatus(&second, store, loaded, head); err != nil {
		t.Fatal(err)
	}
	again, exists, err := store.LoadSetupBaselineIntent()
	if err != nil || !exists || !reflect.DeepEqual(intent, again) || !first.SetupBaselinePending || !second.SetupBaselinePending {
		t.Fatalf("second setup rerun changed or lost the recovered intent: first=%+v second=%+v again=%+v err=%v", first, second, again, err)
	}
}

func TestRunSetupBaselineGateRecoversMissingRequiredIntentFailClosed(t *testing.T) {
	stateDir := t.TempDir()
	store, err := NewStore(filepath.Join(stateDir, "integrated"))
	if err != nil {
		t.Fatal(err)
	}
	head := strings.Repeat("b", 40)
	config := Config{
		SchemaVersion: ConfigSchema, Repository: "owner/repo", RepositoryID: "123", DefaultBranch: "main", StateDir: stateDir,
		SetupBranch: "hive/setup-123", SetupPRNumber: 7, SetupPRURL: "https://example.test/owner/repo/pull/7", SetupHeadSHA: strings.Repeat("a", 40),
		SetupAuthorizationActorID: 42, SetupBaselineRequired: true, VisualHiveConfigDigest: strings.Repeat("c", 64), SetupBaselineContractDigest: strings.Repeat("d", 64), SetupBaselineInitialDigest: emptySetupBaselineCandidateDigest(t),
	}
	if err := store.Save(config); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/branches/main" {
			_, _ = io.WriteString(writer, `{"name":"main","commit":{"sha":"`+head+`"}}`)
			return
		}
		http.Error(writer, request.Method+" "+request.URL.Path, http.StatusNotFound)
	}))
	defer server.Close()
	client := hivegithub.NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())

	intent, blocked, err := reconcileRequiredSetupBaselineBeforeRun(context.Background(), store, config, client)
	if err == nil || !blocked || intent.Phase != SetupBaselinePending || !strings.Contains(err.Error(), "recovered missing required setup baseline checkpoint") {
		t.Fatalf("production run did not recover and stop at the missing baseline gate: blocked=%t intent=%+v err=%v", blocked, intent, err)
	}
	persisted, exists, loadErr := store.LoadSetupBaselineIntent()
	if loadErr != nil || !exists || persisted.SetupHeadSHA != config.SetupHeadSHA || persisted.SetupPRNumber != config.SetupPRNumber {
		t.Fatalf("recovered production gate was not durable: exists=%t intent=%+v err=%v", exists, persisted, loadErr)
	}
}

func emptySetupBaselineCandidateDigest(t *testing.T) string {
	t.Helper()
	digest, err := setupBaselineCandidateDigest(nil)
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

func TestSetupBaselineReviewDeltaTrustsOnlyIntentBoundInventory(t *testing.T) {
	png := func(label string) ([]byte, SetupBaselineCandidate) {
		content := append([]byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}, []byte(label)...)
		digest := sha256.Sum256(content)
		return content, SetupBaselineCandidate{Path: ".visual-hive/snapshots/linux/" + label + ".png", SHA256: hex.EncodeToString(digest[:]), Bytes: int64(len(content))}
	}
	homeBytes, home := png("home")
	newBytes, newlyCaptured := png("new")
	externalBytes, external := png("external")
	contents := map[string][]byte{home.Path: homeBytes, newlyCaptured.Path: newBytes, external.Path: externalBytes}
	blobs := map[string][]byte{}
	allInventory := false
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case request.URL.Path == "/repos/owner/repo/git/trees/"+strings.Repeat("a", 40):
			paths := []string{home.Path}
			if request.URL.Query().Get("recursive") == "1" && allInventory {
				paths = []string{home.Path, newlyCaptured.Path, external.Path}
			}
			entries := ""
			for index, path := range paths {
				if index > 0 {
					entries += ","
				}
				sha := strings.Repeat(fmt.Sprint((index+1)%10), 40)
				blobs[sha] = contents[path]
				entries += fmt.Sprintf(`{"path":%q,"mode":"100644","type":"blob","sha":%q,"size":%d}`, path, sha, len(contents[path]))
			}
			_, _ = fmt.Fprintf(writer, `{"truncated":false,"tree":[%s]}`, entries)
		case strings.HasPrefix(request.URL.Path, "/repos/owner/repo/git/blobs/"):
			sha := strings.TrimPrefix(request.URL.Path, "/repos/owner/repo/git/blobs/")
			content, exists := blobs[sha]
			if !exists {
				http.Error(writer, "missing", http.StatusNotFound)
				return
			}
			_, _ = writer.Write(content)
		case strings.HasPrefix(request.URL.Path, "/repos/owner/repo/contents/"):
			path := strings.TrimPrefix(request.URL.Path, "/repos/owner/repo/contents/")
			content, exists := contents[path]
			if !exists {
				http.Error(writer, "missing", http.StatusNotFound)
				return
			}
			_, _ = fmt.Fprintf(writer, `{"type":"file","path":%q,"encoding":"base64","content":%q}`, path, base64.StdEncoding.EncodeToString(content))
		default:
			http.Error(writer, request.Method+" "+request.URL.Path, http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := hivegithub.NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	config := Config{Repository: "owner/repo"}
	head := strings.Repeat("a", 40)

	partial := SetupBaselineIntent{CaptureCandidates: []SetupBaselineCandidate{home, newlyCaptured}, InitialBaselineCandidates: []SetupBaselineCandidate{home}}
	delta, err := setupBaselineReviewDeltaAtCommit(context.Background(), client, config, partial, head)
	if err != nil || len(delta) != 1 || delta[0] != newlyCaptured {
		t.Fatalf("partial initial inventory did not preserve home and review new: delta=%+v err=%v", delta, err)
	}

	allInventory = true
	allTrusted := SetupBaselineIntent{CaptureCandidates: []SetupBaselineCandidate{home, newlyCaptured}, InitialBaselineCandidates: []SetupBaselineCandidate{home, newlyCaptured}}
	delta, err = setupBaselineReviewDeltaAtCommit(context.Background(), client, config, allTrusted, head)
	if err != nil || len(delta) != 0 {
		t.Fatalf("all intent-bound preexisting baselines were not reused: delta=%+v err=%v", delta, err)
	}
	externalAfterIntent := SetupBaselineIntent{CaptureCandidates: []SetupBaselineCandidate{home, external}, InitialBaselineCandidates: []SetupBaselineCandidate{home}}
	delta, err = setupBaselineReviewDeltaAtCommit(context.Background(), client, config, externalAfterIntent, head)
	if err != nil || len(delta) != 1 || delta[0] != external {
		t.Fatalf("external baseline added after intent bypassed specialized review: delta=%+v err=%v", delta, err)
	}
}

func TestActiveSetupBaselinePhaseTableDurablyRebindsBeforeConfigSave(t *testing.T) {
	head := strings.Repeat("a", 40)
	visualDigest, contractDigest := strings.Repeat("b", 64), strings.Repeat("c", 64)
	emptyDigest := emptySetupBaselineCandidateDigest(t)
	config := Config{Repository: "owner/repo", RepositoryID: "123", DefaultBranch: "main", SetupAuthorizationActorID: 42,
		SetupPRNumber: 7, SetupPRURL: "https://example.test/pull/7", SetupHeadSHA: head, VisualHiveConfigDigest: visualDigest,
		SetupBaselineContractDigest: contractDigest, SetupBaselineInitialDigest: emptyDigest}
	png := SetupBaselineCandidate{Path: ".visual-hive/snapshots/linux/home.png", SHA256: strings.Repeat("d", 64), Bytes: 9}
	pngDigest, _ := setupBaselineCandidateDigest([]SetupBaselineCandidate{png})
	for _, phase := range []string{SetupBaselinePending, SetupBaselineDispatched, SetupBaselineArtifactVerified, SetupBaselineProposalPrepared, SetupBaselinePROpen, SetupBaselineApproved, SetupBaselineMerged} {
		t.Run(phase, func(t *testing.T) {
			store, err := NewStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			intent := SetupBaselineIntent{SchemaVersion: SetupBaselineSchema, Phase: phase, Repository: config.Repository, RepositoryID: config.RepositoryID,
				DefaultBranch: config.DefaultBranch, AuthorizerID: config.SetupAuthorizationActorID, SetupPRNumber: config.SetupPRNumber,
				SetupPRURL: config.SetupPRURL, SetupHeadSHA: head, VisualHiveConfigDigest: visualDigest, ScreenshotContractDigest: contractDigest,
				InitialBaselineDigest: emptyDigest, CreatedAt: now, UpdatedAt: now}
			if phase != SetupBaselinePending {
				intent.CaptureCorrelation, intent.CaptureHeadSHA, intent.DispatchAttemptedAt = strings.Repeat("e", 64), head, now
			}
			if phase != SetupBaselinePending && phase != SetupBaselineDispatched {
				intent.CaptureRunID, intent.CaptureRunURL, intent.DispatchAcknowledgedAt = 77, "https://example.test/run/77", now
				intent.ArtifactID, intent.ArtifactName, intent.ArtifactRoot = 88, "artifact", "root"
				intent.CaptureCandidateDigest, intent.CaptureCandidates = pngDigest, []SetupBaselineCandidate{png}
			}
			if phase == SetupBaselineProposalPrepared || phase == SetupBaselinePROpen || phase == SetupBaselineApproved || phase == SetupBaselineMerged {
				intent.CandidateDigest, intent.Candidates = pngDigest, []SetupBaselineCandidate{png}
				intent.Branch, intent.CommitSHA, intent.Marker = "hive/setup-baseline-123", strings.Repeat("f", 40), setupBaselineMarker(config.Repository, intent.CaptureCorrelation)
			}
			if phase == SetupBaselinePROpen || phase == SetupBaselineApproved || phase == SetupBaselineMerged {
				intent.PRNumber, intent.PRURL, intent.BaseSHA, intent.DiffDigest = 9, "https://example.test/pull/9", head, strings.Repeat("1", 64)
			}
			if phase == SetupBaselineApproved || phase == SetupBaselineMerged {
				intent.ApprovalPlanDigest, intent.ApprovalActorID, intent.ApprovalReason = strings.Repeat("2", 64), 42, "reviewed"
			}
			if phase == SetupBaselineMerged {
				intent.MergeSHA, intent.MergeAttemptedAt = strings.Repeat("3", 40), now
			}
			if err := store.SaveSetupBaselineIntent(intent); err != nil {
				t.Fatal(err)
			}
			client, closeServer := newSetupBaselineRebindTestClient(t, intent, nil)
			defer closeServer()
			if err := reconcileActiveSetupBaselineReconfiguration(context.Background(), store, config, head, true, client); err != nil {
				t.Fatalf("phase %s rejected exact idempotent setup: %v", phase, err)
			}
			changed := config
			changed.VisualHiveConfigDigest = strings.Repeat("9", 64)
			if err := reconcileActiveSetupBaselineReconfiguration(context.Background(), store, changed, head, false, client); err != nil {
				t.Fatalf("phase %s did not complete durable rebind: %v", phase, err)
			}
			if _, exists, err := store.LoadSetupBaselineIntent(); err != nil || exists {
				t.Fatalf("phase %s source intent survived completed retirement: exists=%t err=%v", phase, exists, err)
			}
			rebind, exists, err := store.LoadSetupBaselineRebindIntent()
			if err != nil || !exists || rebind.Phase != SetupBaselineRebindResourcesRetired || rebind.TargetConfigDigest == "" {
				t.Fatalf("phase %s lacks durable rebind checkpoint: exists=%t intent=%+v err=%v", phase, exists, rebind, err)
			}
		})
	}
}

func TestReconcileVerifiedSetupBaselineDispatchPreservesPostMergeProductionRetry(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	baseline := SetupBaselineIntent{
		Phase: SetupBaselineMerged, CaptureCorrelation: strings.Repeat("a", 64), CaptureRunID: 77,
	}
	production, err := newWorkflowDispatchIntentForOperation(
		Config{Repository: "owner/repo", RepositoryID: "123"}, "hive-visual-hive.yml", "main", "production", strings.Repeat("b", 64), now,
	)
	if err != nil {
		t.Fatal(err)
	}
	production.DispatchAttemptedAt, production.DispatchAcknowledgedAt = now, now
	production.RunID, production.RunURL, production.MatchedAt = 88, "https://example.test/runs/88", now
	if err := store.SaveWorkflowDispatchIntent(production); err != nil {
		t.Fatal(err)
	}
	if err := reconcileVerifiedSetupBaselineDispatch(store, baseline); err != nil {
		t.Fatalf("post-merge production retry was rejected: %v", err)
	}
	got, exists, err := store.LoadWorkflowDispatchIntent()
	if err != nil || !exists || got.Operation != "production" || got.RunID != 88 || got.CorrelationID != production.CorrelationID {
		t.Fatalf("exact production retry checkpoint was not preserved: exists=%t intent=%+v err=%v", exists, got, err)
	}
}

func TestPostBaselineProductionValidationRequiresCanonicalPassedTrustedAuthoritative(t *testing.T) {
	for _, test := range []struct {
		name          string
		status        string
		trusted       bool
		authoritative bool
		want          bool
	}{
		{name: "canonical", status: "passed", trusted: true, authoritative: true, want: true},
		{name: "noncanonical status", status: "valid", trusted: true, authoritative: true},
		{name: "untrusted", status: "passed", authoritative: true},
		{name: "non-authoritative", status: "passed", trusted: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := postBaselineProductionValidationAccepted(test.status, test.trusted, test.authoritative); got != test.want {
				t.Fatalf("acceptance=%t want=%t", got, test.want)
			}
		})
	}
}

func TestReconcileVerifiedSetupBaselineDispatchConsumesOnlyExactCapture(t *testing.T) {
	for _, test := range []struct {
		name        string
		correlation string
		runID       int64
		wantError   bool
	}{
		{name: "exact", correlation: strings.Repeat("a", 64), runID: 77},
		{name: "wrong correlation", correlation: strings.Repeat("b", 64), runID: 77, wantError: true},
		{name: "wrong run", correlation: strings.Repeat("a", 64), runID: 78, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, err := NewStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			baseline := SetupBaselineIntent{Phase: SetupBaselineArtifactVerified, CaptureCorrelation: strings.Repeat("a", 64), CaptureRunID: 77}
			dispatch, err := newWorkflowDispatchIntentForOperation(
				Config{Repository: "owner/repo", RepositoryID: "123"}, "hive-visual-hive.yml", "main", setupBaselineWorkflowOperation, test.correlation, now,
			)
			if err != nil {
				t.Fatal(err)
			}
			dispatch.DispatchAttemptedAt, dispatch.DispatchAcknowledgedAt = now, now
			dispatch.RunID, dispatch.RunURL, dispatch.MatchedAt = test.runID, "https://example.test/runs/77", now
			if err := store.SaveWorkflowDispatchIntent(dispatch); err != nil {
				t.Fatal(err)
			}
			err = reconcileVerifiedSetupBaselineDispatch(store, baseline)
			if test.wantError {
				if err == nil || !strings.Contains(err.Error(), "verified capture") {
					t.Fatalf("mismatched capture was accepted: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, exists, err := store.LoadWorkflowDispatchIntent(); err != nil || exists {
				t.Fatalf("exact consumed capture checkpoint remains: exists=%t err=%v", exists, err)
			}
		})
	}
}

func newSetupBaselineRebindTestClient(t *testing.T, intent SetupBaselineIntent, failClose *bool) (*hivegithub.Client, func()) {
	t.Helper()
	closed, branchExists := false, intent.Branch != ""
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		pullJSON := func(state string) string {
			return fmt.Sprintf(`{"number":9,"html_url":"https://example.test/pull/9","state":%q,"merged":false,"body":%q,"head":{"sha":%q,"ref":%q,"repo":{"id":123,"full_name":"owner/repo"}},"base":{"sha":%q,"ref":"main","repo":{"id":123,"full_name":"owner/repo"}}}`, state, intent.Marker, intent.CommitSHA, intent.Branch, intent.BaseSHA)
		}
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo":
			_, _ = io.WriteString(writer, `{"id":123,"full_name":"owner/repo"}`)
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/actions/runs/77":
			_, _ = fmt.Fprintf(writer, `{"id":77,"display_title":%q,"path":%q,"event":"workflow_dispatch","head_sha":%q,"status":"completed","conclusion":"success","repository":{"id":123,"full_name":"owner/repo"}}`, workflowDispatchDisplayTitle(intent.CaptureCorrelation), visualHiveProductionWorkflowPath, intent.CaptureHeadSHA)
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/pulls/9":
			state := "open"
			if closed {
				state = "closed"
			}
			_, _ = io.WriteString(writer, pullJSON(state))
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/pulls":
			if intent.PRNumber > 0 {
				_, _ = io.WriteString(writer, "["+pullJSON("open")+"]")
			} else {
				_, _ = io.WriteString(writer, `[]`)
			}
		case request.Method == http.MethodPatch && request.URL.Path == "/repos/owner/repo/pulls/9":
			if failClose != nil && *failClose {
				*failClose = false
				http.Error(writer, "transient close failure", http.StatusServiceUnavailable)
				return
			}
			closed = true
			_, _ = io.WriteString(writer, pullJSON("closed"))
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/git/ref/heads/"+intent.Branch:
			if !branchExists {
				http.Error(writer, "missing", http.StatusNotFound)
				return
			}
			_, _ = fmt.Fprintf(writer, `{"ref":%q,"object":{"sha":%q,"type":"commit"}}`, "refs/heads/"+intent.Branch, intent.CommitSHA)
		case request.Method == http.MethodDelete && request.URL.Path == "/repos/owner/repo/git/refs/heads/"+intent.Branch:
			branchExists = false
			writer.WriteHeader(http.StatusNoContent)
		default:
			http.Error(writer, request.Method+" "+request.URL.Path, http.StatusNotFound)
		}
	}))
	return hivegithub.NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default()), server.Close
}

func TestSetupBaselineRebindRetriesFromDurablePreparedCheckpointAfterExternalFailure(t *testing.T) {
	head, now := strings.Repeat("a", 40), time.Now().UTC()
	emptyDigest := emptySetupBaselineCandidateDigest(t)
	png := SetupBaselineCandidate{Path: ".visual-hive/snapshots/linux/home.png", SHA256: strings.Repeat("d", 64), Bytes: 9}
	pngDigest, _ := setupBaselineCandidateDigest([]SetupBaselineCandidate{png})
	config := Config{Repository: "owner/repo", RepositoryID: "123", DefaultBranch: "main", SetupAuthorizationActorID: 42,
		SetupPRNumber: 7, SetupPRURL: "https://example.test/pull/7", SetupHeadSHA: head, VisualHiveConfigDigest: strings.Repeat("b", 64),
		SetupBaselineContractDigest: strings.Repeat("c", 64), SetupBaselineInitialDigest: emptyDigest, StateDir: t.TempDir(), Coverage: CoverageComprehensive, Automation: AutomationRepairPR}
	intent := SetupBaselineIntent{SchemaVersion: SetupBaselineSchema, Phase: SetupBaselinePROpen, Repository: config.Repository, RepositoryID: config.RepositoryID,
		DefaultBranch: config.DefaultBranch, AuthorizerID: config.SetupAuthorizationActorID, SetupPRNumber: config.SetupPRNumber, SetupPRURL: config.SetupPRURL,
		SetupHeadSHA: head, VisualHiveConfigDigest: config.VisualHiveConfigDigest, ScreenshotContractDigest: config.SetupBaselineContractDigest,
		InitialBaselineDigest: emptyDigest, CaptureCorrelation: strings.Repeat("e", 64), CaptureHeadSHA: head, CaptureRunID: 77,
		CaptureRunURL: "https://example.test/run/77", DispatchAttemptedAt: now, DispatchAcknowledgedAt: now, ArtifactID: 88, ArtifactName: "artifact", ArtifactRoot: "root",
		CaptureCandidateDigest: pngDigest, CaptureCandidates: []SetupBaselineCandidate{png}, CandidateDigest: pngDigest, Candidates: []SetupBaselineCandidate{png},
		Branch: "hive/setup-baseline-123", CommitSHA: strings.Repeat("f", 40), Marker: setupBaselineMarker(config.Repository, strings.Repeat("e", 64)),
		PRNumber: 9, PRURL: "https://example.test/pull/9", BaseSHA: head, DiffDigest: strings.Repeat("1", 64), CreatedAt: now, UpdatedAt: now}
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSetupBaselineIntent(intent); err != nil {
		t.Fatal(err)
	}
	changed := config
	changed.VisualHiveConfigDigest = strings.Repeat("9", 64)
	failClose := true
	client, closeServer := newSetupBaselineRebindTestClient(t, intent, &failClose)
	defer closeServer()
	if err := reconcileActiveSetupBaselineReconfiguration(context.Background(), store, changed, head, false, client); err == nil || !strings.Contains(err.Error(), "durably prepared and retryable") {
		t.Fatalf("transient external failure did not expose retryable durable recovery: %v", err)
	}
	rebind, exists, err := store.LoadSetupBaselineRebindIntent()
	if err != nil || !exists || rebind.Phase != SetupBaselineRebindPrepared {
		t.Fatalf("prepared checkpoint was lost after external failure: exists=%t intent=%+v err=%v", exists, rebind, err)
	}
	if _, exists, err := store.LoadSetupBaselineIntent(); err != nil || !exists {
		t.Fatalf("source intent was removed before external cleanup completed: exists=%t err=%v", exists, err)
	}
	if err := reconcileActiveSetupBaselineReconfiguration(context.Background(), store, changed, head, false, client); err != nil {
		t.Fatalf("durable setup baseline cancellation retry failed: %v", err)
	}
	rebind, exists, err = store.LoadSetupBaselineRebindIntent()
	if err != nil || !exists || rebind.Phase != SetupBaselineRebindResourcesRetired {
		t.Fatalf("retry did not complete exact retirement: exists=%t intent=%+v err=%v", exists, rebind, err)
	}
	different := changed
	different.Coverage = CoverageEssential
	if err := reconcileActiveSetupBaselineReconfiguration(context.Background(), store, different, head, false, client); err == nil || !strings.Contains(err.Error(), "immutably bound") {
		t.Fatalf("changed retry target replaced durable rebind target: %v", err)
	}
}

func TestSetupBaselineRebindBlocksProductionUntilMatchingSavedSetupConsumesCheckpoint(t *testing.T) {
	now, head := time.Now().UTC(), strings.Repeat("a", 40)
	emptyDigest := emptySetupBaselineCandidateDigest(t)
	config := Config{SchemaVersion: ConfigSchema, Repository: "owner/repo", RepositoryID: "123", DefaultBranch: "main", StateDir: t.TempDir(),
		Coverage: CoverageComprehensive, Automation: AutomationRepairPR, Provider: "codex", MaxActiveIssues: 5, MaxRepairAttempts: 4,
		SetupAuthorizationActorID: 42, SetupPRNumber: 7, SetupPRURL: "https://example.test/pull/7", SetupHeadSHA: head,
		VisualHiveConfigDigest: strings.Repeat("b", 64), SetupBaselineContractDigest: strings.Repeat("c", 64), SetupBaselineInitialDigest: emptyDigest}
	source := SetupBaselineIntent{SchemaVersion: SetupBaselineSchema, Phase: SetupBaselinePending, Repository: config.Repository, RepositoryID: config.RepositoryID,
		DefaultBranch: config.DefaultBranch, AuthorizerID: config.SetupAuthorizationActorID, SetupPRNumber: config.SetupPRNumber, SetupPRURL: config.SetupPRURL,
		SetupHeadSHA: head, VisualHiveConfigDigest: config.VisualHiveConfigDigest, ScreenshotContractDigest: config.SetupBaselineContractDigest,
		InitialBaselineDigest: emptyDigest, CreatedAt: now, UpdatedAt: now}
	target := config
	target.VisualHive, target.SetupBaselineRequired = false, false
	target.VisualHiveConfigDigest, target.SetupBaselineContractDigest = "", ""
	targetDigest, err := setupBaselineRebindTargetDigest(target)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rebind := SetupBaselineRebindIntent{SchemaVersion: SetupBaselineRebindSchema, Phase: SetupBaselineRebindResourcesRetired, Repository: config.Repository,
		RepositoryID: config.RepositoryID, Source: source, TargetConfig: target, TargetConfigDigest: targetDigest, CreatedAt: now, UpdatedAt: now, ResourcesRetiredAt: now}
	if err := store.SaveSetupBaselineRebindIntent(rebind); err != nil {
		t.Fatal(err)
	}
	if _, blocked, err := reconcileRequiredSetupBaselineBeforeRun(context.Background(), store, config, nil); err == nil || !blocked || !strings.Contains(err.Error(), "hive setup") {
		t.Fatalf("production did not expose exact setup retry while rebind was pending: blocked=%t err=%v", blocked, err)
	}
	wrong := target
	wrong.Coverage = CoverageEssential
	if err := completeSetupBaselineRebind(store, wrong); err == nil {
		t.Fatal("mismatched saved setup consumed baseline rebind checkpoint")
	}
	if err := completeSetupBaselineRebind(store, target); err != nil {
		t.Fatalf("matching saved setup could not consume completed rebind: %v", err)
	}
	if _, exists, err := store.LoadSetupBaselineRebindIntent(); err != nil || exists {
		t.Fatalf("completed rebind checkpoint remained: exists=%t err=%v", exists, err)
	}
}

func TestUninstallDrainsCompletedSetupBaselineRebindCheckpoint(t *testing.T) {
	now, head := time.Now().UTC(), strings.Repeat("a", 40)
	emptyDigest := emptySetupBaselineCandidateDigest(t)
	config := Config{SchemaVersion: ConfigSchema, Repository: "owner/repo", RepositoryID: "123", DefaultBranch: "main", StateDir: t.TempDir(),
		SetupAuthorizationActorID: 42, SetupPRNumber: 7, SetupPRURL: "https://example.test/pull/7", SetupHeadSHA: head,
		VisualHiveConfigDigest: strings.Repeat("b", 64), SetupBaselineContractDigest: strings.Repeat("c", 64), SetupBaselineInitialDigest: emptyDigest}
	source := SetupBaselineIntent{SchemaVersion: SetupBaselineSchema, Phase: SetupBaselinePending, Repository: config.Repository, RepositoryID: config.RepositoryID,
		DefaultBranch: config.DefaultBranch, AuthorizerID: config.SetupAuthorizationActorID, SetupPRNumber: config.SetupPRNumber, SetupPRURL: config.SetupPRURL,
		SetupHeadSHA: head, VisualHiveConfigDigest: config.VisualHiveConfigDigest, ScreenshotContractDigest: config.SetupBaselineContractDigest,
		InitialBaselineDigest: emptyDigest, CreatedAt: now, UpdatedAt: now}
	target := config
	target.Coverage = CoverageEssential
	targetDigest, err := setupBaselineRebindTargetDigest(target)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rebind := SetupBaselineRebindIntent{SchemaVersion: SetupBaselineRebindSchema, Phase: SetupBaselineRebindResourcesRetired,
		Repository: config.Repository, RepositoryID: config.RepositoryID, Source: source, TargetConfig: target, TargetConfigDigest: targetDigest,
		CreatedAt: now, UpdatedAt: now, ResourcesRetiredAt: now}
	if err := store.SaveSetupBaselineRebindIntent(rebind); err != nil {
		t.Fatal(err)
	}
	if err := verifyUninstallRecoveryState(store); err == nil || !strings.Contains(err.Error(), "rebind") {
		t.Fatalf("pending rebind was omitted from uninstall recovery inventory: %v", err)
	}
	if err := drainSetupBaselineForUninstall(context.Background(), store, config, nil); err != nil {
		t.Fatal(err)
	}
	if _, exists, err := store.LoadSetupBaselineRebindIntent(); err != nil || exists {
		t.Fatalf("uninstall left setup baseline rebind checkpoint: exists=%t err=%v", exists, err)
	}
	if _, exists, err := store.LoadSetupBaselineIntent(); err != nil || exists {
		t.Fatalf("uninstall left rebind source baseline checkpoint: exists=%t err=%v", exists, err)
	}
	if err := verifyUninstallRecoveryState(store); err != nil {
		t.Fatalf("uninstall recovery remained blocked after exact rebind drain: %v", err)
	}
}

func TestSetupBaselineAmbiguousDispatchUsesSharedRecoveryAndDurableTombstone(t *testing.T) {
	stateDir := t.TempDir()
	store, err := NewStore(filepath.Join(stateDir, "integrated"))
	if err != nil {
		t.Fatal(err)
	}
	head := strings.Repeat("a", 40)
	emptyDigest := emptySetupBaselineCandidateDigest(t)
	config := Config{SchemaVersion: ConfigSchema, Repository: "owner/repo", RepositoryID: "123", DefaultBranch: "main", StateDir: stateDir,
		SetupAuthorizationActorID: 42, SetupBaselineRequired: true, SetupPRNumber: 7, SetupPRURL: "https://example.test/pull/7", SetupHeadSHA: head,
		VisualHiveConfigDigest: strings.Repeat("b", 64), SetupBaselineContractDigest: strings.Repeat("c", 64), SetupBaselineInitialDigest: emptyDigest}
	if err := store.Save(config); err != nil {
		t.Fatal(err)
	}
	dispatch, err := newWorkflowDispatchIntentForOperation(config, "hive-visual-hive.yml", "main", setupBaselineWorkflowOperation, strings.Repeat("d", 64), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	dispatch.DispatchAttemptedAt = time.Now().UTC()
	if err := store.SaveWorkflowDispatchIntent(dispatch); err != nil {
		t.Fatal(err)
	}
	dispatch, _, _ = store.LoadWorkflowDispatchIntent()
	baseline := SetupBaselineIntent{SchemaVersion: SetupBaselineSchema, Phase: SetupBaselineDispatched, Repository: config.Repository, RepositoryID: config.RepositoryID,
		DefaultBranch: config.DefaultBranch, AuthorizerID: config.SetupAuthorizationActorID, SetupPRNumber: config.SetupPRNumber, SetupPRURL: config.SetupPRURL,
		SetupHeadSHA: config.SetupHeadSHA, VisualHiveConfigDigest: config.VisualHiveConfigDigest, ScreenshotContractDigest: config.SetupBaselineContractDigest,
		InitialBaselineDigest: emptyDigest, CaptureCorrelation: dispatch.CorrelationID, CaptureHeadSHA: head, DispatchAttemptedAt: dispatch.PreparedAt,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := store.SaveSetupBaselineIntent(baseline); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if request.URL.Path == "/repos/owner/repo/actions/workflows/hive-visual-hive.yml/runs" {
			_, _ = io.WriteString(writer, `{"total_count":0,"workflow_runs":[]}`)
			return
		}
		http.Error(writer, request.Method+" "+request.URL.Path, http.StatusNotFound)
	}))
	defer server.Close()
	client := hivegithub.NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	_, unchanged, err := dispatchAndWaitAttempt(context.Background(), client, store, config, "owner", "repo", "hive-visual-hive.yml", "main", setupBaselineWorkflowOperation)
	if err == nil || !strings.Contains(err.Error(), "recover-dispatch") || unchanged.Operation != setupBaselineWorkflowOperation || !WorkflowDispatchNeedsRecovery(unchanged) {
		t.Fatalf("ambiguous baseline dispatch did not use exact shared recovery: intent=%+v err=%v", unchanged, err)
	}
	if err := revokeSetupBaselineDispatch(store, config, unchanged); err != nil {
		t.Fatal(err)
	}
	reset, exists, err := store.LoadSetupBaselineIntent()
	if err != nil || !exists || reset.Phase != SetupBaselinePending || len(reset.RetiredCaptureCorrelations) != 1 || reset.RetiredCaptureCorrelations[0] != dispatch.CorrelationID {
		t.Fatalf("revoked capture lacks durable replay tombstone: exists=%t intent=%+v err=%v", exists, reset, err)
	}
	production, _ := newWorkflowDispatchIntentForOperation(config, "hive-visual-hive.yml", "main", "production", dispatch.CorrelationID, dispatch.PreparedAt)
	production.DispatchAttemptedAt = dispatch.DispatchAttemptedAt
	productionDigest, _ := workflowDispatchRequestDigest(production)
	if productionDigest == dispatch.RequestDigest {
		t.Fatal("baseline and production dispatch operations share an authorization digest")
	}
}

func TestCompletedSetupBaselineInvalidatesForNewSetupAndScreenshotContracts(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	oldHead, newHead := strings.Repeat("a", 40), strings.Repeat("b", 40)
	emptyDigest := emptySetupBaselineCandidateDigest(t)
	png := SetupBaselineCandidate{Path: ".visual-hive/snapshots/linux/home.png", SHA256: strings.Repeat("c", 64), Bytes: 9}
	fullDigest, _ := setupBaselineCandidateDigest([]SetupBaselineCandidate{png})
	oldConfig := Config{Repository: "owner/repo", RepositoryID: "123", DefaultBranch: "main", SetupAuthorizationActorID: 42,
		SetupPRNumber: 7, SetupPRURL: "https://example.test/pull/7", SetupHeadSHA: oldHead, VisualHiveConfigDigest: strings.Repeat("d", 64),
		SetupBaselineContractDigest: strings.Repeat("e", 64), SetupBaselineInitialDigest: emptyDigest}
	now := time.Now().UTC()
	completed := SetupBaselineIntent{SchemaVersion: SetupBaselineSchema, Phase: SetupBaselineProductionVerified, Repository: oldConfig.Repository, RepositoryID: oldConfig.RepositoryID,
		DefaultBranch: oldConfig.DefaultBranch, AuthorizerID: oldConfig.SetupAuthorizationActorID, SetupPRNumber: oldConfig.SetupPRNumber, SetupPRURL: oldConfig.SetupPRURL,
		SetupHeadSHA: oldHead, VisualHiveConfigDigest: oldConfig.VisualHiveConfigDigest, ScreenshotContractDigest: oldConfig.SetupBaselineContractDigest,
		InitialBaselineDigest: emptyDigest, CaptureCorrelation: strings.Repeat("f", 64), CaptureHeadSHA: oldHead, CaptureRunID: 77,
		CaptureRunURL: "https://example.test/run/77", ArtifactID: 88, ArtifactName: "artifact", ArtifactRoot: "root",
		CaptureCandidateDigest: fullDigest, CaptureCandidates: []SetupBaselineCandidate{png}, ExistingHeadReverified: true,
		MergeSHA: oldHead, MergeAttemptedAt: now, ProductionRunID: 99, ProductionHeadSHA: oldHead,
		CreatedAt: now, UpdatedAt: now, DispatchAttemptedAt: now, DispatchAcknowledgedAt: now}
	if err := store.SaveSetupBaselineIntent(completed); err != nil {
		t.Fatal(err)
	}
	changed := oldConfig
	changed.SetupPRNumber, changed.SetupPRURL, changed.SetupHeadSHA = 8, "https://example.test/pull/8", newHead
	changed.VisualHiveConfigDigest, changed.SetupBaselineContractDigest = strings.Repeat("1", 64), strings.Repeat("2", 64)
	rebound, err := ensureSetupBaselineIntent(store, changed, newHead, changed.SetupPRNumber, changed.SetupPRURL)
	if err != nil {
		t.Fatal(err)
	}
	if rebound.Phase != SetupBaselinePending || rebound.SetupPRNumber != 8 || rebound.SetupHeadSHA != newHead || rebound.VisualHiveConfigDigest != changed.VisualHiveConfigDigest || rebound.ScreenshotContractDigest != changed.SetupBaselineContractDigest {
		t.Fatalf("Visual upgrade/coverage contract change reused stale completed baselines: %+v", rebound)
	}
}

func TestSetupBaselineProductionVerifiedAuditFailureCannotBeSkipped(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now, head := time.Now().UTC(), strings.Repeat("a", 40)
	candidate := SetupBaselineCandidate{Path: ".visual-hive/snapshots/linux/home.png", SHA256: strings.Repeat("b", 64), Bytes: 9}
	candidateDigest, _ := setupBaselineCandidateDigest([]SetupBaselineCandidate{candidate})
	intent := SetupBaselineIntent{
		SchemaVersion: SetupBaselineSchema, Phase: SetupBaselineProductionVerified, Repository: "owner/repo", RepositoryID: "123", DefaultBranch: "main",
		AuthorizerID: 42, SetupPRNumber: 7, SetupPRURL: "https://example.test/pull/7", SetupHeadSHA: head,
		VisualHiveConfigDigest: strings.Repeat("c", 64), ScreenshotContractDigest: strings.Repeat("d", 64), InitialBaselineDigest: emptySetupBaselineCandidateDigest(t),
		CaptureCorrelation: strings.Repeat("e", 64), CaptureHeadSHA: head, CaptureRunID: 77, CaptureRunURL: "https://example.test/run/77",
		ArtifactID: 88, ArtifactName: "artifact", ArtifactRoot: "root", CaptureCandidateDigest: candidateDigest, CaptureCandidates: []SetupBaselineCandidate{candidate},
		ExistingHeadReverified: true, MergeSHA: head, MergeAttemptedAt: now, ProductionRunID: 99, ProductionHeadSHA: head,
		CreatedAt: now, UpdatedAt: now, DispatchAttemptedAt: now, DispatchAcknowledgedAt: now,
	}
	realAuditPath := store.auditPath
	blockedAuditPath := filepath.Join(t.TempDir(), "audit-directory")
	if err := os.Mkdir(blockedAuditPath, 0o700); err != nil {
		t.Fatal(err)
	}
	store.auditPath = blockedAuditPath
	if _, err := saveSetupBaselineTransition(store, intent, "setup_baseline_production_verified", true, "run=99 head="+head); err == nil {
		t.Fatal("audit I/O failure did not block production_verified transition")
	}
	persisted, exists, err := store.LoadSetupBaselineIntent()
	if err != nil || !exists || persisted.PendingAudit == nil {
		t.Fatalf("failed audit receipt was not durable: exists=%t intent=%+v err=%v", exists, persisted, err)
	}
	receipt := *persisted.PendingAudit
	store.auditPath = realAuditPath
	persisted, err = flushSetupBaselineAudit(store, persisted)
	if err != nil || persisted.PendingAudit != nil {
		t.Fatalf("pending production audit did not recover exactly: intent=%+v err=%v", persisted, err)
	}
	recorded, err := setupBaselineAuditRecorded(store, intent.Repository, receipt)
	if err != nil || !recorded {
		t.Fatalf("recovered production audit receipt is absent: recorded=%t err=%v", recorded, err)
	}
}

func TestSetupBaselineCandidateBlobSupportsMoreThanContentsAPILimit(t *testing.T) {
	value := append([]byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}, bytes.Repeat([]byte("x"), (2<<20)+17)...)
	digest := sha256.Sum256(value)
	candidate := SetupBaselineCandidate{Path: ".visual-hive/snapshots/linux/large.png", SHA256: hex.EncodeToString(digest[:]), Bytes: int64(len(value))}
	head, blobSHA := strings.Repeat("a", 40), strings.Repeat("b", 40)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/repos/owner/repo/git/trees/" + head:
			writer.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(writer, `{"truncated":false,"tree":[{"path":%q,"mode":"100644","type":"blob","sha":%q,"size":%d}]}`, candidate.Path, blobSHA, len(value))
		case "/repos/owner/repo/git/blobs/" + blobSHA:
			_, _ = writer.Write(value)
		default:
			http.Error(writer, request.Method+" "+request.URL.Path, http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := hivegithub.NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	delta, err := setupBaselineCandidateDeltaAtCommit(context.Background(), client, Config{Repository: "owner/repo"}, []SetupBaselineCandidate{candidate}, head)
	if err != nil || len(delta) != 0 {
		t.Fatalf("valid >1 MiB baseline blob was not verified exactly: delta=%+v err=%v", delta, err)
	}
}

func TestSetupBaselineCaptureCancellationWaitsForExactTerminalRun(t *testing.T) {
	head, correlation := strings.Repeat("a", 40), strings.Repeat("b", 64)
	reads, cancels := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/actions/runs/77":
			reads++
			status, conclusion := "in_progress", ""
			if cancels > 0 && reads >= 3 {
				status, conclusion = "completed", "cancelled"
			}
			_, _ = fmt.Fprintf(writer, `{"id":77,"display_title":%q,"path":%q,"event":"workflow_dispatch","head_sha":%q,"status":%q,"conclusion":%q,"repository":{"full_name":"owner/repo"}}`, workflowDispatchDisplayTitle(correlation), visualHiveProductionWorkflowPath, head, status, conclusion)
		case request.Method == http.MethodPost && request.URL.Path == "/repos/owner/repo/actions/runs/77/cancel":
			cancels++
			writer.WriteHeader(http.StatusAccepted)
		default:
			http.Error(writer, request.Method+" "+request.URL.Path, http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := hivegithub.NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	intent := SetupBaselineIntent{Repository: "owner/repo", CaptureRunID: 77, CaptureHeadSHA: head, CaptureCorrelation: correlation}
	if err := cancelSetupBaselineCaptureRunExactWithTiming(context.Background(), client, intent, time.Millisecond, time.Second); err != nil {
		t.Fatal(err)
	}
	if cancels != 1 || reads < 3 {
		t.Fatalf("cancellation did not wait for exact terminal run: cancels=%d reads=%d", cancels, reads)
	}
}

func TestSetupBaselineRebindDurablyTombstonesAmbiguousAcceptedDispatch(t *testing.T) {
	stateDir := t.TempDir()
	store, err := NewStore(filepath.Join(stateDir, "integrated"))
	if err != nil {
		t.Fatal(err)
	}
	now, head, correlation := time.Now().UTC(), strings.Repeat("a", 40), strings.Repeat("b", 64)
	emptyDigest := emptySetupBaselineCandidateDigest(t)
	config := Config{SchemaVersion: ConfigSchema, Repository: "owner/repo", RepositoryID: "123", DefaultBranch: "main", StateDir: stateDir,
		Coverage: CoverageComprehensive, Automation: AutomationRepairPR, Provider: "codex", SetupAuthorizationActorID: 42,
		SetupPRNumber: 7, SetupPRURL: "https://example.test/pull/7", SetupHeadSHA: head, VisualHiveConfigDigest: strings.Repeat("c", 64),
		SetupBaselineContractDigest: strings.Repeat("d", 64), SetupBaselineInitialDigest: emptyDigest}
	baseline := SetupBaselineIntent{SchemaVersion: SetupBaselineSchema, Phase: SetupBaselineDispatched, Repository: config.Repository, RepositoryID: config.RepositoryID,
		DefaultBranch: config.DefaultBranch, AuthorizerID: config.SetupAuthorizationActorID, SetupPRNumber: config.SetupPRNumber, SetupPRURL: config.SetupPRURL,
		SetupHeadSHA: head, VisualHiveConfigDigest: config.VisualHiveConfigDigest, ScreenshotContractDigest: config.SetupBaselineContractDigest,
		InitialBaselineDigest: emptyDigest, CaptureCorrelation: correlation, CaptureHeadSHA: head, DispatchAttemptedAt: now, CreatedAt: now, UpdatedAt: now}
	if err := store.SaveSetupBaselineIntent(baseline); err != nil {
		t.Fatal(err)
	}
	dispatch, err := newWorkflowDispatchIntentForOperation(config, "hive-visual-hive.yml", "main", setupBaselineWorkflowOperation, correlation, now)
	if err != nil {
		t.Fatal(err)
	}
	dispatch.DispatchAttemptedAt = now
	if err := store.SaveWorkflowDispatchIntent(dispatch); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if request.URL.Path == "/repos/owner/repo/actions/workflows/hive-visual-hive.yml/runs" {
			_, _ = io.WriteString(writer, `{"total_count":0,"workflow_runs":[]}`)
			return
		}
		http.Error(writer, request.Method+" "+request.URL.Path, http.StatusNotFound)
	}))
	defer server.Close()
	client := hivegithub.NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	changed := config
	changed.VisualHiveConfigDigest = strings.Repeat("e", 64)
	if err := reconcileActiveSetupBaselineReconfiguration(context.Background(), store, changed, head, false, client); err != nil {
		t.Fatal(err)
	}
	rebind, exists, err := store.LoadSetupBaselineRebindIntent()
	if err != nil || !exists || rebind.Phase != SetupBaselineRebindResourcesRetired || !containsExact(rebind.Source.RetiredCaptureCorrelations, correlation) {
		t.Fatalf("ambiguous dispatch lacks durable rebind tombstone: exists=%t rebind=%+v err=%v", exists, rebind, err)
	}
	if _, exists, err := store.LoadWorkflowDispatchIntent(); err != nil || exists {
		t.Fatalf("tombstoned ambiguous dispatch remained active: exists=%t err=%v", exists, err)
	}
}
