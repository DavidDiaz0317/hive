package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDaemonEnvironmentDropsLongLivedGitHubTokens(t *testing.T) {
	environment := daemonEnvironment([]string{"PATH=/bin", "HIVE_GITHUB_TOKEN=secret", "GH_TOKEN=secret", "GITHUB_TOKEN=secret", "OPENAI_API_KEY=provider"})
	joined := ""
	for _, entry := range environment {
		joined += entry + "\n"
	}
	if containsAny(joined, "HIVE_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN") {
		t.Fatalf("GitHub tokens leaked into daemon environment: %s", joined)
	}
	if !containsAny(joined, "OPENAI_API_KEY=provider") {
		t.Fatalf("repair-provider environment was unexpectedly removed: %s", joined)
	}
}

func TestDaemonStatusIsDurableAndReflectsLiveProcess(t *testing.T) {
	stateDir := t.TempDir()
	status := integratedDaemonStatus{
		SchemaVersion: daemonStatusSchema, PID: os.Getpid(), Running: true, Repository: "owner/repo", StateDir: stateDir,
		StartedAt: time.Now().UTC(), IntervalSeconds: 900,
	}
	status.Executable, _ = os.Executable()
	if err := writeIntegratedDaemonStatus(stateDir, status); err != nil {
		t.Fatal(err)
	}
	loaded := readIntegratedDaemonStatus(stateDir)
	if !loaded.Running || loaded.PID != os.Getpid() || loaded.Repository != "owner/repo" {
		t.Fatalf("unexpected daemon status: %+v", loaded)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "integrated", "daemon.json")); err != nil {
		t.Fatal(err)
	}
}

func containsAny(value string, candidates ...string) bool {
	for _, candidate := range candidates {
		if len(candidate) > 0 && strings.Contains(value, candidate) {
			return true
		}
	}
	return false
}
