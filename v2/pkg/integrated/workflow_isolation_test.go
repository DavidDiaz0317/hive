package integrated

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	hivegithub "github.com/kubestellar/hive/v2/pkg/github"
	"gopkg.in/yaml.v3"
)

type isolatedWorkflowDocument struct {
	Jobs map[string]isolatedWorkflowJob `yaml:"jobs"`
}

type isolatedWorkflowJob struct {
	Name  string            `yaml:"name"`
	If    string            `yaml:"if"`
	Needs workflowNeedsList `yaml:"needs"`
	Steps []struct {
		Name string         `yaml:"name"`
		If   string         `yaml:"if"`
		Uses string         `yaml:"uses"`
		Run  string         `yaml:"run"`
		With map[string]any `yaml:"with"`
	} `yaml:"steps"`
}

func TestGeneratedSetupBaselineJobInventoryMatchesVerifierContract(t *testing.T) {
	document := parseIsolatedWorkflow(t, workflow(isolationWorkflowConfig()))
	capture, captureExists := document.Jobs[setupBaselineCaptureJobID]
	verifier, verifierExists := document.Jobs[setupBaselineVerifyJobID]
	if !captureExists || !verifierExists {
		t.Fatalf("generated workflow is missing baseline inventory IDs: capture=%t verifier=%t", captureExists, verifierExists)
	}
	if capture.Name != hivegithub.SetupBaselineCaptureJobDisplayName || verifier.Name != hivegithub.SetupBaselineVerifyJobDisplayName {
		t.Fatalf("generated display names diverge from verifier allowlist: capture=%q verifier=%q", capture.Name, verifier.Name)
	}
	if !strings.Contains(capture.If, "inputs.hive_operation == 'setup-baseline-capture'") ||
		!strings.Contains(verifier.If, "inputs.hive_operation == 'setup-baseline-capture'") || !contains(verifier.Needs, setupBaselineCaptureJobID) {
		t.Fatalf("generated baseline inventory topology is not the exact two-job lane: capture=%+v verifier=%+v", capture, verifier)
	}
}

type workflowNeedsList []string

func (value *workflowNeedsList) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		*value = workflowNeedsList{node.Value}
		return nil
	case yaml.SequenceNode:
		var decoded []string
		if err := node.Decode(&decoded); err != nil {
			return err
		}
		*value = decoded
		return nil
	default:
		return node.Decode((*[]string)(value))
	}
}

func isolationWorkflowConfig() Config {
	return Config{
		RepositoryID: "123", DefaultBranch: "main", SetupBranch: "hive/setup-123", SetupAuthorizationActorID: 456,
		VisualHive: true, VisualHiveRepo: "owner/visual-hive", VisualHiveRef: strings.Repeat("a", 40), ACMMLevel: 4,
		TestCommands: [][]string{{"node", "--test"}, {"python", "-m", "pytest", "-q"}},
	}
}

func TestGeneratedWorkflowsUseCanonicalYAMLWhitespace(t *testing.T) {
	config := isolationWorkflowConfig()
	workflows := map[string]string{
		"production":   workflow(config),
		"pull_request": pullRequestWorkflow(config),
	}
	for name, value := range workflows {
		if !strings.HasSuffix(value, "\n") || strings.HasSuffix(value, "\n\n") {
			t.Fatalf("%s workflow must have exactly one terminal newline", name)
		}
		if strings.Contains(value, "\n\n\n") {
			t.Fatalf("%s workflow contains a non-canonical repeated blank line", name)
		}
		for lineNumber, line := range strings.Split(value, "\n") {
			if strings.TrimRight(line, " \t") != line {
				t.Fatalf("%s workflow line %d contains trailing whitespace", name, lineNumber+1)
			}
		}
	}
	if !strings.Contains(workflows["pull_request"], `HIVE_UNINSTALL_REQUIRED_FILES: "[]"`) ||
		strings.Contains(workflows["pull_request"], `HIVE_UNINSTALL_REQUIRED_FILES: '[]'`) {
		t.Fatal("pull-request workflow does not use the canonical empty JSON array scalar")
	}
}

func TestIsolatedDependencyInstallPinsTheTargetWorkingDirectory(t *testing.T) {
	shell := isolatedTargetDependencyShell(false)
	if !strings.Contains(shell, `test "$(pwd -P)" = "$HIVE_TARGET_WORKSPACE"`) {
		t.Fatal("isolated dependency installation does not verify its inherited runner-owned workspace")
	}
	if strings.Contains(shell, `cd "$HIVE_TARGET_WORKSPACE"`) {
		t.Fatal("isolated dependency installation must not re-traverse non-public hosted-runner ancestors")
	}
	account := prepareIsolatedTargetAccountShell()
	for _, required := range []string{
		`target_workspace="/opt/hive-target/workspace"`,
		`sudo mount --bind "$GITHUB_WORKSPACE" "$target_workspace"`,
		`test "$(stat -Lc '%d:%i' "$target_workspace")" = "$(stat -Lc '%d:%i' "$GITHUB_WORKSPACE")"`,
		`echo "HIVE_TARGET_WORKSPACE=$target_workspace" >> "$GITHUB_ENV"`,
	} {
		if !strings.Contains(account, required) {
			t.Fatalf("isolated account setup does not bind the exact checkout through a root-controlled path: missing %q", required)
		}
	}
	if strings.Contains(account, `chmod o+x "$RUNNER_WORKSPACE"`) {
		t.Fatal("isolated account setup widens traversal on the hosted runner workspace")
	}
	for _, prefix := range []string{isolatedTargetEnvPrefix(), isolatedVisualTargetEnvPrefix()} {
		if !strings.Contains(prefix, `env -i -C "$HIVE_TARGET_WORKSPACE"`) {
			t.Fatalf("isolated target environment does not enter the root-controlled bind mount: %s", prefix)
		}
		if !strings.Contains(prefix, `HIVE_TARGET_WORKSPACE="$HIVE_TARGET_WORKSPACE"`) {
			t.Fatalf("isolated target environment does not bind the runner-owned workspace: %s", prefix)
		}
		if strings.Contains(prefix, `-C "$GITHUB_WORKSPACE"`) {
			t.Fatalf("isolated target environment re-enters the runner-private checkout path: %s", prefix)
		}
	}
	if !strings.Contains(verifyImmutableTargetCheckoutShell(), `sudo umount "$target_workspace"`) {
		t.Fatal("repository target cleanup does not unmount the isolated checkout")
	}
	review := runnerOwnedEvidenceRebuildShell(true)
	for _, required := range []string{
		`review_workspace="/opt/hive-target/workspace"`,
		`sudo mount --bind "$runner_workspace" "$review_workspace"`,
		`cd "$review_workspace"`,
		`sudo umount "$review_workspace"`,
	} {
		if !strings.Contains(review, required) {
			t.Fatalf("runner-owned review does not preserve the target evidence root: missing %q", required)
		}
	}
}

func TestGeneratedWorkflowsIsolateTargetProcessesFromLifecycleAuthority(t *testing.T) {
	config := isolationWorkflowConfig()
	production := workflow(config)
	pullRequest := pullRequestWorkflow(config)
	productionDocument := parseIsolatedWorkflow(t, production)
	pullDocument := parseIsolatedWorkflow(t, pullRequest)

	for _, name := range []string{"repository-test-001", "repository-test-002", visualExecutionJobName, "visual-hive-production", "visual-hive"} {
		if _, exists := productionDocument.Jobs[name]; !exists {
			t.Fatalf("production workflow is missing isolated job %q", name)
		}
	}
	productionExecution := productionDocument.Jobs[visualExecutionJobName]
	productionAggregator := productionDocument.Jobs["visual-hive-production"]
	executionText := workflowJobText(productionExecution)
	aggregatorText := workflowJobText(productionAggregator)
	for _, required := range []string{
		"sudo -u hive-target -- env -i", "sudo chown -R root:root .git", "sudo chmod -R a-w .git",
		"sudo chown root:root visual-hive.config.yaml", "sudo chmod 0444 visual-hive.config.yaml",
		"git diff --no-ext-diff --no-textconv --exit-code -- .", "HIVE_VISUAL_HIVE_CLI_SHA", "test ! -w \"$VISUAL_HIVE_CLI\"",
		"HIVE_TRUSTED_NODE_SHA", `"$HIVE_TRUSTED_NODE" "$VISUAL_HIVE_CLI" pipeline`, `sudo -u hive-target -- test ! -w "$HIVE_TRUSTED_NODE"`,
		"visual-hive-raw-${{ github.run_id }}",
	} {
		if !strings.Contains(executionText, required) {
			t.Fatalf("isolated target execution is missing %q", required)
		}
	}
	for _, forbidden := range []string{"visual-hive-evidence-${{ github.run_id }}", "visual-hive-bundle-${{ github.run_id }}", "--authoritative-for-resolution"} {
		if strings.Contains(executionText, forbidden) {
			t.Fatalf("target execution can emit lifecycle-authority output %q", forbidden)
		}
	}
	for _, required := range []string{
		"actions/download-artifact@" + downloadArtifactActionSHA, "Rebuild exact immutable Visual Hive CLI on fresh runner",
		"visual-hive-evidence-${{ github.run_id }}", "visual-hive-bundle-${{ github.run_id }}", "--authoritative-for-resolution",
		"ACTIONS_ID_TOKEN_REQUEST_TOKEN", "Raw evidence contains a symbolic link", "Verify isolated runner prerequisites before trusted publication",
		"isolated Visual Hive execution did not complete successfully",
	} {
		if !strings.Contains(aggregatorText, required) {
			t.Fatalf("runner-owned production aggregator is missing %q", required)
		}
	}
	for _, forbidden := range []string{"sudo -u hive-target", "npm --prefix \"$(dirname \"$lockfile\")\" ci", "target_visual_pipeline"} {
		if strings.Contains(aggregatorText, forbidden) {
			t.Fatalf("runner-owned production aggregator executes target code %q", forbidden)
		}
	}
	for _, need := range []string{visualExecutionJobName, "repository-test-001", "repository-test-002"} {
		if !contains(productionAggregator.Needs, need) {
			t.Fatalf("production aggregator does not need isolated job %q: %v", need, productionAggregator.Needs)
		}
	}

	for _, name := range []string{"repository-test-001", "repository-test-002", visualExecutionJobName, "visual-hive"} {
		job, exists := pullDocument.Jobs[name]
		if !exists {
			t.Fatalf("pull-request workflow is missing isolated job %q", name)
		}
		if name != "visual-hive" && !strings.Contains(job.If, "github.event_name == 'pull_request'") {
			t.Fatalf("target job %q is not explicitly pull_request-only: %q", name, job.If)
		}
	}
	pullAggregator := pullDocument.Jobs["visual-hive"]
	if !strings.Contains(pullAggregator.If, "github.event_name == 'pull_request'") || strings.Contains(pullAggregator.If, "pull_request_target") || strings.Contains(pullAggregator.If, "operation == 'uninstall'") {
		t.Fatalf("protected aggregator must be pull_request-only: %q", pullAggregator.If)
	}
	for _, step := range pullAggregator.Steps {
		if step.Name == "Enforce deterministic verdict" || strings.TrimSpace(step.Run) == "" && strings.TrimSpace(step.Uses) == "" {
			continue
		}
		if (step.Uses != "" || step.Run != "") && !strings.Contains(step.If, "operation != 'uninstall'") {
			t.Fatalf("aggregator step %q could execute proposed code during pull_request_target: if=%q", step.Name, step.If)
		}
	}
	if strings.Count(pullRequest, "visual-hive-pr-raw-${{ github.run_id }}") != 2 || strings.Count(pullRequest, "name: visual-hive-pr\n") != 1 {
		t.Fatal("PR raw and final artifacts are not separated into one producer/consumer boundary")
	}
}

func TestTrustedCollectorNodeCannotBeShadowedByTargetPath(t *testing.T) {
	bash := workflowBash(t)
	root := t.TempDir()
	shadow := filepath.Join(root, "shadow")
	if err := os.MkdirAll(shadow, 0o755); err != nil {
		t.Fatal(err)
	}
	trustedNode := filepath.Join(root, "trusted-node")
	writeFixture(t, root, "trusted-node", "#!/usr/bin/env bash\nprintf 'trusted\\n'\n")
	writeFixture(t, shadow, "node", "#!/usr/bin/env bash\nprintf 'shadow\\n'\n")
	if err := os.Chmod(trustedNode, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(shadow, "node"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Invoke the captured absolute runtime directly. Generated workflow shell
	// syntax is checked separately by TestGeneratedIsolationShellsAreExecutable;
	// avoiding `bash -c` here also keeps the assertion portable through Git for
	// Windows' command-line quoting layer.
	command := exec.Command(bash, bashFilesystemPath(trustedNode))
	command.Env = append(os.Environ(), "PATH="+bashFilesystemPath(shadow)+":/usr/bin:/bin")
	output, err := command.CombinedOutput()
	if err != nil || strings.TrimSpace(string(output)) != "trusted" {
		t.Fatalf("target PATH shadow replaced the absolute trusted node: err=%v output=%q", err, output)
	}
}

func TestTrustedCollectorUsesSealedPinnedAndTargetPlaywrightBrowsers(t *testing.T) {
	for name, value := range map[string]string{"production": workflow(isolationWorkflowConfig()), "pull-request": pullRequestWorkflow(isolationWorkflowConfig())} {
		execution := workflowJobText(parseIsolatedWorkflow(t, value).Jobs[visualExecutionJobName])
		for _, invariant := range []string{
			`trusted_browser_path="/opt/hive-target/trusted/playwright-${GITHUB_RUN_ID}"`,
			`target_browser_staging="/opt/hive-target/playwright-staging-${GITHUB_RUN_ID}"`,
			`sudo env PLAYWRIGHT_BROWSERS_PATH="$trusted_browser_path" "$HIVE_TRUSTED_NODE" "$tooling_playwright" install --with-deps chromium`,
			`PLAYWRIGHT_BROWSERS_PATH="$target_browser_staging"`,
			`sudo find "$target_browser_staging" -type l -print -quit`,
			`sudo find "$target_browser_staging" ! -type d ! -type f -print -quit`,
			`sudo du -sb "$target_browser_staging"`,
			`sudo cp -a --no-clobber "$target_browser_staging"/. "$trusted_browser_path"/`,
			`sudo chown -R root:root "$trusted_browser_path"`,
			`sudo find "$trusted_browser_path" -type d -exec chmod a+rx,a-w {} +`,
			`sudo find "$trusted_browser_path" -type f -exec chmod a+r,a-w {} +`,
			`Sealed Playwright browser root is not readable and traversable by the isolated target account`,
			`Sealed Playwright browser root remains writable by the isolated target account`,
			`trusted_tooling="/opt/hive-target/trusted/visual-hive-tooling"`,
			`HIVE_TRUSTED_BROWSER_MANIFEST=$trusted_browser_manifest`,
			`Target Playwright runtime lacks a sealed executable binding`,
			`runtime.chromium.launch({ headless: true })`,
			`Target Playwright runtime could not launch its exact sealed headless browser`,
			`Trusted Playwright browser executable is missing before Visual Hive execution`,
			`PLAYWRIGHT_BROWSERS_PATH="$HIVE_TRUSTED_PLAYWRIGHT_BROWSERS_PATH"`,
			`test "$(sha256sum "$HIVE_TRUSTED_BROWSER_EXECUTABLE" | cut -d ' ' -f 1)" = "$HIVE_TRUSTED_BROWSER_SHA"`,
			`sudo -u hive-target -- test ! -w "$HIVE_TRUSTED_BROWSER_EXECUTABLE"`,
			`sudo rm -rf -- "$expected_browser_path"`,
			`test ! -e "$expected_browser_path"`,
		} {
			if !strings.Contains(execution, invariant) {
				t.Fatalf("%s workflow lost sealed browser invariant %q", name, invariant)
			}
		}
		if strings.Contains(execution, `PLAYWRIGHT_BROWSERS_PATH=/home/hive-target/.cache/ms-playwright HIVE_TARGET_PROCESS=1 "$HIVE_TRUSTED_NODE"`) {
			t.Fatalf("%s Visual collector still accepts the target-owned browser cache", name)
		}
		if !strings.Contains(value, "permissions:\n      contents: read\n      actions: read") || strings.Contains(execution, "contents: write") || strings.Contains(execution, "id-token: write") {
			t.Fatalf("%s browser provisioning changed the read-only target-execution permissions", name)
		}
		if strings.Contains(execution, `mv .hive-visual-tooling "$RUNNER_TEMP/visual-hive-tooling"`) ||
			strings.Contains(execution, `trusted_browser_path="$RUNNER_TEMP/hive-playwright`) ||
			strings.Contains(execution, `sudo chmod o+x "$RUNNER_TEMP"`) {
			t.Fatalf("%s target-readable sealed tooling still traverses the runner-private temp root", name)
		}
		install := strings.Index(value, `sudo env PLAYWRIGHT_BROWSERS_PATH="$trusted_browser_path" "$HIVE_TRUSTED_NODE" "$tooling_playwright" install --with-deps chromium`)
		targetInstall := strings.Index(value, `PLAYWRIGHT_BROWSERS_PATH="$target_browser_staging"`)
		seal := strings.Index(value, `sudo find "$trusted_browser_path" -type d -exec chmod a+rx,a-w {} +`)
		preflight := strings.Index(value, "name: Verify sealed Playwright browser handoff")
		collection := strings.Index(value, "name: Run target-facing Visual Hive collection")
		cleanup := strings.LastIndex(value, `sudo rm -rf -- "$expected_browser_path"`)
		if install < 0 || targetInstall < install || seal < targetInstall || preflight < seal || collection < preflight || cleanup < collection {
			t.Fatalf("%s workflow browser install/seal/preflight/execute/cleanup order is unsafe: install=%d target=%d seal=%d preflight=%d collection=%d cleanup=%d", name, install, targetInstall, seal, preflight, collection, cleanup)
		}
	}
	pullRequestExecution := workflowJobText(parseIsolatedWorkflow(t, pullRequestWorkflow(isolationWorkflowConfig())).Jobs["visual-hive"])
	if !strings.Contains(pullRequestExecution, `const report = JSON.parse(fs.readFileSync(".visual-hive/report.json", "utf8"))`) ||
		!strings.Contains(pullRequestExecution, `["failed", "blocked"].includes(report.status) && verdictSummary.visualHiveVerdict === "blocked"`) {
		t.Fatal("pull-request verifier does not accept Visual Hive's blocked missing-baseline status")
	}
	for _, stale := range []string{"pipeline.summary", "pipeline.verdictSummary", "pipeline.verdictContributions", "pipeline.results"} {
		if strings.Contains(pullRequestExecution, stale) {
			t.Fatalf("pull-request verifier still reads %s from the pipeline envelope", stale)
		}
	}
	dependencyShell := isolatedTargetDependencyShell(true)
	install := strings.Index(dependencyShell, `sudo env PLAYWRIGHT_BROWSERS_PATH="$trusted_browser_path" "$HIVE_TRUSTED_NODE" "$tooling_playwright" install --with-deps chromium`)
	targetDependencies := strings.Index(dependencyShell, "HIVE_TARGET_DEPENDENCIES")
	if install < 0 || targetDependencies < 0 || install > targetDependencies {
		t.Fatal("trusted browser is installed only after target dependency code")
	}
	if strings.Contains(dependencyShell, isolatedVisualTargetEnvPrefix()+` bash`) {
		t.Fatal("target dependency code was granted the final trusted browser directory")
	}
}

func TestGeneratedIsolationShellsAreExecutable(t *testing.T) {
	bash := workflowBash(t)
	for name, value := range map[string]string{"production": workflow(isolationWorkflowConfig()), "pull-request": pullRequestWorkflow(isolationWorkflowConfig())} {
		document := parseIsolatedWorkflow(t, value)
		for jobName, job := range document.Jobs {
			for _, step := range job.Steps {
				if strings.TrimSpace(step.Run) == "" {
					continue
				}
				command := exec.Command(bash, "-n")
				command.Stdin = strings.NewReader(step.Run)
				if output, err := command.CombinedOutput(); err != nil {
					lines := strings.Split(step.Run, "\n")
					for index := range lines {
						lines[index] = fmt.Sprintf("%4d: %s", index+1, lines[index])
					}
					t.Fatalf("%s job %s step %q has invalid shell: %v\n%s\n%s", name, jobName, step.Name, err, output, strings.Join(lines, "\n"))
				}
			}
		}
	}
}

func TestRawTargetEvidenceIsBoundedBeforeArtifactUpload(t *testing.T) {
	for name, value := range map[string]string{"production": workflow(isolationWorkflowConfig()), "pull-request": pullRequestWorkflow(isolationWorkflowConfig())} {
		if !strings.Contains(value, "Raw target evidence contains a symbolic link") {
			t.Fatalf("%s workflow does not reject target symlinks before upload", name)
		}
		for _, invariant := range []string{
			`test "$(find .visual-hive -type f | wc -l)" -le 5000`,
			`test "$(du -sb .visual-hive | cut -f 1)" -le 1073741824`,
		} {
			if !strings.Contains(value, invariant) {
				t.Fatalf("%s workflow does not bound raw evidence before upload: %q", name, invariant)
			}
		}
		for _, invariant := range []string{`artifact_files="$(find .visual-hive -type f | wc -l)"`, `test "$artifact_files" -le 5000`, `test "$artifact_bytes" -le 1073741824`} {
			if !strings.Contains(value, invariant) {
				t.Fatalf("%s workflow does not recheck downloaded raw evidence: %q", name, invariant)
			}
		}
	}
}

func TestProductionPrerequisiteRequiresExactTopologyAndSuccessfulVisualExecution(t *testing.T) {
	bash := workflowBash(t)
	run := func(needs string) error {
		command := exec.Command(bash, "-e", "-o", "pipefail", "-c", productionPrerequisiteShell())
		command.Env = append(os.Environ(), `HIVE_EXPECTED_REPOSITORY_JOBS=["repository-test-001"]`, "HIVE_NEEDS_JSON="+needs)
		return command.Run()
	}
	if err := run(`{"repository-test-001":{"result":"failure"},"visual-hive-execution":{"result":"success"}}`); err != nil {
		t.Fatalf("terminal repository failure with successful isolated Visual execution was rejected: %v", err)
	}
	for _, invalid := range []string{
		`{"repository-test-001":{"result":"success"},"visual-hive-execution":{"result":"failure"}}`,
		`{"repository-test-001":{"result":"success"}}`,
		`{"repository-test-001":{"result":"success"},"repository-test-002":{"result":"success"},"visual-hive-execution":{"result":"success"}}`,
		`{"repository-test-001":{"result":"skipped"},"visual-hive-execution":{"result":"success"}}`,
	} {
		if err := run(invalid); err == nil {
			t.Fatalf("invalid production prerequisite topology/outcome was accepted: %s", invalid)
		}
	}
}

func TestPullRequestEnforcementUsesOnlyRunnerOwnedJobTopology(t *testing.T) {
	bash := workflowBash(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".visual-hive"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(root, ".visual-hive"), "pipeline.json", `{"status":"passed","exitCode":0}`)
	writeFixture(t, filepath.Join(root, ".visual-hive"), "report.json", `{"status":"passed","summary":{},"results":[]}`)
	writeFixture(t, filepath.Join(root, ".visual-hive"), "verdict.json", `{"summary":{"visualHiveVerdict":"passed"},"gatingContributions":[]}`)
	baseEnv := append(os.Environ(),
		`HIVE_EXPECTED_REPOSITORY_JOBS=["repository-test-001"]`,
		`HIVE_NEEDS_JSON={"visual-hive-execution":{"result":"success"},"repository-test-001":{"result":"success"}}`,
		"HIVE_TRUSTED_REBUILD_OUTCOME=success",
		"HIVE_SETUP_OPERATION=setup", "HIVE_SETUP_AUTHORIZED=false",
	)
	run := func(environment []string) error {
		command := exec.Command(bash, "-e", "-o", "pipefail", "-c", pullRequestEnforcementShell())
		command.Dir = root
		command.Env = environment
		return command.Run()
	}
	if err := run(baseEnv); err != nil {
		t.Fatalf("valid runner topology was rejected: %v", err)
	}
	for _, invalid := range []string{
		`HIVE_NEEDS_JSON={"visual-hive-execution":{"result":"failure"},"repository-test-001":{"result":"success"}}`,
		`HIVE_NEEDS_JSON={"visual-hive-execution":{"result":"success"},"repository-test-001":{"result":"success"},"repository-test-002":{"result":"success"}}`,
		`HIVE_NEEDS_JSON={"visual-hive-execution":{"result":"success"}}`,
	} {
		environment := replaceEnvironment(baseEnv, "HIVE_NEEDS_JSON", invalid)
		if err := run(environment); err == nil {
			t.Fatalf("invalid runner-owned topology was accepted: %s", invalid)
		}
	}
	setupEnvironment := replaceEnvironment(baseEnv, "HIVE_NEEDS_JSON", `HIVE_NEEDS_JSON={"visual-hive-execution":{"result":"success"},"repository-test-001":{"result":"failure"}}`)
	setupEnvironment = replaceEnvironment(setupEnvironment, "HIVE_SETUP_AUTHORIZED", "HIVE_SETUP_AUTHORIZED=true")
	setupEnvironment = append(setupEnvironment, "HIVE_REPOSITORY=owner/repo", "HIVE_HEAD_SHA="+strings.Repeat("a", 40), "HIVE_SETUP_CONTEXT=context", "HIVE_SETUP_BINDING_DIGEST=binding", "HIVE_SETUP_DIFF_DIGEST=diff")
	if err := run(setupEnvironment); err != nil {
		t.Fatalf("exact setup exception rejected runner-owned repository failure: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".visual-hive", "setup-bootstrap-repository-failure.json")); err != nil {
		t.Fatalf("setup exception proof missing: %v", err)
	}
}

func TestPullRequestEnforcementTreatsDerivedReadinessAsMissingBaselineOnly(t *testing.T) {
	bash := workflowBash(t)
	root := t.TempDir()
	visualDir := filepath.Join(root, ".visual-hive")
	if err := os.MkdirAll(visualDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, visualDir, "pipeline.json", `{
  "schemaVersion":"visual-hive.pipeline.v1","status":"failed","exitCode":1,
  "steps":[],"artifacts":{}
}`)
	writeFixture(t, visualDir, "report.json", `{
  "schemaVersion":"visual-hive.report.v1","status":"failed",
  "summary":{"missingBaselines":1,"visualDiffs":0,"consoleErrors":0,"pageErrors":0,"flowStepsFailed":0},
  "results":[{"screenshotAssertions":[{"status":"missing_baseline","contractId":"app-shell","screenshotName":"desktop","baselinePath":"snapshots/app.png","actualPath":"artifacts/app.png"}]}]
}`)
	writeFixture(t, visualDir, "verdict.json", `{
  "schemaVersion":"visual-hive.verdict.v1",
  "summary":{"visualHiveVerdict":"blocked","failedBecause":[]},
  "gatingContributions":[
    {"kind":"deterministic_run","status":"blocked","gating":true},
    {"kind":"contract_result","status":"blocked","gating":true},
    {"kind":"missing_baseline","status":"blocked","gating":true},
    {"kind":"readiness_gate","status":"blocked","gating":true}
  ]
}`)
	writeFixture(t, visualDir, "readiness.json", `{"status":"blocked","gates":[{"id":"deterministic:status","status":"blocked"},{"id":"baselines:missing-baseline","status":"blocked"},{"id":"mutation:missing","status":"warning"}]}`)
	environment := append(os.Environ(),
		`HIVE_EXPECTED_REPOSITORY_JOBS=["repository-test-001"]`,
		`HIVE_NEEDS_JSON={"visual-hive-execution":{"result":"success"},"repository-test-001":{"result":"success"}}`,
		"HIVE_TRUSTED_REBUILD_OUTCOME=failure", "HIVE_SETUP_OPERATION=setup", "HIVE_SETUP_AUTHORIZED=true",
		"HIVE_REPOSITORY=owner/repo", "HIVE_HEAD_SHA="+strings.Repeat("a", 40),
		"HIVE_SETUP_CONTEXT=context", "HIVE_SETUP_BINDING_DIGEST=binding", "HIVE_SETUP_DIFF_DIGEST=diff",
	)
	run := func() error {
		command := exec.Command(bash, "-e", "-o", "pipefail", "-c", pullRequestEnforcementShell())
		command.Dir = root
		command.Env = environment
		return command.Run()
	}
	if err := run(); err != nil {
		t.Fatalf("derived missing-baseline readiness did not enter the supported setup handoff: %v", err)
	}
	if _, err := os.Stat(filepath.Join(visualDir, "setup-baseline-required.json")); err != nil {
		t.Fatalf("setup baseline handoff proof missing: %v", err)
	}
	writeFixture(t, visualDir, "readiness.json", `{"status":"blocked","gates":[{"id":"deterministic:status","status":"blocked"},{"id":"baselines:missing-baseline","status":"blocked"},{"id":"security:posture","status":"blocked"}]}`)
	if err := run(); err == nil {
		t.Fatal("independent readiness blocker was misclassified as missing-baseline-only")
	}
}

func parseIsolatedWorkflow(t *testing.T, value string) isolatedWorkflowDocument {
	t.Helper()
	var document isolatedWorkflowDocument
	if err := yaml.Unmarshal([]byte(value), &document); err != nil {
		t.Fatalf("generated workflow is invalid YAML: %v\n%s", err, value)
	}
	return document
}

func workflowJobText(job isolatedWorkflowJob) string {
	var result strings.Builder
	result.WriteString(job.If)
	for _, step := range job.Steps {
		result.WriteString("\n" + step.Name + "\n" + step.If + "\n" + step.Uses + "\n" + step.Run)
		for name, value := range step.With {
			result.WriteString("\n" + name + "=" + strings.TrimSpace(fmt.Sprint(value)))
		}
	}
	return result.String()
}

func replaceEnvironment(environment []string, name, replacement string) []string {
	result := append([]string(nil), environment...)
	prefix := name + "="
	for index, value := range result {
		if strings.HasPrefix(value, prefix) {
			result[index] = replacement
			return result
		}
	}
	return append(result, replacement)
}
