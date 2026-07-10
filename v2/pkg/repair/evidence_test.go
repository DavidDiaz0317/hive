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
