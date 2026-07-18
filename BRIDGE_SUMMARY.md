# Bridge Client Summary

## Overview
This document summarizes the modifications made to the original Geth client to transform it into a specialized "Bridge Client". The goal is to track specific events on the Ethereum chain without maintaining full state, for use with an external Cosmos chain.

## Key Changes

### 1. New Bridge Module (`eth/bridge/`)
*   **Purpose**: Filters and forwards specific ERC20 transfer events to an external Bridge API.
*   **Key Logic**:
    *   **Configuration**: Fetches a list of bridge contract addresses from an external API (`BridgeGetAddressesEP`).
    *   **Filtering**: Scans block receipts for `Transfer` events involving these contracts.
    *   **Classification**: Distinguishes between:
        *   **Deposits**: Transfers *to* a bridge contract.
        *   **Withdrawals (Burns)**: Transfers *from* a bridge contract to a "burn" address (Zero Address or `0x...dEaD`).
    *   **Public Key Recovery**: Recovers the full public key of the transaction sender (using `crypto.SigToPub`), which is required for the bridge but not typically exposed in standard receipts.
    *   **Reporting**: Sends the filtered receipt data (including the recovered public key) to a `BridgePostBlockEP` endpoint.

### 2. "Receipt Only" Sync Mode (`eth/downloader/`)
*   **New Sync Mode**: Introduced `ReceiptSync` (alongside Full and Snap sync).
*   **Hardcoded Override**: The downloader is currently hardcoded to force `ReceiptSync` mode (in `downloader.go`). Peer logs may still say `mode=snap` (camouflage); inside `synchronise()` the mode is forced to `ReceiptSync`.
*   **Simplified Processing**:
    *   Fetches **Headers**, **Bodies**, and **Receipts**.
    *   **Bypasses** standard block import and state validation (EVM execution).
    *   Instead, it runs `processReceiptOnlyContent`, which directly calls the bridge module to process receipts and forward them to the API.
*   **Send/download ceiling = skeleton finalized (F)**, not the network tip. See §5 and [BRIDGE_CASE_A_B_FINALITY.md](BRIDGE_CASE_A_B_FINALITY.md).

### 3. Configuration Updates
*   Added flags and config options for Bridge URLs (`BridgePostBlockEP`, `BridgeGetAddressesEP`) and timeouts.
*   Integrated bridge configuration into the main `Ethereum` service initialization.

### 4. Skeleton vs. Blockchain Head
*   **Dual Head State**:
    *   **Skeleton Head**: Tracks the latest block announced by the Consensus Layer (Beacon Chain). It is updated via `processNewHead` in `skeleton.go` and stored in the `skeleton` database namespace.
    *   **Blockchain Head**: The canonical "tip" of the chain from the execution client's perspective. In `ReceiptSync` mode, this **does not advance** because full blocks are not imported.
*   **Implication**: APIs relying on `blockchain.CurrentHeader()` (like `GetBlobsV2` fork checks) see an old timestamp (Genesis), while the `Skeleton` knows the true network head. This necessitated the fix in `GetBlobsV2`.

### 5. Continuity cursors (C vs F) and CASE A / CASE B
Bridge-geth has no full EL state; the skeleton is the chain view for request + send.

| Cursor | Source | Role |
| --- | --- | --- |
| **C** | API `GET /v1/bridge/block/latest` | Handshake / re-drive origin (`try C+1`) |
| **F** | skeleton `Finalized` (from CL via Engine API) | Only download/POST blocks `≤ F` |

*   **CASE A** (`C ≥ F`): idle until F advances; do not download tip or hard-fail.
*   **CASE B** (`F > C`): re-drive `(C, F]`; cursor rewinds to C, **F is not rewound**.
*   Skeleton **F is monotonic** (never persist a regressive finalized watermark).
*   Log prefix for these paths: **`GONKA:`** (e.g. `GONKA: CASE A idle…`, `GONKA: CASE B re-drive…`). Bridge package messages use `Bridge:`.

Full write-up: [BRIDGE_CASE_A_B_FINALITY.md](BRIDGE_CASE_A_B_FINALITY.md).
Release monitoring and rollback: [BRIDGE_CASE_A_B_RELEASE_ROLLBACK.md](BRIDGE_CASE_A_B_RELEASE_ROLLBACK.md).

## Recent Commit History (as of Dec 2025)
1.  **`278a3a2`**: Fix for classifying bridge logs (specifically for withdrawals).
2.  **`c1949c2`**: Added initial support for withdrawal (burn) detection.
3.  **`cfc1335`**: Minor Docker/build updates (Alpine version bump).
4.  **`4f8ad64`**: Fixes for explicit URL handling in configuration.
5.  **`8c9f51e`**: **Major Commit**. Implemented the core Bridge logic, `ReceiptSync` mode, and `eth/bridge` package.
6.  **Continuity + CASE A/B finality** (uncommitted / follow-on): clamp ReceiptSync to F, idle when `C ≥ F`, wait when F is unavailable, retain the fail-closed importer guard, and keep skeleton F monotonic — see [BRIDGE_CASE_A_B_FINALITY.md](BRIDGE_CASE_A_B_FINALITY.md).
