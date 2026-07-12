# Hive + Visual Hive Quickstart

The integrated release is one product with two implementation repositories. Hive owns setup, scheduling, policy, issues, repair branches, PRs, merges, verification, and closure. Visual Hive owns repository analysis, Playwright execution, mutation evidence, deterministic verdicts, and provenance-bound evidence bundles.

## Install

The signed release includes Hive, a pinned Node 22 runtime, an immutable Visual Hive bundle, Playwright JavaScript support, and the `/hive` Codex skill. Go, Node, Docker, npm, and a Visual Hive checkout are not required.

Prerequisites are Git and a current GitHub CLI with `gh attestation verify` support. Before installation, run `gh auth status` and use `gh auth login` only if it fails; the installer checks this authorization before downloading anything. The account must be able to read the target repository and, for issue/repair/merge automation, have the corresponding repository write permissions. The packaged runtimes support Windows x64 and standard glibc-based Linux x64 distributions such as Ubuntu and Debian; Alpine/musl is not supported by the official bundled Node runtime.

Windows PowerShell:

```powershell
$ErrorActionPreference = "Stop"
$repo = "DavidDiaz0317/hive"
$version = [string]::Join("`n", @(gh api "repos/$repo/releases/latest" --jq .tag_name)).Trim()
if ($LASTEXITCODE -ne 0 -or $version -notmatch '^v[0-9]+\.[0-9]+\.[0-9]+-integrated\.[0-9]+$') { throw "No valid integrated Hive release was found." }
$commit = [string]::Join("`n", @(gh api "repos/$repo/commits/$version" --jq .sha)).Trim().ToLowerInvariant()
if ($LASTEXITCODE -ne 0 -or $commit -notmatch '^[a-f0-9]{40}$') { throw "The release tag did not resolve to an immutable commit." }
$work = Join-Path ([IO.Path]::GetTempPath()) ("hive-bootstrap-" + [guid]::NewGuid().ToString("N"))
New-Item -ItemType Directory -Path $work | Out-Null
try {
  gh release download $version --repo $repo --pattern install-integrated.ps1 --dir $work
  if ($LASTEXITCODE -ne 0) { throw "Installer download failed." }
  $installer = Join-Path $work "install-integrated.ps1"
  gh attestation verify $installer --repo $repo --signer-workflow "$repo/.github/workflows/integrated-release.yml" --source-ref "refs/tags/$version" --source-digest $commit --signer-digest $commit --deny-self-hosted-runners
  if ($LASTEXITCODE -ne 0) { throw "Installer provenance verification failed." }
  $current = [string]::Join("`n", @(gh api "repos/$repo/commits/$version" --jq .sha)).Trim().ToLowerInvariant()
  if ($LASTEXITCODE -ne 0 -or $current -ne $commit) { throw "The release tag changed during verification." }
  & $installer -Version $version -Repository $repo
  if ($LASTEXITCODE -ne 0) { throw "Hive installation failed." }
} finally {
  Remove-Item -LiteralPath $work -Recurse -Force -ErrorAction SilentlyContinue
}
```

Linux:

```bash
set -eu
repo=DavidDiaz0317/hive
version="$(gh api "repos/$repo/releases/latest" --jq .tag_name)"
printf '%s\n' "$version" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+-integrated\.[0-9]+$'
commit="$(gh api "repos/$repo/commits/$version" --jq .sha)"
printf '%s\n' "$commit" | grep -Eq '^[a-f0-9]{40}$'
work="$(mktemp -d "${TMPDIR:-/tmp}/hive-bootstrap.XXXXXX")"
trap 'rm -rf -- "$work"' EXIT INT TERM
gh release download "$version" --repo "$repo" --pattern install-integrated.sh --dir "$work"
gh attestation verify "$work/install-integrated.sh" \
  --repo "$repo" \
  --signer-workflow "$repo/.github/workflows/integrated-release.yml" \
  --source-ref "refs/tags/$version" \
  --source-digest "$commit" \
  --signer-digest "$commit" \
  --deny-self-hosted-runners
[ "$(gh api "repos/$repo/commits/$version" --jq .sha)" = "$commit" ]
sh "$work/install-integrated.sh" --version "$version" --repo "$repo"
```

The maintained fork enables GitHub release immutability before publication, so GitHub locks each published release's assets and associated tag. The release workflow verifies GitHub's resulting `isImmutable` state and removes any mutable publication while permanently burning that tag/version. The bootstrap verifies the installer itself before executing it. The installer then verifies the archive checksum and GitHub Sigstore provenance from the fork's exact integrated-release workflow, exact `refs/tags/<version>` source ref, exact tag commit, and a GitHub-hosted runner. It also verifies the platform and every file in `distribution-manifest.json` before atomically activating the release. Installation, launcher/PATH registration, and Codex-skill replacement form one rollback-capable transaction; a later failure restores the prior installation and skill. Both stages fail closed if provenance verification or the release tag's race-safe commit recheck fails. On Linux the installer prints an absolute `~/.local/bin/hive ...` next command, which works immediately in the same shell even when `~/.local/bin` was not previously on `PATH`.

Authorize the repair provider once before the first production setup:

```bash
codex login
```

## Set up a repository

For an agent-driven, noninteractive production installation, use the platform's exact second command. The Windows installer adds Hive to the current PowerShell process; the Linux installer deliberately does not edit shell profiles, so its absolute launcher works immediately even when `~/.local/bin` was absent from the original `PATH`.

Windows PowerShell:

```text
hive setup --repo OWNER/REPOSITORY --coverage comprehensive --automation auto-merge --provider codex --visual-hive --start --json
```

Linux:

```bash
"$HOME/.local/bin/hive" setup --repo OWNER/REPOSITORY --coverage comprehensive --automation auto-merge --provider codex --visual-hive --start --json
```

For a human-driven installation, `hive setup --repo OWNER/REPOSITORY` asks only for missing plain-language choices:

- Coverage: `essential`, `standard`, `comprehensive`, or `custom`.
- Automation: `advisory`, `issues`, `repair-pr`, or `auto-merge`.

Coverage controls test depth. Automation controls GitHub write authority; the two are independent. Integrated mode always includes Visual Hive's deterministic verdict layer; disabling it is intentionally unsupported. Setup inspects the repository, validates the exact bundled Visual Hive commit, creates or reuses one setup PR, initializes durable state, and optionally starts the persistent scheduler. It does not mutate branch protection while the workflow exists only in an unmerged PR. No YAML, JSON, repository-variable, or dashboard editing is required.

Auto-merge uses two-phase activation. Merge the exact green managed setup PR. The already-started scheduler (or the next `hive start`/`hive run`) then verifies the installed default-branch files and the exact production workflow path, name, event, ref, and head. That production run emits the independent `visual-hive-production` verdict plus one `visual-hive` eligibility seed required by GitHub's seven-day recent-check rule. The seed is never skipped: it actively fails unless the event is `workflow_dispatch`, the ref is the default branch, and the dispatch SHA is still the checked-out current default head. Hive binds both Check Runs to that exact production run and the GitHub Actions App before creating conservative strict protection. If repository-owned policy already exists, Hive never replaces it: the policy must already enforce administrators, strict up-to-date checks, and the exact PR `visual-hive`/GitHub-Actions-App identity. The activation is durable and idempotent, and no evidence application, issue mutation, repair, or merge occurs before it succeeds.

On a repository that has no pre-existing CI, the first setup PR can legitimately have no Visual Hive check: GitHub does not trigger a newly added `pull_request` workflow until that workflow exists on the default branch. Review the exact Hive-owned setup diff and any checks required by the repository's current policy. Hive never treats the candidate workflow as its own security proof; the trusted default-branch run and actively guarded seed after merge establish first-install eligibility. Later repair and upgrade PRs use the installed `visual-hive` check. Hive marks that check green only when its Check Suite maps to the exact `.github/workflows/visual-hive-pr.yml` run with event `pull_request`, the exact PR head, and the matching PR/base association when GitHub returns it. A green production seed, manual same-name Check Run, wrong workflow, or wrong App cannot mask a red or missing PR workflow check.

An agent can use the same noninteractive equivalent (shown here with the Linux exact path; use `hive` in the installer PowerShell session on Windows):

```bash
"$HOME/.local/bin/hive" setup \
  --repo OWNER/REPOSITORY \
  --coverage comprehensive \
  --automation auto-merge \
  --provider codex \
  --visual-hive \
  --start \
  --json
```

The installer also places the `hive` Codex skill under `${CODEX_HOME:-~/.codex}/skills/hive`. Invoke `$hive` or `/hive`; it plans read-only first, asks for coverage and authority, applies setup through Hive MCP when the client exposes it and otherwise uses the bundled CLI directly, verifies doctor, runs the first hosted scan, and ends with durable status. Setup therefore does not depend on a separate, installer-owned rewrite of the user's MCP configuration.

Running the identical setup command again is a true no-op when the target branch already contains the exact managed policy. Omitted values on an existing installation inherit the installed coverage, authority, limits, provider, allowlists, and immutable Visual Hive pin. A new local bundle path can be rebound without creating a repository PR when the immutable pin is unchanged.

Hive automatically assigns each repository an isolated state directory under `~/.hive/repositories/` and durably selects the most recently set-up repository for later commands. Setting `HIVE_STATE_DIR` or passing `--state-dir` remains available for explicit service accounts and migrations, but it is not part of the normal install path. Setting up a second repository therefore cannot collide with or overwrite the first repository's lifecycle state.

Every production workflow dispatch carries a required, cryptographically random Hive correlation input. Hive persists that correlation before calling GitHub and binds the run ID returned by the current GitHub API before waiting for completion. The exact correlated run title is a crash-recovery and older-server fallback; concurrent or manually dispatched runs are never selected by recency, and an interrupted Hive process resumes the same run instead of dispatching a duplicate. During first activation the dispatch checkpoint remains unconsumed across a crash or policy error, so a retry verifies the same run/head/check instead of dispatching another run.

If GitHub accepts no response at all, Hive cannot distinguish a rejected request from an accepted request whose response was lost. It stops immediately and `hive status --json` reports `workflow_dispatch_recovery` with exact read-only plan commands. Prefer the `revoke` plan unless independent transport evidence proves the original request was never accepted. Plan/apply repeats exhaustive exact-correlation discovery and binds the authenticated operator, request digest, action, plan digest, timestamp, expiry, and reason. See [Integrated Hive Recovery and Troubleshooting](integrated-recovery.md#ambiguous-workflow-dispatch-recovery).

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
hive approve-merge --pr NUMBER --head EXACT_40_CHARACTER_SHA --plan --json
hive approve-merge --pr NUMBER --head EXACT_HEAD_FROM_PLAN --base EXACT_BASE_FROM_PLAN --diff-digest EXACT_DIFF_SHA256_FROM_PLAN --reason "reviewed exact config repair" --json
hive revoke-merge-approval --reason "review withdrawn" --json
hive retry-repair --finding FINGERPRINT --recurrence N --attempt N --failure-class infrastructure --failure-id EXACT_FAILURE_ID --reason "toolchain restored" --json
hive recover-dispatch --action revoke --correlation EXACT_CORRELATION_FROM_STATUS --plan --json
hive upgrade --version IMMUTABLE_VISUAL_HIVE_COMMIT --json
hive rollback --json
hive uninstall --json
```

`hive status` includes scheduler PID, last attempt, last successful hosted run, next run, sanitized error state, held PR number/branch/head/reason, active exact-head approval/merge intent, ambiguous dispatch state, and copy-paste approval, repair-retry, or dispatch-recovery plan commands. `pause` immediately cancels an active run, denies lifecycle writes, stops the scheduler, and preserves durable state. `stop` exits the scheduler but preserves state. `resume` restores only the configured authority and restarts the prior scheduler cadence; it never escalates authority.

The managed production workflow is dispatch-only. The local Hive scheduler is the single cadence owner and every hosted run is bound to one durable Hive consumer, preventing duplicate push/schedule scans from queueing behind post-merge verification.

Coverage, automation, issue-limit, and retry-limit changes regenerate the managed repository configuration and workflow through the same single reviewed setup PR path. They are not local-only switches.

When Hive holds a safe repair because a file is outside the auto-merge path allowlist, review the exact diff and first use `hive approve-merge --plan`. The read-only result returns the base SHA and raw-diff SHA-256 that the apply command must repeat. Apply with those values and a reason, or revoke with `hive revoke-merge-approval`. The approval binds the live repository ID, PR, base SHA, head SHA, raw-diff digest, authenticated GitHub actor, reason, and timestamp. Hive records a durable authorization snapshot and merge intent, rechecks every non-path gate, binds the final live base/head, and calls the merge API itself. Do not merge the PR directly.

Upgrade and rollback create reviewable PRs with exact Visual Hive commits. Install the matching signed integrated release before declaring the new pin ready; `hive doctor` rejects a local runtime whose release manifest does not match repository configuration.
