package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	hivegithub "github.com/kubestellar/hive/v2/pkg/github"
	"github.com/kubestellar/hive/v2/pkg/integrated"
	"github.com/kubestellar/hive/v2/pkg/repair"
)

const daemonStatusSchema = "hive.integrated-daemon.v1"
const daemonLeaseSchema = "hive.integrated-daemon-lease.v1"

var errDaemonLeaseHeld = errors.New("hive scheduler lease is already held")

type integratedDaemonLease struct {
	SchemaVersion string    `json:"schema_version"`
	PID           int       `json:"pid"`
	Executable    string    `json:"executable"`
	AcquiredAt    time.Time `json:"acquired_at"`
}

type integratedDaemonStatus struct {
	SchemaVersion   string    `json:"schema_version"`
	PID             int       `json:"pid"`
	Running         bool      `json:"running"`
	Repository      string    `json:"repository,omitempty"`
	StateDir        string    `json:"state_dir"`
	Executable      string    `json:"executable,omitempty"`
	StartedAt       time.Time `json:"started_at,omitempty"`
	StoppedAt       time.Time `json:"stopped_at,omitempty"`
	IntervalSeconds int64     `json:"interval_seconds"`
	LastAttemptAt   time.Time `json:"last_attempt_at,omitempty"`
	LastSuccessAt   time.Time `json:"last_success_at,omitempty"`
	LastRunURL      string    `json:"last_run_url,omitempty"`
	LastError       string    `json:"last_error,omitempty"`
	NextRunAt       time.Time `json:"next_run_at,omitempty"`
}

var daemonStatusMu sync.Mutex

func runIntegratedStart(args []string) int {
	flags := flag.NewFlagSet("hive start", flag.ContinueOnError)
	stateDir := flags.String("state-dir", defaultIntegratedStateDir(), "persistent Hive state directory")
	interval := flags.Duration("interval", 15*time.Minute, "production scan interval")
	jsonOutput := flags.Bool("json", false, "emit machine-readable JSON")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	status, err := ensureIntegratedDaemonStarted(*stateDir, *interval)
	if err != nil {
		fmt.Fprintln(os.Stderr, "start Hive:", err)
		return 1
	}
	if *jsonOutput {
		return encodeJSON(status)
	}
	fmt.Printf("Hive scheduler is running for %s (pid=%d, interval=%s).\n", status.Repository, status.PID, time.Duration(status.IntervalSeconds)*time.Second)
	return 0
}

func ensureIntegratedDaemonStarted(stateDir string, interval time.Duration) (integratedDaemonStatus, error) {
	if interval < time.Minute || interval > 24*time.Hour {
		return integratedDaemonStatus{}, fmt.Errorf("scan interval must be between 1 minute and 24 hours")
	}
	stateDir, err := filepath.Abs(stateDir)
	if err != nil {
		return integratedDaemonStatus{}, err
	}
	store, err := integrated.NewStore(filepath.Join(stateDir, "integrated"))
	if err != nil {
		return integratedDaemonStatus{}, err
	}
	config, err := store.Load()
	if err != nil {
		return integratedDaemonStatus{}, fmt.Errorf("complete hive setup before start: %w", err)
	}
	if status := readIntegratedDaemonStatus(stateDir); status.Running {
		return status, nil
	}
	executable, err := os.Executable()
	if err != nil {
		return integratedDaemonStatus{}, err
	}
	logPath := filepath.Join(stateDir, "integrated", "daemon.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return integratedDaemonStatus{}, err
	}
	command := exec.Command(executable, "daemon", "--state-dir", stateDir, "--interval", interval.String())
	command.Stdin = nil
	command.Stdout, command.Stderr = logFile, logFile
	command.Env = daemonEnvironment(os.Environ())
	configureDetachedProcess(command)
	if err := command.Start(); err != nil {
		_ = logFile.Close()
		return integratedDaemonStatus{}, err
	}
	pid := command.Process.Pid
	_ = command.Process.Release()
	_ = logFile.Close()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		status := readIntegratedDaemonStatus(stateDir)
		if status.Running {
			store.Audit(integrated.AuditEntry{Action: "daemon_start", Allowed: true, Repository: config.Repository, Detail: fmt.Sprintf("pid=%d interval=%s", status.PID, time.Duration(status.IntervalSeconds)*time.Second)})
			return status, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return integratedDaemonStatus{}, fmt.Errorf("scheduler process %d did not claim persistent state; inspect %s", pid, logPath)
}

func runIntegratedStop(args []string) int {
	flags := flag.NewFlagSet("hive stop", flag.ContinueOnError)
	stateDir := flags.String("state-dir", defaultIntegratedStateDir(), "persistent Hive state directory")
	jsonOutput := flags.Bool("json", false, "emit machine-readable JSON")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	status, err := stopIntegratedDaemon(*stateDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "stop Hive:", err)
		return 1
	}
	if *jsonOutput {
		return encodeJSON(status)
	}
	fmt.Println("Hive scheduler is stopped; persistent lifecycle state is preserved.")
	return 0
}

func stopIntegratedDaemon(stateDir string) (integratedDaemonStatus, error) {
	status := readIntegratedDaemonStatus(stateDir)
	if status.Running {
		if err := terminateProcess(status.PID); err != nil && processIsAlive(status.PID) {
			return status, err
		}
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) && processIsAlive(status.PID) {
			time.Sleep(100 * time.Millisecond)
		}
		if processIsAlive(status.PID) {
			return status, fmt.Errorf("scheduler did not exit within 10 seconds")
		}
	}
	status = readIntegratedDaemonStatus(stateDir)
	status.Running = false
	if status.StoppedAt.IsZero() {
		status.StoppedAt = time.Now().UTC()
	}
	_ = writeIntegratedDaemonStatus(stateDir, status)
	if store, err := integrated.NewStore(filepath.Join(stateDir, "integrated")); err == nil {
		if config, loadErr := store.Load(); loadErr == nil {
			store.Audit(integrated.AuditEntry{Action: "daemon_stop", Allowed: true, Repository: config.Repository, Detail: fmt.Sprintf("pid=%d", status.PID)})
		}
	}
	return status, nil
}

func runIntegratedDaemon(args []string) int {
	flags := flag.NewFlagSet("hive daemon", flag.ContinueOnError)
	stateDir := flags.String("state-dir", defaultIntegratedStateDir(), "persistent Hive state directory")
	interval := flags.Duration("interval", 15*time.Minute, "production scan interval")
	runTimeout := flags.Duration("run-timeout", 45*time.Minute, "maximum production run duration")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *interval < time.Minute || *interval > 24*time.Hour {
		fmt.Fprintln(os.Stderr, "scan interval must be between 1 minute and 24 hours")
		return 2
	}
	stateDirAbs, err := filepath.Abs(*stateDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	store, err := integrated.NewStore(filepath.Join(stateDirAbs, "integrated"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	config, err := store.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	lease, err := claimDaemonLease(stateDirAbs)
	if errors.Is(err, errDaemonLeaseHeld) {
		// Concurrent and repeated starts are idempotent. The caller that spawned
		// this process will observe the scheduler which won the lease.
		return 0
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer releaseDaemonLease(lease)
	started := time.Now().UTC()
	executable, _ := os.Executable()
	status := integratedDaemonStatus{SchemaVersion: daemonStatusSchema, PID: os.Getpid(), Running: true, Repository: config.Repository, StateDir: stateDirAbs, Executable: executable, StartedAt: started, IntervalSeconds: int64(interval.Seconds())}
	if err := writeIntegratedDaemonStatus(stateDirAbs, status); err != nil {
		fmt.Fprintln(os.Stderr, "write scheduler status:", err)
		return 1
	}
	store.Audit(integrated.AuditEntry{Action: "daemon_claim", Allowed: true, Repository: config.Repository, Detail: fmt.Sprintf("pid=%d", os.Getpid())})
	defer func() {
		status.Running = false
		status.StoppedAt = time.Now().UTC()
		status.NextRunAt = time.Time{}
		_ = writeIntegratedDaemonStatus(stateDirAbs, status)
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	for {
		status.LastAttemptAt = time.Now().UTC()
		status.LastError = ""
		_ = writeIntegratedDaemonStatus(stateDirAbs, status)
		cycleCtx, cancel := context.WithTimeout(ctx, *runTimeout+time.Minute)
		result, cycleErr := runIntegratedDaemonCycle(cycleCtx, stateDirAbs, *runTimeout)
		cancel()
		now := time.Now().UTC()
		wait := *interval
		if cycleErr != nil {
			if errors.Is(cycleErr, integrated.ErrRunInProgress) {
				fmt.Printf("%s scheduler run skipped because another production run owns the repository lease\n", now.Format(time.RFC3339))
			} else {
				status.LastError = sanitizeDaemonError(cycleErr)
				if wait > time.Minute {
					wait = time.Minute
				}
				fmt.Fprintf(os.Stderr, "%s scheduler run failed: %s\n", now.Format(time.RFC3339), status.LastError)
			}
		} else {
			status.LastSuccessAt = now
			status.LastRunURL = result.Workflow.RunURL
			fmt.Printf("%s scheduler run succeeded: %s\n", now.Format(time.RFC3339), result.Workflow.RunURL)
		}
		status.NextRunAt = now.Add(wait)
		_ = writeIntegratedDaemonStatus(stateDirAbs, status)
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return 0
		case <-timer.C:
		}
	}
}

func runIntegratedDaemonCycle(ctx context.Context, stateDir string, timeout time.Duration) (integrated.RunResult, error) {
	token := resolveGitHubToken("HIVE_GITHUB_TOKEN")
	if token == "" {
		return integrated.RunResult{}, fmt.Errorf("GitHub authorization is unavailable; run gh auth login")
	}
	client := hivegithub.NewClient(token, "", nil, slog.New(slog.NewTextHandler(io.Discard, nil)), "")
	store, err := integrated.NewStore(filepath.Join(stateDir, "integrated"))
	if err != nil {
		return integrated.RunResult{}, err
	}
	config, err := store.Load()
	if err != nil {
		return integrated.RunResult{}, err
	}
	if config.Paused {
		return integrated.RunResult{}, fmt.Errorf("repository automation is paused")
	}
	if ok, message := validateVisualHiveLauncher(config); !ok {
		return integrated.RunResult{}, fmt.Errorf("Visual Hive runtime is not ready: %s", message)
	}
	providerCtx, providerCancel := context.WithTimeout(ctx, 45*time.Second)
	providerErr := (repair.CodexProvider{Command: config.ProviderCommand, Prefix: config.ProviderArgs}).Health(providerCtx)
	providerCancel()
	if providerErr != nil {
		return integrated.RunResult{}, providerErr
	}
	for _, check := range liveRepositoryChecks(ctx, client, config) {
		if check.OK {
			continue
		}
		if check.Name == "branch_protection" && config.Automation == integrated.AutomationAutoMerge {
			pending, pendingErr := daemonCanRunProtectionActivation(ctx, client, config)
			if pendingErr != nil {
				return integrated.RunResult{}, fmt.Errorf("readiness check %s failed: %w", check.Name, pendingErr)
			}
			if pending {
				// RunOnce is the only writer allowed to activate protection. Doctor
				// remains strict, but the scheduler must be able to enter that exact
				// two-phase transition after the managed setup PR is installed.
				continue
			}
		}
		return integrated.RunResult{}, fmt.Errorf("readiness check %s failed: %s", check.Name, check.Message)
	}
	return integrated.RunOnce(ctx, integrated.RunOptions{StateDir: stateDir, Timeout: timeout, GitHub: client})
}

func daemonCanRunProtectionActivation(ctx context.Context, client *hivegithub.Client, config integrated.Config) (bool, error) {
	if client == nil || config.Automation != integrated.AutomationAutoMerge {
		return false, nil
	}
	protection, err := client.BranchProtection(ctx, config.Repository, config.DefaultBranch)
	if err != nil {
		return false, fmt.Errorf("inspect pending protection activation: %w", err)
	}
	expectedAppID, err := client.ExpectedGitHubAppID(ctx, "github-actions")
	if err != nil {
		return false, fmt.Errorf("resolve pending protection activation producer: %w", err)
	}
	activation, exists, err := integrated.ReadProtectionActivation(config.StateDir)
	if err != nil {
		return false, fmt.Errorf("read pending protection activation state: %w", err)
	}
	activationDurable := exists && integrated.ProtectionActivationMatchesConfig(activation, config) && activation.CheckAppID == expectedAppID
	return daemonProtectionActivationRunnable(config.Automation, protection, expectedAppID, activationDurable), nil
}

func daemonProtectionActivationRunnable(automation integrated.Automation, protection hivegithub.BranchProtectionSummary, expectedAppID int64, activationDurable bool) bool {
	if automation != integrated.AutomationAutoMerge || expectedAppID <= 0 {
		return false
	}
	if !protection.Enabled {
		// No repository-owned policy exists, so RunOnce may perform the trusted
		// scan and create Hive's conservative minimum protection.
		return true
	}
	if !protection.Strict || !protection.AdminEnforced {
		return false
	}
	visualRequired := false
	for _, check := range protection.RequiredCheckIdentities {
		if strings.EqualFold(strings.TrimSpace(check.Context), "visual-hive") && check.AppID == expectedAppID {
			visualRequired = true
			break
		}
	}
	// Existing policy is never modified. It is safe to enter RunOnce only when
	// that policy is already exact and the remaining work is the trusted-run
	// activation checkpoint (including a new digest after an upgrade).
	return visualRequired && !activationDurable
}

func claimDaemonLease(stateDir string) (*os.File, error) {
	dir := filepath.Join(stateDir, "integrated")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "daemon.lease")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open Hive scheduler lease: %w", err)
	}
	locked, err := tryLockDaemonLease(file)
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("claim Hive scheduler lease: %w", err)
	}
	if !locked {
		_ = file.Close()
		return nil, errDaemonLeaseHeld
	}
	executable, err := os.Executable()
	if err != nil {
		releaseDaemonLease(file)
		return nil, err
	}
	lease := integratedDaemonLease{SchemaVersion: daemonLeaseSchema, PID: os.Getpid(), Executable: executable, AcquiredAt: time.Now().UTC()}
	data, err := json.MarshalIndent(lease, "", "  ")
	if err != nil {
		releaseDaemonLease(file)
		return nil, err
	}
	if err := file.Truncate(0); err == nil {
		_, err = file.Seek(0, 0)
	}
	if err == nil {
		_, err = file.Write(append(data, '\n'))
	}
	if err == nil {
		err = file.Sync()
	}
	if err != nil {
		releaseDaemonLease(file)
		return nil, fmt.Errorf("persist Hive scheduler lease: %w", err)
	}
	// These files are from the pre-lease implementation. They are never
	// consulted after the lease is held and are safe to clean up here.
	_ = os.Remove(filepath.Join(dir, "daemon.pid"))
	_ = os.Remove(filepath.Join(dir, "daemon.starting"))
	return file, nil
}

func releaseDaemonLease(file *os.File) {
	if file == nil {
		return
	}
	_ = unlockDaemonLease(file)
	_ = file.Close()
}

func readIntegratedDaemonLease(stateDir string) (integratedDaemonLease, bool) {
	path := filepath.Join(stateDir, "integrated", "daemon.lease")
	for attempt := 0; attempt < 5; attempt++ {
		data, err := os.ReadFile(path)
		if err != nil {
			return integratedDaemonLease{}, false
		}
		var lease integratedDaemonLease
		if json.Unmarshal(data, &lease) == nil && lease.SchemaVersion == daemonLeaseSchema && lease.PID > 0 && lease.Executable != "" {
			file, openErr := os.OpenFile(path, os.O_RDWR, 0o600)
			if openErr != nil {
				return integratedDaemonLease{}, false
			}
			locked, lockErr := tryReadDaemonLease(file)
			if locked {
				_ = unlockDaemonLease(file)
			}
			_ = file.Close()
			if lockErr != nil || locked {
				return lease, false
			}
			// The record is immutable while its owner holds the lock. Confirm
			// that ownership did not change between the read and lock probe.
			current, readErr := os.ReadFile(path)
			if readErr == nil && string(current) == string(data) {
				return lease, true
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	return integratedDaemonLease{}, false
}

func readIntegratedDaemonStatus(stateDir string) integratedDaemonStatus {
	stateDir, _ = filepath.Abs(stateDir)
	leasePath := filepath.Join(stateDir, "integrated", "daemon.lease")
	_, leaseErr := os.Stat(leasePath)
	leasePresent := leaseErr == nil || !errors.Is(leaseErr, os.ErrNotExist)
	lease, leaseHeld := integratedDaemonLease{}, false
	if leaseErr == nil {
		lease, leaseHeld = readIntegratedDaemonLease(stateDir)
	}
	leaseStatus := func(lastError string) integratedDaemonStatus {
		status := integratedDaemonStatus{SchemaVersion: daemonStatusSchema, StateDir: stateDir, LastError: lastError}
		if leaseHeld && processIsAlive(lease.PID) {
			status.PID = lease.PID
			status.Executable = lease.Executable
			status.StartedAt = lease.AcquiredAt
			status.Running = true
		}
		return status
	}
	data, err := os.ReadFile(filepath.Join(stateDir, "integrated", "daemon.json"))
	if err != nil {
		return leaseStatus("")
	}
	var status integratedDaemonStatus
	if json.Unmarshal(data, &status) != nil || status.SchemaVersion != daemonStatusSchema {
		return leaseStatus("daemon status is invalid")
	}
	if leasePresent {
		// The live OS lock is the ownership proof. The executable on disk may
		// legitimately have been atomically replaced by an installer while the
		// old daemon keeps running from a deleted/backup inode.
		status.Running = leaseHeld && lease.PID == status.PID && processIsAlive(status.PID)
	} else {
		// Legacy status without a lease needs the stronger executable check to
		// avoid treating a reused PID as Hive.
		status.Running = status.PID > 0 && status.Executable != "" && processIsAlive(status.PID) && processMatchesExecutable(status.PID, status.Executable)
	}
	return status
}

func writeIntegratedDaemonStatus(stateDir string, status integratedDaemonStatus) error {
	daemonStatusMu.Lock()
	defer daemonStatusMu.Unlock()
	status.SchemaVersion = daemonStatusSchema
	dir := filepath.Join(stateDir, "integrated")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(status, "", "  ")
	if err != nil {
		return err
	}
	temporary := filepath.Join(dir, fmt.Sprintf(".daemon-%d.tmp", os.Getpid()))
	if err := os.WriteFile(temporary, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, filepath.Join(dir, "daemon.json"))
}

func daemonEnvironment(environment []string) []string {
	denied := map[string]bool{"HIVE_GITHUB_TOKEN": true, "GH_TOKEN": true, "GITHUB_TOKEN": true}
	result := make([]string, 0, len(environment))
	for _, entry := range environment {
		name, _, _ := strings.Cut(entry, "=")
		if !denied[strings.ToUpper(name)] {
			result = append(result, entry)
		}
	}
	return result
}

func sanitizeDaemonError(err error) string {
	if err == nil {
		return ""
	}
	message := strings.ReplaceAll(err.Error(), "\r", " ")
	message = strings.ReplaceAll(message, "\n", " ")
	if len(message) > 1000 {
		message = message[:1000]
	}
	return message
}
