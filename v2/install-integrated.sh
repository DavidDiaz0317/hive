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
case "$repository" in
  */*)
    owner=${repository%%/*}
    name=${repository#*/}
    case "$owner" in '') echo "Repository must be one owner/name pair." >&2; exit 1 ;; esac
    case "$name" in */*|'') echo "Repository must be one owner/name pair." >&2; exit 1 ;; esac
    ;;
  *) echo "Repository must be one owner/name pair." >&2; exit 1 ;;
esac
case "$owner$name" in *[!A-Za-z0-9_.-]*) echo "Repository contains unsafe characters." >&2; exit 1 ;; esac
if [ -z "$release_dir" ]; then
  gh auth status >/dev/null 2>&1 || {
    echo "GitHub CLI is not authenticated. Run 'gh auth login', then retry the installer." >&2
    exit 1
  }
fi

if [ "$version" = "latest" ]; then
  [ -z "$release_dir" ] || { echo "Set a concrete HIVE_VERSION with HIVE_RELEASE_DIR." >&2; exit 1; }
  version="$(gh api "repos/$repository/releases/latest" --jq .tag_name)"
fi
[ -n "$version" ] || { echo "No published integrated Hive release was found." >&2; exit 1; }
case "$version" in
  ''|[!A-Za-z0-9]*|*[!A-Za-z0-9._-]*) echo "Release version contains unsafe characters." >&2; exit 1 ;;
esac
[ "${#version}" -le 128 ] || { echo "Release version is too long." >&2; exit 1; }
if [ -z "$release_dir" ]; then
  printf '%s\n' "$version" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+-integrated\.[0-9]+$' || {
    echo "Published integrated release versions must match vMAJOR.MINOR.PATCH-integrated.NUMBER." >&2
    exit 1
  }
fi

resolve_release_commit() {
  resolved="$(gh api "repos/$repository/commits/$version" --jq .sha)"
  printf '%s\n' "$resolved" | grep -Eq '^[a-f0-9]{40}$' || {
    echo "Release tag $version did not resolve to an exact 40-character commit." >&2
    exit 1
  }
  printf '%s\n' "$resolved"
}

release_commit=""
if [ -z "$release_dir" ]; then
  release_commit="$(resolve_release_commit)"
fi

asset="hive-integrated-$version-linux-amd64.tar.gz"
tmp="$(mktemp -d "${TMPDIR:-/tmp}/hive-install.XXXXXX")"
activated=0
committed=0
had_previous=0
link_backup_complete=0
link_candidate_installed=0
skill_backup_complete=0
skill_copy_started=0
parent_dir="$(dirname "$install_dir")"
backup_dir="$install_dir.previous"
link_path="$HOME/.local/bin/hive"
link_backup="$link_path.previous.$$"
codex_home="${CODEX_HOME:-$HOME/.codex}"
skill_target="$codex_home/skills/hive"
skill_backup="$skill_target.previous.$$"
cleanup() {
  case "$tmp" in "${TMPDIR:-/tmp}"/hive-install.*) rm -rf -- "$tmp" ;; esac
}
rollback() {
  rollback_failed=0
  if [ "$skill_backup_complete" = "1" ] || [ "$skill_copy_started" = "1" ]; then
    case "$skill_target" in "$codex_home"/skills/*)
      rm -rf -- "$skill_target" || rollback_failed=1
      if [ "$skill_backup_complete" = "1" ] && [ -e "$skill_backup" ]; then mv -- "$skill_backup" "$skill_target" || rollback_failed=1; fi
      ;;
      *) rollback_failed=1 ;;
    esac
  fi
  if [ "$link_backup_complete" = "1" ] || [ "$link_candidate_installed" = "1" ]; then
    rm -f -- "$link_path" || rollback_failed=1
    if [ "$link_backup_complete" = "1" ] && { [ -e "$link_backup" ] || [ -L "$link_backup" ]; }; then mv -- "$link_backup" "$link_path" || rollback_failed=1; fi
  fi
  case "$install_dir" in "$parent_dir"/*)
    rm -rf -- "$install_dir" || rollback_failed=1
    if [ "$had_previous" = "1" ] && [ -e "$backup_dir" ]; then mv -- "$backup_dir" "$install_dir" || rollback_failed=1; fi
    ;;
    *) rollback_failed=1 ;;
  esac
  [ "$rollback_failed" = "0" ]
}
finish() {
  status=$?
  trap - EXIT INT TERM
  if [ "$status" -ne 0 ] && [ "$activated" = "1" ] && [ "$committed" = "0" ]; then
    if rollback; then
      echo "Hive installation failed; the previous installation, launcher, and Codex skill were restored." >&2
    else
      echo "Hive installation failed and rollback was incomplete; inspect $backup_dir, $link_backup, and $skill_backup." >&2
    fi
  fi
  cleanup
  exit "$status"
}
trap finish EXIT
trap 'exit 130' INT TERM

if [ -n "$release_dir" ]; then
  cp -- "$release_dir/$asset" "$release_dir/$asset.sha256" "$tmp/"
else
  gh release download "$version" --repo "$repository" --pattern "$asset" --pattern "$asset.sha256" --dir "$tmp"
fi
if [ "${HIVE_SKIP_ATTESTATION:-0}" = "1" ]; then
  [ -n "$release_dir" ] || { echo "HIVE_SKIP_ATTESTATION is allowed only with HIVE_RELEASE_DIR." >&2; exit 1; }
else
  gh attestation verify "$tmp/$asset" \
    --repo "$repository" \
    --signer-workflow "$repository/.github/workflows/integrated-release.yml" \
    --source-ref "refs/tags/$version" \
    --source-digest "$release_commit" \
    --signer-digest "$release_commit" \
    --deny-self-hosted-runners >/dev/null
  current_release_commit="$(resolve_release_commit)"
  [ "$current_release_commit" = "$release_commit" ] || {
    echo "Release tag $version moved from $release_commit to $current_release_commit during verification." >&2
    exit 1
  }
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
const inventoried = new Set();
for (const file of manifest.files) {
  if (!file.path || path.posix.isAbsolute(file.path) || file.path.includes('\\') || file.path.split('/').includes('..')) throw new Error(`unsafe inventory path: ${file.path}`);
  const target = fs.realpathSync(path.join(root, ...file.path.split('/')));
  if (!target.startsWith(root + path.sep)) throw new Error(`inventory path escaped root: ${file.path}`);
  const data = fs.readFileSync(target);
  if (data.length !== file.size || crypto.createHash('sha256').update(data).digest('hex') !== file.sha256) throw new Error(`inventory mismatch: ${file.path}`);
  if (inventoried.has(file.path)) throw new Error(`duplicate inventory path: ${file.path}`);
  inventoried.add(file.path);
}
const actual = [];
const walk = (dir) => {
  for (const entry of fs.readdirSync(dir, {withFileTypes: true})) {
    const target = path.join(dir, entry.name);
    if (entry.isSymbolicLink()) throw new Error(`distribution contains symlink: ${path.relative(root, target)}`);
    if (entry.isDirectory()) walk(target);
    else if (entry.isFile()) actual.push(path.relative(root, target).split(path.sep).join('/'));
    else throw new Error(`distribution contains unsupported entry: ${path.relative(root, target)}`);
  }
};
walk(root);
const expectedActual = new Set([...inventoried, 'distribution-manifest.json']);
for (const file of actual) if (!expectedActual.has(file)) throw new Error(`uninventoried distribution file: ${file}`);
for (const file of expectedActual) if (!actual.includes(file)) throw new Error(`missing distribution file: ${file}`);
for (const required of ['hive', 'runtime/node', 'visual-hive/visual-hive.mjs', 'visual-hive/release-manifest.json', 'skills/hive/SKILL.md', 'skills/hive/agents/openai.yaml']) {
  if (!fs.statSync(path.join(root, ...required.split('/'))).isFile()) throw new Error(`missing ${required}`);
}
NODE

mkdir -p "$parent_dir" "$HOME/.local/bin"
staging_dir="$install_dir.new.$$"
case "$staging_dir" in "$parent_dir"/*) ;; *) echo "Unsafe staging path." >&2; exit 1 ;; esac
rm -rf -- "$staging_dir"
cp -R "$source_dir" "$staging_dir"
[ ! -e "$backup_dir" ] || rm -rf -- "$backup_dir"
if [ -e "$install_dir" ]; then
  mv -- "$install_dir" "$backup_dir"
  had_previous=1
fi
if ! mv -- "$staging_dir" "$install_dir"; then
  [ ! -e "$install_dir" ] || rm -rf -- "$install_dir"
  if [ "$had_previous" = "1" ] && [ -e "$backup_dir" ]; then
    if mv -- "$backup_dir" "$install_dir"; then
      echo "Hive activation failed; the previous installation was restored." >&2
    else
      echo "Hive activation failed and the previous installation could not be restored; backup remains at $backup_dir." >&2
    fi
  fi
  exit 1
fi
activated=1
if [ -e "$link_path" ] || [ -L "$link_path" ]; then
  [ ! -e "$link_backup" ] && [ ! -L "$link_backup" ] || rm -rf -- "$link_backup"
  mv -- "$link_path" "$link_backup"
  link_backup_complete=1
fi
ln -s "$install_dir/hive" "$link_path"
link_candidate_installed=1

mkdir -p "$codex_home/skills"
case "$skill_target" in "$codex_home"/skills/*) ;; *) echo "Unsafe Codex skill path." >&2; exit 1 ;; esac
if [ -e "$skill_target" ]; then
  [ ! -e "$skill_backup" ] || rm -rf -- "$skill_backup"
  mv -- "$skill_target" "$skill_backup"
  skill_backup_complete=1
fi
skill_copy_started=1
cp -R "$install_dir/skills/hive" "$skill_target"
committed=1
[ ! -e "$backup_dir" ] || rm -rf -- "$backup_dir" || echo "Warning: prior Hive backup remains at $backup_dir" >&2
[ ! -e "$link_backup" ] && [ ! -L "$link_backup" ] || rm -rf -- "$link_backup" || echo "Warning: prior launcher backup remains at $link_backup" >&2
[ ! -e "$skill_backup" ] || rm -rf -- "$skill_backup" || echo "Warning: prior skill backup remains at $skill_backup" >&2

echo "Hive $version installed at $install_dir"
echo "Authenticate with 'gh auth login' only if needed, then run this exact-path command in the current shell:"
echo "$link_path setup --repo owner/repository --coverage comprehensive --automation auto-merge --provider codex --visual-hive --start --json"
