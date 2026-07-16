package scheduler

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kubestellar/hive/v2/pkg/agent"
	"github.com/kubestellar/hive/v2/pkg/config"
	"github.com/kubestellar/hive/v2/pkg/knowledge"
)

type testCanonicalVerifiedReceipt struct {
	evidence  agent.SpecialistEvidenceIdentity
	canonical json.RawMessage
	exportErr error
}

var _ CanonicalVerifiedReceipt = (*testCanonicalVerifiedReceipt)(nil)

func (receipt *testCanonicalVerifiedReceipt) EvidenceIdentity() agent.SpecialistEvidenceIdentity {
	return receipt.evidence
}

func (receipt *testCanonicalVerifiedReceipt) CanonicalReceiptJSON() (json.RawMessage, error) {
	if receipt.exportErr != nil {
		return nil, receipt.exportErr
	}
	return append(json.RawMessage(nil), receipt.canonical...), nil
}

// testCanonicalV3Receipt mirrors the current verified bundle-v3 receipt
// identity shape. Production composition consumes bytes exported by the
// intake-owned canonical type and does not define this shape.
type testCanonicalV3Receipt struct {
	RepositoryID       string `json:"repository_id"`
	WorkflowRunID      string `json:"workflow_run_id"`
	WorkflowRunAttempt string `json:"workflow_run_attempt"`
	ArtifactID         string `json:"artifact_id"`
	SourceArtifactID   string `json:"source_artifact_id"`
	ArtifactName       string `json:"artifact_name"`
	CommitSHA          string `json:"commit_sha"`
	WorkflowName       string `json:"workflow_name"`
	WorkflowRunName    string `json:"workflow_run_name"`
	WorkflowPath       string `json:"workflow_path"`
	BundleSchema       string `json:"bundle_schema"`
	BundleSHA256       string `json:"bundle_sha256"`
	ManifestSHA256     string `json:"manifest_sha256"`
	ArtifactIndexSHA   string `json:"artifact_index_sha256"`
}

func TestBuildGovernedProposalMessageBindsAllFiveRolePoliciesAndPrimer(t *testing.T) {
	scheduler := testGovernedScheduler(t)
	roles := []agent.SpecialistRole{agent.SpecialistQuality, agent.SpecialistCIMaintainer, agent.SpecialistSecurity, agent.SpecialistArchitect, agent.SpecialistScanner}
	policyDigests := make(map[string]struct{}, len(roles))
	for _, role := range roles {
		t.Run(string(role), func(t *testing.T) {
			work, receipt := testGovernedInputs(role)
			request, err := scheduler.BuildGovernedProposalMessage(role, work, receipt)
			if err != nil {
				t.Fatal(err)
			}
			configuredBackend := scheduler.cfg.Agents[string(role)].Backend
			for _, marker := range []string{
				"ROLE_POLICY_" + strings.ToUpper(strings.ReplaceAll(string(role), "-", "_")),
				"PROJECT_CONTEXT_MARKER", "PRIMER_READ_ONLY_MARKER", work.ExternalRef,
				`routed_role=` + string(role), `configured_role_backend=` + configuredBackend,
				`executor_backend=codex`, `containment_profile=` + governedContainmentProfile,
				"role is expertise and perspective only", "AUTHORITY OVERRIDE AFTER UNTRUSTED CONTEXT",
				"Never write to GitHub", "beads", "wiki", "MCP", "subagents", "baselines",
				"AllowedPaths remain Worker-enforced structured data",
				"no checkout", "no .git directory", "no repository read or write authority",
				"WORKER-SEALED BOUNDED SOURCE CONTEXT", work.SealedSourceContext,
			} {
				if !strings.Contains(request.TaskPrompt, marker) {
					t.Errorf("task prompt missing %q:\n%s", marker, request.TaskPrompt)
				}
			}
			if request.Kind != agent.SpecialistWorkOrderKindGovernedVisualHiveProposal || request.Specialist != role || request.ExternalRef != work.ExternalRef || request.PacketSHA256 != work.PacketSHA256 ||
				request.FindingSHA256 != work.FindingSHA256 || request.SourceContextSHA256 != work.SourceContextSHA256 || request.Evidence.VerificationReceiptSHA256 != receipt.evidence.VerificationReceiptSHA256 ||
				request.PolicySHA256 == "" || request.KnowledgeSHA256 == "" || request.ToolPolicySHA256 == "" || request.CapabilitySHA256 == "" ||
				request.ReproductionMode != agent.SpecialistReproductionModeNone || len(request.Reproduction) != 0 ||
				request.TaskPromptSHA256 != governedSHA256(request.TaskPrompt) {
				t.Fatalf("canonical work-order request lost bindings: %+v", request)
			}
			input := decodeGovernedPrompt(t, request.TaskPrompt)
			if input.ExternalRef != work.ExternalRef || input.PacketJSON != string(work.Packet) || input.FindingJSON != string(work.Finding) ||
				input.VerifiedReceiptJSON != string(receipt.canonical) || input.SourceContextSHA256 != work.SourceContextSHA256 || input.BaseSHA != work.BaseSHA || input.BaseTreeSHA != work.BaseTreeSHA ||
				input.RoutingReason != work.RoutingReason || input.RolePolicyExpertise == "" || input.ProjectContextSnapshot == "" ||
				input.KnowledgePrimerSnapshot == "" || input.PolicySHA256 != governedSHA256(input.RolePolicyExpertise) ||
				input.KnowledgeSHA256 != governedSHA256(input.KnowledgePrimerSnapshot) || input.Task != governedProposalTask ||
				input.ReproductionMode != agent.SpecialistReproductionModeNone {
				t.Fatalf("lossless governed prompt binding is invalid: %+v", input)
			}
			if strings.Contains(input.ProjectContextSnapshot, "AUTHORIZED REPOS") || strings.Contains(input.ProjectContextSnapshot, "you may") {
				t.Fatalf("project identity snapshot contains repository access instructions: %q", input.ProjectContextSnapshot)
			}
			capabilitySHA, err := governedJSONDigest(input.Capability)
			if err != nil {
				t.Fatal(err)
			}
			if input.Capability.RoutedRole != role || input.Capability.ConfiguredRoleBackend != configuredBackend ||
				input.Capability.ExecutorBackend != "codex" || input.Capability.ContainmentProfile != governedContainmentProfile ||
				input.Capability.BackendParityClaimed || input.Capability.SourceContextMode != "worker-sealed-bounded-regular-git-blobs" ||
				input.Capability.ReproductionMode != agent.SpecialistReproductionModeNone || !input.Capability.Authority.SealedSourceContextRead ||
				input.Capability.Authority.RepositoryRead || input.Capability.Authority.DisposableWorktreeWrite ||
				input.Capability.Authority.RunReproduction || input.Capability.Authority.RunValidation ||
				input.CapabilitySHA256 != capabilitySHA || request.CapabilitySHA256 != capabilitySHA {
				t.Fatalf("truthful governed capability binding is invalid: %+v", input.Capability)
			}
			artifacts := make(map[string]string, len(input.EvidenceArtifacts))
			for _, artifact := range input.EvidenceArtifacts {
				artifacts[artifact.Reference] = artifact.SHA256
			}
			if artifacts["hive:verified-manifest"] != receipt.evidence.ArtifactSHA256 ||
				artifacts["hive:canonical-verification-receipt"] != receipt.evidence.VerificationReceiptSHA256 ||
				artifacts["screenshots/account.actual.png"] != strings.Repeat("f", 64) {
				t.Fatalf("controller-owned verified evidence artifacts were not composed: %v", artifacts)
			}
			roleConfig := scheduler.cfg.Agents[string(role)]
			expectedToolPolicy := governedToolPolicy{
				Backend:      roleConfig.Backend,
				Model:        roleConfig.Model,
				Mode:         roleConfig.Mode,
				IncludeRepos: roleConfig.ShouldIncludeRepos(),
				Connections:  append([]config.ConnectionConfig(nil), roleConfig.Connections...),
			}
			if roleConfig.Tools != nil {
				expectedToolPolicy.Preset = roleConfig.Tools.Preset
				expectedToolPolicy.Rules = append([]config.ToolRule(nil), roleConfig.Tools.EffectiveRules()...)
			}
			expectedToolSHA, err := governedJSONDigest(expectedToolPolicy)
			if err != nil {
				t.Fatal(err)
			}
			if request.ToolPolicySHA256 != expectedToolSHA || input.Capability.ToolPolicySHA256 != expectedToolSHA {
				t.Fatalf("configured role tool/connection profile was not bound: got %s want %s", request.ToolPolicySHA256, expectedToolSHA)
			}
			policyDigests[request.PolicySHA256] = struct{}{}
		})
	}
	if len(policyDigests) != len(roles) {
		t.Fatalf("all five specialists did not retain distinct role policies: %v", policyDigests)
	}
}

func TestBuildGovernedProposalMessageConsumesCanonicalV3ReceiptBytes(t *testing.T) {
	scheduler := testGovernedScheduler(t)
	work, receipt := testGovernedInputs(agent.SpecialistQuality)
	request, err := scheduler.BuildGovernedProposalMessage(agent.SpecialistQuality, work, receipt)
	if err != nil {
		t.Fatal(err)
	}
	input := decodeGovernedPrompt(t, request.TaskPrompt)
	wantDigest := testGovernedSHA(receipt.canonical)
	if input.VerifiedReceiptJSON != string(receipt.canonical) || input.VerifiedReceiptSHA256 != wantDigest ||
		request.Evidence.VerificationReceiptSHA256 != wantDigest {
		t.Fatalf("canonical receipt bytes and shared evidence digest diverged: input=%+v evidence=%+v", input, request.Evidence)
	}

	tampered := *receipt
	tampered.canonical = append(append(json.RawMessage(nil), receipt.canonical...), ' ')
	if _, err := scheduler.BuildGovernedProposalMessage(agent.SpecialistQuality, work, &tampered); err == nil {
		t.Fatal("canonical receipt bytes that do not match the shared evidence digest were accepted")
	}
	var nilReceipt *testCanonicalVerifiedReceipt
	if _, err := scheduler.BuildGovernedProposalMessage(agent.SpecialistQuality, work, nilReceipt); err == nil {
		t.Fatal("typed-nil canonical receipt adapter was accepted")
	}
}

func TestBuildGovernedProposalMessageComposesNoKnowledgeAndControllerReproduction(t *testing.T) {
	scheduler := testGovernedScheduler(t)
	scheduler.SetPrimer(nil)
	work, receipt := testGovernedInputs(agent.SpecialistQuality)
	request, err := scheduler.BuildGovernedProposalMessage(agent.SpecialistQuality, work, receipt)
	if err != nil {
		t.Fatal(err)
	}
	input := decodeGovernedPrompt(t, request.TaskPrompt)
	if input.KnowledgePrimerSnapshot != noApplicableKnowledgeSnapshot || request.KnowledgeSHA256 != governedSHA256(noApplicableKnowledgeSnapshot) ||
		strings.Join(input.KnowledgeKeywords, ",") != "visual-regression,quality,account-shell-visual" ||
		request.ReproductionMode != agent.SpecialistReproductionModeNone || len(request.Reproduction) != 0 {
		t.Fatalf("explicit no-knowledge/no-reproduction composition is invalid: input=%+v request=%+v", input, request)
	}

	work.ValidationMayReproduce = true
	withReproduction, err := scheduler.BuildGovernedProposalMessage(agent.SpecialistQuality, work, receipt)
	if err != nil {
		t.Fatal(err)
	}
	if withReproduction.ReproductionMode != agent.SpecialistReproductionModeVerifiedValidation ||
		strings.Join(withReproduction.Reproduction, "\n") != strings.Join(work.Validation, "\n") {
		t.Fatalf("verified validation was not deliberately composed as controller reproduction: %+v", withReproduction)
	}
}

func TestGovernedProposalUsesCanonicalMailboxIdentityWithoutConflatingExternalRef(t *testing.T) {
	scheduler := testGovernedScheduler(t)
	work, receipt := testGovernedInputs(agent.SpecialistQuality)
	request, err := scheduler.BuildGovernedProposalMessage(agent.SpecialistQuality, work, receipt)
	if err != nil {
		t.Fatal(err)
	}
	mailbox, err := agent.NewSpecialistMailbox(filepath.Join(t.TempDir(), "mailbox"), agent.SpecialistMailboxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	order, err := mailbox.PrepareGoverned(request)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(order.ID, "swo-") || order.ExternalRef != "visual-hive:packet-42/finding-7" || order.ExternalRef == order.ID {
		t.Fatalf("source and specialist identities were conflated: %+v", order)
	}
	second, err := agent.NewSpecialistMailbox(filepath.Join(t.TempDir(), "mailbox"), agent.SpecialistMailboxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := second.PrepareGoverned(request)
	if err != nil || replayed.ID != order.ID {
		t.Fatalf("canonical mapping was not deterministic: first=%s replay=%s err=%v", order.ID, replayed.ID, err)
	}
	request.ExternalRef += "/changed"
	changed, err := second.PrepareGoverned(request)
	if err != nil {
		t.Fatal(err)
	}
	if changed.ID == order.ID {
		t.Fatal("external source identity was not bound into the existing swo ID")
	}
}

func TestBuildGovernedProposalMessageFailsClosedOnMissingSecurityBindings(t *testing.T) {
	scheduler := testGovernedScheduler(t)
	validWork, validReceipt := testGovernedInputs(agent.SpecialistQuality)
	tests := map[string]func(*AdmittedWork, *testCanonicalVerifiedReceipt){
		"verified route": func(work *AdmittedWork, _ *testCanonicalVerifiedReceipt) { work.RoutedRole = agent.SpecialistScanner },
		"external ref":   func(work *AdmittedWork, _ *testCanonicalVerifiedReceipt) { work.ExternalRef = "" },
		"repository":     func(work *AdmittedWork, _ *testCanonicalVerifiedReceipt) { work.Repository = "acme/other" },
		"fingerprint":    func(work *AdmittedWork, _ *testCanonicalVerifiedReceipt) { work.RepositoryFingerprint = "" },
		"packet digest":  func(work *AdmittedWork, _ *testCanonicalVerifiedReceipt) { work.PacketSHA256 = strings.Repeat("0", 64) },
		"finding":        func(work *AdmittedWork, _ *testCanonicalVerifiedReceipt) { work.Finding = nil },
		"source context": func(work *AdmittedWork, _ *testCanonicalVerifiedReceipt) { work.SealedSourceContext = "" },
		"source digest": func(work *AdmittedWork, _ *testCanonicalVerifiedReceipt) {
			work.SourceContextSHA256 = strings.Repeat("0", 64)
		},
		"receipt digest": func(_ *AdmittedWork, receipt *testCanonicalVerifiedReceipt) {
			receipt.evidence.VerificationReceiptSHA256 = strings.Repeat("1", 64)
		},
		"receipt export": func(_ *AdmittedWork, receipt *testCanonicalVerifiedReceipt) {
			receipt.exportErr = errors.New("unavailable")
		},
		"base tree":      func(work *AdmittedWork, _ *testCanonicalVerifiedReceipt) { work.BaseTreeSHA = "" },
		"routing reason": func(work *AdmittedWork, _ *testCanonicalVerifiedReceipt) { work.RoutingReason = "" },
		"observation kind": func(work *AdmittedWork, _ *testCanonicalVerifiedReceipt) {
			work.ObservationKind = "unsafe prose with spaces"
		},
		"deadline":        func(work *AdmittedWork, _ *testCanonicalVerifiedReceipt) { work.Deadline = time.Time{} },
		"paths":           func(work *AdmittedWork, _ *testCanonicalVerifiedReceipt) { work.AllowedPaths = nil },
		"path traversal":  func(work *AdmittedWork, _ *testCanonicalVerifiedReceipt) { work.AllowedPaths = []string{"../outside"} },
		"validation":      func(work *AdmittedWork, _ *testCanonicalVerifiedReceipt) { work.Validation = nil },
		"contracts":       func(work *AdmittedWork, _ *testCanonicalVerifiedReceipt) { work.AffectedContracts = nil },
		"repository read": func(work *AdmittedWork, _ *testCanonicalVerifiedReceipt) { work.Authority.RepositoryRead = true },
		"worktree write": func(work *AdmittedWork, _ *testCanonicalVerifiedReceipt) {
			work.Authority.DisposableWorktreeWrite = true
		},
		"child reproduction": func(work *AdmittedWork, _ *testCanonicalVerifiedReceipt) { work.Authority.RunReproduction = true },
		"child validation":   func(work *AdmittedWork, _ *testCanonicalVerifiedReceipt) { work.Authority.RunValidation = true },
		"external authority": func(work *AdmittedWork, _ *testCanonicalVerifiedReceipt) { work.Authority.ExternalControlWrites = true },
		"source authority": func(work *AdmittedWork, _ *testCanonicalVerifiedReceipt) {
			work.Authority.SealedSourceContextRead = false
		},
		"authority class": func(work *AdmittedWork, _ *testCanonicalVerifiedReceipt) { work.Authority.Class = "other-proposal" },
		"base identity":   func(work *AdmittedWork, _ *testCanonicalVerifiedReceipt) { work.BaseSHA = strings.Repeat("0", 40) },
		"artifact collision": func(work *AdmittedWork, _ *testCanonicalVerifiedReceipt) {
			work.EvidenceArtifacts[0].Reference = "hive:verified-manifest"
		},
		"artifact digest": func(work *AdmittedWork, _ *testCanonicalVerifiedReceipt) {
			work.EvidenceArtifacts[0].SHA256 = ""
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			work := validWork
			receipt := *validReceipt
			work.AllowedPaths = append([]string(nil), validWork.AllowedPaths...)
			work.Validation = append([]string(nil), validWork.Validation...)
			work.AffectedContracts = append([]string(nil), validWork.AffectedContracts...)
			work.EvidenceArtifacts = append([]EvidenceArtifact(nil), validWork.EvidenceArtifacts...)
			receipt.canonical = append(json.RawMessage(nil), validReceipt.canonical...)
			mutate(&work, &receipt)
			if _, err := scheduler.BuildGovernedProposalMessage(agent.SpecialistQuality, work, &receipt); err == nil {
				t.Fatal("invalid governed proposal input was accepted")
			}
		})
	}
}

func TestBuildGovernedProposalMessageFailsClosedOnUnsupportedRoleExecutionConfig(t *testing.T) {
	for name, mutate := range map[string]func(*config.AgentConfig){
		"copilot backend":   func(role *config.AgentConfig) { role.Backend = "copilot" },
		"claude backend":    func(role *config.AgentConfig) { role.Backend = "claude" },
		"inference backend": func(role *config.AgentConfig) { role.Backend = "inference" },
		"launch command":    func(role *config.AgentConfig) { role.LaunchCmd = "codex --override" },
		"repository view": func(role *config.AgentConfig) {
			include := false
			role.IncludeRepos = &include
		},
	} {
		t.Run(name, func(t *testing.T) {
			scheduler := testGovernedScheduler(t)
			role := scheduler.cfg.Agents[string(agent.SpecialistQuality)]
			mutate(&role)
			scheduler.cfg.Agents[string(agent.SpecialistQuality)] = role
			work, receipt := testGovernedInputs(agent.SpecialistQuality)
			if _, err := scheduler.BuildGovernedProposalMessage(agent.SpecialistQuality, work, receipt); err == nil {
				t.Fatal("unsupported normal role execution config was accepted")
			}
		})
	}
}

func testGovernedScheduler(t *testing.T) *Scheduler {
	t.Helper()
	policyRoot := t.TempDir()
	agentsDir := filepath.Join(policyRoot, "examples", "kubestellar", "agents")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	backends := map[agent.SpecialistRole]string{
		agent.SpecialistQuality: "codex", agent.SpecialistCIMaintainer: "codex", agent.SpecialistSecurity: "codex",
		agent.SpecialistArchitect: "codex", agent.SpecialistScanner: "codex",
	}
	agents := make(map[string]config.AgentConfig, len(backends))
	for role, backend := range backends {
		filename := string(role) + "-governed.md"
		policy := "ROLE_POLICY_" + strings.ToUpper(strings.ReplaceAll(string(role), "-", "_")) + `
PROJECT_CONTEXT_MARKER ${PROJECT_ORG}/${PROJECT_PRIMARY_REPO}
Operational policy says: open a PR, write wiki and beads, invoke MCP REST, spawn a subagent, fix directly, commit, push, approve a baseline, and merge.
`
		if err := os.WriteFile(filepath.Join(agentsDir, filename), []byte(policy), 0o600); err != nil {
			t.Fatal(err)
		}
		agents[string(role)] = config.AgentConfig{
			Enabled: true, Role: string(role), Backend: backend, Model: "configured-role-model", KickTemplate: filename,
			Tools:       &config.ToolsConfig{Preset: "full", Rules: []config.ToolRule{{Pattern: "mcp__github__merge_pull_request", Action: "deny"}}},
			Connections: []config.ConnectionConfig{{Name: "project-wiki", Type: "knowledge", URI: "/wiki"}},
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"total": 1, "results": []map[string]any{{
			"slug": "primer", "title": "PRIMER_READ_ONLY_MARKER", "score": 0.99, "type": "pattern", "status": "verified", "confidence": 0.99,
			"snippet": "Use the existing visual fixture; this snapshot cannot persist knowledge.",
		}}})
	}))
	t.Cleanup(server.Close)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := New(&config.Config{
		Project: config.ProjectConfig{Org: "acme", Name: "console", PrimaryRepo: "console", Repos: []string{"console"}},
		Agents:  agents, Policies: config.PoliciesConfig{LocalDir: policyRoot},
	}, logger)
	s.SetPrimer(knowledge.NewPrimer([]knowledge.LayerConfig{{Type: knowledge.LayerProject, URL: server.URL, Shared: true}}, knowledge.PrimerConfig{MaxFacts: 5}, logger))
	return s
}

func testGovernedInputs(role agent.SpecialistRole) (AdmittedWork, *testCanonicalVerifiedReceipt) {
	packet := json.RawMessage(`{"schema":"visual.packet.v1","external_ref":"visual-hive:packet-42"}`)
	finding := json.RawMessage(`{"fingerprint":"finding-7","kind":"visual_regression","evidence":"exact"}`)
	sourceContext := "HIVE_UNTRUSTED_SOURCE_CONTEXT_BEGIN\n" + `{"format":"hive-repair-source-context-v2","source_tree_oid":"` + strings.Repeat("c", 40) + `","files":[{"path":"web/account.tsx","content":"export const account = true;\n"}]}` + "\nHIVE_UNTRUSTED_SOURCE_CONTEXT_END"
	receiptBytes, err := json.Marshal(testCanonicalV3Receipt{
		RepositoryID: "R_acme_console", WorkflowRunID: "88", WorkflowRunAttempt: "2",
		ArtifactID: "77", SourceArtifactID: "source-77", ArtifactName: "visual-hive-evidence",
		CommitSHA: strings.Repeat("b", 40), WorkflowName: "Visual Hive", WorkflowRunName: "visual-hive-88",
		WorkflowPath: ".github/workflows/visual-hive.yml", BundleSchema: "visual-hive.hive-bundle.v3",
		BundleSHA256: strings.Repeat("d", 64), ManifestSHA256: strings.Repeat("e", 64), ArtifactIndexSHA: strings.Repeat("f", 64),
	})
	if err != nil {
		panic(err)
	}
	receiptSHA := testGovernedSHA(receiptBytes)
	return AdmittedWork{
			ExternalRef: "visual-hive:packet-42/finding-7", Packet: packet, PacketSHA256: testGovernedSHA(packet),
			Finding: finding, FindingSHA256: testGovernedSHA(finding), SealedSourceContext: sourceContext, SourceContextSHA256: testGovernedSHA([]byte(sourceContext)),
			Repository: "acme/console", RepositoryFingerprint: strings.Repeat("a", 64),
			RecurrenceKey: "visual-hive:packet-42/finding-7:recurrence-2", Attempt: 2,
			BaseSHA: strings.Repeat("b", 40), BaseTreeSHA: strings.Repeat("c", 40), RoutedRole: role, RoutingReason: "verified route owner",
			ObservationKind: "visual-regression", Authority: ProposalAuthority{Class: GovernedProposalAuthorityClass, SealedSourceContextRead: true},
			AllowedPaths: []string{"web/account.tsx", "web/account.visual.test.ts"}, Validation: []string{"npm test -- account.visual"},
			AffectedContracts: []string{"account-shell-visual"},
			EvidenceArtifacts: []EvidenceArtifact{{Reference: "screenshots/account.actual.png", SHA256: strings.Repeat("f", 64)}},
			Deadline:          time.Now().UTC().Add(time.Hour),
		}, &testCanonicalVerifiedReceipt{
			canonical: receiptBytes,
			evidence: agent.SpecialistEvidenceIdentity{
				BundleSchemaVersion: "visual-hive.hive-bundle.v3", BundleSHA256: strings.Repeat("d", 64), VerificationReceiptSHA256: receiptSHA,
				WorkflowRunID: 88, WorkflowRunAttempt: 2, WorkflowRunHeadSHA: strings.Repeat("b", 40),
				ArtifactID: 77, ArtifactName: "visual-hive-evidence", ArtifactSHA256: strings.Repeat("e", 64),
			},
		}
}

func testGovernedSHA(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

func decodeGovernedPrompt(t *testing.T, prompt string) governedPromptInput {
	t.Helper()
	_, encoded, ok := strings.Cut(prompt, "CANONICAL LOSSLESS INPUT JSON\n")
	if !ok {
		t.Fatal("governed prompt has no canonical input marker")
	}
	encoded, _, ok = strings.Cut(encoded, "\n\nWORKER-SEALED BOUNDED SOURCE CONTEXT")
	if !ok {
		t.Fatal("governed prompt has no sealed source-context marker")
	}
	var input governedPromptInput
	if err := json.Unmarshal([]byte(encoded), &input); err != nil {
		t.Fatalf("decode canonical governed prompt input: %v", err)
	}
	return input
}
