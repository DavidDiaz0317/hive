package integrated

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type packageJSON struct {
	Scripts         map[string]string `json:"scripts"`
	Dependencies    map[string]string `json:"dependencies"`
	DevDependencies map[string]string `json:"devDependencies"`
}

func InspectCheckout(root, defaultBranch string) (RepositoryInspection, error) {
	inspection := RepositoryInspection{DefaultBranch: defaultBranch, Permissions: map[string]bool{}, Signals: map[string]string{}}
	languages, frameworks, managers := map[string]bool{}, map[string]bool{}, map[string]bool{}
	var packageData packageJSON
	if data, err := os.ReadFile(filepath.Join(root, "package.json")); err == nil {
		_ = json.Unmarshal(data, &packageData)
		languages["TypeScript/JavaScript"] = true
		for name := range mergeMaps(packageData.Dependencies, packageData.DevDependencies) {
			switch {
			case name == "react" || strings.HasPrefix(name, "@react-"):
				frameworks["React"] = true
			case name == "next":
				frameworks["Next.js"] = true
			case name == "vue":
				frameworks["Vue"] = true
			case name == "@playwright/test":
				frameworks["Playwright"] = true
			case strings.Contains(name, "storybook"):
				frameworks["Storybook"] = true
			}
		}
		for name, script := range packageData.Scripts {
			lowerName := strings.ToLower(name)
			if name == "test" || name == "build" || name == "vh:plan" || name == "vh:run" || strings.Contains(lowerName, "test") || strings.Contains(lowerName, "lint") || strings.Contains(lowerName, "typecheck") || strings.Contains(lowerName, "suite") || strings.Contains(lowerName, "mutation") || strings.Contains(lowerName, "mutate") || strings.Contains(lowerName, "e2e") || strings.Contains(lowerName, "visual") {
				inspection.TestCommands = append(inspection.TestCommands, []string{"npm", "run", name})
				inspection.Signals["script:"+name] = script
			}
		}
		inspection.TestCommands = preferGranularTestCommands(inspection.TestCommands)
	}
	if exists(filepath.Join(root, "package-lock.json")) {
		managers["npm"] = true
	}
	if exists(filepath.Join(root, "pnpm-lock.yaml")) {
		managers["pnpm"] = true
	}
	if exists(filepath.Join(root, "yarn.lock")) {
		managers["yarn"] = true
	}
	if exists(filepath.Join(root, "go.mod")) {
		languages["Go"] = true
		managers["Go modules"] = true
		inspection.TestCommands = append(inspection.TestCommands, []string{"go", "test", "./..."})
	}
	if exists(filepath.Join(root, "pyproject.toml")) || exists(filepath.Join(root, "requirements.txt")) {
		languages["Python"] = true
		managers["pip/pyproject"] = true
	}

	count := 0
	_ = filepath.WalkDir(root, func(filePath string, entry fs.DirEntry, err error) error {
		if err != nil || count > 10000 {
			return nil
		}
		relative, relErr := filepath.Rel(root, filePath)
		if relErr != nil {
			return nil
		}
		relative = filepath.ToSlash(relative)
		if entry.IsDir() {
			base := entry.Name()
			if base == ".git" || base == "node_modules" || base == "dist" || base == "vendor" || base == ".visual-hive" {
				return filepath.SkipDir
			}
			return nil
		}
		count++
		lower := strings.ToLower(relative)
		switch filepath.Ext(lower) {
		case ".go":
			languages["Go"] = true
		case ".py":
			languages["Python"] = true
		case ".rs":
			languages["Rust"] = true
		case ".java", ".kt":
			languages["JVM"] = true
		case ".ts", ".tsx", ".js", ".jsx":
			languages["TypeScript/JavaScript"] = true
		}
		if strings.HasPrefix(lower, ".github/workflows/") {
			inspection.CIFiles = append(inspection.CIFiles, relative)
		}
		if strings.Contains(lower, "dockerfile") || strings.HasPrefix(lower, "deploy/") || strings.HasPrefix(lower, "k8s/") || strings.HasSuffix(lower, "vercel.json") || strings.Contains(lower, "terraform") {
			inspection.DeploymentFiles = append(inspection.DeploymentFiles, relative)
		}
		if strings.Contains(lower, "baseline") && (strings.HasSuffix(lower, ".png") || strings.HasSuffix(lower, ".json")) {
			inspection.BaselineFiles = append(inspection.BaselineFiles, relative)
		}
		if strings.Contains(lower, "auth") || strings.Contains(lower, "security") || strings.Contains(lower, "secret") || strings.HasPrefix(lower, ".github/workflows/") {
			inspection.HighRiskPaths = append(inspection.HighRiskPaths, relative)
		}
		return nil
	})
	inspection.Languages = sortedKeys(languages)
	inspection.Frameworks = sortedKeys(frameworks)
	inspection.PackageManagers = sortedKeys(managers)
	inspection.CIFiles = sortedUnique(inspection.CIFiles)
	inspection.DeploymentFiles = sortedUnique(inspection.DeploymentFiles)
	inspection.BaselineFiles = sortedUnique(inspection.BaselineFiles)
	inspection.HighRiskPaths = sortedUnique(inspection.HighRiskPaths)
	sort.SliceStable(inspection.TestCommands, func(i, j int) bool {
		left, right := testCommandPriority(inspection.TestCommands[i]), testCommandPriority(inspection.TestCommands[j])
		if left != right {
			return left < right
		}
		return strings.Join(inspection.TestCommands[i], "\x00") < strings.Join(inspection.TestCommands[j], "\x00")
	})
	return inspection, nil
}

// testCommandPriority keeps generated repair validation plans dependency-safe.
// Repository producers (tests, suites, mutation runs) must execute before proof
// and validation scripts that consume their reports. Derived planning belongs
// last because it may consume both the primary and mutation evidence.
func testCommandPriority(command []string) int {
	value := strings.ToLower(strings.Join(command, " "))
	switch {
	case strings.Contains(value, "test-creation") || strings.Contains(value, "test_creation"):
		return 50
	case strings.Contains(value, "proof") || strings.Contains(value, "verify") || strings.Contains(value, "validation") || strings.Contains(value, "validate") || strings.Contains(value, "check-"):
		return 40
	case strings.Contains(value, "mutation") || strings.Contains(value, "mutate"):
		return 35
	case strings.Contains(value, "suite") || strings.Contains(value, " e2e") || strings.Contains(value, " visual") || strings.Contains(value, " test") || strings.Contains(value, " run"):
		return 30
	case strings.Contains(value, "typecheck") || strings.Contains(value, "lint"):
		return 20
	case strings.Contains(value, " plan"):
		return 25
	case strings.Contains(value, " build"):
		return 10
	default:
		return 30
	}
}

func preferGranularTestCommands(commands [][]string) [][]string {
	hasGranularProducer := false
	for _, command := range commands {
		name := commandScriptName(command)
		if !strings.Contains(name, "suite") && isEvidenceProducerScript(name) {
			hasGranularProducer = true
			break
		}
	}
	if !hasGranularProducer {
		return commands
	}
	result := make([][]string, 0, len(commands))
	for _, command := range commands {
		if !strings.Contains(commandScriptName(command), "suite") {
			result = append(result, command)
		}
	}
	return result
}

func commandScriptName(command []string) string {
	if len(command) == 0 {
		return ""
	}
	return strings.ToLower(command[len(command)-1])
}

func isEvidenceProducerScript(name string) bool {
	if name == "test" || name == "vh:run" || strings.Contains(name, "mutate") || strings.Contains(name, "e2e") || strings.Contains(name, "visual") {
		return true
	}
	return strings.Contains(name, "test") && !strings.Contains(name, "test-creation") && !strings.Contains(name, "proof") && !strings.Contains(name, "check") && !strings.Contains(name, "validate")
}

func mergeMaps(left, right map[string]string) map[string]string {
	result := map[string]string{}
	for key, value := range left {
		result[key] = value
	}
	for key, value := range right {
		result[key] = value
	}
	return result
}

func exists(path string) bool { _, err := os.Stat(path); return err == nil }
func sortedKeys(values map[string]bool) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}
func sortedUnique(values []string) []string {
	sort.Strings(values)
	result := values[:0]
	for _, value := range values {
		if len(result) == 0 || result[len(result)-1] != value {
			result = append(result, value)
		}
	}
	return result
}
