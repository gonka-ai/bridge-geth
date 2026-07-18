# Bridge cursor, skeleton, and finality flow proposal

## Status

This document records the design discussion following the `post2` incident. It
describes the current implementation, identifies where responsibilities are
coupled incorrectly, and proposes a replacement flow.

This is a design proposal only. The proposed changes have not yet been
implemented.

## Requirements

The bridge observer needs two independent controls:

1. The bridge API cursor decides the first block that still needs delivery.
2. Prysm finality decides the highest block that may be delivered.

The required delivery interval is therefore:

```text
first = C + 1
last  = min(F, S)

deliver [first, last] strictly in ascending order
```

For a new/uninitialized API, an optional configured bootstrap block `B` may be
used:

```text
first = max(B, C+1)
```

Prefer initializing the API cursor to `B-1`, so the same `C+1` rule applies to
both bootstrap and recovery.

## Terms and ownership

| Name | Owner/source | Meaning |
| --- | --- | --- |
| `H` | Prysm forkchoice | Canonical execution head selected by the consensus layer |
| `S` | Geth skeleton | Highest contiguous execution header currently materialized in skeleton storage |
| `F` | Prysm forkchoice, verified against `S` | Highest finalized execution header |
| `C` | Bridge API | Last block confirmed by the bridge API |
| `B` | Configuration, optional | First block to observe when the API has no cursor |
| `N` | Delivery loop | Next block to deliver, normally `C+1` |

Prysm supplies chain authority (`H` and `F`). Geth peers supply most historical
header, body, and receipt data. The latest header is often already available
from Prysm's `newPayload`; peers are a fallback and fill missing ancestors.

The intended state relationship during normal operation is:

```text
C < F <= S <= H
```

`C >= F` is a valid temporary state when local finality is stale or the bridge
API is already ahead. It must result in waiting, not sending or process failure.

## How the current implementation works

### 1. Live headers enter through Prysm

Prysm sends an execution payload through Engine API `newPayload`. When the
parent is not available in the local full blockchain, Catalyst:

```text
newPayload(block N)
  -> store header N in Catalyst remoteBlocks
  -> BeaconExtend(header N)
  -> skeleton.processNewHead(N, final=nil)
  -> pendingHeaders[N] = header N
```

The customized `processNewHead` does not advance persisted skeleton head `S`
for a head-only update. It keeps the header in memory until a usable finalized
header is received.

`pendingHeaders`, `lastFinalHead`, and `processCanonicalChain` were introduced
by the initial bridge commit `8c9f51e66`, not by the recent `post2` changes.

### 2. Historical skeleton data comes from Geth peers

Prysm selects `H`. Skeleton sync uses that target and requests missing ancestor
headers from Geth devp2p peers:

```text
H -> H-1 -> H-2 -> ... -> known ancestor
```

Parent hashes connect peer-provided headers to the Prysm-selected target. Bodies
and receipts are also downloaded from Geth peers.

### 3. Finalized resolution depends on a short Catalyst cache

For an unknown local forkchoice head, Catalyst currently resolves:

```text
head H:
  remoteBlocks, then Downloader.GetHeader(H-hash) peer fallback

finalized F:
  remoteBlocks only
```

`remoteBlocks` retains 96 headers. After a restart or rapid Prysm catch-up, the
finalized header can be much older than that window. Catalyst then calls:

```text
skeleton.processNewHead(H, final=nil)
```

and logs `finalized=unknown`, even though Prysm supplied a nonzero finalized
hash.

### 4. Finality currently advances skeleton head

Once Prysm catches up, its finalized header is normally about two epochs behind
the head and becomes available inside the 96-header cache. Catalyst passes the
header to skeleton:

```text
processNewHead(H, F)
  -> walk backward from F through pendingHeaders
  -> connect to existing skeleton head S
  -> write pending headers S+1 ... F
  -> advance skeleton head S to F
  -> publish finalized pointer F
```

This makes skeleton head progression depend on finality. Recent live headers
above `F` remain in `pendingHeaders` instead of advancing persisted `S` normally.

The finalized pointer may jump over a large already-materialized portion of the
skeleton. This is not itself a missing-header jump. In the incident:

```text
before:
  S = 25,555,994
  F = 25,544,388

pending headers committed:
  25,555,995 ... 25,556,004

after:
  S = 25,556,004   (+10)
  F = 25,556,004   (+11,616 marker movement)
```

Headers between the old `F` and old `S` were already present. The architectural
problem is not that `F` moved in one assignment; it is that normal `S`
progression was coupled to finalized updates.

### 5. ReceiptSync uses C as an origin and F as a ceiling

The recent `post2` changes correctly made skeleton finalized `F` the
ReceiptSync download/send ceiling:

```text
CASE A: C >= F
  origin = F
  target = F
  wait

CASE B: C < F
  origin approximately C
  target = F
  process toward F
```

The importer also rejects a result when:

```text
block > F
or
downloaded block hash != skeleton block hash
```

These fail-closed checks are correct and should remain.

However, CASE A currently starts the downloader at old `F`, not at `C`. If `F`
later advances during the same cycle, Geth may download blocks already covered
by the API. `CheckContinuityAndSend` skips those blocks before POSTing, but the
extra download is unnecessary and makes the start point indirect.

### 6. Current bridge delivery failure path

For each downloader result batch, `CheckContinuityAndSend` reads API cursor `C`.
If the API is partially ahead of the batch, it POSTs only blocks greater than
`C`.

On a POST failure, the importer currently logs the error but returns success:

```text
POST N fails
  -> log warning
  -> consume failed downloader batch
  -> do not cache the failed range
  -> request the next result batch
```

The next batch sees `C` behind its expected predecessor. Because the failed
range was never saved to the in-memory cache, continuity recovery cannot cover
the gap and calls `log.Crit`, killing Geth.

The incident followed this exact sequence:

```text
C = 25,544,396
F advances to 25,556,004

POST 25,544,397
  -> 30 second timeout

failed batch is consumed
next batch starts at 25,544,402
expected predecessor is 25,544,401
API remains at 25,544,396
cache does not contain 25,544,397 ... 25,544,401
  -> log.Crit
  -> Geth restart
```

The in-memory cache cannot recover a range that failed before it was cached.
It also disappears on process restart, so it cannot be a correctness boundary.

### 7. Cursor-unavailable behavior is not strict enough

If the cursor endpoint fails or reports no initialized cursor, the current code
can fall back to a finalized-tip origin and legacy direct posting. That loses
control of the required start block.

If `C` is the authoritative delivery checkpoint, failure to read `C` must pause
delivery. An explicit configured bootstrap block is the only safe alternative
for a genuinely uninitialized API.

## What is wrong in the current design

The current implementation combines three independent state machines:

1. Skeleton chain availability (`S`).
2. Consensus finality (`F`).
3. Bridge delivery acknowledgement (`C`).

The concrete problems are:

- `pendingHeaders/processCanonicalChain` makes `S` advance only when `F` is
  usable.
- Catalyst finalized lookup depends on a 96-header memory window.
- CASE A uses old `F` as the downloader origin instead of retaining `C+1` as
  the desired next block.
- Failure to read `C` can fall back to direct posting instead of waiting.
- A POST error is swallowed, so the failed range is consumed.
- The next batch relies on an in-memory cache that cannot contain the failed
  range.
- A bridge API timeout becomes `log.Crit` and kills Geth, disrupting Prysm's
  Engine API connection without repairing the API.
- A large `(C,F]` backlog is scheduled aggressively even when the first POST is
  blocked.

## Proposed flow

### 1. Restore independent, sequential skeleton progression

Restore original Geth `processNewHead` behavior for skeleton head updates:

```text
receive head N
  -> require N == S+1 for a normal extension
  -> verify header N parent hash matches header S
  -> persist header N
  -> set S = N
```

Use original skeleton reorg/restart handling for reannouncements, gaps, and
forks. Peer-backed reverse sync continues to fill missing historical headers.

Normal skeleton progression must not wait for finality. Remove
`pendingHeaders`, `lastFinalHead`, `processCanonicalChain`, and
`cleanupProcessedHeaders` once equivalent upstream head handling is restored.

### 2. Track F as a pointer into S

Prysm remains the authority for the finalized hash. Resolve that hash through:

```text
1. Catalyst remoteBlocks
2. skeleton/local header data when available
3. Downloader.GetHeader(F-hash) peer fallback when necessary
```

Before publishing a new `F`, require:

```text
new F >= old F
finalHeader.Number <= S
skeleton.Header(finalHeader.Number) exists
skeleton.Header(finalHeader.Number).Hash == finalized hash from Prysm
```

If skeleton has not connected the finalized target yet, retain the target and
retry after skeleton progresses. Do not regress `F` and do not move `S` merely
to publish `F`.

`F` may jump across already-stored headers because it is a marker, not a second
header-download cursor.

### 3. Make C the only delivery start cursor

Define:

```text
N = C + 1
upper = min(F, S)
```

Then:

```text
if N > upper:
  wait

if N <= upper:
  download and deliver N ... upper
```

For initial bootstrap, either initialize API `C` to `B-1` or use:

```text
N = max(B, C+1)
```

If API `C` cannot be read, pause delivery and retry. Do not fall back to direct
posting.

### 4. Preserve finalized and canonicality gates

Before a block can enter the bridge delivery path, require:

```text
N == C + 1
N <= F
N <= S
skeleton.Header(N) exists
skeleton.Header(N).Hash == downloadedHeader.Hash
```

Keep the recent ReceiptSync target `F`, monotonic `F`, and hard importer
finality/hash checks.

### 5. Make delivery acknowledgement-driven

Do not consume later downloader results after an unconfirmed POST.

```text
POST N
  -> success:
       confirm or re-read API C
       continue from new C+1

  -> timeout/error:
       stop consuming results
       re-read API C

       if C >= N:
         the request was committed; continue from C+1

       if C < N:
         retry N with bounded backoff
```

The POST endpoint must be idempotent by block number/hash, because a timeout can
occur after the server commits but before Geth receives the response.

Never call `log.Crit` for an API timeout or an API cursor gap. If a required
header is no longer retained locally, restart/reseed the downloader explicitly;
do not kill the entire process as an implicit retry mechanism.

### 6. Apply backpressure with bounded batches

Do not schedule the entire `(C,F]` backlog while the first POST is outstanding.
Use a bounded window:

```text
batchStart = C + 1
batchEnd   = min(F, S, batchStart+batchSize-1)

download -> verify -> filter -> POST -> confirm
then open the next window
```

This keeps memory bounded and makes API throughput control downloader progress.
The API cursor is the durable checkpoint; the recent-range memory cache becomes
an optional optimization rather than a correctness requirement.

## Current versus proposed responsibilities

| Responsibility | Current | Proposed |
| --- | --- | --- |
| Select canonical head | Prysm | Prysm |
| Fill skeleton history | Geth peers | Geth peers |
| Advance live skeleton head | Pending Prysm headers committed at finality | Original sequential skeleton handling |
| Resolve finalized header | 96-header Catalyst cache only | Cache, skeleton/local lookup, then hash-based peer fallback |
| Advance finalized marker | Coupled to `processCanonicalChain` and `S` | Independent monotonic pointer verified inside `S` |
| Select first bridge block | Indirect origin clamp plus per-batch handshake | Exactly `C+1` or configured bootstrap `B` |
| Select last bridge block | `F` after post2 | `min(F,S)` |
| Handle missing cursor | Finalized-tip/direct fallback possible | Pause unless explicit bootstrap is configured |
| Handle POST failure | Consume batch, then cache gap may kill Geth | Stop, re-read `C`, retry same block/range |
| Handle backlog | Schedule aggressively toward `F` | Bounded acknowledgement-driven windows |

## Safety and liveness invariants

The replacement must maintain all of the following:

- Never POST a block above Prysm-derived, skeleton-verified `F`.
- Never POST a block whose hash differs from the canonical skeleton header.
- Never intentionally POST `N+1` while API `C < N`.
- Never infer or advance `C` solely from local memory; API acknowledgement is
  authoritative.
- Never use missing `C` as permission for legacy direct posting.
- Never regress persisted `F`.
- Never make normal skeleton head progression depend on bridge API health.
- Never kill Geth solely because the bridge API timed out.
- Preserve enough state/logging to distinguish a successful-but-timed-out POST
  from a rejected POST.

## Implementation proposal

Implement the replacement as separate reviewable commits.

### Commit 1: restore skeleton head/finality separation

Files:

- `eth/downloader/skeleton.go`
- `eth/downloader/skeleton_test.go`

Changes:

- Restore upstream sequential `processNewHead` behavior.
- Remove `pendingHeaders`, `lastFinalHead`, `processCanonicalChain`, and cleanup
  logic after equivalent upstream gap/reorg behavior is restored.
- Keep monotonic persisted `F`.
- Update `F` only when its header is present in and hash-matches skeleton.
- Keep `Bounds()` capable of exposing a valid retained `F`.
- Add tests showing `S` advances on head-only events while `F` remains unchanged.
- Add tests showing `F` can jump across an existing contiguous `S` without
  changing `S`.

### Commit 2: make finalized-header resolution robust

Files:

- `eth/catalyst/api.go`
- Catalyst/downloader tests

Changes:

- Resolve a nonzero finalized hash beyond the 96-header `remoteBlocks` window.
- Fetch an unknown finalized header by hash when necessary.
- Verify the resolved header against the connected skeleton before advancing
  `F`.
- Preserve `STATUS_SYNCING` while the finalized target is not connected.

### Commit 3: make C authoritative and fail closed

Files:

- `eth/downloader/downloader.go`
- `eth/downloader/beaconsync.go`
- `eth/downloader/receipt_finality_test.go`

Changes:

- Represent the desired next block as `C+1`, including CASE A waiting.
- Keep the dynamic ReceiptSync ceiling at `F`.
- If `C` is unavailable, pause instead of falling back to `F-1` or direct
  posting.
- Add explicit bootstrap block handling if an uninitialized API must be
  supported.
- Preserve importer `N <= F` and canonical-hash guards.

### Commit 4: make bridge delivery acknowledgement-driven

Files:

- `eth/bridge/bridge.go`
- `eth/bridge/*_test.go`
- `eth/downloader/downloader.go`

Changes:

- Propagate POST failures instead of logging and returning success.
- Do not consume later result ranges after an unconfirmed POST.
- On timeout, re-read `C` before deciding whether to retry.
- Retry the same block/range with bounded backoff when `C` did not advance.
- Remove `log.Crit` from normal continuity recovery.
- Treat the recent-range cache as optional acceleration only.
- Add tests for success, definite failure, ambiguous timeout with advanced `C`,
  ambiguous timeout with unchanged `C`, process restart, and cache eviction.

### Commit 5: bound the catch-up window

Files:

- `eth/downloader/downloader.go`
- `eth/downloader/beaconsync.go`
- queue/result-store tests

Changes:

- Limit scheduled work to a configurable or fixed small finalized window above
  `C`.
- Open the next window only after the API confirms progress.
- Verify that a large `F-C` gap does not fill the 8,192-result store or create
  duplicate concurrent fetch work.

### Acceptance sequence

Validate the following state transitions before release:

```text
1. S advances on every connected head while F remains unchanged.
2. F advances independently after Prysm finality is verified inside S.
3. C >= F causes a stable wait with no download above F.
4. F > C sends exactly C+1 ... F in order.
5. A POST timeout never advances to the next unconfirmed block.
6. Re-reading an advanced C after timeout avoids an unnecessary duplicate.
7. Re-reading an unchanged C retries the same block without killing Geth.
8. Restart resumes from API C without relying on an in-memory cache.
9. No block above F or with a noncanonical hash reaches the API.
10. Prysm remains connected while the bridge API is unavailable.
```

Do not remove the post2 finalized ceiling or importer guards to regain
liveness. Restore liveness by separating `S`, `F`, and `C`, and by making API
delivery retryable and acknowledgement-driven.
