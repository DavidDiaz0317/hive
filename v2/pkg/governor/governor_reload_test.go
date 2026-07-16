package governor

import (
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/kubestellar/hive/v2/pkg/config"
)

func TestUpdateConfigAndAgentsRefreshesNormalCadenceSnapshotAtomically(t *testing.T) {
	initialConfig := config.GovernorConfig{Modes: map[string]config.ModeConfig{
		"idle": {Cadences: map[string]string{"worker": "1ns", "quality": "1ns"}},
	}}
	g := New(initialConfig, map[string]config.AgentConfig{
		"worker":  {Enabled: true, Role: "developer"},
		"quality": {Enabled: true, Role: "quality"},
	}, slog.Default())

	if due := g.Evaluate(0, 0, 0, 0); !slices.Contains(due, "worker") || !slices.Contains(due, "quality") {
		t.Fatalf("initial enabled agents were not both eligible: %v", due)
	}

	reloadedConfig := config.GovernorConfig{Modes: map[string]config.ModeConfig{
		"idle": {Cadences: map[string]string{"worker": "1ns", "quality": "1ns"}},
	}}
	reloadedAgents := map[string]config.AgentConfig{
		"worker": {Enabled: true, Role: "reviewer", OnDemand: true},
	}
	g.UpdateConfigAndAgents(reloadedConfig, reloadedAgents)

	// Mutating the watcher's map after the handoff must not mutate the
	// Governor snapshot used by normal cadence or Visual admission.
	worker := reloadedAgents["worker"]
	worker.Role = "mutated-after-reload"
	worker.OnDemand = false
	reloadedAgents["worker"] = worker

	if due := g.Evaluate(0, 0, 0, 0); len(due) != 0 {
		t.Fatalf("reloaded on-demand/disabled agents must not be kicked: %v", due)
	}

	g.mu.RLock()
	defer g.mu.RUnlock()
	if len(g.agents) != 1 || g.agents["worker"].Role != "reviewer" || !g.agents["worker"].OnDemand {
		t.Fatalf("Governor did not retain the exact reloaded enabled-agent snapshot: %#v", g.agents)
	}
	if _, exists := g.state.Cadences["quality"]; exists {
		t.Fatal("disabled agent retained a stale normal cadence after reload")
	}
	if g.state.LastEval.Before(time.Now().Add(-time.Minute)) {
		t.Fatal("normal Governor evaluation did not continue after agent reload")
	}
}
