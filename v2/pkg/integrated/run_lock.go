package integrated

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

var ErrRunInProgress = errors.New("a Hive production run is already in progress")

type productionRunLease struct {
	SchemaVersion string    `json:"schema_version"`
	ID            string    `json:"id"`
	PID           int       `json:"pid"`
	StartedAt     time.Time `json:"started_at"`
	ExpiresAt     time.Time `json:"expires_at"`
}

func acquireProductionRunLease(stateDir string, duration time.Duration) (func(), error) {
	if duration <= 0 {
		duration = 47 * time.Minute
	}
	dir := filepath.Join(stateDir, "integrated")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "production-run.lock")
	now := time.Now().UTC()
	lease := productionRunLease{
		SchemaVersion: "hive.production-run-lease.v1",
		ID:            fmt.Sprintf("%d-%d", os.Getpid(), now.UnixNano()),
		PID:           os.Getpid(),
		StartedAt:     now,
		ExpiresAt:     now.Add(duration),
	}
	payload, err := json.Marshal(lease)
	if err != nil {
		return nil, err
	}

	for attempt := 0; attempt < 2; attempt++ {
		file, createErr := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if createErr == nil {
			if _, err := file.Write(append(payload, '\n')); err != nil {
				_ = file.Close()
				_ = os.Remove(path)
				return nil, err
			}
			if err := file.Sync(); err != nil {
				_ = file.Close()
				_ = os.Remove(path)
				return nil, err
			}
			if err := file.Close(); err != nil {
				_ = os.Remove(path)
				return nil, err
			}
			return func() { releaseProductionRunLease(path, lease.ID) }, nil
		}
		if !errors.Is(createErr, os.ErrExist) {
			return nil, fmt.Errorf("claim Hive production run: %w", createErr)
		}

		existingData, readErr := os.ReadFile(path)
		var existing productionRunLease
		if readErr == nil && json.Unmarshal(existingData, &existing) == nil && existing.SchemaVersion == "hive.production-run-lease.v1" {
			// ExpiresAt bounds stale-file recovery only. It must never authorize
			// stealing a lease from a live process: a long production run may
			// legitimately outlive the originally estimated duration.
			if productionLeaseOwnerAlive(existing.PID) {
				return nil, fmt.Errorf("%w (pid=%d, started=%s, expires=%s)", ErrRunInProgress, existing.PID, existing.StartedAt.Format(time.RFC3339), existing.ExpiresAt.Format(time.RFC3339))
			}
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return nil, fmt.Errorf("remove expired Hive production run lease: %w", err)
			}
			continue
		}

		info, statErr := os.Stat(path)
		if statErr == nil && time.Since(info.ModTime()) < time.Minute {
			return nil, fmt.Errorf("%w (lease is being initialized)", ErrRunInProgress)
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("remove invalid stale Hive production run lease: %w", err)
		}
	}
	return nil, ErrRunInProgress
}

func releaseProductionRunLease(path, id string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var current productionRunLease
	if json.Unmarshal(data, &current) == nil && current.ID == id {
		_ = os.Remove(path)
	}
}
