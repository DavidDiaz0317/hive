package integrated

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kubestellar/hive/v2/pkg/visualhive"
)

func TestActiveRepairFindingSkipsHumanReviewOnlyIssue(t *testing.T) {
	state := visualhive.LifecycleState{Findings: map[string]*visualhive.FindingLifecycle{
		"a-baseline": {
			RepositoryFingerprint: "a-baseline", Status: visualhive.StatusIssueOpen,
			IssueNumber: 100, HumanReviewRequired: true,
		},
		"b-console": {
			RepositoryFingerprint: "b-console", Status: visualhive.StatusIssueOpen,
			IssueNumber: 101,
		},
	}}

	finding, ok := activeRepairFinding(state)
	if !ok || finding.RepositoryFingerprint != "b-console" {
		t.Fatalf("expected actionable finding, got ok=%t finding=%+v", ok, finding)
	}
}

func TestRepairPreparationAndPinnedCLIEnvironment(t *testing.T) {
	checkout := t.TempDir()
	if err := os.WriteFile(filepath.Join(checkout, "package-lock.json"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	commands := repairPreparationCommands(checkout)
	if len(commands) != 1 || commands[0].Name != "npm" || len(commands[0].Args) != 1 || commands[0].Args[0] != "ci" {
		t.Fatalf("unexpected preparation commands: %+v", commands)
	}
	cli := filepath.Join(checkout, "visual-hive", "dist", "index.js")
	environment := repairValidationEnvironment(Config{VisualHiveArgs: []string{cli}})
	if environment["VISUAL_HIVE_CLI"] != cli {
		t.Fatalf("pinned CLI environment = %v", environment)
	}
}
