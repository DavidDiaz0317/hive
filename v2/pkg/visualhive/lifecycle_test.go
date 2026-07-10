package visualhive

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kubestellar/hive/v2/pkg/beads"
)

func TestLifecyclePersistsAndDeduplicatesBundle(t *testing.T) {
	root := t.TempDir()
	manifestPath := writeLifecycleBundle(t, filepath.Join(root, "bundle"), "bundle-present-1", "present", "refs/heads/main", true)
	bundle := validateLocalBundle(t, manifestPath)
	beadStore := newTestBeadStore(t, filepath.Join(root, "beads"))
	lifecycle, err := NewLifecycleStore(filepath.Join(root, "lifecycle"))
	if err != nil {
		t.Fatal(err)
	}

	first, err := lifecycle.ApplyBundle(bundle, beadStore, ApplyLifecycleOptions{TargetRef: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if first.Created != 1 || first.BeadsCreated != 1 || first.OutboxCreated != 1 || first.Idempotent {
		t.Fatalf("unexpected first apply: %+v", first)
	}
	second, err := lifecycle.ApplyBundle(bundle, beadStore, ApplyLifecycleOptions{TargetRef: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if !second.Idempotent || beadStore.Count() != 1 || len(lifecycle.PendingOutbox()) != 1 {
		t.Fatalf("retry was not idempotent: %+v beads=%d outbox=%d", second, beadStore.Count(), len(lifecycle.PendingOutbox()))
	}

	reloaded, err := NewLifecycleStore(filepath.Join(root, "lifecycle"))
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := bundle.Manifest.Observations[0].RepositoryFingerprint
	finding, ok := reloaded.Finding(fingerprint)
	if !ok || finding.Status != StatusDetected || finding.BeadID == "" {
		t.Fatalf("lifecycle state did not survive restart: %+v", finding)
	}
}

func TestLifecycleIssuePRMergeCloseAndRecurrence(t *testing.T) {
	root := t.TempDir()
	beadStore := newTestBeadStore(t, filepath.Join(root, "beads"))
	lifecycle, err := NewLifecycleStore(filepath.Join(root, "lifecycle"))
	if err != nil {
		t.Fatal(err)
	}
	presentPath := writeLifecycleBundle(t, filepath.Join(root, "present"), "bundle-present", "present", "refs/heads/main", true)
	present := validateLocalBundle(t, presentPath)
	if _, err := lifecycle.ApplyBundle(present, beadStore, ApplyLifecycleOptions{TargetRef: "main"}); err != nil {
		t.Fatal(err)
	}
	fingerprint := present.Manifest.Observations[0].RepositoryFingerprint
	openEntry := lifecycle.PendingOutbox()[0]
	if openEntry.Action != OutboxOpenIssue {
		t.Fatalf("expected open issue outbox, got %s", openEntry.Action)
	}
	if err := lifecycle.MarkIssueOpened(fingerprint, 101, "https://github.com/owner/repo/issues/101"); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.MarkOutboxAttempt(openEntry.ID, nil); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.MarkRepairStarted(fingerprint, "hive/repair-app-shell"); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.MarkRepairStarted(fingerprint, "hive/repair-app-shell"); err != nil {
		t.Fatal(err)
	}
	started, _ := lifecycle.Finding(fingerprint)
	if started.RepairAttempts != 1 {
		t.Fatalf("restart replay incremented repair attempts: %+v", started)
	}
	if err := lifecycle.MarkPROpen(fingerprint, "repair-sha", 202, "https://github.com/owner/repo/pull/202"); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.MarkChecks(fingerprint, "wrong-sha", true); err == nil {
		t.Fatal("checks for a stale SHA must not authorize merge")
	}
	if err := lifecycle.MarkChecks(fingerprint, "repair-sha", true); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.MarkMerged(fingerprint, present.Manifest.Source.CommitSHA); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.MarkPostMergeVerifying(fingerprint, "run-303", "https://github.com/owner/repo/actions/runs/303"); err != nil {
		t.Fatal(err)
	}

	absentPath := writeLifecycleBundle(t, filepath.Join(root, "absent"), "bundle-absent", "absent", "refs/heads/main", true)
	absent := validateLocalBundle(t, absentPath)
	resolved, err := lifecycle.ApplyBundle(absent, beadStore, ApplyLifecycleOptions{TargetRef: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Resolved != 1 {
		t.Fatalf("authoritative absence did not resolve finding: %+v", resolved)
	}
	pending := lifecycle.PendingOutbox()
	closeEntry := pending[len(pending)-1]
	if closeEntry.Action != OutboxCloseIssue || closeEntry.IssueNumber != 101 {
		t.Fatalf("expected close issue outbox, got %+v", closeEntry)
	}
	if err := lifecycle.MarkOutboxAttempt(closeEntry.ID, nil); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.MarkIssueClosed(fingerprint, beadStore); err != nil {
		t.Fatal(err)
	}
	finding, _ := lifecycle.Finding(fingerprint)
	if finding.Status != StatusIssueClosed {
		t.Fatalf("finding was not closed: %+v", finding)
	}
	bead, err := beadStore.Get(finding.BeadID)
	if err != nil || bead.Status != beads.StatusClosed {
		t.Fatalf("bead was not closed: %+v err=%v", bead, err)
	}

	recurrencePath := writeLifecycleBundle(t, filepath.Join(root, "recurrence"), "bundle-recurrence", "present", "refs/heads/main", true)
	recurrence := validateLocalBundle(t, recurrencePath)
	reopened, err := lifecycle.ApplyBundle(recurrence, beadStore, ApplyLifecycleOptions{TargetRef: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if reopened.Reopened != 1 || reopened.BeadsCreated != 0 {
		t.Fatalf("recurrence did not reopen the existing lifecycle: %+v", reopened)
	}
	finding, _ = lifecycle.Finding(fingerprint)
	bead, _ = beadStore.Get(finding.BeadID)
	if finding.Recurrences != 1 || bead.Status != beads.StatusOpen {
		t.Fatalf("recurrence did not reopen existing issue/bead: finding=%+v bead=%+v", finding, bead)
	}
	reopenEntry := lifecycle.PendingOutbox()[0]
	if reopenEntry.Action != OutboxReopenIssue || reopenEntry.IssueNumber != 101 {
		t.Fatalf("expected reopen issue outbox, got %+v", reopenEntry)
	}
}

func TestLifecyclePersistsManualBaselineReviewAcrossFreshObservations(t *testing.T) {
	root := t.TempDir()
	beadStore := newTestBeadStore(t, filepath.Join(root, "beads"))
	lifecycle, err := NewLifecycleStore(filepath.Join(root, "lifecycle"))
	if err != nil {
		t.Fatal(err)
	}
	present := validateLocalBundle(t, writeLifecycleBundle(t, filepath.Join(root, "present"), "bundle-review-one", "present", "refs/heads/main", true))
	if _, err := lifecycle.ApplyBundle(present, beadStore, ApplyLifecycleOptions{TargetRef: "main"}); err != nil {
		t.Fatal(err)
	}
	fingerprint := present.Manifest.Observations[0].RepositoryFingerprint
	if err := lifecycle.MarkIssueOpened(fingerprint, 48, "https://example.test/issues/48"); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.MarkRepairStarted(fingerprint, "hive/repair-review"); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.MarkPROpen(fingerprint, "repair-head", 49, "https://example.test/pull/49"); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.MarkManualReviewRequired(fingerprint, "visual_baseline", "Review exact candidate digests in PR #50"); err != nil {
		t.Fatal(err)
	}
	fresh := validateLocalBundle(t, writeLifecycleBundle(t, filepath.Join(root, "fresh"), "bundle-review-two", "present", "refs/heads/main", true))
	if _, err := lifecycle.ApplyBundle(fresh, beadStore, ApplyLifecycleOptions{TargetRef: "main"}); err != nil {
		t.Fatal(err)
	}
	held, _ := lifecycle.Finding(fingerprint)
	if !held.HumanReviewRequired || held.ManualReviewKind != "visual_baseline" || held.ObservationHumanReviewRequired {
		t.Fatalf("fresh producer evidence erased the manual hold: %+v", held)
	}
	if err := lifecycle.MarkPRHeadUpdated(fingerprint, "repair-head-with-baseline"); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.MarkManualReviewComplete(fingerprint, "visual_baseline"); err != nil {
		t.Fatal(err)
	}
	completed, _ := lifecycle.Finding(fingerprint)
	if completed.HumanReviewRequired || completed.ManualReviewKind != "" || completed.RepairCommitSHA != "repair-head-with-baseline" || completed.Status != StatusPROpen {
		t.Fatalf("manual review completion did not reset the exact-head gate: %+v", completed)
	}
}

func TestLifecycleMigratesV1ManualReviewState(t *testing.T) {
	root := filepath.Join(t.TempDir(), "lifecycle")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	legacy := `{"schema_version":"hive.visual-hive-lifecycle.v1","updated_at":"2026-07-10T00:00:00Z","findings":{"fp":{"repository":"owner/repo","fingerprint":"source","repository_fingerprint":"fp","status":"issue_open","issue_kind":"screenshot_diff","severity":"high","owning_agent_hint":"quality","title":"review","body":"review","labels":[],"affected_contracts":[],"validation_command":"vh","human_review_required":true,"first_seen_at":"2026-07-10T00:00:00Z","last_seen_at":"2026-07-10T00:00:00Z","last_bundle_id":"bundle","last_bundle_digest":"digest","repair_attempts":0,"recurrences":0}},"replay_keys":{},"outbox":[]}`
	statePath := filepath.Join(root, "visual-hive-lifecycle.json")
	if err := os.WriteFile(statePath, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := NewLifecycleStore(root)
	if err != nil {
		t.Fatal(err)
	}
	finding, _ := store.Finding("fp")
	if !finding.ObservationHumanReviewRequired || !finding.HumanReviewRequired {
		t.Fatalf("legacy review authority was not migrated: %+v", finding)
	}
	data, err := os.ReadFile(statePath)
	if err != nil || !strings.Contains(string(data), LifecycleSchema) {
		t.Fatalf("migrated lifecycle schema was not persisted: %q err=%v", data, err)
	}
}

func TestPostMergeVerificationFailureReturnsFindingToRepair(t *testing.T) {
	root := t.TempDir()
	beadStore := newTestBeadStore(t, filepath.Join(root, "beads"))
	lifecycle, err := NewLifecycleStore(filepath.Join(root, "lifecycle"))
	if err != nil {
		t.Fatal(err)
	}
	present := validateLocalBundle(t, writeLifecycleBundle(t, filepath.Join(root, "present"), "bundle-present", "present", "refs/heads/main", true))
	if _, err := lifecycle.ApplyBundle(present, beadStore, ApplyLifecycleOptions{TargetRef: "main"}); err != nil {
		t.Fatal(err)
	}
	fingerprint := present.Manifest.Observations[0].RepositoryFingerprint
	if err := lifecycle.MarkIssueOpened(fingerprint, 101, "https://example.test/issues/101"); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.MarkRepairStarted(fingerprint, "hive/repair-a1"); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.MarkPROpen(fingerprint, "repair-sha", 202, "https://example.test/pull/202"); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.MarkChecks(fingerprint, "repair-sha", true); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.MarkMerged(fingerprint, "merge-sha"); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.MarkPostMergeVerifying(fingerprint, "run-303", "https://example.test/runs/303"); err != nil {
		t.Fatal(err)
	}

	stillPresent := validateLocalBundle(t, writeLifecycleBundle(t, filepath.Join(root, "still-present"), "bundle-still-present", "present", "refs/heads/main", true))
	stillPresent.Manifest.Source.WorkflowRunID = "run-303"
	stillPresent.Manifest.Source.CommitSHA = "merge-sha"
	if _, err := lifecycle.ApplyBundle(stillPresent, beadStore, ApplyLifecycleOptions{TargetRef: "main", VerificationRunID: "run-303"}); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.MarkPostMergeFailed(fingerprint, "finding remained present"); err != nil {
		t.Fatal(err)
	}
	finding, _ := lifecycle.Finding(fingerprint)
	if finding.Status != StatusNeedsRevision || finding.MergeSHA != "merge-sha" || finding.LastCheckSummary != "finding remained present" {
		t.Fatalf("failed post-merge verification lost retry evidence: %+v", finding)
	}
}

func TestLifecycleDoesNotResolveFromWrongTargetRef(t *testing.T) {
	root := t.TempDir()
	beadStore := newTestBeadStore(t, filepath.Join(root, "beads"))
	lifecycle, err := NewLifecycleStore(filepath.Join(root, "lifecycle"))
	if err != nil {
		t.Fatal(err)
	}
	present := validateLocalBundle(t, writeLifecycleBundle(t, filepath.Join(root, "present"), "bundle-present", "present", "refs/heads/main", true))
	if _, err := lifecycle.ApplyBundle(present, beadStore, ApplyLifecycleOptions{TargetRef: "main"}); err != nil {
		t.Fatal(err)
	}
	absent := validateLocalBundle(t, writeLifecycleBundle(t, filepath.Join(root, "absent"), "bundle-absent", "absent", "refs/heads/feature", true))
	result, err := lifecycle.ApplyBundle(absent, beadStore, ApplyLifecycleOptions{TargetRef: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Resolved != 0 || result.IgnoredAbsent != 1 {
		t.Fatalf("wrong-ref absence was not ignored: %+v", result)
	}
}

func TestLifecycleInfersAbsenceOnlyForEvaluatedContracts(t *testing.T) {
	root := t.TempDir()
	beadStore := newTestBeadStore(t, filepath.Join(root, "beads"))
	lifecycle, err := NewLifecycleStore(filepath.Join(root, "lifecycle"))
	if err != nil {
		t.Fatal(err)
	}
	present := validateLocalBundle(t, writeLifecycleBundle(t, filepath.Join(root, "present"), "bundle-present-inventory", "present", "refs/heads/main", true))
	if _, err := lifecycle.ApplyBundle(present, beadStore, ApplyLifecycleOptions{TargetRef: "main"}); err != nil {
		t.Fatal(err)
	}
	fingerprint := present.Manifest.Observations[0].RepositoryFingerprint
	if err := lifecycle.MarkIssueOpened(fingerprint, 44, "https://github.com/owner/repo/issues/44"); err != nil {
		t.Fatal(err)
	}

	manifestPath := writeLifecycleBundle(t, filepath.Join(root, "complete"), "bundle-complete-inventory", "present", "refs/heads/main", true)
	manifest := readManifest(t, manifestPath)
	manifest.Observations = []Observation{}
	manifest.ReplayProtection.Nonce = manifest.BundleID
	manifest.ReplayProtection.Key = replayKey(manifest)
	file := manifest.Files[0]
	manifest.OverallDigest = digestBundleContent(manifest, []string{fmt.Sprintf("file\x00%s\x00%s\x00%d", file.Path, file.SHA256, file.Size)})
	manifest.Provenance.SubjectDigest = manifest.OverallDigest
	writeManifest(t, manifestPath, manifest)
	complete := validateLocalBundle(t, manifestPath)
	result, err := lifecycle.ApplyBundle(complete, beadStore, ApplyLifecycleOptions{TargetRef: "main", VerificationRunID: "501", VerificationURL: "https://github.com/owner/repo/actions/runs/501"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Resolved != 1 {
		t.Fatalf("expected exhaustive inventory to resolve omitted finding: %+v", result)
	}
	finding, _ := lifecycle.Finding(fingerprint)
	if finding.ValidationRunID != "501" || finding.ValidationRunURL == "" {
		t.Fatalf("verification evidence was not persisted: %+v", finding)
	}
}

func TestLifecycleRejectsPostMergeAbsenceAtWrongSHA(t *testing.T) {
	root := t.TempDir()
	beadStore := newTestBeadStore(t, filepath.Join(root, "beads"))
	lifecycle, _ := NewLifecycleStore(filepath.Join(root, "lifecycle"))
	present := validateLocalBundle(t, writeLifecycleBundle(t, filepath.Join(root, "present"), "bundle-present-sha", "present", "refs/heads/main", true))
	_, _ = lifecycle.ApplyBundle(present, beadStore, ApplyLifecycleOptions{TargetRef: "main"})
	fingerprint := present.Manifest.Observations[0].RepositoryFingerprint
	_ = lifecycle.MarkIssueOpened(fingerprint, 55, "https://github.com/owner/repo/issues/55")
	_ = lifecycle.MarkRepairStarted(fingerprint, "hive/repair")
	_ = lifecycle.MarkPROpen(fingerprint, "repair-sha", 56, "https://github.com/owner/repo/pull/56")
	_ = lifecycle.MarkChecks(fingerprint, "repair-sha", true)
	_ = lifecycle.MarkMerged(fingerprint, "different-merge-sha")
	absent := validateLocalBundle(t, writeLifecycleBundle(t, filepath.Join(root, "absent"), "bundle-absent-sha", "absent", "refs/heads/main", true))
	result, err := lifecycle.ApplyBundle(absent, beadStore, ApplyLifecycleOptions{TargetRef: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Resolved != 0 || result.IgnoredAbsent != 1 {
		t.Fatalf("wrong-SHA absence must not resolve: %+v", result)
	}
}

func TestLifecycleAuditIsAppendOnlyJSONL(t *testing.T) {
	root := t.TempDir()
	beadStore := newTestBeadStore(t, filepath.Join(root, "beads"))
	lifecycle, err := NewLifecycleStore(filepath.Join(root, "lifecycle"))
	if err != nil {
		t.Fatal(err)
	}
	bundle := validateLocalBundle(t, writeLifecycleBundle(t, filepath.Join(root, "bundle"), "bundle-audit", "present", "main", true))
	if _, err := lifecycle.ApplyBundle(bundle, beadStore, ApplyLifecycleOptions{TargetRef: "main"}); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(filepath.Join(root, "lifecycle", "visual-hive-lifecycle-audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	count := 0
	for scanner.Scan() {
		count++
	}
	if err := scanner.Err(); err != nil || count == 0 {
		t.Fatalf("expected append-only audit entries, count=%d err=%v", count, err)
	}
}

func TestIssuePublicationSelectionEnforcesActiveWIPAndRanksDirectFailuresFirst(t *testing.T) {
	observations := []Observation{
		{RepositoryFingerprint: "coverage", State: "present", Severity: "high", IssueKind: "missing_visual_coverage"},
		{RepositoryFingerprint: "regression", State: "present", Severity: "high", IssueKind: "visual_regression"},
		{RepositoryFingerprint: "medium", State: "present", Severity: "medium", IssueKind: "visual_regression"},
	}
	findings := map[string]*FindingLifecycle{
		"existing": {RepositoryFingerprint: "existing", IssueNumber: 17, Status: StatusIssueOpen},
	}

	selected := selectIssuePublications(observations, findings, 2, false)

	if len(selected) != 1 || !selected["regression"] {
		t.Fatalf("expected one remaining slot to select the direct high-severity failure, got %v", selected)
	}
}

func TestIssuePublicationSelectionPrefersRepairableConsoleFailure(t *testing.T) {
	observations := []Observation{
		{RepositoryFingerprint: "baseline", State: "present", Severity: "high", IssueKind: "screenshot_diff", Title: "Review missing baseline for deploy-preview"},
		{RepositoryFingerprint: "regression", State: "present", Severity: "high", IssueKind: "screenshot_diff", Title: "deploy-preview failed deterministic validation"},
		{RepositoryFingerprint: "console", State: "present", Severity: "high", IssueKind: "external_repo_onboarding", Title: "Repair deploy-preview: console_error"},
	}
	findings := map[string]*FindingLifecycle{
		"existing": {RepositoryFingerprint: "existing", IssueNumber: 17, Status: StatusIssueOpen, HumanReviewRequired: true},
	}

	selected := selectIssuePublications(observations, findings, 2, true)

	if len(selected) != 1 || !selected["console"] {
		t.Fatalf("expected repair mode to select the actionable console failure, got %v", selected)
	}
	if !observationNeedsHumanReview(observations[0]) {
		t.Fatal("missing-baseline observations must require human review")
	}
}

func TestIssuePublicationSelectionPrefersTestOnlyRepairOverUnrepairableBacklog(t *testing.T) {
	observations := []Observation{
		{RepositoryFingerprint: "onboarding", State: "present", Severity: "critical", IssueKind: "external_repo_onboarding", Title: "Review readiness gate"},
		{RepositoryFingerprint: "tests", State: "present", Severity: "high", IssueKind: "test_adequacy_gap", Title: "Add repository unit test coverage"},
	}
	selected := selectIssuePublications(observations, nil, 1, true)
	if len(selected) != 1 || !selected["tests"] {
		t.Fatalf("repair mode should select the bounded test-only repair before advisory backlog: %v", selected)
	}
	selected = selectIssuePublications(observations, nil, 1, false)
	if len(selected) != 1 || !selected["onboarding"] {
		t.Fatalf("issues-only mode should preserve severity ordering: %v", selected)
	}
}

func writeLifecycleBundle(t *testing.T, root, bundleID, state, ref string, authoritative bool) string {
	t.Helper()
	manifestPath := writeTestBundle(t, root, false)
	manifest := readManifest(t, manifestPath)
	manifest.BundleID = bundleID
	manifest.Source.Ref = ref
	manifest.Scan.AuthoritativeForResolution = authoritative
	if authoritative {
		manifest.Scan.Scope = "full"
	} else {
		manifest.Scan.Scope = "partial"
	}
	manifest.Observations[0].State = state
	manifest.ReplayProtection.Nonce = bundleID
	manifest.ReplayProtection.Key = replayKey(manifest)
	file := manifest.Files[0]
	manifest.OverallDigest = digestBundleContent(manifest, []string{fmt.Sprintf("file\x00%s\x00%s\x00%d", file.Path, file.SHA256, file.Size)})
	manifest.Provenance.SubjectDigest = manifest.OverallDigest
	writeManifest(t, manifestPath, manifest)
	return manifestPath
}

func validateLocalBundle(t *testing.T, manifestPath string) *ValidatedBundle {
	t.Helper()
	bundle, err := ValidateBundle(manifestPath, ValidationOptions{Now: time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC), MaxACMM: 6, AllowLocal: true})
	if err != nil {
		t.Fatal(err)
	}
	return bundle
}

func newTestBeadStore(t *testing.T, dir string) *beads.Store {
	t.Helper()
	store, err := beads.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	return store
}
