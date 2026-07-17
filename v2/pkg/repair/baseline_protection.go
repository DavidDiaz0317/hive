package repair

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	maxVisualHiveConfigBytes       = 1 << 20
	maxVisualBaselinePathBytes     = 4096
	maxVisualBaselineListingBytes  = 4 << 20
	maxVisualBaselineProtectedPath = 65536
)

// BaselineProtection is the exact repository-relative visual baseline scope
// that the repair worker must preserve. Enforced is intentionally explicit so
// non-Visual-Hive repair callers keep their existing behavior until they bind
// protection derived from a trusted checkout.
type BaselineProtection struct {
	Enforced bool
	Roots    []string
	Files    []string
}

// InspectVisualBaselineProtection derives the configured Visual Hive snapshot
// root and exact repository baseline files from one checkout. Production
// callers retain this trusted view while candidate worktrees are inspected
// again, preventing a repair from escaping protection by changing snapshotDir.
func InspectVisualBaselineProtection(ctx context.Context, repositoryDir string) (BaselineProtection, error) {
	result := BaselineProtection{Enforced: true}
	if strings.TrimSpace(repositoryDir) == "" {
		return BaselineProtection{}, fmt.Errorf("inspect visual baseline protection: repository directory is required")
	}

	configPath := filepath.Join(repositoryDir, "visual-hive.config.yaml")
	info, err := os.Lstat(configPath)
	switch {
	case err == nil:
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return BaselineProtection{}, fmt.Errorf("inspect visual baseline protection: visual-hive.config.yaml must be a regular file")
		}
		if info.Size() < 0 || info.Size() > maxVisualHiveConfigBytes {
			return BaselineProtection{}, fmt.Errorf("inspect visual baseline protection: visual-hive.config.yaml exceeds %d bytes", maxVisualHiveConfigBytes)
		}
		data, readErr := os.ReadFile(configPath)
		if readErr != nil {
			return BaselineProtection{}, fmt.Errorf("inspect visual baseline protection: read config: %w", readErr)
		}
		if int64(len(data)) != info.Size() {
			return BaselineProtection{}, fmt.Errorf("inspect visual baseline protection: visual-hive.config.yaml changed while it was read")
		}
		var config struct {
			Visual struct {
				SnapshotDir string `yaml:"snapshotDir"`
			} `yaml:"visual"`
		}
		decoder := yaml.NewDecoder(bytes.NewReader(data))
		if decodeErr := decoder.Decode(&config); decodeErr != nil {
			return BaselineProtection{}, fmt.Errorf("inspect visual baseline protection: parse config: %w", decodeErr)
		}
		var trailing any
		if decodeErr := decoder.Decode(&trailing); decodeErr != io.EOF {
			if decodeErr == nil {
				decodeErr = fmt.Errorf("multiple YAML documents are not supported")
			}
			return BaselineProtection{}, fmt.Errorf("inspect visual baseline protection: parse config: %w", decodeErr)
		}
		snapshotDir := config.Visual.SnapshotDir
		if snapshotDir == "" {
			snapshotDir = ".visual-hive/snapshots"
		} else if strings.TrimSpace(snapshotDir) != snapshotDir {
			return BaselineProtection{}, fmt.Errorf("inspect visual baseline protection: visual.snapshotDir contains surrounding whitespace")
		}
		normalized, normalizeErr := normalizeVisualBaselinePath(snapshotDir)
		if normalizeErr != nil {
			return BaselineProtection{}, fmt.Errorf("inspect visual baseline protection: invalid visual.snapshotDir: %w", normalizeErr)
		}
		result.Roots = append(result.Roots, normalized)
	case os.IsNotExist(err):
		// A repository without a Visual Hive config has no configured root, but
		// exact conventional baseline files are still protected below.
	default:
		return BaselineProtection{}, fmt.Errorf("inspect visual baseline protection: stat config: %w", err)
	}

	listing, err := runGitBytes(ctx, repositoryDir, maxVisualBaselineListingBytes,
		"ls-files", "-z", "--cached", "--others", "--exclude-standard", "--")
	if err != nil {
		return BaselineProtection{}, fmt.Errorf("inspect visual baseline protection: list repository files: %w", err)
	}
	for _, raw := range bytes.Split(listing, []byte{0}) {
		if len(raw) == 0 {
			continue
		}
		file, normalizeErr := normalizeVisualBaselinePath(string(raw))
		if normalizeErr != nil {
			return BaselineProtection{}, fmt.Errorf("inspect visual baseline protection: invalid repository path: %w", normalizeErr)
		}
		if visualBaselineConventionFile(file) || visualBaselinePathRestricted(file, result) {
			result.Files = append(result.Files, file)
		}
		if len(result.Files) > maxVisualBaselineProtectedPath {
			return BaselineProtection{}, fmt.Errorf("inspect visual baseline protection: more than %d protected files", maxVisualBaselineProtectedPath)
		}
	}
	return normalizeBaselineProtection(result)
}

func normalizeBaselineProtection(protection BaselineProtection) (BaselineProtection, error) {
	if len(protection.Roots) > maxVisualBaselineProtectedPath || len(protection.Files) > maxVisualBaselineProtectedPath {
		return BaselineProtection{}, fmt.Errorf("visual baseline protection exceeds its path-count bound")
	}
	result := BaselineProtection{Enforced: protection.Enforced}
	for _, root := range protection.Roots {
		normalized, err := normalizeVisualBaselinePath(root)
		if err != nil {
			return BaselineProtection{}, fmt.Errorf("invalid protected visual baseline root: %w", err)
		}
		result.Roots = append(result.Roots, normalized)
	}
	for _, file := range protection.Files {
		normalized, err := normalizeVisualBaselinePath(file)
		if err != nil {
			return BaselineProtection{}, fmt.Errorf("invalid protected visual baseline file: %w", err)
		}
		result.Files = append(result.Files, normalized)
	}
	result.Roots = sortedFoldedUnique(result.Roots)
	result.Files = sortedFoldedUnique(result.Files)
	return result, nil
}

func mergeBaselineProtection(values ...BaselineProtection) (BaselineProtection, error) {
	result := BaselineProtection{}
	for _, value := range values {
		normalized, err := normalizeBaselineProtection(value)
		if err != nil {
			return BaselineProtection{}, err
		}
		result.Enforced = result.Enforced || normalized.Enforced
		result.Roots = append(result.Roots, normalized.Roots...)
		result.Files = append(result.Files, normalized.Files...)
	}
	return normalizeBaselineProtection(result)
}

func baselineProtectionForWorktree(ctx context.Context, worktree string, trusted BaselineProtection) (BaselineProtection, error) {
	normalized, err := normalizeBaselineProtection(trusted)
	if err != nil {
		return BaselineProtection{}, err
	}
	if !normalized.Enforced {
		return normalized, nil
	}
	candidate, err := InspectVisualBaselineProtection(ctx, worktree)
	if err != nil {
		return BaselineProtection{}, fmt.Errorf("inspect candidate visual baseline protection: %w", err)
	}
	return mergeBaselineProtection(normalized, candidate)
}

func visualBaselinePathRestricted(file string, protection BaselineProtection) bool {
	if !protection.Enforced {
		return false
	}
	normalized, err := normalizeVisualBaselinePath(file)
	if err != nil {
		return true
	}
	for _, protectedFile := range protection.Files {
		if strings.EqualFold(normalized, protectedFile) {
			return true
		}
	}
	for _, root := range protection.Roots {
		if strings.EqualFold(normalized, root) || len(normalized) > len(root) && strings.EqualFold(normalized[:len(root)], root) && normalized[len(root)] == '/' {
			return true
		}
	}
	return false
}

func normalizeVisualBaselinePath(value string) (string, error) {
	if value == "" || len(value) > maxVisualBaselinePathBytes || strings.ContainsRune(value, 0) || strings.Contains(value, "\\") {
		return "", fmt.Errorf("path must be a bounded non-empty POSIX-style repository path")
	}
	looksLikeDrivePath := len(value) >= 2 && value[1] == ':' && (value[0] >= 'A' && value[0] <= 'Z' || value[0] >= 'a' && value[0] <= 'z')
	if filepath.IsAbs(value) || filepath.VolumeName(value) != "" || looksLikeDrivePath || strings.HasPrefix(value, "/") {
		return "", fmt.Errorf("path %q must be repository-relative", value)
	}
	normalized := path.Clean(strings.TrimPrefix(value, "./"))
	if normalized == "." || normalized == ".." || strings.HasPrefix(normalized, "../") || strings.HasPrefix(normalized, "/") {
		return "", fmt.Errorf("path %q escapes the repository", value)
	}
	return normalized, nil
}

func visualBaselineConventionFile(file string) bool {
	lower := strings.ToLower(file)
	if !strings.HasSuffix(lower, ".png") && !strings.HasSuffix(lower, ".json") {
		return false
	}
	for _, suffix := range []string{"-actual.png", "-diff.png", ".actual.png", ".diff.png", "-actual.json", "-diff.json", ".actual.json", ".diff.json"} {
		if strings.HasSuffix(lower, suffix) {
			return false
		}
	}
	return strings.Contains(lower, "baseline") || strings.Contains("/"+lower, "/__screenshots__/") ||
		strings.Contains(lower, "-snapshots/") || strings.HasPrefix(lower, ".visual-hive/snapshots/")
}

func sortedFoldedUnique(values []string) []string {
	sort.Slice(values, func(i, j int) bool {
		return strings.ToLower(values[i]) < strings.ToLower(values[j])
	})
	result := make([]string, 0, len(values))
	for _, value := range values {
		if len(result) == 0 || !strings.EqualFold(result[len(result)-1], value) {
			result = append(result, value)
		}
	}
	return result
}
