package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	hivegithub "github.com/kubestellar/hive/v2/pkg/github"
	"github.com/kubestellar/hive/v2/pkg/integrated"
)

const daemonStatusSchema = "hive.integrated-daemon.v1"

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
	startingPath := filepath.Join(stateDir, "integrated", "daemon.starting")
	starting, err := os.OpenFile(startingPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return integratedDaemonStatus{}, fmt.Errorf("another Hive start is in progress")
	}
	_ = starting.Close()
	defer os.Remove(startingPath)
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
		if status.Running && status.PID == pid {
			store.Audit(integrated.AuditEntry{Action: "daemon_start", Allowed: true, Repository: config.Repository, Detail: fmt.Sprintf("pid=%d interval=%s", pid, interval)})
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
	status := readIntegratedDaemonStatus(*stateDir)
	if status.Running {
		if err := terminateProcess(status.PID); err != nil {
			fmt.Fprintln(os.Stderr, "stop Hive:", err)
			return 1
		}
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) && processIsAlive(status.PID) {
			time.Sleep(100 * time.Millisecond)
		}
		if processIsAlive(status.PID) {
			fmt.Fprintln(os.Stderr, "stop Hive: scheduler did not exit within 10 seconds")
			return 1
		}
	}
	status = readIntegratedDaemonStatus(*stateDir)
	status.Running = false
	if status.StoppedAt.IsZero() {
		status.StoppedAt = time.Now().UTC()
	}
	_ = writeIntegratedDaemonStatus(*stateDir, status)
	if store, err := integrated.NewStore(filepath.Join(*stateDir, "integrated")); err == nil {
		if config, loadErr := store.Load(); loadErr == nil {
			store.Audit(integrated.AuditEntry{Action: "daemon_stop", Allowed: true, Repository: config.Repository, Detail: fmt.Sprintf("pid=%d", status.PID)})
		}
	}
	if *jsonOutput {
		return encodeJSON(status)
	}
	fmt.Println("Hive scheduler is stopped; persistent lifecycle state is preserved.")
	return 0
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
	if err := claimDaemonPID(stateDirAbs); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	started := time.Now().UTC()
	executable, _ := os.Executable()
	status := integratedDaemonStatus{SchemaVersion: daemonStatusSchema, PID: os.Getpid(), Running: true, Repository: config.Repository, StateDir: stateDirAbs, Executable: executable, StartedAt: started, IntervalSeconds: int64(interval.Seconds())}
	_ = writeIntegratedDaemonStatus(stateDirAbs, status)
	store.Audit(integrated.AuditEntry{Action: "daemon_claim", Allowed: true, Repository: config.Repository, Detail: fmt.Sprintf("pid=%d", os.Getpid())})
	defer func() {
		status.Running = false
		status.StoppedAt = time.Now().UTC()
		status.NextRunAt = time.Time{}
		_ = writeIntegratedDaemonStatus(stateDirAbs, status)
		releaseDaemonPID(stateDirAbs, os.Getpid())
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
			status.LastError = sanitizeDaemonError(cycleErr)
			if wait > time.Minute {
				wait = time.Minute
			}
			fmt.Fprintf(os.Stderr, "%s scheduler run failed: %s\n", now.Format(time.RFC3339), status.LastError)
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
	return integrated.RunOnce(ctx, integrated.RunOptions{StateDir: stateDir, Timeout: timeout, GitHub: client})
}

func claimDaemonPID(stateDir string) error {
	path := filepath.Join(stateDir, "integrated", "daemon.pid")
	if data, err := os.ReadFile(path); err == nil {
		pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
		status := readIntegratedDaemonStatus(stateDir)
		if pid > 0 && status.Running && status.PID == pid {
			return fmt.Errorf("Hive scheduler is already running with pid %d", pid)
		}
		_ = os.Remove(path)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("claim Hive scheduler state: %w", err)
	}
	if _, err := fmt.Fprintf(file, "%d\n", os.Getpid()); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func releaseDaemonPID(stateDir string, pid int) {
	path := filepath.Join(stateDir, "integrated", "daemon.pid")
	data, err := os.ReadFile(path)
	if err == nil && strings.TrimSpace(string(data)) == strconv.Itoa(pid) {
		_ = os.Remove(path)
	}
}

func readIntegratedDaemonStatus(stateDir string) integratedDaemonStatus {
	stateDir, _ = filepath.Abs(stateDir)
	data, err := os.ReadFile(filepath.Join(stateDir, "integrated", "daemon.json"))
	if err != nil {
		return integratedDaemonStatus{SchemaVersion: daemonStatusSchema, StateDir: stateDir, Running: false}
	}
	var status integratedDaemonStatus
	if json.Unmarshal(data, &status) != nil || status.SchemaVersion != daemonStatusSchema {
		return integratedDaemonStatus{SchemaVersion: daemonStatusSchema, StateDir: stateDir, Running: false, LastError: "daemon status is invalid"}
	}
	status.Running = status.PID > 0 && status.Executable != "" && processIsAlive(status.PID) && processMatchesExecutable(status.PID, status.Executable)
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
