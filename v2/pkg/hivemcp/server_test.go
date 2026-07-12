package hivemcp

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestServerInitializeListAndStructuredCall(t *testing.T) {
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"hive_status","arguments":{"state_dir":"/tmp/hive"}}}`,
	}, "\n") + "\n"
	var output bytes.Buffer
	err := Serve(context.Background(), strings.NewReader(input), &output, func(_ context.Context, name string, arguments map[string]any) (any, error) {
		return map[string]any{"tool": name, "state_dir": arguments["state_dir"]}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(&output)
	var initialize, list, call map[string]any
	for _, target := range []*map[string]any{&initialize, &list, &call} {
		if err := decoder.Decode(target); err != nil {
			t.Fatal(err)
		}
	}
	if initialize["result"].(map[string]any)["protocolVersion"] != ProtocolVersion {
		t.Fatalf("bad initialize response: %+v", initialize)
	}
	tools := list["result"].(map[string]any)["tools"].([]any)
	requiredTools := map[string]bool{
		"hive_setup_plan": false, "hive_setup_apply": false, "hive_doctor": false,
		"hive_status": false, "hive_run": false, "hive_start": false,
		"hive_stop": false, "hive_plan_merge_approval": false, "hive_approve_merge": false,
		"hive_revoke_merge_approval": false, "hive_retry_repair": false, "hive_pause": false,
		"hive_plan_dispatch_recovery": false, "hive_recover_dispatch": false,
		"hive_resume": false, "hive_upgrade": false, "hive_rollback": false,
		"hive_uninstall": false,
	}
	var issueLimit, uninstall, upgrade map[string]any
	for _, candidate := range tools {
		value := candidate.(map[string]any)
		if name, ok := value["name"].(string); ok {
			if _, required := requiredTools[name]; required {
				requiredTools[name] = true
			}
		}
		if value["name"] == "hive_set_issue_limit" {
			issueLimit = value
		}
		if value["name"] == "hive_uninstall" {
			uninstall = value
		}
		if value["name"] == "hive_upgrade" {
			upgrade = value
		}
	}
	if issueLimit == nil {
		t.Fatal("hive_set_issue_limit is missing")
	}
	for name, found := range requiredTools {
		if !found {
			t.Fatalf("%s is missing", name)
		}
	}
	properties := issueLimit["inputSchema"].(map[string]any)["properties"].(map[string]any)
	value := properties["value"].(map[string]any)
	if value["minimum"] != float64(1) && value["minimum"] != 1 {
		t.Fatalf("unexpected issue-limit schema: %+v", value)
	}
	if !knownTool("hive_set_retry_limit") {
		t.Fatal("hive_set_retry_limit is missing")
	}
	if uninstall == nil || upgrade == nil {
		t.Fatal("upgrade or uninstall schema is missing")
	}
	uninstallProperties := uninstall["inputSchema"].(map[string]any)["properties"].(map[string]any)
	if _, ok := uninstallProperties["delete_state"]; !ok {
		t.Fatal("hive_uninstall cannot express --delete-state")
	}
	upgradeProperties := upgrade["inputSchema"].(map[string]any)["properties"].(map[string]any)
	if upgradeProperties["value"].(map[string]any)["pattern"] != `^[a-fA-F0-9]{40}$` {
		t.Fatalf("hive_upgrade schema does not require an immutable commit: %+v", upgradeProperties["value"])
	}
	result := call["result"].(map[string]any)
	if result["isError"] != false || result["structuredContent"].(map[string]any)["tool"] != "hive_status" {
		t.Fatalf("bad tool call response: %+v", call)
	}
}

func TestServerReportsToolExecutionErrorsInsideResult(t *testing.T) {
	input := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"hive_run","arguments":{}}}` + "\n"
	var output bytes.Buffer
	_ = Serve(context.Background(), strings.NewReader(input), &output, func(context.Context, string, map[string]any) (any, error) {
		return nil, context.DeadlineExceeded
	})
	if !strings.Contains(output.String(), `"isError":true`) || !strings.Contains(output.String(), "deadline exceeded") {
		t.Fatalf("expected tool execution error result: %s", output.String())
	}
}
