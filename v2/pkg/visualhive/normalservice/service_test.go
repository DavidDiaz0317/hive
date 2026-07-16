package normalservice

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	hivegithub "github.com/kubestellar/hive/v2/pkg/github"
	"github.com/kubestellar/hive/v2/pkg/integrated"
	"github.com/kubestellar/hive/v2/pkg/repair"
	"github.com/kubestellar/hive/v2/pkg/visualhive"
	visualcontroller "github.com/kubestellar/hive/v2/pkg/visualhive/controller"
)

func TestNormalServiceCrashReplayKeepsOneImportProposalPRAndVerdict(t *testing.T) {
	fixture := newServiceFixture(t)
	fixture.intake.failCompletion = true
	service := fixture.service(t, fixture.verifier)

	if err := service.RunCycle(context.Background()); err == nil || !strings.Contains(err.Error(), "completion checkpoint interrupted") {
		t.Fatalf("first cycle error = %v", err)
	}
	if fixture.source.fetches != 1 || fixture.intake.imports != 1 || fixture.repairer.runs != 1 || fixture.verifier.calls != 1 || fixture.source.consumes != 0 {
		t.Fatalf("first cycle duplicated or skipped work: source=%+v intake=%+v repair=%+v verifier=%+v", fixture.source, fixture.intake, fixture.repairer, fixture.verifier)
	}
	if err := service.RunCycle(context.Background()); err != nil {
		t.Fatalf("replay cycle: %v", err)
	}
	if fixture.source.fetches != 1 || fixture.intake.imports != 1 || fixture.repairer.runs != 1 || fixture.verifier.calls != 1 || fixture.source.consumes != 1 {
		t.Fatalf("durable replay repeated import/proposal/PR/verdict: source=%+v intake=%+v repair=%+v verifier=%+v", fixture.source, fixture.intake, fixture.repairer, fixture.verifier)
	}
	if fixture.intake.completion.VerdictStatus != "failed" || fixture.intake.completion.VerdictHeadSHA != fixture.repairer.outcome.Result.CommitSHA {
		t.Fatalf("red exact-head receipt was not recorded without reinterpretation: %+v", fixture.intake.completion)
	}
}

func TestNormalServiceAmbiguousConsumeRecoversWithoutNewWorkflowOrPR(t *testing.T) {
	fixture := newServiceFixture(t)
	fixture.source.failConsumeAfterSideEffect = true
	service := fixture.service(t, fixture.verifier)
	if err := service.RunCycle(context.Background()); err == nil || !strings.Contains(err.Error(), "consume response lost") {
		t.Fatalf("ambiguous consume error = %v", err)
	}
	if err := service.RunCycle(context.Background()); err != nil {
		t.Fatalf("consume recovery: %v", err)
	}
	if fixture.source.fetches != 1 || fixture.intake.imports != 1 || fixture.repairer.runs != 1 || fixture.verifier.calls != 1 || fixture.source.consumeSideEffects != 1 {
		t.Fatalf("ambiguous consume replay duplicated work or deletion: source=%+v intake=%+v repair=%+v verifier=%+v", fixture.source, fixture.intake, fixture.repairer, fixture.verifier)
	}
}

func TestNormalServiceLeavesWorkerPROpenUntilExactHeadVerifierExists(t *testing.T) {
	fixture := newServiceFixture(t)
	service := fixture.service(t, nil)
	if err := service.RunCycle(context.Background()); !errors.Is(err, ErrFinalVerdictPending) {
		t.Fatalf("cycle error = %v, want pending verdict", err)
	}
	if fixture.repairer.runs != 1 || fixture.source.consumes != 0 || fixture.intake.completes != 0 {
		t.Fatalf("missing verifier consumed or completed PR: source=%+v intake=%+v repair=%+v", fixture.source, fixture.intake, fixture.repairer)
	}
	if err := service.RunCycle(context.Background()); !errors.Is(err, ErrFinalVerdictPending) {
		t.Fatalf("replay error = %v, want pending verdict", err)
	}
	if fixture.repairer.runs != 1 || fixture.source.consumes != 0 {
		t.Fatalf("pending exact PR was duplicated or consumed")
	}
}

func TestNormalServiceNoDispatchIsIdleAndNeverRunsWorker(t *testing.T) {
	fixture := newServiceFixture(t)
	fixture.intake.dispatch = nil
	service := fixture.service(t, fixture.verifier)
	if err := service.RunCycle(context.Background()); !errors.Is(err, ErrNoDispatch) {
		t.Fatalf("cycle error = %v, want idle", err)
	}
	if fixture.repairer.runs != 0 || fixture.verifier.calls != 0 || fixture.source.consumeSideEffects != 1 {
		t.Fatalf("pause/WIP/no-work path launched specialist or failed to retire exact report")
	}
}

func TestNormalServiceLeaseContentionDoesNotRunCycleUntilOwnership(t *testing.T) {
	fixture := newServiceFixture(t)
	fixture.intake.dispatch = nil
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	claims, released := 0, false
	service, err := New(Options{
		StateDir: t.TempDir(), PollInterval: time.Hour, LeaseRetry: time.Millisecond,
		AcquireLease: func() (func(), error) {
			claims++
			if claims == 1 {
				return nil, integrated.ErrRunInProgress
			}
			return func() { released = true }, nil
		},
		Source: fixture.source, Intake: fixture.intake, Repairer: fixture.repairer, Verdict: fixture.verifier,
		OnCycle: func(err error) {
			if errors.Is(err, ErrNoDispatch) {
				cancel()
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	service.Run(ctx)
	if claims != 2 || !released || fixture.source.fetches != 1 {
		t.Fatalf("lease reconciliation = claims=%d released=%t fetches=%d", claims, released, fixture.source.fetches)
	}
}

type serviceFixture struct {
	work     integrated.NormalVisualWork
	source   *fakeArtifactSource
	intake   *fakeIntake
	repairer *fakeRepairer
	verifier *fakeVerdictVerifier
}

func newServiceFixture(t *testing.T) *serviceFixture {
	t.Helper()
	head := strings.Repeat("b", 40)
	bundle := strings.Repeat("a", 64)
	work := integrated.NormalVisualWork{
		Config:   integrated.Config{Repository: "owner/repo", DefaultBranch: "main"},
		Workflow: integrated.WorkflowRunEvidence{CorrelationID: strings.Repeat("c", 64), RunID: 42, HeadSHA: head, BundleArtifact: 7, EvidenceArtifact: 8},
		Artifact: hivegithub.VerifiedVisualHiveArtifact{BundleSHA256: bundle},
	}
	envelope := visualcontroller.DispatchEnvelope{
		SourceExternalRef: "visual-hive://owner/repo/finding",
		Work:              visualhive.AdmittedVisualWork{RepositoryFingerprint: strings.Repeat("d", 64)},
	}
	outcome := RepairOutcome{
		WorkOrderID: "swo-" + strings.Repeat("e", 64), RequestSHA256: strings.Repeat("e", 64),
		Result: repair.Result{RepositoryFingerprint: envelope.Work.RepositoryFingerprint, Branch: "hive/repair-one", CommitSHA: strings.Repeat("f", 40), PRNumber: 9, PRURL: "https://example.test/pr/9"},
	}
	receipt := json.RawMessage(`{"schema_version":"visual-hive.pr-verdict.v1","status":"failed"}`)
	digest := sha256.Sum256(receipt)
	return &serviceFixture{
		work:     work,
		source:   &fakeArtifactSource{work: work},
		intake:   &fakeIntake{dispatch: []visualcontroller.DispatchEnvelope{envelope}},
		repairer: &fakeRepairer{outcome: outcome},
		verifier: &fakeVerdictVerifier{receipt: PullRequestVerdictReceipt{HeadSHA: outcome.Result.CommitSHA, Status: "failed", Receipt: receipt, ReceiptSHA256: hex.EncodeToString(digest[:])}},
	}
}

func (fixture *serviceFixture) service(t *testing.T, verifier PullRequestVerdictVerifier) *Service {
	t.Helper()
	service, err := New(Options{
		StateDir: t.TempDir(), AcquireLease: func() (func(), error) { return func() {}, nil },
		Source: fixture.source, Intake: fixture.intake, Repairer: fixture.repairer, Verdict: verifier,
	})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

type fakeArtifactSource struct {
	mu                         sync.Mutex
	work                       integrated.NormalVisualWork
	fetches                    int
	consumes                   int
	consumeSideEffects         int
	failConsumeAfterSideEffect bool
}

func (source *fakeArtifactSource) Fetch(context.Context) (integrated.NormalVisualWork, error) {
	source.mu.Lock()
	defer source.mu.Unlock()
	source.fetches++
	return source.work, nil
}

func (source *fakeArtifactSource) Consume(_ integrated.WorkflowRunEvidence, allow bool) error {
	source.mu.Lock()
	defer source.mu.Unlock()
	source.consumes++
	if source.consumeSideEffects == 0 {
		source.consumeSideEffects++
		if source.failConsumeAfterSideEffect {
			source.failConsumeAfterSideEffect = false
			return errors.New("consume response lost")
		}
		return nil
	}
	if !allow {
		return errors.New("duplicate consume without recovery authority")
	}
	return nil
}

type fakeIntake struct {
	dispatch       []visualcontroller.DispatchEnvelope
	imports        int
	completes      int
	failCompletion bool
	completion     visualcontroller.SpecialistPullRequestCompletion
}

func (intake *fakeIntake) Import(context.Context, hivegithub.VerifiedVisualHiveArtifact) (visualcontroller.Result, error) {
	intake.imports++
	return visualcontroller.Result{DispatchPending: append([]visualcontroller.DispatchEnvelope(nil), intake.dispatch...)}, nil
}

func (intake *fakeIntake) CompleteSpecialistPullRequest(_ string, completion visualcontroller.SpecialistPullRequestCompletion) error {
	intake.completes++
	if intake.failCompletion {
		intake.failCompletion = false
		return errors.New("completion checkpoint interrupted")
	}
	intake.completion = completion
	return nil
}

type fakeRepairer struct {
	runs    int
	outcome RepairOutcome
}

func (repairer *fakeRepairer) Run(context.Context, visualcontroller.DispatchEnvelope) (RepairOutcome, error) {
	repairer.runs++
	return repairer.outcome, nil
}

type fakeVerdictVerifier struct {
	calls   int
	receipt PullRequestVerdictReceipt
}

func (verifier *fakeVerdictVerifier) VerifyPullRequest(_ context.Context, request PullRequestVerdictRequest) (PullRequestVerdictReceipt, error) {
	verifier.calls++
	if request.PullRequestNumber <= 0 || request.HeadSHA != verifier.receipt.HeadSHA || request.IdempotencyKey == "" {
		return PullRequestVerdictReceipt{}, errors.New("verifier received an inexact PR request")
	}
	return verifier.receipt, nil
}
