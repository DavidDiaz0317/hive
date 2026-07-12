package integrated

import (
	"strings"
	"testing"
)

func TestReadOnlySetupPlanDoesNotRequireProviderExecutable(t *testing.T) {
	options := SetupOptions{
		Repository: "owner/repo", Coverage: CoverageStandard, Automation: AutomationAdvisory,
		Provider: "codex", StateDir: t.TempDir(), MaxActiveIssues: 1, MaxRepairAttempts: 1,
		VisualHive: true,
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
