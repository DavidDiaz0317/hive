package integrated

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInspectCheckoutBuildsRepositorySpecificSignals(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "package.json", `{"scripts":{"test":"vitest","typecheck":"tsc --noEmit"},"dependencies":{"react":"19.0.0"},"devDependencies":{"@playwright/test":"1.50.0"}}`)
	writeFixture(t, root, "package-lock.json", `{}`)
	writeFixture(t, root, ".github/workflows/ci.yml", "name: ci")
	writeFixture(t, root, "src/auth/Login.tsx", "export const Login = 1")
	writeFixture(t, root, "visual-hive.baselines/linux/home.png", "png")
	inspection, err := InspectCheckout(root, "main")
	if err != nil {
		t.Fatal(err)
	}
	if !contains(inspection.Languages, "TypeScript/JavaScript") || !contains(inspection.Frameworks, "React") || !contains(inspection.Frameworks, "Playwright") || !contains(inspection.PackageManagers, "npm") {
		t.Fatalf("missing detection signals: %+v", inspection)
	}
	if len(inspection.TestCommands) != 2 || len(inspection.CIFiles) != 1 || len(inspection.BaselineFiles) != 1 || len(inspection.HighRiskPaths) == 0 {
		t.Fatalf("unexpected inspection: %+v", inspection)
	}
}

func TestInspectCheckoutOrdersEvidenceProducersBeforeProofConsumers(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "package.json", `{"scripts":{"vh:test-creation":"node test-creation.js","vh:suite":"node suite.js","vh:mutation-proof":"node proof.js","vh:mutate":"node mutate.js","vh:run":"node run.js","vh:plan":"node plan.js","typecheck":"tsc --noEmit","build":"vite build"}}`)
	writeFixture(t, root, "package-lock.json", `{}`)

	inspection, err := InspectCheckout(root, "main")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"build", "typecheck", "vh:plan", "vh:run", "vh:mutate", "vh:mutation-proof", "vh:test-creation"}
	if len(inspection.TestCommands) != len(want) {
		t.Fatalf("unexpected commands: %+v", inspection.TestCommands)
	}
	for index, name := range want {
		command := inspection.TestCommands[index]
		if len(command) != 3 || command[2] != name {
			t.Fatalf("command %d = %v, want npm run %s", index, command, name)
		}
	}
}

func TestInspectCheckoutRetainsSuiteWhenItIsOnlyProducer(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "package.json", `{"scripts":{"build":"vite build","test:suite":"node suite.js"}}`)

	inspection, err := InspectCheckout(root, "main")
	if err != nil {
		t.Fatal(err)
	}
	if len(inspection.TestCommands) != 2 || inspection.TestCommands[1][2] != "test:suite" {
		t.Fatalf("standalone suite should be retained: %+v", inspection.TestCommands)
	}
}

func TestBuildSetupPlanKeepsCoverageAndAuthoritySeparate(t *testing.T) {
	plan := buildSetupPlan(SetupOptions{Repository: "owner/repo", Coverage: CoverageComprehensive, Automation: AutomationIssues, Provider: "codex", VisualHive: true, MaxActiveIssues: 5}, RepositoryInspection{DefaultBranch: "main"})
	if plan.Coverage != CoverageComprehensive || plan.Automation != AutomationIssues || plan.ACMMLevel != 4 {
		t.Fatalf("dimensions were conflated: %+v", plan)
	}
	if plan.MaxActiveIssues != 5 {
		t.Fatalf("active issue WIP limit was not preserved: %+v", plan)
	}
	if len(plan.TestingLayers) < 10 || !plan.ReadOnly {
		t.Fatalf("comprehensive read-only plan incomplete: %+v", plan)
	}
}

func TestComprehensiveSetupBootstrapsNodeTestWithoutWeakeningAutoMergePaths(t *testing.T) {
	inspection := RepositoryInspection{
		Languages:    []string{"TypeScript/JavaScript"},
		TestCommands: [][]string{{"npm", "run", "build"}, {"npm", "run", "vh:run"}},
	}
	commands := testCommandsForCoverage(inspection, CoverageComprehensive)
	if !hasCommand(commands, "node", "--test") {
		t.Fatalf("comprehensive Node setup must include the zero-test-safe built-in runner: %+v", commands)
	}
	if hasCommand(testCommandsForCoverage(inspection, CoverageStandard), "node", "--test") {
		t.Fatal("standard coverage must not add the comprehensive unit-test bootstrap")
	}
	if contains(defaultAllowedAutoMergePaths(), "package.json") {
		t.Fatal("test bootstrap must not broaden auto-merge to package metadata")
	}
}

func TestVisualTestConfigRequiresHumanMergeAuthority(t *testing.T) {
	if !contains(defaultAllowedRepairPaths(), "visual-hive.config.yaml") {
		t.Fatal("repair PR mode must be able to propose repository testing-plan improvements")
	}
	if contains(defaultAllowedAutoMergePaths(), "visual-hive.config.yaml") {
		t.Fatal("Visual Hive config changes must remain outside the default auto-merge allowlist")
	}
}

func TestExactCommitPinRejectsAbbreviatedOrDifferentRefs(t *testing.T) {
	sha := "0123456789012345678901234567890123456789"
	if !exactCommitPin(sha, sha) {
		t.Fatal("exact immutable commit should match")
	}
	if exactCommitPin(sha[:8], sha) || exactCommitPin(sha, "1123456789012345678901234567890123456789") {
		t.Fatal("abbreviated or different refs must not match")
	}
}

func TestWorkflowUsesTwoArtifactProvenanceAndPinnedActions(t *testing.T) {
	config := Config{DefaultBranch: "main", VisualHiveRepo: "owner/visual-hive", VisualHiveRef: "0123456789012345678901234567890123456789", ACMMLevel: 4, TestCommands: [][]string{{"node", "--test"}}}
	value := workflow(config)
	for _, required := range []string{checkoutActionSHA, setupNodeActionSHA, uploadArtifactActionSHA, "steps.evidence.outputs.artifact-id", "visual-hive-bundle-${{ github.run_id }}"} {
		if !containsString(value, required) {
			t.Fatalf("workflow missing %q", required)
		}
	}
	if !containsString(value, "pipeline-exit-code.txt") || !containsString(value, "set +e") {
		t.Fatal("workflow must publish evidence after a deterministic red verdict")
	}
	if strings.Count(value, "include-hidden-files: true") != 2 {
		t.Fatal("both hidden .visual-hive artifact uploads must opt in explicitly")
	}
	if !containsString(value, "hive integration-smoke") {
		t.Fatal("workflow must validate Hive import artifacts before finalizing a lifecycle bundle")
	}
	if !containsString(value, "playwright_cli") || !containsString(value, "install --with-deps chromium") || !containsString(value, "--skip-install") {
		t.Fatal("workflow must install the target Playwright browser revision once before the production scan")
	}
	if !containsString(value, "--acmm-request 4") {
		t.Fatal("workflow must bind bundle authority to Hive's configured ACMM level")
	}
	if !containsString(value, "node --test") {
		t.Fatal("production workflow must execute the repository unit-test bootstrap")
	}
	if containsString(value, "pull_request_target") || containsString(value, "issues: write") || containsString(value, "pull-requests: write") {
		t.Fatalf("workflow has an unsafe write lane:\n%s", value)
	}
}

func TestPullRequestWorkflowIsReadOnlyPinnedAndVerdictEnforcing(t *testing.T) {
	value := pullRequestWorkflow(Config{DefaultBranch: "main", VisualHiveRepo: "owner/visual-hive", VisualHiveRef: "0123456789012345678901234567890123456789", TestCommands: [][]string{{"node", "--test"}}})
	for _, required := range []string{checkoutActionSHA, setupNodeActionSHA, uploadArtifactActionSHA, "node --test", "visual-hive-pr", "pipeline-exit-code.txt", "Enforce deterministic verdict"} {
		if !containsString(value, required) {
			t.Fatalf("pull request workflow missing %q", required)
		}
	}
	if containsString(value, "pull_request_target") || containsString(value, "issues: write") || containsString(value, "pull-requests: write") {
		t.Fatalf("pull request workflow has an unsafe write lane:\n%s", value)
	}
}

func TestManagedRepositoryConfigExcludesLocalPaths(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, ".github/workflows/visual-hive-issue-lifecycle.yml", "issues: write")
	writeFixture(t, root, ".github/workflows/visual-hive-trusted-publisher.yml", "issues: write")
	config := Config{
		Repository: "owner/repo", DefaultBranch: "main", Coverage: CoverageStandard, Automation: AutomationIssues,
		Provider: "codex", ACMMLevel: 4, VisualHive: true, VisualHiveRepo: "owner/visual-hive",
		VisualHiveRef: "0123456789012345678901234567890123456789", CheckoutDir: `C:\private\checkout`, StateDir: `C:\private\state`,
	}
	if err := writeManagedFiles(root, config, RepositoryInspection{}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, ".hive", "integrated.json"))
	if err != nil {
		t.Fatal(err)
	}
	value := string(data)
	if strings.Contains(value, "private") || strings.Contains(value, "checkout_dir") || strings.Contains(value, "state_dir") {
		t.Fatalf("local paths leaked into repository config: %s", value)
	}
	for _, relative := range []string{".github/workflows/visual-hive-issue-lifecycle.yml", ".github/workflows/visual-hive-trusted-publisher.yml"} {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(relative))); !os.IsNotExist(err) {
			t.Fatalf("standalone writer %s must be removed in integrated mode", relative)
		}
	}
	prWorkflow, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "visual-hive-pr.yml"))
	if err != nil || !strings.Contains(string(prWorkflow), config.VisualHiveRef) {
		t.Fatalf("managed pull request workflow is missing or not pinned: %v", err)
	}
}

func writeFixture(t *testing.T, root, relative, content string) {
	t.Helper()
	target := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func containsString(value, target string) bool {
	return len(target) > 0 && strings.Contains(value, target)
}

func hasCommand(commands [][]string, want ...string) bool {
	for _, command := range commands {
		if strings.Join(command, "\x00") == strings.Join(want, "\x00") {
			return true
		}
	}
	return false
}
