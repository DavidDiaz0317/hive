package repair

import (
	"context"
	"fmt"
	"os/exec"
	"path"
	"regexp"
	"sort"
	"strings"
)

const (
	modelPatchBegin = "HIVE_PATCH_BEGIN"
	modelPatchEnd   = "HIVE_PATCH_END"
	maxModelPatch   = 128 << 10
)

var diffHeader = regexp.MustCompile(`(?m)^diff --git a/([^\r\n]+) b/([^\r\n]+)\r?$`)

func extractModelPatch(output string) (string, error) {
	begin := strings.Index(output, modelPatchBegin)
	end := strings.Index(output, modelPatchEnd)
	if begin < 0 && end < 0 {
		return "", nil
	}
	if begin < 0 || end < 0 || end <= begin || strings.Count(output, modelPatchBegin) != 1 || strings.Count(output, modelPatchEnd) != 1 {
		return "", fmt.Errorf("model response has invalid Hive patch markers")
	}
	patch := strings.TrimSpace(output[begin+len(modelPatchBegin) : end])
	patch = strings.ReplaceAll(patch, "\r\n", "\n")
	patch = strings.TrimPrefix(patch, "```diff\n")
	patch = strings.TrimPrefix(patch, "```patch\n")
	patch = strings.TrimSuffix(patch, "```")
	patch = strings.TrimSpace(patch)
	if patch == "" || len(patch) > maxModelPatch {
		return "", fmt.Errorf("model patch is empty or exceeds %d bytes", maxModelPatch)
	}
	if providerSecret.MatchString(patch) || strings.ContainsRune(patch, '\x00') {
		return "", fmt.Errorf("model patch contains an unsafe value")
	}
	return patch + "\n", nil
}

func patchChangedFiles(patchText string) ([]string, error) {
	if len(patchText) == 0 || len(patchText) > maxModelPatch {
		return nil, fmt.Errorf("model patch is empty or exceeds %d bytes", maxModelPatch)
	}
	if providerSecret.MatchString(patchText) || strings.ContainsRune(patchText, '\x00') {
		return nil, fmt.Errorf("model patch contains an unsafe value")
	}
	lower := strings.ToLower(patchText)
	for _, forbidden := range []string{"git binary patch", "binary files ", "rename from ", "rename to ", "copy from ", "copy to ", "old mode ", "new mode ", "submodule "} {
		if strings.Contains(lower, forbidden) {
			return nil, fmt.Errorf("model patch contains forbidden %s metadata", strings.TrimSpace(forbidden))
		}
	}
	matches := diffHeader.FindAllStringSubmatch(patchText, -1)
	if len(matches) == 0 || len(matches) > 64 {
		return nil, fmt.Errorf("model patch must contain between 1 and 64 text file diffs")
	}
	files := make([]string, 0, len(matches))
	seen := map[string]bool{}
	for _, match := range matches {
		left, right := strings.TrimSpace(match[1]), strings.TrimSpace(match[2])
		if left != right || !safePatchPath(right) {
			return nil, fmt.Errorf("model patch has an unsafe or renamed path")
		}
		if seen[right] {
			return nil, fmt.Errorf("model patch repeats file %s", right)
		}
		seen[right] = true
		files = append(files, right)
	}
	sort.Strings(files)
	return files, nil
}

func safePatchPath(file string) bool {
	return file != "" && strings.TrimSpace(file) == file && !strings.Contains(file, "\"") && !strings.Contains(file, "\\") && !strings.Contains(file, ":") && !strings.HasPrefix(file, "/") &&
		!strings.HasPrefix(file, "../") && path.Clean(file) == file && !strings.HasPrefix(file, ".git/")
}

func applyModelPatch(ctx context.Context, worktree, patchText string) error {
	for _, args := range [][]string{
		{"apply", "--check", "--whitespace=error-all", "--recount", "-"},
		{"apply", "--whitespace=error-all", "--recount", "-"},
	} {
		command := exec.CommandContext(ctx, "git", args...)
		command.Dir = worktree
		command.Env = append(providerEnvironment(), "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=credential.interactive", "GIT_CONFIG_VALUE_0=false")
		command.Stdin = strings.NewReader(patchText)
		var output limitedBuffer
		command.Stdout, command.Stderr = &output, &output
		if err := command.Run(); err != nil {
			return fmt.Errorf("git %s model patch failed: %w: %s", strings.Join(args[:len(args)-1], " "), err, safeExcerpt(output.String()))
		}
	}
	return nil
}

func modelPatchAlreadyApplied(ctx context.Context, worktree, patchText string) (bool, error) {
	ok, _, err := checkModelPatch(ctx, worktree, patchText, true, false)
	return ok, err
}

func applyIncrementalModelPatch(ctx context.Context, worktree, patchText string) error {
	files, err := patchChangedFiles(patchText)
	if err != nil {
		return err
	}
	addArgs := append([]string{"add", "--"}, files...)
	if _, err := runGit(ctx, worktree, addArgs...); err != nil {
		return fmt.Errorf("stage cumulative repair state for incremental patch: %w", err)
	}
	resetIndex := func() error {
		resetArgs := append([]string{"reset", "--"}, files...)
		_, resetErr := runGit(ctx, worktree, resetArgs...)
		return resetErr
	}
	applyErr := func() error {
		canApply, _, err := checkModelPatch(ctx, worktree, patchText, false, true)
		if err != nil {
			return err
		}
		if canApply {
			return applyIndexedModelPatch(ctx, worktree, patchText)
		}
		hunks, err := splitModelPatchHunks(patchText)
		if err != nil {
			return err
		}
		pending := make([]string, 0, len(hunks))
		for index, hunk := range hunks {
			alreadyApplied, _, checkErr := checkModelPatch(ctx, worktree, hunk, true, true)
			if checkErr != nil {
				return checkErr
			}
			if alreadyApplied {
				continue
			}
			applicable, detail, checkErr := checkModelPatch(ctx, worktree, hunk, false, true)
			if checkErr != nil {
				return checkErr
			}
			if !applicable {
				return fmt.Errorf("incremental model patch hunk %d is neither applicable nor already present: %s", index+1, safeExcerpt(detail))
			}
			pending = append(pending, hunk)
		}
		if len(pending) == 0 {
			return nil
		}
		return applyIndexedModelPatch(ctx, worktree, strings.Join(pending, ""))
	}()
	resetErr := resetIndex()
	if applyErr != nil {
		if resetErr != nil {
			return fmt.Errorf("%v; restore repair index after rejected incremental patch: %w", applyErr, resetErr)
		}
		return applyErr
	}
	if resetErr != nil {
		return fmt.Errorf("restore repair index after incremental patch: %w", resetErr)
	}
	return nil
}

func applyIndexedModelPatch(ctx context.Context, worktree, patchText string) error {
	for _, check := range []bool{true, false} {
		args := []string{"apply", "--index"}
		if check {
			args = append(args, "--check")
		}
		args = append(args, "--whitespace=error-all", "--recount", "-")
		command := exec.CommandContext(ctx, "git", args...)
		command.Dir = worktree
		command.Env = append(providerEnvironment(), "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=credential.interactive", "GIT_CONFIG_VALUE_0=false")
		command.Stdin = strings.NewReader(patchText)
		var output limitedBuffer
		command.Stdout, command.Stderr = &output, &output
		if err := command.Run(); err != nil {
			return fmt.Errorf("git %s incremental model patch failed: %w: %s", strings.Join(args[:len(args)-1], " "), err, safeExcerpt(output.String()))
		}
	}
	return nil
}

func checkModelPatch(ctx context.Context, worktree, patchText string, reverse, indexed bool) (bool, string, error) {
	args := []string{"apply"}
	if indexed {
		args = append(args, "--index")
	}
	if reverse {
		args = append(args, "--reverse")
	}
	args = append(args, "--check", "--whitespace=error-all", "--recount", "-")
	command := exec.CommandContext(ctx, "git", args...)
	command.Dir = worktree
	command.Env = append(providerEnvironment(), "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=credential.interactive", "GIT_CONFIG_VALUE_0=false")
	command.Stdin = strings.NewReader(patchText)
	var output limitedBuffer
	command.Stdout, command.Stderr = &output, &output
	if err := command.Run(); err != nil {
		if ctx.Err() != nil {
			return false, output.String(), ctx.Err()
		}
		return false, output.String(), nil
	}
	return true, output.String(), nil
}

func splitModelPatchHunks(patchText string) ([]string, error) {
	if strings.Contains(patchText, "--- /dev/null") || strings.Contains(patchText, "+++ /dev/null") {
		return nil, fmt.Errorf("incremental revision patches cannot partially apply file creation or deletion")
	}
	var header, current strings.Builder
	hunks := []string{}
	flush := func() {
		if current.Len() > 0 {
			hunks = append(hunks, current.String())
			current.Reset()
		}
	}
	for _, line := range strings.SplitAfter(strings.ReplaceAll(patchText, "\r\n", "\n"), "\n") {
		switch {
		case strings.HasPrefix(line, "diff --git "):
			flush()
			header.Reset()
			header.WriteString(line)
		case strings.HasPrefix(line, "@@ "):
			flush()
			if header.Len() == 0 {
				return nil, fmt.Errorf("incremental model patch hunk is missing a file header")
			}
			current.WriteString(header.String())
			current.WriteString(line)
		case current.Len() > 0:
			current.WriteString(line)
		default:
			header.WriteString(line)
		}
	}
	flush()
	if len(hunks) == 0 {
		return nil, fmt.Errorf("incremental model patch contains no unified diff hunks")
	}
	return hunks, nil
}

func equalStringSets(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	leftCopy, rightCopy := append([]string(nil), left...), append([]string(nil), right...)
	sort.Strings(leftCopy)
	sort.Strings(rightCopy)
	for index := range leftCopy {
		if leftCopy[index] != rightCopy[index] {
			return false
		}
	}
	return true
}
