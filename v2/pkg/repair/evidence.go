package repair

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/kubestellar/hive/v2/pkg/visualhive"
)

const maxVerdictEvidenceBytes = 2 << 20
const maxCoverageEvidenceBytes = 2 << 20
const maxTestCreationEvidenceBytes = 2 << 20

var ErrNoActionableEvidence = errors.New("verified Visual Hive evidence has no actionable contribution")

type verdictEvidence struct {
	SchemaVersion    string                 `json:"schemaVersion"`
	AllContributions []evidenceContribution `json:"allContributions"`
}

type evidenceContribution struct {
	Source     string `json:"source"`
	Kind       string `json:"kind"`
	Status     string `json:"status"`
	Gating     bool   `json:"gating"`
	Mode       string `json:"mode"`
	ContractID string `json:"contractId"`
	TargetID   string `json:"targetId"`
	Reason     string `json:"reason"`
	Key        string `json:"key"`
	Authority  string `json:"authority"`
}

type coverageEvidence struct {
	MaintenanceFindings []coverageMaintenanceFinding `json:"maintenanceFindings"`
	Recommendations     []coverageRecommendation     `json:"recommendations"`
}

type coverageMaintenanceFinding struct {
	ID                string   `json:"id"`
	Kind              string   `json:"kind"`
	ContractID        string   `json:"contractId"`
	TargetID          string   `json:"targetId"`
	Route             string   `json:"route"`
	Viewport          string   `json:"viewport"`
	Message           string   `json:"message"`
	Evidence          []string `json:"evidence"`
	RecommendedAction string   `json:"recommendedAction"`
}

type coverageRecommendation struct {
	ID                   string   `json:"id"`
	Kind                 string   `json:"kind"`
	Title                string   `json:"title"`
	ContractID           string   `json:"contractId"`
	TargetID             string   `json:"targetId"`
	Route                string   `json:"route"`
	Viewport             string   `json:"viewport"`
	Rationale            []string `json:"rationale"`
	SuggestedTests       []string `json:"suggestedTests"`
	MaintenanceFindingID string   `json:"maintenanceFindingId"`
	SuggestedConfigYAML  string   `json:"suggestedConfigYaml"`
}

type testCreationEvidence struct {
	SchemaVersion   string                       `json:"schemaVersion"`
	Recommendations []testCreationRecommendation `json:"recommendations"`
}

type testCreationRecommendation struct {
	ID             string   `json:"id"`
	GapID          string   `json:"gapId"`
	Source         string   `json:"source"`
	Kind           string   `json:"kind"`
	Priority       string   `json:"priority"`
	Title          string   `json:"title"`
	Rationale      []string `json:"rationale"`
	SuggestedTests []string `json:"suggestedTests"`
	Artifacts      []string `json:"artifacts"`
}

// LoadEvidenceSummary reads only the deterministic verdict from an independently
// fetched source artifact. It deliberately does not expose arbitrary repository
// prose or large artifact payloads to the repair model.
func LoadEvidenceSummary(root string, finding visualhive.FindingLifecycle) (string, error) {
	if strings.TrimSpace(root) == "" {
		return "", nil
	}
	verdictPath := filepath.Join(root, "verdict.json")
	info, err := os.Lstat(verdictPath)
	if err != nil {
		return "", fmt.Errorf("verified Visual Hive evidence is missing verdict.json: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maxVerdictEvidenceBytes {
		return "", fmt.Errorf("verified Visual Hive verdict has an invalid size or type")
	}
	file, err := os.Open(verdictPath)
	if err != nil {
		return "", err
	}
	defer file.Close()
	var verdict verdictEvidence
	decoder := json.NewDecoder(io.LimitReader(file, maxVerdictEvidenceBytes+1))
	if err := decoder.Decode(&verdict); err != nil {
		return "", fmt.Errorf("decode verified Visual Hive verdict: %w", err)
	}
	if len(verdict.AllContributions) > 2048 {
		return "", fmt.Errorf("verified Visual Hive verdict has too many contributions")
	}
	if strings.EqualFold(strings.TrimSpace(finding.IssueKind), "test_adequacy_gap") {
		return loadTestCreationEvidenceSummary(root, finding)
	}

	contracts := make(map[string]bool, len(finding.AffectedContracts))
	for _, contract := range finding.AffectedContracts {
		contracts[strings.ToLower(strings.TrimSpace(contract))] = true
	}
	title := strings.ToLower(finding.Title)
	matched := make([]evidenceContribution, 0)
	for _, contribution := range verdict.AllContributions {
		if !contracts[strings.ToLower(strings.TrimSpace(contribution.ContractID))] {
			continue
		}
		status := strings.ToLower(strings.TrimSpace(contribution.Status))
		if status != "failed" && status != "blocked" && status != "warning" {
			continue
		}
		if signal := strings.ToLower(strings.TrimSpace(contribution.Kind)); signal != "" && strings.Contains(title, signal) {
			matched = append(matched, contribution)
		}
	}
	if len(matched) == 0 {
		for _, contribution := range verdict.AllContributions {
			if contracts[strings.ToLower(strings.TrimSpace(contribution.ContractID))] && contribution.Gating && strings.EqualFold(contribution.Status, "failed") {
				matched = append(matched, contribution)
			}
		}
	}
	if len(matched) == 0 {
		coverageSummary, coverageErr := loadCoverageEvidenceSummary(root, finding, contracts)
		if coverageErr != nil {
			return "", coverageErr
		}
		if coverageSummary != "" {
			return coverageSummary, nil
		}
		return "", fmt.Errorf("%w for affected contracts", ErrNoActionableEvidence)
	}

	lines := make([]string, 0, len(matched))
	seen := map[string]bool{}
	for _, contribution := range matched {
		values := []string{contribution.Key, contribution.Source, contribution.Kind, contribution.Status, contribution.ContractID, contribution.TargetID, contribution.Reason}
		valid := true
		for index, value := range values {
			value = strings.TrimSpace(value)
			if strings.ContainsRune(value, '\x00') || len(value) > 4096 || providerSecret.MatchString(value) {
				valid = false
				break
			}
			values[index] = value
		}
		if !valid {
			return "", fmt.Errorf("verified Visual Hive evidence contains an unsafe value")
		}
		line := fmt.Sprintf("- key=%s source=%s kind=%s status=%s contract=%s target=%s reason=%s", values[0], values[1], values[2], values[3], values[4], values[5], values[6])
		if !seen[line] {
			seen[line] = true
			lines = append(lines, line)
		}
	}
	sort.Strings(lines)
	summary := strings.Join(lines, "\n")
	if len(summary) > 16<<10 {
		return "", fmt.Errorf("verified Visual Hive evidence summary exceeds limit")
	}
	return string(bytes.Clone([]byte(summary))), nil
}

func loadTestCreationEvidenceSummary(root string, finding visualhive.FindingLifecycle) (string, error) {
	path := filepath.Join(root, "test-creation-plan.json")
	info, err := os.Lstat(path)
	if err != nil {
		return "", fmt.Errorf("verified Visual Hive test evidence is missing test-creation-plan.json: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maxTestCreationEvidenceBytes {
		return "", fmt.Errorf("verified Visual Hive test-creation evidence has an invalid size or type")
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	var evidence testCreationEvidence
	if err := json.NewDecoder(io.LimitReader(file, maxTestCreationEvidenceBytes+1)).Decode(&evidence); err != nil {
		return "", fmt.Errorf("decode verified Visual Hive test-creation evidence: %w", err)
	}
	if evidence.SchemaVersion != "visual-hive.test-creation-plan.v1" {
		return "", fmt.Errorf("verified Visual Hive test-creation evidence has unsupported schema %q", evidence.SchemaVersion)
	}
	if len(evidence.Recommendations) > 2048 {
		return "", fmt.Errorf("verified Visual Hive test-creation evidence has too many recommendations")
	}
	title := strings.ToLower(strings.TrimSpace(finding.Title))
	lines := make([]string, 0, 1)
	for _, recommendation := range evidence.Recommendations {
		if !strings.EqualFold(strings.TrimSpace(recommendation.Source), "testing_layer") || !strings.EqualFold(strings.TrimSpace(recommendation.Kind), "unit_test") {
			continue
		}
		if recommendation.Title != "" && !strings.Contains(title, strings.ToLower(strings.TrimSpace(recommendation.Title))) {
			continue
		}
		values := []string{
			recommendation.ID, recommendation.GapID, recommendation.Source, recommendation.Kind, recommendation.Priority,
			recommendation.Title, strings.Join(recommendation.Rationale, ","), strings.Join(recommendation.SuggestedTests, ","), strings.Join(recommendation.Artifacts, ","),
		}
		for index, value := range values {
			value = strings.TrimSpace(value)
			if strings.ContainsRune(value, '\x00') || len(value) > 4096 || providerSecret.MatchString(value) {
				return "", fmt.Errorf("verified Visual Hive test-creation evidence contains an unsafe value")
			}
			values[index] = strings.ReplaceAll(value, "\n", "\\n")
		}
		lines = append(lines, fmt.Sprintf("- key=test_creation.%s gap=%s source=%s kind=%s priority=%s title=%s rationale=%s suggested_tests=%s artifacts=%s required_scope=test_files_only", values[0], values[1], values[2], values[3], values[4], values[5], values[6], values[7], values[8]))
	}
	if len(lines) == 0 {
		return "", fmt.Errorf("verified Visual Hive test-creation evidence has no matching unit-test recommendation")
	}
	sort.Strings(lines)
	summary := strings.Join(lines, "\n")
	if len(summary) > 16<<10 {
		return "", fmt.Errorf("verified Visual Hive test-creation evidence summary exceeds limit")
	}
	return string(bytes.Clone([]byte(summary))), nil
}

func loadCoverageEvidenceSummary(root string, finding visualhive.FindingLifecycle, contracts map[string]bool) (string, error) {
	kind := strings.ToLower(strings.TrimSpace(finding.IssueKind))
	if kind != "missing_visual_coverage" && kind != "weak_visual_test" {
		return "", nil
	}
	path := filepath.Join(root, "coverage-recommendations.json")
	info, err := os.Lstat(path)
	if err != nil {
		return "", fmt.Errorf("verified Visual Hive coverage evidence is missing coverage-recommendations.json: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maxCoverageEvidenceBytes {
		return "", fmt.Errorf("verified Visual Hive coverage evidence has an invalid size or type")
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	var evidence coverageEvidence
	if err := json.NewDecoder(io.LimitReader(file, maxCoverageEvidenceBytes+1)).Decode(&evidence); err != nil {
		return "", fmt.Errorf("decode verified Visual Hive coverage evidence: %w", err)
	}
	if len(evidence.MaintenanceFindings) > 2048 || len(evidence.Recommendations) > 2048 {
		return "", fmt.Errorf("verified Visual Hive coverage evidence has too many entries")
	}
	recommendations := make(map[string]string, len(evidence.Recommendations))
	for _, recommendation := range evidence.Recommendations {
		if id := strings.TrimSpace(recommendation.MaintenanceFindingID); id != "" {
			recommendations[id] = recommendation.SuggestedConfigYAML
		}
	}
	title := strings.ToLower(finding.Title)
	lines := make([]string, 0)
	for _, item := range evidence.Recommendations {
		if !contracts[strings.ToLower(strings.TrimSpace(item.ContractID))] {
			continue
		}
		itemKind, itemTitle := strings.ToLower(strings.TrimSpace(item.Kind)), strings.ToLower(strings.TrimSpace(item.Title))
		if (itemKind == "" || !strings.Contains(title, itemKind)) && (itemTitle == "" || !strings.Contains(title, itemTitle)) {
			continue
		}
		values := []string{item.ID, item.Kind, item.ContractID, item.TargetID, item.Route, item.Viewport, item.Title, strings.Join(item.Rationale, ","), strings.Join(item.SuggestedTests, ","), item.SuggestedConfigYAML}
		for index, value := range values {
			value = strings.TrimSpace(value)
			if strings.ContainsRune(value, '\x00') || len(value) > 4096 || providerSecret.MatchString(value) {
				return "", fmt.Errorf("verified Visual Hive coverage evidence contains an unsafe value")
			}
			values[index] = strings.ReplaceAll(value, "\n", "\\n")
		}
		lines = append(lines, fmt.Sprintf("- key=coverage.%s source=coverage kind=%s status=warning contract=%s target=%s route=%s viewport=%s reason=%s rationale=%s suggested_tests=%s suggested_config=%s", values[0], values[1], values[2], values[3], values[4], values[5], values[6], values[7], values[8], values[9]))
	}
	for _, item := range evidence.MaintenanceFindings {
		if !contracts[strings.ToLower(strings.TrimSpace(item.ContractID))] || !strings.Contains(title, strings.ToLower(strings.TrimSpace(item.Kind))) {
			continue
		}
		values := []string{item.ID, item.Kind, item.ContractID, item.TargetID, item.Route, item.Viewport, item.Message, item.RecommendedAction, strings.Join(item.Evidence, ","), recommendations[strings.TrimSpace(item.ID)]}
		for index, value := range values {
			value = strings.TrimSpace(value)
			if strings.ContainsRune(value, '\x00') || len(value) > 4096 || providerSecret.MatchString(value) {
				return "", fmt.Errorf("verified Visual Hive coverage evidence contains an unsafe value")
			}
			values[index] = strings.ReplaceAll(value, "\n", "\\n")
		}
		lines = append(lines, fmt.Sprintf("- key=coverage.%s source=coverage kind=%s status=warning contract=%s target=%s route=%s viewport=%s reason=%s action=%s evidence=%s suggested_config=%s", values[0], values[1], values[2], values[3], values[4], values[5], values[6], values[7], values[8], values[9]))
	}
	sort.Strings(lines)
	summary := strings.Join(lines, "\n")
	if len(summary) > 16<<10 {
		return "", fmt.Errorf("verified Visual Hive coverage evidence summary exceeds limit")
	}
	return string(bytes.Clone([]byte(summary))), nil
}
