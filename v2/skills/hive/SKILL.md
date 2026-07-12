---
name: hive
description: Set up, verify, and operate Hive with Visual Hive as production repository testing and repair automation. Use when a user invokes /hive or $hive, asks to install Hive on a repository, choose testing coverage or autonomous authority, run a production scan, manage Hive findings/issues/repair PRs, pause or resume automation, or perform an immutable upgrade, rollback, or uninstall.
---

# Hive

Use Hive's MCP tools as the control plane. Keep coverage depth separate from GitHub write authority and take setup through a verified hosted run when the user asks for production setup.

## Setup workflow

1. Identify the `owner/repository`. Never request a pasted token; use the user's existing GitHub authorization flow.
2. Call `hive_setup_plan` before any mutation. Show detected stack, test layers, files, warnings, and required permissions in plain language.
3. If absent, ask for exactly two choices:
   - Coverage: `essential`, `standard`, `comprehensive`, or `custom`.
   - Automation: `advisory`, `issues`, `repair-pr`, or `auto-merge`.
4. Explain that `issues` can open/update/close findings but cannot create a branch; `repair-pr` can create and revise one linked PR but cannot merge; `auto-merge` can merge only after exact-SHA deterministic, required-check, protection, risk, and hold gates all pass.
5. Obtain explicit setup approval, then call `hive_setup_apply` with the same reviewed values. Let Hive choose its isolated repository state directory unless the user explicitly supplied `state_dir`.
6. Report the one setup PR. If the user authorized taking setup to production, wait for its hosted checks, merge it only when green and reviewable, then continue. Setup never mutates protection before this merge.
7. For `auto-merge`, call `hive_run` (or leave the requested scheduler running). Hive must verify the installed files and exact production workflow path/event/head, complete the independent production verdict and actively guarded PR-context eligibility seed, and durably activate exact GitHub-Actions-App-bound `visual-hive` protection before applying evidence or making any lifecycle write. Repository-owned policy is never replaced. A green seed or same-name check is not a PR verdict: merge gating must prove the Check Suite came from `.github/workflows/visual-hive-pr.yml`, event `pull_request`, and the exact PR head/base association.
8. Call `hive_doctor`, resolve every failed check, and verify `production_ready=true`. Verify the hosted run, provenance-bound evidence, lifecycle mutations allowed by the selected authority, and duplicate-free durable state. End with `hive_status`.

## Operating rules

- Use `hive_status` before changing an existing installation.
- Use `hive_set_coverage` only for test depth. Use `hive_set_automation` only for issue/PR/merge authority.
- Coverage, automation, and limit tools create or reuse the managed setup PR when repository files must change. Wait for exact-head checks and merge that PR before expecting doctor or production runs to pass.
- Use `hive_pause` immediately when the user requests a stop or when an unexpected write occurs. Confirm status before `hive_resume`.
- Treat policy denial as a real stop. Never silently downgrade a denied issue, PR, close, reopen, or merge into advice.
- Keep Hive as the only GitHub lifecycle writer in integrated mode. Do not enable Visual Hive's standalone publishers concurrently.
- Never close a finding because a partial, stale, changed-files-only, expired, or unrelated scan omitted it. Closure requires an authoritative target-branch run that evaluated the affected contract after the recorded merge.
- For `repair-pr`, leave the issue open until post-merge verification. For `auto-merge`, require all gates returned by Hive; do not merge separately with a GitHub command.
- For a `merge_policy` path hold, review the exact diff, call `hive_plan_merge_approval` with the PR number and exact head, then repeat its exact base SHA and raw-diff digest in `hive_approve_merge` with an accountable reason. Hive must perform the merge. Never use a direct GitHub merge as a substitute. Use `hive_revoke_merge_approval` if the review is withdrawn.
- For a resumable infrastructure or patch-engine failure, read the exact recurrence, attempt, failure class, and failure ID from `hive_status`, correct the cause, and call `hive_retry_repair`. Never fabricate or omit the failure ID.
- If `hive_status` reports `workflow_dispatch_recovery`, do not rerun, delete state, or invent a digest. Use `hive_plan_dispatch_recovery` with the exact correlation. Prefer `revoke` whenever the original request might still arrive; use `retry` only with independent evidence that it was not accepted. Repeat every exact value from the plan in `hive_recover_dispatch` with an accountable reason.
- Use `hive_upgrade` and `hive_rollback` only with immutable 40-character Visual Hive commit SHAs. Review the generated PR.
- Use `hive_uninstall` only after explicit confirmation. Preserve local state unless the user explicitly requests permanent state deletion.

## Tool fallback

If Hive MCP tools are unavailable but the CLI is installed, use `hive` when it is on `PATH`; otherwise use the installer's exact launcher (`$HOME/.local/bin/hive` on Linux or `$env:LOCALAPPDATA\Hive\hive.exe` on Windows). Run the equivalent CLI with `--json`. Hive automatically selects isolated per-repository state; preserve an explicit `--state-dir` only when the user supplied one. Start with:

```text
hive setup --repo OWNER/REPO --coverage LEVEL --automation MODE --provider codex --visual-hive --start --json
hive run --json
hive doctor --json
hive status --json
```

Do not ask the user to hand-edit workflow YAML or repository variables. If neither MCP nor the CLI is available, report the missing installation rather than improvising repository writes.
