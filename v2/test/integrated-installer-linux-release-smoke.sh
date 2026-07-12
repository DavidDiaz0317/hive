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
download_dir_marker="$work_root/release-download-dir"
release_commit=0123456789abcdef0123456789abcdef01234567
published_version="$version"
if ! printf '%s\n' "$published_version" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+-integrated\.[0-9]+$'; then
  # A workflow_dispatch rehearsal correctly names its assembled archive with
  # the commit SHA. Exercise the published-only trust path through a valid
  # synthetic tag name while retaining the exact assembled archive bytes.
  published_version=v0.0.0-integrated.0
fi
published_release_dir="$work_root/published-release"
published_asset="hive-integrated-$published_version-linux-amd64.tar.gz"
mkdir -p "$published_release_dir"
cp -- "$release_dir/hive-integrated-$version-linux-amd64.tar.gz" "$published_release_dir/$published_asset"
(cd "$published_release_dir" && sha256sum "$published_asset" > "$published_asset.sha256")
mkdir -p "$attestation_bin"
cat > "$attestation_bin/gh" <<'SH'
#!/usr/bin/env sh
set -eu
command_name="${1:-}"
shift || true
case "$command_name" in
  auth)
    [ "$#" -eq 1 ]
    [ "${1:-}" = status ]
    ;;
  api)
    [ "$#" -eq 3 ]
    [ "$1" = "repos/$HIVE_FAKE_REPOSITORY/commits/$HIVE_FAKE_VERSION" ]
    [ "$2" = --jq ]
    [ "$3" = .sha ]
    printf '%s\n' "$HIVE_FAKE_COMMIT"
    ;;
  release)
    [ "$#" -eq 10 ]
    [ "$1" = download ]
    [ "$2" = "$HIVE_FAKE_VERSION" ]
    [ "$3" = --repo ]
    [ "$4" = "$HIVE_FAKE_REPOSITORY" ]
    [ "$5" = --pattern ]
    [ "$6" = "$HIVE_FAKE_ASSET" ]
    [ "$7" = --pattern ]
    [ "$8" = "$HIVE_FAKE_ASSET.sha256" ]
    [ "$9" = --dir ]
    destination="${10}"
    [ -d "$destination" ]
    printf '%s\n' "$destination" > "$HIVE_FAKE_DOWNLOAD_DIR_MARKER"
    cp -- "$HIVE_FAKE_RELEASE_DIR/$HIVE_FAKE_ASSET" "$HIVE_FAKE_RELEASE_DIR/$HIVE_FAKE_ASSET.sha256" "$destination/"
    ;;
  attestation)
    [ "$#" -eq 13 ]
    [ "$1" = verify ]
    download_dir="$(cat "$HIVE_FAKE_DOWNLOAD_DIR_MARKER")"
    [ "$2" = "$download_dir/$HIVE_FAKE_ASSET" ]
    [ "$3" = --repo ]
    [ "$4" = "$HIVE_FAKE_REPOSITORY" ]
    [ "$5" = --signer-workflow ]
    [ "$6" = "$HIVE_FAKE_REPOSITORY/.github/workflows/integrated-release.yml" ]
    [ "$7" = --source-ref ]
    [ "$8" = "refs/tags/$HIVE_FAKE_VERSION" ]
    [ "$9" = --source-digest ]
    [ "${10}" = "$HIVE_FAKE_COMMIT" ]
    [ "${11}" = --signer-digest ]
    [ "${12}" = "$HIVE_FAKE_COMMIT" ]
    [ "${13}" = --deny-self-hosted-runners ]
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
  export HIVE_FAKE_VERSION="$published_version"
  export HIVE_FAKE_COMMIT="$release_commit"
  export HIVE_FAKE_RELEASE_DIR="$published_release_dir"
  export HIVE_FAKE_ASSET="$published_asset"
  export HIVE_FAKE_REPOSITORY="$repository"
  export HIVE_FAKE_ATTESTATION_MARKER="$attestation_marker"
  export HIVE_FAKE_DOWNLOAD_DIR_MARKER="$download_dir_marker"
  sh "$installer" --version "$published_version" --repo "$repository"
)
test -f "$attestation_marker"
"$work_root/attested-install/runtime/node" "$work_root/attested-install/visual-hive/visual-hive.mjs" --version
echo "Signed Linux integrated installer smoke passed: $work_root"
