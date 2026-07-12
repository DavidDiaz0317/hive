package repair

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
)

type ProviderResult struct {
	Summary string
	Output  string
}

type Provider interface {
	Name() string
	Health(ctx context.Context) error
	Run(ctx context.Context, worktree, prompt string) (ProviderResult, error)
}

// ProviderRunError records whether the provider process was successfully
// launched. Once launched, a timeout or non-zero exit is an ambiguous model
// invocation and must consume exactly one bounded model ordinal. A launch
// failure is infrastructure and can resume the same uncounted ordinal.
type ProviderRunError struct {
	Launched bool
	Cause    error
}

func (e *ProviderRunError) Error() string { return e.Cause.Error() }
func (e *ProviderRunError) Unwrap() error { return e.Cause }

func providerRunWasLaunched(err error) bool {
	var runErr *ProviderRunError
	if errors.As(err, &runErr) {
		return runErr.Launched
	}
	// Third-party Provider implementations cannot prove a failure happened
	// before launch. Fail closed by charging an invocation that may have run.
	return true
}

// CodexProvider invokes the stable non-interactive Codex CLI surface. Prefix
// can pin a packaged launcher, for example: ["--yes",
// "@openai/codex@0.144.1"], while Command is "npx".
type CodexProvider struct {
	Command string
	Prefix  []string
}

func (p CodexProvider) Name() string { return "codex" }

func (p CodexProvider) Health(ctx context.Context) error {
	if strings.TrimSpace(p.Command) == "" {
		return fmt.Errorf("Codex provider command is required")
	}
	command := exec.CommandContext(ctx, p.Command, append(append([]string(nil), p.Prefix...), "login", "status")...)
	command.Env = providerEnvironment()
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	if err := command.Run(); err != nil {
		return fmt.Errorf("Codex authentication health check failed: %w: %s", err, safeExcerpt(output.String()))
	}
	if !strings.Contains(strings.ToLower(output.String()), "logged in") {
		return fmt.Errorf("Codex authentication health check did not report a logged-in session")
	}
	return nil
}

func (p CodexProvider) Run(ctx context.Context, worktree, prompt string) (ProviderResult, error) {
	args := append([]string(nil), p.Prefix...)
	args = append(args,
		"--ask-for-approval", "never",
		"exec", "--cd", worktree,
		"--sandbox", "read-only",
		"--ephemeral", "--ignore-user-config", "--color", "never", "-",
	)
	command := exec.CommandContext(ctx, p.Command, args...)
	command.Dir = worktree
	command.Env = providerEnvironment()
	command.Stdin = strings.NewReader(prompt)
	var stdout providerOutputBuffer
	var stderr limitedBuffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		return ProviderResult{}, &ProviderRunError{Launched: false, Cause: fmt.Errorf("start Codex repair run: %w", err)}
	}
	if err := command.Wait(); err != nil {
		output := safeProviderOutput(stdout.String())
		return ProviderResult{Summary: safeExcerpt(output), Output: output}, &ProviderRunError{
			Launched: true,
			Cause:    fmt.Errorf("Codex repair run failed: %w: %s", err, safeExcerpt(stderr.String())),
		}
	}
	output := safeProviderOutput(stdout.String())
	return ProviderResult{Summary: safeExcerpt(output), Output: output}, nil
}

func providerEnvironment() []string {
	result := make([]string, 0, len(os.Environ()))
	for _, pair := range os.Environ() {
		name, _, _ := strings.Cut(pair, "=")
		if blockedEnvironmentName.MatchString(name) || strings.EqualFold(name, "GH_TOKEN") || strings.EqualFold(name, "GITHUB_TOKEN") {
			continue
		}
		result = append(result, pair)
	}
	result = append(result, "GIT_TERMINAL_PROMPT=0")
	return result
}

var blockedEnvironmentName = regexp.MustCompile(`(?i)(^|_)(TOKEN|SECRET|PASSWORD|PRIVATE_KEY|API_KEY)($|_)`)

type limitedBuffer struct {
	mu    sync.Mutex
	head  bytes.Buffer
	tail  []byte
	total int
}

func (b *limitedBuffer) Write(value []byte) (int, error) {
	const limit = 16 << 10
	const headLimit = limit / 2
	const tailLimit = limit - headLimit
	original := len(value)
	b.mu.Lock()
	defer b.mu.Unlock()
	b.total += original
	if b.head.Len() < headLimit {
		remaining := headLimit - b.head.Len()
		written := min(len(value), remaining)
		_, _ = b.head.Write(value[:written])
		value = value[written:]
	}
	if len(value) > 0 {
		if len(value) > tailLimit {
			value = value[len(value)-tailLimit:]
		}
		b.tail = append(b.tail, value...)
		if len(b.tail) > tailLimit {
			b.tail = append([]byte(nil), b.tail[len(b.tail)-tailLimit:]...)
		}
	}
	return original, nil
}

func (b *limitedBuffer) String() string {
	const limit = 16 << 10
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.total <= limit {
		return b.head.String() + string(b.tail)
	}
	return b.head.String() + "\n...[bounded output omitted]...\n" + string(b.tail)
}

type providerOutputBuffer struct{ bytes.Buffer }

func (b *providerOutputBuffer) Write(value []byte) (int, error) {
	const limit = 256 << 10
	original := len(value)
	if b.Len() < limit {
		remaining := limit - b.Len()
		if len(value) > remaining {
			value = value[:remaining]
		}
		_, _ = b.Buffer.Write(value)
	}
	return original, nil
}

var providerSecret = regexp.MustCompile(`(?i)(github_pat_[A-Za-z0-9_]{10,}|gh[pousr]_[A-Za-z0-9]{10,}|sk-[A-Za-z0-9_-]{10,})`)

func safeExcerpt(value string) string {
	value = safeProviderOutput(value)
	value = strings.TrimSpace(value)
	const limit = 8 << 10
	if len(value) > limit {
		half := limit / 2
		value = value[:half] + "\n...[bounded excerpt]...\n" + value[len(value)-half:]
	}
	return value
}

func safeProviderOutput(value string) string {
	return strings.TrimSpace(providerSecret.ReplaceAllString(value, "[REDACTED]"))
}
