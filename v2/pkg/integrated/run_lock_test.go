package integrated

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestProductionRunLeasePreventsOverlapAndReleases(t *testing.T) {
	stateDir := t.TempDir()
	release, err := acquireProductionRunLease(stateDir, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := acquireProductionRunLease(stateDir, time.Minute); !errors.Is(err, ErrRunInProgress) {
		t.Fatalf("overlapping run was not rejected: %v", err)
	}
	release()
	releaseAgain, err := acquireProductionRunLease(stateDir, time.Minute)
	if err != nil {
		t.Fatalf("released lease could not be reclaimed: %v", err)
	}
	releaseAgain()
}

func TestProductionRunLeaseRecoversExpiredOwner(t *testing.T) {
	stateDir := t.TempDir()
	dir := filepath.Join(stateDir, "integrated")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	expired := productionRunLease{
		SchemaVersion: "hive.production-run-lease.v1", ID: "expired", PID: 1 << 30,
		StartedAt: now.Add(-2 * time.Hour), ExpiresAt: now.Add(-time.Hour),
	}
	data, _ := json.Marshal(expired)
	if err := os.WriteFile(filepath.Join(dir, "production-run.lock"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	release, err := acquireProductionRunLease(stateDir, time.Minute)
	if err != nil {
		t.Fatalf("expired lease was not recovered: %v", err)
	}
	release()
}

func TestProductionRunLeaseRecoversDeadOwnerBeforeExpiry(t *testing.T) {
	stateDir := t.TempDir()
	dir := filepath.Join(stateDir, "integrated")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	dead := productionRunLease{
		SchemaVersion: "hive.production-run-lease.v1", ID: "dead", PID: 1 << 30,
		StartedAt: time.Now().Add(-time.Minute), ExpiresAt: time.Now().Add(time.Hour),
	}
	data, _ := json.Marshal(dead)
	if err := os.WriteFile(filepath.Join(dir, "production-run.lock"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	release, err := acquireProductionRunLease(stateDir, time.Minute)
	if err != nil {
		t.Fatalf("dead lease owner was not recovered: %v", err)
	}
	release()
}

func TestProductionRunLeaseNeverStealsExpiredLiveOwner(t *testing.T) {
	stateDir := t.TempDir()
	dir := filepath.Join(stateDir, "integrated")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	live := productionRunLease{
		SchemaVersion: "hive.production-run-lease.v1", ID: "live-but-expired", PID: os.Getpid(),
		StartedAt: time.Now().Add(-time.Hour), ExpiresAt: time.Now().Add(-time.Minute),
	}
	data, _ := json.Marshal(live)
	path := filepath.Join(dir, "production-run.lock")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := acquireProductionRunLease(stateDir, time.Minute); !errors.Is(err, ErrRunInProgress) {
		t.Fatalf("expired live owner was stolen: %v", err)
	}
	persisted, err := os.ReadFile(path)
	if err != nil || string(persisted) != string(data) {
		t.Fatalf("live lease was modified: data=%q err=%v", persisted, err)
	}
}
