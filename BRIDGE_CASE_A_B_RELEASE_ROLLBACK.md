# Bridge CASE A/B release and rollback playbook

## Purpose

This document records the risk boundary and rollback plan for the CASE A/B
finality changes planned for the `post2` release.

Both parts are intended to ship together:

1. ReceiptSync scheduling uses skeleton finalized **F**, not skeleton head **H**.
2. Skeleton finalized persistence is monotonic and remains visible when retained
   below the active subchain tail.

They should be committed separately before release so either risk boundary can
be reviewed and reverted independently.

## Safety invariants

These invariants must remain true during any partial or full rollback:

- Never POST a block above skeleton finalized F.
- Never remove `ensureBridgeReceiptResultFinalized` without replacing it with an
  equally strong finality and canonical-hash check.
- API cursor C chooses the next bridge block; C must never set or advance F.
- If F is missing, ReceiptSync must wait or fail closed. It must not fall back to H.
- A downloaded above-F result must return an error. It must not be consumed and
  silently discarded.
- Preserve the geth datadir for diagnosis before wiping or reseeding it.

## Release components

### Component A: ReceiptSync finalized window

Expected commit title:

```text
fix: clamp ReceiptSync to finalized window
```

Files and behavior:

- `eth/downloader/downloader.go`
  - waits for an initial F;
  - computes `origin = clamp(C, skeletonTail-1, F)`;
  - idles CASE A when `C >= F`;
  - retains the hard importer finality/canonicality guard.
- `eth/downloader/beaconsync.go`
  - uses F as the dynamic ReceiptSync fetch target;
  - fails closed if F unexpectedly becomes unavailable;
  - leaves FullSync and SnapSync targeting H.
- `eth/downloader/receipt_finality_test.go`
  - covers origin clamping, finalized targeting, and importer rejection.

Release commit: `<record commit hash before post2>`

### Component B: monotonic skeleton finality

Expected commit title:

```text
fix: preserve monotonic skeleton finality
```

Files and behavior:

- `eth/downloader/skeleton.go`
  - never persists a lower F than the value already on disk;
  - ignores regressive CL finalized markers;
  - publishes F only after canonical-chain processing succeeds;
  - retains pending headers and retries link gaps;
  - returns a retained finalized header even when F is below the active tail.
- `eth/downloader/receipt_finality_test.go`
  - covers monotonic persistence and retained F below tail.

Release commit: `<record commit hash before post2>`

## Expected runtime sequence

Given `C=100`, `F=90`, and `H=120`:

```text
ReceiptSync origin = 90
ReceiptSync target = 90
CASE A idles; skeleton continues receiving Engine API updates
F advances to 95; already-confirmed 91..95 are skipped by API continuity
F advances to 105; only 101..105 are POSTed
```

Expected log markers:

- `GONKA: CASE A idle`
- `GONKA: skeleton finalized advanced`
- `GONKA: ReceiptSync imported all available finalized headers, waiting`
- `GONKA: CASE B re-drive`
- `Bridge: API is fully ahead of segment, skipping`

## Concerns to monitor

### Shared skeleton blast radius

`skeleton.go` is shared by beacon FullSync, SnapSync, and ReceiptSync. Component B
is therefore broader than Component A even though bridge-geth normally forces
ReceiptSync.

Watch for:

- repeated `finalized not advanced; will retry when skeleton links` without a
  later `skeleton finalized advanced`;
- unexpected skeleton reorg/restart loops;
- missing skeleton head, tail, or finalized header errors;
- F remaining unchanged while the CL finalized checkpoint advances;
- non-ReceiptSync testing or tooling behaving differently.

### Fail-closed waiting

When no F is available, ReceiptSync now waits instead of downloading toward H.
This intentionally prefers bridge liveness loss over unsafe posting.

Watch for:

- `ReceiptSync waiting for skeleton finalized watermark` persisting after the CL
  has a finalized checkpoint;
- `receipt sync lost skeleton finalized watermark` after ReceiptSync already ran;
- a retained numeric F whose header is missing from the skeleton database.

### Existing customized skeleton baseline

The initial bridge release (`8c9f51e66`) replaced upstream sequential head
extension with `pendingHeaders` plus `processCanonicalChain`. Some upstream
skeleton/downloader tests already fail on the pre-post2 branch. Do not attribute
those baseline failures to post2 without reproducing against the pre-post2 commit.

## Rollback decision matrix

| Symptom | First action | Code rollback |
| --- | --- | --- |
| CASE A idles and F later advances | None; expected | None |
| F does not advance but CL is finalized | Preserve logs/datadir; compare skeleton H/F and CL checkpoint | Consider Component B only |
| FullSync/SnapSync or skeleton reorg behavior regresses | Reproduce against pre-post2 commit | Revert Component B |
| ReceiptSync schedules or attempts to POST above F | Stop bridge observer immediately | Revert full post2 release; do not remove only the guard |
| Above-F importer rejection appears | Preserve the error and inspect why the scheduler crossed F | Do not weaken the guard; revert Component A only as part of full release rollback |
| ReceiptSync waits forever because F/header is missing | Snapshot datadir, verify CL F, inspect `Bounds()` state | Prefer Component B rollback or geth reseed; do not fall back to H |
| API C is below retained skeleton tail | Stop restart loop and preserve state | Operational reseed is expected; code rollback does not recreate missing history |

## Partial rollback: Component B only

Use this when shared skeleton behavior is the suspected regression but the
ReceiptSync C/F scheduler is behaving correctly.

After the release commits exist:

```text
git revert <component-B-commit>
```

Expected result:

- Component A continues to cap ReceiptSync at whatever F `Bounds()` exposes.
- The hard importer guard remains active.
- Monotonic-F and below-tail visibility protections are removed.
- A restart may again expose an older or temporarily unavailable F, causing
  ReceiptSync to wait. This is safe but may require a geth datadir reseed.

Validate after rollback:

- no block above F is scheduled or POSTed;
- CASE A waits instead of entering a refusal loop;
- F from `Bounds()` matches the persisted skeleton state;
- CL finality eventually becomes visible to ReceiptSync.

## Full rollback: Components B and A

Use a full rollback only if post2 cannot operate safely. Revert in reverse order:

```text
git revert <component-B-commit>
git revert <component-A-commit>
```

Important: the pre-post2 code can reproduce CASE A because it downloads toward H
while the importer rejects blocks above F. Therefore, before starting the rolled
back binary, do one of the following:

- keep the bridge observer stopped; or
- restore the entire previously deployed binary/configuration as a unit; or
- ensure API C, skeleton retained history, and F are aligned through an approved
  operational reseed.

Do not selectively remove the finality guard to make the old downloader run.
That would restore liveness by permitting unfinalized bridge observations.

## Operational recovery without code rollback

If code is correct but the local skeleton is irrecoverably stale:

1. Stop bridge-geth.
2. Save logs and a copy/snapshot of the geth datadir.
3. Record API cursor C, skeleton H/F/tail, and the CL finalized checkpoint.
4. Wipe/reseed only the geth datadir using the approved deployment procedure.
5. Keep API C unchanged.
6. Start bridge-geth and verify it waits for F, skips blocks already covered by C,
   and POSTs only `(C,F]` once F advances beyond C.

Restarting only the bridge container may not rebuild the skeleton when the geth
datadir is mounted persistently.

## Pre-release validation

Run:

```text
go test ./eth/downloader -run '^(TestClampReceiptSyncOrigin|TestBeaconHeaderTarget|TestEnsureBridgeReceiptResultFinalized|TestSaveSyncStatusPreservesMonotonicFinalized|TestBoundsReturnsRetainedFinalizedBelowTail)$' -count=1
go test ./eth/bridge -count=1
git diff --check
```

Before deploying post2, record:

- Component A commit hash;
- Component B commit hash;
- deployed image tag/digest;
- pre-upgrade API C;
- pre-upgrade skeleton H/F/tail;
- pre-upgrade CL finalized checkpoint;
- geth and Prysm datadir persistence/recreation behavior.

## Post-release acceptance

The release is accepted when all of the following are observed:

- no POST occurs above F;
- F never regresses across repeated Engine API updates or geth restart;
- CASE A does not create a backfill failure loop;
- skeleton continues advancing while ReceiptSync is idle;
- CASE B sends only the missing range above C;
- API continuity skips already-confirmed blocks;
- no new FullSync/SnapSync regression is attributable to Component B.
