package repair

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kubestellar/hive/v2/pkg/visualhive"
)

func TestLoadEvidenceSummaryFiltersToFindingSignalAndContracts(t *testing.T) {
	root := t.TempDir()
	verdict := `{"schemaVersion":"visual-hive.verdict.v1","allContributions":[
{"source":"screenshot_diff","kind":"missing_baseline","status":"blocked","gating":true,"contractId":"deploy-preview-smoke","targetId":"deployPreview","reason":"review baseline","key":"screenshot_diff.missing_baseline.deploy-preview-smoke"},
{"source":"playwright","kind":"console_error","status":"failed","gating":true,"contractId":"deploy-preview-smoke","targetId":"deployPreview","reason":"Failed to load resource: 404","key":"playwright.console_error.deploy-preview-smoke"},
{"source":"playwright","kind":"console_error","status":"failed","gating":true,"contractId":"another-contract","targetId":"other","reason":"unrelated","key":"playwright.console_error.another-contract"}
]}`
	if err := os.WriteFile(filepath.Join(root, "verdict.json"), []byte(verdict), 0o600); err != nil {
		t.Fatal(err)
	}
	summary, err := LoadEvidenceSummary(root, visualhive.FindingLifecycle{
		Title: "Repair deploy-preview-smoke: console_error", AffectedContracts: []string{"deploy-preview-smoke"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(summary, "playwright.console_error.deploy-preview-smoke") || strings.Contains(summary, "missing_baseline") || strings.Contains(summary, "another-contract") {
		t.Fatalf("unexpected evidence summary: %s", summary)
	}
}

func TestLoadEvidenceSummaryRejectsCredentials(t *testing.T) {
	root := t.TempDir()
	verdict := `{"allContributions":[{"source":"playwright","kind":"console_error","status":"failed","gating":true,"contractId":"app","reason":"github_pat_abcdefghijklmnopqrstuvwxyz123456","key":"playwright.console_error.app"}]}`
	if err := os.WriteFile(filepath.Join(root, "verdict.json"), []byte(verdict), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadEvidenceSummary(root, visualhive.FindingLifecycle{Title: "console_error", AffectedContracts: []string{"app"}})
	if err == nil || !strings.Contains(err.Error(), "unsafe") {
		t.Fatalf("expected credential rejection, got %v", err)
	}
}

func TestLoadEvidenceSummaryUsesDeterministicCoverageRecommendation(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "verdict.json"), []byte(`{"allContributions":[{"source":"playwright","kind":"deterministic_run","status":"passed","gating":true,"reason":"passed"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	coverage := `{"maintenanceFindings":[
{"id":"missing-mobile-scenarios","kind":"missing_mobile_viewport","contractId":"scenario-gallery-contract","targetId":"localPreview","route":"/scenarios","viewport":"mobile","message":"Contract has no mobile screenshot.","evidence":["viewports=desktop"],"recommendedAction":"expand"},
{"id":"unrelated","kind":"missing_mobile_viewport","contractId":"other","message":"unrelated"}
],"recommendations":[{"maintenanceFindingId":"missing-mobile-scenarios","suggestedConfigYaml":"screenshots:\n  - name: scenarios-mobile\n    route: /scenarios\n    viewport: mobile"}]}`
	if err := os.WriteFile(filepath.Join(root, "coverage-recommendations.json"), []byte(coverage), 0o600); err != nil {
		t.Fatal(err)
	}
	summary, err := LoadEvidenceSummary(root, visualhive.FindingLifecycle{
		IssueKind: "missing_visual_coverage", Title: "Maintain visual test: missing_mobile_viewport", AffectedContracts: []string{"scenario-gallery-contract"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(summary, "coverage.missing-mobile-scenarios") || !strings.Contains(summary, "suggested_config=screenshots") || strings.Contains(summary, "unrelated") {
		t.Fatalf("unexpected coverage evidence summary: %s", summary)
	}
}
