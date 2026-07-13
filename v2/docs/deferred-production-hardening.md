# Deferred production hardening

This ledger records non-blocking P2 work deferred by the Hive + Visual Hive
release-scope freeze on 2026-07-12. None of these items is required for the
current two-command installation, repository lifecycle, or persistent
scheduler acceptance criteria.

## Deferred items

- Add an optional Windows service installation mode for operation before an
  interactive user login. The supported scheduler in this release persists
  across restarts and resumes at the owning user's next login without asking
  for stored credentials.
- Shorten the cold Windows installer recovery-matrix runtime. The complete
  matrix is bounded, passes, and remains inside the hosted release-job limit;
  further optimization is a CI-maintenance improvement.
- Add optional visual-provider adapters beyond the first-party Playwright
  path. No paid provider is required for deterministic verdicts.
- Expand package-manager-specific convenience paths beyond the npm and mixed
  JavaScript/Python proof repositories used by this release.
- Add further synthetic failure variants beyond the existing immutable
  candidate, exact-head/diff, hosted evidence, durable journal, lifecycle
  authority, and duplicate-prevention coverage.
- Normalize pre-existing Go formatting drift in untouched upstream files. The
  frozen candidate's changed and new Go files are checked independently.

These items must be reconsidered in a separate goal with their own acceptance
criteria. They are not release blockers for the current frozen candidate.
