# Current acceptance blockers

## VH-SETUP-008

- Acceptance stage: final clean-room setup / pre-setup baseline inventory
- Observed error: the automatic Windows checkout reached 150 characters, and `git show <commit>:.visual-hive/snapshots/app-shell-visual-stability__app-shell-desktop__desktop.png` failed with `Filename too long` when the checkout plus revision-path probe reached 273 characters.
- Root cause: pre-setup baseline inventory read a path-qualified revision even though `git ls-tree` had already returned the exact baseline blob SHA. Git for Windows probes the revision-path argument relative to the checkout before resolving it as an object, crossing the legacy path boundary.
- Subsystem: Hive integrated setup baseline inventory (`v2/pkg/integrated`).
- Planned fix: validate the exact blob SHA returned by `git ls-tree` and read that object with `git cat-file blob <sha>`, preserving committed-object binding, canonical PNG validation, and safe stderr handling.
- Focused test: use the reproduced long checkout and exact baseline path, prove the committed blob wins over a mutated working-tree file, and retain rejection of a committed non-PNG blob.
- State: verified-stage

## VH-SETUP-001

- Acceptance stage: hosted setup checks / deterministic verifier
- Observed error: `Enforce deterministic verdict` exits 1 with `Isolated deterministic Visual Hive verdict failed` after repository tests, setup authorization, isolated Visual Hive execution, and trusted evidence rebuild succeed.
- Root cause: the generated verifier reads `summary`, `results`, `verdictSummary`, and `verdictContributions` from `pipeline.json`. Visual Hive 0.3.2 stores run summary and results in `report.json`, and authoritative verdict summary/contributions in `verdict.json`.
- Subsystem: Hive integrated setup workflow generation (`v2/pkg/integrated`).
- Planned fix: read each value from its canonical Visual Hive artifact, preserve the exclusive-missing-baseline fail-closed checks, and reject absent or mixed-failure evidence.
- Focused test: execute the generated enforcement shell against realistic split `pipeline.json`, `report.json`, `verdict.json`, and `readiness.json` fixtures; retain adjacent passing-verdict and independent-blocker rejection cases.
- State: verified-stage

## VH-SETUP-003

- Acceptance stage: hosted setup baseline review checks
- Observed error: baseline PR #2's `pull_request` run was cancelled while its final verifier attempted to download `visual-hive-pr-raw-29348845633`; the artifact was absent because every evidence-producing dependency job had been cancelled. A simultaneous `pull_request_target` run was skipped.
- Root cause: the generated PR workflow uses one cancel-in-progress concurrency group for both `pull_request` and authorization-only `pull_request_target` events, so the second event can cancel the evidence-producing run for the same PR/head.
- Subsystem: Hive integrated setup workflow generation (`v2/pkg/integrated`).
- Planned fix: isolate concurrency by event while preserving per-event/head duplicate cancellation and keeping `pull_request_target` from executing untrusted PR code.
- Focused test: generated workflow concurrency must include the event name; shell/job invariants must still prove target-event jobs do not execute untrusted checkout or target commands; run the exact baseline-only PR workflow stage.
- State: verified-stage

## VH-SETUP-002

- Acceptance stage: setup apply / existing PR reconciliation
- Observed error: the supported setup command pushed expected head `55ee1ad8681f0a79a4455d22410715734f53bcc8`, then exited 1 because the immediate PR read still returned prior head `f22b5fb0c60cadcf5a2d72e099e509d34a2d13b7`.
- Root cause: GitHub PR-head propagation lag; a read immediately afterward returned the expected head, the branch ref matched, and exactly one PR remained open.
- Subsystem: transient GitHub API consistency during setup PR update.
- Planned fix: no product edit; observe the exact-head run, then use the identical supported setup command as idempotent reconciliation after propagation.
- Focused test: confirm PR #1 and its branch both resolve to `55ee1ad8681f0a79a4455d22410715734f53bcc8`, then confirm reconciliation records exact-head setup authority without another branch or PR.
- State: verified-stage

## VH-SETUP-004

- Acceptance stage: post-baseline full production verification
- Observed error: the successful `workflow_dispatch` run `29350643725` was rejected with `repository-test run does not match the exact completed workflow_dispatch binding` before its runner-owned repository-test jobs could be accepted.
- Root cause: the production workflow intentionally configures a correlation-bearing `run-name`. GitHub exposes that runtime name in both the workflow run `name` and each job's `workflow_name`, while Hive incorrectly required those runtime fields to equal the static workflow-definition name.
- Subsystem: Hive runner-owned repository-test evidence verification (`v2/pkg/github`).
- Planned fix: continue verifying the exact run ID, workflow ID/path, event, repository, head, terminal state, and active static workflow definition; bind every relevant job's `workflow_name` to the exact observed run name.
- Focused test: accept a correlation-bearing run name whose jobs carry that exact runtime name, and reject any run/job runtime-name divergence while retaining definition spoofing rejection.
- State: verified-stage

## VH-SETUP-005

- Acceptance stage: post-baseline full production verification retry
- Observed error: after retaining the exact successful production run checkpoint from the first verifier failure, the next supported `hive run` stopped with `lingering setup baseline dispatch no longer matches the verified capture`.
- Root cause: setup-baseline reconciliation treated every retained dispatch after capture verification as the old baseline-capture dispatch. It did not distinguish the exact production dispatch intentionally retained while phase `merged` retries post-merge verification.
- Subsystem: Hive setup-baseline durable state reconciliation (`v2/pkg/integrated`).
- Planned fix: consume only the exact completed baseline-capture checkpoint; preserve an exact production checkpoint while phase `merged` so the ordinary production dispatcher can resume it.
- Focused test: preserve a correlation-bound production retry checkpoint in phase `merged`, consume the exact capture checkpoint, and reject mismatched capture correlation/run bindings.
- State: verified-stage

## VH-SETUP-006

- Acceptance stage: post-baseline trusted bundle verification
- Observed error: after exact production dispatch recovery and runner-job verification succeeded, trusted bundle ingestion stopped with `Visual Hive workflow definition or run name mismatch`.
- Root cause: the bundle provenance verifier repeated the static-name assumption from VH-SETUP-004. GitHub exposes the correlation-bearing `run-name` as the run runtime name, while the separately fetched active workflow definition retains the static name.
- Subsystem: Hive trusted Visual Hive artifact provenance verification (`v2/pkg/github`).
- Planned fix: bind the static expected name to the active workflow definition; bind the exact correlated title to `display_title`; accept only the expected static or exact correlated value for the runtime `name` to support GitHub API variants.
- Focused test: accept the exact correlated runtime name while preserving rejection of arbitrary runtime names, wrong correlated titles, wrong definition names, paths, heads, events, and artifacts.
- State: verified-stage

## VH-SETUP-007

- Acceptance stage: post-baseline trusted bundle semantic gate
- Observed error: the independently verified bundle was rejected as `not trusted authoritative valid evidence` even though its canonical validation was `status=passed`, `trusted=true`, and `authoritativeForResolution=true`.
- Root cause: the post-baseline gate compared the validation status to `valid`, but Hive's Visual Hive validator canonically emits `passed`.
- Subsystem: Hive setup-baseline production acceptance (`v2/pkg/integrated`).
- Planned fix: require the canonical `passed` status while retaining independent trust and authoritative-resolution requirements.
- Focused test: accept only `passed + trusted + authoritative`; reject the stale `valid` literal and either missing authority flag.
- State: verified-stage
