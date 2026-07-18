# Unsupported Fork Issue: `engine_getBlobsV2`

## Summary

The bridge client can emit `Unsupported fork` for `engine_getBlobsV2` when Prysm requests Osaka-era blob data, even though the beacon/sync side is already operating on an Osaka block.

This happens because the bridge client's `ReceiptSync` mode advances the downloader skeleton but does not necessarily advance Geth's canonical blockchain head. Some Engine API fork checks use the canonical blockchain head timestamp. If that canonical head is still genesis or another pre-Osaka header, Geth thinks Osaka is not active and rejects `engine_getBlobsV2`.

## Observed Logs

Prysm:

```text
PRSM: ERROR [05-04|18:09:48.000] Failed to process data column sidecars from execution error="reconstruct data column sidecars: fetch cells and proofs from execution client for block 0x94e4956cdc41936f28eeeba7452eb5dc28e27537dbe5c509acc2d71d461fd399: get blobs V2: Unsupported fork" prefix=sync
```

Geth:

```text
GETH: WARN [05-04|18:09:48.152] Served engine_getBlobsV2 conn=127.0.0.1:47400 reqid=411631 duration="34.616us" err="Unsupported fork" errdata="{\"err\":\"engine_getBlobsV2 is not available before Osaka fork\"}"
```

## Why It Says `Unsupported fork`

`Unsupported fork` is the standard Engine API error category for a method called outside the fork range where that method is valid.

In this case:

* `engine_getBlobsV2` is an Osaka-and-later Engine API method.
* If Geth believes the local execution head is before Osaka, it returns `-38005 Unsupported fork`.
* The detailed error text explains the specific method/fork mismatch:

```text
engine_getBlobsV2 is not available before Osaka fork
```

The error type is defined in `beacon/engine/errors.go`:

```go
UnsupportedFork = &EngineAPIError{code: -38005, msg: "Unsupported fork"}
```

## Root Cause In Bridge Client

The bridge client intentionally runs in `ReceiptSync` mode. It fetches headers, bodies, and receipts for bridge processing, but it bypasses normal full block import and EVM execution.

That creates two different notions of chain progress:

* **Skeleton head**: Tracks the real beacon-chain-driven sync head.
* **Canonical blockchain head**: The execution client's local imported head, exposed by `BlockChain().CurrentHeader()`.

In a normal full/snap node, these two are expected to move together. In the bridge client, they may not.

So this can happen:

1. Prysm reaches an Osaka-era block.
2. Prysm needs data column sidecars / cells and proofs.
3. Prysm calls `engine_getBlobsV2`.
4. Geth checks Osaka activation using the canonical blockchain head timestamp.
5. The canonical head is stale because `ReceiptSync` did not fully import blocks.
6. The stale head timestamp is pre-Osaka.
7. Geth rejects the request as `Unsupported fork`.

## Relevant Code Path

The failure comes from `ConsensusAPI.GetBlobsV2` in `eth/catalyst/api.go`.

The upstream-style fork guard is:

```go
head := api.eth.BlockChain().CurrentHeader()
if api.config().LatestFork(head.Time) < forks.Osaka {
    return nil, unsupportedForkErr("engine_getBlobsV2 is not available before Osaka fork")
}
```

That logic is correct for a standard execution client. It is unsafe for this bridge client in `ReceiptSync` mode because `CurrentHeader()` may be stale.

## Bridge-Specific Fix

In this bridge client, the `GetBlobsV2` fork check should not rely on `BlockChain().CurrentHeader()` while in `ReceiptSync` mode.

The current workspace already contains a bridge-specific change in `eth/catalyst/api.go` that comments out the stale-head fork check:

```go
// GONKA: In ReceiptSync mode, we might not have the latest header time, so we skip the check
/*
    head := api.eth.BlockChain().CurrentHeader()
    if api.config().LatestFork(head.Time) < forks.Osaka {
        return nil, unsupportedForkErr("engine_getBlobsV2 is not available before Osaka fork")
    }
*/
```

With this change, `engine_getBlobsV2` no longer rejects only because the canonical imported execution head is stale.

## What If The Logs Still Appear?

If the logs still show:

```text
engine_getBlobsV2 is not available before Osaka fork
```

then the running Geth process is probably not using the patched code.

Likely explanations:

* The binary was not rebuilt after the patch.
* The service/container is running an older image.
* Prysm is connected to a different execution client instance.
* The patch exists in the working tree but not in the deployed branch/commit.

## Verification Checklist

1. Confirm the source contains the `GetBlobsV2` bridge-specific skip in `eth/catalyst/api.go`.
2. Rebuild the geth binary or Docker image from that source.
3. Restart the execution client.
4. Confirm Prysm's execution endpoint points to the rebuilt bridge-geth instance.
5. Watch for `Served engine_getBlobsV2` logs.

Expected behavior after the patch:

* No `Unsupported fork` error caused by `engine_getBlobsV2 is not available before Osaka fork`.
* If blobs are unavailable, `GetBlobsV2` may return `null`, which is different from an Engine API fork error.

## Important Distinction

`Unsupported fork` does not mean Prysm is calling a nonexistent method. It means Geth decided that the method is not valid for the fork it thinks it is currently on.

For this bridge client, that decision can be wrong if it is based on the canonical blockchain head instead of the skeleton/sync head.

