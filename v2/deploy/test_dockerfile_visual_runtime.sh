#!/usr/bin/env bash
set -euo pipefail

DOCKERFILE="$(cd "$(dirname "$0")/.." && pwd)/Dockerfile"

grep -Fq 'bash bubblewrap curl' "$DOCKERFILE"
grep -Eq '^ARG CODEX_VERSION=[0-9]+\.[0-9]+\.[0-9]+$' "$DOCKERFILE"
grep -Fq 'npm install -g @openai/codex@${CODEX_VERSION}' "$DOCKERFILE"
grep -Fq 'test -f /opt/hive/codex/bin/codex' "$DOCKERFILE"
grep -Fq 'chmod 0555 /opt/hive/codex/bin/codex' "$DOCKERFILE"
grep -Fq 'ENV PATH="/opt/hive/codex/bin:${PATH}"' "$DOCKERFILE"

echo "Dockerfile contains the contained Visual Hive runtime prerequisites."
