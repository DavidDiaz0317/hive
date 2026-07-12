package integrated

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"time"

	"github.com/kubestellar/hive/v2/pkg/automation"
	hivegithub "github.com/kubestellar/hive/v2/pkg/github"
)

type ManagementOperation string

const (
	OperationUpgrade   ManagementOperation = "upgrade"
	OperationRollback  ManagementOperation = "rollback"
	OperationUninstall ManagementOperation = "uninstall"
)

type ManagementOptions struct {
	Operation         ManagementOperation
	StateDir          string
	VisualHiveRef     string
	VisualHiveCommand string
	VisualHiveArgs    []string
	DeleteState       bool
	GitHub            *hivegithub.Client
}

type ManagementResult struct {
	SchemaVersion string              `json:"schema_version"`
	Operation     ManagementOperation `json:"operation"`
	Repository    string              `json:"repository"`
	Branch        string              `json:"branch"`
	CommitSHA     string              `json:"commit_sha"`
	PRNumber      int                 `json:"pr_number"`
	PRURL         string              `json:"pr_url"`
	PreviousRef   string              `json:"previous_ref,omitempty"`
	RequestedRef  string              `json:"requested_ref,omitempty"`
	Idempotent    bool                `json:"idempotent"`
	StateDeleted  bool                `json:"state_deleted"`
}

func RunManagement(ctx context.Context, options ManagementOptions) (ManagementResult, error) {
	result := ManagementResult{SchemaVersion: "hive.management.v1", Operation: options.Operation}
	if options.GitHub == nil || strings.TrimSpace(options.StateDir) == "" {
		return result, fmt.Errorf("GitHub client and persistent state directory are required")
	}
	if options.Operation != OperationUpgrade && options.Operation != OperationRollback && options.Operation != OperationUninstall {
		return result, fmt.Errorf("unsupported management operation %q", options.Operation)
	}
	release, leaseErr := acquireProductionRunLease(options.StateDir, 25*time.Minute)
	if leaseErr != nil {
		return result, fmt.Errorf("serialize %s with production runs: %w", options.Operation, leaseErr)
	}
	defer release()
	store, err := NewStore(filepath.Join(options.StateDir, "integrated"))
	if err != nil {
		return result, err
	}
	config, err := store.Load()
	if err != nil {
		return result, err
	}
	if _, err := verifyLiveRepositoryIdentity(ctx, options.GitHub, config); err != nil {
		return result, err
	}
	result.Repository, result.PreviousRef = config.Repository, config.VisualHiveRef
	policy := automation.Policy{ACMMLevel: config.ACMMLevel, Mode: automationMode(config.Automation), AllowedRepositories: []string{config.Repository}}
	if options.Operation == OperationUpgrade || options.Operation == OperationRollback {
		requested := strings.ToLower(strings.TrimSpace(options.VisualHiveRef))
		if requested == "" && options.Operation == OperationRollback {
			requested = strings.ToLower(strings.TrimSpace(config.PreviousVersion))
		}
		if !regexp.MustCompile(`^[a-f0-9]{40}$`).MatchString(requested) {
			return result, fmt.Errorf("%s requires an immutable 40-character Visual Hive commit SHA", options.Operation)
		}
		result.RequestedRef, options.VisualHiveRef = requested, requested
		if requested == config.VisualHiveRef {
			// Local state is not proof that a prior upgrade PR reached the
			// default branch. Only report a no-op when every managed production
			// file at the target branch still matches this exact pin and policy.
			if installedErr := VerifyInstalledSetup(ctx, options.GitHub, config); installedErr == nil {
				if strings.TrimSpace(options.VisualHiveCommand) != "" && (config.VisualHiveCommand != options.VisualHiveCommand || !reflect.DeepEqual(config.VisualHiveArgs, options.VisualHiveArgs)) {
					config.VisualHiveCommand = options.VisualHiveCommand
					config.VisualHiveArgs = append([]string(nil), options.VisualHiveArgs...)
					if err := store.Save(config); err != nil {
						return result, err
					}
				}
				result.Idempotent = true
				return result, nil
			}
		}
		if err := VerifyVisualHiveCommit(ctx, options.GitHub, config.VisualHiveRepo, requested); err != nil {
			return result, err
		}
	}
	branch := managedOperationBranch(string(options.Operation), config.RepositoryID)
	if err := authorizeSetup(store, policy, config.Repository, automation.ActionSetupBranch); err != nil {
		return result, err
	}
	defaultBranch, err := ensureCheckout(ctx, config.Repository, config.CheckoutDir)
	if err != nil {
		return result, err
	}
	if _, err := git(ctx, config.CheckoutDir, "switch", "-C", branch, "origin/"+defaultBranch); err != nil {
		return result, err
	}
	managed := managedSetupFiles(config.VisualHive)
	title := ""
	marker := fmt.Sprintf("<!-- hive-%s: %s -->", options.Operation, strings.ToLower(config.Repository))
	candidate := config
	switch options.Operation {
	case OperationUpgrade, OperationRollback:
		requested := options.VisualHiveRef
		if !strings.EqualFold(requested, config.VisualHiveRef) {
			candidate.PreviousVersion = config.VisualHiveRef
		}
		candidate.VisualHiveRef, candidate.UpdatedAt = requested, time.Now().UTC()
		if strings.TrimSpace(options.VisualHiveCommand) != "" {
			candidate.VisualHiveCommand = options.VisualHiveCommand
			candidate.VisualHiveArgs = append([]string(nil), options.VisualHiveArgs...)
		}
		inspection, inspectErr := InspectCheckout(config.CheckoutDir, defaultBranch)
		if inspectErr != nil {
			return result, inspectErr
		}
		if err := writeManagedFiles(config.CheckoutDir, candidate, inspection); err != nil {
			return result, err
		}
		title = fmt.Sprintf("%s Visual Hive to %s", titleCaseOperation(options.Operation), requested[:12])
	case OperationUninstall:
		candidate.Paused = true
		for _, relative := range managed {
			if err := os.Remove(filepath.Join(config.CheckoutDir, filepath.FromSlash(relative))); err != nil && !os.IsNotExist(err) {
				return result, err
			}
		}
		title = "Uninstall Hive production automation"
	}
	if err := authorizeSetup(store, policy, config.Repository, automation.ActionSetupCommit); err != nil {
		return result, err
	}
	if err := stageManagedPaths(ctx, config.CheckoutDir, managed); err != nil {
		return result, err
	}
	changed, err := git(ctx, config.CheckoutDir, "diff", "--cached", "--name-only")
	if err != nil {
		return result, err
	}
	result.Idempotent = strings.TrimSpace(changed) == ""
	if !result.Idempotent {
		message := "chore: " + string(options.Operation) + " Hive integration"
		if _, err := git(ctx, config.CheckoutDir, "-c", "user.name=Hive Setup", "-c", "user.email=hive-setup@users.noreply.github.com", "commit", "-m", message, "-m", managedCommitTrailers(config.RepositoryID, string(options.Operation))); err != nil {
			return result, err
		}
	}
	sha, err := git(ctx, config.CheckoutDir, "rev-parse", "HEAD")
	if err != nil {
		return result, err
	}
	result.CommitSHA, result.Branch = strings.TrimSpace(sha), branch
	if err := authorizeSetup(store, policy, config.Repository, automation.ActionSetupPush); err != nil {
		return result, err
	}
	if err := pushManagedBranch(ctx, config.CheckoutDir, branch, config.RepositoryID, string(options.Operation), result.CommitSHA); err != nil {
		return result, err
	}
	if err := authorizeSetup(store, policy, config.Repository, automation.ActionSetupPR); err != nil {
		return result, err
	}
	body := fmt.Sprintf("%s\n\nHive-managed `%s` operation.\n\n- Previous Visual Hive ref: `%s`\n- Requested Visual Hive ref: `%s`\n\nThis PR is intentionally reviewable and idempotent. Hive remains paused immediately for uninstall; upgrade and rollback take effect after this PR is merged.", marker, options.Operation, config.VisualHiveRef, result.RequestedRef)
	pull, err := options.GitHub.UpsertRepairPullRequest(ctx, config.Repository, branch, result.CommitSHA, defaultBranch, title, body, marker)
	if err != nil {
		return result, err
	}
	if err := verifyManagedPullHead(string(options.Operation), pull, result.CommitSHA); err != nil {
		return result, err
	}
	result.PRNumber, result.PRURL = pull.Number, pull.URL
	candidate.SetupBranch, candidate.SetupPRNumber, candidate.SetupPRURL = branch, pull.Number, pull.URL
	if options.Operation == OperationUninstall {
		config.Paused = true
		if err := store.Save(config); err != nil {
			return result, err
		}
		if err := store.AuditStrict(AuditEntry{Action: "uninstall", Allowed: true, Repository: config.Repository, Detail: result.PRURL}); err != nil {
			return result, err
		}
		if options.DeleteState {
			if err := deleteManagedState(options.StateDir); err != nil {
				return result, err
			}
			result.StateDeleted = true
		}
	} else {
		if err := store.Save(candidate); err != nil {
			return result, err
		}
		if err := store.AuditStrict(AuditEntry{Action: string(options.Operation), Allowed: true, Repository: config.Repository, Detail: result.PRURL}); err != nil {
			return result, err
		}
	}
	return result, nil
}

func titleCaseOperation(operation ManagementOperation) string {
	value := string(operation)
	if value == "" {
		return value
	}
	return strings.ToUpper(value[:1]) + value[1:]
}

func deleteManagedState(stateDir string) error {
	absolute, err := filepath.Abs(stateDir)
	if err != nil {
		return err
	}
	absolute = filepath.Clean(absolute)
	volume := filepath.VolumeName(absolute) + string(os.PathSeparator)
	home, _ := os.UserHomeDir()
	if absolute == volume || absolute == filepath.Clean(home) || len(filepath.SplitList(absolute)) == 0 {
		return fmt.Errorf("refusing to delete unsafe state path %s", absolute)
	}
	marker := filepath.Join(absolute, "integrated", "config.json")
	if info, statErr := os.Stat(marker); statErr != nil || info.IsDir() {
		return fmt.Errorf("refusing to delete %s without a managed Hive config marker", absolute)
	}
	return os.RemoveAll(absolute)
}
