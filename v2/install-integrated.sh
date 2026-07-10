#!/usr/bin/env sh
set -eu

version="${HIVE_VERSION:-latest}"
repository="${HIVE_REPOSITORY:-DavidDiaz0317/hive}"
install_dir="${HIVE_INSTALL_DIR:-$HOME/.local/share/hive}"
release_dir="${HIVE_RELEASE_DIR:-}"

while [ "$#" -gt 0 ]; do
  case "$1" in
    --version) version="$2"; shift 2 ;;
    --repo) repository="$2"; shift 2 ;;
    --install-dir) install_dir="$2"; shift 2 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

[ "$(uname -s)" = "Linux" ] || { echo "This installer supports Linux; use install-integrated.ps1 on Windows." >&2; exit 1; }
[ "$(uname -m)" = "x86_64" ] || { echo "The integrated Hive installer currently supports Linux x64 only." >&2; exit 1; }
[ -n "$release_dir" ] || command -v gh >/dev/null 2>&1 || { echo "GitHub CLI is required for signed release verification and authorization: https://cli.github.com/" >&2; exit 1; }
command -v sha256sum >/dev/null 2>&1 || { echo "sha256sum is required." >&2; exit 1; }

if [ "$version" = "latest" ]; then
  [ -z "$release_dir" ] || { echo "Set a concrete HIVE_VERSION with HIVE_RELEASE_DIR." >&2; exit 1; }
  version="$(gh api "repos/$repository/releases/latest" --jq .tag_name)"
fi
[ -n "$version" ] || { echo "No published integrated Hive release was found." >&2; exit 1; }

asset="hive-integrated-$version-linux-amd64.tar.gz"
tmp="$(mktemp -d "${TMPDIR:-/tmp}/hive-install.XXXXXX")"
cleanup() {
  case "$tmp" in "${TMPDIR:-/tmp}"/hive-install.*) rm -rf -- "$tmp" ;; esac
}
trap cleanup EXIT INT TERM

if [ -n "$release_dir" ]; then
  cp -- "$release_dir/$asset" "$release_dir/$asset.sha256" "$tmp/"
else
  gh release download "$version" --repo "$repository" --pattern "$asset" --pattern "$asset.sha256" --dir "$tmp"
fi
if [ "${HIVE_SKIP_ATTESTATION:-0}" = "1" ]; then
  [ -n "$release_dir" ] || { echo "HIVE_SKIP_ATTESTATION is allowed only with HIVE_RELEASE_DIR." >&2; exit 1; }
else
  gh attestation verify "$tmp/$asset" --repo "$repository" >/dev/null
fi
(cd "$tmp" && sha256sum --check "$asset.sha256")
mkdir "$tmp/extract"
tar -xzf "$tmp/$asset" -C "$tmp/extract"
source_dir="$(find "$tmp/extract" -mindepth 1 -maxdepth 1 -type d | head -n 1)"
[ -n "$source_dir" ] || { echo "Release archive has no distribution directory." >&2; exit 1; }

"$source_dir/runtime/node" - "$source_dir" <<'NODE'
const crypto = require('node:crypto');
const fs = require('node:fs');
const path = require('node:path');
const root = fs.realpathSync(process.argv[2]);
const manifest = JSON.parse(fs.readFileSync(path.join(root, 'distribution-manifest.json'), 'utf8'));
if (manifest.schema_version !== 'hive.integrated-distribution.v1' || manifest.os !== 'linux' || manifest.architecture !== 'amd64') throw new Error('distribution platform mismatch');
for (const file of manifest.files) {
  if (!file.path || path.posix.isAbsolute(file.path) || file.path.includes('\\') || file.path.split('/').includes('..')) throw new Error(`unsafe inventory path: ${file.path}`);
  const target = fs.realpathSync(path.join(root, ...file.path.split('/')));
  if (!target.startsWith(root + path.sep)) throw new Error(`inventory path escaped root: ${file.path}`);
  const data = fs.readFileSync(target);
  if (data.length !== file.size || crypto.createHash('sha256').update(data).digest('hex') !== file.sha256) throw new Error(`inventory mismatch: ${file.path}`);
}
for (const required of ['hive', 'runtime/node', 'visual-hive/visual-hive.mjs', 'visual-hive/release-manifest.json', 'skills/hive/SKILL.md', 'skills/hive/agents/openai.yaml']) {
  if (!fs.statSync(path.join(root, ...required.split('/'))).isFile()) throw new Error(`missing ${required}`);
}
NODE

parent_dir="$(dirname "$install_dir")"
mkdir -p "$parent_dir" "$HOME/.local/bin"
staging_dir="$install_dir.new.$$"
backup_dir="$install_dir.previous"
case "$staging_dir" in "$parent_dir"/*) ;; *) echo "Unsafe staging path." >&2; exit 1 ;; esac
rm -rf -- "$staging_dir"
cp -R "$source_dir" "$staging_dir"
[ ! -e "$backup_dir" ] || rm -rf -- "$backup_dir"
[ ! -e "$install_dir" ] || mv -- "$install_dir" "$backup_dir"
mv -- "$staging_dir" "$install_dir"
ln -sfn "$install_dir/hive" "$HOME/.local/bin/hive"
[ ! -e "$backup_dir" ] || rm -rf -- "$backup_dir"

codex_home="${CODEX_HOME:-$HOME/.codex}"
mkdir -p "$codex_home/skills"
skill_target="$codex_home/skills/hive"
case "$skill_target" in "$codex_home"/skills/*) rm -rf -- "$skill_target" ;; *) echo "Unsafe Codex skill path." >&2; exit 1 ;; esac
cp -R "$install_dir/skills/hive" "$skill_target"

echo "Hive $version installed at $install_dir"
echo "Ensure $HOME/.local/bin is on PATH, run 'gh auth login', then 'hive setup --repo owner/repository'."
