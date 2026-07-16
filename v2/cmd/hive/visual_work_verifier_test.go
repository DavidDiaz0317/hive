package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	hivegithub "github.com/kubestellar/hive/v2/pkg/github"
	"github.com/kubestellar/hive/v2/pkg/integrated"
	"github.com/kubestellar/hive/v2/pkg/visualhive"
	"github.com/kubestellar/hive/v2/pkg/visualhive/normalservice"
)

func TestNormalVisualPullRequestVerifierAppliesOnlyExactSealedCheckEvidence(t *testing.T) {
	fingerprint := strings.Repeat("f", 64)
	baseSHA, headSHA := strings.Repeat("a", 40), strings.Repeat("b", 40)
	lifecycle := writeNormalVisualVerifierLifecycle(t, fingerprint, headSHA)
	snapshot := normalVisualVerifierSnapshot(t, baseSHA, headSHA)
	verified := &fakeNormalVisualVerifiedPullRequest{snapshot: snapshot}
	var fetched hivegithub.VisualHivePullRequestBundleRequest
	config := integrated.Config{
		Repository: "owner/repo", RepositoryID: "123", DefaultBranch: "main", StateDir: t.TempDir(),
		VisualHive: true, Automation: integrated.AutomationRepairPR, ACMMLevel: 5,
	}
	verifier := &normalVisualPullRequestVerifier{
		lifecycle:    lifecycle,
		loadConfig:   func() (integrated.Config, int, error) { return config, 4, nil },
		actionsAppID: func(context.Context) (int64, error) { return 15368, nil },
		fetch: func(_ context.Context, request hivegithub.VisualHivePullRequestBundleRequest) (normalVisualVerifiedPullRequest, error) {
			fetched = request
			return verified, nil
		},
	}
	receipt, err := verifier.VerifyPullRequest(context.Background(), normalservice.PullRequestVerdictRequest{
		IdempotencyKey: "workflow:order", Repository: "owner/repo", RepositoryFingerprint: fingerprint,
		PullRequestNumber: 7, HeadBranch: "hive/repair-proof", HeadSHA: headSHA, BaseBranch: "main", BaseSHA: baseSHA,
	})
	if err != nil {
		t.Fatal(err)
	}
	if verified.applyCalls != 1 || verified.appliedFingerprint != fingerprint || receipt.HeadSHA != headSHA || receipt.Status != "success" || receipt.ReceiptSHA256 != snapshot.ReceiptSHA256 {
		t.Fatalf("exact check evidence was not applied once: verified=%+v receipt=%+v", verified, receipt)
	}
	if fetched.Repository != config.Repository || fetched.ExpectedBaseRepositoryID != 123 || fetched.ExpectedHeadRepositoryID != 123 ||
		fetched.ExpectedBaseSHA != baseSHA || fetched.ExpectedHeadSHA != headSHA || fetched.ExpectedHeadBranch != "hive/repair-proof" ||
		fetched.ExpectedWorkflowName != normalVisualPullRequestWorkflowName || fetched.ExpectedWorkflowPath != normalVisualPullRequestWorkflowPath ||
		fetched.ExpectedGitHubActionsAppID != 15368 || fetched.ExpectedProducerGitCommit != hivegithub.VisualHivePullRequestProducerCommit || fetched.MaxACMM != 4 ||
		fetched.DestinationDir != filepath.Join(config.StateDir, "visual-hive", "pr-verifier") {
		t.Fatalf("verifier fetch request lost independently knowable pins: %+v", fetched)
	}
	var persistedIdentity visualhive.PullRequestCheckReceiptIdentity
	if json.Unmarshal(receipt.Receipt, &persistedIdentity) != nil || persistedIdentity.ReplayKey != snapshot.Identity.ReplayKey {
		t.Fatalf("service receipt is not the canonical sealed identity: %s", receipt.Receipt)
	}
}

func TestNormalVisualPullRequestVerifierRejectsReceiptDriftBeforeLifecycleApply(t *testing.T) {
	fingerprint := strings.Repeat("f", 64)
	baseSHA, headSHA := strings.Repeat("a", 40), strings.Repeat("b", 40)
	lifecycle := writeNormalVisualVerifierLifecycle(t, fingerprint, headSHA)
	snapshot := normalVisualVerifierSnapshot(t, baseSHA, strings.Repeat("c", 40))
	verified := &fakeNormalVisualVerifiedPullRequest{snapshot: snapshot}
	config := integrated.Config{Repository: "owner/repo", RepositoryID: "123", DefaultBranch: "main", StateDir: t.TempDir(), VisualHive: true, Automation: integrated.AutomationRepairPR, ACMMLevel: 5}
	verifier := &normalVisualPullRequestVerifier{
		lifecycle: lifecycle, loadConfig: func() (integrated.Config, int, error) { return config, 5, nil },
		actionsAppID: func(context.Context) (int64, error) { return 15368, nil },
		fetch: func(context.Context, hivegithub.VisualHivePullRequestBundleRequest) (normalVisualVerifiedPullRequest, error) {
			return verified, nil
		},
	}
	_, err := verifier.VerifyPullRequest(context.Background(), normalservice.PullRequestVerdictRequest{
		IdempotencyKey: "workflow:order", Repository: "owner/repo", RepositoryFingerprint: fingerprint,
		PullRequestNumber: 7, HeadBranch: "hive/repair-proof", HeadSHA: headSHA, BaseBranch: "main", BaseSHA: baseSHA,
	})
	if err == nil || !strings.Contains(err.Error(), "differs from the exact installed Worker PR") || verified.applyCalls != 0 {
		t.Fatalf("drifted receipt reached lifecycle apply: err=%v verified=%+v", err, verified)
	}
}

type fakeNormalVisualVerifiedPullRequest struct {
	snapshot           visualhive.PullRequestCheckReceiptSnapshot
	applyCalls         int
	appliedFingerprint string
}

func (verified *fakeNormalVisualVerifiedPullRequest) Receipt() (visualhive.PullRequestCheckReceiptSnapshot, error) {
	return verified.snapshot, nil
}

func (verified *fakeNormalVisualVerifiedPullRequest) ApplyCheckEvidence(_ *visualhive.LifecycleStore, fingerprint string) (visualhive.ApplyPullRequestCheckUpdateResult, error) {
	verified.applyCalls++
	verified.appliedFingerprint = fingerprint
	return visualhive.ApplyPullRequestCheckUpdateResult{ReceiptSHA256: verified.snapshot.ReceiptSHA256, ReplayKey: verified.snapshot.Identity.ReplayKey}, nil
}

func writeNormalVisualVerifierLifecycle(t *testing.T, fingerprint, headSHA string) *visualhive.LifecycleStore {
	t.Helper()
	dir := t.TempDir()
	state := visualhive.LifecycleState{
		SchemaVersion: visualhive.LifecycleSchema, UpdatedAt: time.Now().UTC(),
		Findings: map[string]*visualhive.FindingLifecycle{fingerprint: {
			Repository: "owner/repo", RepositoryID: "123", Fingerprint: "finding", RepositoryFingerprint: fingerprint,
			Status: visualhive.StatusPROpen, Branch: "hive/repair-proof", RepairCommitSHA: headSHA, PRNumber: 7,
		}},
		ReplayKeys: map[string]string{}, Outbox: []*visualhive.OutboxEntry{},
	}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "visual-hive-lifecycle.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	lifecycle, err := visualhive.NewLifecycleStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	return lifecycle
}

func normalVisualVerifierSnapshot(t *testing.T, baseSHA, headSHA string) visualhive.PullRequestCheckReceiptSnapshot {
	t.Helper()
	identity := visualhive.PullRequestCheckReceiptIdentity{
		SchemaVersion: visualhive.PullRequestCheckReceiptSchema,
		Source: visualhive.PullRequestSourceBinding{
			SchemaVersion: visualhive.PullRequestSourceBindingSchema, Repository: "owner/repo", RepositoryID: "123", PullRequest: 7,
			Base: visualhive.PullRequestRevisionIdentity{Repository: "owner/repo", RepositoryID: "123", Ref: "main", SHA: baseSHA},
			Head: visualhive.PullRequestRevisionIdentity{Repository: "owner/repo", RepositoryID: "123", Ref: "hive/repair-proof", SHA: headSHA},
		},
		Workflow:  visualhive.PullRequestCheckWorkflowIdentity{Name: normalVisualPullRequestWorkflowName, Path: normalVisualPullRequestWorkflowPath, Event: "pull_request", AppID: "15368"},
		Producer:  visualhive.Producer{Name: "visual-hive", Version: "test", GitCommit: hivegithub.VisualHivePullRequestProducerCommit},
		Check:     visualhive.PullRequestCheckResultIdentity{State: "success", Conclusion: "success"},
		Authority: visualhive.PullRequestCheckAuthority{CheckEvidenceOnly: true}, ReplayKey: strings.Repeat("e", 64),
	}
	data, err := json.Marshal(identity)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	return visualhive.PullRequestCheckReceiptSnapshot{Identity: identity, ReceiptSHA256: hex.EncodeToString(digest[:])}
}
