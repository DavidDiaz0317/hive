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

const ledgerSchema = "hive.normal-visual-work.v3"

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
	DeferUnselectedSpecialistDispatches(context.Context, string, []string, string) error
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
		var deferred []string
		envelope, deferred, found, err = selectDispatchSet(result.DispatchPending)
		if err != nil {
			return err
		}
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
		ledger.DeferredSourceExternalRefs = deferred
		ledger.DeferralsRecorded = len(deferred) == 0
		if err := service.saveLedger(ledger); err != nil {
			return err
		}
	}
	if !ledger.DeferralsRecorded {
		if err := service.options.Intake.DeferUnselectedSpecialistDispatches(ctx, ledger.SourceExternalRef, ledger.DeferredSourceExternalRefs, ledger.WorkflowKey); err != nil {
			return err
		}
		ledger.DeferralsRecorded = true
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
	ledger.VerdictReceipt = append([]byte(nil), receipt.Receipt...)
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

func selectDispatchSet(values []visualcontroller.DispatchEnvelope) (visualcontroller.DispatchEnvelope, []string, bool, error) {
	ordered := append([]visualcontroller.DispatchEnvelope(nil), values...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].SourceExternalRef < ordered[j].SourceExternalRef })
	for index, value := range ordered {
		if strings.TrimSpace(value.SourceExternalRef) == "" || (index > 0 && value.SourceExternalRef == ordered[index-1].SourceExternalRef) {
			return visualcontroller.DispatchEnvelope{}, nil, false, errors.New("controller returned empty or duplicate launchable source refs")
		}
	}
	if len(ordered) == 0 {
		return visualcontroller.DispatchEnvelope{}, nil, false, nil
	}
	deferred := make([]string, 0, len(ordered)-1)
	for _, value := range ordered[1:] {
		deferred = append(deferred, value.SourceExternalRef)
	}
	return ordered[0], deferred, true, nil
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
	SchemaVersion              string                         `json:"schema_version"`
	WorkflowKey                string                         `json:"workflow_key"`
	Workflow                   integrated.WorkflowRunEvidence `json:"workflow"`
	Repository                 string                         `json:"repository"`
	BaseBranch                 string                         `json:"base_branch"`
	PacketDigest               string                         `json:"packet_digest"`
	SourceExternalRef          string                         `json:"source_external_ref,omitempty"`
	DeferredSourceExternalRefs []string                       `json:"deferred_source_external_refs,omitempty"`
	DeferralsRecorded          bool                           `json:"deferrals_recorded,omitempty"`
	WorkOrderID                string                         `json:"work_order_id,omitempty"`
	RequestSHA256              string                         `json:"request_sha256,omitempty"`
	Branch                     string                         `json:"branch,omitempty"`
	CommitSHA                  string                         `json:"commit_sha,omitempty"`
	PullRequestNumber          int                            `json:"pull_request_number,omitempty"`
	PullRequestURL             string                         `json:"pull_request_url,omitempty"`
	VerdictHeadSHA             string                         `json:"verdict_head_sha,omitempty"`
	VerdictStatus              string                         `json:"verdict_status,omitempty"`
	VerdictReceipt             []byte                         `json:"verdict_receipt_bytes,omitempty"`
	VerdictReceiptSHA256       string                         `json:"verdict_receipt_sha256,omitempty"`
	CompletionRecorded         bool                           `json:"completion_recorded,omitempty"`
	ConsumeStarted             bool                           `json:"consume_started,omitempty"`
	Consumed                   bool                           `json:"consumed,omitempty"`
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
	if err := json.Unmarshal(data, &ledger); err != nil {
		return workLedger{}, false, errors.New("normal Visual Hive service ledger is corrupt")
	}
	if err := validateWorkLedger(ledger); err != nil {
		return workLedger{}, false, fmt.Errorf("normal Visual Hive service ledger is corrupt: %w", err)
	}
	return ledger, true, nil
}

func (service *Service) saveLedger(ledger workLedger) error {
	ledger.SchemaVersion = ledgerSchema
	if err := validateWorkLedger(ledger); err != nil {
		return fmt.Errorf("refuse invalid normal Visual Hive service ledger: %w", err)
	}
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

func validateWorkLedger(ledger workLedger) error {
	if ledger.SchemaVersion != ledgerSchema || !validSHA256Value(ledger.WorkflowKey) || strings.TrimSpace(ledger.Repository) == "" ||
		strings.TrimSpace(ledger.BaseBranch) == "" || !validSHA256Value(ledger.PacketDigest) || !validSHA256Value(ledger.Workflow.CorrelationID) ||
		ledger.Workflow.RunID <= 0 || ledger.Workflow.BundleArtifact <= 0 || ledger.Workflow.EvidenceArtifact <= 0 || !validGitObject(ledger.Workflow.HeadSHA) {
		return errors.New("workflow and packet binding is incomplete")
	}
	expectedKey, err := workflowKey(integrated.NormalVisualWork{
		Config:   integrated.Config{Repository: ledger.Repository, DefaultBranch: ledger.BaseBranch},
		Workflow: ledger.Workflow, Artifact: hivegithub.VerifiedVisualHiveArtifact{BundleSHA256: ledger.PacketDigest},
	})
	if err != nil || expectedKey != ledger.WorkflowKey {
		return errors.New("workflow key does not match the stored workflow and packet")
	}
	if (ledger.WorkOrderID == "") != (ledger.RequestSHA256 == "") {
		return errors.New("specialist work-order identity is partial")
	}
	if ledger.SourceExternalRef != strings.TrimSpace(ledger.SourceExternalRef) {
		return errors.New("selected source external ref is not canonical")
	}
	for index, ref := range ledger.DeferredSourceExternalRefs {
		if ref == "" || ref != strings.TrimSpace(ref) || ref == ledger.SourceExternalRef ||
			(index > 0 && ref <= ledger.DeferredSourceExternalRefs[index-1]) {
			return errors.New("deferred source external refs are empty, duplicate, selected, or unsorted")
		}
	}
	if ledger.SourceExternalRef == "" && (len(ledger.DeferredSourceExternalRefs) != 0 || ledger.DeferralsRecorded) {
		return errors.New("unselected dispatch state exists without a selected source ref")
	}
	if ledger.SourceExternalRef != "" && !ledger.DeferralsRecorded && len(ledger.DeferredSourceExternalRefs) == 0 {
		return errors.New("selected dispatch has an incomplete empty deferral checkpoint")
	}
	if ledger.WorkOrderID != "" && (!validSHA256Value(ledger.RequestSHA256) || ledger.WorkOrderID != "swo-"+ledger.RequestSHA256) {
		return errors.New("specialist work-order identity is invalid")
	}
	prPresent := ledger.PullRequestNumber != 0 || ledger.PullRequestURL != "" || ledger.Branch != "" || ledger.CommitSHA != ""
	if prPresent && (ledger.SourceExternalRef == "" || ledger.WorkOrderID == "" || ledger.PullRequestNumber <= 0 || strings.TrimSpace(ledger.PullRequestURL) == "" ||
		strings.TrimSpace(ledger.Branch) == "" || ledger.CommitSHA != strings.ToLower(strings.TrimSpace(ledger.CommitSHA)) || !validGitObject(ledger.CommitSHA)) {
		return errors.New("Worker pull-request identity is partial or invalid")
	}
	verdictPresent := ledger.VerdictReceiptSHA256 != "" || ledger.VerdictHeadSHA != "" || ledger.VerdictStatus != "" || len(ledger.VerdictReceipt) != 0
	if verdictPresent {
		if !prPresent || validateVerdictReceipt(ledger, PullRequestVerdictReceipt{
			HeadSHA: ledger.VerdictHeadSHA, Status: ledger.VerdictStatus, Receipt: json.RawMessage(ledger.VerdictReceipt), ReceiptSHA256: ledger.VerdictReceiptSHA256,
		}) != nil {
			return errors.New("exact-head verdict binding is partial or invalid")
		}
	}
	if ledger.SourceExternalRef == "" && (ledger.WorkOrderID != "" || prPresent || verdictPresent || ledger.CompletionRecorded) {
		return errors.New("unimported workflow carries specialist or pull-request state")
	}
	if ledger.SourceExternalRef != "" && !ledger.DeferralsRecorded && (ledger.WorkOrderID != "" || prPresent || verdictPresent || ledger.CompletionRecorded || ledger.ConsumeStarted || ledger.Consumed) {
		return errors.New("specialist or workflow side effects started before all launchable peers were durably deferred")
	}
	if ledger.CompletionRecorded && !verdictPresent {
		return errors.New("controller completion has no exact-head verdict")
	}
	if ledger.ConsumeStarted && ledger.SourceExternalRef != "" && !ledger.CompletionRecorded {
		return errors.New("admitted workflow consumption started before controller completion")
	}
	if ledger.Consumed && !ledger.ConsumeStarted {
		return errors.New("workflow is consumed without a durable consume checkpoint")
	}
	return nil
}

func validSHA256Value(value string) bool {
	if len(value) != sha256.Size*2 || value != strings.ToLower(strings.TrimSpace(value)) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func (service *Service) clearLedger() error {
	err := os.Remove(service.ledgerPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
