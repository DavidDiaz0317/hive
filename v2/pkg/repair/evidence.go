package repair

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/kubestellar/hive/v2/pkg/visualhive"
)

const maxVerdictEvidenceBytes = 2 << 20

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
		return "", fmt.Errorf("verified Visual Hive verdict has no actionable contribution for affected contracts")
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
