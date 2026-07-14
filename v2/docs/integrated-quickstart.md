# Hive + Visual Hive Quickstart

The integrated release is one product with two implementation repositories. Hive owns setup, scheduling, policy, issues, repair branches, PRs, merges, verification, and closure. Visual Hive owns repository analysis, Playwright execution, mutation evidence, deterministic verdicts, and provenance-bound evidence bundles.

## Install

The signed release includes Hive, a pinned Node 22 runtime, an immutable Visual Hive bundle, Playwright JavaScript support, and the `/hive` Codex skill. Go, Node, Docker, npm, and a Visual Hive checkout are not required.

Prerequisites are Git and a current GitHub CLI with `gh attestation verify` support. Windows also uses the built-in `tar.exe`/libarchive tool for bounded native ZIP extraction; the installer checks for it before downloading. Before installation, run `gh auth status` and use `gh auth login` only if it fails; the installer checks this authorization before downloading anything. The account must be able to read the target repository and, for issue/repair/merge automation, have the corresponding repository write permissions. The packaged runtimes support Windows x64 and standard glibc-based Linux x64 distributions such as Ubuntu and Debian; Alpine/musl is not supported by the official bundled Node runtime. Linux repair automation also requires `bwrap` on `PATH`, normally supplied by the distribution's open-source `bubblewrap` package. Ubuntu hosts that enforce AppArmor user-namespace restrictions must also install `apparmor-profiles` and load the packaged `/usr/share/apparmor/extra-profiles/bwrap-userns-restrict` profile with `apparmor_parser`; the setup agent must obtain system-administrator approval before making those host-level changes. Hive provider health names the missing package or profile and stops setup before any model request. Persistent Linux scheduling additionally requires systemd/logind and `loginctl`. `hive setup --start` checks `Linger` for the exact current numeric UID, enables it when needed, and rechecks it before reporting reboot persistence; missing tooling, denied enablement, or a failed recheck stops startup. Linger is an account-wide setting that may support other user services, so Hive uninstall deliberately leaves it enabled. Windows deliberately uses a least-privilege `InteractiveToken` task so it can reuse the signed-in user's GitHub and Codex credential stores without saving a Windows password. It is restart-on-failure and resumes at the next user logon (`login_persistent=true`), but it cannot run before that first logon after reboot (`reboot_persistent=false`). Use a Linux systemd service account when unattended pre-login reboot execution is mandatory.

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

The maintained fork enables GitHub release immutability before publication, so GitHub locks each published release's assets and associated tag. The release workflow verifies GitHub's resulting `isImmutable` state and removes any mutable publication while permanently burning that tag/version. The bootstrap verifies the installer itself before executing it. The installer then verifies the archive checksum and GitHub Sigstore provenance from the fork's exact integrated-release workflow, exact `refs/tags/<version>` source ref, exact tag commit, and a GitHub-hosted runner. Before extraction it rejects unsafe paths, multiple roots, links/unsupported entry types, duplicate Windows identities, excessive entries, and excessive expanded bytes. It independently hashes the inventoried Node validator, strips `NODE_OPTIONS`/`NODE_PATH`, then verifies the platform and every manifest file, hash, size, link, and extra-file boundary before atomically activating the release. Installation, launcher/PATH registration, Codex-skill replacement, and exact scheduler migration form one rollback-capable transaction. Before replacing a recognized installation, the candidate binary durably inventories every exact registered scheduler, writes a bounded journal, stops the services and lease owners, and after activation restarts each prior interval with the new immutable Hive commit and executable SHA-256 binding. A normal failure restores the prior installation, launcher/PATH, skill, and schedulers; an abrupt host/process failure leaves the journal for the same signed installer to resume without guessing. Both stages fail closed if provenance verification or the release tag's race-safe commit recheck fails. The installer prints an exact absolute Hive next command on both platforms; it works immediately and cannot resolve an older Hive earlier on `PATH`.

Publication does not require a model credential. Its mandatory Linux and Windows matrix installs the exact native `codex-cli 0.144.1` artifact into an empty `CODEX_HOME`, proves the reviewed capability inventory and fail-closed option parser, runs deterministic no-model read/write/loopback containment probes, and requires Health to reach the expected unauthenticated boundary without leaving a runnable model attestation. If the fork later configures a dedicated `HIVE_RELEASE_CODEX_AUTH_JSON_B64` Actions secret, a separate supplemental matrix also runs the model-backed random-nonce and structured JSONL containment tests; that optional credential is never required to publish and local operator credentials must not be copied into GitHub. Hive currently ships Linux x64 and Windows x64 only, so the workflow deliberately does not claim a macOS artifact or gate; adding macOS packaging requires adding the same credential-free containment gate before publication.

An explicit install directory must be an absolute, dedicated Hive leaf; filesystem roots, home directories, common parent directories, and generic workspace/data leaves are rejected. Before an upgrade moves or removes either the active directory or its `.previous` backup, the installer independently verifies the exact integrated-distribution schema, platform, immutable commits, complete file inventory, hashes, and required launchers. An unrelated or modified directory is never adopted or deleted: move it aside manually after inspection, or choose a new dedicated install directory and retry.

The Linux `~/.local/bin/hive` launcher and the `${CODEX_HOME:-~/.codex}/skills/hive` directory are protected by the same ownership rule. Hive replaces them only when the launcher points to the exact recognized active distribution and the skill has an exact packaged inventory. A same-named script, directory, modified skill, symlink, or junction is preserved and installation stops with the path to move aside. On Windows, use `-NoCodexSkill` if you intentionally want to retain a different skill with that name; on Linux, set `CODEX_HOME` to a clean location.

This integrated product is released and installed from the maintained `DavidDiaz0317/hive` fork only. The commands above intentionally bind provenance to that repository. Do not publish integrated tags or assets to, push integration branches to, or open integration pull requests against `kubestellar/hive`; its deployment instructions are retained only for the legacy service. A different fork is a different trust root and must use its own exact repository in every download, attestation, and installer argument.

Authorize the repair provider once before the first production setup:

```bash
codex login
```

Repair automation requires a reviewed native Codex executable. Release validation remains pinned to `codex-cli 0.144.1`; the Windows desktop-packaged `codex-cli 0.144.0-alpha.4` build is also accepted only on Windows after matching the same reviewed capability inventory and containment probes. Hive auto-detects it from `PATH` and `${CODEX_HOME:-~/.codex}/.sandbox-bin`, seals its exact bytes separately for health checks and each one-shot model invocation, and rejects `npx`, npm, `.cmd`, PowerShell, shebang, script, and package-runner wrappers. Each invocation replaces Codex's legacy disk-readable `read-only` sandbox with a final no-files permission profile: only Codex's platform-minimal runtime paths are readable, tool network is disabled, no repository/operator path is granted, approval escalation is impossible, and native Windows explicitly selects the unelevated sandbox backend so restricted reads fail closed. If auto-detection finds no usable binary, pass the native path explicitly with `--provider-command /absolute/path/to/codex` (or the Windows `.exe` path); do not add a wrapper through `--provider-arg`. Linux and macOS require their normal Codex kernel sandbox support; a host that cannot activate it fails the provider run instead of falling back to disk-wide access.

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

Coverage controls test depth. Automation controls GitHub write authority; the two are independent. Integrated mode always includes Visual Hive's deterministic verdict layer; disabling it is intentionally unsupported. Setup inspects the repository, validates the exact bundled Visual Hive commit, creates or reuses one setup PR, initializes durable state, and optionally starts the persistent scheduler. The read-only plan resolves the same installed release manifest used by apply and reports its exact `visual_hive_repository`, immutable `visual_hive_ref`, `allowed_auto_merge_paths`, and numeric `allowed_auto_merge_risk` tiers; together the immutable dependency is the reported `repository@commit`. The conservative defaults are test-only paths and `automatic` risk. An accountable operator can replace either list without editing JSON by repeating `--auto-merge-path GLOB` or `--auto-merge-risk automatic|low|medium|restricted`; the MCP setup tools expose the equivalent `auto_merge_paths` and `auto_merge_risks` arrays. Omitted values preserve an existing installation. Any resulting repository-policy change uses the same reviewed setup PR. It does not mutate branch protection while the workflow exists only in an unmerged PR. No YAML, JSON, repository-variable, or dashboard editing is required.

| Automation | Durable findings | GitHub issues | Repair branches and PRs | Merge |
| --- | --- | --- | --- | --- |
| `advisory` | Yes | None | None | None |
| `issues` | Yes | Open, update, reopen, and close | None | None |
| `repair-pr` | Yes | Full issue lifecycle | Create and revise one linked repair PR | Never; a human merges |
| `auto-merge` | Yes | Full issue lifecycle | Same as `repair-pr` | Hive only, and only after every deterministic, exact-head, protection, path, risk, hold, budget, and post-validation gate passes |

`repair-pr` is the "open PRs only" level. A denied action is a hard policy stop, not a silent fallback to a lower level. At every level Hive remains the only lifecycle writer; Visual Hive's standalone publishers must stay disabled.

The inspection selects `npm`, `pnpm`, or Yarn from the nearest committed ancestor lockfile for each package root and rejects ambiguous same-scope locks. The installer handles independent lockless package roots separately, avoids reinstalling children owned by an ancestor lock, and chooses the immutable/frozen Yarn flag from the resolved Yarn major version. For comprehensive coverage, Hive prefers a repository-authored aggregate such as `test:all` only when a strict unconditional `run SCRIPT && ...` graph reaches every selected terminal test. The only semantic substitution is a Vitest coverage leaf whose normalized command is byte-for-byte the unit leaf after removing coverage-only flags. Hive then runs that aggregate once instead of duplicating its browser and smoke suites; if proof fails, it retains the explicit commands.

On pull requests, GitHub's runner-owned step outcomes enforce the required check. The generated TSV and exit-code files are review diagnostics only. On production runs, each configured repository command executes in its own GitHub-hosted job named `Hive repository test NNN`, in the exact `Execute exact repository test command` step. Hive exhaustively reads every job attempt for its already selected `workflow_dispatch` run and requires the exact workflow definition, head SHA, job topology, one terminal command job per configured command, and successful `visual-hive-production` and `visual-hive` jobs. Missing, duplicate, rerun, or unexpected namespaced jobs fail closed. The canonical GitHub job/step conclusions—not target-written files or artifact text—are the authority for opening or resolving a `repository_test_failure`. A failed production run is usable only when this runner-owned evidence completely explains the repository-test failure and the required Visual jobs succeeded; resolution requires a later complete run in which every configured command job is green.

An exact same-repository Hive setup PR can expose a pre-existing failure when comprehensive coverage adds a repository-authored test or security suite that the old installation never ran. This does not deadlock installation. Before opening or updating the managed PR, local Hive authenticates through GitHub CLI, resolves the numeric GitHub `User` ID, re-reads the exact PR/base/head, and creates a successful commit status whose context is `hive/setup-authorized/<binding-sha256>` and whose target URL is that PR. The binding covers repository name and immutable ID, PR number, setup/base refs and SHAs, authorizer ID, a canonical no-renames raw diff plus both old and new blob bytes, every required regular file's mode/object/size/content digest, and every path that must be absent.

The read-only `Hive setup authorization` job checks out only the trusted base tree, fetches the exact proposed managed-branch commit as Git objects without checking out or executing that tree, recomputes the binding, permits only the fixed managed setup paths and Visual Hive PNG baselines, then accepts only the latest exact-context successful status created by the configured numeric `User` and targeting the same PR. PR body text, marker text, association, and author name are not authorization. Any base, head, diff, blob, ref, repository, PR, actor, or target-URL change invalidates the status. If the repository tests fail but the Visual verdict is green, only this exact out-of-band authorization allows the managed setup PR's required check to pass; its JSON proof is diagnostic, not lifecycle authority. After merge, the first production run derives the stable failure from the independently verified GitHub job/step evidence. Hive may propose the smallest dependency manifest/lockfile repair, but those paths are never added to the auto-merge allowlist: the repair remains held for exact-head approval and all required checks. Every ordinary PR stays red while the command fails, and only a complete green post-merge target-branch run can close the issue.

After the setup PR merges, Hive creates visual baselines only on GitHub-hosted Linux. Local setup never executes target dependency, build, server, or browser commands under the operator account. A dedicated `setup-baseline-capture` workflow lane runs the target in an isolated account with sealed bundled Node and a root-owned Chromium installation. A separate fresh verifier exhaustively reads every job attempt and accepts exactly two successful non-skipped jobs (all generated jobs from the other lane must be skipped), plus the exact correlation, repository ID, default-branch head, run, bounded PNG inventory, and SHA-256 manifest. Rerun duplicates, later-page jobs, and later-page artifact-name duplicates fail closed. Hive then opens one held, candidate-PNG-only baseline PR.

Baseline review has its own authority and cannot use a generic repair approval. Run `hive approve-baseline --plan --json` (MCP `hive_plan_baseline_approval`), visually review every reported PNG and exact candidate/run/artifact/PR binding, then run the returned apply command with an accountable reason (MCP `hive_approve_baseline`). The apply binds the numeric setup authorizer, immutable repository ID, capture run and artifact, PR and exact head/base, raw-diff digest, complete candidate digest, and plan digest. Hive is the sole merger. It always requires a green `visual-hive` Check Run from the exact GitHub Actions App, `.github/workflows/visual-hive-pr.yml`, `pull_request` event, and current PR head/base association. On a truly unconfigured new repository, this candidate-only, explicitly approved PR is the sole one-time merge allowed before protection exists. If any classic protection or ruleset is already configured, it must already be strict, administrator-enforced/no-bypass, mergeable at the current base, and every required check must be green; a partial policy fails closed. After merge Hive performs a complete trusted production run, verifies the merged candidate tree and authoritative bundle, and only a later run may activate protection or write lifecycle state.

Auto-merge uses staged activation. Merge the exact green managed setup PR, complete the hosted baseline capture/review/merge and its full post-merge production verification, then let the already-started scheduler (or the next `hive start`/`hive run`) verify the installed default-branch files and exact production workflow path, name, event, ref, and head. That activation run emits the independent `visual-hive-production` verdict plus one `visual-hive` eligibility seed required by GitHub's seven-day recent-check rule. The seed is never skipped: it actively fails unless the event is `workflow_dispatch`, the ref is the default branch, and the dispatch SHA is still the checked-out current default head. Hive binds both Check Runs to that exact production run and the GitHub Actions App before creating conservative strict protection. If repository-owned policy already exists, Hive never replaces it: the policy must already enforce administrators, strict up-to-date checks, and the exact PR `visual-hive`/GitHub-Actions-App identity. The activation is durable and idempotent, and no evidence application, issue mutation, repair, or merge occurs before it succeeds.

On a repository that has no pre-existing CI, the first setup PR can legitimately have no Visual Hive check: GitHub does not trigger a newly added `pull_request` workflow until that workflow exists on the default branch. Review the exact Hive-owned setup diff and any checks required by the repository's current policy. Hive never treats the candidate workflow as its own security proof; the trusted default-branch run and actively guarded seed after merge establish first-install eligibility. Later repair and upgrade PRs use the installed `visual-hive` check. Hive marks that check green only when its Check Suite maps to the exact `.github/workflows/visual-hive-pr.yml` run with event `pull_request`, the exact PR head, and the matching PR/base association when GitHub returns it. A green production seed, manual same-name Check Run, wrong workflow, or wrong App cannot mask a red or missing PR workflow check.

The generated PR workflow is repository-owned. Exact Check Suite path/event/head/App provenance proves which GitHub Actions run emitted a result; it does not attest that candidate workflow source is semantically unchanged. Hive refuses autonomous merge when a PR changes a workflow and re-verifies the exact installed managed bytes before every production run, but a repository administrator or human merge remains outside Hive's enforcement. For production, protect the default branch with strict up-to-date `visual-hive`, include administrators, prohibit force-push/deletion and unreviewed bypass, require independent code-owner approval for `.github/workflows/**`, `.hive/integrated.json`, and `visual-hive.config.yaml`, and dismiss stale approvals after a push. Do not treat a green check as sufficient review for a workflow-changing PR, and do not merge Hive repair PRs outside Hive. If the repository cannot enforce those controls, use `issues` or `repair-pr`, not `auto-merge`.

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

### Optional MCP registration and exact CLI parity

The installed Hive binary exposes a standard-input/output MCP server. If an MCP client supports local command registration, register the Windows command as `hive` with argument `mcp`. On Linux, use the immediately available absolute command `$HOME/.local/bin/hive` with argument `mcp`. In generic command/argument form:

```json
{
  "command": "hive",
  "args": ["mcp"]
}
```

Registration is optional: `/hive` uses these tools when the client exposes them and otherwise invokes the same bundled CLI. Every MCP tool returns the CLI's structured JSON result, uses the same automatically selected repository state unless `state_dir` is explicitly supplied, and preserves these exact operation mappings:

| CLI operation | MCP tool |
| --- | --- |
| `hive setup --plan --json` | `hive_setup_plan` |
| `hive setup --start --json` | `hive_setup_apply` |
| `hive doctor --json` | `hive_doctor` |
| `hive status --json` | `hive_status` |
| `hive run --json` | `hive_run` |
| `hive start --json` | `hive_start` |
| `hive stop --json` | `hive_stop` |
| `hive pause --json` | `hive_pause` |
| `hive resume --json` | `hive_resume` |
| `hive approve-merge --plan --json` | `hive_plan_merge_approval` |
| `hive approve-merge --json` | `hive_approve_merge` |
| `hive approve-baseline --plan --json` | `hive_plan_baseline_approval` |
| `hive approve-baseline --json` | `hive_approve_baseline` |
| `hive revoke-merge-approval --json` | `hive_revoke_merge_approval` |
| `hive retry-repair --json` | `hive_retry_repair` |
| `hive recover-dispatch --plan --json` | `hive_plan_dispatch_recovery` |
| `hive recover-dispatch --json` | `hive_recover_dispatch` |
| `hive transfer-setup-authorizer --json` | `hive_transfer_setup_authorizer` |
| `hive set-coverage --json` | `hive_set_coverage` |
| `hive set-automation --json` | `hive_set_automation` |
| `hive set-issue-limit --json` | `hive_set_issue_limit` |
| `hive set-retry-limit --json` | `hive_set_retry_limit` |
| `hive upgrade --json` | `hive_upgrade` |
| `hive rollback --json` | `hive_rollback` |
| `hive uninstall --json` | `hive_uninstall` |
| `hive uninstall --cancel --json` | `hive_uninstall` with `cancel: true` |

MCP arguments use the CLI long-flag name with underscores: `state_dir`, `coverage`, `automation`, `provider`, `visual_hive`, `max_active_issues`, `max_repair_attempts`, `start`, `run_interval_seconds`, `timeout_seconds`, `interval_seconds`, `value`, `delete_state`, and `cancel`. `hive_setup_apply` defaults `start` to `true`, matching the shown CLI mapping; pass `start: false` to omit `--start`. Exact repair-merge approval bindings are `pr_number`, `head_sha`, `base_sha`, `diff_digest`, and `reason`. Hosted baseline approval additionally binds `repository_id`, `capture_run_id`, `artifact_id`, `candidate_digest`, `actor_id`, and `plan_digest`; use the complete values returned by the baseline plan. Exact repair-retry bindings are `finding`, `recurrence`, `attempt`, `failure_class`, `failure_id`, and `reason`. Dispatch recovery uses `action`, `correlation`, `request_digest`, `plan_digest`, `planned_at`, and `reason`. Authorizer transfer uses `new_authorizer` plus `reason`, or `cancel: true` before merge. Uninstall accepts `cancel: true` only by itself, never with `delete_state`. The MCP schemas reject unknown properties and require immutable SHA/digest formats where the CLI does.

Running the identical setup command again is a true no-op when the target branch already contains the exact managed policy. Omitted values on an existing installation inherit the installed coverage, authority, limits, provider, allowlists, and immutable Visual Hive pin. A new local bundle path can be rebound without creating a repository PR when the immutable pin is unchanged.

Changing managed setup while hosted baseline work is active is also restart-safe. Before Hive changes the saved configuration it persists an immutable baseline-rebind target and keeps production blocked. The exact proposal branch/commit is checkpointed before its first push, so a crash before GitHub returns a PR number still leaves enough authority to discover/close any exact PR and retire the exact/proven-descendant ref. A known capture cancellation is polled to an exact terminal run; an ambiguous accepted dispatch is exhaustively rediscovered when possible and always receives a durable correlation tombstone before its dispatch checkpoint is removed. If GitHub cleanup is interrupted, `hive status --json` and `hive doctor --json` return the one complete setup retry command; a different setup request is rejected until that transaction completes. The checkpoint is consumed only after the matching changed setup and its replacement baseline intent are both durable.

Hive automatically assigns each new repository a bounded deterministic state directory under `~/.hive/repos/<short-name>-<stable-hash>/`, reports it in the setup plan, and uses the fixed `integrated/checkout` leaf so the repository slug is not repeated in Windows Git object paths. Exact repository identity remains durable in the state and checkout ownership markers. Existing installations under the legacy `~/.hive/repositories/<owner--repository>/` layout continue to be selected when their durable config binds the requested repository. Setting `HIVE_STATE_DIR` or passing `--state-dir` remains available for explicit service accounts and migrations, but it is not part of the normal install path. Setting up a second repository therefore cannot collide with or overwrite the first repository's lifecycle state.

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
hive set-issue-limit --value 5 --json
hive set-retry-limit --value 4 --json
hive approve-merge --pr NUMBER --head EXACT_40_CHARACTER_SHA --plan --json
hive approve-merge --pr NUMBER --head EXACT_HEAD_FROM_PLAN --base EXACT_BASE_FROM_PLAN --diff-digest EXACT_DIFF_SHA256_FROM_PLAN --reason "reviewed exact config repair" --json
hive approve-baseline --plan --json
# Visually inspect every candidate, then run the exact apply command returned by the plan with a real reason.
hive revoke-merge-approval --reason "review withdrawn" --json
hive retry-repair --finding FINGERPRINT --recurrence N --attempt N --failure-class infrastructure --failure-id EXACT_FAILURE_ID --reason "toolchain restored" --json
hive recover-dispatch --action revoke --correlation EXACT_CORRELATION_FROM_STATUS --plan --json
hive transfer-setup-authorizer --new-authorizer NEXT_GITHUB_LOGIN --reason "planned maintainer handoff" --json
hive upgrade --version IMMUTABLE_VISUAL_HIVE_COMMIT --json
hive rollback --json
hive uninstall --json
```

If GitHub updates the cleanup branch or strict-base snapshot before merge, use the exact `cancel_command` returned by uninstall/status instead of editing local state or weakening checks. Cancellation authenticates the recorded numeric authorizer, closes only the exact unmerged same-repository PR with its recorded number, URL, transaction marker, head branch, and base branch, and deletes the managed ref only when its current head is the original Hive commit or a proven descendant. It is restart-safe after partial API failure, restores the prior setup PR identity, and deliberately leaves automation paused. Run `hive resume` explicitly to resume the old installation, or run `hive uninstall` again to create a fresh cleanup transaction.

`hive status` includes scheduler PID, last attempt, last successful hosted run, next run, sanitized error state, hosted setup-baseline phase/run/artifact/PR and exact next command, any durable baseline-rebind cleanup and its complete setup retry, held repair PR number/branch/head/reason, active exact-head approval/merge intent, ambiguous dispatch state, and copy-paste baseline/repair approval, transfer-finalization/cancel, or dispatch-recovery commands plus an accountable repair-retry command template. `hive doctor` reports `setup_baseline_state` or `setup_baseline_rebind_state` as not ready until the exact replacement/post-merge production verification is durable; this is expected and must not be bypassed. `pause` immediately cancels an active run, denies lifecycle writes, stops the scheduler, and preserves durable state. `stop` exits the scheduler but preserves state. `resume` restores only the configured authority and restarts the prior scheduler cadence; it never escalates authority.

The managed production workflow is dispatch-only. The local Hive scheduler is the single cadence owner and every hosted run is bound to one durable Hive consumer, preventing duplicate push/schedule scans from queueing behind post-merge verification.

Coverage, automation, issue-limit, and retry-limit changes regenerate the managed repository configuration and workflow through the same single reviewed setup PR path. They are not local-only switches.

When Hive holds a safe repair because a file is outside the auto-merge path allowlist, review the exact diff and first use `hive approve-merge --plan`. The read-only result returns the base SHA and raw-diff SHA-256 that the apply command must repeat. Apply with those values and a reason, or revoke with `hive revoke-merge-approval`. The approval binds the live repository ID, PR, base SHA, head SHA, raw-diff digest, authenticated GitHub actor, reason, and timestamp. Hive records a durable authorization snapshot and merge intent, rechecks every non-path gate, binds the final live base/head, and calls the merge API itself. Do not merge the PR directly.

Upgrade and rollback create reviewable PRs with exact Visual Hive commits. Install the matching signed integrated release before declaring the new pin ready; `hive doctor` rejects a local runtime whose release manifest does not match repository configuration.

## Uninstall

Uninstall is deliberately two phase. Preparation first refuses ambiguous integrated recovery intents, then durably pauses automation and runs a bounded, audited drain. A pending hosted baseline capture is retired without guessing; an exact unmerged baseline PR is closed only after repository-ID/number/URL/marker/ref/diff verification and its branch is deleted only at the recorded commit or a proven descendant. An exact baseline PR already merged through durable Hive authority is preserved for final target reconciliation. The remaining drain closes only exact Hive-authored lifecycle issues, exact-marker repair PRs, and exact-head Hive repair branches, cancels pending outbox entries, and records lifecycle and repair checkpoints as `cancelled` rather than falsely claiming the defect was fixed. A persistent failing repository therefore does not have to become green before uninstall. Any ownership, author, marker, branch, head, approval, or merge-evidence mismatch stops the drain with automation paused and all evidence preserved for an exact retry. Once drained, Hive records the cleanup branch/head/base/path set/diff and opens the cleanup PR:

```bash
hive uninstall --delete-state --json
```

Even with `--delete-state`, this preparation call returns `finalization_pending=true` and never deletes state. Review and merge the recorded cleanup PR, then run the exact `next_command` returned by the first call. The second call repeats the quiescence proof and deletes state only after it re-verifies the same repository, PR, head, base, diff, changed paths, and a current immutable default-branch commit on which every Hive-managed file is absent. For an already-clean repository, a later unrelated default-branch commit is accepted only when the immutable current head remains free of every managed file.

Branch protection is reconciled before its ownership record is discarded, but Hive never deletes or rewrites a whole policy during uninstall. GitHub provides no conditional exact-context mutation that can rule out a concurrent administrator change between read and write. If branch protection or a ruleset still requires `visual-hive`, finalization fails closed and asks the operator to remove only that exact context while preserving every unrelated check and control. After absence is verified, retrying finalization proceeds. Any open PR, unmerged or edited cleanup, restored managed file, pending lifecycle/outbox/repair state, recovery intent, API ambiguity, or protection mismatch leaves all local state intact.
