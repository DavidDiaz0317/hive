package agent

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	SpecialistWorkOrderSchema = "hive.specialist-work-order.v1"
	SpecialistReceiptSchema   = "hive.specialist-receipt.v1"
	SpecialistLeaseSchema     = "hive.specialist-lease.v1"

	defaultMaxWorkOrderBytes     = int64(2 << 20)
	defaultMaxReceiptBytes       = int64(512 << 10)
	defaultMaxUnifiedDiffBytes   = int64(8 << 20)
	defaultMaxLeaseBytes         = int64(64 << 10)
	maxSpecialistTaskPromptBytes = 768 << 10
)

var (
	ErrSpecialistReceiptPending = errors.New("specialist receipt is pending")
	ErrSpecialistLeaseHeld      = errors.New("specialist work order lease is held")
	ErrSpecialistOrderComplete  = errors.New("specialist work order is complete")

	digestPattern       = regexp.MustCompile(`^[a-f0-9]{64}$`)
	gitObjectPattern    = regexp.MustCompile(`^(?:[a-f0-9]{40}|[a-f0-9]{64})$`)
	repositoryPattern   = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9_.-]{0,99})/[a-z0-9](?:[a-z0-9_.-]{0,99})$`)
	workOrderIDPattern  = regexp.MustCompile(`^swo-[a-f0-9]{64}$`)
	safeIdentityPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:@/-]{0,255}$`)
)

// SpecialistRole is one of Hive's existing persistent specialist roles.
type SpecialistRole string

const (
	SpecialistQuality      SpecialistRole = "quality"
	SpecialistCIMaintainer SpecialistRole = "ci-maintainer"
	SpecialistSecurity     SpecialistRole = "sec-check"
	SpecialistArchitect    SpecialistRole = "architect"
	SpecialistScanner      SpecialistRole = "scanner"
)

// SpecialistCompletionStatus distinguishes a bounded proposed patch from a
// safe no-patch result. Blocked receipts never carry a diff.
type SpecialistCompletionStatus string

const (
	SpecialistCompletionProposed SpecialistCompletionStatus = "proposed"
	SpecialistCompletionBlocked  SpecialistCompletionStatus = "blocked"
)

// SpecialistEvidenceIdentity binds a work order to evidence already verified
// by Hive. The mailbox does not perform provenance verification itself.
type SpecialistEvidenceIdentity struct {
	BundleSchemaVersion       string `json:"bundle_schema_version"`
	BundleSHA256              string `json:"bundle_sha256"`
	VerificationReceiptSHA256 string `json:"verification_receipt_sha256"`
	WorkflowRunID             uint64 `json:"workflow_run_id"`
	WorkflowRunAttempt        uint64 `json:"workflow_run_attempt"`
	WorkflowRunHeadSHA        string `json:"workflow_run_head_sha"`
	ArtifactID                uint64 `json:"artifact_id"`
	ArtifactName              string `json:"artifact_name"`
	ArtifactSHA256            string `json:"artifact_sha256"`
}

// SpecialistWorkOrderRequest is the caller-owned, content-bound task input.
// Prepare derives the immutable ID and request digest from every field here.
type SpecialistWorkOrderRequest struct {
	Repository            string                     `json:"repository"`
	RepositoryFingerprint string                     `json:"repository_fingerprint"`
	RecurrenceKey         string                     `json:"recurrence_key"`
	Attempt               uint64                     `json:"attempt"`
	BaseSHA               string                     `json:"base_sha"`
	BaseTreeSHA           string                     `json:"base_tree_sha"`
	Evidence              SpecialistEvidenceIdentity `json:"evidence"`
	Specialist            SpecialistRole             `json:"specialist"`
	RouteReason           string                     `json:"route_reason"`
	AllowedPaths          []string                   `json:"allowed_paths"`
	Validation            []string                   `json:"validation"`
	TaskPrompt            string                     `json:"task_prompt"`
	TaskPromptSHA256      string                     `json:"task_prompt_sha256"`
	Deadline              time.Time                  `json:"deadline"`
}

// SpecialistWorkOrder is an immutable request stored in the mailbox.
type SpecialistWorkOrder struct {
	SchemaVersion string `json:"schema_version"`
	ID            string `json:"id"`
	RequestSHA256 string `json:"request_sha256"`
	SpecialistWorkOrderRequest
}

// SpecialistLease durably assigns one immutable work order to one specialist
// session. Its digest is included in the eventual receipt.
type SpecialistLease struct {
	SchemaVersion string         `json:"schema_version"`
	WorkOrderID   string         `json:"work_order_id"`
	RequestSHA256 string         `json:"request_sha256"`
	Specialist    SpecialistRole `json:"specialist"`
	Owner         string         `json:"owner"`
	SessionID     string         `json:"session_id"`
	AcquiredAt    time.Time      `json:"acquired_at"`
	Deadline      time.Time      `json:"deadline"`
	LeaseSHA256   string         `json:"lease_sha256"`
}

// SpecialistReceipt binds a proposed patch to the exact request, checkout,
// role, lease, session, and task prompt that produced it.
type SpecialistReceipt struct {
	SchemaVersion     string                     `json:"schema_version"`
	Status            SpecialistCompletionStatus `json:"status"`
	WorkOrderID       string                     `json:"work_order_id"`
	RequestSHA256     string                     `json:"request_sha256"`
	Specialist        SpecialistRole             `json:"specialist"`
	LeaseOwner        string                     `json:"lease_owner"`
	SessionID         string                     `json:"session_id"`
	LeaseSHA256       string                     `json:"lease_sha256"`
	BaseSHA           string                     `json:"base_sha"`
	BaseTreeSHA       string                     `json:"base_tree_sha"`
	TaskPromptSHA256  string                     `json:"task_prompt_sha256"`
	UnifiedDiffSHA256 string                     `json:"unified_diff_sha256"`
	CompletedAt       time.Time                  `json:"completed_at"`
	Summary           string                     `json:"summary"`
	ReceiptSHA256     string                     `json:"receipt_sha256"`
}

// SpecialistMailboxPaths are safe, deterministic paths the existing agent
// manager can place into a role-specific task packet.
type SpecialistMailboxPaths struct {
	OrderDirectory string
	Order          string
	UnifiedDiff    string
	Receipt        string
	Lease          string
}

type SpecialistMailboxOptions struct {
	MaxWorkOrderBytes   int64
	MaxReceiptBytes     int64
	MaxUnifiedDiffBytes int64
	Now                 func() time.Time
}

// SpecialistMailbox is a durable, file-backed handoff boundary. It contains no
// provider, repair, integrated-controller, or Visual Hive dependencies.
type SpecialistMailbox struct {
	root                string
	orders              string
	leases              string
	maxWorkOrderBytes   int64
	maxReceiptBytes     int64
	maxUnifiedDiffBytes int64
	now                 func() time.Time
}

// NewSpecialistMailbox creates or opens a secure mailbox rooted at an absolute
// path. Mailbox directories are private to the current OS account.
func NewSpecialistMailbox(root string, options SpecialistMailboxOptions) (*SpecialistMailbox, error) {
	if strings.TrimSpace(root) == "" || !filepath.IsAbs(root) {
		return nil, errors.New("specialist mailbox root must be an absolute path")
	}
	root = filepath.Clean(root)
	mailbox := &SpecialistMailbox{
		root:                root,
		orders:              filepath.Join(root, "orders"),
		leases:              filepath.Join(root, "leases"),
		maxWorkOrderBytes:   boundedLimit(options.MaxWorkOrderBytes, defaultMaxWorkOrderBytes),
		maxReceiptBytes:     boundedLimit(options.MaxReceiptBytes, defaultMaxReceiptBytes),
		maxUnifiedDiffBytes: boundedLimit(options.MaxUnifiedDiffBytes, defaultMaxUnifiedDiffBytes),
		now:                 options.Now,
	}
	if mailbox.now == nil {
		mailbox.now = time.Now
	}
	for _, directory := range []string{mailbox.root, mailbox.orders, mailbox.leases} {
		if err := secureDirectory(directory); err != nil {
			return nil, fmt.Errorf("secure specialist mailbox directory: %w", err)
		}
	}
	return mailbox, nil
}

// Prepare validates and atomically persists one immutable work order. Repeating
// the same request returns the same order; altered content under its ID fails.
func (m *SpecialistMailbox) Prepare(request SpecialistWorkOrderRequest) (SpecialistWorkOrder, error) {
	if err := requireSecureDirectory(m.orders); err != nil {
		return SpecialistWorkOrder{}, err
	}
	normalized, err := m.normalizeRequest(request, true)
	if err != nil {
		return SpecialistWorkOrder{}, err
	}
	digest, err := digestJSON(normalized)
	if err != nil {
		return SpecialistWorkOrder{}, err
	}
	order := SpecialistWorkOrder{
		SchemaVersion:              SpecialistWorkOrderSchema,
		ID:                         "swo-" + digest,
		RequestSHA256:              digest,
		SpecialistWorkOrderRequest: normalized,
	}
	paths, err := m.Paths(order.ID)
	if err != nil {
		return SpecialistWorkOrder{}, err
	}
	if _, err := os.Lstat(paths.OrderDirectory); err == nil {
		stored, loadErr := m.loadOrder(order.ID)
		if loadErr != nil {
			return SpecialistWorkOrder{}, loadErr
		}
		if !equalJSON(stored, order) {
			return SpecialistWorkOrder{}, errors.New("specialist work order ID already exists with different content")
		}
		return stored, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return SpecialistWorkOrder{}, err
	}

	temporary, err := os.MkdirTemp(m.orders, ".prepare-")
	if err != nil {
		return SpecialistWorkOrder{}, err
	}
	defer os.RemoveAll(temporary)
	if err := os.Chmod(temporary, 0o700); err != nil {
		return SpecialistWorkOrder{}, err
	}
	encoded, err := json.MarshalIndent(order, "", "  ")
	if err != nil {
		return SpecialistWorkOrder{}, err
	}
	encoded = append(encoded, '\n')
	if int64(len(encoded)) > m.maxWorkOrderBytes {
		return SpecialistWorkOrder{}, errors.New("specialist work order exceeds size limit")
	}
	if err := atomicWriteFile(filepath.Join(temporary, "order.json"), encoded, false); err != nil {
		return SpecialistWorkOrder{}, err
	}
	if err := os.Rename(temporary, paths.OrderDirectory); err != nil {
		stored, loadErr := m.loadOrder(order.ID)
		if loadErr != nil {
			return SpecialistWorkOrder{}, fmt.Errorf("publish specialist work order: %w", err)
		}
		if !equalJSON(stored, order) {
			return SpecialistWorkOrder{}, errors.New("specialist work order ID already exists with different content")
		}
		return stored, nil
	}
	return order, nil
}

// Paths resolves paths only for a structurally valid content-derived order ID.
func (m *SpecialistMailbox) Paths(orderID string) (SpecialistMailboxPaths, error) {
	if !workOrderIDPattern.MatchString(orderID) {
		return SpecialistMailboxPaths{}, errors.New("invalid specialist work order ID")
	}
	directory := filepath.Join(m.orders, orderID)
	return SpecialistMailboxPaths{
		OrderDirectory: directory,
		Order:          filepath.Join(directory, "order.json"),
		UnifiedDiff:    filepath.Join(directory, "proposal.diff"),
		Receipt:        filepath.Join(directory, "receipt.json"),
		Lease:          filepath.Join(m.leases, orderID+".json"),
	}, nil
}

// LoadWorkOrder returns one fully validated immutable order by its exact
// content-derived ID. Callers never need to parse mailbox files themselves.
func (m *SpecialistMailbox) LoadWorkOrder(orderID string) (SpecialistWorkOrder, error) {
	if err := requireSecureDirectory(m.orders); err != nil {
		return SpecialistWorkOrder{}, err
	}
	return m.loadOrder(orderID)
}

// ValidateRequestForOrder proves that current controller configuration still
// canonicalizes to the exact immutable order. Recovery uses the stored prompt
// and allows an expired deadline to be inspected, but never to be reacquired.
func (m *SpecialistMailbox) ValidateRequestForOrder(order SpecialistWorkOrder, request SpecialistWorkOrderRequest) error {
	stored, err := m.requireStoredOrder(order)
	if err != nil {
		return err
	}
	normalized, err := m.normalizeRequest(request, false)
	if err != nil {
		return err
	}
	digest, err := digestJSON(normalized)
	if err != nil {
		return err
	}
	expected := SpecialistWorkOrder{
		SchemaVersion:              SpecialistWorkOrderSchema,
		ID:                         "swo-" + digest,
		RequestSHA256:              digest,
		SpecialistWorkOrderRequest: normalized,
	}
	if !equalJSON(stored, expected) {
		return errors.New("current specialist request does not match the durable work order")
	}
	return nil
}

// LoadLease returns the fully validated durable lease for an exact order.
func (m *SpecialistMailbox) LoadLease(order SpecialistWorkOrder) (SpecialistLease, error) {
	if err := requireSecureDirectory(m.leases); err != nil {
		return SpecialistLease{}, err
	}
	stored, err := m.requireStoredOrder(order)
	if err != nil {
		return SpecialistLease{}, err
	}
	return m.loadLease(stored)
}

// ReleaseUndeliveredLease removes only the exact durable lease supplied by the
// controller after dispatch proved that no specialist kick was delivered. It
// refuses any receipt, proposal, status marker, summary, temporary output, or
// lease drift so a possibly launched task can never be made dispatchable again.
func (m *SpecialistMailbox) ReleaseUndeliveredLease(order SpecialistWorkOrder, lease SpecialistLease) error {
	if err := requireSecureDirectory(m.leases); err != nil {
		return err
	}
	stored, err := m.requireStoredOrder(order)
	if err != nil {
		return err
	}
	active, err := m.loadLease(stored)
	if err != nil {
		return err
	}
	if !equalJSON(active, lease) {
		return errors.New("refuse to abandon a stale or mismatched specialist lease")
	}
	paths, _ := m.Paths(stored.ID)
	if err := requireUnstartedSpecialistOrderDirectory(paths); err != nil {
		return err
	}
	quarantine, err := randomSibling(paths.Lease, ".abandoned-")
	if err != nil {
		return err
	}
	if err := os.Rename(paths.Lease, quarantine); err != nil {
		return fmt.Errorf("quarantine unlaunched specialist lease: %w", err)
	}
	restore := func(cause error) error {
		if _, currentErr := os.Lstat(paths.Lease); errors.Is(currentErr, os.ErrNotExist) {
			if restoreErr := os.Rename(quarantine, paths.Lease); restoreErr != nil {
				return errors.Join(cause, fmt.Errorf("restore specialist lease after failed abandonment: %w", restoreErr))
			}
		}
		return cause
	}
	encoded, err := readSecureRegularFile(quarantine, defaultMaxLeaseBytes)
	if err != nil {
		return restore(err)
	}
	var quarantined SpecialistLease
	if err := decodeStrict(encoded, &quarantined); err != nil || !equalJSON(quarantined, lease) {
		return restore(errors.New("specialist lease changed while abandoning an unlaunched dispatch"))
	}
	if err := requireUnstartedSpecialistOrderDirectory(paths); err != nil {
		return restore(err)
	}
	if err := os.Remove(quarantine); err != nil {
		return restore(fmt.Errorf("remove abandoned specialist lease: %w", err))
	}
	return nil
}

func requireUnstartedSpecialistOrderDirectory(paths SpecialistMailboxPaths) error {
	entries, err := os.ReadDir(paths.OrderDirectory)
	if err != nil {
		return err
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(paths.Order) || entries[0].Type()&os.ModeSymlink != 0 || !entries[0].Type().IsRegular() {
		return errors.New("refuse to abandon a specialist lease after any task output exists")
	}
	return nil
}

// LoadCompletion supports crash-safe provider replay: an identical resumed
// invocation can consume the verified existing receipt without re-kicking the
// specialist session.
func (m *SpecialistMailbox) LoadCompletion(order SpecialistWorkOrder) (SpecialistLease, SpecialistReceipt, []byte, error) {
	lease, err := m.LoadLease(order)
	if err != nil {
		return SpecialistLease{}, SpecialistReceipt{}, nil, err
	}
	receipt, diff, err := m.LoadReceipt(order, lease)
	if err != nil {
		return SpecialistLease{}, SpecialistReceipt{}, nil, err
	}
	return lease, receipt, diff, nil
}

// ListPending returns immutable orders without a receipt, sorted by deadline
// and then deterministic order ID.
func (m *SpecialistMailbox) ListPending() ([]SpecialistWorkOrder, error) {
	if err := requireSecureDirectory(m.orders); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(m.orders)
	if err != nil {
		return nil, err
	}
	pending := make([]SpecialistWorkOrder, 0, len(entries))
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".prepare-") {
			if entry.Type()&os.ModeSymlink != 0 || !entry.IsDir() {
				return nil, fmt.Errorf("unsafe temporary entry in specialist orders directory: %q", entry.Name())
			}
			continue
		}
		if entry.Type()&os.ModeSymlink != 0 || !entry.IsDir() || !workOrderIDPattern.MatchString(entry.Name()) {
			return nil, fmt.Errorf("unsafe entry in specialist orders directory: %q", entry.Name())
		}
		order, err := m.loadOrder(entry.Name())
		if err != nil {
			return nil, err
		}
		paths, _ := m.Paths(order.ID)
		if _, err := os.Lstat(paths.Receipt); err == nil {
			lease, err := m.loadLease(order)
			if err != nil {
				return nil, err
			}
			if _, _, err := m.LoadReceipt(order, lease); err != nil {
				return nil, err
			}
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		pending = append(pending, order)
	}
	sort.Slice(pending, func(i, j int) bool {
		if pending[i].Deadline.Equal(pending[j].Deadline) {
			return pending[i].ID < pending[j].ID
		}
		return pending[i].Deadline.Before(pending[j].Deadline)
	})
	return pending, nil
}

// AcquireLease creates a durable O_EXCL assignment. An identical owner/session
// may reopen its live lease. An expired lease is atomically quarantined before a
// new owner can acquire the order.
func (m *SpecialistMailbox) AcquireLease(order SpecialistWorkOrder, owner, sessionID string, deadline time.Time) (SpecialistLease, error) {
	if err := requireSecureDirectory(m.leases); err != nil {
		return SpecialistLease{}, err
	}
	stored, err := m.requireStoredOrder(order)
	if err != nil {
		return SpecialistLease{}, err
	}
	owner = strings.TrimSpace(owner)
	sessionID = strings.TrimSpace(sessionID)
	if !safeIdentityPattern.MatchString(owner) || !safeIdentityPattern.MatchString(sessionID) {
		return SpecialistLease{}, errors.New("safe specialist lease owner and session ID are required")
	}
	now := m.now().UTC()
	deadline = deadline.UTC()
	if !deadline.After(now) || deadline.After(stored.Deadline) {
		return SpecialistLease{}, errors.New("specialist lease deadline must be in the future and within the work order deadline")
	}
	paths, _ := m.Paths(stored.ID)
	if _, err := os.Lstat(paths.Receipt); err == nil {
		existingLease, leaseErr := m.loadLease(stored)
		if leaseErr != nil {
			return SpecialistLease{}, leaseErr
		}
		if _, _, receiptErr := m.LoadReceipt(stored, existingLease); receiptErr != nil {
			return SpecialistLease{}, receiptErr
		}
		return SpecialistLease{}, ErrSpecialistOrderComplete
	} else if !errors.Is(err, os.ErrNotExist) {
		return SpecialistLease{}, err
	}
	for attempts := 0; attempts < 4; attempts++ {
		lease := SpecialistLease{
			SchemaVersion: SpecialistLeaseSchema,
			WorkOrderID:   stored.ID, RequestSHA256: stored.RequestSHA256, Specialist: stored.Specialist,
			Owner: owner, SessionID: sessionID, AcquiredAt: now, Deadline: deadline,
		}
		lease.LeaseSHA256, err = digestLease(lease)
		if err != nil {
			return SpecialistLease{}, err
		}
		encoded, err := json.MarshalIndent(lease, "", "  ")
		if err != nil {
			return SpecialistLease{}, err
		}
		encoded = append(encoded, '\n')
		file, err := os.OpenFile(paths.Lease, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			writeErr := writeAndSync(file, encoded)
			if writeErr != nil {
				_ = os.Remove(paths.Lease)
				return SpecialistLease{}, writeErr
			}
			return lease, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return SpecialistLease{}, err
		}
		existing, err := m.loadLease(stored)
		if err != nil {
			return SpecialistLease{}, err
		}
		if existing.Owner == owner && existing.SessionID == sessionID && existing.Deadline.After(now) {
			if completed, completionErr := m.hasCompletion(stored); completionErr != nil {
				return SpecialistLease{}, completionErr
			} else if completed {
				return SpecialistLease{}, ErrSpecialistOrderComplete
			}
			return existing, nil
		}
		if existing.Deadline.After(now) {
			return SpecialistLease{}, fmt.Errorf("%w by %s/%s until %s", ErrSpecialistLeaseHeld, existing.Owner, existing.SessionID, existing.Deadline.Format(time.RFC3339Nano))
		}
		quarantine, err := randomSibling(paths.Lease, ".expired-")
		if err != nil {
			return SpecialistLease{}, err
		}
		if err := os.Rename(paths.Lease, quarantine); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return SpecialistLease{}, fmt.Errorf("quarantine expired specialist lease: %w", err)
		}
		_ = os.Remove(quarantine)
	}
	return SpecialistLease{}, errors.New("specialist lease changed repeatedly during acquisition")
}

// NewSpecialistReceipt creates a fully bound receipt for an already acquired
// lease and proposed unified diff.
func NewSpecialistReceipt(order SpecialistWorkOrder, lease SpecialistLease, status SpecialistCompletionStatus, unifiedDiff []byte, summary string, completedAt time.Time) (SpecialistReceipt, error) {
	if err := validateLeaseForOrder(order, lease); err != nil {
		return SpecialistReceipt{}, err
	}
	completedAt = completedAt.UTC()
	if completedAt.Before(lease.AcquiredAt) || completedAt.After(lease.Deadline) || completedAt.After(order.Deadline) {
		return SpecialistReceipt{}, errors.New("specialist receipt completion time is outside its lease")
	}
	summary = strings.TrimSpace(summary)
	if summary == "" || len(summary) > 16<<10 || strings.IndexByte(summary, 0) >= 0 {
		return SpecialistReceipt{}, errors.New("bounded specialist receipt summary is required")
	}
	receipt := SpecialistReceipt{
		SchemaVersion: SpecialistReceiptSchema,
		Status:        status,
		WorkOrderID:   order.ID, RequestSHA256: order.RequestSHA256, Specialist: order.Specialist,
		LeaseOwner: lease.Owner, SessionID: lease.SessionID, LeaseSHA256: lease.LeaseSHA256,
		BaseSHA: order.BaseSHA, BaseTreeSHA: order.BaseTreeSHA, TaskPromptSHA256: order.TaskPromptSHA256,
		CompletedAt: completedAt, Summary: summary,
	}
	switch status {
	case SpecialistCompletionProposed:
		if err := validateUnifiedDiff(unifiedDiff, defaultMaxUnifiedDiffBytes); err != nil {
			return SpecialistReceipt{}, err
		}
		if err := validateUnifiedDiffPaths(unifiedDiff, order.AllowedPaths); err != nil {
			return SpecialistReceipt{}, err
		}
		diffDigest := sha256.Sum256(unifiedDiff)
		receipt.UnifiedDiffSHA256 = hex.EncodeToString(diffDigest[:])
	case SpecialistCompletionBlocked:
		if len(unifiedDiff) != 0 {
			return SpecialistReceipt{}, errors.New("blocked specialist receipt must not include a unified diff")
		}
	default:
		return SpecialistReceipt{}, errors.New("specialist receipt status must be proposed or blocked")
	}
	digest, err := digestReceipt(receipt)
	if err != nil {
		return SpecialistReceipt{}, err
	}
	receipt.ReceiptSHA256 = digest
	return receipt, nil
}

// SubmitReceipt validates the active lease and writes the diff first and the
// receipt last. A byte-identical resubmission is idempotent.
func (m *SpecialistMailbox) SubmitReceipt(order SpecialistWorkOrder, lease SpecialistLease, receipt SpecialistReceipt, unifiedDiff []byte) error {
	stored, err := m.requireStoredOrder(order)
	if err != nil {
		return err
	}
	active, err := m.loadLease(stored)
	if err != nil {
		return err
	}
	if !equalJSON(active, lease) {
		return errors.New("specialist receipt lease is stale or mismatched")
	}
	// Broker observation may occur after a controller restart or the lease
	// deadline. Receipt authority is bound to the provider's completion time,
	// which validateReceipt requires to fall within this exact active lease and
	// immutable order. Do not reject a valid in-lease completion merely because
	// Hive persisted it later.
	if err := m.validateReceipt(stored, active, receipt, unifiedDiff); err != nil {
		return err
	}
	paths, _ := m.Paths(stored.ID)
	if _, err := os.Lstat(paths.Receipt); err == nil {
		existing, existingDiff, loadErr := m.LoadReceipt(stored, active)
		if loadErr != nil {
			return loadErr
		}
		if !equalJSON(existing, receipt) || !bytes.Equal(existingDiff, unifiedDiff) {
			return errors.New("specialist receipt already exists with different content")
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	wroteDiff := false
	if receipt.Status == SpecialistCompletionProposed {
		if existingDiff, err := readSecureRegularFile(paths.UnifiedDiff, m.maxUnifiedDiffBytes); err == nil {
			if !bytes.Equal(existingDiff, unifiedDiff) {
				return errors.New("specialist unified diff already exists with different content")
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		} else if err := atomicWriteFile(paths.UnifiedDiff, unifiedDiff, false); err != nil {
			return err
		} else {
			wroteDiff = true
		}
	} else if _, err := os.Lstat(paths.UnifiedDiff); err == nil {
		return errors.New("blocked specialist receipt conflicts with an existing unified diff")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	encoded, err := json.MarshalIndent(receipt, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	if int64(len(encoded)) > m.maxReceiptBytes {
		if wroteDiff {
			_ = os.Remove(paths.UnifiedDiff)
		}
		return errors.New("specialist receipt exceeds size limit")
	}
	if err := atomicWriteFile(paths.Receipt, encoded, false); err != nil {
		if wroteDiff {
			_ = os.Remove(paths.UnifiedDiff)
		}
		return err
	}
	return nil
}

// LoadReceipt strictly validates a completed receipt and its unified diff.
func (m *SpecialistMailbox) LoadReceipt(order SpecialistWorkOrder, lease SpecialistLease) (SpecialistReceipt, []byte, error) {
	stored, err := m.requireStoredOrder(order)
	if err != nil {
		return SpecialistReceipt{}, nil, err
	}
	active, err := m.loadLease(stored)
	if err != nil {
		return SpecialistReceipt{}, nil, err
	}
	if !equalJSON(active, lease) {
		return SpecialistReceipt{}, nil, errors.New("specialist receipt lease is stale or mismatched")
	}
	paths, _ := m.Paths(stored.ID)
	encoded, err := readSecureRegularFile(paths.Receipt, m.maxReceiptBytes)
	if errors.Is(err, os.ErrNotExist) {
		return SpecialistReceipt{}, nil, ErrSpecialistReceiptPending
	}
	if err != nil {
		return SpecialistReceipt{}, nil, err
	}
	var receipt SpecialistReceipt
	if err := decodeStrict(encoded, &receipt); err != nil {
		return SpecialistReceipt{}, nil, fmt.Errorf("decode specialist receipt: %w", err)
	}
	var diff []byte
	if receipt.Status == SpecialistCompletionProposed {
		diff, err = readSecureRegularFile(paths.UnifiedDiff, m.maxUnifiedDiffBytes)
		if err != nil {
			return SpecialistReceipt{}, nil, fmt.Errorf("read specialist unified diff: %w", err)
		}
	} else if receipt.Status == SpecialistCompletionBlocked {
		if _, err := os.Lstat(paths.UnifiedDiff); err == nil {
			return SpecialistReceipt{}, nil, errors.New("blocked specialist receipt must not have a unified diff file")
		} else if !errors.Is(err, os.ErrNotExist) {
			return SpecialistReceipt{}, nil, err
		}
	}
	if err := m.validateReceipt(stored, active, receipt, diff); err != nil {
		return SpecialistReceipt{}, nil, err
	}
	return receipt, diff, nil
}

// WaitReceipt polls at the caller-supplied interval until a valid receipt is
// available or the context is canceled. It never hides a malformed receipt.
func (m *SpecialistMailbox) WaitReceipt(ctx context.Context, order SpecialistWorkOrder, lease SpecialistLease, pollInterval time.Duration) (SpecialistReceipt, []byte, error) {
	if ctx == nil {
		return SpecialistReceipt{}, nil, errors.New("specialist receipt wait requires a context")
	}
	if pollInterval <= 0 {
		return SpecialistReceipt{}, nil, errors.New("specialist receipt polling interval must be positive")
	}
	for {
		receipt, diff, err := m.LoadReceipt(order, lease)
		if err == nil {
			return receipt, diff, nil
		}
		if !errors.Is(err, ErrSpecialistReceiptPending) {
			return SpecialistReceipt{}, nil, err
		}
		timer := time.NewTimer(pollInterval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return SpecialistReceipt{}, nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func (m *SpecialistMailbox) normalizeRequest(request SpecialistWorkOrderRequest, requireFuture bool) (SpecialistWorkOrderRequest, error) {
	request.Repository = strings.ToLower(strings.TrimSpace(request.Repository))
	request.RepositoryFingerprint = strings.ToLower(strings.TrimSpace(request.RepositoryFingerprint))
	request.RecurrenceKey = strings.TrimSpace(request.RecurrenceKey)
	request.BaseSHA = strings.ToLower(strings.TrimSpace(request.BaseSHA))
	request.BaseTreeSHA = strings.ToLower(strings.TrimSpace(request.BaseTreeSHA))
	request.RouteReason = strings.TrimSpace(request.RouteReason)
	request.TaskPromptSHA256 = strings.ToLower(strings.TrimSpace(request.TaskPromptSHA256))
	request.Deadline = request.Deadline.UTC()
	request.Evidence.BundleSchemaVersion = strings.TrimSpace(request.Evidence.BundleSchemaVersion)
	request.Evidence.BundleSHA256 = strings.ToLower(strings.TrimSpace(request.Evidence.BundleSHA256))
	request.Evidence.VerificationReceiptSHA256 = strings.ToLower(strings.TrimSpace(request.Evidence.VerificationReceiptSHA256))
	request.Evidence.WorkflowRunHeadSHA = strings.ToLower(strings.TrimSpace(request.Evidence.WorkflowRunHeadSHA))
	request.Evidence.ArtifactName = strings.TrimSpace(request.Evidence.ArtifactName)
	request.Evidence.ArtifactSHA256 = strings.ToLower(strings.TrimSpace(request.Evidence.ArtifactSHA256))
	if !repositoryPattern.MatchString(request.Repository) || !digestPattern.MatchString(request.RepositoryFingerprint) {
		return request, errors.New("valid repository identity and fingerprint are required")
	}
	if request.RecurrenceKey == "" || len(request.RecurrenceKey) > 1024 || strings.IndexByte(request.RecurrenceKey, 0) >= 0 || request.Attempt == 0 {
		return request, errors.New("bounded recurrence key and positive attempt are required")
	}
	if !gitObjectPattern.MatchString(request.BaseSHA) || !gitObjectPattern.MatchString(request.BaseTreeSHA) {
		return request, errors.New("exact base commit and tree object IDs are required")
	}
	if err := validateEvidence(request.Evidence); err != nil {
		return request, err
	}
	if !validSpecialistRole(request.Specialist) {
		return request, errors.New("work order must select one of Hive's five persistent specialist roles")
	}
	if request.RouteReason == "" || len(request.RouteReason) > 4096 || strings.IndexByte(request.RouteReason, 0) >= 0 {
		return request, errors.New("bounded specialist route reason is required")
	}
	paths, err := normalizeAllowedPaths(request.AllowedPaths)
	if err != nil {
		return request, err
	}
	request.AllowedPaths = paths
	validation, err := normalizeValidation(request.Validation)
	if err != nil {
		return request, err
	}
	request.Validation = validation
	if strings.TrimSpace(request.TaskPrompt) == "" || len(request.TaskPrompt) > maxSpecialistTaskPromptBytes || strings.IndexByte(request.TaskPrompt, 0) >= 0 {
		return request, errors.New("bounded specialist task prompt is required")
	}
	taskDigest := sha256.Sum256([]byte(request.TaskPrompt))
	if request.TaskPromptSHA256 != hex.EncodeToString(taskDigest[:]) {
		return request, errors.New("specialist task prompt digest mismatch")
	}
	if request.Deadline.IsZero() || (requireFuture && !request.Deadline.After(m.now().UTC())) {
		return request, errors.New("specialist work order deadline must be in the future")
	}
	return request, nil
}

func (m *SpecialistMailbox) requireStoredOrder(order SpecialistWorkOrder) (SpecialistWorkOrder, error) {
	if err := m.validateStoredOrder(order); err != nil {
		return SpecialistWorkOrder{}, err
	}
	stored, err := m.loadOrder(order.ID)
	if err != nil {
		return SpecialistWorkOrder{}, err
	}
	if !equalJSON(stored, order) {
		return SpecialistWorkOrder{}, errors.New("specialist work order does not match durable mailbox content")
	}
	return stored, nil
}

func (m *SpecialistMailbox) loadOrder(orderID string) (SpecialistWorkOrder, error) {
	paths, err := m.Paths(orderID)
	if err != nil {
		return SpecialistWorkOrder{}, err
	}
	if err := requireSecureDirectory(paths.OrderDirectory); err != nil {
		return SpecialistWorkOrder{}, err
	}
	encoded, err := readSecureRegularFile(paths.Order, m.maxWorkOrderBytes)
	if err != nil {
		return SpecialistWorkOrder{}, err
	}
	var order SpecialistWorkOrder
	if err := decodeStrict(encoded, &order); err != nil {
		return SpecialistWorkOrder{}, fmt.Errorf("decode specialist work order: %w", err)
	}
	if err := m.validateStoredOrder(order); err != nil {
		return SpecialistWorkOrder{}, err
	}
	if order.ID != orderID {
		return SpecialistWorkOrder{}, errors.New("specialist work order path and content IDs differ")
	}
	return order, nil
}

func (m *SpecialistMailbox) validateStoredOrder(order SpecialistWorkOrder) error {
	if order.SchemaVersion != SpecialistWorkOrderSchema {
		return errors.New("unsupported specialist work order schema")
	}
	normalized, err := m.normalizeRequest(order.SpecialistWorkOrderRequest, false)
	if err != nil {
		return err
	}
	if !equalJSON(normalized, order.SpecialistWorkOrderRequest) {
		return errors.New("specialist work order is not canonical")
	}
	digest, err := digestJSON(normalized)
	if err != nil {
		return err
	}
	if order.RequestSHA256 != digest || order.ID != "swo-"+digest {
		return errors.New("specialist work order content digest mismatch")
	}
	return nil
}

func (m *SpecialistMailbox) loadLease(order SpecialistWorkOrder) (SpecialistLease, error) {
	paths, _ := m.Paths(order.ID)
	encoded, err := readSecureRegularFile(paths.Lease, defaultMaxLeaseBytes)
	if err != nil {
		return SpecialistLease{}, err
	}
	var lease SpecialistLease
	if err := decodeStrict(encoded, &lease); err != nil {
		return SpecialistLease{}, fmt.Errorf("decode specialist lease: %w", err)
	}
	if err := validateLeaseForOrder(order, lease); err != nil {
		return SpecialistLease{}, err
	}
	return lease, nil
}

func (m *SpecialistMailbox) hasCompletion(order SpecialistWorkOrder) (bool, error) {
	paths, _ := m.Paths(order.ID)
	if _, err := os.Lstat(paths.Receipt); errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	lease, err := m.loadLease(order)
	if err != nil {
		return false, err
	}
	if _, _, err := m.LoadReceipt(order, lease); err != nil {
		return false, err
	}
	return true, nil
}

func (m *SpecialistMailbox) validateReceipt(order SpecialistWorkOrder, lease SpecialistLease, receipt SpecialistReceipt, unifiedDiff []byte) error {
	if receipt.SchemaVersion != SpecialistReceiptSchema || receipt.WorkOrderID != order.ID || receipt.RequestSHA256 != order.RequestSHA256 {
		return errors.New("specialist receipt request identity mismatch")
	}
	if receipt.Specialist != order.Specialist || receipt.LeaseOwner != lease.Owner || receipt.SessionID != lease.SessionID || receipt.LeaseSHA256 != lease.LeaseSHA256 {
		return errors.New("specialist receipt role, owner, session, or lease mismatch")
	}
	if receipt.BaseSHA != order.BaseSHA || receipt.BaseTreeSHA != order.BaseTreeSHA || receipt.TaskPromptSHA256 != order.TaskPromptSHA256 {
		return errors.New("specialist receipt checkout or task identity mismatch")
	}
	if receipt.CompletedAt.Location() != time.UTC || receipt.CompletedAt.Before(lease.AcquiredAt) || receipt.CompletedAt.After(lease.Deadline) || receipt.CompletedAt.After(order.Deadline) {
		return errors.New("specialist receipt completion time is outside its lease")
	}
	if strings.TrimSpace(receipt.Summary) == "" || receipt.Summary != strings.TrimSpace(receipt.Summary) || len(receipt.Summary) > 16<<10 || strings.IndexByte(receipt.Summary, 0) >= 0 {
		return errors.New("specialist receipt summary is invalid")
	}
	switch receipt.Status {
	case SpecialistCompletionProposed:
		if err := validateUnifiedDiff(unifiedDiff, m.maxUnifiedDiffBytes); err != nil {
			return err
		}
		if err := validateUnifiedDiffPaths(unifiedDiff, order.AllowedPaths); err != nil {
			return err
		}
		diffDigest := sha256.Sum256(unifiedDiff)
		if receipt.UnifiedDiffSHA256 != hex.EncodeToString(diffDigest[:]) {
			return errors.New("specialist receipt unified diff digest mismatch")
		}
	case SpecialistCompletionBlocked:
		if len(unifiedDiff) != 0 || receipt.UnifiedDiffSHA256 != "" {
			return errors.New("blocked specialist receipt must not include a unified diff or digest")
		}
	default:
		return errors.New("specialist receipt status must be proposed or blocked")
	}
	digest, err := digestReceipt(receipt)
	if err != nil {
		return err
	}
	if receipt.ReceiptSHA256 != digest {
		return errors.New("specialist receipt content digest mismatch")
	}
	return nil
}

func validateEvidence(evidence SpecialistEvidenceIdentity) error {
	if evidence.BundleSchemaVersion == "" || len(evidence.BundleSchemaVersion) > 256 ||
		!digestPattern.MatchString(evidence.BundleSHA256) || !digestPattern.MatchString(evidence.VerificationReceiptSHA256) ||
		evidence.WorkflowRunID == 0 || evidence.WorkflowRunAttempt == 0 || !gitObjectPattern.MatchString(evidence.WorkflowRunHeadSHA) ||
		evidence.ArtifactID == 0 || evidence.ArtifactName == "" || len(evidence.ArtifactName) > 255 || strings.IndexByte(evidence.ArtifactName, 0) >= 0 ||
		!digestPattern.MatchString(evidence.ArtifactSHA256) {
		return errors.New("complete verified bundle, workflow run, and artifact identity is required")
	}
	return nil
}

func validateLeaseForOrder(order SpecialistWorkOrder, lease SpecialistLease) error {
	if lease.SchemaVersion != SpecialistLeaseSchema || lease.WorkOrderID != order.ID || lease.RequestSHA256 != order.RequestSHA256 || lease.Specialist != order.Specialist {
		return errors.New("specialist lease request identity mismatch")
	}
	if !safeIdentityPattern.MatchString(lease.Owner) || !safeIdentityPattern.MatchString(lease.SessionID) {
		return errors.New("specialist lease owner or session is invalid")
	}
	if lease.AcquiredAt.Location() != time.UTC || lease.Deadline.Location() != time.UTC || lease.Deadline.Before(lease.AcquiredAt) || lease.Deadline.After(order.Deadline) {
		return errors.New("specialist lease timing is invalid")
	}
	digest, err := digestLease(lease)
	if err != nil {
		return err
	}
	if lease.LeaseSHA256 != digest {
		return errors.New("specialist lease content digest mismatch")
	}
	return nil
}

func validSpecialistRole(role SpecialistRole) bool {
	switch role {
	case SpecialistQuality, SpecialistCIMaintainer, SpecialistSecurity, SpecialistArchitect, SpecialistScanner:
		return true
	default:
		return false
	}
}

func normalizeAllowedPaths(values []string) ([]string, error) {
	if len(values) == 0 || len(values) > 512 {
		return nil, errors.New("at least one bounded allowed path is required")
	}
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.ReplaceAll(strings.TrimSpace(value), `\`, "/")
		clean := path.Clean(value)
		if value == "" || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || strings.HasPrefix(clean, "/") || strings.Contains(clean, ":") || strings.IndexByte(clean, 0) >= 0 || len(clean) > 512 {
			return nil, fmt.Errorf("unsafe specialist allowed path %q", value)
		}
		if _, err := path.Match(clean, "probe"); err != nil {
			return nil, fmt.Errorf("invalid specialist allowed path pattern %q", value)
		}
		if _, exists := seen[clean]; exists {
			continue
		}
		seen[clean] = struct{}{}
		result = append(result, clean)
	}
	sort.Strings(result)
	return result, nil
}

func normalizeValidation(values []string) ([]string, error) {
	if len(values) == 0 || len(values) > 128 {
		return nil, errors.New("at least one bounded validation command is required")
	}
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || len(value) > 4096 || strings.ContainsAny(value, "\x00\r\n") {
			return nil, errors.New("specialist validation commands must be bounded single lines")
		}
		if _, exists := seen[value]; exists {
			return nil, errors.New("specialist validation commands must be unique")
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result, nil
}

func validateUnifiedDiff(diff []byte, limit int64) error {
	if len(diff) == 0 || int64(len(diff)) > limit || bytes.IndexByte(diff, 0) >= 0 {
		return errors.New("specialist unified diff is empty, oversized, or contains NUL")
	}
	text := string(diff)
	if !strings.Contains(text, "diff --git ") || !strings.Contains(text, "\n--- ") || !strings.Contains(text, "\n+++ ") || !strings.Contains(text, "\n@@ ") {
		return errors.New("specialist proposal is not a unified git diff")
	}
	return nil
}

func validateUnifiedDiffPaths(diff []byte, allowed []string) error {
	found := false
	for _, line := range strings.Split(string(diff), "\n") {
		if !strings.HasPrefix(line, "diff --git ") {
			continue
		}
		found = true
		oldPath, newPath, err := parseGitDiffPaths(strings.TrimPrefix(line, "diff --git "))
		if err != nil {
			return err
		}
		for _, candidate := range []struct {
			value  string
			prefix string
		}{{oldPath, "a/"}, {newPath, "b/"}} {
			if !strings.HasPrefix(candidate.value, candidate.prefix) {
				return errors.New("specialist unified diff has an unsafe path header")
			}
			relative := strings.TrimPrefix(candidate.value, candidate.prefix)
			clean := path.Clean(relative)
			if clean != relative || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || strings.HasPrefix(clean, "/") || strings.Contains(clean, ":") || strings.IndexByte(clean, 0) >= 0 {
				return errors.New("specialist unified diff contains an unsafe repository path")
			}
			if !matchesAllowedPath(clean, allowed) {
				return fmt.Errorf("specialist unified diff path %q is outside the work order scope", clean)
			}
		}
	}
	if !found {
		return errors.New("specialist unified diff contains no file headers")
	}
	return nil
}

func parseGitDiffPaths(value string) (string, string, error) {
	first, remainder, err := nextGitPathToken(value)
	if err != nil {
		return "", "", err
	}
	second, remainder, err := nextGitPathToken(remainder)
	if err != nil {
		return "", "", err
	}
	if strings.TrimSpace(remainder) != "" {
		return "", "", errors.New("specialist unified diff has an invalid path header")
	}
	return first, second, nil
}

func nextGitPathToken(value string) (string, string, error) {
	value = strings.TrimLeft(value, " \t")
	if value == "" {
		return "", "", errors.New("specialist unified diff has a missing path")
	}
	if value[0] != '"' {
		if index := strings.IndexAny(value, " \t"); index >= 0 {
			return value[:index], value[index:], nil
		}
		return value, "", nil
	}
	escaped := false
	for index := 1; index < len(value); index++ {
		switch {
		case escaped:
			escaped = false
		case value[index] == '\\':
			escaped = true
		case value[index] == '"':
			decoded, err := strconv.Unquote(value[:index+1])
			if err != nil {
				return "", "", errors.New("specialist unified diff has an invalid quoted path")
			}
			return decoded, value[index+1:], nil
		}
	}
	return "", "", errors.New("specialist unified diff has an unterminated quoted path")
}

func matchesAllowedPath(candidate string, allowed []string) bool {
	for _, scope := range allowed {
		if candidate == scope {
			return true
		}
		if strings.HasSuffix(scope, "/**") {
			prefix := strings.TrimSuffix(scope, "**")
			if strings.HasPrefix(candidate, prefix) {
				return true
			}
		}
		if matched, err := path.Match(scope, candidate); err == nil && matched {
			return true
		}
	}
	return false
}

func digestLease(lease SpecialistLease) (string, error) {
	copy := lease
	copy.LeaseSHA256 = ""
	return digestJSON(copy)
}

func digestReceipt(receipt SpecialistReceipt) (string, error) {
	copy := receipt
	copy.ReceiptSHA256 = ""
	return digestJSON(copy)
}

func digestJSON(value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func equalJSON(left, right any) bool {
	a, errA := json.Marshal(left)
	b, errB := json.Marshal(right)
	return errA == nil && errB == nil && bytes.Equal(a, b)
}

func boundedLimit(value, fallback int64) int64 {
	if value <= 0 {
		return fallback
	}
	return value
}

func secureDirectory(directory string) error {
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	if err := requireSecureDirectory(directory); err != nil {
		return err
	}
	return os.Chmod(directory, 0o700)
}

func requireSecureDirectory(directory string) error {
	info, err := os.Lstat(directory)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("specialist mailbox path is not a real directory: %s", directory)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("specialist mailbox directory permissions are not private: %s", directory)
	}
	return nil
}

func readSecureRegularFile(filePath string, limit int64) ([]byte, error) {
	info, err := os.Lstat(filePath)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("specialist mailbox path is not a regular file: %s", filePath)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("specialist mailbox file permissions are not private: %s", filePath)
	}
	if info.Size() > limit {
		return nil, fmt.Errorf("specialist mailbox file exceeds size limit: %s", filePath)
	}
	file, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, fmt.Errorf("specialist mailbox file changed while opening: %s", filePath)
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("specialist mailbox file exceeds size limit: %s", filePath)
	}
	return data, nil
}

func atomicWriteFile(target string, data []byte, replace bool) error {
	if err := requireSecureDirectory(filepath.Dir(target)); err != nil {
		return err
	}
	if !replace {
		if _, err := os.Lstat(target); err == nil {
			return os.ErrExist
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	temporary, err := randomSibling(target, ".tmp-")
	if err != nil {
		return err
	}
	file, err := os.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if err := writeAndSync(file, data); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	if !replace {
		if _, err := os.Lstat(target); err == nil {
			_ = os.Remove(temporary)
			return os.ErrExist
		} else if !errors.Is(err, os.ErrNotExist) {
			_ = os.Remove(temporary)
			return err
		}
	}
	if err := os.Rename(temporary, target); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	return nil
}

func writeAndSync(file *os.File, data []byte) error {
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return err
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func randomSibling(target, marker string) (string, error) {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}
	return target + marker + hex.EncodeToString(nonce[:]), nil
}

func decodeStrict(data []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("unexpected trailing JSON value")
		}
		return err
	}
	return nil
}
