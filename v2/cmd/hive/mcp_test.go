package main

import (
	"errors"
	"strings"
	"testing"
)

func TestMCPSetupPreservesOmittedExistingPolicyAndExplicitIntegratedVisualHive(t *testing.T) {
	args, err := mcpCLIArgs("hive_setup_apply", map[string]any{"state_dir": "state", "visual_hive": true})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	for _, absent := range []string{"--repo", "--coverage", "--automation", "--max-active-issues", "--max-repair-attempts", "--provider"} {
		if strings.Contains(joined, absent) {
			t.Fatalf("omitted setup value was forced into MCP command: %s", joined)
		}
	}
	if !strings.Contains(joined, "--visual-hive=true") || !strings.Contains(joined, "--start") || !strings.Contains(joined, "--json") {
		t.Fatalf("integrated Visual Hive or apply behavior was lost: %s", joined)
	}
}

func TestMCPSetupAndUninstallPreserveOperatorOptions(t *testing.T) {
	setup, err := mcpCLIArgs("hive_setup_apply", map[string]any{"repo": "owner/repo", "start": false, "run_interval_seconds": 120})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(setup, " ")
	if strings.Contains(joined, "--start") || !strings.Contains(joined, "--run-interval 120s") {
		t.Fatalf("MCP setup lost scheduler parity: %s", joined)
	}
	uninstall, err := mcpCLIArgs("hive_uninstall", map[string]any{"state_dir": "state", "delete_state": true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(uninstall, " "), "--delete-state") {
		t.Fatalf("MCP uninstall lost explicit state-deletion parity: %v", uninstall)
	}
}

func TestMCPProductionCommandParity(t *testing.T) {
	cases := map[string]map[string]any{
		"hive_setup_plan":             {"repo": "owner/repo", "coverage": "comprehensive", "automation": "auto-merge"},
		"hive_setup_apply":            {"repo": "owner/repo", "coverage": "comprehensive", "automation": "auto-merge"},
		"hive_doctor":                 {},
		"hive_status":                 {},
		"hive_run":                    {},
		"hive_start":                  {},
		"hive_stop":                   {},
		"hive_plan_merge_approval":    {"pr_number": 7, "head_sha": strings.Repeat("a", 40)},
		"hive_approve_merge":          {"pr_number": 7, "head_sha": strings.Repeat("a", 40), "base_sha": strings.Repeat("b", 40), "diff_digest": strings.Repeat("c", 64), "reason": "reviewed"},
		"hive_revoke_merge_approval":  {"reason": "cancelled"},
		"hive_retry_repair":           {"finding": "fingerprint", "recurrence": 0, "attempt": 2, "failure_class": "infrastructure", "failure_id": "failure-1", "reason": "tool restored"},
		"hive_plan_dispatch_recovery": {"action": "retry", "correlation": strings.Repeat("d", 64)},
		"hive_recover_dispatch":       {"action": "retry", "correlation": strings.Repeat("d", 64), "request_digest": strings.Repeat("e", 64), "plan_digest": strings.Repeat("f", 64), "planned_at": "2026-07-11T12:00:00Z", "reason": "transport verified"},
		"hive_set_coverage":           {"value": "comprehensive"},
		"hive_set_automation":         {"value": "auto-merge"},
		"hive_set_issue_limit":        {"value": 5},
		"hive_set_retry_limit":        {"value": 4},
		"hive_pause":                  {},
		"hive_resume":                 {},
		"hive_upgrade":                {"value": strings.Repeat("b", 40)},
		"hive_rollback":               {"value": strings.Repeat("c", 40)},
		"hive_uninstall":              {},
	}
	for name, arguments := range cases {
		t.Run(name, func(t *testing.T) {
			args, err := mcpCLIArgs(name, arguments)
			if err != nil || len(args) == 0 {
				t.Fatalf("mapping = %v, %v", args, err)
			}
		})
	}
}

func TestMCPNonZeroStructuredCLIResultIsAnError(t *testing.T) {
	_, err := decodeCLIResult([]string{"approve-merge"}, []byte(`{"error":"gate failed"}`), nil, errors.New("exit status 1"))
	if err == nil || !strings.Contains(err.Error(), "gate failed") || !strings.Contains(err.Error(), "exit status 1") {
		t.Fatalf("structured CLI denial was not propagated as MCP error: %v", err)
	}
}

func TestMCPDispatchRecoveryPreservesExactPlanApplyBindings(t *testing.T) {
	correlation, requestDigest, planDigest := strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64)
	args, err := mcpCLIArgs("hive_recover_dispatch", map[string]any{
		"state_dir": "state", "action": "revoke", "correlation": correlation,
		"request_digest": requestDigest, "plan_digest": planDigest, "planned_at": "2026-07-11T12:00:00.123Z", "reason": "retire uncertain transport",
	})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	for _, binding := range []string{"recover-dispatch", "--action revoke", "--correlation " + correlation, "--request-digest " + requestDigest, "--plan-digest " + planDigest, "--planned-at 2026-07-11T12:00:00.123Z", "--reason retire uncertain transport", "--json"} {
		if !strings.Contains(joined, binding) {
			t.Errorf("MCP recovery command lost %q: %s", binding, joined)
		}
	}
}
