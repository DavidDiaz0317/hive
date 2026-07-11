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
	expired := productionRunLease{
		SchemaVersion: "hive.production-run-lease.v1", ID: "expired", PID: 42,
		StartedAt: time.Now().Add(-time.Hour), ExpiresAt: time.Now().Add(-time.Minute),
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
