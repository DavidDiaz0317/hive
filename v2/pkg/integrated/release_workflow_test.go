package integrated

import (
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
)

func TestIntegratedReleasePublishesOnlyFromTagPush(t *testing.T) {
	data, err := os.ReadFile("../../../.github/workflows/integrated-release.yml")
	if err != nil {
		t.Fatal(err)
	}
	workflow := string(data)
	for _, invariant := range []string{
		`RELEASE_TAG: ${{ (github.event_name == 'push' && github.ref_type == 'tag' && github.ref_name) || '' }}`,
		`RELEASE_VERSION: ${{ (github.event_name == 'push' && github.ref_type == 'tag' && github.ref_name) || github.sha }}`,
		`if: github.event_name == 'push' && needs.build.outputs.release_tag != ''`,
	} {
		if !strings.Contains(workflow, invariant) {
			t.Fatalf("integrated release lost tag-push-only publication invariant %q", invariant)
		}
	}
	if strings.Contains(workflow, `if: needs.build.outputs.release_tag != ''`) {
		t.Fatal("manual tag-ref dispatch could reach the publish job")
	}
}

func TestIntegratedReleaseIsForkOnlyAndExecutionBounded(t *testing.T) {
	data, err := os.ReadFile("../../../.github/workflows/integrated-release.yml")
	if err != nil {
		t.Fatal(err)
	}
	workflow := string(data)
	for _, invariant := range []string{
		`[[ "$GITHUB_REPOSITORY" == "DavidDiaz0317/hive" ]]`,
		`timeout-minutes: 90`,
		`timeout-minutes: 45`,
		`timeout-minutes: 15`,
		`--connect-timeout 15 --max-time 300 --retry 3 --retry-all-errors`,
	} {
		if !strings.Contains(workflow, invariant) {
			t.Fatalf("integrated release lost fork/timeout invariant %q", invariant)
		}
	}
}

func TestIntegratedReleaseSmokesPublicInstallerDefaultsAndPersistentSchedulerHarness(t *testing.T) {
	data, err := os.ReadFile("../../../.github/workflows/integrated-release.yml")
	if err != nil {
		t.Fatal(err)
	}
	workflow := string(data)
	for _, invariant := range []string{
		`$env:CODEX_HOME = Join-Path $work "codex-home"`,
		`skills/hive/SKILL.md`,
		`Get-Command hive -CommandType Application`,
		`TestSystemdServiceManagerInstallStartInspectAndUninstall`,
		`TestDaemonServiceLifecycleRecoversForcedKillAndUninstalls`,
		`TestInstallerTransitionDurablyStopsAndRestartsExactOwnedSchedulers`,
	} {
		if !strings.Contains(workflow, invariant) {
			t.Fatalf("integrated release lost public-default/persistence smoke %q", invariant)
		}
	}
	publicInstall := `install-integrated.ps1 -Version $env:RELEASE_VERSION -ReleaseDir .release/dist -SkipAttestation -InstallDir $install`
	if !strings.Contains(workflow, publicInstall) {
		t.Fatalf("actual Windows archive is not installed through public defaults: want %q", publicInstall)
	}
}

func TestIntegratedReleaseInstallsBrowserBeforeVisualBrowserGates(t *testing.T) {
	data, err := os.ReadFile("../../../.github/workflows/integrated-release.yml")
	if err != nil {
		t.Fatal(err)
	}
	workflow := string(data)
	install := strings.Index(workflow, "npx --no-install playwright install --with-deps chromium")
	tests := strings.Index(workflow, "npm test")
	demo := strings.Index(workflow, "npm run demo:all")
	if install == -1 {
		t.Fatal("integrated release must install Playwright Chromium before browser-backed Visual Hive gates")
	}
	if tests == -1 || install > tests {
		t.Fatal("integrated release must install Playwright Chromium before running Visual Hive tests")
	}
	if demo == -1 || install > demo {
		t.Fatal("integrated release must install Playwright Chromium before running demo:all")
	}
}

func TestIntegratedReleaseProvesWindowsPersistentSchedulerRecovery(t *testing.T) {
	data, err := os.ReadFile("../../../.github/workflows/integrated-release.yml")
	if err != nil {
		t.Fatal(err)
	}
	workflow := string(data)
	for _, invariant := range []string{
		"Prove persistent Windows scheduler recovery",
		`HIVE_TEST_WINDOWS_TASK_SCHEDULER: "1"`,
		`HIVE_TEST_WINDOWS_TASK_RESTART: "1"`,
		"TestWindowsPersistentDaemonStartsAndStopsThroughTaskScheduler",
		"-timeout 4m",
	} {
		if !strings.Contains(workflow, invariant) {
			t.Fatalf("integrated release lost persistent Windows scheduler proof %q", invariant)
		}
	}
}

func TestIntegratedReleaseGatesOnCredentialFreeProviderContainmentForEveryShippedOS(t *testing.T) {
	data, err := os.ReadFile("../../../.github/workflows/integrated-release.yml")
	if err != nil {
		t.Fatal(err)
	}
	workflow := string(data)
	for _, invariant := range []string{
		"provider-containment:",
		"os: [ubuntu-latest, windows-latest]",
		"@openai/codex@0.144.1",
		"sudo apt-get install -y --no-install-recommends apparmor-profiles bubblewrap",
		"/usr/share/apparmor/extra-profiles/bwrap-userns-restrict",
		`sudo apparmor_parser -r "$profile"`,
		`test -x "$(command -v bwrap)"`,
		`find "$RUNNER_TEMP/hive-codex-gate/node_modules/@openai"`,
		"hive-release-codex-empty-home",
		"TestCodexProviderRealNoModelHealthPreflightWithoutAuthorization",
		"provider-authenticated-supplemental:",
		"HIVE_RELEASE_CODEX_AUTH_JSON_B64",
		"TestCodexProviderRealWorktreeInstructionsAndToolsAreIsolated",
		"TestCodexProviderRealStructuredContainmentDeniesReadWriteAndNetwork",
		"needs: [build, windows-smoke, provider-containment]",
	} {
		if !strings.Contains(workflow, invariant) {
			t.Fatalf("integrated release lost real provider-containment gate %q", invariant)
		}
	}
	if count := strings.Count(workflow, "sudo apt-get install -y --no-install-recommends apparmor-profiles bubblewrap"); count != 2 {
		t.Fatalf("integrated release must install the Linux containment prerequisite in mandatory and supplemental jobs, got %d", count)
	}
	if count := strings.Count(workflow, `sudo apparmor_parser -r "$profile"`); count != 2 {
		t.Fatalf("integrated release must load the packaged bwrap profile in mandatory and supplemental jobs, got %d", count)
	}
	if strings.Contains(workflow, "macos-latest") {
		t.Fatal("integrated release must not claim an unshipped macOS artifact or containment gate")
	}
	mandatoryStart := strings.Index(workflow, "  provider-containment:")
	supplementalStart := strings.Index(workflow, "  provider-authenticated-supplemental:")
	publishStart := strings.Index(workflow, "  publish:")
	if mandatoryStart < 0 || supplementalStart <= mandatoryStart || publishStart <= supplementalStart {
		t.Fatal("provider containment jobs are not ordered as mandatory, supplemental, then publish")
	}
	mandatory := workflow[mandatoryStart:supplementalStart]
	if strings.Contains(mandatory, "HIVE_RELEASE_CODEX_AUTH_JSON_B64") || strings.Contains(mandatory, "TestCodexProviderRealStructuredContainmentDeniesReadWriteAndNetwork") {
		t.Fatal("mandatory provider containment gate must remain credential-free and no-model")
	}
	publish := workflow[publishStart:]
	if strings.Contains(publish, "needs: [build, windows-smoke, provider-containment, provider-authenticated-supplemental]") {
		t.Fatal("optional authenticated provider coverage must not block publication")
	}
}

func TestIntegratedReleasePinsFinalVisualHiveDependency(t *testing.T) {
	data, err := os.ReadFile("../../../.github/workflows/integrated-release.yml")
	if err != nil {
		t.Fatal(err)
	}
	workflow := string(data)
	const visualRef = "6891c7fcc831eac8d560ea933d2e72e21a39a475"
	for _, invariant := range []string{
		"repository: DavidDiaz0317/visual-hive",
		"VISUAL_HIVE_REF: ${{ inputs.visual_hive_ref || '" + visualRef + "' }}",
		`NODE_VERSION: 22.23.1`,
	} {
		if !strings.Contains(workflow, invariant) {
			t.Fatalf("integrated release lost exact final dependency invariant %q", invariant)
		}
	}
	if strings.Contains(workflow, "16edc8ab5737314123cbad5823e63cfff0bad386") {
		t.Fatal("integrated release retained the superseded Visual Hive pin")
	}
}

func TestIntegratedReleaseShipsCompletePackageManagerRuntime(t *testing.T) {
	data, err := os.ReadFile("../../../.github/workflows/integrated-release.yml")
	if err != nil {
		t.Fatal(err)
	}
	workflow := string(data)
	for _, invariant := range []string{
		`--node-runtime "$GITHUB_WORKSPACE/.release/runtime-linux"`,
		`--node-runtime "$GITHUB_WORKSPACE/.release/runtime-windows"`,
		`v2/runtime-launchers/linux/pnpm`,
		`v2/runtime-launchers/linux/yarn`,
		`v2/runtime-launchers/windows/pnpm.cmd`,
		`v2/runtime-launchers/windows/yarn.cmd`,
		`foreach ($launcher in @("node.exe", "npm.cmd", "npx.cmd", "corepack.cmd", "pnpm.cmd", "pnpx.cmd", "yarn.cmd", "yarnpkg.cmd"))`,
		`Join-Path $install "runtime/npm.cmd"`,
		`Join-Path $install "runtime/corepack.cmd"`,
		`COREPACK_HOME="$RUNNER_TEMP/hive-release-corepack"`,
		`pnpm@9.15.9`,
		`yarn@1.22.22`,
		`--frozen-lockfile --ignore-scripts`,
		`test "$runtime_digest_after" = "$runtime_digest_before"`,
	} {
		if !strings.Contains(workflow, invariant) {
			t.Fatalf("integrated release lost complete package-manager runtime invariant %q", invariant)
		}
	}
}

func TestIntegratedReleaseProvesVisualHiveLauncherWithoutGlobalNode(t *testing.T) {
	data, err := os.ReadFile("../../../.github/workflows/integrated-release.yml")
	if err != nil {
		t.Fatal(err)
	}
	workflow := string(data)
	for _, invariant := range []string{
		`PATH="$launcher_path" "$linux_root/bin/visual-hive" --version`,
		`$install = Join-Path $work "Hive-Clean Install"`,
		`Get-Command visual-hive -CommandType Application`,
		`Get-Command node -CommandType Application -ErrorAction SilentlyContinue`,
		`Join-Path $install "visual-hive.cmd"`,
		`Packaged Visual Hive identity mismatch without global Node`,
	} {
		if !strings.Contains(workflow, invariant) {
			t.Fatalf("integrated release lost no-global-Node Visual Hive launcher proof %q", invariant)
		}
	}

	installerChecks := map[string][]string{
		"../../install-integrated.ps1": {
			`inventoried.has("visual-hive.cmd")`,
		},
		"../../install-integrated.sh": {
			`visual_link_path="$HOME/.local/bin/visual-hive"`,
			`ln -s "$install_dir/bin/visual-hive" "$visual_link_path"`,
			`inventoried.has('bin/visual-hive')`,
		},
	}
	for path, invariants := range installerChecks {
		installer, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, invariant := range invariants {
			if !strings.Contains(string(installer), invariant) {
				t.Fatalf("integrated installer %s lost Visual Hive launcher ownership invariant %q", path, invariant)
			}
		}
	}
}

func TestRootReadmeLeadsWithSignedIntegratedQuickstart(t *testing.T) {
	data, err := os.ReadFile("../../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	readme := string(data)
	production := strings.Index(readme, "## Production quickstart: signed Hive + Visual Hive")
	legacy := strings.Index(readme, "## Legacy KubeStellar deployment")
	if production < 0 || legacy < 0 || production > legacy {
		t.Fatal("root README must lead with the signed integrated product before legacy deployment instructions")
	}
	for _, invariant := range []string{
		"repo=DavidDiaz0317/hive",
		"gh attestation verify",
		`--signer-workflow "$repo/.github/workflows/integrated-release.yml"`,
		`setup --repo OWNER/REPOSITORY --coverage comprehensive --automation auto-merge --provider codex --visual-hive --start --json`,
		"do **not** install the signed Hive + Visual Hive product",
	} {
		if !strings.Contains(readme, invariant) {
			t.Fatalf("root README lost production/legacy distinction %q", invariant)
		}
	}
}

func TestIntegratedQuickstartDocumentsResolvedSetupDependency(t *testing.T) {
	data, err := os.ReadFile("../../docs/integrated-quickstart.md")
	if err != nil {
		t.Fatal(err)
	}
	quickstart := string(data)
	for _, invariant := range []string{"same installed release manifest used by apply", "`visual_hive_repository`", "immutable `visual_hive_ref`", "`repository@commit`"} {
		if !strings.Contains(quickstart, invariant) {
			t.Fatalf("integrated quickstart lost setup dependency disclosure %q", invariant)
		}
	}
}

func TestLinuxReleaseSmokeSupportsCommitSHARehearsals(t *testing.T) {
	data, err := os.ReadFile("../../test/integrated-installer-linux-release-smoke.sh")
	if err != nil {
		t.Fatal(err)
	}
	smoke := string(data)
	for _, invariant := range []string{
		`if ! printf '%s\n' "$version" | grep -Eq`,
		`Branch-only Linux integrated installer smoke passed`,
		`published_version="$version"`,
		`published_asset="hive-integrated-$published_version-linux-amd64.tar.gz"`,
		`sha256sum "$published_asset"`,
		`--version "$published_version"`,
		`[ "$1" = "repos/$HIVE_FAKE_REPOSITORY/commits/$HIVE_FAKE_VERSION" ]`,
		`[ "$2" = "$HIVE_FAKE_VERSION" ]`,
		`[ "$6" = "$HIVE_FAKE_ASSET" ]`,
		`[ "$8" = "$HIVE_FAKE_ASSET.sha256" ]`,
		`[ "$2" = "$download_dir/$HIVE_FAKE_ASSET" ]`,
		`[ "${13}" = --deny-self-hosted-runners ]`,
		`export HOME="$work_root/attested-home"`,
		`mkdir -p "$HOME"`,
	} {
		if !strings.Contains(smoke, invariant) {
			t.Fatalf("Linux release smoke lost branch-rehearsal published-trust fixture %q", invariant)
		}
	}
	branchExit := strings.Index(smoke, `Branch-only Linux integrated installer smoke passed`)
	publishedFixture := strings.Index(smoke, `attestation_bin="$work_root/attestation-bin"`)
	if branchExit < 0 || publishedFixture < 0 || branchExit > publishedFixture {
		t.Fatal("commit-SHA rehearsal must exit before exercising tag-only provenance")
	}
	if strings.Contains(smoke, `published_version=v0.0.0-integrated.0`) {
		t.Fatal("commit-SHA rehearsal must not relabel immutable release bytes with a synthetic tag identity")
	}
}

func TestIntegratedReleaseVerifiesPublishedReleaseIsImmutable(t *testing.T) {
	data, err := os.ReadFile("../../../.github/workflows/integrated-release.yml")
	if err != nil {
		t.Fatal(err)
	}
	workflow := string(data)
	for _, invariant := range []string{
		`source v2/integrated-release-immutability.sh`,
		`if ! verify_published_release_immutable`,
		`trap cleanup_on_exit EXIT`,
		`release_verified=true`,
		`trap - EXIT`,
	} {
		if !strings.Contains(workflow, invariant) {
			t.Fatalf("integrated release lost immutable-publication verification %q", invariant)
		}
	}
	query := strings.Index(workflow, `if ! verify_published_release_immutable`)
	trap := strings.Index(workflow, `trap cleanup_on_exit EXIT`)
	if query < 0 || trap < 0 || trap > query {
		t.Fatal("release immutability polling must execute under the cleanup trap")
	}
	helper, err := os.ReadFile("../../integrated-release-immutability.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, invariant := range []string{
		`releases/tags/$RELEASE_TAG`,
		`release(tagName:$tag)`,
		`isDraft`,
		`[[ -z "$release_node" ]]`,
	} {
		if !strings.Contains(string(helper), invariant) {
			t.Fatalf("release cleanup lost exact published/draft absence proof %q", invariant)
		}
	}
}

func TestIntegratedReleaseImmutabilityFailurePaths(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the production helper executes on the Ubuntu publish runner")
	}
	command := exec.Command("bash", "../../test/integrated-release-immutability-failure-smoke.sh")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("integrated release immutability failure smoke: %v\n%s", err, output)
	}
}

func TestWindowsInstallerFailureSmokeClearsExpectedChildExit(t *testing.T) {
	data, err := os.ReadFile("../../test/integrated-installer-windows-failure-smoke.ps1")
	if err != nil {
		t.Fatal(err)
	}
	smoke := strings.TrimSpace(string(data))
	if !strings.HasSuffix(smoke, `$global:LASTEXITCODE = 0`) {
		t.Fatal("successful Windows failure smoke must clear the final expected child-process exit code")
	}
}
