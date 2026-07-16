package normalservice

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	hivegithub "github.com/kubestellar/hive/v2/pkg/github"
	"github.com/kubestellar/hive/v2/pkg/integrated"
	"github.com/kubestellar/hive/v2/pkg/repair"
	visualcontroller "github.com/kubestellar/hive/v2/pkg/visualhive/controller"
)

const ledgerSchema = "hive.normal-visual-work.v1"

var (
	ErrFinalVerdictPending = errors.New("exact-head pull-request verdict is pending")
	ErrNoDispatch          = errors.New("verified packet produced no currently launchable specialist dispatch")
)

type ArtifactSource interface {
	Fetch(context.Context) (integrated.NormalVisualWork, error)
	Consume(integrated.WorkflowRunEvidence, bool) error
}

type Intake interface {
	Import(context.Context, hivegithub.VerifiedVisualHiveArtifact) (visualcontroller.Result, error)
	RevalidateSpecialistBoundary(string, string, string) (visualcontroller.DispatchEnvelope, error)
	CompleteSpecialistPullRequest(string, visualcontroller.SpecialistPullRequestCompletion) error
}

type RepairOutcome struct {
	WorkOrderID   string
	RequestSHA256 string
	Result        repair.Result
}

type Repairer interface {
	Run(context.Context, visualcontroller.DispatchEnvelope) (RepairOutcome, error)
}

type PullRequestVerdictRequest struct {
	IdempotencyKey    string
	Repository        string
	PullRequestNumber int
	HeadBranch        string
	HeadSHA           string
	BaseBranch        string
}

// PullRequestVerdictReceipt is observation-only. Status and Receipt are
// durable exact-head deterministic evidence; they grant no merge, baseline,
// lifecycle-resolution, or issue-closing authority.
type PullRequestVerdictReceipt struct {
	HeadSHA       string
	Status        string
	Receipt       json.RawMessage
	ReceiptSHA256 string
}

type PullRequestVerdictVerifier interface {
	VerifyPullRequest(context.Context, PullRequestVerdictRequest) (PullRequestVerdictReceipt, error)
}

type LeaseAcquirer func() (func(), error)

type Options struct {
	StateDir     string
	PollInterval time.Duration
	LeaseRetry   time.Duration
	AcquireLease LeaseAcquirer
	Source       ArtifactSource
	Intake       Intake
	Repairer     Repairer
	Verdict      PullRequestVerdictVerifier
	Logger       *slog.Logger
	OnCycle      func(error)
}

// Service is a bounded sequential reconciler, not another queue or manager.
// The existing integrated dispatch intent, native intake envelope, mailbox,
// Worker store, and one small exact-binding ledger own crash recovery.
type Service struct {
	options Options
	mu      sync.Mutex
}

func New(options Options) (*Service, error) {
	if strings.TrimSpace(options.StateDir) == "" || options.AcquireLease == nil || options.Source == nil || options.Intake == nil || options.Repairer == nil {
		return nil, errors.New("normal Visual Hive service requires state, lease, source, intake, and Worker repairer")
	}
	root, err := filepath.Abs(options.StateDir)
	if err != nil {
		return nil, err
	}
	options.StateDir = root
	if options.PollInterval <= 0 {
		options.PollInterval = 5 * time.Minute
	}
	if options.LeaseRetry <= 0 {
		options.LeaseRetry = 30 * time.Second
	}
	if options.Logger == nil {
		options.Logger = slog.Default()
	}
	if err := os.MkdirAll(filepath.Join(root, "normal-service"), 0o700); err != nil {
		return nil, err
	}
	return &Service{options: options}, nil
}

// Run claims lifetime ownership before touching workflow state. Lease
// contention is an idle condition and cannot block the ordinary Governor loop,
// because this method is intended to run in its own bounded goroutine.
func (service *Service) Run(ctx context.Context) {
	if service == nil {
		return
	}
	var release func()
	for release == nil {
		claimed, err := service.options.AcquireLease()
		if err == nil {
			release = claimed
			break
		}
		service.report(err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(service.options.LeaseRetry):
		}
	}
	defer release()
	for {
		err := service.RunCycle(ctx)
		service.report(err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(service.options.PollInterval):
		}
	}
}

func (service *Service) report(err error) {
	if service.options.OnCycle != nil {
		service.options.OnCycle(err)
	}
	if err != nil && !errors.Is(err, ErrNoDispatch) && !errors.Is(err, ErrFinalVerdictPending) && !errors.Is(err, integrated.ErrRunInProgress) {
		service.options.Logger.Warn("normal Visual Hive cycle held", "error", err)
	}
}

func (service *Service) RunCycle(ctx context.Context) error {
	if service == nil {
		return errors.New("normal Visual Hive service is nil")
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	ledger, exists, err := service.loadLedger()
	if err != nil {
		return err
	}
	if exists && ledger.Consumed {
		if err := service.clearLedger(); err != nil {
			return err
		}
		exists = false
	}
	if exists && ledger.ConsumeStarted && ledger.SourceExternalRef == "" {
		if err := service.options.Source.Consume(ledger.Workflow, true); err != nil {
			return err
		}
		ledger.Consumed = true
		if err := service.saveLedger(ledger); err != nil {
			return err
		}
		return ErrNoDispatch
	}
	if exists && ledger.VerdictReceiptSHA256 != "" {
		return service.finish(ctx, ledger)
	}
	var envelope visualcontroller.DispatchEnvelope
	if exists && ledger.SourceExternalRef != "" {
		envelope, err = service.options.Intake.RevalidateSpecialistBoundary(ledger.SourceExternalRef, ledger.WorkOrderID, ledger.RequestSHA256)
		if err != nil {
			return err
		}
	} else {
		work, fetchErr := service.options.Source.Fetch(ctx)
		if fetchErr != nil {
			return fetchErr
		}
		key, keyErr := workflowKey(work)
		if keyErr != nil {
			return keyErr
		}
		if !exists {
			ledger = workLedger{
				SchemaVersion: ledgerSchema, WorkflowKey: key, Workflow: work.Workflow,
				Repository: work.Config.Repository, BaseBranch: work.Config.DefaultBranch, PacketDigest: work.Artifact.BundleSHA256,
			}
			if err := service.saveLedger(ledger); err != nil {
				return err
			}
		} else if ledger.WorkflowKey != key || ledger.Repository != work.Config.Repository || ledger.PacketDigest != work.Artifact.BundleSHA256 {
			return errors.New("fetched Visual Hive artifact differs from the one active durable service binding")
		}
		result, importErr := service.options.Intake.Import(ctx, work.Artifact)
		if importErr != nil {
			return importErr
		}
		if len(result.Errors) > 0 {
			return fmt.Errorf("native Visual Hive intake held: %s", strings.Join(result.Errors, "; "))
		}
		var found bool
		envelope, found = selectDispatch(result.DispatchPending, "")
		if !found {
			// A green report or a current pause/WIP/policy hold creates no model or
			// PR. Retire this exact workflow and let the ordinary cadence produce a
			// fresh report after state changes.
			ledger.ConsumeStarted = true
			if err := service.saveLedger(ledger); err != nil {
				return err
			}
			if err := service.options.Source.Consume(work.Workflow, true); err != nil {
				return err
			}
			ledger.Consumed = true
			if err := service.saveLedger(ledger); err != nil {
				return err
			}
			return ErrNoDispatch
		}
		ledger.SourceExternalRef = envelope.SourceExternalRef
		if err := service.saveLedger(ledger); err != nil {
			return err
		}
	}
	if ledger.PullRequestNumber == 0 {
		outcome, err := service.options.Repairer.Run(ctx, envelope)
		if err != nil {
			return err
		}
		if err := validateRepairOutcome(envelope, outcome); err != nil {
			return err
		}
		ledger.WorkOrderID, ledger.RequestSHA256 = outcome.WorkOrderID, outcome.RequestSHA256
		ledger.Branch, ledger.CommitSHA = outcome.Result.Branch, strings.ToLower(outcome.Result.CommitSHA)
		ledger.PullRequestNumber, ledger.PullRequestURL = outcome.Result.PRNumber, outcome.Result.PRURL
		if err := service.saveLedger(ledger); err != nil {
			return err
		}
	}
	if service.options.Verdict == nil {
		return ErrFinalVerdictPending
	}
	if _, err := service.options.Intake.RevalidateSpecialistBoundary(ledger.SourceExternalRef, ledger.WorkOrderID, ledger.RequestSHA256); err != nil {
		return err
	}
	receipt, err := service.options.Verdict.VerifyPullRequest(ctx, PullRequestVerdictRequest{
		IdempotencyKey: ledger.WorkflowKey + ":" + ledger.WorkOrderID, Repository: ledger.Repository,
		PullRequestNumber: ledger.PullRequestNumber, HeadBranch: ledger.Branch, HeadSHA: ledger.CommitSHA, BaseBranch: ledger.BaseBranch,
	})
	if err != nil {
		return err
	}
	if err := validateVerdictReceipt(ledger, receipt); err != nil {
		return err
	}
	ledger.VerdictHeadSHA = strings.ToLower(receipt.HeadSHA)
	ledger.VerdictStatus = strings.TrimSpace(receipt.Status)
	ledger.VerdictReceipt = append(json.RawMessage(nil), receipt.Receipt...)
	ledger.VerdictReceiptSHA256 = strings.ToLower(receipt.ReceiptSHA256)
	if err := service.saveLedger(ledger); err != nil {
		return err
	}
	return service.finish(ctx, ledger)
}

func (service *Service) finish(_ context.Context, ledger workLedger) error {
	if !ledger.CompletionRecorded {
		completion := visualcontroller.SpecialistPullRequestCompletion{
			WorkOrderID: ledger.WorkOrderID, RequestSHA256: ledger.RequestSHA256, Branch: ledger.Branch, CommitSHA: ledger.CommitSHA,
			PullRequestNumber: ledger.PullRequestNumber, PullRequestURL: ledger.PullRequestURL,
			VerdictHeadSHA: ledger.VerdictHeadSHA, VerdictReceiptSHA256: ledger.VerdictReceiptSHA256, VerdictStatus: ledger.VerdictStatus,
		}
		if err := service.options.Intake.CompleteSpecialistPullRequest(ledger.SourceExternalRef, completion); err != nil {
			return err
		}
		ledger.CompletionRecorded = true
		if err := service.saveLedger(ledger); err != nil {
			return err
		}
	}
	ledger.ConsumeStarted = true
	if err := service.saveLedger(ledger); err != nil {
		return err
	}
	if err := service.options.Source.Consume(ledger.Workflow, true); err != nil {
		return err
	}
	ledger.Consumed = true
	return service.saveLedger(ledger)
}

func selectDispatch(values []visualcontroller.DispatchEnvelope, sourceExternalRef string) (visualcontroller.DispatchEnvelope, bool) {
	ordered := append([]visualcontroller.DispatchEnvelope(nil), values...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].SourceExternalRef < ordered[j].SourceExternalRef })
	for _, value := range ordered {
		if sourceExternalRef == "" || value.SourceExternalRef == sourceExternalRef {
			return value, true
		}
	}
	return visualcontroller.DispatchEnvelope{}, false
}

func validateRepairOutcome(envelope visualcontroller.DispatchEnvelope, outcome RepairOutcome) error {
	digest := strings.ToLower(strings.TrimSpace(outcome.RequestSHA256))
	if outcome.WorkOrderID != "swo-"+digest || len(digest) != sha256.Size*2 || outcome.Result.RepositoryFingerprint != envelope.Work.RepositoryFingerprint ||
		outcome.Result.PRNumber <= 0 || strings.TrimSpace(outcome.Result.PRURL) == "" || strings.TrimSpace(outcome.Result.Branch) == "" ||
		!validGitObject(outcome.Result.CommitSHA) {
		return errors.New("Worker result lacks the one exact specialist order and pull request identity")
	}
	return nil
}

func validateVerdictReceipt(ledger workLedger, receipt PullRequestVerdictReceipt) error {
	receipt.HeadSHA = strings.ToLower(strings.TrimSpace(receipt.HeadSHA))
	receipt.ReceiptSHA256 = strings.ToLower(strings.TrimSpace(receipt.ReceiptSHA256))
	if receipt.HeadSHA != ledger.CommitSHA || strings.TrimSpace(receipt.Status) == "" || !json.Valid(receipt.Receipt) {
		return errors.New("pull-request verdict is not a valid exact-head receipt")
	}
	digest := sha256.Sum256(receipt.Receipt)
	if receipt.ReceiptSHA256 != hex.EncodeToString(digest[:]) {
		return errors.New("pull-request verdict receipt digest mismatch")
	}
	return nil
}

func workflowKey(work integrated.NormalVisualWork) (string, error) {
	encoded, err := json.Marshal(struct {
		Repository string                         `json:"repository"`
		Workflow   integrated.WorkflowRunEvidence `json:"workflow"`
		BundleSHA  string                         `json:"bundle_sha256"`
	}{work.Config.Repository, work.Workflow, work.Artifact.BundleSHA256})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func validGitObject(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

type workLedger struct {
	SchemaVersion        string                         `json:"schema_version"`
	WorkflowKey          string                         `json:"workflow_key"`
	Workflow             integrated.WorkflowRunEvidence `json:"workflow"`
	Repository           string                         `json:"repository"`
	BaseBranch           string                         `json:"base_branch"`
	PacketDigest         string                         `json:"packet_digest"`
	SourceExternalRef    string                         `json:"source_external_ref,omitempty"`
	WorkOrderID          string                         `json:"work_order_id,omitempty"`
	RequestSHA256        string                         `json:"request_sha256,omitempty"`
	Branch               string                         `json:"branch,omitempty"`
	CommitSHA            string                         `json:"commit_sha,omitempty"`
	PullRequestNumber    int                            `json:"pull_request_number,omitempty"`
	PullRequestURL       string                         `json:"pull_request_url,omitempty"`
	VerdictHeadSHA       string                         `json:"verdict_head_sha,omitempty"`
	VerdictStatus        string                         `json:"verdict_status,omitempty"`
	VerdictReceipt       json.RawMessage                `json:"verdict_receipt,omitempty"`
	VerdictReceiptSHA256 string                         `json:"verdict_receipt_sha256,omitempty"`
	CompletionRecorded   bool                           `json:"completion_recorded,omitempty"`
	ConsumeStarted       bool                           `json:"consume_started,omitempty"`
	Consumed             bool                           `json:"consumed,omitempty"`
}

func (service *Service) ledgerPath() string {
	return filepath.Join(service.options.StateDir, "normal-service", "active.json")
}

func (service *Service) loadLedger() (workLedger, bool, error) {
	data, err := os.ReadFile(service.ledgerPath())
	if errors.Is(err, os.ErrNotExist) {
		return workLedger{}, false, nil
	}
	if err != nil {
		return workLedger{}, false, err
	}
	var ledger workLedger
	if json.Unmarshal(data, &ledger) != nil || ledger.SchemaVersion != ledgerSchema || ledger.WorkflowKey == "" || ledger.Repository == "" {
		return workLedger{}, false, errors.New("normal Visual Hive service ledger is corrupt")
	}
	return ledger, true, nil
}

func (service *Service) saveLedger(ledger workLedger) error {
	ledger.SchemaVersion = ledgerSchema
	encoded, err := json.MarshalIndent(ledger, "", "  ")
	if err != nil {
		return err
	}
	path := service.ledgerPath()
	temporary, err := os.CreateTemp(filepath.Dir(path), ".active-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(append(encoded, '\n')); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return durableReplaceLedger(temporaryPath, path)
}

func (service *Service) clearLedger() error {
	err := os.Remove(service.ledgerPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
