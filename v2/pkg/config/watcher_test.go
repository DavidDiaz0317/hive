package config

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestWatcher_ReloadsOnChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hive.yaml")

	initial := minimalValidYAML("original-org", "ghp_tok")
	if err := os.WriteFile(path, []byte(initial), 0o600); err != nil {
		t.Fatal(err)
	}

	var reloadCount atomic.Int32
	var lastOrg atomic.Value

	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	w := NewWatcher(path, func(cfg *Config) {
		reloadCount.Add(1)
		lastOrg.Store(cfg.Project.Org)
	}, logger)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go w.Start(ctx)

	// Give the watcher time to start
	time.Sleep(100 * time.Millisecond)

	// Modify the file
	updated := minimalValidYAML("updated-org", "ghp_tok")
	if err := os.WriteFile(path, []byte(updated), 0o600); err != nil {
		t.Fatal(err)
	}

	// Wait for debounce + reload
	const waitForReload = 2 * time.Second
	deadline := time.Now().Add(waitForReload)
	for time.Now().Before(deadline) {
		if reloadCount.Load() > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if reloadCount.Load() == 0 {
		t.Fatal("expected at least one reload after file change")
	}

	org, ok := lastOrg.Load().(string)
	if !ok || org != "updated-org" {
		t.Errorf("expected org = %q after reload, got %q", "updated-org", org)
	}
}

func TestWatcher_CancelStops(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hive.yaml")
	if err := os.WriteFile(path, []byte(minimalValidYAML("o", "ghp_tok")), 0o600); err != nil {
		t.Fatal(err)
	}

	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	w := NewWatcher(path, func(cfg *Config) {}, logger)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		w.Start(ctx)
		close(done)
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
		// OK
	case <-time.After(2 * time.Second):
		t.Fatal("watcher did not stop after context cancellation")
	}
}

func TestWatcher_Debounce(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hive.yaml")
	if err := os.WriteFile(path, []byte(minimalValidYAML("o", "ghp_tok")), 0o600); err != nil {
		t.Fatal(err)
	}

	var reloadCount atomic.Int32
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	w := NewWatcher(path, func(cfg *Config) {
		reloadCount.Add(1)
	}, logger)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Start(ctx)
	time.Sleep(100 * time.Millisecond)

	// Write multiple times in quick succession — should debounce to one reload
	const rapidWrites = 5
	for i := 0; i < rapidWrites; i++ {
		os.WriteFile(path, []byte(minimalValidYAML("o", "ghp_tok")), 0o600)
		time.Sleep(50 * time.Millisecond)
	}

	// Wait for debounce to settle
	time.Sleep(time.Second)

	count := reloadCount.Load()
	if count > 2 {
		t.Errorf("expected debouncing to reduce reloads, got %d reloads for %d writes", count, rapidWrites)
	}
}

func TestWatcherProgrammaticSaveFailureDoesNotSuppressExternalEdit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hive.yaml")
	if err := os.WriteFile(path, []byte(minimalValidYAML("original-org", "ghp_tok")), 0o600); err != nil {
		t.Fatal(err)
	}

	var reloadedOrg string
	w := NewWatcher(path, func(cfg *Config) {
		reloadedOrg = cfg.Project.Org
	}, slog.Default())
	invalid, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	invalid.Project.Org = ""
	if err := w.ProgrammaticSave(invalid); err == nil {
		t.Fatal("ProgrammaticSave unexpectedly accepted invalid config")
	}

	if err := os.WriteFile(path, []byte(minimalValidYAML("external-org", "ghp_tok")), 0o600); err != nil {
		t.Fatal(err)
	}
	w.reload()
	if reloadedOrg != "external-org" {
		t.Fatalf("external edit was suppressed after failed save: got org %q", reloadedOrg)
	}
}

func TestWatcherProgrammaticSaveSuppressesOnlyMatchingDigest(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hive.yaml")
	if err := os.WriteFile(path, []byte(minimalValidYAML("original-org", "ghp_tok")), 0o600); err != nil {
		t.Fatal(err)
	}

	var reloadCount int
	var reloadedOrg string
	w := NewWatcher(path, func(cfg *Config) {
		reloadCount++
		reloadedOrg = cfg.Project.Org
	}, slog.Default())
	candidate, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	candidate.Project.Org = "programmatic-org"
	if err := w.ProgrammaticSave(candidate); err != nil {
		t.Fatal(err)
	}

	// The watcher may consume its own exact write without reloading it.
	w.reload()
	if reloadCount != 0 {
		t.Fatalf("matching programmatic write unexpectedly reloaded %d times", reloadCount)
	}

	// Once an external editor changes the bytes, that edit must be delivered.
	if err := os.WriteFile(path, []byte(minimalValidYAML("external-org", "ghp_tok")), 0o600); err != nil {
		t.Fatal(err)
	}
	w.reload()
	if reloadCount != 1 || reloadedOrg != "external-org" {
		t.Fatalf("external edit not delivered: reloads=%d org=%q", reloadCount, reloadedOrg)
	}
}

func TestWatcherProgrammaticSaveDoesNotConsumeRacingExternalDigest(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hive.yaml")
	if err := os.WriteFile(path, []byte(minimalValidYAML("original-org", "ghp_tok")), 0o600); err != nil {
		t.Fatal(err)
	}

	var reloadedOrg string
	w := NewWatcher(path, func(cfg *Config) {
		reloadedOrg = cfg.Project.Org
	}, slog.Default())
	programmatic := []byte(minimalValidYAML("programmatic-org", "ghp_tok"))
	external := []byte(minimalValidYAML("racing-external-org", "ghp_tok"))
	if err := w.programmaticSave(digestBytes(programmatic), func() error {
		if err := os.WriteFile(path, programmatic, 0o600); err != nil {
			return err
		}
		// Simulate an editor winning after Hive's write but before watcher
		// bookkeeping completes.
		return os.WriteFile(path, external, 0o600)
	}); err != nil {
		t.Fatal(err)
	}
	w.reload()
	if reloadedOrg != "racing-external-org" {
		t.Fatalf("racing external edit was consumed as our save: got org %q", reloadedOrg)
	}
}

func TestWatcherProgrammaticSaveRejectsPendingExternalEdit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hive.yaml")
	if err := os.WriteFile(path, []byte(minimalValidYAML("original-org", "ghp_tok")), 0o600); err != nil {
		t.Fatal(err)
	}

	var reloadedOrg string
	w := NewWatcher(path, func(cfg *Config) {
		reloadedOrg = cfg.Project.Org
	}, slog.Default())
	candidate, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	candidate.Project.Org = "dashboard-org"

	// The external editor writes after the candidate was staged but before
	// Hive reaches its persistence boundary.
	if err := os.WriteFile(path, []byte(minimalValidYAML("external-org", "ghp_tok")), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := w.ProgrammaticSave(candidate); !errors.Is(err, ErrConcurrentConfigEdit) {
		t.Fatalf("ProgrammaticSave error = %v, want ErrConcurrentConfigEdit", err)
	}
	w.reload()
	if reloadedOrg != "external-org" {
		t.Fatalf("pending external edit was not reloaded: got org %q", reloadedOrg)
	}
}
