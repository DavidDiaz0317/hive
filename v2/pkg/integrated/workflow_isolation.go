package integrated

import (
	"encoding/json"
	"fmt"
	"strings"

	hivegithub "github.com/kubestellar/hive/v2/pkg/github"
)

const (
	downloadArtifactActionSHA = "3e5f45b2cfb9172054b4087a40e8e0b5a5461e7c" // actions/download-artifact v8.0.1
	visualExecutionJobName    = "visual-hive-execution"
	isolatedTargetAccount     = "hive-target"
	isolatedTargetRoot        = "/opt/hive-target"
	isolatedTargetWorkspace   = isolatedTargetRoot + "/workspace"
	isolatedTrustedRoot       = isolatedTargetRoot + "/trusted"
	isolatedTrustedTooling    = isolatedTrustedRoot + "/visual-hive-tooling"
	setupBaselineCaptureJobID = "setup-baseline-capture"
	setupBaselineVerifyJobID  = "setup-baseline-verify"
)

func workflow(config Config) string {
	return isolatedWorkflow(config)
}

func pullRequestWorkflow(config Config) string {
	return isolatedPullRequestWorkflow(config)
}

// isolatedWorkflow renders the production workflow as three independent trust
// zones: one job per repository test, an unprivileged target-execution job,
// and a fresh runner-owned verifier/bundle job. Only the final verifier emits
// artifacts that Hive may consume for issue or repair lifecycle authority.
func isolatedWorkflow(config Config) string {
	repositoryJobs, repositoryNeeds := isolatedRepositoryTestWorkflowJobs(config, "", "inputs.hive_operation == 'production'")
	productionNeeds := appendWorkflowNeed(repositoryNeeds, visualExecutionJobName)
	productionNeeds = strings.Replace(productionNeeds, "    if: ${{ always() }}", "    if: ${{ always() && inputs.hive_operation == 'production' }}", 1)
	jobs := joinWorkflowJobs(
		repositoryJobs,
		isolatedVisualExecutionWorkflowJob(config, false, "inputs.hive_operation == 'production'"),
		productionAggregatorWorkflowJob(config, productionNeeds),
		isolatedSetupBaselineCaptureWorkflowJobs(config),
	)
	return fmt.Sprintf(`name: Hive Visual Hive Production

on:
  workflow_dispatch:
    inputs:
      hive_dispatch_id:
        description: Opaque durable Hive dispatch correlation
        required: true
        type: string
      hive_operation:
        description: Exact Hive workflow lane
        required: true
        default: production
        type: choice
        options:
          - production
          - setup-baseline-capture

run-name: "Hive Visual Hive Production [${{ inputs.hive_dispatch_id }}]"

permissions:
  contents: read
  actions: read

concurrency:
  group: hive-visual-hive-production
  cancel-in-progress: false

jobs:
%s

  # GitHub allows an App-bound context to become required only after that
  # context has completed successfully in the repository within seven days.
  # This job executes no target code and fails actively on stale dispatches.
  visual-hive:
    if: ${{ inputs.hive_operation == 'production' }}
    runs-on: ubuntu-latest
    timeout-minutes: 10
    steps:
      - name: Reject a non-default production dispatch
        shell: bash
        env:
          HIVE_EVENT_NAME: ${{ github.event_name }}
          HIVE_REF: ${{ github.ref }}
          HIVE_DEFAULT_BRANCH: ${{ github.event.repository.default_branch }}
        run: |
          set -euo pipefail
          test "$HIVE_EVENT_NAME" = "workflow_dispatch"
          test "$HIVE_REF" = "refs/heads/$HIVE_DEFAULT_BRANCH"
      - uses: actions/checkout@%s
        with:
          ref: ${{ github.event.repository.default_branch }}
          fetch-depth: 1
          persist-credentials: false
      - name: Verify dispatch SHA is the current default head
        shell: bash
        env:
          HIVE_DISPATCH_SHA: ${{ github.sha }}
        run: |
          set -euo pipefail
          test "$(git rev-parse HEAD)" = "$HIVE_DISPATCH_SHA"
`, jobs, checkoutActionSHA)
}

// isolatedPullRequestWorkflow keeps the protected visual-hive context on a
// fresh verifier job. The target job has read-only GitHub permissions and runs
// every dependency/script/browser process as a separate unprivileged account.
func isolatedPullRequestWorkflow(config Config) string {
	setupAuthorizationJob := setupAuthorizationWorkflowJob(config)
	uninstallCheckJob := uninstallCheckPublisherWorkflowJob()
	repositoryJobs, repositoryNeeds := isolatedRepositoryTestWorkflowJobs(config, "setup-authorization", "github.event_name == 'pull_request' && needs.setup-authorization.outputs.operation != 'uninstall'")
	aggregatorNeeds := appendWorkflowNeeds(repositoryNeeds, "setup-authorization", visualExecutionJobName)
	aggregatorNeeds = strings.Replace(aggregatorNeeds, "    if: ${{ always() }}", "    if: ${{ always() && github.event_name == 'pull_request' }}", 1)
	jobs := joinWorkflowJobs(
		setupAuthorizationJob,
		uninstallCheckJob,
		repositoryJobs,
		isolatedVisualExecutionWorkflowJob(config, true),
		pullRequestAggregatorWorkflowJob(config, aggregatorNeeds),
	)
	return fmt.Sprintf(`name: Visual Hive PR

on:
  pull_request:
  # Base-branch-only uninstall publication. No proposed checkout, dependency,
  # test, or Visual Hive process is ever executed by this event.
  pull_request_target:
    types: [opened, synchronize, reopened]

permissions: {}

concurrency:
  group: visual-hive-pr-${{ github.event_name }}-${{ github.event.pull_request.number }}
  cancel-in-progress: true

jobs:
%s
`, jobs)
}

func isolatedRepositoryTestWorkflowJobs(config Config, prerequisite, condition string) (string, string) {
	var jobs strings.Builder
	jobNames := make([]string, 0, len(config.TestCommands))
	dependencyInstall := isolatedTargetDependencyShell(false)
	checkoutRef := "${{ github.sha }}"
	if prerequisite != "" {
		checkoutRef = "${{ github.event.pull_request.head.sha }}"
	}
	jobIndex := 0
	for _, command := range config.TestCommands {
		if len(command) == 0 {
			continue
		}
		jobIndex++
		jobID := fmt.Sprintf("repository-test-%03d", jobIndex)
		jobNames = append(jobNames, jobID)
		quoted := make([]string, 0, len(command))
		for _, argument := range command {
			quoted = append(quoted, shellQuote(argument))
		}
		gate := ""
		if prerequisite != "" {
			gate = fmt.Sprintf("    needs: %s\n", prerequisite)
		}
		if condition != "" {
			gate += fmt.Sprintf("    if: ${{ %s }}\n", condition)
		}
		fmt.Fprintf(&jobs, `  %s:
    name: Hive repository test %03d
%s    permissions:
      contents: read
    runs-on: ubuntu-latest
    timeout-minutes: 30
    steps:
      - uses: actions/checkout@%s
        with:
          ref: %s
          fetch-depth: 1
          persist-credentials: false
      - uses: actions/setup-node@%s
        with:
          node-version: 22.23.1
      - uses: actions/setup-python@%s
        with:
          python-version: "3.11"
      - name: Prepare isolated target account
        shell: bash
        run: |
%s
      - name: Install repository test dependencies
        shell: bash
        run: |
%s
      - name: Stop dependency processes and verify exact tracked source
        shell: bash
        env:
          HIVE_TARGET_HEAD_SHA: %s
        run: |
%s
      - name: Execute exact repository test command
        shell: bash
        run: |
          set -euo pipefail
          %s %s
      - name: Terminate isolated target processes and reverify source
        if: always()
        shell: bash
        env:
          HIVE_TARGET_HEAD_SHA: %s
        run: |
%s

`, jobID, jobIndex, gate, checkoutActionSHA, checkoutRef, setupNodeActionSHA, setupPythonActionSHA,
			indentWorkflowShell(prepareIsolatedTargetAccountShell(), 10),
			indentWorkflowShell(dependencyInstall, 10), checkoutRef, indentWorkflowShell(verifyAndSealTargetCheckoutShell(), 10),
			isolatedTargetEnvPrefix(), strings.Join(quoted, " "), checkoutRef, indentWorkflowShell(verifyImmutableTargetCheckoutShell(), 10))
	}
	return strings.TrimSuffix(jobs.String(), "\n"), workflowNeeds(jobNames)
}

func isolatedVisualExecutionWorkflowJob(config Config, pullRequest bool, condition ...string) string {
	gate := ""
	checkoutRef := "${{ github.sha }}"
	rawArtifact := "visual-hive-raw-${{ github.run_id }}"
	modeArgs := "--mode full --ci --continue-on-error --skip-install"
	captureScope := ""
	finalEnforcement := `          if [[ "$HIVE_TARGET_PIPELINE_OUTCOME" != "success" && "$HIVE_TARGET_PIPELINE_OUTCOME" != "failure" ]]; then
            echo "Visual pipeline did not reach a terminal runner-owned outcome" >&2
            exit 1
          fi`
	if pullRequest {
		gate = "    needs: setup-authorization\n    if: ${{ github.event_name == 'pull_request' && needs.setup-authorization.outputs.operation != 'uninstall' }}\n"
		checkoutRef = "${{ github.event.pull_request.head.sha }}"
		rawArtifact = "visual-hive-pr-raw-${{ github.run_id }}"
		modeArgs = "--mode pr --changed-files \"$HIVE_CHANGED_FILES\" --ci --continue-on-error --skip-install"
		captureScope = `      - name: Capture immutable pull request scope
        shell: bash
        env:
          HIVE_BASE_SHA: ${{ github.event.pull_request.base.sha }}
          HIVE_HEAD_SHA: ${{ github.event.pull_request.head.sha }}
        run: |
          set -euo pipefail
          input_dir="/tmp/hive-visual-input-${{ github.run_id }}"
          sudo install -d -o root -g root -m 0755 "$input_dir"
          git diff --no-ext-diff --no-textconv --no-renames --name-only "$HIVE_BASE_SHA" "$HIVE_HEAD_SHA" > "$RUNNER_TEMP/changed-files.pr.txt"
          sudo install -o root -g root -m 0444 "$RUNNER_TEMP/changed-files.pr.txt" "$input_dir/changed-files.pr.txt"
          echo "HIVE_CHANGED_FILES=$input_dir/changed-files.pr.txt" >> "$GITHUB_ENV"
`
	} else if len(condition) > 0 && strings.TrimSpace(condition[0]) != "" {
		gate = fmt.Sprintf("    if: ${{ %s }}\n", strings.TrimSpace(condition[0]))
	}
	return fmt.Sprintf(`  %s:
    name: %s
%s    permissions:
      contents: read
      actions: read
    runs-on: ubuntu-latest
    timeout-minutes: 45
    steps:
      - uses: actions/checkout@%s
        with:
          ref: %s
          fetch-depth: 0
          persist-credentials: false
      - uses: actions/checkout@%s
        with:
          repository: %s
          ref: %s
          path: .hive-visual-tooling
          persist-credentials: false
      - uses: actions/setup-node@%s
        with:
          node-version: 22.23.1
          cache: npm
          cache-dependency-path: .hive-visual-tooling/package-lock.json
      - uses: actions/setup-python@%s
        with:
          python-version: "3.11"
      - name: Build exact Visual Hive tooling before target execution
        working-directory: .hive-visual-tooling
        shell: bash
        env:
          HIVE_VISUAL_HIVE_REF: %s
        run: |
          set -euo pipefail
          test "$(git rev-parse HEAD)" = "$HIVE_VISUAL_HIVE_REF"
          npm ci
          npm run build
          test -z "$(git status --porcelain --untracked-files=no)"
      - name: Seal immutable tooling and prepare isolated target account
        shell: bash
        run: |
          set -euo pipefail
%s
          trusted_tooling=%q
          sudo mv .hive-visual-tooling "$trusted_tooling"
          cli="$trusted_tooling/packages/cli/dist/index.js"
          test -f "$cli"
          cli_sha="$(sha256sum "$cli" | cut -d ' ' -f 1)"
          trusted_node="$(readlink -f "$(command -v node)")"
          tool_cache="$(readlink -f "$RUNNER_TOOL_CACHE")"
          node_runtime="$(dirname "$(dirname "$trusted_node")")"
          test -x "$trusted_node"
          case "$node_runtime" in
            "$tool_cache"/node/*) ;;
            *) echo "setup-node resolved outside the GitHub-hosted tool cache" >&2; exit 1 ;;
          esac
          sudo chown -R root:root "$node_runtime"
          sudo chmod -R a-w "$node_runtime"
          trusted_node_sha="$(sha256sum "$trusted_node" | cut -d ' ' -f 1)"
          echo "VISUAL_HIVE_CLI=$cli" >> "$GITHUB_ENV"
          echo "HIVE_VISUAL_HIVE_CLI_SHA=$cli_sha" >> "$GITHUB_ENV"
          echo "HIVE_TRUSTED_NODE=$trusted_node" >> "$GITHUB_ENV"
          echo "HIVE_TRUSTED_NODE_SHA=$trusted_node_sha" >> "$GITHUB_ENV"
          sudo chown -R root:root "$trusted_tooling"
          sudo chmod -R a-w "$trusted_tooling"
%s      - name: Install target dependencies in isolated account
        shell: bash
        run: |
%s
      - name: Stop dependency processes and verify exact tracked source
        shell: bash
        env:
          HIVE_TARGET_HEAD_SHA: %s
        run: |
%s
      - name: Verify sealed Playwright browser handoff
        shell: bash
        run: |
%s
      - name: Run target-facing Visual Hive collection
        id: target_visual_pipeline
        continue-on-error: true
        shell: bash
        run: |
          set +e
          %s "$HIVE_TRUSTED_NODE" "$VISUAL_HIVE_CLI" pipeline --config visual-hive.config.yaml %s
          pipeline_exit=$?
          set -e
          printf '%%s\n' "$pipeline_exit" > .visual-hive/pipeline-exit-code.txt
          exit "$pipeline_exit"
      - name: Terminate target account and reverify immutable tooling
        if: always()
        shell: bash
        env:
          HIVE_TARGET_PIPELINE_OUTCOME: ${{ steps.target_visual_pipeline.outcome }}
          HIVE_TARGET_PIPELINE_CONCLUSION: ${{ steps.target_visual_pipeline.conclusion }}
        run: |
          set -euo pipefail
          target_workspace="${HIVE_TARGET_WORKSPACE:-}"
          expected_browser_path="`+isolatedTrustedRoot+`/playwright-${GITHUB_RUN_ID}"
          expected_browser_manifest="$RUNNER_TEMP/hive-trusted-playwright-${GITHUB_RUN_ID}.tsv"
          cleanup_trusted_browser() {
            sudo rm -rf -- "$expected_browser_path"
            rm -f -- "$expected_browser_manifest"
          }
          cleanup_target_workspace() {
            if [ -n "$target_workspace" ] && mountpoint -q "$target_workspace"; then
              sudo umount "$target_workspace" || true
            fi
            cleanup_trusted_browser
          }
          trap cleanup_target_workspace EXIT
          test -n "$target_workspace"
          sudo pkill -KILL -u %s 2>/dev/null || true
          test "$(sha256sum "$VISUAL_HIVE_CLI" | cut -d ' ' -f 1)" = "$HIVE_VISUAL_HIVE_CLI_SHA"
          test ! -w "$VISUAL_HIVE_CLI"
          test "$(sha256sum "$HIVE_TRUSTED_NODE" | cut -d ' ' -f 1)" = "$HIVE_TRUSTED_NODE_SHA"
          sudo -u %s -- test ! -w "$HIVE_TRUSTED_NODE"
%s
          if [ -n "${HIVE_TRUSTED_BROWSER_EXECUTABLE:-}" ] && [ -n "${HIVE_TRUSTED_PLAYWRIGHT_BROWSERS_PATH:-}" ] && [ -n "${HIVE_TRUSTED_BROWSER_SHA:-}" ]; then
            case "$HIVE_TRUSTED_BROWSER_EXECUTABLE" in
              "$HIVE_TRUSTED_PLAYWRIGHT_BROWSERS_PATH"/*) ;;
              *) echo "Pinned browser escaped its sealed root" >&2; exit 1 ;;
            esac
            test -x "$HIVE_TRUSTED_BROWSER_EXECUTABLE"
            test "$(sha256sum "$HIVE_TRUSTED_BROWSER_EXECUTABLE" | cut -d ' ' -f 1)" = "$HIVE_TRUSTED_BROWSER_SHA"
            sudo -u %s -- test ! -w "$HIVE_TRUSTED_BROWSER_EXECUTABLE"
          else
            echo "Pinned browser preparation did not complete" >&2
          fi
          test "$(git rev-parse HEAD)" = "%s"
          git diff --no-ext-diff --no-textconv --exit-code -- .
          if find .visual-hive -type l -print -quit | grep -q .; then
            echo "Raw target evidence contains a symbolic link" >&2
            exit 1
          fi
          test "$(find .visual-hive -type f | wc -l)" -le 5000
          test "$(du -sb .visual-hive | cut -f 1)" -le 1073741824
          sudo umount "$target_workspace"
          cleanup_trusted_browser
          trap - EXIT
          if mountpoint -q "$target_workspace"; then
            echo "Isolated target workspace remained mounted" >&2
            exit 1
          fi
          test ! -e "$expected_browser_path"
          test ! -e "$expected_browser_manifest"
          runner_outcome="$RUNNER_TEMP/hive-visual-runner-outcome-${GITHUB_RUN_ID}.json"
          RUNNER_OUTCOME_PATH="$runner_outcome" node <<'NODE'
          const fs = require("fs");
          const outcome = process.env.HIVE_TARGET_PIPELINE_OUTCOME;
          const conclusion = process.env.HIVE_TARGET_PIPELINE_CONCLUSION;
          if (!["success", "failure"].includes(outcome) || !["success", "failure"].includes(conclusion)) {
            throw new Error("Visual pipeline lacks a terminal runner-owned outcome receipt");
          }
          fs.writeFileSync(process.env.RUNNER_OUTCOME_PATH, JSON.stringify({
            schemaVersion: "hive.visual-runner-outcome.v1",
            outcome,
            conclusion
          }) + "\n", { flag: "wx", mode: 0o600 });
          NODE
          sudo rm -f -- .visual-hive/hive-runner-outcome.json
          sudo install -o root -g root -m 0444 "$runner_outcome" .visual-hive/hive-runner-outcome.json
          rm -f -- "$runner_outcome"
          test -f .visual-hive/hive-runner-outcome.json
          test ! -L .visual-hive/hive-runner-outcome.json
          test ! -w .visual-hive/hive-runner-outcome.json
      - name: Upload isolated raw Visual evidence
        if: always()
        uses: actions/upload-artifact@%s
        with:
          name: %s
          path: .visual-hive
          if-no-files-found: error
          include-hidden-files: true
          retention-days: 14
      - name: Confirm target execution runner outcome
        if: always()
        shell: bash
        env:
          HIVE_TARGET_PIPELINE_OUTCOME: ${{ steps.target_visual_pipeline.outcome }}
        run: |
          set -euo pipefail
%s
`, visualExecutionJobName, visualExecutionJobName, gate, checkoutActionSHA, checkoutRef, checkoutActionSHA,
		config.VisualHiveRepo, config.VisualHiveRef, setupNodeActionSHA, setupPythonActionSHA, config.VisualHiveRef,
		indentWorkflowShell(prepareIsolatedTargetAccountShell(), 10), isolatedTrustedTooling,
		captureScope, indentWorkflowShell(isolatedTargetDependencyShell(true), 10), checkoutRef, indentWorkflowShell(verifyAndSealTargetCheckoutShell(), 10),
		indentWorkflowShell(trustedBrowserHandoffVerificationShell(), 10), isolatedVisualTargetEnvPrefix(), modeArgs, isolatedTargetAccount, isolatedTargetAccount,
		indentWorkflowShell(trustedBrowserHandoffVerificationShell(), 10), isolatedTargetAccount, checkoutRef, uploadArtifactActionSHA, rawArtifact,
		indentWorkflowShell(finalEnforcement, 10))
}

func isolatedSetupBaselineCaptureWorkflowJobs(config Config) string {
	target := isolatedVisualExecutionWorkflowJob(config, false, "inputs.hive_operation == 'setup-baseline-capture'")
	target = strings.ReplaceAll(target, visualExecutionJobName, setupBaselineCaptureJobID)
	target = strings.Replace(target, "name: "+setupBaselineCaptureJobID, "name: "+hivegithub.SetupBaselineCaptureJobDisplayName, 1)
	target = strings.Replace(target, "--mode full --ci --continue-on-error --skip-install", "--mode full --ci --continue-on-error --skip-install --bootstrap-baselines", 1)
	target = strings.ReplaceAll(target, "visual-hive-raw-${{ github.run_id }}", "setup-baseline-raw-${{ github.run_id }}")
	verifier := fmt.Sprintf(`  %s:
    name: %s
    needs: %s
    if: ${{ always() && inputs.hive_operation == 'setup-baseline-capture' && needs.%s.result == 'success' }}
    permissions:
      contents: read
      actions: read
    runs-on: ubuntu-latest
    timeout-minutes: 20
    steps:
%s
      - name: Download isolated baseline capture
        uses: actions/download-artifact@%s
        with:
          name: setup-baseline-raw-${{ github.run_id }}
          path: .visual-hive
      - name: Validate exact Linux baseline candidate set
        shell: bash
        env:
          HIVE_EVENT_NAME: ${{ github.event_name }}
          HIVE_REPOSITORY: ${{ github.repository }}
          HIVE_REPOSITORY_ID: ${{ github.event.repository.id }}
          HIVE_CAPTURE_HEAD: ${{ github.sha }}
          HIVE_CAPTURE_REF: ${{ github.ref }}
          HIVE_DEFAULT_BRANCH: ${{ github.event.repository.default_branch }}
          HIVE_CAPTURE_CORRELATION: ${{ inputs.hive_dispatch_id }}
          HIVE_WORKFLOW_RUN_ID: ${{ github.run_id }}
          HIVE_WORKFLOW_NAME: Hive Visual Hive Production
          HIVE_WORKFLOW_PATH: .github/workflows/hive-visual-hive.yml
        run: |
          set -euo pipefail
          test "$HIVE_EVENT_NAME" = "workflow_dispatch"
          test "$HIVE_CAPTURE_REF" = "refs/heads/$HIVE_DEFAULT_BRANCH"
          test "$(git rev-parse HEAD)" = "$HIVE_CAPTURE_HEAD"
          test -f "$VISUAL_HIVE_CLI"
          test ! -w "$VISUAL_HIVE_CLI"
          node <<'NODE'
          const crypto = require("crypto");
          const fs = require("fs");
          const path = require("path");
          const correlation = String(process.env.HIVE_CAPTURE_CORRELATION || "").toLowerCase();
          const head = String(process.env.HIVE_CAPTURE_HEAD || "").toLowerCase();
          const repository = String(process.env.HIVE_REPOSITORY || "");
          const repositoryID = String(process.env.HIVE_REPOSITORY_ID || "");
          const runID = String(process.env.HIVE_WORKFLOW_RUN_ID || "");
          if (!/^[a-f0-9]{64}$/u.test(correlation) || !/^[a-f0-9]{40}$/u.test(head) ||
              !/^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/u.test(repository) || !/^[1-9][0-9]*$/u.test(repositoryID) || !/^[1-9][0-9]*$/u.test(runID)) {
            throw new Error("baseline capture immutable identity is invalid");
          }
          const sourceRoot = path.resolve(".visual-hive/snapshots");
          const outputRoot = path.resolve(process.env.RUNNER_TEMP, "setup-baseline-artifact");
          if (!fs.existsSync(sourceRoot) || !fs.lstatSync(sourceRoot).isDirectory() || fs.lstatSync(sourceRoot).isSymbolicLink()) {
            throw new Error("baseline capture produced no regular snapshots directory");
          }
          const records = [];
          const visit = (directory) => {
            for (const name of fs.readdirSync(directory).sort()) {
              const absolute = path.join(directory, name);
              const stat = fs.lstatSync(absolute);
              if (stat.isSymbolicLink()) throw new Error("baseline candidate contains a symbolic link");
              if (stat.isDirectory()) { visit(absolute); continue; }
              if (!stat.isFile()) throw new Error("baseline candidate is not a regular file");
              const relativeSource = path.relative(process.cwd(), absolute).split(path.sep).join("/");
              if (!/^\.visual-hive\/snapshots\/(?:[^/]+\/)*[^/]+\.png$/u.test(relativeSource) || relativeSource.includes("..")) {
                throw new Error("unexpected baseline candidate path " + relativeSource);
              }
              if (stat.size <= 8 || stat.size > 20 * 1024 * 1024) throw new Error("baseline candidate has invalid size");
              const content = fs.readFileSync(absolute);
              if (!content.subarray(0, 8).equals(Buffer.from([137,80,78,71,13,10,26,10]))) throw new Error("baseline candidate is not PNG");
              const destination = path.join(outputRoot, ...relativeSource.split("/"));
              fs.mkdirSync(path.dirname(destination), { recursive: true });
              fs.copyFileSync(absolute, destination, fs.constants.COPYFILE_EXCL);
              records.push({ path: relativeSource, sha256: crypto.createHash("sha256").update(content).digest("hex"), bytes: stat.size });
            }
          };
          visit(sourceRoot);
          records.sort((a, b) => a.path.localeCompare(b.path));
          const totalBytes = records.reduce((sum, item) => sum + item.bytes, 0);
          if (records.length < 1 || records.length > 200 || totalBytes > 500 * 1024 * 1024) throw new Error("baseline candidate set is empty or exceeds bounds");
          const candidateDigest = crypto.createHash("sha256").update(JSON.stringify(records)).digest("hex");
          const manifest = {
            schema_version: "hive.setup-baseline-artifact.v1", repository, repository_id: repositoryID,
            capture_correlation: correlation, capture_head: head, workflow_run_id: runID,
            workflow_name: process.env.HIVE_WORKFLOW_NAME, workflow_path: process.env.HIVE_WORKFLOW_PATH,
            event: "workflow_dispatch", platform: "linux", runner: "ubuntu-latest",
            candidate_digest: candidateDigest, file_count: records.length, total_bytes: totalBytes, files: records
          };
          fs.mkdirSync(outputRoot, { recursive: true });
          fs.writeFileSync(path.join(outputRoot, "setup-baseline-manifest.json"), JSON.stringify(manifest, null, 2) + "\n", { flag: "wx" });
          NODE
      - name: Upload correlation-bound setup baseline artifact
        uses: actions/upload-artifact@%s
        with:
          name: hive-setup-baselines-${{ inputs.hive_dispatch_id }}
          path: ${{ runner.temp }}/setup-baseline-artifact
          if-no-files-found: error
          include-hidden-files: true
          retention-days: 14
`, setupBaselineVerifyJobID, hivegithub.SetupBaselineVerifyJobDisplayName, setupBaselineCaptureJobID, setupBaselineCaptureJobID,
		runnerOwnedVerifierSetupSteps(config, "${{ github.sha }}", ""), downloadArtifactActionSHA, uploadArtifactActionSHA)
	return target + "\n" + verifier
}

func productionAggregatorWorkflowJob(config Config, needs string) string {
	expectedJSON, _ := json.Marshal(isolatedRepositoryJobIDs(config))
	return fmt.Sprintf(`  visual-hive-production:
%s
    permissions:
      contents: read
      actions: read
    runs-on: ubuntu-latest
    timeout-minutes: 30
    steps:
      - name: Verify isolated runner prerequisites before trusted publication
        shell: bash
        env:
          HIVE_NEEDS_JSON: ${{ toJSON(needs) }}
          HIVE_EXPECTED_REPOSITORY_JOBS: '%s'
        run: |
%s
%s
      - name: Download isolated raw Visual evidence
        uses: actions/download-artifact@%s
        with:
          name: visual-hive-raw-${{ github.run_id }}
          path: .visual-hive
      - name: Rebuild and validate runner-owned lifecycle evidence
        shell: bash
        run: |
%s
      - name: Upload independently verifiable evidence
        id: evidence
        uses: actions/upload-artifact@%s
        with:
          name: visual-hive-evidence-${{ github.run_id }}
          path: |
            .visual-hive
            !.visual-hive/bundles/**
          if-no-files-found: error
          include-hidden-files: true
          retention-days: 14
      - name: Build Hive lifecycle bundle bound to evidence artifact
        shell: bash
        env:
          VISUAL_HIVE_WORKFLOW_ARTIFACT_ID: ${{ steps.evidence.outputs.artifact-id }}
        run: |
          set -euo pipefail
          args=()
          while IFS= read -r contract; do
            if [ -n "$contract" ]; then args+=(--evaluated-contract "$contract"); fi
          done < .visual-hive/evaluated-contracts.txt
          authority_args=()
          if [ "$(tr -d '\r\n' < .visual-hive/authoritative-resolution.txt)" = "true" ]; then
            authority_args+=(--authoritative-for-resolution)
          fi
          env -u GITHUB_TOKEN -u GH_TOKEN -u GITHUB_OUTPUT -u GITHUB_ENV -u GITHUB_PATH -u GITHUB_STEP_SUMMARY -u ACTIONS_ID_TOKEN_REQUEST_TOKEN -u ACTIONS_ID_TOKEN_REQUEST_URL node "$VISUAL_HIVE_CLI" hive bundle --config visual-hive.config.yaml --issues .visual-hive/issues.json --acmm-request %d --scan-scope full "${authority_args[@]}" "${args[@]}"
      - name: Upload trusted Hive bundle
        id: bundle
        uses: actions/upload-artifact@%s
        with:
          name: visual-hive-bundle-${{ github.run_id }}
          path: .visual-hive/bundles
          if-no-files-found: error
          include-hidden-files: true
          retention-days: 14
`, needs, string(expectedJSON), indentWorkflowShell(productionPrerequisiteShell(), 10), runnerOwnedVerifierSetupSteps(config, "${{ github.sha }}", ""), downloadArtifactActionSHA,
		indentWorkflowShell(runnerOwnedEvidenceRebuildShell(false), 10), uploadArtifactActionSHA, config.ACMMLevel, uploadArtifactActionSHA)
}

func pullRequestAggregatorWorkflowJob(config Config, needs string) string {
	expectedJobs := isolatedRepositoryJobIDs(config)
	expectedJSON, _ := json.Marshal(expectedJobs)
	return fmt.Sprintf(`  visual-hive:
%s
    permissions:
      contents: read
      actions: read
    runs-on: ubuntu-latest
    timeout-minutes: 30
    steps:
%s
      - name: Download isolated raw Visual evidence
        if: ${{ needs.setup-authorization.outputs.operation != 'uninstall' }}
        uses: actions/download-artifact@%s
        with:
          name: visual-hive-pr-raw-${{ github.run_id }}
          path: .visual-hive
      - name: Capture runner-owned pull request scope
        if: ${{ needs.setup-authorization.outputs.operation != 'uninstall' }}
        shell: bash
        env:
          HIVE_BASE_SHA: ${{ github.event.pull_request.base.sha }}
          HIVE_HEAD_SHA: ${{ github.event.pull_request.head.sha }}
        run: |
          set -euo pipefail
          git diff --no-ext-diff --no-textconv --no-renames --name-only "$HIVE_BASE_SHA" "$HIVE_HEAD_SHA" > .visual-hive/changed-files.pr.txt
      - name: Rebuild and validate runner-owned review evidence
        if: ${{ needs.setup-authorization.outputs.operation != 'uninstall' }}
        id: trusted_rebuild
        continue-on-error: true
        shell: bash
        run: |
%s
      - name: Enforce deterministic verdict
        if: always()
        shell: bash
        env:
          HIVE_NEEDS_JSON: ${{ toJSON(needs) }}
          HIVE_EXPECTED_REPOSITORY_JOBS: '%s'
          HIVE_REPOSITORY: ${{ github.repository }}
          HIVE_HEAD_SHA: ${{ github.event.pull_request.head.sha }}
          HIVE_SETUP_AUTHORIZED: ${{ needs.setup-authorization.outputs.authorized }}
          HIVE_SETUP_CONTEXT: ${{ needs.setup-authorization.outputs.context }}
          HIVE_SETUP_BINDING_DIGEST: ${{ needs.setup-authorization.outputs.binding_digest }}
          HIVE_SETUP_DIFF_DIGEST: ${{ needs.setup-authorization.outputs.diff_digest }}
          HIVE_SETUP_OPERATION: ${{ needs.setup-authorization.outputs.operation }}
          HIVE_TRUSTED_REBUILD_OUTCOME: ${{ steps.trusted_rebuild.outcome }}
        run: |
%s
      - name: Upload review evidence
        if: ${{ always() && needs.setup-authorization.outputs.operation != 'uninstall' }}
        uses: actions/upload-artifact@%s
        with:
          name: visual-hive-pr
          path: .visual-hive
          if-no-files-found: error
          include-hidden-files: true
          retention-days: 14
`, needs, runnerOwnedVerifierSetupSteps(config, "${{ github.event.pull_request.head.sha }}", "needs.setup-authorization.outputs.operation != 'uninstall'"), downloadArtifactActionSHA,
		indentWorkflowShell(runnerOwnedEvidenceRebuildShell(true), 10), string(expectedJSON),
		indentWorkflowShell(pullRequestEnforcementShell(), 10), uploadArtifactActionSHA)
}

func isolatedRepositoryJobIDs(config Config) []string {
	result := make([]string, 0, len(config.TestCommands))
	for _, command := range config.TestCommands {
		if len(command) > 0 {
			result = append(result, fmt.Sprintf("repository-test-%03d", len(result)+1))
		}
	}
	return result
}

func productionPrerequisiteShell() string {
	return `set -euo pipefail
node <<'NODE'
const needs = JSON.parse(process.env.HIVE_NEEDS_JSON || "{}");
const expectedRepositoryJobs = JSON.parse(process.env.HIVE_EXPECTED_REPOSITORY_JOBS || "[]");
const expected = [...expectedRepositoryJobs, "visual-hive-execution"].sort();
const actual = Object.keys(needs).sort();
if (JSON.stringify(actual) !== JSON.stringify(expected)) {
  throw new Error("production runner prerequisite topology does not match the installed plan");
}
if (needs["visual-hive-execution"]?.result !== "success") {
  throw new Error("isolated Visual Hive execution did not complete successfully");
}
for (const name of expectedRepositoryJobs) {
  if (!needs[name] || !["success", "failure"].includes(needs[name].result)) {
    throw new Error("repository test job lacks a terminal runner result: " + name);
  }
}
NODE`
}

func runnerOwnedVerifierSetupSteps(config Config, targetRef, condition string) string {
	conditionLine := ""
	if strings.TrimSpace(condition) != "" {
		conditionLine = fmt.Sprintf("        if: ${{ %s }}\n", condition)
	}
	return fmt.Sprintf(`      - uses: actions/checkout@%s
%s        with:
          ref: %s
          fetch-depth: 0
          persist-credentials: false
      - uses: actions/checkout@%s
%s        with:
          repository: %s
          ref: %s
          path: .hive-visual-tooling
          persist-credentials: false
      - uses: actions/setup-node@%s
%s        with:
          node-version: 22.23.1
          cache: npm
          cache-dependency-path: .hive-visual-tooling/package-lock.json
      - name: Rebuild exact immutable Visual Hive CLI on fresh runner
%s        working-directory: .hive-visual-tooling
        shell: bash
        env:
          HIVE_VISUAL_HIVE_REF: %s
        run: |
          set -euo pipefail
          test "$(git rev-parse HEAD)" = "$HIVE_VISUAL_HIVE_REF"
          npm ci
          npm run build
          test -z "$(git status --porcelain --untracked-files=no)"
      - name: Move reverified tooling outside target checkout
%s        shell: bash
        run: |
          set -euo pipefail
          mv .hive-visual-tooling "$RUNNER_TEMP/visual-hive-tooling"
          cli="$RUNNER_TEMP/visual-hive-tooling/packages/cli/dist/index.js"
          test -f "$cli"
          sudo chown -R root:root "$RUNNER_TEMP/visual-hive-tooling"
          sudo chmod -R a-w "$RUNNER_TEMP/visual-hive-tooling"
          echo "VISUAL_HIVE_CLI=$cli" >> "$GITHUB_ENV"
`, checkoutActionSHA, conditionLine, targetRef, checkoutActionSHA, conditionLine, config.VisualHiveRepo, config.VisualHiveRef,
		setupNodeActionSHA, conditionLine, conditionLine, config.VisualHiveRef, conditionLine)
}

func runnerOwnedEvaluationScopeScript() string {
	return `const fs = require("fs");
 const readJSON = (file) => JSON.parse(fs.readFileSync(file, "utf8"));
 const runnerOutcome = readJSON(".visual-hive/hive-runner-outcome.json");
 if (runnerOutcome.schemaVersion !== "hive.visual-runner-outcome.v1" || !["success", "failure"].includes(runnerOutcome.outcome) ||
     !["success", "failure"].includes(runnerOutcome.conclusion)) {
   throw new Error("runner-owned Visual pipeline outcome receipt is malformed");
 }
 const pipeline = readJSON(".visual-hive/pipeline.json");
const report = readJSON(".visual-hive/report.json");
if (!Number.isInteger(pipeline.exitCode) || !["passed", "failed", "blocked"].includes(pipeline.status)) {
  throw new Error("pipeline report lacks a terminal deterministic status");
}
fs.writeFileSync(".visual-hive/pipeline-exit-code.txt", String(pipeline.exitCode) + "\n");

 const planPath = ".visual-hive/plan.json";
 const plan = fs.existsSync(planPath) ? readJSON(planPath) : {};
const planRows = Array.isArray(plan.items) ? plan.items : [];
const planExcluded = Array.isArray(plan.excluded) ? plan.excluded : [];
const reportSelected = Array.isArray(report.selectedContracts) ? report.selectedContracts : [];
const reportExcluded = Array.isArray(report.excludedContracts) ? report.excludedContracts : [];
const results = Array.isArray(report.results) ? report.results : [];
const normalizedID = (value) => typeof value === "string" && value.trim() ? value.trim() : null;
const rowID = (row) => row && typeof row === "object" ? normalizedID(row.contractId || row.id) : null;
const sorted = (values) => [...values].sort();
const unique = (values) => new Set(values).size === values.length;
const sameIDs = (left, right) => JSON.stringify(sorted(left)) === JSON.stringify(sorted(right));
const plannedIDs = planRows.map(rowID);
const excludedPlanIDs = planExcluded.map(rowID);
const selectedIDs = reportSelected.map(normalizedID);
const excludedReportIDs = reportExcluded.map(rowID);
const resultIDs = results.map((result) => result && typeof result === "object" ? normalizedID(result.contractId) : null);
const identityComplete = Boolean(planPath) && Array.isArray(plan.items) && Array.isArray(plan.excluded) &&
  Array.isArray(report.selectedContracts) && Array.isArray(report.excludedContracts) && Array.isArray(report.results) &&
  [...plannedIDs, ...excludedPlanIDs, ...selectedIDs, ...excludedReportIDs, ...resultIDs].every(Boolean) &&
  results.every((result) => result && typeof result === "object" && normalizedID(result.contractId) && ["passed", "failed", "created", "skipped"].includes(result.status));
if (!identityComplete || !unique(plannedIDs) || !unique(excludedPlanIDs) || !unique(selectedIDs) || !unique(excludedReportIDs) || !unique(resultIDs) ||
    !sameIDs(plannedIDs, selectedIDs) || !sameIDs(excludedPlanIDs, excludedReportIDs) || !sameIDs(selectedIDs, resultIDs) ||
    selectedIDs.some((contractID) => excludedPlanIDs.includes(contractID))) {
  throw new Error("plan and report lack an exact one-to-one contract evaluation binding");
}
 const passedIDs = runnerOutcome.outcome === "success"
   ? sorted(results.filter((result) => result.status === "passed").map((result) => normalizedID(result.contractId)))
   : [];
fs.writeFileSync(".visual-hive/evaluated-contracts.txt", passedIDs.length ? passedIDs.join("\n") + "\n" : "");
 const authoritative = runnerOutcome.outcome === "success" && plannedIDs.length > 0 && passedIDs.length === plannedIDs.length && pipeline.mode === "full" && plan.mode === "full" && report.mode === "full" &&
   pipeline.status === "passed" && pipeline.exitCode === 0 && report.status === "passed";
 fs.writeFileSync(".visual-hive/authoritative-resolution.txt", authoritative ? "true\n" : "false\n");`
}

func runnerOwnedPlanRegenerationAndEvaluationShell() string {
	return `rm -f .visual-hive/plan.json .visual-hive/plan.full.json
safe_env=(env -u GITHUB_TOKEN -u GH_TOKEN -u GITHUB_OUTPUT -u GITHUB_ENV -u GITHUB_PATH -u GITHUB_STEP_SUMMARY -u ACTIONS_ID_TOKEN_REQUEST_TOKEN -u ACTIONS_ID_TOKEN_REQUEST_URL -u NODE_OPTIONS -u BASH_ENV -u ENV)
"${safe_env[@]}" node "$VISUAL_HIVE_CLI" plan --config visual-hive.config.yaml --mode full --output .visual-hive/plan.json
test -f .visual-hive/plan.json
test ! -e .visual-hive/plan.full.json
node <<'NODE'
` + runnerOwnedEvaluationScopeScript() + `
NODE`
}

// runnerOwnedResolutionScopeReceiptScript admits repository-level resolution
// scopes only from artifacts freshly regenerated by the immutable CLI in the
// verifier job. The rebuild removes the target-authored copies first, so a
// successful validation is also a current-run source binding.
func runnerOwnedResolutionScopeReceiptScript() string {
	return `const fs = require("fs");
const readJSON = (file) => JSON.parse(fs.readFileSync(file, "utf8"));
const fail = (message) => { throw new Error(message); };
const scopes = fs.readFileSync(".visual-hive/evaluated-contracts.txt", "utf8").split(/\r?\n/).map((value) => value.trim()).filter(Boolean);

const layers = readJSON(".visual-hive/testing-layers.json");
const layerStatuses = new Set(["covered", "partial", "missing", "not_applicable", "unknown"]);
if (layers.schemaVersion !== 1 || !Array.isArray(layers.layers) || layers.layers.length !== 12) fail("runner-owned testing-layer receipt is incomplete");
const layerIDs = layers.layers.map((layer) => layer && Number.isInteger(layer.id) ? layer.id : -1);
if (new Set(layerIDs).size !== 12 || layerIDs.some((id) => id < 0 || id > 11) ||
    layers.layers.some((layer) => !layerStatuses.has(layer.status) || !Array.isArray(layer.evidence) || !Array.isArray(layer.gaps) || !Array.isArray(layer.skippedReasons))) {
  fail("runner-owned testing-layer receipt is malformed");
}
const resolvedLayerStatuses = new Set(["covered", "not_applicable"]);
for (const layer of layers.layers) {
  if (resolvedLayerStatuses.has(layer.status) && layer.gaps.length === 0 && layer.skippedReasons.length === 0) {
    scopes.push("testing-layer:" + layer.id);
  }
}

const workflows = readJSON(".visual-hive/workflows.json");
if (workflows.schemaVersion !== 1 || !workflows.summary || typeof workflows.summary !== "object" ||
    !Array.isArray(workflows.workflows) || workflows.workflows.length === 0 || !Array.isArray(workflows.findings) ||
    workflows.workflows.some((workflow) => !workflow || typeof workflow.path !== "string" || !workflow.path.trim())) {
  fail("runner-owned workflow-safety receipt is malformed");
}
const workflowPaths = workflows.workflows.map((workflow) => workflow.path.trim());
if (new Set(workflowPaths).size !== workflowPaths.length) fail("runner-owned workflow-safety receipt contains duplicate paths");
if (workflows.findings.length === 0) scopes.push("workflow-safety");

const providers = readJSON(".visual-hive/provider-results.json");
const evidence = readJSON(".visual-hive/evidence-packet.json");
const report = readJSON(".visual-hive/report.json");
if (providers.schemaVersion !== 1 || !Array.isArray(providers.providers) || providers.providers.length === 0 || !Array.isArray(evidence.providers)) {
  fail("runner-owned provider-governance receipt is incomplete");
}
const providerRows = providers.providers.map((provider) => ({
  id: provider && typeof provider.providerId === "string" ? provider.providerId.trim() : "",
  resultID: provider && provider.result && typeof provider.result.providerId === "string" ? provider.result.providerId.trim() : "",
  status: provider && provider.result && typeof provider.result.status === "string" ? provider.result.status : "",
  uploadStatus: provider && provider.result && provider.result.upload && typeof provider.result.upload.status === "string" ? provider.result.upload.status : ""
}));
const evidenceRows = evidence.providers.map((provider) => ({
  id: provider && typeof provider.providerId === "string" ? provider.providerId.trim() : "",
  status: provider && typeof provider.status === "string" ? provider.status : "",
  uploadStatus: provider && provider.upload && typeof provider.upload.status === "string" ? provider.upload.status : ""
}));
const reportRows = (Array.isArray(report.providerResults) ? report.providerResults : []).map((provider) => ({
  id: provider && typeof provider.providerId === "string" ? provider.providerId.trim() : "",
  status: provider && typeof provider.status === "string" ? provider.status : "",
  uploadStatus: provider && provider.upload && typeof provider.upload.status === "string" ? provider.upload.status : ""
}));
const providerStatuses = new Set(["passed", "failed", "skipped", "missing_credentials", "mock"]);
const providerUploadStatuses = new Set(["uploaded", "skipped", "blocked", "missing_credentials", "failed", "dry_run"]);
const providerReceipt = (row) => row.id + "\u0000" + row.status + "\u0000" + row.uploadStatus;
if (providerRows.some((row) => !row.id || row.id !== row.resultID || !providerStatuses.has(row.status) || (row.uploadStatus && !providerUploadStatuses.has(row.uploadStatus))) ||
    new Set(providerRows.map((row) => row.id)).size !== providerRows.length ||
    reportRows.some((row) => !row.id || !providerStatuses.has(row.status) || (row.uploadStatus && !providerUploadStatuses.has(row.uploadStatus))) ||
    new Set(reportRows.map((row) => row.id)).size !== reportRows.length ||
    JSON.stringify([...reportRows, ...providerRows].map(providerReceipt).sort()) !== JSON.stringify(evidenceRows.map(providerReceipt).sort())) {
  fail("runner-owned provider-governance receipt does not match rebuilt evidence");
}
const issueProducingProviderStatuses = new Set(["failed", "missing_credentials", "blocked"]);
if (evidenceRows.every((row) => !issueProducingProviderStatuses.has(row.status) && !issueProducingProviderStatuses.has(row.uploadStatus))) {
  scopes.push("provider-governance");
}

const evaluated = [...new Set(scopes)].sort();
fs.writeFileSync(".visual-hive/evaluated-contracts.txt", evaluated.join("\n") + "\n");`
}

func runnerOwnedEvidenceRebuildShell(_ bool) string {
	return `set -euo pipefail
runner_workspace="$(readlink -f "$GITHUB_WORKSPACE")"
review_workspace="` + isolatedTargetWorkspace + `"
test "$runner_workspace" = "$GITHUB_WORKSPACE"
test ! -e "` + isolatedTargetRoot + `"
sudo install -d -o root -g root -m 0755 "` + isolatedTargetRoot + `" "$review_workspace"
sudo mount --bind "$runner_workspace" "$review_workspace"
cleanup_review_workspace() {
  cd "$runner_workspace" || true
  if mountpoint -q "$review_workspace"; then
    sudo umount "$review_workspace" || true
  fi
}
trap cleanup_review_workspace EXIT
mountpoint -q "$review_workspace"
test "$(stat -Lc '%d:%i' "$review_workspace")" = "$(stat -Lc '%d:%i' "$runner_workspace")"
cd "$review_workspace"
test -f "$VISUAL_HIVE_CLI"
test ! -w "$VISUAL_HIVE_CLI"
test -d .visual-hive
if find .visual-hive -type l -print -quit | grep -q .; then
  echo "Raw evidence contains a symbolic link" >&2
  exit 1
fi
artifact_files="$(find .visual-hive -type f | wc -l)"
artifact_bytes="$(du -sb .visual-hive | cut -f 1)"
test "$artifact_files" -le 5000
test "$artifact_bytes" -le 1073741824
 rm -f .visual-hive/evidence-packet.json .visual-hive/evidence-summary.md .visual-hive/verdict.json .visual-hive/verdict.md .visual-hive/issues.json .visual-hive/issues.md .visual-hive/baselines.json .visual-hive/artifacts-index.json .visual-hive/capability-parity.json .visual-hive/workflows.json .visual-hive/provider-results.json .visual-hive/testing-layers.json .visual-hive/testing-layers.md
 ` + runnerOwnedPlanRegenerationAndEvaluationShell() + `
"${safe_env[@]}" node "$VISUAL_HIVE_CLI" workflows --config visual-hive.config.yaml
"${safe_env[@]}" node "$VISUAL_HIVE_CLI" providers list --config visual-hive.config.yaml --mock-results --format json
"${safe_env[@]}" node "$VISUAL_HIVE_CLI" evidence --config visual-hive.config.yaml
"${safe_env[@]}" node "$VISUAL_HIVE_CLI" layers --config visual-hive.config.yaml --format json
"${safe_env[@]}" node "$VISUAL_HIVE_CLI" verdict --config visual-hive.config.yaml
"${safe_env[@]}" node "$VISUAL_HIVE_CLI" issues --config visual-hive.config.yaml --write
"${safe_env[@]}" node "$VISUAL_HIVE_CLI" baselines list --config visual-hive.config.yaml --write
"${safe_env[@]}" node "$VISUAL_HIVE_CLI" hive integration-smoke --config visual-hive.config.yaml --mode measured
"${safe_env[@]}" node "$VISUAL_HIVE_CLI" capabilities --config visual-hive.config.yaml
node <<'NODE'
` + runnerOwnedResolutionScopeReceiptScript() + `
NODE
"${safe_env[@]}" node "$VISUAL_HIVE_CLI" artifacts --config visual-hive.config.yaml --complete
cleanup_review_workspace
trap - EXIT
if mountpoint -q "$review_workspace"; then
  echo "Runner-owned review workspace remained mounted" >&2
  exit 1
fi`
}

func pullRequestEnforcementShell() string {
	return `set -euo pipefail
if [ "$HIVE_SETUP_OPERATION" = "uninstall" ]; then
  test "$HIVE_SETUP_AUTHORIZED" = "true"
  echo "Exact out-of-band-authorized Hive uninstall; all managed files are absent at this immutable head."
  exit 0
fi
node <<'NODE'
const fs = require("fs");
          const needs = JSON.parse(process.env.HIVE_NEEDS_JSON || "{}");
          const expected = JSON.parse(process.env.HIVE_EXPECTED_REPOSITORY_JOBS || "[]");
          const actualRepositoryJobs = Object.keys(needs).filter((name) => name.startsWith("repository-test-")).sort();
          if (JSON.stringify(actualRepositoryJobs) !== JSON.stringify(expected)) {
            throw new Error("runner-owned repository job topology does not match the installed plan");
          }
          if (!needs["visual-hive-execution"] || !["success", "failure"].includes(needs["visual-hive-execution"].result)) {
            throw new Error("isolated target execution lacks a terminal runner result");
          }
          for (const name of expected) {
            if (!needs[name] || !["success", "failure"].includes(needs[name].result)) {
              throw new Error("repository test job lacks a terminal runner result: " + name);
            }
          }
          const repositoryFailed = expected.some((name) => needs[name].result === "failure");
          const pipeline = JSON.parse(fs.readFileSync(".visual-hive/pipeline.json", "utf8"));
          const report = JSON.parse(fs.readFileSync(".visual-hive/report.json", "utf8"));
          const verdict = JSON.parse(fs.readFileSync(".visual-hive/verdict.json", "utf8"));
          const readiness = fs.existsSync(".visual-hive/readiness.json") ? JSON.parse(fs.readFileSync(".visual-hive/readiness.json", "utf8")) : {};
          const visualVerdict = verdict?.summary?.visualHiveVerdict;
          const summary = report.summary || {};
          const verdictSummary = verdict.summary || {};
          const contributions = Array.isArray(verdict.gatingContributions) ? verdict.gatingContributions : [];
          const screenshots = (Array.isArray(report.results) ? report.results : []).flatMap((result) => Array.isArray(result.screenshotAssertions) ? result.screenshotAssertions : []);
          const blocking = contributions.filter((item) => item && item.gating === true && item.status !== "passed");
          const visualPassed = needs["visual-hive-execution"].result === "success" && process.env.HIVE_TRUSTED_REBUILD_OUTCOME === "success" && pipeline.exitCode === 0 && ["passed", "warning"].includes(visualVerdict) && Array.isArray(verdict.gatingContributions) && blocking.length === 0;
          const missing = screenshots.filter((item) => item && item.status === "missing_baseline");
          const readinessBlocked = (Array.isArray(readiness.gates) ? readiness.gates : []).filter((gate) => gate && gate.status === "blocked");
          const exclusiveMissingBaselineReadiness = readiness.status === "blocked" && readinessBlocked.some((gate) => gate.id === "baselines:missing-baseline") &&
            readinessBlocked.every((gate) => ["deterministic:status", "baselines:missing-baseline"].includes(gate.id));
          const exclusiveMissingBaseline = ["failed", "blocked"].includes(report.status) && verdictSummary.visualHiveVerdict === "blocked" &&
            Array.isArray(verdictSummary.failedBecause) && verdictSummary.failedBecause.length === 0 &&
            Number(summary.missingBaselines) > 0 && Number(summary.missingBaselines) === missing.length &&
            Number(summary.visualDiffs || 0) === 0 && Number(summary.consoleErrors || 0) === 0 && Number(summary.pageErrors || 0) === 0 && Number(summary.flowStepsFailed || 0) === 0 &&
            exclusiveMissingBaselineReadiness && blocking.some((item) => item.kind === "missing_baseline") && blocking.every((item) => item.status === "blocked" && ["deterministic_run", "contract_result", "missing_baseline", "readiness_gate"].includes(item.kind)) &&
            missing.every((item) => item.contractId && (item.screenshotName || item.name) && item.baselinePath && item.actualPath);
          if (!repositoryFailed && process.env.HIVE_SETUP_OPERATION === "setup" && process.env.HIVE_SETUP_AUTHORIZED === "true" && exclusiveMissingBaseline) {
            const proof = {
              schemaVersion: "hive.setup-baseline-required.v1", repository: process.env.HIVE_REPOSITORY,
              headSha: process.env.HIVE_HEAD_SHA, missingBaselines: missing.length,
              authorizationContext: process.env.HIVE_SETUP_CONTEXT, bindingDigest: process.env.HIVE_SETUP_BINDING_DIGEST,
              diffDigest: process.env.HIVE_SETUP_DIFF_DIGEST,
              disposition: "exact_setup_authorized_missing_baselines_only; hosted_post_merge_capture_required"
            };
            fs.writeFileSync(".visual-hive/setup-baseline-required.json", JSON.stringify(proof, null, 2) + "\n");
            console.log("::warning::Exact setup is blocked only by absent initial baselines. Merge may proceed; Hive must capture and review Linux baselines before production activation or lifecycle writes.");
            process.exit(0);
          }
          if (repositoryFailed && visualPassed && process.env.HIVE_SETUP_AUTHORIZED === "true") {
            const proof = {
              schemaVersion: "hive.setup-bootstrap-repository-failure.v2",
              repository: process.env.HIVE_REPOSITORY,
              headSha: process.env.HIVE_HEAD_SHA,
              repositoryOutcome: "failure",
              visualOutcome: "success",
              authorizationContext: process.env.HIVE_SETUP_CONTEXT,
              bindingDigest: process.env.HIVE_SETUP_BINDING_DIGEST,
              diffDigest: process.env.HIVE_SETUP_DIFF_DIGEST,
              disposition: "out_of_band_exact_setup_authorization; production Hive repairs the runner-owned repository-test failure"
            };
            fs.writeFileSync(".visual-hive/setup-bootstrap-repository-failure.json", JSON.stringify(proof, null, 2) + "\n");
            console.log("::warning::The runner-owned repository test plan is already failing. This exact out-of-band-authorized setup PR may install Hive; production Hive will open and repair the failure. Every unauthorized PR remains blocked.");
            process.exit(0);
          }
          if (repositoryFailed) throw new Error("Repository test plan failed");
          if (!visualPassed) throw new Error("Isolated deterministic Visual Hive verdict failed");
NODE`
}

func prepareIsolatedTargetAccountShell() string {
	return fmt.Sprintf(`set -euo pipefail
if id -u %s >/dev/null 2>&1; then
  echo "Reserved target account already exists" >&2
  exit 1
fi
sudo useradd --create-home --shell /bin/bash %s
sudo install -d -o %s -g %s -m 0755 /home/%s/.local/bin /home/%s/.cache
python_bin="$(command -v python)"
(cd / && sudo -u %s -- env -i HOME=/home/%s PATH="$PATH" "$python_bin" -I -m venv /home/%s/.venv)
target_root=%q
target_workspace=%q
trusted_root=%q
test ! -e "$target_root"
sudo install -d -o root -g root -m 0755 "$target_root" "$target_workspace" "$trusted_root"
test -d "$GITHUB_WORKSPACE"
test ! -L "$GITHUB_WORKSPACE"
test "$(readlink -f "$GITHUB_WORKSPACE")" = "$GITHUB_WORKSPACE"
sudo chown -R %s:%s "$GITHUB_WORKSPACE"
test -d .git
test ! -L .git
test -f visual-hive.config.yaml
test ! -L visual-hive.config.yaml
sudo chown -R root:root .git
sudo chmod -R a-w .git
sudo chown root:root visual-hive.config.yaml
sudo chmod 0444 visual-hive.config.yaml
sudo chmod 1777 "$GITHUB_WORKSPACE"
sudo mount --bind "$GITHUB_WORKSPACE" "$target_workspace"
mountpoint -q "$target_workspace"
test "$(stat -Lc '%%d:%%i' "$target_workspace")" = "$(stat -Lc '%%d:%%i' "$GITHUB_WORKSPACE")"
echo "HIVE_TARGET_WORKSPACE=$target_workspace" >> "$GITHUB_ENV"
`, isolatedTargetAccount, isolatedTargetAccount, isolatedTargetAccount, isolatedTargetAccount,
		isolatedTargetAccount, isolatedTargetAccount, isolatedTargetAccount, isolatedTargetAccount, isolatedTargetAccount,
		isolatedTargetRoot, isolatedTargetWorkspace, isolatedTrustedRoot,
		isolatedTargetAccount, isolatedTargetAccount)
}

func verifyAndSealTargetCheckoutShell() string {
	return fmt.Sprintf(`set -euo pipefail
sudo pkill -KILL -u %s 2>/dev/null || true
test "$(git rev-parse HEAD)" = "$HIVE_TARGET_HEAD_SHA"
git diff --no-ext-diff --no-textconv --exit-code -- .
while IFS= read -r -d '' tracked; do
  if [ -L "$tracked" ]; then
    sudo chown -h root:root -- "$tracked"
  else
    sudo chown root:root -- "$tracked"
    sudo chmod a-w -- "$tracked"
  fi
  directory="$(dirname -- "$tracked")"
  while :; do
    sudo chown root:root -- "$directory"
    sudo chmod 1777 -- "$directory"
    if [ "$directory" = "." ]; then
      break
    fi
    directory="$(dirname -- "$directory")"
  done
done < <(git ls-files -z)
`, isolatedTargetAccount)
}

func verifyImmutableTargetCheckoutShell() string {
	return fmt.Sprintf(`set -euo pipefail
target_workspace="${HIVE_TARGET_WORKSPACE:-}"
cleanup_target_workspace() {
  if [ -n "$target_workspace" ] && mountpoint -q "$target_workspace"; then
    sudo umount "$target_workspace" || true
  fi
}
trap cleanup_target_workspace EXIT
test -n "$target_workspace"
sudo pkill -KILL -u %s 2>/dev/null || true
test "$(git rev-parse HEAD)" = "$HIVE_TARGET_HEAD_SHA"
git diff --no-ext-diff --no-textconv --exit-code -- .
sudo umount "$target_workspace"
trap - EXIT
if mountpoint -q "$target_workspace"; then
  echo "Isolated target workspace remained mounted" >&2
  exit 1
fi
`, isolatedTargetAccount)
}

func isolatedTargetDependencyShell(includeTrustedBrowser bool) string {
	targetInstall := strings.ReplaceAll(dedentWorkflowShell(targetPackageInstallShell()), "corepack enable", "corepack enable --install-directory /home/hive-target/.local/bin")
	pythonInstall := `if [ -f pyproject.toml ]; then
  python -m pip install -e .
elif [ -f requirements.txt ]; then
  python -m pip install -r requirements.txt
fi`
	trustedBrowserBefore := ""
	trustedBrowserAfter := ""
	targetEnvironment := isolatedTargetEnvPrefix()
	if includeTrustedBrowser {
		trustedBrowserBefore = `tooling_playwright="` + isolatedTrustedTooling + `/node_modules/@playwright/test/cli.js"
trusted_browser_path="` + isolatedTrustedRoot + `/playwright-${GITHUB_RUN_ID}"
target_browser_staging="` + isolatedTargetRoot + `/playwright-staging-${GITHUB_RUN_ID}"
test ! -e "$trusted_browser_path"
test ! -e "$target_browser_staging"
sudo install -d -o root -g root -m 0755 "$trusted_browser_path"
sudo install -d -o ` + isolatedTargetAccount + ` -g ` + isolatedTargetAccount + ` -m 0700 "$target_browser_staging"
browser_provisioning_complete=0
cleanup_browser_provisioning() {
  sudo rm -rf -- "$target_browser_staging"
  if [ "$browser_provisioning_complete" -ne 1 ]; then
    sudo rm -rf -- "$trusted_browser_path"
  fi
}
trap cleanup_browser_provisioning EXIT
sudo env PLAYWRIGHT_BROWSERS_PATH="$trusted_browser_path" "$HIVE_TRUSTED_NODE" "$tooling_playwright" install --with-deps chromium
trusted_browser_executable="$(PLAYWRIGHT_BROWSERS_PATH="$trusted_browser_path" "$HIVE_TRUSTED_NODE" -e 'const { chromium } = require(process.argv[1]); process.stdout.write(chromium.executablePath())' "` + isolatedTrustedTooling + `/node_modules/playwright")"
trusted_browser_executable="$(readlink -f "$trusted_browser_executable")"
trusted_browser_path="$(readlink -f "$trusted_browser_path")"
case "$trusted_browser_executable" in
  "$trusted_browser_path"/*) ;;
  *) echo "Pinned browser resolved outside its dedicated root" >&2; exit 1 ;;
esac
if [ ! -x "$trusted_browser_executable" ]; then
  echo "Pinned Visual Hive Playwright browser executable is missing after install: $trusted_browser_executable" >&2
  exit 1
fi
`
		targetEnvironment = strings.Replace(targetEnvironment, "PLAYWRIGHT_BROWSERS_PATH=/home/"+isolatedTargetAccount+"/.cache/ms-playwright", `PLAYWRIGHT_BROWSERS_PATH="$target_browser_staging"`, 1)
		trustedBrowserAfter = `sudo pkill -KILL -u ` + isolatedTargetAccount + ` 2>/dev/null || true
test -d "$target_browser_staging"
if sudo find "$target_browser_staging" -type l -print -quit | grep -q .; then
  echo "Target Playwright browser staging contains a symbolic link" >&2
  exit 1
fi
if sudo find "$target_browser_staging" ! -type d ! -type f -print -quit | grep -q .; then
  echo "Target Playwright browser staging contains a non-regular entry" >&2
  exit 1
fi
test "$(sudo find "$target_browser_staging" -type f | wc -l)" -le 10000
test "$(sudo du -sb "$target_browser_staging" | cut -f 1)" -le 2147483648
sudo cp -a --no-clobber "$target_browser_staging"/. "$trusted_browser_path"/
sudo rm -rf -- "$target_browser_staging"
test ! -e "$target_browser_staging"
sudo chown -R root:root "$trusted_browser_path"
sudo find "$trusted_browser_path" -type d -exec chmod a+rx,a-w {} +
sudo find "$trusted_browser_path" -type f -exec chmod a+r,a-w {} +
if ! sudo -u ` + isolatedTargetAccount + ` -- test -r "$trusted_browser_path" -a -x "$trusted_browser_path"; then
  echo "Sealed Playwright browser root is not readable and traversable by the isolated target account: $trusted_browser_path" >&2
  exit 1
fi
if sudo -u ` + isolatedTargetAccount + ` -- test -w "$trusted_browser_path"; then
  echo "Sealed Playwright browser root remains writable by the isolated target account: $trusted_browser_path" >&2
  exit 1
fi
HIVE_TRUSTED_PLAYWRIGHT_BROWSERS_PATH="$trusted_browser_path"

trusted_browser_manifest="$RUNNER_TEMP/hive-trusted-playwright-${GITHUB_RUN_ID}.tsv"
test ! -e "$trusted_browser_manifest"
touch "$trusted_browser_manifest"
chmod 0600 "$trusted_browser_manifest"
record_trusted_browser() {
  scope="$1"
  runtime="$2"
  executable="$3"
  case "$scope$runtime$executable" in
    *$'\n'*|*$'\t'*) echo "Playwright runtime identity contains unsupported whitespace" >&2; exit 1 ;;
  esac
  executable="$(readlink -f "$executable")"
  case "$executable" in
    "$trusted_browser_path"/*) ;;
    *) echo "Playwright runtime $runtime resolved outside the sealed browser root: $executable" >&2; exit 1 ;;
  esac
  if [ ! -x "$executable" ]; then
    echo "Playwright browser provisioning incomplete for $runtime: expected executable is missing at $executable (PLAYWRIGHT_BROWSERS_PATH=$trusted_browser_path)" >&2
    exit 1
  fi
  digest="$(sha256sum "$executable" | cut -d ' ' -f 1)"
  sudo -u ` + isolatedTargetAccount + ` -- test ! -w "$executable"
  printf '%s\t%s\t%s\t%s\n' "$scope" "$runtime" "$executable" "$digest" >> "$trusted_browser_manifest"
}
record_trusted_browser visual-hive "$tooling_playwright" "$trusted_browser_executable"
while IFS= read -r -d '' playwright_cli; do
  target_browser_executable="$(` + isolatedVisualTargetEnvPrefix() + ` "$HIVE_TRUSTED_NODE" -e 'const path = require("node:path"); const cli = process.argv[1]; const runtime = require(require.resolve("playwright", { paths: [path.dirname(cli)] })); process.stdout.write(runtime.chromium.executablePath())' "$playwright_cli")"
  record_trusted_browser target "$playwright_cli" "$target_browser_executable"
done < <(find "$HIVE_TARGET_WORKSPACE" -path '*/node_modules/@playwright/test/cli.js' -not -path '*/.git/*' -print0 | sort -z)
test "$(wc -l < "$trusted_browser_manifest")" -ge 1
chmod 0400 "$trusted_browser_manifest"
browser_provisioning_complete=1
trap - EXIT
echo "HIVE_TRUSTED_PLAYWRIGHT_BROWSERS_PATH=$trusted_browser_path" >> "$GITHUB_ENV"
echo "HIVE_TRUSTED_BROWSER_EXECUTABLE=$trusted_browser_executable" >> "$GITHUB_ENV"
echo "HIVE_TRUSTED_BROWSER_SHA=$(sha256sum "$trusted_browser_executable" | cut -d ' ' -f 1)" >> "$GITHUB_ENV"
echo "HIVE_TRUSTED_BROWSER_MANIFEST=$trusted_browser_manifest" >> "$GITHUB_ENV"
`
	}
	return fmt.Sprintf(`set -euo pipefail
%s
%s bash --noprofile --norc -euo pipefail <<'HIVE_TARGET_DEPENDENCIES'
test -n "${HIVE_TARGET_WORKSPACE:-}"
test "$(pwd -P)" = "$HIVE_TARGET_WORKSPACE"
%s
%s
while IFS= read -r -d '' playwright_cli; do
  node "$playwright_cli" install chromium
done < <(find . -path '*/node_modules/@playwright/test/cli.js' -not -path './.git/*' -print0 | sort -z)
HIVE_TARGET_DEPENDENCIES
%s
mkdir -p .visual-hive
sudo chown -R %s:%s .visual-hive
`, trustedBrowserBefore, targetEnvironment, targetInstall, pythonInstall, trustedBrowserAfter, isolatedTargetAccount, isolatedTargetAccount)
}

func trustedBrowserHandoffVerificationShell() string {
	return `set -euo pipefail
expected_browser_path="` + isolatedTrustedRoot + `/playwright-${GITHUB_RUN_ID}"
if [ "${HIVE_TRUSTED_PLAYWRIGHT_BROWSERS_PATH:-}" != "$expected_browser_path" ]; then
  echo "Trusted Playwright browser path is missing or changed: expected $expected_browser_path, got ${HIVE_TRUSTED_PLAYWRIGHT_BROWSERS_PATH:-<unset>}" >&2
  exit 1
fi
if [ ! -d "$HIVE_TRUSTED_PLAYWRIGHT_BROWSERS_PATH" ] || [ -w "$HIVE_TRUSTED_PLAYWRIGHT_BROWSERS_PATH" ]; then
  echo "Trusted Playwright browser directory is absent or writable: $HIVE_TRUSTED_PLAYWRIGHT_BROWSERS_PATH" >&2
  exit 1
fi
if [ ! -f "${HIVE_TRUSTED_BROWSER_MANIFEST:-}" ]; then
  echo "Trusted Playwright browser manifest is missing; provisioning did not complete" >&2
  exit 1
fi
manifest_rows=0
while IFS="$(printf '\t')" read -r scope runtime executable digest; do
  test -n "$scope" && test -n "$runtime" && test -n "$executable" && test -n "$digest"
  case "$executable" in
    "$HIVE_TRUSTED_PLAYWRIGHT_BROWSERS_PATH"/*) ;;
    *) echo "Trusted Playwright manifest executable escaped its sealed root: $executable" >&2; exit 1 ;;
  esac
  if [ ! -x "$executable" ]; then
    echo "Trusted Playwright browser executable is missing before Visual Hive execution: $executable (runtime=$runtime, PLAYWRIGHT_BROWSERS_PATH=$HIVE_TRUSTED_PLAYWRIGHT_BROWSERS_PATH)" >&2
    exit 1
  fi
  test "$(sha256sum "$executable" | cut -d ' ' -f 1)" = "$digest"
  sudo -u ` + isolatedTargetAccount + ` -- test ! -w "$executable"
  manifest_rows=$((manifest_rows + 1))
done < "$HIVE_TRUSTED_BROWSER_MANIFEST"
test "$manifest_rows" -ge 1
while IFS= read -r -d '' playwright_cli; do
  expected_executable="$(` + isolatedVisualTargetEnvPrefix() + ` "$HIVE_TRUSTED_NODE" -e 'const path = require("node:path"); const cli = process.argv[1]; const runtime = require(require.resolve("playwright", { paths: [path.dirname(cli)] })); process.stdout.write(runtime.chromium.executablePath())' "$playwright_cli")"
  expected_executable="$(readlink -f "$expected_executable")"
  if ! grep -Fq "$(printf 'target\t%s\t%s\t' "$playwright_cli" "$expected_executable")" "$HIVE_TRUSTED_BROWSER_MANIFEST"; then
    echo "Target Playwright runtime lacks a sealed executable binding: $playwright_cli expects $expected_executable" >&2
    exit 1
  fi
  if ! timeout --signal=KILL 60s ` + isolatedVisualTargetEnvPrefix() + ` "$HIVE_TRUSTED_NODE" -e 'const path = require("node:path"); const cli = process.argv[1]; const runtime = require(require.resolve("playwright", { paths: [path.dirname(cli)] })); (async () => { const browser = await runtime.chromium.launch({ headless: true }); await browser.close(); })().catch(error => { console.error(error); process.exit(1); });' "$playwright_cli"; then
    echo "Target Playwright runtime could not launch its exact sealed headless browser: $playwright_cli (PLAYWRIGHT_BROWSERS_PATH=$HIVE_TRUSTED_PLAYWRIGHT_BROWSERS_PATH)" >&2
    exit 1
  fi
done < <(find "$HIVE_TARGET_WORKSPACE" -path '*/node_modules/@playwright/test/cli.js' -not -path '*/.git/*' -print0 | sort -z)
`
}

func isolatedTargetEnvPrefix() string {
	return "sudo -u " + isolatedTargetAccount + ` -- env -i -C "$HIVE_TARGET_WORKSPACE" HOME=/home/` + isolatedTargetAccount + ` PATH="/home/` + isolatedTargetAccount + `/.local/bin:/home/` + isolatedTargetAccount + `/.venv/bin:$PATH" LANG=C.UTF-8 CI=true PLAYWRIGHT_BROWSERS_PATH=/home/` + isolatedTargetAccount + `/.cache/ms-playwright HIVE_TARGET_WORKSPACE="$HIVE_TARGET_WORKSPACE" HIVE_TARGET_PROCESS=1`
}

func isolatedVisualTargetEnvPrefix() string {
	return "sudo -u " + isolatedTargetAccount + ` -- env -i -C "$HIVE_TARGET_WORKSPACE" HOME=/home/` + isolatedTargetAccount + ` PATH="/home/` + isolatedTargetAccount + `/.local/bin:/home/` + isolatedTargetAccount + `/.venv/bin:$PATH" LANG=C.UTF-8 CI=true PLAYWRIGHT_BROWSERS_PATH="$HIVE_TRUSTED_PLAYWRIGHT_BROWSERS_PATH" HIVE_TARGET_WORKSPACE="$HIVE_TARGET_WORKSPACE" HIVE_TARGET_PROCESS=1`
}

func workflowNeeds(jobNames []string) string {
	if len(jobNames) == 0 {
		return "    if: ${{ always() }}"
	}
	return "    needs:\n" + workflowNeedLines(jobNames) + "\n    if: ${{ always() }}"
}

func appendWorkflowNeed(needs, jobName string) string {
	return appendWorkflowNeeds(needs, jobName)
}

func appendWorkflowNeeds(needs string, jobNames ...string) string {
	if strings.HasPrefix(needs, "    needs:\n") {
		insert := workflowNeedLines(jobNames) + "\n"
		return strings.Replace(needs, "    needs:\n", "    needs:\n"+insert, 1)
	}
	return "    needs:\n" + workflowNeedLines(jobNames) + "\n" + needs
}

func workflowNeedLines(jobNames []string) string {
	lines := make([]string, 0, len(jobNames))
	seen := map[string]bool{}
	for _, name := range jobNames {
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		lines = append(lines, "      - "+name)
	}
	return strings.Join(lines, "\n")
}

func joinWorkflowJobs(jobs ...string) string {
	joined := make([]string, 0, len(jobs))
	for _, job := range jobs {
		job = strings.Trim(job, "\n")
		if job != "" {
			joined = append(joined, job)
		}
	}
	return strings.Join(joined, "\n\n")
}

func dedentWorkflowShell(value string) string {
	lines := strings.Split(strings.Trim(value, "\n"), "\n")
	for index := range lines {
		lines[index] = strings.TrimPrefix(lines[index], "          ")
	}
	return strings.Join(lines, "\n")
}

func indentWorkflowShell(value string, spaces int) string {
	prefix := strings.Repeat(" ", spaces)
	lines := strings.Split(strings.Trim(value, "\n"), "\n")
	for index := range lines {
		if lines[index] != "" {
			lines[index] = prefix + lines[index]
		}
	}
	return strings.Join(lines, "\n")
}
