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

func TestIntegratedReleaseInstallsBrowserBeforeVisualDemo(t *testing.T) {
	data, err := os.ReadFile("../../../.github/workflows/integrated-release.yml")
	if err != nil {
		t.Fatal(err)
	}
	workflow := string(data)
	install := strings.Index(workflow, "npx --no-install playwright install --with-deps chromium")
	demo := strings.Index(workflow, "npm run demo:all")
	if install == -1 {
		t.Fatal("integrated release must install Playwright Chromium before browser-backed Visual Hive gates")
	}
	if demo == -1 || install > demo {
		t.Fatal("integrated release must install Playwright Chromium before running demo:all")
	}
}

func TestLinuxReleaseSmokeSupportsCommitSHARehearsals(t *testing.T) {
	data, err := os.ReadFile("../../test/integrated-installer-linux-release-smoke.sh")
	if err != nil {
		t.Fatal(err)
	}
	smoke := string(data)
	for _, invariant := range []string{
		`published_version="$version"`,
		`published_version=v0.0.0-integrated.0`,
		`published_asset="hive-integrated-$published_version-linux-amd64.tar.gz"`,
		`sha256sum "$published_asset"`,
		`--version "$published_version"`,
		`[ "$1" = "repos/$HIVE_FAKE_REPOSITORY/commits/$HIVE_FAKE_VERSION" ]`,
		`[ "$2" = "$HIVE_FAKE_VERSION" ]`,
		`[ "$6" = "$HIVE_FAKE_ASSET" ]`,
		`[ "$8" = "$HIVE_FAKE_ASSET.sha256" ]`,
		`[ "$2" = "$download_dir/$HIVE_FAKE_ASSET" ]`,
		`[ "${13}" = --deny-self-hosted-runners ]`,
	} {
		if !strings.Contains(smoke, invariant) {
			t.Fatalf("Linux release smoke lost branch-rehearsal published-trust fixture %q", invariant)
		}
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
