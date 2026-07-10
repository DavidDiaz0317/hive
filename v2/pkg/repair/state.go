package repair

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	StateSchemaV1 = "hive.repair-worker-state.v1"
	StateSchema   = "hive.repair-worker-state.v2"
)

type Stage string

const (
	StagePrepared      Stage = "prepared"
	StageModelComplete Stage = "model_complete"
	StageNoChange      Stage = "no_change"
	StageValidated     Stage = "validated"
	StageCommitted     Stage = "committed"
	StagePushed        Stage = "pushed"
	StagePROpen        Stage = "pr_open"
)

type Attempt struct {
	Repository            string          `json:"repository"`
	RepositoryFingerprint string          `json:"repository_fingerprint"`
	Attempt               int             `json:"attempt"`
	Branch                string          `json:"branch"`
	Worktree              string          `json:"worktree"`
	Stage                 Stage           `json:"stage"`
	Provider              string          `json:"provider"`
	LifecycleStarted      bool            `json:"lifecycle_started,omitempty"`
	ModelSummary          string          `json:"model_summary,omitempty"`
	PriorModelSummary     string          `json:"prior_model_summary,omitempty"`
	ModelPatch            string          `json:"model_patch,omitempty"`
	CommitSHA             string          `json:"commit_sha,omitempty"`
	PRNumber              int             `json:"pr_number,omitempty"`
	PRURL                 string          `json:"pr_url,omitempty"`
	ChangedFiles          []string        `json:"changed_files,omitempty"`
	BaselineReview        *BaselineReview `json:"baseline_review,omitempty"`
	StartedAt             time.Time       `json:"started_at"`
	UpdatedAt             time.Time       `json:"updated_at"`
}

type BaselineReviewStatus string

const (
	BaselineReviewCandidateReady BaselineReviewStatus = "candidate_ready"
	BaselineReviewProposalOpen   BaselineReviewStatus = "proposal_open"
	BaselineReviewApproved       BaselineReviewStatus = "approved"
	BaselineReviewRejected       BaselineReviewStatus = "rejected"
)

// BaselineCandidate is review evidence only. ActualPath points to a Hive-owned
// local copy; BaselinePath is always a repository-relative path under the
// configured baseline root. Neither field grants approval authority.
type BaselineCandidate struct {
	ContractID     string `json:"contract_id"`
	ScreenshotName string `json:"screenshot_name"`
	Route          string `json:"route,omitempty"`
	Viewport       string `json:"viewport,omitempty"`
	Platform       string `json:"platform"`
	ActualPath     string `json:"actual_path"`
	BaselinePath   string `json:"baseline_path"`
	SHA256         string `json:"sha256"`
	Bytes          int64  `json:"bytes"`
	Source         string `json:"source"`
}

type BaselineReview struct {
	Status            BaselineReviewStatus `json:"status"`
	Candidates        []BaselineCandidate  `json:"candidates"`
	RepairHeadSHA     string               `json:"repair_head_sha,omitempty"`
	SourceWorkflowRun int64                `json:"source_workflow_run_id,omitempty"`
	SourceArtifactID  int64                `json:"source_artifact_id,omitempty"`
	SourceRunURL      string               `json:"source_run_url,omitempty"`
	ProposalBranch    string               `json:"proposal_branch,omitempty"`
	ProposalCommitSHA string               `json:"proposal_commit_sha,omitempty"`
	ProposalPRNumber  int                  `json:"proposal_pr_number,omitempty"`
	ProposalPRURL     string               `json:"proposal_pr_url,omitempty"`
	ProposalMergeSHA  string               `json:"proposal_merge_sha,omitempty"`
	RejectionReason   string               `json:"rejection_reason,omitempty"`
}

type State struct {
	SchemaVersion string              `json:"schema_version"`
	Attempts      map[string]*Attempt `json:"attempts"`
}

type Store struct {
	mu   sync.Mutex
	path string
	data State
}

func NewStore(dir string) (*Store, error) {
	if dir == "" {
		return nil, fmt.Errorf("repair state directory is required")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	store := &Store{path: filepath.Join(dir, "repair-worker-state.json"), data: State{SchemaVersion: StateSchema, Attempts: map[string]*Attempt{}}}
	data, err := os.ReadFile(store.path)
	if err == nil {
		if err := json.Unmarshal(data, &store.data); err != nil {
			return nil, fmt.Errorf("parse repair worker state: %w", err)
		}
		migrated := false
		if store.data.SchemaVersion == StateSchemaV1 {
			store.data.SchemaVersion = StateSchema
			migrated = true
		} else if store.data.SchemaVersion != StateSchema {
			return nil, fmt.Errorf("unsupported repair state schema %q", store.data.SchemaVersion)
		}
		if store.data.Attempts == nil {
			store.data.Attempts = map[string]*Attempt{}
		}
		if migrated {
			if err := store.persistLocked(); err != nil {
				return nil, fmt.Errorf("migrate repair worker state: %w", err)
			}
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	return store, nil
}

func (s *Store) Get(fingerprint string) (Attempt, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	attempt := s.data.Attempts[fingerprint]
	if attempt == nil {
		return Attempt{}, false
	}
	return cloneAttempt(*attempt), true
}

func (s *Store) Snapshot() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := State{SchemaVersion: s.data.SchemaVersion, Attempts: make(map[string]*Attempt, len(s.data.Attempts))}
	for key, attempt := range s.data.Attempts {
		if attempt != nil {
			copy := cloneAttempt(*attempt)
			result.Attempts[key] = &copy
		}
	}
	return result
}

func (s *Store) Put(attempt Attempt) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	attempt.UpdatedAt = time.Now().UTC()
	copy := cloneAttempt(attempt)
	s.data.Attempts[attempt.RepositoryFingerprint] = &copy
	return s.persistLocked()
}

func cloneAttempt(attempt Attempt) Attempt {
	attempt.ChangedFiles = append([]string(nil), attempt.ChangedFiles...)
	if attempt.BaselineReview != nil {
		review := *attempt.BaselineReview
		review.Candidates = append([]BaselineCandidate(nil), attempt.BaselineReview.Candidates...)
		attempt.BaselineReview = &review
	}
	return attempt
}

func (s *Store) persistLocked() error {
	data, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	temporary := s.path + ".tmp"
	if err := os.WriteFile(temporary, append(data, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Rename(temporary, s.path); err != nil {
		return err
	}
	return nil
}
