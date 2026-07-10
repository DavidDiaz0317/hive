#!/usr/bin/env bash
set -euo pipefail

hive_root="${1:?Hive v2 source root is required}"
visual_bundle="${2:?unpacked Visual Hive release bundle is required}"
work_root="${3:-$(mktemp -d /tmp/hive-integrated-linux.XXXXXX)}"
hive_commit="${HIVE_COMMIT:-$(git -C "$hive_root" rev-parse HEAD)}"
visual_commit="${VISUAL_HIVE_COMMIT:-$(node -e "const m=require(process.argv[1]); process.stdout.write(m.gitCommit)" "$visual_bundle/release-manifest.json")}" 
node_version="22.23.1"
release_version="vlocal-linux"

[[ "$hive_commit" =~ ^[a-f0-9]{40}$ ]] || { echo "immutable Hive commit is required" >&2; exit 1; }
[[ "$visual_commit" =~ ^[a-f0-9]{40}$ ]] || { echo "immutable Visual Hive commit is required" >&2; exit 1; }

mkdir -p "$work_root/node" "$work_root/release"
archive="node-v${node_version}-linux-x64.tar.xz"
base="https://nodejs.org/dist/v${node_version}"
curl --fail --silent --show-error --location "$base/SHASUMS256.txt" -o "$work_root/node/SHASUMS256.txt"
curl --fail --silent --show-error --location "$base/$archive" -o "$work_root/node/$archive"
(cd "$work_root/node" && grep "  $archive$" SHASUMS256.txt | sha256sum --check --strict -)
tar -xJf "$work_root/node/$archive" -C "$work_root/node"
node_root="$work_root/node/node-v${node_version}-linux-x64"

(cd "$hive_root" && go build -trimpath -ldflags "-s -w -X main.gitHash=$hive_commit -X main.gitShort=${hive_commit:0:12}" -o "$work_root/hive" ./cmd/hive)
distribution="$work_root/release/hive-integrated-${release_version}-linux-amd64"
(cd "$hive_root" && go run ./cmd/hive-dist \
  --hive "$work_root/hive" --hive-commit "$hive_commit" \
  --visual-hive "$visual_bundle" --visual-hive-commit "$visual_commit" \
  --node "$node_root/bin/node" --node-license "$node_root/LICENSE" --node-version "v${node_version}" \
  --skill "$hive_root/skills/hive" --target-os linux --target-arch amd64 --output "$distribution" >/dev/null)

(cd "$work_root/release" && tar -czf "hive-integrated-${release_version}-linux-amd64.tar.gz" "hive-integrated-${release_version}-linux-amd64")
(cd "$work_root/release" && sha256sum "hive-integrated-${release_version}-linux-amd64.tar.gz" > "hive-integrated-${release_version}-linux-amd64.tar.gz.sha256")

export HOME="$work_root/home"
export HIVE_VERSION="$release_version"
export HIVE_RELEASE_DIR="$work_root/release"
export HIVE_SKIP_ATTESTATION=1
export HIVE_INSTALL_DIR="$work_root/installed"
mkdir -p "$HOME"
sh "$hive_root/install-integrated.sh"
"$HIVE_INSTALL_DIR/runtime/node" "$HIVE_INSTALL_DIR/visual-hive/visual-hive.mjs" --version
"$HIVE_INSTALL_DIR/hive" setup --repo DavidDiaz0317/visual-hive-demo-site --coverage comprehensive --automation advisory --provider codex --visual-hive --plan --state-dir "$work_root/state" --json > "$work_root/setup-plan.json"
grep -q '"schema_version": "hive.setup-plan.v1"' "$work_root/setup-plan.json"
grep -q '"read_only": true' "$work_root/setup-plan.json"
echo "Linux integrated installer smoke passed: $work_root"
