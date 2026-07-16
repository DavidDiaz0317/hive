package credentialguard

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
)

type credentialLiteralPattern struct {
	name    string
	pattern *regexp.Regexp
}

// TestRepositoryHasNoCredentialLiterals prevents realistic provider-shaped
// fixture values from being committed. Tests that exercise credential
// detection must assemble their input at runtime instead.
func TestRepositoryHasNoCredentialLiterals(t *testing.T) {
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve credential guard source path")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(sourceFile), "..", ".."))

	patterns := credentialLiteralPatterns()

	var findings []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if path != root && ignoredCredentialGuardDir(entry.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !entry.Type().IsRegular() || !credentialGuardTextFile(path) {
			return nil
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()

		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		scanner := bufio.NewScanner(file)
		buffer := make([]byte, 64<<10)
		scanner.Buffer(buffer, 2<<20)
		line := 0
		for scanner.Scan() {
			line++
			for _, candidate := range patterns {
				if candidate.pattern.Match(scanner.Bytes()) {
					findings = append(findings, fmt.Sprintf("%s:%d: %s", filepath.ToSlash(relative), line, candidate.name))
				}
			}
		}
		return scanner.Err()
	})
	if err != nil {
		t.Fatalf("scan repository fixture literals: %v", err)
	}
	if len(findings) != 0 {
		sort.Strings(findings)
		t.Fatalf("credential-shaped literals must be assembled at runtime:\n%s", strings.Join(findings, "\n"))
	}
}

func credentialLiteralPatterns() []credentialLiteralPattern {
	return []credentialLiteralPattern{
		{name: "cloud access key identifier", pattern: regexp.MustCompile(`\b(?:AK` + `IA|AS` + `IA)[A-Z0-9]{16}\b`)},
		{name: "collaboration token", pattern: regexp.MustCompile(`\bxo` + `x[baprs]-[A-Za-z0-9-]{10,}\b`)},
		{name: "source-host token", pattern: regexp.MustCompile(`\b(?:github` + `_pat_[A-Za-z0-9_]{20,}|gh` + `[pousr]_[A-Za-z0-9]{20,})\b`)},
		{name: "alternate source-host token", pattern: regexp.MustCompile(`\bgl` + `pat-[A-Za-z0-9_-]{20,}\b`)},
		{name: "model-provider key", pattern: regexp.MustCompile(`\bs` + `k-(?:(?:proj|svcacct)-)?[A-Za-z0-9_-]{20,}\b`)},
		{name: "browser-platform key", pattern: regexp.MustCompile(`\bAI` + `za[0-9A-Za-z_-]{20,}\b`)},
		{name: "package-registry token", pattern: regexp.MustCompile(`\bnpm` + `_[A-Za-z0-9]{30,}\b`)},
		{name: "python-registry token", pattern: regexp.MustCompile(`\bpyp` + `i-[A-Za-z0-9_-]{20,}\b`)},
		{name: "payment-provider live key", pattern: regexp.MustCompile(`\b(?:s` + `k|rk)_live_[A-Za-z0-9]{16,}\b`)},
	}
}

func TestCredentialLiteralPatterns(t *testing.T) {
	fixtures := []string{
		"AK" + "IA" + "ABCDEFGHIJKLMNOP",
		"AS" + "IA" + "ABCDEFGHIJKLMNOP",
		"xo" + "xb-1234567890-ABCDEFGHIJ",
		"github" + "_pat_ABCDEFGHIJKLMNOPQRSTUVWXYZ123456",
		"gh" + "p_ABCDEFGHIJKLMNOPQRSTUVWXYZ123456",
		"gl" + "pat-ABCDEFGHIJKLMNOPQRSTUVWXYZ123456",
		"s" + "k-proj-ABCDEFGHIJKLMNOPQRSTUVWXYZ123456",
		"AI" + "zaABCDEFGHIJKLMNOPQRSTUVWXYZ123456",
		"npm" + "_ABCDEFGHIJKLMNOPQRSTUVWXYZ1234567890",
		"pyp" + "i-ABCDEFGHIJKLMNOPQRSTUVWXYZ123456",
		"s" + "k_live_ABCDEFGHIJKLMNOPQRSTUVWXYZ",
	}
	patterns := credentialLiteralPatterns()
	for index, fixture := range fixtures {
		matched := false
		for _, candidate := range patterns {
			matched = matched || candidate.pattern.MatchString(fixture)
		}
		if !matched {
			t.Errorf("credential fixture %d was not detected", index)
		}
	}
}

func ignoredCredentialGuardDir(name string) bool {
	switch name {
	case ".git", ".beads", "node_modules", "vendor", "dist", "build", "coverage":
		return true
	default:
		return false
	}
}

func credentialGuardTextFile(path string) bool {
	switch strings.ToLower(filepath.Base(path)) {
	case "dockerfile", "makefile", "procfile", ".gitignore", ".gitattributes", ".npmrc":
		return true
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".go", ".md", ".yaml", ".yml", ".json", ".toml", ".sh", ".ps1", ".txt", ".env", ".example", ".html", ".xml", ".ini", ".conf", ".properties", ".sql", ".py", ".rb", ".java", ".rs", ".cs", ".c", ".cc", ".cpp", ".h", ".hpp", ".js", ".mjs", ".cjs", ".jsx", ".ts", ".tsx", ".vue", ".svelte":
		return true
	default:
		return false
	}
}
