package repair

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

const maxCodexProviderExecutableBytes = 512 << 20

type codexProviderFileIdentity struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

type codexProviderIdentityAttestation struct {
	ConfigKey       string
	IdentitySHA256  string
	Command         codexProviderFileIdentity
	Prefix          []string
	PrefixArtifacts []codexProviderFileIdentity
	SealedCommand   codexProviderFileIdentity
	SealRoot        string
}

var codexProviderAttestations = struct {
	sync.RWMutex
	values map[string]*codexProviderIdentityAttestation
}{values: make(map[string]*codexProviderIdentityAttestation)}

func codexProviderConfigurationKey(provider CodexProvider) (string, error) {
	encoded, err := json.Marshal(struct {
		Command string   `json:"command"`
		Prefix  []string `json:"prefix"`
	}{Command: provider.Command, Prefix: append([]string(nil), provider.Prefix...)})
	if err != nil {
		return "", fmt.Errorf("encode Codex provider configuration: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func forgetCodexProviderAttestation(provider CodexProvider) {
	key, err := codexProviderConfigurationKey(provider)
	if err != nil {
		return
	}
	codexProviderAttestations.Lock()
	delete(codexProviderAttestations.values, key)
	codexProviderAttestations.Unlock()
}

func rememberCodexProviderAttestation(provider CodexProvider, attestation *codexProviderIdentityAttestation) {
	codexProviderAttestations.Lock()
	codexProviderAttestations.values[attestation.ConfigKey] = attestation
	codexProviderAttestations.Unlock()
}

func consumeCodexProviderAttestation(provider CodexProvider) (*codexProviderIdentityAttestation, error) {
	key, err := codexProviderConfigurationKey(provider)
	if err != nil {
		return nil, err
	}
	codexProviderAttestations.Lock()
	attestation := codexProviderAttestations.values[key]
	delete(codexProviderAttestations.values, key)
	codexProviderAttestations.Unlock()
	if attestation == nil {
		return nil, fmt.Errorf("Codex provider has no unconsumed successful Health identity attestation; run Health immediately before each model invocation")
	}
	return attestation, nil
}

func prepareCodexProviderAttestation(provider CodexProvider) (*codexProviderIdentityAttestation, error) {
	key, err := codexProviderConfigurationKey(provider)
	if err != nil {
		return nil, err
	}
	command := strings.TrimSpace(provider.Command)
	if command == "" || strings.ContainsRune(command, '\x00') {
		return nil, fmt.Errorf("Codex provider command is required")
	}
	resolved, err := exec.LookPath(command)
	if err != nil {
		return nil, fmt.Errorf("resolve Codex provider executable: %w", err)
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return nil, fmt.Errorf("resolve absolute Codex provider executable: %w", err)
	}
	commandIdentity, err := inspectCodexProviderFile(resolved)
	if err != nil {
		return nil, fmt.Errorf("attest Codex provider executable: %w", err)
	}
	if !codexProviderExecutableCanBeSealed(commandIdentity.Path) {
		return nil, fmt.Errorf("Codex provider must be a reviewed native executable that Hive can seal; script and package-runner wrappers (.cmd, .ps1, shebang, npx, npm, and similar launchers) are unsupported; point --provider-command at the native Codex binary")
	}
	if err := validateCodexProviderPrefix(provider.Prefix); err != nil {
		return nil, err
	}

	prefixArtifacts := make([]codexProviderFileIdentity, 0)
	seenArtifacts := make(map[string]bool)
	for _, argument := range provider.Prefix {
		if strings.ContainsRune(argument, '\x00') {
			return nil, fmt.Errorf("Codex provider prefix contains a NUL byte")
		}
		artifactPath := codexProviderPrefixArtifactPath(argument)
		if artifactPath == "" {
			continue
		}
		absolute := filepath.Clean(artifactPath)
		if seenArtifacts[absolute] {
			continue
		}
		info, statErr := os.Lstat(absolute)
		if os.IsNotExist(statErr) {
			// An absolute literal may be a model argument rather than an input
			// artifact. The literal remains bound by ConfigKey.
			continue
		}
		if statErr != nil || info == nil {
			return nil, fmt.Errorf("inspect Codex provider prefix artifact: %w", statErr)
		}
		identity, identityErr := inspectCodexProviderFile(absolute)
		if identityErr != nil {
			return nil, fmt.Errorf("attest Codex provider prefix artifact: %w", identityErr)
		}
		seenArtifacts[absolute] = true
		prefixArtifacts = append(prefixArtifacts, identity)
	}

	identityBytes, err := json.Marshal(struct {
		Command         codexProviderFileIdentity   `json:"command"`
		Prefix          []string                    `json:"prefix"`
		PrefixArtifacts []codexProviderFileIdentity `json:"prefix_artifacts"`
	}{Command: commandIdentity, Prefix: append([]string(nil), provider.Prefix...), PrefixArtifacts: prefixArtifacts})
	if err != nil {
		return nil, fmt.Errorf("encode Codex provider identity: %w", err)
	}
	identityDigest := sha256.Sum256(identityBytes)
	identitySHA := hex.EncodeToString(identityDigest[:])
	sealedPath, sealRoot, sealErr := sealCodexProviderExecutable(commandIdentity, identitySHA)
	if sealErr != nil {
		return nil, sealErr
	}
	sealedCommand, err := inspectCodexProviderFile(sealedPath)
	if err != nil {
		_ = cleanupCodexProviderSeal(sealRoot, sealedPath)
		return nil, fmt.Errorf("verify sealed Codex provider executable: %w", err)
	}
	if sealedCommand.SHA256 != commandIdentity.SHA256 || sealedCommand.Bytes != commandIdentity.Bytes {
		_ = cleanupCodexProviderSeal(sealRoot, sealedPath)
		return nil, fmt.Errorf("sealed Codex provider executable does not match its attested source")
	}
	return &codexProviderIdentityAttestation{
		ConfigKey: key, IdentitySHA256: identitySHA, Command: commandIdentity,
		Prefix: append([]string(nil), provider.Prefix...), PrefixArtifacts: prefixArtifacts,
		SealedCommand: sealedCommand, SealRoot: sealRoot,
	}, nil
}

func validateCodexProviderPrefix(prefix []string) error {
	valueOptions := map[string]bool{
		"--enable": true, "--disable": true, "--model": true, "--color": true,
	}
	flagOptions := map[string]bool{"--no-alt-screen": true}
	expectValue := false
	for _, argument := range prefix {
		if strings.ContainsRune(argument, '\x00') {
			return fmt.Errorf("Codex provider prefix contains a NUL byte")
		}
		trimmed := strings.TrimSpace(argument)
		if trimmed == "" {
			return fmt.Errorf("Codex provider prefix contains an empty argument")
		}
		if trimmed == "--%" || strings.Trim(trimmed, "-") == "" {
			return fmt.Errorf("Codex provider prefix contains parser terminator %q; operator arguments may not terminate Hive's mandatory security options", safeExcerpt(trimmed))
		}
		if expectValue {
			if codexProviderPrefixLooksLikePayload(trimmed) {
				return fmt.Errorf("Codex provider prefix contains a script, package, or path payload; point --provider-command at the reviewed native Codex binary and pass only native Codex options")
			}
			expectValue = false
			continue
		}
		option, value, hasValue := strings.Cut(trimmed, "=")
		if !strings.HasPrefix(option, "-") {
			return fmt.Errorf("Codex provider prefix contains positional payload %q; script and package-runner wrappers are unsupported; point --provider-command at the reviewed native Codex binary", safeExcerpt(option))
		}
		if !valueOptions[option] && !flagOptions[option] {
			return fmt.Errorf("Codex provider prefix option %q is not in Hive's reviewed native-option allowlist; script, interpreter, and package payload options are unsupported; point --provider-command at the reviewed native Codex binary", safeExcerpt(option))
		}
		if hasValue && codexProviderPrefixLooksLikePayload(value) {
			return fmt.Errorf("Codex provider prefix contains a script, package, or path payload; point --provider-command at the reviewed native Codex binary and pass only native Codex options")
		}
		if !hasValue && valueOptions[option] {
			expectValue = true
		}
	}
	if expectValue {
		return fmt.Errorf("Codex provider prefix option is missing its value")
	}
	return nil
}

func codexProviderPrefixLooksLikePayload(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || strings.HasPrefix(value, "@") || filepath.IsAbs(value) {
		return true
	}
	lower := strings.ToLower(value)
	for _, suffix := range []string{".cmd", ".bat", ".ps1", ".sh", ".js", ".mjs", ".cjs", ".ts", ".py"} {
		if strings.HasSuffix(lower, suffix) {
			return true
		}
	}
	return false
}

func codexProviderPrefixArtifactPath(argument string) string {
	candidates := []string{argument}
	if _, value, found := strings.Cut(argument, "="); found {
		candidates = append(candidates, value)
	}
	if strings.HasPrefix(argument, "@") {
		candidates = append(candidates, strings.TrimPrefix(argument, "@"))
	}
	for _, candidate := range candidates {
		candidate = strings.TrimSpace(candidate)
		if filepath.IsAbs(candidate) {
			return candidate
		}
	}
	return ""
}

func (attestation *codexProviderIdentityAttestation) revalidate(ctx context.Context) error {
	if err := attestation.revalidateSource(ctx); err != nil {
		return err
	}
	sealed, err := inspectCodexProviderFile(attestation.SealedCommand.Path)
	if err != nil || sealed != attestation.SealedCommand || sealed.SHA256 != attestation.Command.SHA256 || sealed.Bytes != attestation.Command.Bytes {
		return fmt.Errorf("Hive-sealed Codex provider executable changed after Health")
	}
	return nil
}

func (attestation *codexProviderIdentityAttestation) revalidateSource(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	actualCommand, err := inspectCodexProviderFile(attestation.Command.Path)
	if err != nil || actualCommand != attestation.Command {
		return fmt.Errorf("Codex provider executable changed after Health")
	}
	for _, expected := range attestation.PrefixArtifacts {
		actual, inspectErr := inspectCodexProviderFile(expected.Path)
		if inspectErr != nil || actual != expected {
			return fmt.Errorf("Codex provider prefix artifact changed after Health")
		}
	}
	return nil
}

func (attestation *codexProviderIdentityAttestation) authorizationCopy() *codexProviderIdentityAttestation {
	copy := *attestation
	copy.Prefix = append([]string(nil), attestation.Prefix...)
	copy.PrefixArtifacts = append([]codexProviderFileIdentity(nil), attestation.PrefixArtifacts...)
	copy.SealedCommand = codexProviderFileIdentity{}
	copy.SealRoot = ""
	return &copy
}

func (attestation *codexProviderIdentityAttestation) sealForRun() (*codexProviderIdentityAttestation, error) {
	sealedPath, sealRoot, err := sealCodexProviderExecutable(attestation.Command, attestation.IdentitySHA256)
	if err != nil {
		return nil, err
	}
	sealedCommand, err := inspectCodexProviderFile(sealedPath)
	if err != nil || sealedCommand.SHA256 != attestation.Command.SHA256 || sealedCommand.Bytes != attestation.Command.Bytes {
		_ = cleanupCodexProviderSeal(sealRoot, sealedPath)
		return nil, fmt.Errorf("verify per-run sealed Codex provider executable")
	}
	copy := attestation.authorizationCopy()
	copy.SealedCommand = sealedCommand
	copy.SealRoot = sealRoot
	return copy, nil
}

func (attestation *codexProviderIdentityAttestation) cleanupSeal() error {
	if attestation == nil || attestation.SealRoot == "" || attestation.SealedCommand.Path == "" {
		return nil
	}
	return cleanupCodexProviderSeal(attestation.SealRoot, attestation.SealedCommand.Path)
}

func (attestation *codexProviderIdentityAttestation) provider() CodexProvider {
	return CodexProvider{Command: attestation.SealedCommand.Path, Prefix: append([]string(nil), attestation.Prefix...)}
}

func inspectCodexProviderFile(candidate string) (codexProviderFileIdentity, error) {
	absolute, err := filepath.Abs(candidate)
	if err != nil {
		return codexProviderFileIdentity{}, err
	}
	absolute = filepath.Clean(absolute)
	if err := rejectCodexProviderLinkedPath(absolute); err != nil {
		return codexProviderFileIdentity{}, err
	}
	before, err := os.Lstat(absolute)
	if err != nil || before == nil || !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 || before.Size() <= 0 || before.Size() > maxCodexProviderExecutableBytes {
		return codexProviderFileIdentity{}, fmt.Errorf("path is not a bounded ordinary file")
	}
	file, err := os.Open(absolute)
	if err != nil {
		return codexProviderFileIdentity{}, err
	}
	hasher := sha256.New()
	written, copyErr := io.Copy(hasher, io.LimitReader(file, maxCodexProviderExecutableBytes+1))
	opened, statErr := file.Stat()
	closeErr := file.Close()
	after, lstatErr := os.Lstat(absolute)
	if copyErr != nil || statErr != nil || closeErr != nil || lstatErr != nil {
		return codexProviderFileIdentity{}, fmt.Errorf("read stable ordinary file")
	}
	if written != before.Size() || written > maxCodexProviderExecutableBytes || !os.SameFile(before, opened) || !os.SameFile(before, after) || after.Mode()&os.ModeSymlink != 0 {
		return codexProviderFileIdentity{}, fmt.Errorf("ordinary file identity changed while hashing")
	}
	return codexProviderFileIdentity{Path: absolute, SHA256: hex.EncodeToString(hasher.Sum(nil)), Bytes: written}, nil
}

func rejectCodexProviderLinkedPath(absolute string) error {
	volume := filepath.VolumeName(absolute)
	remainder := strings.TrimPrefix(absolute, volume)
	remainder = strings.TrimLeft(remainder, string(filepath.Separator))
	cursor := volume + string(filepath.Separator)
	if volume == "" {
		cursor = string(filepath.Separator)
	}
	for _, component := range strings.Split(remainder, string(filepath.Separator)) {
		if component == "" {
			continue
		}
		cursor = filepath.Join(cursor, component)
		info, err := os.Lstat(cursor)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("path resolves through a symbolic link")
		}
		reparse, err := codexProviderPathIsReparsePoint(cursor)
		if err != nil {
			return err
		}
		if reparse {
			return fmt.Errorf("path resolves through a reparse point")
		}
	}
	return nil
}

func codexProviderExecutableCanBeSealed(path string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(filepath.Ext(path), ".exe")
	}
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer file.Close()
	header := make([]byte, 2)
	_, _ = io.ReadFull(file, header)
	return string(header) != "#!"
}

func sealCodexProviderExecutable(identity codexProviderFileIdentity, identitySHA string) (string, string, error) {
	sealRoot, err := os.MkdirTemp("", "hive-codex-provider-seals-")
	if err != nil {
		return "", "", fmt.Errorf("create Hive-owned Codex provider seal directory: %w", err)
	}
	if err := os.Chmod(sealRoot, 0o700); err != nil {
		_ = os.Remove(sealRoot)
		return "", "", fmt.Errorf("protect Hive-owned Codex provider seal directory: %w", err)
	}
	extension := filepath.Ext(identity.Path)
	destination := filepath.Join(sealRoot, identitySHA+extension)
	source, err := os.Open(identity.Path)
	if err != nil {
		_ = os.Remove(sealRoot)
		return "", "", fmt.Errorf("open attested Codex provider executable: %w", err)
	}
	defer source.Close()
	temporary, err := os.CreateTemp(sealRoot, identitySHA+"-*.tmp")
	if err != nil {
		_ = os.Remove(sealRoot)
		return "", "", fmt.Errorf("create Codex provider seal: %w", err)
	}
	if err := temporary.Chmod(0o500); err != nil {
		_ = temporary.Close()
		_ = os.Remove(temporary.Name())
		_ = os.Remove(sealRoot)
		return "", "", fmt.Errorf("protect temporary Codex provider seal: %w", err)
	}
	_, copyErr := io.Copy(temporary, io.LimitReader(source, maxCodexProviderExecutableBytes+1))
	syncErr := temporary.Sync()
	closeErr := temporary.Close()
	if copyErr != nil || syncErr != nil || closeErr != nil {
		_ = os.Remove(temporary.Name())
		_ = os.Remove(sealRoot)
		return "", "", fmt.Errorf("write Codex provider seal")
	}
	if err := os.Rename(temporary.Name(), destination); err != nil {
		_ = os.Remove(temporary.Name())
		_ = os.Remove(sealRoot)
		return "", "", fmt.Errorf("activate Codex provider seal: %w", err)
	}
	if err := os.Chmod(destination, 0o500); err != nil {
		_ = cleanupCodexProviderSeal(sealRoot, destination)
		return "", "", fmt.Errorf("protect Codex provider seal: %w", err)
	}
	return destination, sealRoot, nil
}

func cleanupCodexProviderSeal(root, executable string) error {
	root, executable = filepath.Clean(root), filepath.Clean(executable)
	if root == "" || executable == "" || filepath.Dir(executable) != root || !strings.HasPrefix(filepath.Base(root), "hive-codex-provider-seals-") {
		return fmt.Errorf("refuse unsafe Codex provider seal cleanup")
	}
	rootInfo, err := os.Lstat(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	rootReparse, reparseErr := codexProviderPathIsReparsePoint(root)
	if reparseErr != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 || rootReparse {
		return fmt.Errorf("refuse linked or replaced Codex provider seal cleanup")
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 || entries[0].Name() != filepath.Base(executable) || entries[0].Type()&os.ModeSymlink != 0 {
		return fmt.Errorf("refuse Codex provider seal cleanup with unexpected inventory")
	}
	if _, err := inspectCodexProviderFile(executable); err != nil {
		return fmt.Errorf("refuse Codex provider seal cleanup for non-ordinary executable")
	}
	if err := os.Remove(executable); err != nil {
		return err
	}
	return os.Remove(root)
}
