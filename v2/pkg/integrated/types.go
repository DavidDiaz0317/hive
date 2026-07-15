package integrated

import (
	"encoding/json"
	"time"

	"github.com/kubestellar/hive/v2/pkg/automation"
)

const (
	PlanSchema                    = "hive.setup-plan.v1"
	ConfigSchema                  = "hive.integrated-config.v2"
	legacyConfigSchema            = "hive.integrated-config.v1"
	managedRepositoryConfigSchema = "hive.integrated-config.v1"
)

func supportedDurableConfigSchema(schema string) bool {
	return schema == ConfigSchema || schema == legacyConfigSchema
}

type Coverage string

const (
	CoverageEssential     Coverage = "essential"
	CoverageStandard      Coverage = "standard"
	CoverageComprehensive Coverage = "comprehensive"
	CoverageCustom        Coverage = "custom"
)

type Automation string

const (
	AutomationAdvisory  Automation = "advisory"
	AutomationIssues    Automation = "issues"
	AutomationRepairPR  Automation = "repair-pr"
	AutomationAutoMerge Automation = "auto-merge"
)

type RepositoryInspection struct {
	DefaultBranch    string            `json:"default_branch"`
	RepositoryID     string            `json:"repository_id,omitempty"`
	Languages        []string          `json:"languages"`
	Frameworks       []string          `json:"frameworks"`
	PackageManagers  []string          `json:"package_managers"`
	TestCommands     [][]string        `json:"test_commands"`
	CIFiles          []string          `json:"ci_files"`
	DeploymentFiles  []string          `json:"deployment_files"`
	BaselineFiles    []string          `json:"baseline_files"`
	HighRiskPaths    []string          `json:"high_risk_paths"`
	BranchProtection bool              `json:"branch_protection"`
	Permissions      map[string]bool   `json:"permissions"`
	Signals          map[string]string `json:"signals"`
	packageScripts   map[string]map[string]string
	packageRunners   map[string]string
}

type SetupPlan struct {
	SchemaVersion         string                `json:"schema_version"`
	GeneratedAt           time.Time             `json:"generated_at"`
	Repository            string                `json:"repository"`
	StateDir              string                `json:"state_dir"`
	Coverage              Coverage              `json:"coverage"`
	Automation            Automation            `json:"automation"`
	Provider              string                `json:"provider"`
	ACMMLevel             int                   `json:"acmm_level"`
	MaxActiveIssues       int                   `json:"max_active_issues"`
	MaxRepairAttempts     int                   `json:"max_repair_attempts"`
	AllowedAutoMergePaths []string              `json:"allowed_auto_merge_paths"`
	AllowedAutoMergeRisk  []automation.RiskTier `json:"allowed_auto_merge_risk"`
	VisualHive            bool                  `json:"visual_hive"`
	VisualHiveRepository  string                `json:"visual_hive_repository"`
	VisualHiveRef         string                `json:"visual_hive_ref"`
	Inspection            RepositoryInspection  `json:"inspection"`
	TestingLayers         []string              `json:"testing_layers"`
	FilesToManage         []string              `json:"files_to_manage"`
	RequiredActions       []string              `json:"required_actions"`
	Warnings              []string              `json:"warnings"`
	ReadOnly              bool                  `json:"read_only"`
}

// ManagedPathPreimage is the exact repository-owned state that preceded Hive
// management of one path. Content is kept only in Hive's durable local state;
// it is never copied into the managed repository configuration.
type ManagedPathPreimage struct {
	Existed       bool   `json:"existed"`
	GitMode       string `json:"git_mode,omitempty"`
	Content       []byte `json:"content,omitempty"`
	ContentSHA256 string `json:"content_sha256,omitempty"`
}

type Config struct {
	SchemaVersion                     string                         `json:"schema_version"`
	Repository                        string                         `json:"repository"`
	RepositoryID                      string                         `json:"repository_id,omitempty"`
	DefaultBranch                     string                         `json:"default_branch"`
	Coverage                          Coverage                       `json:"coverage"`
	Automation                        Automation                     `json:"automation"`
	Provider                          string                         `json:"provider"`
	ProviderCommand                   string                         `json:"provider_command"`
	ProviderArgs                      []string                       `json:"provider_args,omitempty"`
	ACMMLevel                         int                            `json:"acmm_level"`
	MaxActiveIssues                   int                            `json:"max_active_issues"`
	MaxRepairAttempts                 int                            `json:"max_repair_attempts"`
	VisualHive                        bool                           `json:"visual_hive"`
	VisualHiveRepo                    string                         `json:"visual_hive_repo"`
	VisualHiveRef                     string                         `json:"visual_hive_ref"`
	VisualHiveCommand                 string                         `json:"visual_hive_command"`
	VisualHiveArgs                    []string                       `json:"visual_hive_args,omitempty"`
	VisualHiveConfigDigest            string                         `json:"visual_hive_config_digest,omitempty"`
	ManagedPreimagesVersion           string                         `json:"managed_preimages_version,omitempty"`
	ManagedPathPreimages              map[string]ManagedPathPreimage `json:"managed_path_preimages,omitempty"`
	SetupBaselineRequired             bool                           `json:"setup_baseline_required,omitempty"`
	SetupBaselineContractDigest       string                         `json:"setup_baseline_contract_digest,omitempty"`
	SetupBaselineInitialDigest        string                         `json:"setup_baseline_initial_digest,omitempty"`
	SetupBaselineInitialCandidates    []SetupBaselineCandidate       `json:"setup_baseline_initial_candidates,omitempty"`
	TestCommands                      [][]string                     `json:"test_commands"`
	AllowedRepairPaths                []string                       `json:"allowed_repair_paths"`
	AllowedAutoMergePaths             []string                       `json:"allowed_auto_merge_paths"`
	AllowedAutoMergeRisk              []automation.RiskTier          `json:"allowed_auto_merge_risk"`
	CheckoutDir                       string                         `json:"checkout_dir"`
	StateDir                          string                         `json:"state_dir"`
	Paused                            bool                           `json:"paused"`
	SetupBranch                       string                         `json:"setup_branch,omitempty"`
	SetupPRNumber                     int                            `json:"setup_pr_number,omitempty"`
	SetupPRURL                        string                         `json:"setup_pr_url,omitempty"`
	SetupHeadSHA                      string                         `json:"setup_head_sha,omitempty"`
	SetupAuthorizationActorID         int64                          `json:"setup_authorization_actor_id,omitempty"`
	SetupAuthorizationPreviousActorID int64                          `json:"setup_authorization_previous_actor_id,omitempty"`
	InstalledAt                       time.Time                      `json:"installed_at"`
	UpdatedAt                         time.Time                      `json:"updated_at"`
	PreviousVersion                   string                         `json:"previous_version,omitempty"`
}

// MarshalJSON keeps repository preimage bytes out of CLI/status/API output.
// Store.Save deliberately uses a persistence alias so the same bytes remain
// available in the permission-bounded local state needed for uninstall.
func (config Config) MarshalJSON() ([]byte, error) {
	type publicConfig Config
	redacted := publicConfig(config)
	redacted.ManagedPathPreimages = redactManagedPathPreimages(config.ManagedPathPreimages)
	return json.Marshal(redacted)
}

func redactManagedPathPreimages(source map[string]ManagedPathPreimage) map[string]ManagedPathPreimage {
	if source == nil {
		return nil
	}
	redacted := make(map[string]ManagedPathPreimage, len(source))
	for relative, preimage := range source {
		preimage.Content = nil
		redacted[relative] = preimage
	}
	return redacted
}

type SetupResult struct {
	Plan                        SetupPlan `json:"plan"`
	Applied                     bool      `json:"applied"`
	Idempotent                  bool      `json:"idempotent"`
	ProtectionActivationPending bool      `json:"protection_activation_pending"`
	ActivationMessage           string    `json:"activation_message,omitempty"`
	Config                      *Config   `json:"config,omitempty"`
	Branch                      string    `json:"branch,omitempty"`
	CommitSHA                   string    `json:"commit_sha,omitempty"`
	PRNumber                    int       `json:"pr_number,omitempty"`
	PRURL                       string    `json:"pr_url,omitempty"`
	SetupAuthorizationContext   string    `json:"setup_authorization_context,omitempty"`
	SetupAuthorizationStatusID  int64     `json:"setup_authorization_status_id,omitempty"`
	SetupAuthorizationCreatorID int64     `json:"setup_authorization_creator_id,omitempty"`
	SetupAuthorizationReused    bool      `json:"setup_authorization_reused,omitempty"`
	SetupBaselinePending        bool      `json:"setup_baseline_pending,omitempty"`
	SetupBaselinePhase          string    `json:"setup_baseline_phase,omitempty"`
	SetupBaselineNextCommand    string    `json:"setup_baseline_next_command,omitempty"`
	SchedulerStartRequested     bool      `json:"scheduler_start_requested,omitempty"`
	SchedulerStartPending       bool      `json:"scheduler_start_pending,omitempty"`
	SchedulerStartMessage       string    `json:"scheduler_start_message,omitempty"`
}
