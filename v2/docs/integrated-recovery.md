# Integrated Hive Recovery and Troubleshooting

## Scheduler recovery

`hive setup --start` launches a detached scheduler. The scheduler claims `integrated/daemon.pid`, writes atomic `integrated/daemon.json` status, and logs to `integrated/daemon.log`. It runs immediately, waits one minute after setup/readiness errors, and returns to the configured cadence after a successful production run.

After a host restart:

```bash
hive status --json
hive start --json
```

`start` is idempotent. It reuses a live scheduler, rejects concurrent starts, and replaces only a stale PID whose process identity does not match the installed Hive executable. Repair and lifecycle stores are loaded before new work; an active PR is resumed rather than duplicated.

## Common doctor failures

- `visual_hive_runtime`: reinstall the signed integrated release matching the configured exact Visual Hive commit. Do not point Hive at a mutable branch.
- `provider`: run the provider's login command in the same user account that owns the scheduler.
- `github_auth`: run `gh auth status`, then `gh auth login` if needed. Confirm access to the exact target repository.
- `setup_pr_merged`: review and merge the single Hive setup PR after checks pass. The scheduler will retry without creating another setup PR.
- `workflow_installed`: rerun idempotent `hive setup` and inspect the setup PR.
- `branch_protection`: auto-merge requires at least one successful required check on the default branch.

## Failed or interrupted repairs

Use `hive status --json` to inspect the fingerprint, bead, issue, repair attempt, branch, PR, check SHA, validation run, and retry count. Do not delete a repair branch while its PR is open. Hive resumes the existing attempt after restart; a new attempt is created only after a terminal revision decision and within the configured retry budget.

If GitHub mutation partially succeeded, rerun `hive run --json`. The durable outbox and stable markers reconcile the existing issue/PR instead of creating duplicates. If an external action was performed outside Hive, pause, reconcile GitHub and lifecycle state, then resume.

## Upgrade and rollback

An upgrade or rollback PR changes the repository's immutable Visual Hive workflow pin. The local bundled runtime must match that pin. Install the corresponding signed integrated release, then run:

```bash
hive doctor --json
hive start --json
```

If the new runtime fails doctor or compatibility checks, restore the previous integrated release and use `hive rollback`. Pixelmatch remains the default local verdict engine; optional ODiff/VRT adapters can be rolled back independently and cannot block recovery unless an operator explicitly made them required outside Visual Hive.

## Uninstall

`hive uninstall` stops the scheduler, pauses automation, and opens a cleanup PR. Local durable state is preserved by default. Use `--delete-state` only after the cleanup PR and lifecycle reconciliation are reviewed; deletion requires the managed config marker and refuses unsafe roots.

