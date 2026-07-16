package repair

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kubestellar/hive/v2/pkg/agent"
)

const specialistProviderTestDiff = "diff --git a/src/value.txt b/src/value.txt\n--- a/src/value.txt\n+++ b/src/value.txt\n@@ -1 +1 @@\n-broken\n+fixed\n"

type fakeSpecialistDispatcher struct {
	identity      agent.SpecialistSessionIdentity
	checkErr      error
	inspectErr    error
	dispatchErr   error
	observeErr    error
	releaseErr    error
	dispatch      func(agent.SpecialistDispatchRequest)
	dispatchReply func(agent.SpecialistDispatchRequest) agent.SpecialistDispatchResult
	observeReply  func(agent.SpecialistTaskResponseRequest) agent.SpecialistTaskResponse
	checkCalls    int
	inspectCalls  int
	dispatchCalls []agent.SpecialistDispatchRequest
	observeCalls  []agent.SpecialistTaskResponseRequest
	releaseCalls  []agent.SpecialistDispatchRequest
}

func (f *fakeSpecialistDispatcher) CheckSpecialistRole(_ context.Context, _ agent.SpecialistRole) (agent.SpecialistSessionIdentity, error) {
	f.checkCalls++
	return f.identity, f.checkErr
}

func (f *fakeSpecialistDispatcher) InspectSpecialistRole(_ context.Context, _ agent.SpecialistRole) (agent.SpecialistSessionIdentity, error) {
	f.inspectCalls++
	return f.identity, f.inspectErr
}

func (f *fakeSpecialistDispatcher) DispatchSpecialistTask(_ context.Context, request agent.SpecialistDispatchRequest) (agent.SpecialistDispatchResult, error) {
	f.dispatchCalls = append(f.dispatchCalls, request)
	if f.dispatch != nil {
		f.dispatch(request)
	}
	if f.dispatchReply != nil {
		return f.dispatchReply(request), f.dispatchErr
	}
	return agent.SpecialistDispatchResult{SpecialistSessionIdentity: f.identity}, f.dispatchErr
}

func (f *fakeSpecialistDispatcher) ObserveSpecialistTaskResponse(_ context.Context, request agent.SpecialistTaskResponseRequest) (agent.SpecialistTaskResponse, error) {
	f.observeCalls = append(f.observeCalls, request)
	if f.observeReply != nil {
		return f.observeReply(request), f.observeErr
	}
	if f.observeErr != nil {
		return agent.SpecialistTaskResponse{}, f.observeErr
	}
	return agent.SpecialistTaskResponse{}, agent.ErrSpecialistTaskResponsePending
}

func (f *fakeSpecialistDispatcher) ReleaseSpecialistTask(role agent.SpecialistRole, taskID string) error {
	f.releaseCalls = append(f.releaseCalls, agent.SpecialistDispatchRequest{TaskID: taskID, Specialist: role})
	return f.releaseErr
}

type specialistProviderFixture struct {
	provider   *SpecialistProvider
	dispatcher *fakeSpecialistDispatcher
	mailbox    *agent.SpecialistMailbox
	worktree   string
	clock      *time.Time
	baseSHA    string
	baseTree   string
}

func TestSpecialistProviderReleasesOnlyDefinitelyUndeliveredLease(t *testing.T) {
	t.Run("typed non-delivery permits immediate exact retry", func(t *testing.T) {
		fixture := newSpecialistProviderFixture(t)
		prompt := "bounded retry prompt"
		fixture.dispatcher.dispatchErr = fmt.Errorf("%w: kick failed before delivery", agent.ErrSpecialistTaskNotDelivered)
		_, err := fixture.provider.Run(context.Background(), fixture.worktree, prompt)
		var runErr *ProviderRunError
		if err == nil || !errors.As(err, &runErr) || runErr.Launched {
			t.Fatalf("definite non-delivery result = %#v, %v", runErr, err)
		}
		pending, listErr := fixture.mailbox.ListPending()
		if listErr != nil || len(pending) != 1 {
			t.Fatalf("pending order after non-delivery = %+v, %v", pending, listErr)
		}
		if _, leaseErr := fixture.mailbox.LoadLease(pending[0]); !errors.Is(leaseErr, os.ErrNotExist) {
			t.Fatalf("definitely undelivered lease was retained: %v", leaseErr)
		}
		fixture.dispatcher.dispatchErr = nil
		fixture.dispatcher.dispatch = func(request agent.SpecialistDispatchRequest) {
			writeSpecialistOutput(t, fixture.mailbox, request.TaskID, agent.SpecialistCompletionBlocked, nil, "bounded retry completed")
		}
		_, err = fixture.provider.Run(context.Background(), fixture.worktree, prompt)
		if err == nil || !strings.Contains(err.Error(), "bounded retry completed") {
			t.Fatalf("exact retry did not consume the same blocked order: %v", err)
		}
		if len(fixture.dispatcher.dispatchCalls) != 2 || fixture.dispatcher.dispatchCalls[0].TaskID != fixture.dispatcher.dispatchCalls[1].TaskID {
			t.Fatalf("retry dispatch identities = %+v", fixture.dispatcher.dispatchCalls)
		}
	})

	t.Run("unclassified error preserves ambiguous lease", func(t *testing.T) {
		fixture := newSpecialistProviderFixture(t)
		fixture.dispatcher.dispatchErr = errors.New("third-party dispatcher returned an unknown error")
		_, err := fixture.provider.Run(context.Background(), fixture.worktree, "ambiguous dispatch prompt")
		var runErr *ProviderRunError
		if err == nil || !errors.As(err, &runErr) || !runErr.Launched {
			t.Fatalf("unclassified dispatch result = %#v, %v", runErr, err)
		}
		pending, listErr := fixture.mailbox.ListPending()
		if listErr != nil || len(pending) != 1 {
			t.Fatalf("ambiguous pending order = %+v, %v", pending, listErr)
		}
		if _, leaseErr := fixture.mailbox.LoadLease(pending[0]); leaseErr != nil {
			t.Fatalf("ambiguous dispatch lease was removed: %v", leaseErr)
		}
		if len(fixture.dispatcher.dispatchCalls) != 1 {
			t.Fatalf("ambiguous dispatcher call count = %d", len(fixture.dispatcher.dispatchCalls))
		}
	})
}

func TestSpecialistProviderRecoversExactPreparedAndLeasedOrdersWithoutDuplicates(t *testing.T) {
	t.Run("crash after prepare before lease dispatches exact order once", func(t *testing.T) {
		fixture := newSpecialistProviderFixture(t)
		order, err := fixture.provider.prepareInvocation(context.Background(), fixture.worktree, "prepared crash window")
		if err != nil {
			t.Fatal(err)
		}
		fixture.dispatcher.dispatch = func(request agent.SpecialistDispatchRequest) {
			writeSpecialistOutput(t, fixture.mailbox, request.TaskID, agent.SpecialistCompletionProposed, []byte(specialistProviderTestDiff), "recovered prepared order")
		}
		result, err := fixture.provider.runPreparedInvocation(context.Background(), fixture.worktree, order.ID)
		if err != nil || !strings.Contains(result.Output, modelPatchBegin) {
			t.Fatalf("prepared recovery = %#v, %v", result, err)
		}
		if len(fixture.dispatcher.dispatchCalls) != 1 || fixture.dispatcher.dispatchCalls[0].TaskID != order.ID {
			t.Fatalf("prepared recovery dispatches = %+v", fixture.dispatcher.dispatchCalls)
		}
	})

	t.Run("live lease is observed and brokered without check or dispatch", func(t *testing.T) {
		fixture := newSpecialistProviderFixture(t)
		order, err := fixture.provider.prepareInvocation(context.Background(), fixture.worktree, "leased crash window")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.mailbox.AcquireLease(order, "hive-repair-controller", fixture.dispatcher.identity.SessionID, order.Deadline); err != nil {
			t.Fatal(err)
		}
		writeSpecialistOutput(t, fixture.mailbox, order.ID, agent.SpecialistCompletionProposed, []byte(specialistProviderTestDiff), "broker recovered completion")
		fixture.dispatcher.releaseErr = agent.ErrSpecialistTaskLeaseAbsent
		result, err := fixture.provider.runPreparedInvocation(context.Background(), fixture.worktree, order.ID)
		if err != nil || !strings.Contains(result.Output, modelPatchBegin) {
			t.Fatalf("leased recovery = %#v, %v", result, err)
		}
		if fixture.dispatcher.checkCalls != 0 || fixture.dispatcher.inspectCalls != 1 || len(fixture.dispatcher.dispatchCalls) != 0 {
			t.Fatalf("leased recovery check/inspect/dispatch = %d/%d/%d", fixture.dispatcher.checkCalls, fixture.dispatcher.inspectCalls, len(fixture.dispatcher.dispatchCalls))
		}
	})

	t.Run("expired ambiguous lease never redispatches", func(t *testing.T) {
		fixture := newSpecialistProviderFixture(t)
		order, err := fixture.provider.prepareInvocation(context.Background(), fixture.worktree, "expired lease")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.mailbox.AcquireLease(order, "hive-repair-controller", fixture.dispatcher.identity.SessionID, order.Deadline); err != nil {
			t.Fatal(err)
		}
		*fixture.clock = order.Deadline.Add(time.Second)
		_, err = fixture.provider.runPreparedInvocation(context.Background(), fixture.worktree, order.ID)
		var runErr *ProviderRunError
		if err == nil || !errors.As(err, &runErr) || !runErr.Launched || !strings.Contains(err.Error(), "expired ambiguous lease") {
			t.Fatalf("expired lease result = %#v, %v", runErr, err)
		}
		if fixture.dispatcher.checkCalls != 0 || fixture.dispatcher.inspectCalls != 1 || len(fixture.dispatcher.dispatchCalls) != 0 {
			t.Fatalf("expired lease reached a specialist: %d/%d/%d", fixture.dispatcher.checkCalls, fixture.dispatcher.inspectCalls, len(fixture.dispatcher.dispatchCalls))
		}
	})

	t.Run("expired lease recovers an in-deadline Codex completion", func(t *testing.T) {
		fixture := newSpecialistProviderFixture(t)
		order, err := fixture.provider.prepareInvocation(context.Background(), fixture.worktree, "expired completed lease")
		if err != nil {
			t.Fatal(err)
		}
		lease, err := fixture.mailbox.AcquireLease(order, "hive-repair-controller", fixture.dispatcher.identity.SessionID, order.Deadline)
		if err != nil {
			t.Fatal(err)
		}
		fixture.dispatcher.observeReply = func(request agent.SpecialistTaskResponseRequest) agent.SpecialistTaskResponse {
			return structuredSpecialistTaskResponseAt(t, fixture, request, agent.SpecialistCompletionProposed, []byte(specialistProviderTestDiff), "recovered completed response", lease.Deadline.Add(-time.Second))
		}
		fixture.dispatcher.releaseErr = agent.ErrSpecialistTaskLeaseAbsent
		*fixture.clock = lease.Deadline.Add(time.Second)
		result, err := fixture.provider.runPreparedInvocation(context.Background(), fixture.worktree, order.ID)
		if err != nil || !strings.Contains(result.Output, "+fixed") {
			t.Fatalf("expired completion recovery = %#v, %v", result, err)
		}
		if fixture.dispatcher.inspectCalls != 1 || len(fixture.dispatcher.dispatchCalls) != 0 || len(fixture.dispatcher.observeCalls) != 1 {
			t.Fatalf("expired completion inspect/dispatch/observe = %d/%d/%d", fixture.dispatcher.inspectCalls, len(fixture.dispatcher.dispatchCalls), len(fixture.dispatcher.observeCalls))
		}
	})
}

func TestSpecialistProviderProposalIsBrokeredAndCompletedReplayDoesNotRedispatch(t *testing.T) {
	fixture := newSpecialistProviderFixture(t)
	diff := []byte("diff --git a/src/value.txt b/src/value.txt\n--- a/src/value.txt\n+++ b/src/value.txt\n@@ -1 +1 @@\n-broken\n+fixed\n")
	fixture.dispatcher.observeReply = func(request agent.SpecialistTaskResponseRequest) agent.SpecialistTaskResponse {
		return structuredSpecialistTaskResponse(t, fixture, request, agent.SpecialistCompletionProposed, diff, "fixed the exact value")
	}

	if err := fixture.provider.Health(context.Background()); err != nil {
		t.Fatalf("Health() error = %v", err)
	}
	prompt := "  exact bounded task prompt\n"
	result, err := fixture.provider.Run(context.Background(), fixture.worktree, prompt)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Summary != "fixed the exact value" {
		t.Fatalf("Run() summary = %q", result.Summary)
	}
	wantOutput := modelPatchBegin + "\n" + strings.TrimSuffix(string(diff), "\n") + "\n" + modelPatchEnd
	if result.Output != wantOutput {
		t.Fatalf("Run() output = %q, want %q", result.Output, wantOutput)
	}
	if len(fixture.dispatcher.dispatchCalls) != 1 || len(fixture.dispatcher.observeCalls) != 1 || len(fixture.dispatcher.releaseCalls) != 1 {
		t.Fatalf("dispatch/observe/release calls = %d/%d/%d, want 1/1/1", len(fixture.dispatcher.dispatchCalls), len(fixture.dispatcher.observeCalls), len(fixture.dispatcher.releaseCalls))
	}
	request := fixture.dispatcher.dispatchCalls[0]
	if request.Specialist != agent.SpecialistQuality || request.TaskID == "" {
		t.Fatalf("dispatch request = %#v", request)
	}
	for _, required := range []string{"Do not call tools", "Do not read or write files", "Hive alone owns", "CANONICAL_WORK_ORDER_JSON", "CANONICAL_LEASE_JSON", "hive.specialist-model-result.v1", `"task_prompt":"  exact bounded task prompt\n"`, "HIVE_SPECIALIST_DISPATCH_READY_"} {
		if !strings.Contains(request.Message, required) {
			t.Fatalf("dispatch message is missing %q: %s", required, request.Message)
		}
	}
	if marker := agent.SpecialistDispatchReadyMarker(request.TaskID); marker == "" || !strings.HasSuffix(strings.TrimSpace(request.Message), marker) {
		t.Fatalf("dispatch message does not end in its task-specific readiness marker: %q", request.Message)
	}
	for _, forbidden := range []string{"completion.status", "proposal.diff", "summary.txt", "receipt.json", "hive specialist complete"} {
		if strings.Contains(request.Message, forbidden) {
			t.Fatalf("dispatch message contains forbidden filesystem/helper instruction %q: %s", forbidden, request.Message)
		}
	}
	order, err := fixture.mailbox.LoadWorkOrder(request.TaskID)
	if err != nil {
		t.Fatalf("LoadWorkOrder() error = %v", err)
	}
	if order.Repository != "daviddiaz0317/proof" || order.RepositoryFingerprint != strings.Repeat("a", 64) || order.RecurrenceKey != "finding:visual-1" || order.Attempt != 2 {
		t.Fatalf("work order lifecycle identity = %#v", order.SpecialistWorkOrderRequest)
	}
	if order.BaseSHA != fixture.baseSHA || order.BaseTreeSHA != fixture.baseTree || order.TaskPrompt != prompt {
		t.Fatalf("work order checkout/prompt identity = base %s tree %s prompt %q", order.BaseSHA, order.BaseTreeSHA, order.TaskPrompt)
	}
	if order.Evidence.WorkflowRunID != 41 || order.Evidence.ArtifactID != 73 || order.RouteReason != "test adequacy routes to quality" {
		t.Fatalf("work order evidence/route identity = %#v", order.SpecialistWorkOrderRequest)
	}
	lease, receipt, storedDiff, err := fixture.mailbox.LoadCompletion(order)
	if err != nil {
		t.Fatalf("LoadCompletion() error = %v", err)
	}
	if lease.SessionID != fixture.dispatcher.identity.SessionID || receipt.Status != agent.SpecialistCompletionProposed || !strings.Contains(string(storedDiff), "+fixed") {
		t.Fatalf("completion = lease %#v receipt %#v diff %q", lease, receipt, storedDiff)
	}

	// A completed content-identical order is consumed without even checking the
	// manager. This proves a crash/retry cannot duplicate specialist work.
	fixture.dispatcher.checkErr = errors.New("manager deliberately unavailable during replay")
	replayed, err := fixture.provider.Run(context.Background(), fixture.worktree, prompt)
	if err != nil {
		t.Fatalf("replayed Run() error = %v", err)
	}
	if replayed.Output != result.Output || len(fixture.dispatcher.dispatchCalls) != 1 || fixture.dispatcher.checkCalls != 2 {
		t.Fatalf("replay output/calls = %q dispatch=%d checks=%d", replayed.Output, len(fixture.dispatcher.dispatchCalls), fixture.dispatcher.checkCalls)
	}
}

func TestSpecialistProviderBlockedCompletionReturnsNoPatch(t *testing.T) {
	fixture := newSpecialistProviderFixture(t)
	fixture.dispatcher.observeReply = func(request agent.SpecialistTaskResponseRequest) agent.SpecialistTaskResponse {
		return structuredSpecialistTaskResponse(t, fixture, request, agent.SpecialistCompletionBlocked, nil, "verified context is insufficient")
	}

	result, err := fixture.provider.Run(context.Background(), fixture.worktree, "bounded prompt")
	if err == nil {
		t.Fatal("Run() error = nil, want blocked result")
	}
	var runErr *ProviderRunError
	var blocked *SpecialistBlockedError
	if !errors.As(err, &runErr) || !runErr.Launched || !errors.As(err, &blocked) {
		t.Fatalf("Run() error = %T %v, want launched SpecialistBlockedError", err, err)
	}
	if result.Output != "" || result.Summary != "verified context is insufficient" {
		t.Fatalf("blocked result = %#v", result)
	}
	if len(fixture.dispatcher.releaseCalls) != 1 {
		t.Fatalf("release calls = %d, want 1", len(fixture.dispatcher.releaseCalls))
	}
}

func TestSpecialistModelResultIsStrictAndContentBound(t *testing.T) {
	fixture := newSpecialistProviderFixture(t)
	order, err := fixture.provider.prepareInvocation(context.Background(), fixture.worktree, "strict model result")
	if err != nil {
		t.Fatal(err)
	}
	lease, err := fixture.mailbox.AcquireLease(order, "hive-repair-controller", fixture.dispatcher.identity.SessionID, order.Deadline)
	if err != nil {
		t.Fatal(err)
	}
	valid := specialistModelResult{
		SchemaVersion: specialistModelResultSchema, WorkOrderID: order.ID, RequestSHA256: order.RequestSHA256,
		LeaseSHA256: lease.LeaseSHA256, SessionID: lease.SessionID, Specialist: order.Specialist,
		BaseSHA: order.BaseSHA, BaseTreeSHA: order.BaseTreeSHA, TaskPromptSHA256: order.TaskPromptSHA256,
		Status: agent.SpecialistCompletionProposed, Summary: "bounded proposal", UnifiedDiff: specialistProviderTestDiff,
	}
	encoded, _ := json.Marshal(valid)
	if _, diff, err := parseSpecialistModelResult(string(encoded), order, lease); err != nil || !strings.Contains(string(diff), "+fixed") {
		t.Fatalf("valid result = %q, %v", diff, err)
	}

	tests := []struct {
		name  string
		value string
		want  string
	}{
		{name: "surrounding prose", value: "result: " + string(encoded), want: "decode strict"},
		{name: "second document", value: string(encoded) + " {}", want: "exactly one"},
		{name: "unknown field", value: strings.TrimSuffix(string(encoded), "}") + `,"authority":"merge"}`, want: "unknown field"},
		{name: "wrong request", value: strings.Replace(string(encoded), order.RequestSHA256, strings.Repeat("f", 64), 1), want: "identity"},
		{name: "blocked with diff", value: strings.Replace(string(encoded), `"status":"proposed"`, `"status":"blocked"`, 1), want: "empty unified_diff"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, _, err := parseSpecialistModelResult(test.value, order, lease); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestSpecialistProviderFailsClosedOnStaleCheckoutAndWrongRoleOrSession(t *testing.T) {
	t.Run("stale checkout", func(t *testing.T) {
		fixture := newSpecialistProviderFixture(t)
		fixture.provider.config.ExpectedBaseSHA = strings.Repeat("f", 40)
		_, err := fixture.provider.Run(context.Background(), fixture.worktree, "bounded prompt")
		if err == nil || !strings.Contains(err.Error(), "checkout is stale") {
			t.Fatalf("Run() error = %v, want stale checkout", err)
		}
		if fixture.dispatcher.checkCalls != 0 || len(fixture.dispatcher.dispatchCalls) != 0 {
			t.Fatalf("stale checkout reached specialist: checks=%d dispatch=%d", fixture.dispatcher.checkCalls, len(fixture.dispatcher.dispatchCalls))
		}
	})

	t.Run("wrong role", func(t *testing.T) {
		fixture := newSpecialistProviderFixture(t)
		fixture.dispatcher.identity.Specialist = agent.SpecialistScanner
		_, err := fixture.provider.Run(context.Background(), fixture.worktree, "bounded prompt")
		if err == nil || !strings.Contains(err.Error(), "role mismatch") {
			t.Fatalf("Run() error = %v, want role mismatch", err)
		}
		if len(fixture.dispatcher.dispatchCalls) != 0 {
			t.Fatalf("wrong role dispatched %d tasks", len(fixture.dispatcher.dispatchCalls))
		}
	})

	t.Run("wrong dispatch session", func(t *testing.T) {
		fixture := newSpecialistProviderFixture(t)
		fixture.dispatcher.dispatchReply = func(agent.SpecialistDispatchRequest) agent.SpecialistDispatchResult {
			identity := fixture.dispatcher.identity
			identity.SessionID = "hive-quality-restarted"
			return agent.SpecialistDispatchResult{SpecialistSessionIdentity: identity}
		}
		_, err := fixture.provider.Run(context.Background(), fixture.worktree, "bounded prompt")
		if err == nil || !strings.Contains(err.Error(), "does not match the durable work order lease") {
			t.Fatalf("Run() error = %v, want session mismatch", err)
		}
		var runErr *ProviderRunError
		if !errors.As(err, &runErr) || !runErr.Launched || len(fixture.dispatcher.releaseCalls) != 1 {
			t.Fatalf("session mismatch error/release = %#v/%d", runErr, len(fixture.dispatcher.releaseCalls))
		}
	})
}

func TestSpecialistProviderRejectsTamperedLeaseAndOutput(t *testing.T) {
	t.Run("lease", func(t *testing.T) {
		fixture := newSpecialistProviderFixture(t)
		fixture.dispatcher.dispatch = func(request agent.SpecialistDispatchRequest) {
			paths, err := fixture.mailbox.Paths(request.TaskID)
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := os.ReadFile(paths.Lease)
			if err != nil {
				t.Fatal(err)
			}
			var lease map[string]any
			if err := json.Unmarshal(encoded, &lease); err != nil {
				t.Fatal(err)
			}
			lease["session_id"] = "tampered-session"
			encoded, _ = json.MarshalIndent(lease, "", "  ")
			if err := os.WriteFile(paths.Lease, append(encoded, '\n'), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		_, err := fixture.provider.Run(context.Background(), fixture.worktree, "bounded prompt")
		if err == nil || (!strings.Contains(err.Error(), "digest mismatch") && !strings.Contains(err.Error(), "stale or mismatched")) {
			t.Fatalf("Run() error = %v, want tampered lease rejection", err)
		}
		if len(fixture.dispatcher.releaseCalls) != 0 {
			t.Fatalf("tampered live lease was released: %d", len(fixture.dispatcher.releaseCalls))
		}
	})

	t.Run("completion status", func(t *testing.T) {
		fixture := newSpecialistProviderFixture(t)
		fixture.dispatcher.dispatch = func(request agent.SpecialistDispatchRequest) {
			paths, _ := fixture.mailbox.Paths(request.TaskID)
			mustWritePrivateFile(t, filepath.Join(paths.OrderDirectory, "summary.txt"), []byte("attempted tamper"))
			mustWritePrivateFile(t, filepath.Join(paths.OrderDirectory, "completion.status"), []byte("resolved\n"))
		}
		_, err := fixture.provider.Run(context.Background(), fixture.worktree, "bounded prompt")
		if err == nil || !strings.Contains(err.Error(), "exactly proposed or blocked") {
			t.Fatalf("Run() error = %v, want invalid status rejection", err)
		}
	})
}

func TestSpecialistProviderTimeoutCancelAndPendingReplayDoNotDuplicateDispatch(t *testing.T) {
	t.Run("timeout and pending replay", func(t *testing.T) {
		fixture := newSpecialistProviderFixture(t)
		// Leave enough time for Windows Git process startup; the timeout is meant
		// to occur only after the specialist task has been dispatched.
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, err := fixture.provider.Run(ctx, fixture.worktree, "bounded prompt")
		if err == nil || !errors.Is(err, ErrSpecialistWorkPending) {
			t.Fatalf("Run() error = %v, want durable pending work", err)
		}
		if len(fixture.dispatcher.dispatchCalls) != 1 || len(fixture.dispatcher.releaseCalls) != 0 {
			t.Fatalf("first run dispatch/release = %d/%d", len(fixture.dispatcher.dispatchCalls), len(fixture.dispatcher.releaseCalls))
		}
		replayCtx, replayCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer replayCancel()
		_, err = fixture.provider.Run(replayCtx, fixture.worktree, "bounded prompt")
		if err == nil || !errors.Is(err, ErrSpecialistWorkPending) {
			t.Fatalf("pending replay error = %v", err)
		}
		if len(fixture.dispatcher.dispatchCalls) != 1 || fixture.dispatcher.inspectCalls != 1 {
			t.Fatalf("pending replay dispatch/inspect = %d/%d", len(fixture.dispatcher.dispatchCalls), fixture.dispatcher.inspectCalls)
		}
	})

	t.Run("cancel", func(t *testing.T) {
		fixture := newSpecialistProviderFixture(t)
		ctx, cancel := context.WithCancel(context.Background())
		fixture.dispatcher.dispatch = func(agent.SpecialistDispatchRequest) { cancel() }
		_, err := fixture.provider.Run(ctx, fixture.worktree, "different bounded prompt")
		if err == nil || !errors.Is(err, ErrSpecialistWorkPending) {
			t.Fatalf("Run() error = %v, want durable pending work", err)
		}
		if len(fixture.dispatcher.releaseCalls) != 0 {
			t.Fatalf("canceled live task was released: %d", len(fixture.dispatcher.releaseCalls))
		}
	})
}

func TestSpecialistProviderFailsClosedWhenMailboxIsOutsideRoleWorkDir(t *testing.T) {
	fixture := newSpecialistProviderFixture(t)
	fixture.dispatcher.identity.WorkDir = t.TempDir()
	_, err := fixture.provider.Run(context.Background(), fixture.worktree, "bounded prompt")
	if err == nil || !strings.Contains(err.Error(), "mailbox must be contained") {
		t.Fatalf("Run() error = %v, want mailbox containment rejection", err)
	}
	if len(fixture.dispatcher.dispatchCalls) != 0 {
		t.Fatalf("outside mailbox dispatched %d tasks", len(fixture.dispatcher.dispatchCalls))
	}
}

func newSpecialistProviderFixture(t *testing.T) specialistProviderFixture {
	t.Helper()
	worktree, baseSHA, baseTree := makeSpecialistTestRepository(t)
	now := time.Now().UTC().Truncate(time.Second)
	clock := &now
	roleWorkDir := t.TempDir()
	mailbox, err := agent.NewSpecialistMailbox(filepath.Join(roleWorkDir, ".hive-specialist-mailbox"), agent.SpecialistMailboxOptions{Now: func() time.Time { return *clock }})
	if err != nil {
		t.Fatalf("NewSpecialistMailbox() error = %v", err)
	}
	dispatcher := &fakeSpecialistDispatcher{identity: agent.SpecialistSessionIdentity{
		Specialist:     agent.SpecialistQuality,
		AgentName:      "quality",
		AgentID:        "quality",
		Backend:        "codex",
		SessionID:      "hive-quality-test",
		TmuxSession:    "hive-quality-test",
		WorkDir:        roleWorkDir,
		ProviderSHA256: strings.Repeat("e", 64),
	}}
	provider, err := NewSpecialistProvider(SpecialistProviderConfig{
		Dispatcher:            dispatcher,
		Mailbox:               mailbox,
		Repository:            "DavidDiaz0317/Proof",
		RepositoryFingerprint: strings.Repeat("a", 64),
		RecurrenceKey:         "finding:visual-1",
		Attempt:               2,
		ExpectedBaseSHA:       baseSHA,
		ExpectedBaseTreeSHA:   baseTree,
		Evidence: agent.SpecialistEvidenceIdentity{
			BundleSchemaVersion:       "visual-hive.hive-bundle.v3",
			BundleSHA256:              strings.Repeat("b", 64),
			VerificationReceiptSHA256: strings.Repeat("c", 64),
			WorkflowRunID:             41,
			WorkflowRunAttempt:        3,
			WorkflowRunHeadSHA:        baseSHA,
			ArtifactID:                73,
			ArtifactName:              "visual-hive-evidence",
			ArtifactSHA256:            strings.Repeat("d", 64),
		},
		Specialist:   agent.SpecialistQuality,
		RouteReason:  "test adequacy routes to quality",
		AllowedPaths: []string{"src/**"},
		Validation:   []string{"npm test"},
		Deadline:     now.Add(10 * time.Minute),
		PollInterval: 5 * time.Millisecond,
		Now:          func() time.Time { return *clock },
	})
	if err != nil {
		t.Fatalf("NewSpecialistProvider() error = %v", err)
	}
	return specialistProviderFixture{provider: provider, dispatcher: dispatcher, mailbox: mailbox, worktree: worktree, clock: clock, baseSHA: baseSHA, baseTree: baseTree}
}

func makeSpecialistTestRepository(t *testing.T) (string, string, string) {
	t.Helper()
	root := t.TempDir()
	runSpecialistTestGit(t, root, "init", "--initial-branch=main")
	runSpecialistTestGit(t, root, "config", "user.name", "Hive Test")
	runSpecialistTestGit(t, root, "config", "user.email", "hive@example.invalid")
	if err := os.MkdirAll(filepath.Join(root, "src"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "src", "value.txt"), []byte("broken\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runSpecialistTestGit(t, root, "add", "--", "src/value.txt")
	runSpecialistTestGit(t, root, "commit", "-m", "initial")
	baseSHA := strings.TrimSpace(runSpecialistTestGit(t, root, "rev-parse", "HEAD"))
	baseTree := strings.TrimSpace(runSpecialistTestGit(t, root, "rev-parse", "HEAD^{tree}"))
	return root, baseSHA, baseTree
}

func runSpecialistTestGit(t *testing.T, directory string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "git", args...)
	command.Dir = directory
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, output)
	}
	return string(output)
}

func writeSpecialistOutput(t *testing.T, mailbox *agent.SpecialistMailbox, orderID string, status agent.SpecialistCompletionStatus, diff []byte, summary string) {
	t.Helper()
	paths, err := mailbox.Paths(orderID)
	if err != nil {
		t.Fatal(err)
	}
	mustWritePrivateFile(t, filepath.Join(paths.OrderDirectory, "summary.txt"), []byte(summary+"\n"))
	if status == agent.SpecialistCompletionProposed {
		mustWritePrivateFile(t, paths.UnifiedDiff, diff)
	}
	mustWritePrivateFile(t, filepath.Join(paths.OrderDirectory, "completion.status"), []byte(string(status)+"\n"))
}

func mustWritePrivateFile(t *testing.T, path string, value []byte) {
	t.Helper()
	if err := os.WriteFile(path, value, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatalf("chmod %s: %v", path, err)
	}
}

func structuredSpecialistTaskResponse(t *testing.T, fixture specialistProviderFixture, request agent.SpecialistTaskResponseRequest, status agent.SpecialistCompletionStatus, diff []byte, summary string) agent.SpecialistTaskResponse {
	return structuredSpecialistTaskResponseAt(t, fixture, request, status, diff, summary, fixture.clock.UTC())
}

func structuredSpecialistTaskResponseAt(t *testing.T, fixture specialistProviderFixture, request agent.SpecialistTaskResponseRequest, status agent.SpecialistCompletionStatus, diff []byte, summary string, completedAt time.Time) agent.SpecialistTaskResponse {
	t.Helper()
	order, err := fixture.mailbox.LoadWorkOrder(request.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := fixture.mailbox.LoadLease(order)
	if err != nil {
		t.Fatal(err)
	}
	result := specialistModelResult{
		SchemaVersion: specialistModelResultSchema, WorkOrderID: order.ID, RequestSHA256: order.RequestSHA256,
		LeaseSHA256: lease.LeaseSHA256, SessionID: lease.SessionID, Specialist: order.Specialist,
		BaseSHA: order.BaseSHA, BaseTreeSHA: order.BaseTreeSHA, TaskPromptSHA256: order.TaskPromptSHA256,
		Status: status, Summary: summary, UnifiedDiff: string(diff),
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	dispatchDigest := sha256.Sum256([]byte(request.Message))
	responseDigest := sha256.Sum256(encoded)
	return agent.SpecialistTaskResponse{
		SchemaVersion: agent.SpecialistTaskResponseSchema, WorkOrderID: order.ID, Specialist: order.Specialist,
		SessionID: lease.SessionID, TurnID: "turn-specialist-test", ProviderSHA256: fixture.dispatcher.identity.ProviderSHA256,
		DispatchSHA256: hex.EncodeToString(dispatchDigest[:]), ResponseSHA256: hex.EncodeToString(responseDigest[:]),
		Response: string(encoded), CompletedAt: completedAt.UTC(),
	}
}
