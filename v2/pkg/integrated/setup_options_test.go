package integrated

import "testing"

func TestReadOnlySetupPlanDoesNotRequireProviderExecutable(t *testing.T) {
	options := SetupOptions{
		Repository: "owner/repo", Coverage: CoverageStandard, Automation: AutomationAdvisory,
		Provider: "codex", StateDir: t.TempDir(), MaxActiveIssues: 1, MaxRepairAttempts: 1,
	}
	if err := validateSetupOptions(options); err != nil {
		t.Fatalf("read-only setup plan rejected without provider executable: %v", err)
	}
	options.Apply = true
	if err := validateSetupOptions(options); err == nil {
		t.Fatal("applied setup accepted without provider executable")
	}
}
