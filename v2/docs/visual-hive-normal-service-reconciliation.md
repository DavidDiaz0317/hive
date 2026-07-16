# Visual Hive normal-service reconciliation

Status: fork-local working vertical through one Worker-owned pull request,
2026-07-16. Production activation is deliberately held at the exact-head
verdict boundary until the producer isolation and verifier requirements below
are implemented and reviewed.

The normative product contract is
`docs/visual-hive-integration-contract.md`. This document records the actual
implementation and remaining proof; it does not grant additional authority.

## Safety and source boundary

All implementation and tests in this checkpoint ran only in the isolated Hive
fork worktree:

`C:\Users\david\OneDrive\Documents\vh-worktrees\hive-normal-service-integration`

Branch: `codex/vh-normal-service-integration`.

No upstream/real Hive or KubeStellar Console checkout, remote, workflow, issue,
pull request, or production state was changed. The immutable Visual Hive
producer reference currently under review is
`97bd5cc5d0d1e37405d2a91cb1995d4fac0c676a` (tree prefix `e53af25b`) in the separate
`vis-proof-harness` worktree. It is a producer input, not a Hive commit, and
must not be cherry-picked into Hive.

## Product topology

There is no Visual-specific manager, role registry, queue, dashboard, wiki, or
graph. The normal service is a serial reconciler around existing Hive owners:

```text
existing production workflow intent and verified bundle transport
    -> native Visual Hive intake Controller
    -> existing Governor admission and normal routed bead store
    -> existing Scheduler (role policy + project + knowledge)
    -> existing SpecialistMailbox: one content-derived swo-*
    -> existing ordinary agent.Manager facade
    -> isolated one-shot Codex proposal child
    -> existing repair.Worker
    -> one branch/commit/pull request, never merge
    -> exact-head Visual Hive verifier [P0 HELD]
    -> receipt-only controller completion, never resolution
```

Visual Hive owns deterministic findings and verdict evidence. Hive owns
admission, policy, work state, proposal dispatch, patch validation, Git,
GitHub, lifecycle, and audit. The one-shot proposal child owns none of those
capabilities.

## Implemented path

### Service ownership and transport

- `integrated.AcquireNormalVisualWorkLease` retains the same OS-backed
  production-run lock used by the legacy integrated runtime for the complete
  service lifetime. The legacy and normal runtimes therefore cannot consume
  the same workflow concurrently.
- Lease contention is an idle retry in a dedicated goroutine. The normal
  Governor loop, persistent agents, and dashboard remain independent.
- `integrated.FetchNormalVisualWork` reuses installed setup verification,
  exact correlated workflow dispatch/resume, job/run/artifact verification,
  full v3 source fetch, producer pin, and live installed-workflow head check.
  It calls no legacy lifecycle, manager, repair, baseline, or merge path.
- `integrated.ConsumeNormalVisualWork` deletes only the exact durable workflow
  intent. An already-absent intent is accepted only after the service has
  durably recorded that consumption began.

### Intake, Governor, and Scheduler

- `visualhive/controller.Controller.Import` is the only bundle-to-Hive intake.
  It routes to existing normal role stores and calls the existing Governor.
- `BuildSchedulerAdmittedWork` is the only intake-envelope projection into
  `scheduler.AdmittedWork`. It preserves canonical work, packet, finding,
  evidence receipts, exact base/tree, recurrence, role, paths, contracts,
  validation, and deadline. It independently rejects valid JSON receipt bytes
  that differ from the exact receipt carried by the evidence identity.
- The existing Scheduler composes the governed request with the current normal
  role snapshot, project context, knowledge primer, capability/policy digests,
  and a separately injected contained-Codex executor profile.
- Worker owns the source composer. It reads the admitted exact Git tree through
  Git object IDs and reads only receipt-bound bounded JSON below the verified
  evidence root. The child receives bytes, not a checkout or evidence path.
- `PrepareGoverned` creates one canonical `swo-<request-sha256>`. Intake
  durably reserves that exact ID and digest before a fresh model launch.

### Existing Manager and Worker

- `configureNormalVisualWorkRunner` installs the contained child dispatcher on
  the already existing ordinary `agent.Manager`; it does not construct another
  Manager.
- The proposal executor is one-shot Codex in a private contained directory.
  Normal role backend, launch command, tools, connections, and persistent pane
  remain unchanged and inert with respect to the child.
- `repair.Worker` remains the sole component allowed to validate/apply the
  diff, re-run installed argv commands, commit, push, and create/reuse a pull
  request.
- The Worker policy is forced to `repair-pr` even when the installed repository
  is configured for `auto-merge`. This vertical cannot merge.

## Revalidation boundaries

| Boundary | Current state rechecked |
| --- | --- |
| Fetch | Installed repository identity, setup, pause request, workflow/ref/producer |
| Intake/admission | Governor mode, role enablement/config, budget, WIP, pause, installed policy |
| Scheduler preparation | Exact intake envelope, Worker base/tree, contained executor readiness |
| `swo-*` reservation | Current Governor/role/budget/WIP/pause/policy and immutable envelope |
| Fresh child launch | Same current controller checks immediately before dispatch |
| Worker side effects | Dynamic installed policy plus controller guard before apply, validation, commit, push, and PR |
| Exact-head verdict | Exact order, request, PR, head, and current controller policy immediately before verifier |
| Receipt completion | Exact open Worker PR and exact-head receipt; no merge/baseline/resolution authority |

Already leased work is not redispatched when a later fresh-launch guard denies.
Recovery observes only the exact persisted order, lease, Manager completion
spool, receipt, and proposal. This prevents a pause or policy change from
turning an ambiguous model call into a second call while still denying new side
effects.

The contained provider executable/configuration is fixed for the process
lifetime because the existing Manager dispatcher cannot be safely hot-swapped.
A change requires a controlled Hive restart. Normal role, Governor, ACMM, and
installed repository policy continue to reload through their existing owners.

## Crash and replay model

The service ledger is
`<state-dir>/visual-hive/normal-service/active.json`. It is a small exact-binding
checkpoint, not a queue. It records the workflow, packet digest, source ref,
the exact selected source ref, every unselected launchable source ref and its
controller-owned deferral checkpoint, one work-order/request identity, Worker
PR, verdict receipt, completion, and intent-consumption checkpoints using
durable atomic replacement. Canonical
verdict JSON is stored as opaque encoded bytes, never re-indented, and its
SHA-256 is rechecked after every disk load. The complete ledger state machine
is validated before fetch, import, Worker, verifier, completion, or consume.

Ordering is:

1. bind exact workflow and packet;
2. import once and persist the controller-owned source ref;
3. prepare/reserve one `swo-*` and let Worker create/recover one PR;
4. persist the exact-head verdict receipt;
5. atomically close the routed bead with the exact completion receipt;
6. persist `consume_started`;
7. consume the exact workflow intent;
8. persist `consumed` and clear on the next cadence.

After the source ref is persisted, restart reopens and revalidates the
controller-owned dispatch. It does not refetch or reimport the artifact.
Worker response loss may re-enter `Worker.Run`, but Worker/mailbox state returns
the same durable order/proposal/PR; it cannot create a second model side effect.
Controller completion is exact-byte idempotent, including the crash window
after bead close but before the service saved `completion_recorded`.

## Exact-head verdict: deliberate P0 hold

The production `PullRequestVerdictVerifier` is intentionally `nil`. The one
Worker PR remains open and the source workflow intent remains unconsumed until
all of the following are true:

1. The PR evidence producer runs under a distinct evidence UID and writes to a
   protected evidence root that the untrusted target service cannot modify.
   The current shared `hive-target` UID allows forged evidence before root
   sealing and is not acceptable.
2. The producer emits an authenticated/cryptographically bound root receipt,
   and a hostile target-service overwrite test proves pre-seal and post-seal
   forgery fails.
3. The Hive verifier accepts only independently knowable repository, PR,
   exact head/base, workflow, artifact-name, and pinned producer facts. It
   discovers the exact run attempt and artifact IDs through authenticated
   GitHub metadata.
4. Internal workflow/plan/report/config/changed/contracts/scopes/runtime/
   execution/baseline digests are derived inside the verifier as untrusted
   artifact claims and cross-bound to the index, exact head Git blobs, and root
   receipt. A caller must never fabricate or pre-parse these pins.
5. The adapter uses the reviewed
   `FetchAndVerifyVisualHivePullRequestBundle` primitive and applies evidence
   with `verified.ApplyCheckEvidence(store, fingerprint)`. It must not call the
   generic `MarkChecksWithEvidence`, add a second poller, merge, update a
   baseline, or mark a finding resolved.

Both red and green deterministic receipts are recorded as observations. A
green receipt does not itself grant merge or lifecycle-resolution authority.

## Fake/no-GitHub proof currently passing

- lifetime ownership contention performs no fetch/import/Worker work;
- a no-dispatch pause/WIP/green path runs no proposal or PR;
- crash after one Worker PR side effect recovers through the same controller
  dispatch without another fetch/import or side effect;
- crash after verdict persistence performs no second fetch, import, proposal,
  PR, or verdict;
- ambiguous workflow-intent consumption produces one deletion side effect,
  including the no-dispatch path without starting another workflow;
- missing exact-head verifier leaves the one PR open and unconsumed;
- identical controller completion replay succeeds and an altered receipt fails;
- malformed workflow/order/PR/verdict/consume ledger transitions fail before
  any source, intake, Worker, or verifier call;
- Scheduler composition produces and reserves one canonical `swo-*`;
- fresh-launch pause denial occurs before model dispatch;
- exact leased recovery performs no recomposition, second reservation,
  readiness restart, guard rerun, or redispatch;
- source composition uses a real temporary Git repository and returns bytes
  from the sealed tree even after the checkout changes.

Existing controller tests additionally cover live pause, automation downgrade,
role disable/re-enable, nested role-capability drift, Governor mode/cadence
drift, ACMM drift, installed path-policy drift, manual review, expiry, WIP,
budget, runtime-config reload, immutable-envelope tamper, and terminal WIP
retirement.

## Current one-dispatch scope boundary

The runnable vertical selects the first launchable controller dispatch (ordered
by source ref), durably marks every other launchable peer as deferred on its
existing routed role bead with the exact packet, selected source ref, workflow
correlation, and reason, then creates or recovers one specialist work order and
one Worker PR. Only after that PR's exact-head verdict is recorded does it
consume the source workflow intent. Import still admits and represents every
finding through existing lifecycle and bead owners; packet consumption does
not delete deferred beads. Every imported bead also stores the explicit
`unavailable_no_verified_facts` keyword state instead of fabricating or writing
knowledge; a later selected specialist still reads the existing Scheduler
primer. An exact same-packet replay keeps those peers deferred, while a later
verified packet may re-admit them through the Governor. The implementation
therefore preserves a multi-finding packet without claiming multi-PR processing.

That limit is acceptable for the one-defect disposable-repository proof and the
one-defect Console-fork P0 scenario. General packet fan-out needs a later
controller-owned selection/completion contract and a ledger that binds every
selected dispatch without becoming another queue or Manager. It is not safe to
add an ad hoc service loop that consumes the packet after only some work or that
bypasses the existing Governor, Scheduler, Manager, mailbox, or Worker owners.

Focused commands passing at this checkpoint:

```text
go test ./pkg/visualhive/normalservice -count=1
go test ./pkg/visualhive/controller
go test ./pkg/repair -run '^TestGovernedSourceComposerBindsSchedulerToWorkerSealedTree$' -count=1
go test ./pkg/repair -run '^(TestSpecialistProviderProposalIsBrokeredAndCompletedReplayDoesNotRedispatch|TestGovernedSchedulerCompositionReservesOnceAndLeasedRecoveryDoesNotRecompose)$' -count=1
go test -run '^$' ./cmd/hive ./pkg/repair ./pkg/visualhive/controller ./pkg/visualhive/normalservice ./pkg/integrated
```

A broader source-context selection run exceeded its 180-second Windows timeout
without producing a test failure. It is not claimed as a passing full repair
suite.

## Remaining path to a working demo

1. Cherry-pick the independently reviewed Hive verifier commit only after both
   P0 producer isolation and callable-verifier requirements pass.
2. Add the narrow production verdict adapter and adversarial exact-head tests.
3. Run all bounded local Hive gates and a fake end-to-end normal-service test.
4. Use a disposable fork/private real-code repository with `repair-pr`, a
   dedicated state root/dashboard port, reviewed healthy baseline, and no
   merge. Record exact SHAs, run/artifact IDs, admission, `swo-*`, Worker PR,
   verdict receipt, replay counts, and unrelated normal cadence.
5. Only after that succeeds, repeat against a KubeStellar Console fork with a
   dedicated namespaced Hive built from this Hive fork. Preserve Console's
   Auto-QA, test generation, visual regression, trust workflows, existing
   checks, and production Hive. Leave every demo PR unmerged.

Release packaging, new roles, new dashboards, baseline automation, broad tool
creation, direct Visual Hive writes, and Console `kc-agent`/MCP integration are
not on this critical path.

## Checkpoint ledger

| Commit | Result |
| --- | --- |
| `15a4560b` | atomic runtime-config compatibility base |
| `39527e5c` | native Visual Hive intake foundation |
| `f34a9085` | governed contained Codex dispatcher hardening |
| `65bd48f6` | normal-service fetch/intake/Scheduler/Manager/Worker vertical |
| `c8058fbe` | service replay/lease/idle fake proof |
| `a89e22d2` | exact completion replay and sealed-tree binding |
| `0a5f95ab` | one reservation/fresh guard/leased recovery proof |
| `102c4d94` | controller-owned resume without refetch/reimport |
| `1d15ab47` | crash-safe no-dispatch workflow consumption |
| `630d5fad` | exact verdict-byte persistence and ledger state validation |
| `55e704ff` | lossless Scheduler projection and exact receipt cross-binding |

These commits are checkpoints in the isolated fork branch, not release or
upstream claims.
