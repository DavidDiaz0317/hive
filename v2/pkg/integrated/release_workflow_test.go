package integrated

import (
	"os"
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
