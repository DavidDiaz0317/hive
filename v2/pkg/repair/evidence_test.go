package repair

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kubestellar/hive/v2/pkg/visualhive"
)

func TestLoadEvidenceSummaryClassifiesMissingRepairSignal(t *testing.T) {
	root := t.TempDir()
	verdict := `{"schemaVersion":"visual-hive.verdict.v1","allContributions":[
{"source":"workflow_audit","kind":"workflow_safety","status":"failed","gating":false,"contractId":"workflow-safety","reason":"Scheduled workflow does not publish a reviewed baseline manifest.","key":"workflow_audit.workflow_safety"}
]}`
	if err := os.WriteFile(filepath.Join(root, "verdict.json"), []byte(verdict), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadEvidenceSummary(root, visualhive.FindingLifecycle{
		Title: "Scheduled workflow should publish a baseline manifest", AffectedContracts: []string{"workflow-safety"},
	})
	if !errors.Is(err, ErrNoActionableEvidence) {
		t.Fatalf("expected missing repair signal classification, got %v", err)
	}
}

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

func TestLoadEvidenceSummaryUsesFirstClassFlowRecommendation(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "verdict.json"), []byte(`{"allContributions":[{"source":"playwright","kind":"deterministic_run","status":"passed","gating":true}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	coverage := `{"maintenanceFindings":[],"recommendations":[
{"id":"flow-steps:app-shell-stability","kind":"add_flow_steps","title":"Add deterministic flow steps for \"app-shell-stability\"","contractId":"app-shell-stability","targetId":"localPreview","route":"/","rationale":["Contract has no deterministic user-flow steps."],"suggestedConfigYaml":"steps:\n  - action: goto\n    route: /\n  - action: assertVisible\n    selector: '[data-testid=dashboard-page]'","suggestedTests":["Run the contract locally."]},
{"id":"flow-steps:other","kind":"add_flow_steps","title":"Add deterministic flow steps for other","contractId":"other"}
]}`
	if err := os.WriteFile(filepath.Join(root, "coverage-recommendations.json"), []byte(coverage), 0o600); err != nil {
		t.Fatal(err)
	}
	summary, err := LoadEvidenceSummary(root, visualhive.FindingLifecycle{
		IssueKind: "missing_visual_coverage", Title: `[Visual Hive] Add visual coverage: Add deterministic flow steps for "app-shell-stability"`, AffectedContracts: []string{"app-shell-stability"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(summary, "coverage.flow-steps:app-shell-stability") || !strings.Contains(summary, "kind=add_flow_steps") || !strings.Contains(summary, "assertVisible") || strings.Contains(summary, "flow-steps:other") {
		t.Fatalf("unexpected first-class coverage evidence: %s", summary)
	}
}

func TestLoadEvidenceSummaryUsesVerifiedTestCreationRecommendation(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "verdict.json"), []byte(`{"schemaVersion":"visual-hive.verdict.v1","allContributions":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	plan := `{"schemaVersion":"visual-hive.test-creation-plan.v1","recommendations":[
{"id":"layer-2-unknown","gapId":"testing-layer:2:unknown","source":"testing_layer","kind":"unit_test","priority":"medium","title":"Add unit test evidence for Unit","rationale":["No repository unit test was detected."],"suggestedTests":["Add focused tests for non-visual behavior."],"artifacts":[".visual-hive/repo-map.json"]},
{"id":"layer-3-unknown","source":"testing_layer","kind":"accessibility_check","title":"unrelated"}
]}`
	if err := os.WriteFile(filepath.Join(root, "test-creation-plan.json"), []byte(plan), 0o600); err != nil {
		t.Fatal(err)
	}
	summary, err := LoadEvidenceSummary(root, visualhive.FindingLifecycle{
		IssueKind: "test_adequacy_gap", Title: "[Visual Hive] Add repository test coverage: Add unit test evidence for Unit",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(summary, "test_creation.layer-2-unknown") || !strings.Contains(summary, "required_scope=test_files_only") || strings.Contains(summary, "unrelated") {
		t.Fatalf("unexpected test-creation evidence: %s", summary)
	}
}
