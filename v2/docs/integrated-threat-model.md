# Integrated Hive Threat Model

## Trust boundaries

- Pull-request code is untrusted. PR workflows receive read-only permissions, no lifecycle credential, and no protected-target secret. Generated workflows never use `pull_request_target` to execute PR code.
- Protected target workflows produce deterministic Visual Hive evidence. Evidence is not trusted because the producer says it is trusted; Hive independently verifies GitHub repository identity, run, artifact, conclusion, event, exact SHA, attestation requirement, expiry, and replay key.
- Hive is the sole GitHub lifecycle writer in integrated mode. Visual Hive standalone publishing must remain disabled while Hive integration is enabled.
- Repair agents run in isolated branches/worktrees. Central policy and the GitHub proxy authorize issue, branch, commit, PR, close/reopen, and merge operations independently of prompts.

## Supply chain

- Integrated archives have GitHub Sigstore build provenance and SHA-256 checksum assets.
- The installer rejects path traversal, absolute paths, digest/size drift, wrong platform, and missing required files before activation.
- `release-manifest.json` binds the Visual Hive payload to its exact commit and inventories every regular non-symlinked file. Hive verifies it before executing the CLI and in `hive doctor`.
- Generated Actions use immutable commit SHAs. Visual Hive workflow checkout uses an exact 40-character commit.
- Node is an exact 22.x release downloaded from nodejs.org and verified against that release's `SHASUMS256.txt` during packaging.

## Credentials

- The installer and daemon use the GitHub CLI authorization flow; users never paste tokens into repository config.
- The detached scheduler removes inherited GitHub token environment variables and resolves authorization for each run. Provider credentials remain local to the operator environment and are never written to evidence, audit logs, issues, or repair prompts.
- Untrusted target code is never given Hive's lifecycle credential. Trusted writes happen only after the hosted evidence boundary.

## Evidence and lifecycle defenses

Hive rejects expired, tampered, partial/non-authoritative, stale-run, cross-repository, and replay-collision bundles. It rejects symlinked, oversized, secret-bearing, absolute, and traversal paths. A partial or changed-files scan cannot close a finding. Closure requires a successful complete target-branch run at the verified merge descendant that evaluated the affected contract and observed the finding absent.

One repository-scoped fingerprint maps to one durable bead, issue, active repair branch, and PR. Duplicate delivery is idempotent. Recurrence reopens the same issue and bead. Exact-head checks, required branch protection, file/risk allowlists, retry budgets, pause/kill state, baseline review, and forbidden change classes are all merge prerequisites.

## Emergency response

Run `hive pause` to deny new lifecycle writes immediately. Run `hive stop` to terminate scheduling while preserving state. Revoke GitHub authorization if credential compromise is suspected. Preserve `integrated/audit.jsonl`, lifecycle state, repair state, and `daemon.log` for review. Do not delete state until reconciliation is complete.

