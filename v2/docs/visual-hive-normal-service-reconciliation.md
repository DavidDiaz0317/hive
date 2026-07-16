# Visual Hive normal-service reconciliation

Status: implementation checkpoint, 2026-07-16. This branch is not a working
product. It has not passed the required local Governor-to-PR vertical test.

The binding contract is `docs/visual-hive-integration-contract.md` from
coordinator commit `3f9ddac558fe2e8911fcf0ce4b91a88d8e5508f7`.

## Source ledger

| Input | Audited state | Use on this branch |
| --- | --- | --- |
| Coordinator | `3f9ddac5`, clean | Branch base and binding documentation |
| Execution | `8c18fcf0`, then `49d835ef`, clean | Cherry-picked as `07dd2f65`, then `78b3db04` |
| Intake | Uncommitted at base `728ce71b` | Read-only design input; no code copied |
| Secure dispatcher | Uncommitted at base `844cb9e4` | Read-only design input; no code copied |
| PR v3 verifier | Separately owned at `codex/vh-pr-verifier` | Do not duplicate; accept only an audited commit later |

No remote was read or mutated from this worktree. No upstream Hive,
KubeStellar Console, hosted GitHub object, or production Hive state was
changed.

## Exact final composition

| Stage | Existing owner that must remain authoritative | Reconciliation |
| --- | --- | --- |
| Production transport | `integrated.Config`, durable `WorkflowDispatchIntent`, `dispatchAndWait`, and `FetchAndVerifyVisualHiveBundle` | Extract a fetch-only cycle. Do not create an inbox, API, queue, or generic kick. Return the validated v3 bundle, exact workflow binding, and `VerifiedVisualHiveArtifact`; do not call legacy lifecycle, bead, manager, or repair paths. |
| Import | Intake `Controller.Import` | The verified v3 artifact is the only import input. Intake's sealed plan/cache must retain the full typed work and artifact receipts needed after lifecycle replay. |
| Admission | The same normal `Governor` | Intake constructs the live admission request. Paused, budget, WIP, role-enabled, cadence, and safe-execution denials are re-evaluated when their bound inputs change. No second role registry. |
| Scheduler input | Intake `DispatchEnvelope` | Add one intake-owned adapter from a revalidated envelope to `scheduler.AdmittedWork` plus the canonical intake v3 receipt. No other constructor is permitted. |
| Prompt composition | The same normal `Scheduler` | Compose normal role policy, project context, and knowledge primer with a separately injected Codex proposal-executor profile. Normal role backend/model/launch/tools/connections remain inert. |
| Evidence content | Controller/Worker-owned bounded reader | Read only declared digest-bound regular JSON/UTF-8 artifacts below the sealed evidence root. The child receives bytes in the prompt, never a path or filesystem authority. Binary screenshots remain receipts only. |
| Durable proposal | Existing `SpecialistMailbox` | Call `PrepareGoverned` once, reserve its exact `swo-*` ID and request SHA in the intake envelope, then persist both in repair state before launch. Recovery uses only `LoadWorkOrder(ID)`. |
| Proposal execution | Existing ordinary `agent.Manager` facade | Dispatch one isolated, one-shot child through the audited dispatcher. Do not create a persistent second specialist manager. The child has no checkout, raw repository path, GitHub, Hive, wiki, graph, MCP, or filesystem-write authority. |
| Repair | Existing `repair.Worker` | Worker alone validates the proposal, applies it, validates the result, commits, branches, pushes, and creates the PR. Visual Hive supplies evidence and final verdict only. |
| Audit/UI | Existing lifecycle, normal role bead stores, Governor state, and dashboard audit | Project exact present work into the normal routed store and use existing `/api/audit`; do not add a dashboard or queue. |
| PR rerun | Separately audited PR-v3 producer/verifier commit | Cherry-pick later behind a narrow verifier interface. Never substitute the review-only verifier and never accept `pull_request_target`. No merge behavior. |

## Intake envelope to Scheduler mapping

The adapter belongs with intake because only intake can prove that its durable
envelope is canonical. It must perform these operations in order:

1. Revalidate the complete `DispatchEnvelope` and its canonical v3 receipt.
2. Use the exact immutable admitted-work JSON as Scheduler packet bytes and the
   exact finding JSON as finding bytes. Do not hash a mutable envelope after
   mailbox reservation fields are added.
3. Do not equate the v3 aggregate `PacketDigest` with the SHA-256 of the
   Scheduler's canonical packet projection. Preserve and bind both identities.
4. Map repository, repository fingerprint, source ref, base commit/tree,
   recurrence, attempt, deadline, role, routing reason, issue kind, severity,
   title, body, labels, allowed paths, affected contracts, validation command,
   reproduction source, proposal-only authority, and provenance without
   projection through a lifecycle title/body.
5. Map every artifact receipt in canonical order with exact path, SHA-256,
   bytes, kind, and content type. The recurrence key is
   `<repository-fingerprint>:r<recurrence>`.
6. Use intake's complete v3 `SpecialistEvidenceIdentity` and exact stored
   verification-receipt JSON. Those types replace the private legacy execution
   and dispatcher shapes; compatibility must not weaken validation.

`scheduler.AdmittedWork`, `EvidenceArtifact`, and
`WorkerEvidenceArtifactReader` are in `pkg/scheduler/governed_proposal.go`.
The current checkpoint binds raw work, packet, finding, and receipt bytes and
cross-checks their typed fields before composition.

## No-strength-loss evidence path

Implemented in commits `64b6abed`, `78b3db04`, and `07dd2f65`:

- `pkg/repair/governed_evidence_reader.go` opens a rooted filesystem handle,
  rejects absolute/traversing paths, links, reparse points, non-regular files,
  size drift, and digest drift, and rechecks file identity around the bounded
  read.
- Selection is deterministic: at most 16 artifacts, 32 KiB each, and 96 KiB
  total. Only declared JSON/UTF-8 evidence can be embedded.
- Mutation-survivor and test-adequacy observations fail closed when no declared
  receipt is safely embeddable. Screenshots and other binary evidence retain
  their receipts but are never decoded into the prompt.
- Artifact bytes appear as explicitly untrusted evidence data alongside the
  complete admitted-work and typed finding JSON.
- `TestGovernedQualityPromptPreservesMutationEvidenceAcrossMailboxReplay`
  proves distinctive mutation operator/survivor, missing-test, selector, and
  flow bytes reach the normal quality prompt. After the evidence file changes,
  recovery through `LoadWorkOrder` returns byte-identical request bytes without
  recomposition or reread.

## Worker and mailbox seam

The production call must be inserted at the existing sealed context boundary,
not in a parallel repair implementation:

- `pkg/repair/worker.go`: the base tree is sealed and verified before source
  context is constructed; replace the subsequent direct provider preparation
  with governed Scheduler composition and `PrepareGoverned`.
- Persist the returned work-order ID and request SHA in both the intake
  reservation and repair attempt before setting `StageModelRunning`.
- On `StageModelRunning`, accept only the persisted exact ID and call
  `LoadWorkOrder`. Never read live role policy, project config, primer, evidence
  files, or mutable provider config on replay.
- Preserve the existing Worker-only mutation sequence: diff/path validation,
  authorization, apply, sealed-tree verification, validation, commit, push, PR,
  and `MarkPROpen`.

The current repair state persists `ModelInvocationID` but not the request SHA.
The audited intake/dispatcher reconciliation must add that binding rather than
reconstruct it.

## Legacy-runtime coexistence and activation

The legacy specialist lease at
`<stateDir>/integrated/specialists/runtime.lease` has no TTL or session
identity. PID and `AcquiredAt` are not liveness proof. The OS lock is the
authority.

The normal service must acquire and retain the exact same exclusive lock for
its entire ownership period. A probe followed by release has a restart race.
While another process holds the lock, including during malformed/partial lease
JSON initialization, the service must:

- durably audit `legacy_runtime_active` with `Allowed:false` through
  `integrated.Store.AuditStrict` and the normal dashboard audit sink;
- perform zero Visual admission, composition, mailbox preparation, Manager
  inspection/dispatch, or Worker execution; and
- leave the unrelated normal Governor, agents, and dashboard cadence running.

An unlocked lease is stale, not expired. After acquiring the lifetime fence,
unsafe or repository-mismatched lease state fails closed. The service then
reconciles in this order:

1. Load repair state by stable repository fingerprint.
2. For `StageModelRunning`, use only the persisted `ModelInvocationID` to load
   the old mailbox order. Cross-check repository, fingerprint, recurrence,
   attempt, role, base/tree, paths, validation, request SHA, order, lease,
   receipt, and proposal digests.
3. If a complete durable response exists, adopt it without Manager inspection,
   launch, or a second model call. An order/lease without a provable completion
   is held as `legacy_model_ambiguous`.
4. Search all normal routed stores by stable external ref. Reject zero/multiple
   conflicting matches and validate controller metadata before accepting an
   existing bead.
5. Project the exact sealed present work into exactly one normal routed store
   without calling lifecycle apply, outbox, or GitHub. Legacy and normal bead
   UUIDs may differ; external ref is the cross-store identity.
6. Reopen-verify a durable adoption record linking both bead IDs and all
   order/request/lease/receipt/proposal digests, update the lifecycle finding to
   the normal bead ID, and attach repair state to normal ownership.
7. Only after that proof may the legacy bead be marked migrated/retired. Never
   delete it. On any partial failure or ambiguity, durably hold both beads.

The replay defect to avoid is in `pkg/visualhive/lifecycle.go`: an existing
replay key returns before `beads.ImportBatch`. Activation therefore cannot be
implemented as another `ApplyBundle` call. Intake's sealed import plan/cache is
also required because lifecycle state alone cannot reconstruct the full v3
work and artifact receipts.

## Fetch-only normal-service driver

This remains deliberately unwired until the audited intake commit provides the
import/activation transaction boundary.

The implementation must extract, rather than duplicate, the exact production
workflow path from `pkg/integrated/run.go`:

1. Load installed `integrated.Config` and use its repository ID, repository,
   default branch, Visual Hive ref, state directory, and
   `RunIntervalSeconds`.
2. Gate on installed pause/pause request, the retained legacy-runtime fence,
   setup validity, and the exact durable production `WorkflowDispatchIntent`.
   `dispatchAndWait` must resume a correlated intent and must not redispatch it.
3. Reuse exact workflow/run/artifact discovery, then call
   `FetchAndVerifyVisualHiveBundle` with full source-artifact fetch. Reverify the
   live installed workflow head before returning.
4. Return a sealed value containing `WorkflowRunEvidence`, the validated
   bundle, and `VerifiedVisualHiveArtifact`. Do not invoke legacy lifecycle,
   beads, outbox, specialist manager, or repair.
5. Call intake `Controller.Import`, admission, activation, and durable dispatch.
   Consume the workflow intent only after the exact import/activation boundary
   reports durable success. Exact replay is a no-op; conflicts fail closed.
6. Run one background serial cycle with a per-cycle context deadline. Ticks
   coalesce while a cycle is active; failures are audited and retried on the
   next installed interval. The normal Governor loop never waits on this
   goroutine.

Tests must inject the fetch cycle/fake GitHub transport, block or fail it, and
prove multiple unrelated normal evaluations and dashboard reads still occur,
with maximum Visual concurrency of one.

## Config reload

Implemented in `00391727` and hardened in `610f6474`:

- the existing watcher calls `Governor.UpdateConfigAndAgents` with
  `cfg.EnabledAgents()`;
- Governor policy and enabled agents swap under one lock;
- removed agents lose stale cadences and can no longer be kicked; and
- construction and reload deeply clone every nested mutable `AgentConfig`
  pointer, slice, and map, including channels, tools/rules, connections/auth,
  options, and display/routing collections.

The intake admission commit must read this same locked snapshot. It must not
introduce a role registry. Pending or transiently denied work is then
re-evaluated against the new role/policy digest, while prepared work continues
only from its persisted order.

## Acceptance matrix

| Proof | Status on this branch |
| --- | --- |
| Typed mutation/test-adequacy artifact reaches normal quality prompt and mailbox replay is byte-identical | Implemented and passing |
| Unsafe path/link, size/SHA drift, binary non-embedding, and required-JSON fail-closed behavior | Implemented and passing |
| Role/executor separation and immutable governed mailbox request | Execution commits integrated; focused tests passing |
| Governor agent enable/disable/role snapshot updates on normal config reload, including nested caller mutation | Implemented and passing |
| Intake packet tamper/malformed, policy drift after prepare, pause/budget/WIP re-evaluation, and no duplicate bead/work | Blocked on audited intake commit |
| One-shot child containment and rejection of legacy completion for new governed work | Blocked on audited dispatcher commit |
| Worker request-SHA persistence, crash recovery with one model call, and one branch/PR maximum | Blocked on intake/dispatcher reconciliation |
| Live legacy lease hold, completed legacy order adoption, ambiguous hold, and cross-store activation migration | Blocked on intake sealed cache plus dispatcher recovery seam |
| Non-overlapping fetch-only ticker while unrelated normal cadence/dashboard continue | Design complete; production wiring blocked on intake transaction boundary |
| Exact `pull_request` v3 producer/verifier adversarial matrix | Owned by separate `codex/vh-pr-verifier` task; no duplicate work here |
| Full local normal-service Governor-to-PR vertical | Not run; product claim prohibited |

## Worklog and verification

- Created local worktree
  `C:\Users\david\OneDrive\Documents\vh-worktrees\hive-normal-service-integration`
  on `codex/vh-normal-service-integration` from the coordinator commit.
- Cherry-picked the ordered execution pair as `07dd2f65` and `78b3db04`.
- Added exact typed evidence preservation and bounded reading in `64b6abed`.
- Added atomic Governor config/agent refresh in `00391727` and complete nested
  snapshot isolation in `610f6474`.
- Passing focused commands:
  - `go test ./pkg/agent -count=1 -timeout 2m`
  - `go test ./pkg/scheduler -count=1 -timeout 2m`
  - `go test ./pkg/repair -run '^TestGovernedEvidenceArtifactReader' -count=1 -timeout 2m`
  - `go test ./pkg/governor -count=1 -timeout 2m`
  - `go test ./cmd/hive -run '^$' -count=1 -timeout 2m`
- A combined `go test ./pkg/scheduler ./pkg/repair -timeout 4m` run passed
  Scheduler but timed out in the unrelated existing Windows Git subprocess
  `TestSealedTreeGuardCannotBeTransplantedAcrossAttempts`. The full repair
  suite is therefore not claimed green.

Next integration is intentionally gated on audited intake and dispatcher commit
IDs. The separate PR-v3 commit is a third modular dependency.
