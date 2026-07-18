# Bridge ReceiptSync: CASE A / CASE B finality fix

Release concerns and exact partial/full rollback procedures are recorded in
[BRIDGE_CASE_A_B_RELEASE_ROLLBACK.md](BRIDGE_CASE_A_B_RELEASE_ROLLBACK.md).

## Problem (nn-002 class)

Bridge-geth has **no full EL state**. Skeleton is the only chain view used to:

1. Decide which bodies/receipts to request
2. Decide which blocks may be POSTed to the API (`block ≤ F`)

Two cursors were conflated:

| Name | Source | Meaning |
| --- | --- | --- |
| **C** | `GET /v1/bridge/block/latest` | API last confirmed block |
| **F** | skeleton `progress.Finalized` | Send/download ceiling (CL finality) |

Handshake always aims at **C+1**. Posting is only allowed for **≤ F**.

### Broken combo (pre-fix)

Layer 1 (`receiptSyncOrigin` from continuity work) clamped origin to **skeleton tip (head)**:

```text
origin = clamp(C, tail-1, latest-1)   // latest = tip, not F
download toward tip
refuse if block > F  → hard backfill failure
```

When **C ≥ F** (API ahead of local finality watermark):

```text
try C+1 > F → "Refusing to process non-finalized block" → Beacon backfilling failed → loop
```

That is **CASE A**. Restarting the bridge container alone often kept geth datadir/skeleton, so `F` stayed wedged while API `C` was unchanged.

Separately, Gonka’s custom skeleton finality path (`pendingHeaders` + `processCanonicalChain`, from initial bridge release) could leave **torn / regressing F** when CL briefly announced a high finalized and later sync cycles re-read older status.

## Intended model

```text
Prysm (CL) ──Engine API──► skeleton F advances (monotonic)
API C ──handshake──► origin = C   (cursor only; never sets F)

Send/download window:  (C, F]
  CASE A: C ≥ F  → idle until F moves
  CASE B: F > C  → re-drive C+1…F (cursor rewind; F unchanged)
```

This matches the original bridge philosophy in `BRIDGE_DETAILED_CHANGES.md` (“only process finalized/safe range”) and PR #1376 continuity (Layer 1 re-seed from C), without downloading unfinalized tip ranges.

## What we changed

### 1. Clamp ReceiptSync to F (not tip)

- `receiptSyncOrigin(latest, final)`: `origin = clamp(C, tail-1, F)`
- Sync/fetch target for ReceiptSync = **F**
- When `C ≥ F`: `origin = F` → fetch from `F+1` with target `F` → idle

### 2. Keep the importer fail-closed

- Correct scheduling prevents blocks above F from reaching the importer during normal CASE A operation
- `ensureBridgeReceiptResultFinalized` remains a hard fail-closed invariant for unexpected above-F results
- If F is unavailable, ReceiptSync waits instead of falling back to the unfinalized skeleton tip

### 3. Monotonic skeleton F

- Never persist a lower `progress.Finalized` than disk (`saveSyncStatus`)
- Only publish F after `processCanonicalChain` succeeds
- Ignore regressive finalized markers from CL
- On link gaps: do not tear sync; keep pending headers and retry
- `Bounds()` still returns F when it sits below the active subchain tip

## Log markers

Gonka downloader/catalyst logs use the **`GONKA:`** prefix (bridge package uses `Bridge:`).

| Log | Case |
| --- | --- |
| `GONKA: CASE A idle — API latest at/above skeleton finalized; waiting for F` | A |
| `GONKA: CASE B re-drive — skeleton finalized ahead of API latest; syncing (C,F]` | B |
| `GONKA: ReceiptSync waiting for skeleton finalized watermark` | No F yet |
| `GONKA: ReceiptSync mode` (`origin`, `finalized`, `head`) | both |
| `GONKA: skeleton finalized advanced` | F moved |
| `GONKA: ignoring regressive finalized marker` | F protection |
| `GONKA: finalized not advanced; will retry when skeleton links` | F deferred |
| `GONKA: ReceiptSync imported all available finalized headers, waiting` | idle at F |

## Ops notes

- **API `C` does not reset on bridge upgrade.** It only moves when new blocks are POSTed and drain confirms.
- Bridge recreate wipes Prysm DB (`--force-clear-db`) but **keeps geth datadir** → same skeleton possible after upgrade.
- If `F` is wedged far behind tip even after this fix, wipe/reseed **geth** datadir so skeleton can rebuild; then CASE A idle → F advances → CASE B-style catch-up from C.

## Upstream vs Gonka

| Behavior | Upstream geth | Gonka |
| --- | --- | --- |
| Skeleton extends head sequentially | yes | replaced with pendingHeaders + canonical-from-final |
| ReceiptSync / no state | no | yes |
| Origin from API C | no | yes (`06055a1` continuity) |
| Download ceiling = F | n/a | **this fix** (was tip before) |
| Hard refuse above F | n/a | retained as an unreachable fail-closed invariant |

The CASE A stall was **not** original Ethereum behavior; it was our continuity + finality-gate combination using tip as the download upper bound.

## Files touched

- `eth/downloader/downloader.go` — origin/target clamp, wait for initial F, CASE A/B logs
- `eth/downloader/beaconsync.go` — fetchHeaders target = F in ReceiptSync
- `eth/downloader/skeleton.go` — monotonic F, Bounds exposure, advance logging
