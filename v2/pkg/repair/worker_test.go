package repair

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kubestellar/hive/v2/pkg/automation"
	hivegithub "github.com/kubestellar/hive/v2/pkg/github"
	"github.com/kubestellar/hive/v2/pkg/visualhive"
)

type fakeProvider struct {
	runs           int
	requiredMarker string
}

func (p *fakeProvider) Name() string                 { return "test-model" }
func (p *fakeProvider) Health(context.Context) error { return nil }
func (p *fakeProvider) Run(_ context.Context, worktree, _ string) (ProviderResult, error) {
	if p.requiredMarker != "" {
		if _, err := os.Stat(p.requiredMarker); err != nil {
			return ProviderResult{}, fmt.Errorf("repair preparation marker is missing: %w", err)
		}
	}
	p.runs++
	value := "fixed\n"
	if p.runs > 1 {
		value = "fixed again\n"
	}
	return ProviderResult{Summary: "updated src/value.txt"}, os.WriteFile(filepath.Join(worktree, "src", "value.txt"), []byte(value), 0o600)
}

type noChangeThenFixProvider struct{ runs int }

func (p *noChangeThenFixProvider) Name() string                 { return "test-model" }
func (p *noChangeThenFixProvider) Health(context.Context) error { return nil }
func (p *noChangeThenFixProvider) Run(_ context.Context, worktree, prompt string) (ProviderResult, error) {
	p.runs++
	if p.runs == 1 {
		return ProviderResult{Summary: "I could not identify the concrete failure."}, nil
	}
	if !strings.Contains(prompt, "Prior bounded model response") || !strings.Contains(prompt, "key=playwright.console_error.deploy-preview-smoke") {
		return ProviderResult{}, fmt.Errorf("retry prompt did not include prior response and verified evidence")
	}
	return ProviderResult{Summary: "fixed from verified evidence"}, os.WriteFile(filepath.Join(worktree, "src", "value.txt"), []byte("fixed after retry\n"), 0o600)
}

type patchProvider struct{}

func (p *patchProvider) Name() string                 { return "patch-model" }
func (p *patchProvider) Health(context.Context) error { return nil }
func (p *patchProvider) Run(_ context.Context, _ string, prompt string) (ProviderResult, error) {
	if !strings.Contains(prompt, modelPatchBegin) || !strings.Contains(prompt, "intentionally read-only") {
		return ProviderResult{}, fmt.Errorf("repair prompt did not require the read-only patch contract")
	}
	output := `HIVE_PATCH_BEGIN
diff --git a/src/value.txt b/src/value.txt
--- a/src/value.txt
+++ b/src/value.txt
@@ -1 +1 @@
-broken
+fixed by model patch
HIVE_PATCH_END`
	return ProviderResult{Summary: "proposed bounded patch", Output: output}, nil
}

func TestRepairPreparationHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_REPAIR_PREPARATION_HELPER") != "1" {
		return
	}
	marker := os.Args[len(os.Args)-1]
	if err := os.WriteFile(marker, []byte("prepared\n"), 0o600); err != nil {
		os.Exit(2)
	}
	os.Exit(0)
}

func TestWorkerPreparesIsolatedWorktreeBeforeModel(t *testing.T) {
	repository, _ := seedGitRepository(t)
	state, err := NewStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "prepared")
	t.Setenv("GO_WANT_REPAIR_PREPARATION_HELPER", "1")
	provider := &fakeProvider{requiredMarker: marker}
	worker := &Worker{
		Config: Config{
			RepositoryDir: repository, WorktreeRoot: filepath.Join(t.TempDir(), "worktrees"), BaseBranch: "main",
			Policy:              automation.Policy{ACMMLevel: 5, Mode: automation.ModeRepairPR, AllowedRepositories: []string{"owner/repo"}, MaxRepairAttempts: 3},
			AllowedRepairPaths:  []string{"src/**"},
			PreparationCommands: []Command{{Name: os.Args[0], Args: []string{"-test.run=^TestRepairPreparationHelperProcess$", "--", marker}}},
			ValidationCommands:  []Command{{Name: "git", Args: []string{"diff", "--check"}}},
			ModelTimeout:        time.Minute, CommandTimeout: time.Minute,
		},
		Provider: provider, State: state, Lifecycle: &fakeLifecycle{}, GitHub: &fakePRClient{},
	}
	finding := visualhive.FindingLifecycle{
		Repository: "owner/repo", RepositoryFingerprint: "owner/repo:prepared", Fingerprint: "prepared",
		Status: visualhive.StatusIssueOpen, Title: "Repair prepared value", Body: "Value should be fixed.",
		IssueKind: "functional", Severity: "medium", OwningAgentHint: "quality", IssueNumber: 9, IssueURL: "https://example.test/issues/9",
	}

	if _, err := worker.Run(context.Background(), finding); err != nil {
		t.Fatal(err)
	}
	if provider.runs != 1 {
		t.Fatalf("provider runs = %d, want 1", provider.runs)
	}
}

type fakeLifecycle struct {
	branch, sha string
	pr          int
	starts      int
	decisions   []string
}

func (f *fakeLifecycle) MarkRepairStarted(_ string, branch string) error {
	f.branch = branch
	f.starts++
	return nil
}
func (f *fakeLifecycle) MarkPROpen(_ string, sha string, number int, _ string) error {
	f.sha, f.pr = sha, number
	return nil
}
func (f *fakeLifecycle) RecordAuthorization(_ string, action string, allowed bool, _ string) {
	f.decisions = append(f.decisions, fmt.Sprintf("%s:%t", action, allowed))
}

type fakePRClient struct{ calls int }

func (f *fakePRClient) UpsertRepairPullRequest(_ context.Context, _, _, _, _, _, _ string) (hivegithub.RepairPullRequest, error) {
	f.calls++
	return hivegithub.RepairPullRequest{Number: 17, URL: "https://example.test/pull/17"}, nil
}

func TestWorkerCreatesRealBranchCommitPushAndPRAndResumes(t *testing.T) {
	repository, remote := seedGitRepository(t)
	state, err := NewStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	provider := &fakeProvider{}
	lifecycle := &fakeLifecycle{}
	pulls := &fakePRClient{}
	worker := &Worker{
		Config: Config{
			RepositoryDir: repository, WorktreeRoot: filepath.Join(t.TempDir(), "worktrees"), BaseBranch: "main",
			Policy:             automation.Policy{ACMMLevel: 5, Mode: automation.ModeRepairPR, AllowedRepositories: []string{"owner/repo"}, MaxRepairAttempts: 3},
			AllowedRepairPaths: []string{"src/**"}, ValidationCommands: []Command{{Name: "git", Args: []string{"diff", "--check"}}},
			ModelTimeout: time.Minute, CommandTimeout: time.Minute,
		},
		Provider: provider, State: state, Lifecycle: lifecycle, GitHub: pulls,
	}
	finding := visualhive.FindingLifecycle{
		Repository: "owner/repo", RepositoryFingerprint: "owner/repo:stable-finding", Fingerprint: "stable-finding",
		Status: visualhive.StatusIssueOpen, Title: "Repair the value", Body: "Value should be fixed.", IssueKind: "functional",
		Severity: "medium", OwningAgentHint: "quality", IssueNumber: 9, IssueURL: "https://example.test/issues/9",
	}
	result, err := worker.Run(context.Background(), finding)
	if err != nil {
		attempt, _ := state.Get(finding.RepositoryFingerprint)
		data, readErr := os.ReadFile(filepath.Join(attempt.Worktree, "src", "value.txt"))
		t.Fatalf("%v; attempt=%+v content=%q readErr=%v", err, attempt, data, readErr)
	}
	if provider.runs != 1 || pulls.calls != 1 || result.PRNumber != 17 || result.CommitSHA == "" || lifecycle.sha != result.CommitSHA {
		t.Fatalf("unexpected result=%+v provider=%d pulls=%d lifecycle=%+v", result, provider.runs, pulls.calls, lifecycle)
	}
	remoteContent := gitOutput(t, remote, "show", result.Branch+":src/value.txt")
	if strings.TrimSpace(remoteContent) != "fixed" {
		t.Fatalf("remote branch was not pushed: %q", remoteContent)
	}
	resumed, err := worker.Run(context.Background(), finding)
	if err != nil {
		t.Fatal(err)
	}
	if !resumed.Resumed || provider.runs != 1 || pulls.calls != 1 {
		t.Fatalf("restart should reuse completed attempt: %+v runs=%d pulls=%d", resumed, provider.runs, pulls.calls)
	}
}

func TestWorkerDeniesModelAtLowerACMMBeforeRun(t *testing.T) {
	repository, _ := seedGitRepository(t)
	state, _ := NewStore(filepath.Join(t.TempDir(), "state"))
	provider := &fakeProvider{}
	lifecycle := &fakeLifecycle{}
	worker := &Worker{
		Config:   Config{RepositoryDir: repository, WorktreeRoot: filepath.Join(t.TempDir(), "worktrees"), BaseBranch: "main", Policy: automation.Policy{ACMMLevel: 2, Mode: automation.ModeRepairPR, AllowedRepositories: []string{"owner/repo"}}},
		Provider: provider, State: state, Lifecycle: lifecycle, GitHub: &fakePRClient{},
	}
	_, err := worker.Run(context.Background(), visualhive.FindingLifecycle{Repository: "owner/repo", RepositoryFingerprint: "fp", IssueNumber: 1, IssueURL: "https://example.test/1"})
	if err == nil || !strings.Contains(err.Error(), "denied") || provider.runs != 0 {
		t.Fatalf("expected pre-provider denial, got %v runs=%d", err, provider.runs)
	}
}

func TestWorkerRevisesTheSameBranchAndPullRequest(t *testing.T) {
	repository, _ := seedGitRepository(t)
	state, _ := NewStore(filepath.Join(t.TempDir(), "state"))
	provider := &fakeProvider{}
	lifecycle := &fakeLifecycle{}
	pulls := &fakePRClient{}
	worker := &Worker{
		Config: Config{
			RepositoryDir: repository, WorktreeRoot: filepath.Join(t.TempDir(), "worktrees"), BaseBranch: "main",
			Policy:             automation.Policy{ACMMLevel: 5, Mode: automation.ModeRepairPR, AllowedRepositories: []string{"owner/repo"}, MaxRepairAttempts: 3},
			AllowedRepairPaths: []string{"src/**"}, ValidationCommands: []Command{{Name: "git", Args: []string{"diff", "--check"}}},
			ModelTimeout: time.Minute, CommandTimeout: time.Minute,
		},
		Provider: provider, State: state, Lifecycle: lifecycle, GitHub: pulls,
	}
	finding := visualhive.FindingLifecycle{
		Repository: "owner/repo", RepositoryFingerprint: "owner/repo:revision", Status: visualhive.StatusIssueOpen,
		Title: "Repair the value", Body: "Value should be fixed.", IssueKind: "functional", Severity: "medium",
		OwningAgentHint: "quality", IssueNumber: 9, IssueURL: "https://example.test/issues/9",
	}
	first, err := worker.Run(context.Background(), finding)
	if err != nil {
		t.Fatal(err)
	}
	finding.Status, finding.RepairAttempts = visualhive.StatusNeedsRevision, 1
	second, err := worker.Run(context.Background(), finding)
	if err != nil {
		t.Fatal(err)
	}
	if first.Branch != second.Branch || first.PRNumber != second.PRNumber || pulls.calls != 2 || provider.runs != 2 {
		t.Fatalf("revision created duplicate lifecycle objects: first=%+v second=%+v pulls=%d runs=%d", first, second, pulls.calls, provider.runs)
	}
	attempt, _ := state.Get(finding.RepositoryFingerprint)
	if attempt.Attempt != 2 || attempt.Stage != StagePROpen {
		t.Fatalf("revision attempt was not persisted: %+v", attempt)
	}
}

func TestWorkerStartsFreshBranchAfterMergedFixNeedsRevision(t *testing.T) {
	repository, _ := seedGitRepository(t)
	state, _ := NewStore(filepath.Join(t.TempDir(), "state"))
	provider := &fakeProvider{}
	lifecycle := &fakeLifecycle{}
	pulls := &fakePRClient{}
	worker := &Worker{
		Config: Config{
			RepositoryDir: repository, WorktreeRoot: filepath.Join(t.TempDir(), "worktrees"), BaseBranch: "main",
			Policy:             automation.Policy{ACMMLevel: 5, Mode: automation.ModeRepairPR, AllowedRepositories: []string{"owner/repo"}, MaxRepairAttempts: 3},
			AllowedRepairPaths: []string{"src/**"}, ValidationCommands: []Command{{Name: "git", Args: []string{"diff", "--check"}}},
			ModelTimeout: time.Minute, CommandTimeout: time.Minute,
		},
		Provider: provider, State: state, Lifecycle: lifecycle, GitHub: pulls,
	}
	finding := visualhive.FindingLifecycle{
		Repository: "owner/repo", RepositoryFingerprint: "owner/repo:post-merge", Status: visualhive.StatusIssueOpen,
		Title: "Repair the value", Body: "Value should be fixed.", IssueKind: "functional", Severity: "medium",
		OwningAgentHint: "quality", IssueNumber: 9, IssueURL: "https://example.test/issues/9",
	}
	first, err := worker.Run(context.Background(), finding)
	if err != nil {
		t.Fatal(err)
	}
	finding.Status, finding.RepairAttempts, finding.MergeSHA = visualhive.StatusNeedsRevision, 1, "merged-first-fix"
	second, err := worker.Run(context.Background(), finding)
	if err != nil {
		t.Fatal(err)
	}
	if first.Branch == second.Branch || provider.runs != 2 || pulls.calls != 2 || lifecycle.starts != 2 {
		t.Fatalf("post-merge retry did not start a fresh bounded attempt: first=%+v second=%+v starts=%d pulls=%d runs=%d", first, second, lifecycle.starts, pulls.calls, provider.runs)
	}
	attempt, _ := state.Get(finding.RepositoryFingerprint)
	if attempt.Attempt != 2 || !attempt.LifecycleStarted || attempt.PriorModelSummary == "" {
		t.Fatalf("post-merge attempt did not retain retry context: %+v", attempt)
	}
}

func TestWorkerRetriesNoChangeCheckpointOnCleanNewAttempt(t *testing.T) {
	repository, _ := seedGitRepository(t)
	state, _ := NewStore(filepath.Join(t.TempDir(), "state"))
	provider := &noChangeThenFixProvider{}
	lifecycle := &fakeLifecycle{}
	pulls := &fakePRClient{}
	worker := &Worker{
		Config: Config{
			RepositoryDir: repository, WorktreeRoot: filepath.Join(t.TempDir(), "worktrees"), BaseBranch: "main",
			Policy:             automation.Policy{ACMMLevel: 5, Mode: automation.ModeRepairPR, AllowedRepositories: []string{"owner/repo"}, MaxRepairAttempts: 3},
			AllowedRepairPaths: []string{"src/**"}, ValidationCommands: []Command{{Name: "git", Args: []string{"diff", "--check"}}},
			EvidenceSummary: "- key=playwright.console_error.deploy-preview-smoke source=playwright kind=console_error status=failed contract=deploy-preview-smoke target=deployPreview reason=404",
			ModelTimeout:    time.Minute, CommandTimeout: time.Minute,
		},
		Provider: provider, State: state, Lifecycle: lifecycle, GitHub: pulls,
	}
	finding := visualhive.FindingLifecycle{
		Repository: "owner/repo", RepositoryFingerprint: "owner/repo:no-change", Status: visualhive.StatusIssueOpen,
		Title: "Repair deploy-preview-smoke: console_error", Body: "Evidence-backed failure.", IssueKind: "functional", Severity: "high",
		AffectedContracts: []string{"deploy-preview-smoke"}, OwningAgentHint: "quality", IssueNumber: 9, IssueURL: "https://example.test/issues/9",
	}
	if _, err := worker.Run(context.Background(), finding); err == nil || !strings.Contains(err.Error(), "without a source or test change") {
		t.Fatalf("expected bounded no-change failure, got %v", err)
	}
	first, _ := state.Get(finding.RepositoryFingerprint)
	if first.Stage != StageNoChange || first.ModelSummary == "" || first.Attempt != 1 {
		t.Fatalf("no-change checkpoint was not durable: %+v", first)
	}
	finding.Status, finding.RepairAttempts = visualhive.StatusRepairRunning, 1
	result, err := worker.Run(context.Background(), finding)
	if err != nil {
		t.Fatal(err)
	}
	second, _ := state.Get(finding.RepositoryFingerprint)
	if provider.runs != 2 || pulls.calls != 1 || second.Attempt != 2 || second.Stage != StagePROpen || second.Branch == first.Branch || result.PRNumber != 17 {
		t.Fatalf("retry did not produce exactly one PR on a clean new attempt: first=%+v second=%+v result=%+v runs=%d pulls=%d", first, second, result, provider.runs, pulls.calls)
	}
}

func TestWorkerAppliesAuthorizedReadOnlyModelPatch(t *testing.T) {
	repository, remote := seedGitRepository(t)
	state, _ := NewStore(filepath.Join(t.TempDir(), "state"))
	lifecycle := &fakeLifecycle{}
	worker := &Worker{
		Config: Config{
			RepositoryDir: repository, WorktreeRoot: filepath.Join(t.TempDir(), "worktrees"), BaseBranch: "main",
			Policy:             automation.Policy{ACMMLevel: 5, Mode: automation.ModeRepairPR, AllowedRepositories: []string{"owner/repo"}, MaxRepairAttempts: 3},
			AllowedRepairPaths: []string{"src/**"}, ValidationCommands: []Command{{Name: "git", Args: []string{"diff", "--check"}}},
			ModelTimeout: time.Minute, CommandTimeout: time.Minute,
		},
		Provider: &patchProvider{}, State: state, Lifecycle: lifecycle, GitHub: &fakePRClient{},
	}
	finding := visualhive.FindingLifecycle{
		Repository: "owner/repo", RepositoryFingerprint: "owner/repo:patch", Status: visualhive.StatusIssueOpen,
		Title: "Repair value", Body: "Value is broken.", IssueKind: "functional", Severity: "high",
		OwningAgentHint: "quality", IssueNumber: 9, IssueURL: "https://example.test/issues/9",
	}
	result, err := worker.Run(context.Background(), finding)
	if err != nil {
		t.Fatal(err)
	}
	if content := strings.TrimSpace(gitOutput(t, remote, "show", result.Branch+":src/value.txt")); content != "fixed by model patch" {
		t.Fatalf("authorized model patch was not committed and pushed: %q", content)
	}
	if !containsDecision(lifecycle.decisions, string(automation.ActionApplyPatch)+":true") {
		t.Fatalf("patch application authority was not audited: %v", lifecycle.decisions)
	}
}

func containsDecision(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func TestValidateChangedFilesRejectsSensitivePaths(t *testing.T) {
	for _, file := range []string{".github/workflows/test.yml", "visual-hive.baselines/a.png", "src/auth/token.ts", "deploy/app.yml"} {
		if err := validateChangedFiles([]string{file}, []string{"**"}); err == nil {
			t.Fatalf("expected %s to require review", file)
		}
	}
}

func TestTestAdequacyScopeIsCentrallyTestOnly(t *testing.T) {
	finding := visualhive.FindingLifecycle{IssueKind: "test_adequacy_gap"}
	if err := validateFindingScope(finding, []string{"tests/safe-default.test.js"}); err != nil {
		t.Fatalf("focused test file should be allowed: %v", err)
	}
	for _, files := range [][]string{{"src/App.tsx"}, {"package.json"}, {"visual-hive.config.yaml"}, {"tests/safe.test.js", "src/App.tsx"}} {
		if err := validateFindingScope(finding, files); err == nil {
			t.Fatalf("test adequacy scope allowed non-test files: %v", files)
		}
	}
	prompt := repairPrompt(finding, "verified evidence", "")
	if !strings.Contains(prompt, "test-adequacy repair") || !strings.Contains(prompt, "Change only focused files") {
		t.Fatalf("test-only constraint missing from repair prompt: %s", prompt)
	}
}

func TestCheckpointLocalValidationFailureCreatesBoundedRetryState(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	attempt := Attempt{
		RepositoryFingerprint: "owner/repo:test-gap", Stage: StageModelComplete,
		ModelSummary: "first patch", ModelPatch: "diff --git a/test/x.test.js b/test/x.test.js",
	}
	if err := checkpointLocalValidationFailure(store, &attempt, fmt.Errorf("expected 2, got 1")); err != nil {
		t.Fatal(err)
	}
	saved, ok := store.Get(attempt.RepositoryFingerprint)
	if !ok || saved.Stage != StageNoChange || saved.ModelPatch != "" || !strings.Contains(saved.ModelSummary, "expected 2, got 1") {
		t.Fatalf("local validation failure was not persisted for a bounded retry: %+v", saved)
	}
}

func seedGitRepository(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	runCommand(t, root, "git", "init", "--bare", remote)
	seed := filepath.Join(root, "seed")
	runCommand(t, root, "git", "init", "-b", "main", seed)
	if err := os.MkdirAll(filepath.Join(seed, "src"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(seed, "src", "value.txt"), []byte("broken\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runCommand(t, seed, "git", "add", ".")
	runCommand(t, seed, "git", "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "seed")
	runCommand(t, seed, "git", "remote", "add", "origin", remote)
	runCommand(t, seed, "git", "push", "-u", "origin", "main")
	clone := filepath.Join(root, "clone")
	runCommand(t, root, "git", "clone", "--branch", "main", remote, clone)
	return clone, remote
}

func runCommand(t *testing.T, dir, name string, args ...string) {
	t.Helper()
	command := exec.Command(name, args...)
	command.Dir = dir
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, output)
	}
}

func gitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = dir
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
	return string(output)
}
