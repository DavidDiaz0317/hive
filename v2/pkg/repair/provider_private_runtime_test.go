package repair

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestCodexProviderPrivateRuntimeDropsDynamicAmbientAuthority(t *testing.T) {
	t.Setenv("GO_WANT_CODEX_PROVIDER_HELPER", "1")
	t.Setenv("HIVE_TEST_EXPECT_CLEAN_CODEX_ENV", "1")
	for name, value := range map[string]string{
		"GH_TOKEN": "gh-canary", "GITHUB_TOKEN": "github-canary", "GITHUB_APP_PRIVATE_KEY": "app-key-canary",
		"SSH_AUTH_SOCK": "ssh-canary", "DYNAMIC_PROVIDER_TOKEN": "dynamic-canary", "HTTPS_PROXY": "proxy-canary",
		"HTTP_PROXY": "proxy-canary", "ALL_PROXY": "proxy-canary", "AWS_SHARED_CREDENTIALS_FILE": "credential-path-canary",
		"GIT_CONFIG_GLOBAL": "git-config-canary", "HOME": "ambient-home-canary", "USERPROFILE": "ambient-home-canary",
		"CODEX_HOME": "ambient-codex-home-canary", "APPDATA": "ambient-appdata-canary", "LOCALAPPDATA": "ambient-localappdata-canary",
		"SSL_CERT_FILE": "ambient-cert-file-canary", "SSL_CERT_DIR": "ambient-cert-dir-canary", "COMSPEC": "ambient-comspec-canary",
		"PATH": "ambient-path-canary",
	} {
		t.Setenv(name, value)
	}
	provider := CodexProvider{Command: os.Args[0], CodexHome: t.TempDir()}
	if err := provider.Health(context.Background()); err != nil {
		t.Fatalf("clean private Health: %v", err)
	}
	result, err := provider.Run(context.Background(), "ignored-worktree", "Return the denial sentinel.")
	if err != nil || result.Output != "DENIED" {
		t.Fatalf("clean private Run: result=%+v err=%v", result, err)
	}
}

func TestCodexProviderPrivateRuntimeUsesPrivateWindowsProfileDirectories(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows private profile environment assertion")
	}
	t.Setenv("APPDATA", "ambient-appdata-canary")
	t.Setenv("LOCALAPPDATA", "ambient-localappdata-canary")
	state, err := prepareCodexPrivateRuntime(&codexProviderIdentityAttestation{}, os.Args[0], "")
	if err != nil {
		t.Fatal(err)
	}
	defer state.cleanup()
	environment := make(map[string]string)
	for _, pair := range state.environment {
		name, value, _ := strings.Cut(pair, "=")
		environment[strings.ToUpper(name)] = value
	}
	for _, name := range []string{"APPDATA", "LOCALAPPDATA"} {
		value := filepath.Clean(environment[name])
		if value == "" || value == filepath.Clean("ambient-"+strings.ToLower(name)+"-canary") || filepath.Dir(value) != filepath.Clean(state.root) {
			t.Fatalf("%s was not replaced with a private invocation directory: %q", name, value)
		}
		info, statErr := os.Lstat(value)
		if statErr != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			t.Fatalf("%s private directory is unsafe: info=%v err=%v", name, info, statErr)
		}
		if reparse, reparseErr := codexProviderPathIsReparsePoint(value); reparseErr != nil || reparse {
			t.Fatalf("%s private directory is linked: reparse=%t err=%v", name, reparse, reparseErr)
		}
	}
}

func TestCodexLinuxContainmentHelperPathIsSealedAndSourceDriftFailsClosed(t *testing.T) {
	source := filepath.Join(t.TempDir(), "bwrap-source")
	if err := os.WriteFile(source, []byte("ordinary-bwrap-test-bytes"), 0o700); err != nil {
		t.Fatal(err)
	}
	identity, err := inspectCodexProviderFile(source)
	if err != nil {
		t.Fatal(err)
	}
	sealed, root, err := sealCodexContainmentHelper(identity)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanupCodexContainmentHelperSeal(root, sealed.Path)
	commandIdentity, err := inspectCodexProviderFile(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	attestation := &codexProviderIdentityAttestation{
		Command: commandIdentity, ContainmentHelper: identity, SealedContainmentHelper: sealed, ContainmentHelperRoot: root,
	}
	environment, err := bindCodexRuntimePath("linux", []string{"PATH=ambient-untrusted", "LANG=C"}, attestation, "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(environment, "\n") != "LANG=C\nPATH="+root {
		t.Fatalf("Linux helper PATH retained ambient entries: %q", environment)
	}
	if err := os.WriteFile(source, []byte("changed-bwrap-test-bytes!"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := attestation.revalidateSource(context.Background()); err == nil || !strings.Contains(err.Error(), "containment helper changed") {
		t.Fatalf("helper drift was not rejected before launch: %v", err)
	}
}

func TestCodexProviderOutputOverflowCancelsContainedProcess(t *testing.T) {
	t.Setenv("GO_WANT_CODEX_PROVIDER_HELPER", "1")
	provider := CodexProvider{Command: os.Args[0]}
	if err := provider.Health(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HIVE_TEST_CODEX_OUTPUT_OVERFLOW", "1")
	result, err := provider.Run(context.Background(), "", "Overflow the bounded transport.")
	if err == nil || !providerRunWasLaunched(err) || result.Output != "" || !strings.Contains(err.Error(), "hard cap") {
		t.Fatalf("overflow did not cancel fail-closed: result=%+v err=%v", result, err)
	}
}

func TestCodexProviderRejectsAndCleansUnexpectedNeutralCWDFile(t *testing.T) {
	t.Setenv("GO_WANT_CODEX_PROVIDER_HELPER", "1")
	provider := CodexProvider{Command: os.Args[0]}
	if err := provider.Health(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HIVE_TEST_CODEX_WRITE_EXTRA_FILE", "1")
	result, err := provider.Run(context.Background(), "", "Attempt an unexpected file.")
	if err == nil || !providerRunWasLaunched(err) || result.Output != "" || !strings.Contains(err.Error(), "unexpected neutral-cwd files") {
		t.Fatalf("unexpected neutral cwd file was not rejected: result=%+v err=%v", result, err)
	}
}

func TestCodexProviderTimeoutReapsContainedDescendant(t *testing.T) {
	t.Setenv("GO_WANT_CODEX_PROVIDER_HELPER", "1")
	provider := CodexProvider{Command: os.Args[0]}
	if err := provider.Health(context.Background()); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "descendant-alive.log")
	t.Setenv("HIVE_TEST_CODEX_DESCENDANT_MARKER", marker)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	result, err := provider.Run(ctx, "", "Wait until the controller deadline.")
	if err == nil || !providerRunWasLaunched(err) || result.Output != "" || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout did not remain a launched contained failure: result=%+v err=%v", result, err)
	}
	first, statErr := os.Stat(marker)
	if statErr != nil || first.Size() == 0 {
		t.Fatalf("descendant probe never ran: info=%v err=%v", first, statErr)
	}
	time.Sleep(250 * time.Millisecond)
	second, statErr := os.Stat(marker)
	if statErr != nil || second.Size() != first.Size() {
		t.Fatalf("descendant survived process-tree cleanup: before=%v after=%v err=%v", first.Size(), second, statErr)
	}
}

func TestCodexProviderReapsDescendantRetainingInheritedOutputHandles(t *testing.T) {
	t.Setenv("GO_WANT_CODEX_PROVIDER_HELPER", "1")
	provider := CodexProvider{Command: os.Args[0]}
	if err := provider.Health(context.Background()); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "inherited-output-descendant.log")
	t.Setenv("HIVE_TEST_CODEX_DESCENDANT_MARKER", marker)
	t.Setenv("HIVE_TEST_CODEX_INHERITED_OUTPUT_DESCENDANT", "1")
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	started := time.Now()
	result, err := provider.Run(ctx, "", "Return after spawning the inherited-handle adversary.")
	if err != nil || result.Output != "DENIED" {
		t.Fatalf("inherited-handle descendant blocked or corrupted output: result=%+v err=%v", result, err)
	}
	if elapsed := time.Since(started); elapsed > 8*time.Second {
		t.Fatalf("inherited output handles delayed bounded cleanup for %s", elapsed)
	}
	first, statErr := os.Stat(marker)
	if statErr != nil || first.Size() == 0 {
		t.Fatalf("inherited-handle descendant never ran: info=%v err=%v", first, statErr)
	}
	time.Sleep(250 * time.Millisecond)
	second, statErr := os.Stat(marker)
	if statErr != nil || second.Size() != first.Size() {
		t.Fatalf("inherited-handle descendant survived job/group cleanup: before=%v after=%v err=%v", first.Size(), second, statErr)
	}
}

func TestCodexProviderRejectsLinkedAuthenticationSourceBeforeInvocation(t *testing.T) {
	t.Setenv("GO_WANT_CODEX_PROVIDER_HELPER", "1")
	target := filepath.Join(t.TempDir(), "real-auth.json")
	if err := os.WriteFile(target, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	if err := os.Symlink(target, filepath.Join(home, "auth.json")); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}
	provider := CodexProvider{Command: os.Args[0], CodexHome: home}
	if err := provider.Health(context.Background()); err == nil || !strings.Contains(strings.ToLower(err.Error()), "link") {
		t.Fatalf("linked authentication source was accepted: %v", err)
	}
	result, err := provider.Run(context.Background(), "", "must not launch")
	if err == nil || providerRunWasLaunched(err) || result.Output != "" {
		t.Fatalf("failed linked-auth Health left runnable authorization: result=%+v err=%v", result, err)
	}
}
