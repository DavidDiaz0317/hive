#!/usr/bin/env sh
set -eu

installer="${1:?Linux installer path is required}"
version="${2:?Integrated release version is required}"
work_root="${3:?Writable proof root is required}"
repository="${HIVE_REPOSITORY:-DavidDiaz0317/hive}"

export HOME="$work_root/home"
export HIVE_VERSION="$version"
export HIVE_REPOSITORY="$repository"
export HIVE_INSTALL_DIR="$work_root/installed"
mkdir -p "$HOME"

sh "$installer"
"$HIVE_INSTALL_DIR/runtime/node" "$HIVE_INSTALL_DIR/visual-hive/visual-hive.mjs" --version
"$HIVE_INSTALL_DIR/hive" setup \
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
echo "Signed Linux integrated installer smoke passed: $work_root"
