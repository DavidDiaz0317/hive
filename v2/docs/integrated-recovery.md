# Integrated Hive Recovery and Troubleshooting

## Scheduler recovery

`hive setup --start` launches a detached scheduler. The scheduler owns one OS-locked, ownership-bearing `integrated/daemon.lease`, writes atomic `integrated/daemon.json` status, and logs to `integrated/daemon.log`. Crashed/stale owners are recovered without a permanent start marker. It runs immediately, waits one minute after setup/readiness errors, and returns to the configured cadence after a successful production run.

After a host restart:

```bash
hive status --json
hive start --json
```

`start` is idempotent. Concurrent starts converge on the OS-held lease owner, and stale/crashed ownership is recoverable. Repair and lifecycle stores are loaded before new work; an active PR is resumed rather than duplicated.

## Common doctor failures

- `visual_hive_runtime`: reinstall the signed integrated release matching the configured exact Visual Hive commit. Do not point Hive at a mutable branch.
- `provider`: run the provider's login command in the same user account that owns the scheduler.
- `github_auth`: run `gh auth status`, then `gh auth login` if needed. Confirm access to the exact target repository.
- `setup_installed`: review and merge the single Hive setup/upgrade PR after checks pass. A stale historical PR is ignored only when the exact durable policy is already installed on the target branch.
- `workflow_installed`: rerun idempotent `hive setup` and inspect the setup PR.
- `branch_protection`: before the setup PR is merged, doctor reports setup activation pending and remains read-only. After merge, the next started Hive run verifies the exact production workflow path/event/default head, completes `visual-hive-production` plus the actively guarded `visual-hive` eligibility seed, and activates strict/up-to-date protection with the PR `visual-hive` context bound to the exact GitHub Actions App ID. If protection is absent, leave the scheduler running or run `hive run`; if repository-owned policy exists but is missing strict mode, administrator enforcement, or the exact App identity, update that policy explicitly because Hive will not replace it. A green production seed never counts as a PR verdict; Hive requires exact `.github/workflows/visual-hive-pr.yml`/`pull_request`/PR-head Check Suite provenance at merge time.

## Failed or interrupted repairs

Use `hive status --json` to inspect the fingerprint, bead, issue, repair attempt, branch, PR, check SHA, validation run, and retry count. Do not delete a repair branch while its PR is open. Hive resumes the existing attempt after restart; a new attempt is created only after a terminal revision decision and within the configured retry budget.

Infrastructure and patch-engine failures pause at an exact durable checkpoint without silently consuming another model attempt. `hive status --json` returns `resumable_repairs` with the recurrence, attempt, failure class, failure ID, and exact next command. Restore the missing dependency or investigate the patch engine, then run that `hive retry-repair` command. Model-invalid patches consume a model attempt and are revised normally; they cannot be reclassified into an endless same-patch retry.

If GitHub mutation partially succeeded, rerun `hive run --json`. The durable outbox and stable markers reconcile the existing issue/PR instead of creating duplicates. A production workflow dispatch is checkpointed in `integrated/workflow-dispatch.json` before the API call and bound to a random correlation, exact repository/ref/workflow, and the run ID returned by GitHub's current API. Exact-title correlation recovers the run after a crash before that response was persisted and supports older servers that return no run details. Restart recovery waits for that same run; it never substitutes the newest manual run. The checkpoint is removed only after its evidence is successfully consumed, or after an observed terminal failed/cancelled run is handled. An ambiguous transport failure retains the checkpoint and fails closed rather than issuing an uncorrelated duplicate.

### Ambiguous workflow dispatch recovery

When the workflow-dispatch connection fails without an HTTP response, Hive cannot prove whether GitHub accepted the request. A resumed daemon performs one exhaustive, paginated search for the exact workflow, ref, event, and 256-bit correlation. It binds the run if found. If no run is found, it releases the production lease immediately, marks `production_ready=false`, and returns both copy-paste recovery plan commands in `hive status --json`. It does not wait for the normal run timeout and does not dispatch again automatically.

Plan first. Planning is read-only and fails if discovery is incomplete or an exact run exists:

```bash
hive recover-dispatch --action revoke --correlation EXACT_CORRELATION_FROM_STATUS --plan --json
```

The result contains the exact request digest, plan digest, authenticated actor, plan time, expiry, discovery counts, and apply command. Apply that command with an accountable reason. Apply re-verifies the immutable repository ID and actor, repeats the full paginated search, rejects a changed or expired plan, and records the decision in `integrated/audit.jsonl` before changing state.

Choose deliberately:

- `revoke` retires the ambiguous intent. The next `hive run` creates a fresh correlation. This is the preferred action whenever the original request might still reach GitHub or provider timing is uncertain; a late old run remains an identifiable orphan and cannot be consumed by the fresh correlation.
- `retry` preserves the correlation and authorizes one retry. Use it only when transport/provider evidence establishes that the original request was not accepted. Hive searches again immediately before the retry POST and requires the completed result to remain the sole exact match. GitHub's workflow-dispatch API has no atomic search-and-create or idempotency-key boundary, so an arbitrarily delayed original request can still race that final check; if two runs ever share the correlation, Hive rejects both and remains fail-closed.

Never delete `workflow-dispatch.json` manually, invent digests, or bypass plan/apply. If discovery errors, leave state intact and retry the read-only plan after GitHub recovers.

During first auto-merge activation, the dispatch checkpoint also spans the protection mutation. If Hive stops after the trusted run or receives an ambiguous protection response, rerun `hive run --json`: Hive reopens the exact run/head/job/Check Run binding, verifies live policy, and durably records activation without applying the evidence twice. If the default branch moved during that run, Hive discards only the exactly bound stale activation run and dispatches a fresh one on the next attempt.

If a generated repair or baseline branch collides with an existing remote ref, Hive will overwrite it only when the observed remote tip has the exact repository-ID/operation trailers and is an ancestor of the local checkpoint. Otherwise preserve the branch for investigation and choose an explicit recovery path; do not force-push it manually. A concurrent remote move is rejected by the exact SHA lease and can be retried after reviewing the new tip.

Every Hive merge has an audited exact-head/base/diff authorization snapshot and authenticated writer identity, so restart recovery can complete a Hive-initiated merge without accepting another actor's or an out-of-band direct merge. A stale approval is invalidated automatically; use `hive revoke-merge-approval --reason "..." --json` for an explicit withdrawal. If someone merged a held PR outside Hive, pause and investigate; Hive fails closed rather than silently clearing the hold.

## Upgrade and rollback

An upgrade or rollback PR changes the repository's immutable Visual Hive workflow pin. The local bundled runtime must match that pin. Install the corresponding signed integrated release, merge the reviewed upgrade/rollback PR, then run:

```bash
hive doctor --json
hive start --json
```

If the new runtime fails doctor or compatibility checks, restore the previous integrated release and use `hive rollback`. Pixelmatch remains the default local verdict engine; optional ODiff/VRT adapters can be rolled back independently and cannot block recovery unless an operator explicitly made them required outside Visual Hive.

## Uninstall

`hive uninstall` stops the scheduler, pauses automation, and opens a cleanup PR. Local durable state is preserved by default. Use `--delete-state` only after the cleanup PR and lifecycle reconciliation are reviewed; deletion requires the managed config marker and refuses unsafe roots.
