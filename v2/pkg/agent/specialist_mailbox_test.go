package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const testSpecialistDiff = "diff --git a/tests/widget_test.go b/tests/widget_test.go\n--- a/tests/widget_test.go\n+++ b/tests/widget_test.go\n@@ -1 +1 @@\n-old\n+new\n"

func TestSpecialistMailboxPrepareIsContentBoundAndIdempotent(t *testing.T) {
	now := time.Date(2026, 7, 16, 1, 2, 3, 0, time.UTC)
	mailbox := newTestSpecialistMailbox(t, &now, SpecialistMailboxOptions{})
	request := testSpecialistRequest(now, "finding:visual:7", now.Add(2*time.Hour))

	first, err := mailbox.Prepare(request)
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	second, err := mailbox.Prepare(request)
	if err != nil {
		t.Fatalf("Prepare(idempotent) error = %v", err)
	}
	if !equalJSON(first, second) {
		t.Fatalf("idempotent orders differ:\n%+v\n%+v", first, second)
	}
	if first.SchemaVersion != SpecialistWorkOrderSchema || first.ID != "swo-"+first.RequestSHA256 || !digestPattern.MatchString(first.RequestSHA256) {
		t.Fatalf("unexpected content identity: %+v", first)
	}
	if err := mailbox.ValidateRequestForOrder(first, request); err != nil {
		t.Fatalf("ValidateRequestForOrder(exact) error = %v", err)
	}
	if got := strings.Join(first.AllowedPaths, ","); got != "src/widget.ts,tests/widget_test.go" {
		t.Fatalf("canonical allowed paths = %q", got)
	}
	paths, err := mailbox.Paths(first.ID)
	if err != nil {
		t.Fatal(err)
	}
	assertPrivateMode(t, paths.OrderDirectory, 0o700)
	assertPrivateMode(t, paths.Order, 0o600)

	otherMailbox := newTestSpecialistMailbox(t, &now, SpecialistMailboxOptions{})
	other, err := otherMailbox.Prepare(request)
	if err != nil {
		t.Fatal(err)
	}
	if other.ID != first.ID || other.RequestSHA256 != first.RequestSHA256 {
		t.Fatalf("same request was not deterministic: %s != %s", first.ID, other.ID)
	}

	mutations := map[string]func(*SpecialistWorkOrderRequest){
		"repository":  func(value *SpecialistWorkOrderRequest) { value.Repository = "acme/other" },
		"fingerprint": func(value *SpecialistWorkOrderRequest) { value.RepositoryFingerprint = strings.Repeat("9", 64) },
		"recurrence":  func(value *SpecialistWorkOrderRequest) { value.RecurrenceKey = "finding:visual:8" },
		"attempt":     func(value *SpecialistWorkOrderRequest) { value.Attempt++ },
		"base commit": func(value *SpecialistWorkOrderRequest) { value.BaseSHA = strings.Repeat("d", 40) },
		"base tree":   func(value *SpecialistWorkOrderRequest) { value.BaseTreeSHA = strings.Repeat("e", 40) },
		"bundle":      func(value *SpecialistWorkOrderRequest) { value.Evidence.BundleSHA256 = strings.Repeat("8", 64) },
		"run":         func(value *SpecialistWorkOrderRequest) { value.Evidence.WorkflowRunID++ },
		"artifact":    func(value *SpecialistWorkOrderRequest) { value.Evidence.ArtifactID++ },
		"role":        func(value *SpecialistWorkOrderRequest) { value.Specialist = SpecialistScanner },
		"reason":      func(value *SpecialistWorkOrderRequest) { value.RouteReason = "general discovery" },
		"paths":       func(value *SpecialistWorkOrderRequest) { value.AllowedPaths = append(value.AllowedPaths, "README.md") },
		"validation":  func(value *SpecialistWorkOrderRequest) { value.Validation = append(value.Validation, "go vet ./...") },
		"prompt": func(value *SpecialistWorkOrderRequest) {
			value.TaskPrompt += "\nExplain the evidence."
			value.TaskPromptSHA256 = sha256HexString(value.TaskPrompt)
		},
		"deadline": func(value *SpecialistWorkOrderRequest) { value.Deadline = value.Deadline.Add(time.Minute) },
	}
	for name, mutate := range mutations {
		t.Run("binds_"+name, func(t *testing.T) {
			changed := cloneSpecialistRequest(request)
			mutate(&changed)
			order, err := mailbox.Prepare(changed)
			if err != nil {
				t.Fatalf("Prepare(changed) error = %v", err)
			}
			if order.ID == first.ID || order.RequestSHA256 == first.RequestSHA256 {
				t.Fatalf("mutation %q did not change content identity", name)
			}
			if err := mailbox.ValidateRequestForOrder(first, changed); err == nil {
				t.Fatalf("ValidateRequestForOrder accepted %q drift", name)
			}
		})
	}
	now = request.Deadline.Add(time.Minute)
	if err := mailbox.ValidateRequestForOrder(first, request); err != nil {
		t.Fatalf("expired exact order could not be inspected safely: %v", err)
	}
}

func TestSpecialistMailboxRejectsExistingIDWithAlteredBody(t *testing.T) {
	now := time.Date(2026, 7, 16, 2, 0, 0, 0, time.UTC)
	mailbox := newTestSpecialistMailbox(t, &now, SpecialistMailboxOptions{})
	request := testSpecialistRequest(now, "finding:1", now.Add(time.Hour))
	order, err := mailbox.Prepare(request)
	if err != nil {
		t.Fatal(err)
	}
	paths, _ := mailbox.Paths(order.ID)
	encoded, err := os.ReadFile(paths.Order)
	if err != nil {
		t.Fatal(err)
	}
	var altered map[string]any
	if err := json.Unmarshal(encoded, &altered); err != nil {
		t.Fatal(err)
	}
	altered["route_reason"] = "tampered route"
	encoded, _ = json.Marshal(altered)
	if err := os.WriteFile(paths.Order, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := mailbox.Prepare(request); err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("Prepare(tampered ID) error = %v", err)
	}
}

func TestSpecialistMailboxValidatesRequestsAndPaths(t *testing.T) {
	now := time.Date(2026, 7, 16, 2, 30, 0, 0, time.UTC)
	mailbox := newTestSpecialistMailbox(t, &now, SpecialistMailboxOptions{})
	base := testSpecialistRequest(now, "finding:2", now.Add(time.Hour))
	tests := map[string]func(*SpecialistWorkOrderRequest){
		"unknown role":      func(value *SpecialistWorkOrderRequest) { value.Specialist = "generic" },
		"traversal":         func(value *SpecialistWorkOrderRequest) { value.AllowedPaths = []string{"../secret"} },
		"absolute path":     func(value *SpecialistWorkOrderRequest) { value.AllowedPaths = []string{"C:/secret"} },
		"prompt mismatch":   func(value *SpecialistWorkOrderRequest) { value.TaskPromptSHA256 = strings.Repeat("0", 64) },
		"unverified bundle": func(value *SpecialistWorkOrderRequest) { value.Evidence.VerificationReceiptSHA256 = "" },
		"past deadline":     func(value *SpecialistWorkOrderRequest) { value.Deadline = now.Add(-time.Second) },
		"missing command":   func(value *SpecialistWorkOrderRequest) { value.Validation = nil },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			request := cloneSpecialistRequest(base)
			mutate(&request)
			if _, err := mailbox.Prepare(request); err == nil {
				t.Fatal("Prepare() accepted invalid request")
			}
		})
	}
	if _, err := mailbox.Paths("../../escape"); err == nil {
		t.Fatal("Paths() accepted traversal")
	}
}

func TestSpecialistMailboxSupportsRepairWorkerContextEnvelope(t *testing.T) {
	now := time.Date(2026, 7, 16, 2, 45, 0, 0, time.UTC)
	mailbox := newTestSpecialistMailbox(t, &now, SpecialistMailboxOptions{})
	request := testSpecialistRequest(now, "finding:large-context", now.Add(time.Hour))
	request.TaskPrompt = strings.Repeat("x", 600<<10)
	request.TaskPromptSHA256 = sha256HexString(request.TaskPrompt)
	if _, err := mailbox.Prepare(request); err != nil {
		t.Fatalf("Prepare(600 KiB repair context) error = %v", err)
	}
	request.RecurrenceKey = "finding:oversize-context"
	request.TaskPrompt = strings.Repeat("x", maxSpecialistTaskPromptBytes+1)
	request.TaskPromptSHA256 = sha256HexString(request.TaskPrompt)
	if _, err := mailbox.Prepare(request); err == nil || !strings.Contains(err.Error(), "task prompt") {
		t.Fatalf("Prepare(oversize context) error = %v", err)
	}
}

func TestSpecialistMailboxLeaseIsExclusiveIdempotentAndRecoverable(t *testing.T) {
	now := time.Date(2026, 7, 16, 3, 0, 0, 0, time.UTC)
	mailbox := newTestSpecialistMailbox(t, &now, SpecialistMailboxOptions{})
	order, err := mailbox.Prepare(testSpecialistRequest(now, "finding:lease", now.Add(4*time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	first, err := mailbox.AcquireLease(order, "hive-controller", "quality-1", now.Add(time.Hour))
	if err != nil {
		t.Fatalf("AcquireLease() error = %v", err)
	}
	again, err := mailbox.AcquireLease(order, "hive-controller", "quality-1", now.Add(30*time.Minute))
	if err != nil || !equalJSON(first, again) {
		t.Fatalf("AcquireLease(idempotent) = %+v, %v", again, err)
	}
	if _, err := mailbox.AcquireLease(order, "other", "quality-2", now.Add(2*time.Hour)); !errors.Is(err, ErrSpecialistLeaseHeld) {
		t.Fatalf("AcquireLease(contender) error = %v", err)
	}
	paths, _ := mailbox.Paths(order.ID)
	assertPrivateMode(t, paths.Lease, 0o600)

	now = first.Deadline.Add(time.Second)
	recovered, err := mailbox.AcquireLease(order, "hive-controller", "quality-2", now.Add(time.Hour))
	if err != nil {
		t.Fatalf("AcquireLease(recovery) error = %v", err)
	}
	if recovered.SessionID != "quality-2" || recovered.LeaseSHA256 == first.LeaseSHA256 {
		t.Fatalf("expired lease was not safely replaced: %+v", recovered)
	}
	matches, err := filepath.Glob(paths.Lease + ".expired-*")
	if err != nil || len(matches) != 0 {
		t.Fatalf("expired lease quarantine was not cleaned: %v, %v", matches, err)
	}
}

func TestSpecialistMailboxAbandonsOnlyExactUnlaunchedLease(t *testing.T) {
	now := time.Date(2026, 7, 16, 3, 30, 0, 0, time.UTC)
	mailbox := newTestSpecialistMailbox(t, &now, SpecialistMailboxOptions{})
	order, err := mailbox.Prepare(testSpecialistRequest(now, "finding:unlaunched", now.Add(2*time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	lease, err := mailbox.AcquireLease(order, "hive-controller", "quality-1", now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	tampered := lease
	tampered.SessionID = "quality-other"
	if err := mailbox.ReleaseUndeliveredLease(order, tampered); err == nil || !strings.Contains(err.Error(), "mismatched") {
		t.Fatalf("mismatched lease abandonment = %v", err)
	}
	if err := mailbox.ReleaseUndeliveredLease(order, lease); err != nil {
		t.Fatal(err)
	}
	paths, _ := mailbox.Paths(order.ID)
	if _, err := os.Lstat(paths.Lease); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("abandoned lease remains: %v", err)
	}
	replacement, err := mailbox.AcquireLease(order, "hive-controller", "quality-2", now.Add(time.Hour))
	if err != nil || replacement.SessionID != "quality-2" {
		t.Fatalf("immediate retry did not acquire a new lease: %+v, %v", replacement, err)
	}
	matches, err := filepath.Glob(paths.Lease + ".abandoned-*")
	if err != nil || len(matches) != 0 {
		t.Fatalf("abandoned lease quarantine remains: %v, %v", matches, err)
	}
}

func TestSpecialistMailboxRefusesLeaseAbandonmentAfterAnyOutput(t *testing.T) {
	now := time.Date(2026, 7, 16, 3, 45, 0, 0, time.UTC)
	for _, output := range []string{"summary.txt", "completion.status", "proposal.diff", ".tmp-partial"} {
		t.Run(output, func(t *testing.T) {
			mailbox := newTestSpecialistMailbox(t, &now, SpecialistMailboxOptions{})
			request := testSpecialistRequest(now, "finding:output:"+strings.ReplaceAll(output, ".", "-"), now.Add(2*time.Hour))
			order, err := mailbox.Prepare(request)
			if err != nil {
				t.Fatal(err)
			}
			lease, err := mailbox.AcquireLease(order, "hive-controller", "quality-1", now.Add(time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			paths, _ := mailbox.Paths(order.ID)
			if err := os.WriteFile(filepath.Join(paths.OrderDirectory, output), []byte("partial"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := mailbox.ReleaseUndeliveredLease(order, lease); err == nil || !strings.Contains(err.Error(), "task output") {
				t.Fatalf("lease with %s output was abandoned: %v", output, err)
			}
			loaded, err := mailbox.LoadLease(order)
			if err != nil || !equalJSON(loaded, lease) {
				t.Fatalf("refused abandonment changed the lease: %+v, %v", loaded, err)
			}
		})
	}
}

func TestSpecialistMailboxReceiptRoundTripAndPendingSort(t *testing.T) {
	now := time.Date(2026, 7, 16, 4, 0, 0, 0, time.UTC)
	mailbox := newTestSpecialistMailbox(t, &now, SpecialistMailboxOptions{})
	late, err := mailbox.Prepare(testSpecialistRequest(now, "finding:late", now.Add(3*time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	early, err := mailbox.Prepare(testSpecialistRequest(now, "finding:early", now.Add(2*time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	pending, err := mailbox.ListPending()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 2 || pending[0].ID != early.ID || pending[1].ID != late.ID {
		t.Fatalf("ListPending() was not deadline sorted: %+v", pending)
	}
	lease, err := mailbox.AcquireLease(early, "hive-controller", "quality-session", now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	diff := []byte(testSpecialistDiff)
	receipt, err := NewSpecialistReceipt(early, lease, SpecialistCompletionProposed, diff, "Adds the missing deterministic test.", now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := mailbox.SubmitReceipt(early, lease, receipt, diff); err != nil {
		t.Fatalf("SubmitReceipt() error = %v", err)
	}
	if err := mailbox.SubmitReceipt(early, lease, receipt, diff); err != nil {
		t.Fatalf("SubmitReceipt(idempotent) error = %v", err)
	}
	loaded, loadedDiff, err := mailbox.LoadReceipt(early, lease)
	if err != nil {
		t.Fatalf("LoadReceipt() error = %v", err)
	}
	if !equalJSON(loaded, receipt) || string(loadedDiff) != string(diff) {
		t.Fatalf("loaded receipt differs: %+v", loaded)
	}
	loadedOrder, err := mailbox.LoadWorkOrder(early.ID)
	if err != nil || !equalJSON(loadedOrder, early) {
		t.Fatalf("LoadWorkOrder() = %+v, %v", loadedOrder, err)
	}
	loadedLease, loadedCompletion, replayDiff, err := mailbox.LoadCompletion(loadedOrder)
	if err != nil || !equalJSON(loadedLease, lease) || !equalJSON(loadedCompletion, receipt) || string(replayDiff) != string(diff) {
		t.Fatalf("LoadCompletion() = %+v, %+v, %q, %v", loadedLease, loadedCompletion, replayDiff, err)
	}
	if _, err := mailbox.AcquireLease(early, "hive-controller", "quality-rekick", now.Add(90*time.Minute)); !errors.Is(err, ErrSpecialistOrderComplete) {
		t.Fatalf("AcquireLease(completed) error = %v", err)
	}
	paths, _ := mailbox.Paths(early.ID)
	assertPrivateMode(t, paths.UnifiedDiff, 0o600)
	assertPrivateMode(t, paths.Receipt, 0o600)
	pending, err = mailbox.ListPending()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].ID != late.ID {
		t.Fatalf("completed order remains pending: %+v", pending)
	}
}

func TestSpecialistMailboxPersistsLateObservedInLeaseCompletion(t *testing.T) {
	now := time.Date(2026, 7, 16, 4, 0, 0, 0, time.UTC)
	mailbox := newTestSpecialistMailbox(t, &now, SpecialistMailboxOptions{})
	order, err := mailbox.Prepare(testSpecialistRequest(now, "finding:late-observation", now.Add(10*time.Minute)))
	if err != nil {
		t.Fatal(err)
	}
	lease, err := mailbox.AcquireLease(order, "controller", "session-late", now.Add(5*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := NewSpecialistReceipt(order, lease, SpecialistCompletionProposed, []byte(testSpecialistDiff), "Completed before the controller restarted.", now.Add(4*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	now = lease.Deadline.Add(time.Minute)
	if err := mailbox.SubmitReceipt(order, lease, receipt, []byte(testSpecialistDiff)); err != nil {
		t.Fatalf("late observation rejected an in-lease completion: %v", err)
	}
	if _, _, err := mailbox.LoadReceipt(order, lease); err != nil {
		t.Fatalf("late observed receipt did not round trip: %v", err)
	}
	if _, err := NewSpecialistReceipt(order, lease, SpecialistCompletionBlocked, nil, "Too late.", lease.Deadline.Add(time.Nanosecond)); err == nil {
		t.Fatal("out-of-lease completion time was accepted")
	}
}

func TestSpecialistMailboxRejectsMismatchedStaleSymlinkAndOversizeReceipts(t *testing.T) {
	now := time.Date(2026, 7, 16, 5, 0, 0, 0, time.UTC)
	mailbox := newTestSpecialistMailbox(t, &now, SpecialistMailboxOptions{})
	order, err := mailbox.Prepare(testSpecialistRequest(now, "finding:receipt", now.Add(3*time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	lease, err := mailbox.AcquireLease(order, "hive-controller", "quality-1", now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	diff := []byte(testSpecialistDiff)
	receipt, err := NewSpecialistReceipt(order, lease, SpecialistCompletionProposed, diff, "A bounded patch.", now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	mutations := map[string]func(*SpecialistReceipt){
		"request": func(value *SpecialistReceipt) { value.RequestSHA256 = strings.Repeat("1", 64) },
		"role":    func(value *SpecialistReceipt) { value.Specialist = SpecialistScanner },
		"session": func(value *SpecialistReceipt) { value.SessionID = "quality-2" },
		"base":    func(value *SpecialistReceipt) { value.BaseSHA = strings.Repeat("f", 40) },
		"tree":    func(value *SpecialistReceipt) { value.BaseTreeSHA = strings.Repeat("e", 40) },
		"task":    func(value *SpecialistReceipt) { value.TaskPromptSHA256 = strings.Repeat("2", 64) },
		"diff":    func(value *SpecialistReceipt) { value.UnifiedDiffSHA256 = strings.Repeat("3", 64) },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			changed := receipt
			mutate(&changed)
			changed.ReceiptSHA256, _ = digestReceipt(changed)
			if err := mailbox.SubmitReceipt(order, lease, changed, diff); err == nil {
				t.Fatal("SubmitReceipt() accepted mismatched receipt")
			}
		})
	}

	now = lease.Deadline.Add(time.Second)
	replacement, err := mailbox.AcquireLease(order, "hive-controller", "quality-2", now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := mailbox.SubmitReceipt(order, lease, receipt, diff); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("SubmitReceipt(stale) error = %v", err)
	}
	newReceipt, err := NewSpecialistReceipt(order, replacement, SpecialistCompletionProposed, diff, "Replacement patch.", now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := mailbox.SubmitReceipt(order, replacement, newReceipt, diff); err != nil {
		t.Fatal(err)
	}
	paths, _ := mailbox.Paths(order.ID)
	if err := os.Remove(paths.UnifiedDiff); err != nil {
		t.Fatal(err)
	}
	external := filepath.Join(t.TempDir(), "external.diff")
	if err := os.WriteFile(external, diff, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, paths.UnifiedDiff); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symlink unavailable: %v", err)
		}
		t.Fatal(err)
	}
	if _, _, err := mailbox.LoadReceipt(order, replacement); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("LoadReceipt(symlink) error = %v", err)
	}
}

func TestSpecialistMailboxRejectsOversizeFilesAndMalformedJSON(t *testing.T) {
	now := time.Date(2026, 7, 16, 6, 0, 0, 0, time.UTC)
	mailbox := newTestSpecialistMailbox(t, &now, SpecialistMailboxOptions{MaxReceiptBytes: 128, MaxUnifiedDiffBytes: 96})
	order, err := mailbox.Prepare(testSpecialistRequest(now, "finding:bounds", now.Add(2*time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	lease, err := mailbox.AcquireLease(order, "hive-controller", "quality", now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	diff := []byte(testSpecialistDiff + strings.Repeat("+padding\n", 20))
	receipt, err := NewSpecialistReceipt(order, lease, SpecialistCompletionProposed, diff, "Bounded patch.", now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := mailbox.SubmitReceipt(order, lease, receipt, diff); err == nil || !strings.Contains(err.Error(), "oversized") {
		t.Fatalf("SubmitReceipt(oversize diff) error = %v", err)
	}

	paths, _ := mailbox.Paths(order.ID)
	if err := os.WriteFile(paths.Receipt, []byte(`{"schema_version":"bad"} trailing`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := mailbox.LoadReceipt(order, lease); err == nil {
		t.Fatal("LoadReceipt() accepted malformed JSON")
	}
	if err := os.WriteFile(paths.Receipt, []byte(strings.Repeat("x", 129)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := mailbox.LoadReceipt(order, lease); err == nil || !strings.Contains(err.Error(), "size limit") {
		t.Fatalf("LoadReceipt(oversize JSON) error = %v", err)
	}
}

func TestSpecialistMailboxBlockedReceiptIsDiffFreeAndIdempotent(t *testing.T) {
	now := time.Date(2026, 7, 16, 6, 30, 0, 0, time.UTC)
	mailbox := newTestSpecialistMailbox(t, &now, SpecialistMailboxOptions{})
	order, err := mailbox.Prepare(testSpecialistRequest(now, "finding:blocked", now.Add(time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	lease, err := mailbox.AcquireLease(order, "hive-controller", "quality-blocked", now.Add(30*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewSpecialistReceipt(order, lease, SpecialistCompletionProposed, nil, "No patch.", now.Add(time.Minute)); err == nil {
		t.Fatal("NewSpecialistReceipt(proposed without diff) succeeded")
	}
	if _, err := NewSpecialistReceipt(order, lease, SpecialistCompletionBlocked, []byte(testSpecialistDiff), "Provider could not safely edit the target.", now.Add(time.Minute)); err == nil {
		t.Fatal("NewSpecialistReceipt(blocked with diff) succeeded")
	}
	if _, err := NewSpecialistReceipt(order, lease, "unknown", nil, "Unknown status.", now.Add(time.Minute)); err == nil {
		t.Fatal("NewSpecialistReceipt(unknown status) succeeded")
	}
	receipt, err := NewSpecialistReceipt(order, lease, SpecialistCompletionBlocked, nil, "Provider could not identify a safe repository-local change.", now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	tampered := receipt
	tampered.UnifiedDiffSHA256 = strings.Repeat("a", 64)
	tampered.ReceiptSHA256, _ = digestReceipt(tampered)
	if err := mailbox.SubmitReceipt(order, lease, tampered, nil); err == nil || !strings.Contains(err.Error(), "must not include") {
		t.Fatalf("SubmitReceipt(blocked with digest) error = %v", err)
	}
	if err := mailbox.SubmitReceipt(order, lease, receipt, nil); err != nil {
		t.Fatalf("SubmitReceipt(blocked) error = %v", err)
	}
	if err := mailbox.SubmitReceipt(order, lease, receipt, nil); err != nil {
		t.Fatalf("SubmitReceipt(blocked idempotent) error = %v", err)
	}
	loadedLease, loaded, diff, err := mailbox.LoadCompletion(order)
	if err != nil || !equalJSON(loadedLease, lease) || !equalJSON(loaded, receipt) || len(diff) != 0 {
		t.Fatalf("LoadCompletion(blocked) = %+v, %+v, %q, %v", loadedLease, loaded, diff, err)
	}
	paths, _ := mailbox.Paths(order.ID)
	if _, err := os.Lstat(paths.UnifiedDiff); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("blocked receipt wrote a diff file: %v", err)
	}
	if _, err := mailbox.AcquireLease(order, "hive-controller", "quality-rekick", now.Add(45*time.Minute)); !errors.Is(err, ErrSpecialistOrderComplete) {
		t.Fatalf("AcquireLease(completed blocked order) error = %v", err)
	}
}

func TestSpecialistMailboxWaitReceiptHonorsPollingAndContext(t *testing.T) {
	now := time.Date(2026, 7, 16, 7, 0, 0, 0, time.UTC)
	mailbox := newTestSpecialistMailbox(t, &now, SpecialistMailboxOptions{})
	order, err := mailbox.Prepare(testSpecialistRequest(now, "finding:wait", now.Add(time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	lease, err := mailbox.AcquireLease(order, "hive-controller", "quality", now.Add(30*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	diff := []byte(testSpecialistDiff)
	receipt, err := NewSpecialistReceipt(order, lease, SpecialistCompletionProposed, diff, "Asynchronous patch.", now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		time.Sleep(20 * time.Millisecond)
		done <- mailbox.SubmitReceipt(order, lease, receipt, diff)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	loaded, _, err := mailbox.WaitReceipt(ctx, order, lease, 5*time.Millisecond)
	if err != nil || loaded.ReceiptSHA256 != receipt.ReceiptSHA256 {
		t.Fatalf("WaitReceipt() = %+v, %v", loaded, err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	second, err := mailbox.Prepare(testSpecialistRequest(now, "finding:cancel", now.Add(time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	secondLease, err := mailbox.AcquireLease(second, "hive-controller", "quality-2", now.Add(30*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	canceled, cancelNow := context.WithCancel(context.Background())
	cancelNow()
	if _, _, err := mailbox.WaitReceipt(canceled, second, secondLease, time.Millisecond); !errors.Is(err, context.Canceled) {
		t.Fatalf("WaitReceipt(canceled) error = %v", err)
	}
	if _, _, err := mailbox.WaitReceipt(context.Background(), second, secondLease, 0); err == nil {
		t.Fatal("WaitReceipt() accepted zero polling interval")
	}
}

func newTestSpecialistMailbox(t *testing.T, now *time.Time, options SpecialistMailboxOptions) *SpecialistMailbox {
	t.Helper()
	options.Now = func() time.Time { return *now }
	mailbox, err := NewSpecialistMailbox(filepath.Join(t.TempDir(), "specialist-mailbox"), options)
	if err != nil {
		t.Fatalf("NewSpecialistMailbox() error = %v", err)
	}
	return mailbox
}

func testSpecialistRequest(now time.Time, recurrence string, deadline time.Time) SpecialistWorkOrderRequest {
	prompt := "Use verified Visual Hive evidence to add the smallest deterministic repository test. Do not write to GitHub."
	return SpecialistWorkOrderRequest{
		Repository:            "Acme/Widget",
		RepositoryFingerprint: strings.Repeat("a", 64),
		RecurrenceKey:         recurrence,
		Attempt:               1,
		BaseSHA:               strings.Repeat("b", 40),
		BaseTreeSHA:           strings.Repeat("c", 40),
		Evidence: SpecialistEvidenceIdentity{
			BundleSchemaVersion:       "visual-hive.hive-bundle.v3",
			BundleSHA256:              strings.Repeat("d", 64),
			VerificationReceiptSHA256: strings.Repeat("e", 64),
			WorkflowRunID:             12345,
			WorkflowRunAttempt:        2,
			WorkflowRunHeadSHA:        strings.Repeat("b", 40),
			ArtifactID:                67890,
			ArtifactName:              "visual-hive-evidence",
			ArtifactSHA256:            strings.Repeat("f", 64),
		},
		Specialist:       SpecialistQuality,
		RouteReason:      "verified test adequacy gap routes to quality",
		AllowedPaths:     []string{"tests/widget_test.go", `src\widget.ts`},
		Validation:       []string{"go test ./... -count=1"},
		TaskPrompt:       prompt,
		TaskPromptSHA256: sha256HexString(prompt),
		Deadline:         deadline,
	}
}

func cloneSpecialistRequest(value SpecialistWorkOrderRequest) SpecialistWorkOrderRequest {
	copy := value
	copy.AllowedPaths = append([]string(nil), value.AllowedPaths...)
	copy.Validation = append([]string(nil), value.Validation...)
	return copy
}

func sha256HexString(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func assertPrivateMode(t *testing.T, target string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != want {
		t.Fatalf("mode(%s) = %o, want %o", target, info.Mode().Perm(), want)
	}
}
