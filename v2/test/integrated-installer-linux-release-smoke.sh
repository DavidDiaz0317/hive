#!/usr/bin/env sh
set -eu

installer="${1:?Linux installer path is required}"
version="${2:?Integrated release version is required}"
work_root="${3:?Writable proof root is required}"
repository="${HIVE_REPOSITORY:-DavidDiaz0317/hive}"
release_dir="${HIVE_RELEASE_DIR:-}"

export HOME="$work_root/home"
export HIVE_VERSION="$version"
export HIVE_REPOSITORY="$repository"
export HIVE_INSTALL_DIR="$work_root/installed"
mkdir -p "$HOME"

sh "$installer"
"$HIVE_INSTALL_DIR/runtime/node" "$HIVE_INSTALL_DIR/visual-hive/visual-hive.mjs" --version
# Use the exact launcher printed by the installer. This proves the second
# command works in the same shell even when ~/.local/bin is not on PATH.
"$HOME/.local/bin/hive" setup \
  --repo DavidDiaz0317/visual-hive-demo-site \
  --coverage comprehensive \
  --automation advisory \
  --provider codex \
  --visual-hive \
  --plan \
  --state-dir "$work_root/state" \
  --json > "$work_root/setup-plan.json"
grep -q '"schema_version": "hive.setup-plan.v1"' "$work_root/setup-plan.json"
grep -q '"read_only": true' "$work_root/setup-plan.json"

# A failure after activation must restore the previous install and launcher.
printf '%s\n' previous-install > "$HIVE_INSTALL_DIR/transaction-marker"
blocked_codex="$work_root/blocked-codex"
mkdir -p "$blocked_codex"
printf '%s\n' blocks-directory > "$blocked_codex/skills"
if CODEX_HOME="$blocked_codex" sh "$installer" >"$work_root/post-activation-failure.log" 2>&1; then
  echo "post-activation Codex skill failure unexpectedly succeeded" >&2
  exit 1
fi
grep -q 'previous installation, launcher, and Codex skill were restored' "$work_root/post-activation-failure.log"
test -f "$HIVE_INSTALL_DIR/transaction-marker"
test "$(readlink "$HOME/.local/bin/hive")" = "$HIVE_INSTALL_DIR/hive"

# If moving a pre-existing skill to its backup fails, rollback must not delete
# that untouched skill. Wrap only mv so the failure is deterministic and leave
# all other install operations real.
real_mv="$(command -v mv)"
fake_bin="$work_root/fake-bin"
mkdir -p "$fake_bin" "$HOME/.codex/skills/hive"
printf '%s\n' original-skill > "$HOME/.codex/skills/hive/original-skill.txt"
cat > "$fake_bin/mv" <<'SH'
#!/usr/bin/env sh
set -eu
if [ "${1:-}" = "--" ]; then shift; fi
if [ "${1:-}" = "${HIVE_TEST_FAIL_MOVE_SOURCE:-}" ]; then
  echo "synthetic skill backup move failure" >&2
  exit 93
fi
exec "$HIVE_TEST_REAL_MV" -- "$@"
SH
chmod +x "$fake_bin/mv"
if PATH="$fake_bin:$PATH" \
   HIVE_TEST_REAL_MV="$real_mv" \
   HIVE_TEST_FAIL_MOVE_SOURCE="$HOME/.codex/skills/hive" \
   sh "$installer" >"$work_root/skill-backup-move-failure.log" 2>&1; then
  echo "skill backup move failure unexpectedly succeeded" >&2
  exit 1
fi
grep -q 'synthetic skill backup move failure' "$work_root/skill-backup-move-failure.log"
test -f "$HOME/.codex/skills/hive/original-skill.txt"
test -f "$HIVE_INSTALL_DIR/transaction-marker"
test "$(readlink "$HOME/.local/bin/hive")" = "$HIVE_INSTALL_DIR/hive"

# Exercise the published-release trust policy with a fake gh transport. The
# installer must bind provenance to the exact tag ref and tag/source commit,
# then re-resolve that commit after verification.
test -n "$release_dir"
attestation_bin="$work_root/attestation-bin"
attestation_marker="$work_root/attestation-verified"
release_commit=0123456789abcdef0123456789abcdef01234567
mkdir -p "$attestation_bin"
cat > "$attestation_bin/gh" <<'SH'
#!/usr/bin/env sh
set -eu
command_name="${1:-}"
shift || true
case "$command_name" in
  auth)
    [ "${1:-}" = status ]
    ;;
  api)
    case "${1:-}" in
      repos/*/releases/latest) printf '%s\n' "$HIVE_FAKE_VERSION" ;;
      repos/*/commits/*) printf '%s\n' "$HIVE_FAKE_COMMIT" ;;
      *) exit 61 ;;
    esac
    ;;
  release)
    [ "${1:-}" = download ]
    destination=""
    while [ "$#" -gt 0 ]; do
      if [ "$1" = --dir ]; then destination="$2"; shift 2; else shift; fi
    done
    [ -n "$destination" ]
    cp -- "$HIVE_FAKE_RELEASE_DIR/$HIVE_FAKE_ASSET" "$HIVE_FAKE_RELEASE_DIR/$HIVE_FAKE_ASSET.sha256" "$destination/"
    ;;
  attestation)
    [ "${1:-}" = verify ]
    shift
    source_ref="" source_digest="" signer_digest="" signer_workflow="" denied=0
    while [ "$#" -gt 0 ]; do
      case "$1" in
        --source-ref) source_ref="$2"; shift 2 ;;
        --source-digest) source_digest="$2"; shift 2 ;;
        --signer-digest) signer_digest="$2"; shift 2 ;;
        --signer-workflow) signer_workflow="$2"; shift 2 ;;
        --deny-self-hosted-runners) denied=1; shift ;;
        *) shift ;;
      esac
    done
    [ "$source_ref" = "refs/tags/$HIVE_FAKE_VERSION" ]
    [ "$source_digest" = "$HIVE_FAKE_COMMIT" ]
    [ "$signer_digest" = "$HIVE_FAKE_COMMIT" ]
    [ "$signer_workflow" = "$HIVE_FAKE_REPOSITORY/.github/workflows/integrated-release.yml" ]
    [ "$denied" = 1 ]
    : > "$HIVE_FAKE_ATTESTATION_MARKER"
    ;;
  *) exit 62 ;;
esac
SH
chmod +x "$attestation_bin/gh"
(
  unset HIVE_RELEASE_DIR HIVE_SKIP_ATTESTATION
  export PATH="$attestation_bin:$PATH"
  export HIVE_INSTALL_DIR="$work_root/attested-install"
  export HIVE_FAKE_VERSION="$version"
  export HIVE_FAKE_COMMIT="$release_commit"
  export HIVE_FAKE_RELEASE_DIR="$release_dir"
  export HIVE_FAKE_ASSET="hive-integrated-$version-linux-amd64.tar.gz"
  export HIVE_FAKE_REPOSITORY="$repository"
  export HIVE_FAKE_ATTESTATION_MARKER="$attestation_marker"
  sh "$installer" --version "$version" --repo "$repository"
)
test -f "$attestation_marker"
"$work_root/attested-install/runtime/node" "$work_root/attested-install/visual-hive/visual-hive.mjs" --version
echo "Signed Linux integrated installer smoke passed: $work_root"
