# Bridge Client: Technical Deep Dive & Change Log

## 1. Core Philosophy & Architectural Intent

The `bridge-geth` repository implements a specialized version of the Go Ethereum (Geth) client designed specifically for cross-chain bridging. Unlike a standard Ethereum node, the Bridge Client's primary purpose is not to validate the entire blockchain state or execute EVM transactions.

### The "Receipt-Only" Philosophy
The core idea is to create a high-performance bridge version of the Geth client that:
*   **Bypasses Validation**: It does not need to execute blocks or maintain a state trie (no need for `TrieDB` or full block validation).
*   **Rule-Based Filtering**: It retrieves block data from the "safe range" of blocks (highly finalized blocks with nearly zero chance of reorgs).
*   **Data Forwarding**: It extracts specific events (ERC20 Transfers) and forwards them to an external Bridge API.
*   **100% Proof Protection**: By only processing blocks from a finalized/safe range, it ensures the integrity of the data sent to the external chain without the overhead of local verification.

**Important:** “Safe range” means **skeleton finalized F**, not the network tip. Continuity handshake uses API cursor **C** only to choose *where to resume*; it must not pull or POST above F. See [BRIDGE_CASE_A_B_FINALITY.md](BRIDGE_CASE_A_B_FINALITY.md).

---

## 2. Key Architectural Changes

### 2.1 Receipt-Only Sync Mode (`ReceiptSync`)
Standard Geth supports `FullSync` and `SnapSync`. We introduced a third mode: `ReceiptSync`.

*   **Forced Mode**: In [eth/downloader/downloader.go](file:///Users/gliberman/Documents/GitHub/bridge-geth/eth/downloader/downloader.go), the sync mode is hardcoded to `ReceiptSync`. Peer-facing logs may still report `mode=snap` as camouflage.
*   **Simplified Pipeline**: Instead of the complex state-management loops, we use `processReceiptOnlyContent` in `eth/downloader/downloader.go`.
*   **Selective Data Fetching**: 
    *   **Headers**: Fetched to track the chain tip and verify structure.
    *   **Receipts**: The primary source of event data (Logs).
    *   **Bodies**: Crucial because we need the **Transactions** to recover the **Public Key** of the sender, which is not included in receipts alone.
*   **Ceiling**: ReceiptSync origin/target and POST gate use **F** (`skeleton` finalized). Tip is only used for head tracking / peer behavior, not as the send window.

### 2.2 The Bridge Module (`eth/bridge/`)
A dedicated package for bridge-specific logic.

*   **Dynamic Configuration**: Bridge contract addresses are not hardcoded; they are fetched from the external API via `getBridgeContractAddresses` ([eth/bridge/bridge.go:L116](file:///Users/gliberman/Documents/GitHub/bridge-geth/eth/bridge/bridge.go#L116)).
*   **Classification Engine**: The client distinguishes between:
    *   **Deposits**: Tokens sent *to* a bridge contract.
    *   **Burns (Withdrawals)**: Tokens sent *from* a bridge contract to a "burn" address ([zeroAddress](file:///Users/gliberman/Documents/GitHub/bridge-geth/eth/bridge/bridge.go#L31) or [deadAddress](file:///Users/gliberman/Documents/GitHub/bridge-geth/eth/bridge/bridge.go#L32)).
*   **Public Key Recovery**: Since the Bridge API requires the full public key of the user (to identify them on the destination chain), we use `crypto.SigToPub` in `recoverPublicKey` ([eth/bridge/bridge.go:L160](file:///Users/gliberman/Documents/GitHub/bridge-geth/eth/bridge/bridge.go#L160)) to derive the key from the transaction signature.

---

## 3. Nuanced Technical Details

### 3.1 Skeleton vs. Blockchain Head
Because the client does not "import" blocks into the canonical blockchain (as that would trigger EVM execution), the `blockchain.CurrentHeader()` often stays at Genesis or an old checkpoint. However, the `Skeleton` (used for Beacon Chain sync) tracks the real network head.
*   **Issue**: Some Geth APIs check `blockchain.CurrentHeader()` for fork timing.
*   **Nuance**: In `ReceiptSync` mode, we must rely on the `Skeleton` bounds to determine the true state of the network.
*   **Finalized watermark**: `progress.Finalized` is the send/download ceiling. It must advance with CL finality and must **not regress** when a sync cycle re-reads older status (Gonka monotonic F).

### 3.2 Dual Logic for Deposits and Burns
The logic in `classifyBridgeLog` in `eth/bridge/bridge.go` ensures that we don't just look for "bridge address in logs", but specifically verify the direction of the transfer to correctly categorize user intent (Locking vs. Unlocking tokens).

### 3.3 Continuity: API cursor C vs skeleton finalized F
Handshake (`GetLastConfirmedBlock`) returns **C**. ReceiptSync origin is derived as:

```text
origin = clamp(C, skeletonTail-1, F)
download/POST only through F
```

| Case | Condition | Behavior |
| --- | --- | --- |
| **A** | `C ≥ F` | Scheduler idles at F until finality advances; importer remains fail-closed |
| **B** | `F > C` | Re-drive `(C, F]`; F unchanged |

**Bug that was fixed:** continuity clamped origin to **tip** (`latest-1`) while still hard-refusing `block > F`. When API was ahead of a wedged/regressing F (`C ≥ F`), every cycle tried `C+1` and failed backfill (nn-002 class). Details and ops notes: [BRIDGE_CASE_A_B_FINALITY.md](BRIDGE_CASE_A_B_FINALITY.md).

**Log markers:** downloader/catalyst use `GONKA:`; bridge package uses `Bridge:`. CASE A/B emit `GONKA: CASE A …` / `GONKA: CASE B …`.

---

## 4. Analysis of Latest 7 Commits

### Commit `2250c5a`: Previous Uncommitted Changes
*   **Changes**: Small updates to [eth/backend.go](file:///Users/gliberman/Documents/GitHub/bridge-geth/eth/backend.go) and [eth/catalyst/api.go](file:///Users/gliberman/Documents/GitHub/bridge-geth/eth/catalyst/api.go).
*   **Rationale**: Maintenance and alignment of API endpoints with the backend bridge service.

### Commit `2057002`: Skip Blockchain Header Check
*   **Changes**: Modified `eth/downloader/downloader.go` and `eth/catalyst/api.go` to bypass header verification against the local DB when `ReceiptSync` is active.
*   **Rationale**: Since we don't maintain a standard blockchain state, the traditional "is this header in my DB?" checks fail. Skipping these allows the sync to proceed based on Beacon Chain finality alone.

### Commit `278a3a2` & `c1949c2`: Withdrawal/Burn Support
*   **Changes**: Major refactor of `classifyBridgeLog` in [eth/bridge/bridge.go](file:///Users/gliberman/Documents/GitHub/bridge-geth/eth/bridge/bridge.go).
*   **Rationale**: Initially, the bridge only handled deposits. These commits added detection for "burns" (sending to `0x0` or `0xdead`) from bridge-managed token contracts, enabling full bidirectional support.

### Commit `cfc1335`: Docker & Flags
*   **Changes**: Updated Dockerfiles to newer Alpine versions and added explicit CLI flags in `cmd/utils/flags.go`.
*   **Rationale**: Improving dev-ops and making bridge configuration (like API URLs) explicit via command-line arguments.

### Commit `4f8ad64`: Explicit URLs Fix
*   **Changes**: Cleaned up how `BridgePostBlockEP` and `BridgeGetAddressesEP` are handled in the config.
*   **Rationale**: Fixed a bug where default URLs might override user-provided explicit URLs.

### Commit `8c9f51e`: Initial Bridge Release
*   **Changes**: Massive addition of the `eth/bridge` package and the `ReceiptSync` mode in the downloader.
*   **Rationale**: The foundation of the project. It introduced the concept of syncing receipts without state and the recovery of public keys for cross-chain identification.

### Follow-on: CASE A / CASE B finality (see dedicated doc)
*   **Changes**: Clamp ReceiptSync origin/fetch target to skeleton **F**; idle CASE A; wait when F is unavailable; retain the hard importer guard; monotonic skeleton Finalized; `Bounds()` exposes F below subchain tip; `GONKA: CASE A/B` logs.
*   **Files**: `eth/downloader/downloader.go`, `beaconsync.go`, `skeleton.go`.
*   **Rationale**: Align implementation with the “finalized/safe range” philosophy and continuity re-drive from C. Normal CASE A idles in the scheduler; an unexpected above-F importer result still fails closed rather than being discarded.
*   **Doc**: [BRIDGE_CASE_A_B_FINALITY.md](BRIDGE_CASE_A_B_FINALITY.md).
*   **Release/rollback**: [BRIDGE_CASE_A_B_RELEASE_ROLLBACK.md](BRIDGE_CASE_A_B_RELEASE_ROLLBACK.md).
