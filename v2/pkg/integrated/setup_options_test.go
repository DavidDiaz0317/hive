package integrated

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kubestellar/hive/v2/pkg/automation"
)

func TestSetupAuthorizationIdentityStaysStableWhileOperationBranchChanges(t *testing.T) {
	config := Config{SetupBranch: "hive/upgrade-123", SetupAuthorizationActorID: 456}
	if updateSetupAuthorizationBranchForManagedChange(&config, "hive/setup-123", "") || config.SetupBranch != "hive/upgrade-123" || config.SetupAuthorizationActorID != 456 {
		t.Fatalf("identical cross-user setup rerun rotated authorization: %+v", config)
	}
	if !updateSetupAuthorizationBranchForManagedChange(&config, "hive/setup-123", "visual-hive.config.yaml\n") || config.SetupBranch != "hive/setup-123" || config.SetupAuthorizationActorID != 456 {
		t.Fatalf("substantive setup after upgrade did not rerender the operation branch while preserving the base-workflow authorizer: %+v", config)
	}
}

func TestEffectiveSetupAutoMergePolicyIsVisiblePreservedAndExplicit(t *testing.T) {
	prior := Config{AllowedAutoMergePaths: []string{"tests/**"}, AllowedAutoMergeRisk: []automation.RiskTier{automation.RiskLow}}
	preserved := effectiveSetupAutoMergePolicy(SetupOptions{}, prior, true)
	if strings.Join(preserved.AllowedAutoMergePaths, ",") != "tests/**" || len(preserved.AllowedAutoMergeRisk) != 1 || preserved.AllowedAutoMergeRisk[0] != automation.RiskLow {
		t.Fatalf("existing auto-merge policy was not preserved: %+v", preserved)
	}
	overridden := effectiveSetupAutoMergePolicy(SetupOptions{
		AllowedAutoMergePaths: []string{"**/*.test.*", "**/*.test.*"}, AutoMergePathsExplicit: true,
	}, prior, true)
	if len(overridden.AllowedAutoMergePaths) != 1 || overridden.AllowedAutoMergePaths[0] != "**/*.test.*" || overridden.AllowedAutoMergeRisk[0] != automation.RiskLow {
		t.Fatalf("explicit path policy did not replace only paths: %+v", overridden)
	}
	fresh := effectiveSetupAutoMergePolicy(SetupOptions{}, Config{}, false)
	if len(fresh.AllowedAutoMergePaths) == 0 || len(fresh.AllowedAutoMergeRisk) != 1 || fresh.AllowedAutoMergeRisk[0] != automation.RiskAutomatic {
		t.Fatalf("fresh setup did not expose conservative test-only/automatic defaults: %+v", fresh)
	}
}

func TestSetupRejectsInvalidExplicitAutoMergePolicy(t *testing.T) {
	base := SetupOptions{
		Repository: "owner/repo", Coverage: CoverageStandard, Automation: AutomationAutoMerge,
		Provider: "codex", StateDir: t.TempDir(), MaxActiveIssues: 5, MaxRepairAttempts: 4,
		VisualHive: true, VisualHiveRepo: "owner/visual-hive", VisualHiveRef: strings.Repeat("a", 40),
	}
	invalidPath := base
	invalidPath.AutoMergePathsExplicit = true
	invalidPath.AllowedAutoMergePaths = []string{"../workflow.yml"}
	if err := validateSetupOptions(invalidPath); err == nil || !strings.Contains(err.Error(), "repository-relative") {
		t.Fatalf("unsafe auto-merge path was accepted: %v", err)
	}
	invalidRisk := base
	invalidRisk.AutoMergeRiskExplicit = true
	invalidRisk.AllowedAutoMergeRisk = []automation.RiskTier{99}
	if err := validateSetupOptions(invalidRisk); err == nil || !strings.Contains(err.Error(), "risk tier") {
		t.Fatalf("unknown auto-merge risk was accepted: %v", err)
	}
}

func TestReadOnlySetupPlanDoesNotRequireProviderExecutable(t *testing.T) {
	options := SetupOptions{
		Repository: "owner/repo", Coverage: CoverageStandard, Automation: AutomationAdvisory,
		Provider: "codex", StateDir: t.TempDir(), MaxActiveIssues: 1, MaxRepairAttempts: 1,
		VisualHive: true, VisualHiveRepo: "owner/visual-hive", VisualHiveRef: strings.Repeat("a", 40),
	}
	if err := validateSetupOptions(options); err != nil {
		t.Fatalf("read-only setup plan rejected without provider executable: %v", err)
	}
	options.Apply = true
	if err := validateSetupOptions(options); err == nil {
		t.Fatal("applied setup accepted without provider executable")
	}
}

func TestIntegratedSetupRejectsVisualHiveDisable(t *testing.T) {
	options := SetupOptions{
		Repository: "owner/repo", Coverage: CoverageStandard, Automation: AutomationAdvisory,
		Provider: "codex", StateDir: t.TempDir(), MaxActiveIssues: 1, MaxRepairAttempts: 1,
	}
	if err := validateSetupOptions(options); err == nil {
		t.Fatal("integrated setup accepted a configuration without Visual Hive")
	}
}

func TestAutoMergeSetupPlanExplainsPostMergeActivation(t *testing.T) {
	options := SetupOptions{
		Repository: "owner/repo", Coverage: CoverageComprehensive, Automation: AutomationAutoMerge,
		Provider: "codex", StateDir: t.TempDir(), MaxActiveIssues: 5, MaxRepairAttempts: 4, VisualHive: true, Start: true,
	}
	plan := buildSetupPlan(options, RepositoryInspection{DefaultBranch: "main"})
	joined := strings.Join(append(append([]string{}, plan.RequiredActions...), plan.Warnings...), "\n")
	for _, expected := range []string{"merge the exact setup PR", "trusted", "activate exact-App-bound protection"} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("two-phase setup plan does not explain %q: %s", expected, joined)
		}
	}
	message := setupActivationMessage(AutomationAutoMerge, true, true)
	if !strings.Contains(message, "already-started") || !strings.Contains(message, "before any lifecycle write") {
		t.Fatalf("started setup result does not explain automatic activation: %q", message)
	}
}

func TestSetupPlanDoesNotWarnWhenReviewedBaselinesExist(t *testing.T) {
	options := SetupOptions{
		Repository: "owner/repo", Coverage: CoverageComprehensive, Automation: AutomationAdvisory,
		Provider: "codex", StateDir: t.TempDir(), MaxActiveIssues: 5, MaxRepairAttempts: 4, VisualHive: true,
	}
	plan := buildSetupPlan(options, RepositoryInspection{
		DefaultBranch: "main",
		BaselineFiles: []string{".visual-hive/snapshots/dashboard.png"},
	})
	for _, warning := range plan.Warnings {
		if strings.Contains(warning, "No reviewed visual baselines") {
			t.Fatalf("setup plan reported a missing baseline despite reviewed snapshots: %v", plan.Warnings)
		}
	}
}

func TestSetupPlanDisclosesExactVisualHiveDependencyUsedByApply(t *testing.T) {
	ref := strings.Repeat("b", 40)
	options := SetupOptions{
		Repository: "owner/repo", Coverage: CoverageComprehensive, Automation: AutomationRepairPR,
		Provider: "codex", StateDir: t.TempDir(), MaxActiveIssues: 5, MaxRepairAttempts: 4,
		VisualHive: true, VisualHiveRepo: "owner/visual-hive", VisualHiveRef: ref,
	}
	if err := validateSetupOptions(options); err != nil {
		t.Fatalf("resolved read-only setup dependency was rejected: %v", err)
	}
	plan := buildSetupPlan(options, RepositoryInspection{DefaultBranch: "main"})
	if plan.VisualHiveRepository != options.VisualHiveRepo || plan.VisualHiveRef != ref {
		t.Fatalf("plan dependency = %s@%s, want %s@%s", plan.VisualHiveRepository, plan.VisualHiveRef, options.VisualHiveRepo, ref)
	}
	data, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	for _, disclosed := range []string{`"visual_hive_repository":"owner/visual-hive"`, `"visual_hive_ref":"` + ref + `"`} {
		if !strings.Contains(string(data), disclosed) {
			t.Fatalf("setup plan JSON omitted %s: %s", disclosed, data)
		}
	}
	body := setupPRBody("<!-- hive-setup: owner/repo -->", plan)
	if !strings.Contains(body, "`"+options.VisualHiveRepo+"@"+ref+"`") {
		t.Fatalf("setup review body did not disclose the exact dependency: %s", body)
	}
}
