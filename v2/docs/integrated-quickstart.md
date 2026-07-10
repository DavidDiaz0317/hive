# Hive + Visual Hive Quickstart

The integrated release is one product with two implementation repositories. Hive owns setup, scheduling, policy, issues, repair branches, PRs, merges, verification, and closure. Visual Hive owns repository analysis, Playwright execution, mutation evidence, deterministic verdicts, and provenance-bound evidence bundles.

## Install

The signed release includes Hive, a pinned Node 22 runtime, an immutable Visual Hive bundle, Playwright JavaScript support, and the `/hive` Codex skill. Go, Node, Docker, npm, and a Visual Hive checkout are not required.

Prerequisites are Git and a current GitHub CLI with `gh attestation verify` support. The packaged runtimes support Windows x64 and standard glibc-based Linux x64 distributions such as Ubuntu and Debian; Alpine/musl is not supported by the official bundled Node runtime.

Windows PowerShell:

```powershell
& ([scriptblock]::Create((Invoke-RestMethod "https://raw.githubusercontent.com/DavidDiaz0317/hive/v0.3.1-integrated.3/v2/install-integrated.ps1")))
```

Linux:

```bash
curl --fail --silent --show-error --location \
  https://raw.githubusercontent.com/DavidDiaz0317/hive/v0.3.1-integrated.3/v2/install-integrated.sh | sh
```

The installer verifies the archive checksum, GitHub Sigstore build attestation, platform, and every file in `distribution-manifest.json` before atomically activating it. The install fails closed if `gh attestation verify` fails.

Authorize GitHub and the repair provider once:

```bash
gh auth login
codex login
```

## Set up a repository

Run:

```bash
hive setup --repo OWNER/REPOSITORY
```

Hive asks only for missing plain-language choices:

- Coverage: `essential`, `standard`, `comprehensive`, or `custom`.
- Automation: `advisory`, `issues`, `repair-pr`, or `auto-merge`.

Coverage controls test depth. Automation controls GitHub write authority; the two are independent. Setup inspects the repository, validates the exact bundled Visual Hive commit, creates one setup PR, initializes durable state, and starts the persistent scheduler. No YAML or repository-variable editing is required.

An agent can use the noninteractive equivalent:

```bash
hive setup \
  --repo OWNER/REPOSITORY \
  --coverage comprehensive \
  --automation repair-pr \
  --provider codex \
  --visual-hive \
  --start \
  --json
```

The installer also places the `hive` Codex skill under `${CODEX_HOME:-~/.codex}/skills/hive`. Invoke `$hive` or `/hive`; it plans read-only first, asks for coverage and authority, applies setup through Hive MCP, verifies doctor, runs the first hosted scan, and ends with durable status.

## Operate

```bash
hive doctor --json
hive status --json
hive run --json
hive start --interval 15m --json
hive stop --json
hive pause --json
hive resume --json
hive set-coverage --value standard --json
hive set-automation --value repair-pr --json
hive upgrade --version IMMUTABLE_VISUAL_HIVE_COMMIT --json
hive rollback --json
hive uninstall --json
```

`hive status` includes scheduler PID, last attempt, last successful hosted run, next run, and sanitized error state. `pause` denies lifecycle writes while preserving the scheduler and durable state. `stop` exits the scheduler but preserves state. `resume` restores only the configured authority; it never escalates it.

Upgrade and rollback create reviewable PRs with exact Visual Hive commits. Install the matching signed integrated release before declaring the new pin ready; `hive doctor` rejects a local runtime whose release manifest does not match repository configuration.
