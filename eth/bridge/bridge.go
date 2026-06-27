package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/eth/ethconfig"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/params"
)

// Add these variables at the package level
var (
	ethereumConfig *ethconfig.Config
	chainConfig    *params.ChainConfig
	configOnce     sync.Once

	zeroAddress = common.Address{}
	deadAddress = common.HexToAddress("0x000000000000000000000000000000000000dEaD")

	// recentRanges is a rolling in-RAM history of the last N finalized ranges Geth
	// has sent (N = ethereumConfig.BridgeMaxCachedRanges). It is the Layer-2 fast
	// path that lets Geth resend shallow gaps without a devp2p re-download.
	recentRanges   []FinalizedRange
	recentRangesMu sync.Mutex
)

// FinalizedRange holds one download batch of payloads kept in memory for in-RAM
// continuity recovery.
type FinalizedRange struct {
	StartBlock uint64
	EndBlock   uint64
	Blocks     []BlockRequest
}

func SetConfig(ethereumCfg *ethconfig.Config, chainCfg *params.ChainConfig) {
	configOnce.Do(func() {
		ethereumConfig = ethereumCfg
		chainConfig = chainCfg
		// Add logging to verify the bridge API configuration
		if ethereumConfig.BridgePostBlockEP != "" || ethereumConfig.BridgeGetAddressesEP != "" {
			log.Info("Bridge configuration initialized",
				"timeout", ethereumConfig.BridgeTimeout,
				"postBlockURL", ethereumConfig.BridgePostBlockEP,
				"getAddressesURL", ethereumConfig.BridgeGetAddressesEP)
		} else {
			log.Warn("Bridge URLs not set")
		}
	})
}

func IsConfigured() bool {
	return ethereumConfig != nil && ethereumConfig.BridgePostBlockEP != "" && ethereumConfig.BridgeGetAddressesEP != ""
}

// isBurnAddress returns true if the address is the zero address or the dead address
func isBurnAddress(addr common.Address) bool {
	return addr == zeroAddress || addr == deadAddress
}

// classifyBridgeLog determines if a log is a deposit (lock) or burn (withdraw) related to the bridge
func classifyBridgeLog(
	logEntry *types.Log,
	bridgeAddrs map[common.Address]struct{},
) (isDeposit, isBurn bool) {
	// Ensure this is an ERC20 Transfer log
	if len(logEntry.Topics) < 3 || logEntry.Topics[0] != TransferSignature {
		return false, false
	}

	from := common.BytesToAddress(logEntry.Topics[1].Bytes())
	to := common.BytesToAddress(logEntry.Topics[2].Bytes())

	// Case 1: Deposit / lock - user sends tokens TO the bridge contract
	if _, ok := bridgeAddrs[to]; ok {
		return true, false
	}

	// Case 2: Burn / withdraw - bridge token contract sends to a burn address
	// Require:
	//   - event emitted by a bridge token contract
	//   - destination is a well-known burn address
	//   - from is not zero (exclude mints for extra safety)
	if _, ok := bridgeAddrs[logEntry.Address]; ok && isBurnAddress(to) && from != zeroAddress {
		return false, true
	}

	return false, false
}

// ReceiptData represents a single receipt data to be sent to the bridge
type ReceiptData struct {
	Contract     string `json:"contract"`
	Owner        string `json:"owner"`
	PublicKey    string `json:"publicKey"`
	Amount       string `json:"amount"`
	ReceiptIndex string `json:"receiptIndex"`
}

// BlockRequest represents a block with receipts to be sent to the bridge service
type BlockRequest struct {
	BlockNumber  string        `json:"blockNumber"`
	OriginChain  string        `json:"originChain"`
	ReceiptsRoot string        `json:"receiptsRoot"`
	Receipts     []ReceiptData `json:"receipts"`
}

// TransferSignature is the event signature for ERC20 Transfer events
var TransferSignature = crypto.Keccak256Hash([]byte("Transfer(address,address,uint256)"))

// getBridgeContractAddresses returns all bridge contract addresses, fetching them from API each time
func getBridgeContractAddresses(ctx context.Context) ([]common.Address, error) {
	if ethereumConfig == nil || ethereumConfig.BridgeGetAddressesEP == "" {
		return nil, fmt.Errorf("bridge API not configured")
	}

	// Build the full URL for fetching addresses with query parameter
	url := ethereumConfig.BridgeGetAddressesEP + "?chain=" + ethereumConfig.BridgeChain

	// Send request to fetch contract addresses (no payload needed for GET request)
	resp, err := sendToEndpoint(ctx, url, nil, ethereumConfig.BridgeTimeout)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch contract addresses from bridge API: %w", err)
	}
	defer resp.Body.Close()

	// Parse the response structure: { "chain_name": "...", "chain_id": "...", "addresses": [...] }
	var response struct {
		ChainName string   `json:"chain_name"`
		ChainID   string   `json:"chain_id"`
		Addresses []string `json:"addresses"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		return nil, fmt.Errorf("failed to decode contract addresses from bridge API: %w", err)
	}

	// Convert all addresses to common.Address
	var addresses []common.Address
	for i, addrStr := range response.Addresses {
		address := common.HexToAddress(addrStr)
		addresses = append(addresses, address)
		log.Info("Fetched bridge contract address", "index", i, "address", address.Hex())
	}

	if len(addresses) == 0 {
		return nil, fmt.Errorf("no addresses found in API response")
	}

	log.Info("Fetched bridge contract addresses", "count", len(addresses), "chain_name", response.ChainName, "chain_id", response.ChainID)

	return addresses, nil
}

// IsContinuityConfigured reports whether the optional "last confirmed block"
// endpoint is set. When it is not set, the bridge behaves exactly as before
// (no continuity enforcement) so a geth upgrade can ship before the cosmos
// chain upgrade that exposes this endpoint.
func IsContinuityConfigured() bool {
	return ethereumConfig != nil && ethereumConfig.BridgeGetLastBlockEP != ""
}

// GetLastConfirmedBlock asks the cosmos chain for the last block it has confirmed
// for the ethereum origin chain. It returns a tri-state result:
//
//   - ok == true, err == nil: the chain returned a confirmed block number; use it.
//   - ok == false, err == nil: the feature is absent (endpoint unset, not
//     implemented yet (404/501), or empty response). The caller must proceed as
//     usual with no continuity enforcement.
//   - ok == false, err != nil: the endpoint exists but the chain is unreachable
//     (transport error, timeout, or 5xx). The caller should pause and retry,
//     since this is exactly the cosmos-down / gap scenario.
func GetLastConfirmedBlock(ctx context.Context) (block uint64, ok bool, err error) {
	if !IsContinuityConfigured() {
		return 0, false, nil
	}

	url := ethereumConfig.BridgeGetLastBlockEP + "?chain=" + ethereumConfig.BridgeChain
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		// Misconfigured URL: treat as feature absent rather than blocking forever.
		log.Warn("Bridge: failed to build last-confirmed-block request, skipping continuity check", "url", url, "err", err)
		return 0, false, nil
	}

	client := &http.Client{Timeout: ethereumConfig.BridgeTimeout}
	resp, err := client.Do(req)
	if err != nil {
		// Transport error (connection refused / timeout): cosmos is down.
		return 0, false, fmt.Errorf("bridge last-confirmed-block endpoint unreachable: %w", err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusNotImplemented:
		// Chain not upgraded yet: feature absent, proceed as usual.
		log.Debug("Bridge: last-confirmed-block endpoint not implemented by chain, continuity check skipped", "status", resp.StatusCode)
		return 0, false, nil
	case resp.StatusCode >= 500:
		// Server error: treat as cosmos temporarily down.
		return 0, false, fmt.Errorf("bridge last-confirmed-block endpoint returned status %d", resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return 0, false, fmt.Errorf("bridge last-confirmed-block endpoint returned status %d", resp.StatusCode)
	}

	// Unified schema: decode ONLY the `blockNumber` key (see §1.4 / Task 2). The
	// public handshake endpoint returns { "chainId": ..., "blockNumber": ... },
	// omitting blockNumber when the chain is uninitialized.
	var response struct {
		BlockNumber json.Number `json:"blockNumber"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		return 0, false, fmt.Errorf("failed to decode last-confirmed-block response: %w", err)
	}

	raw := response.BlockNumber
	if raw == "" {
		// No block reported yet (fresh chain / empty response): feature present
		// but nothing confirmed. Proceed as usual from the natural origin.
		log.Debug("Bridge: chain reported no confirmed block yet, continuity check skipped")
		return 0, false, nil
	}

	value, err := raw.Int64()
	if err != nil || value < 0 {
		return 0, false, fmt.Errorf("invalid last-confirmed-block value %q: %w", raw.String(), err)
	}

	log.Info("Bridge: chain reported last confirmed block", "lastConfirmed", value)
	return uint64(value), true, nil
}

// recoverPublicKey recovers the full public key from a transaction
func recoverPublicKey(signer types.Signer, tx *types.Transaction) (string, error) {
	// Use the signer to recover the sender address first
	// This validates that the signature is correct for this signer type
	sender, err := types.Sender(signer, tx)
	if err != nil {
		return "", fmt.Errorf("failed to recover sender: %w", err)
	}

	// Get the signature hash using the signer
	sighash := signer.Hash(tx)

	// Get the raw signature values
	v, r, s := tx.RawSignatureValues()

	// Use the same logic as recoverPlain function in transaction_signing.go
	// but recover the full public key instead of just the address

	// For modern signers with non-legacy transactions, V is already 0 or 1
	// For legacy transactions and older signers, we need to adjust V
	var recoveryV *big.Int

	// Check if this is a modern signer handling a legacy transaction
	if tx.Type() == types.LegacyTxType {
		// For legacy transactions, we need to handle V based on the signer type
		switch signer.(type) {
		case types.FrontierSigner:
			// Frontier: V is 27 or 28, convert to 0 or 1
			recoveryV = new(big.Int).Sub(v, big.NewInt(27))
		case types.HomesteadSigner:
			// Homestead: V is 27 or 28, convert to 0 or 1
			recoveryV = new(big.Int).Sub(v, big.NewInt(27))
		case types.EIP155Signer:
			// EIP155: need to extract recovery ID from protected V
			if !tx.Protected() {
				recoveryV = new(big.Int).Sub(v, big.NewInt(27))
			} else {
				// V = chainID * 2 + 35 + recoveryID, so recoveryID = V - chainID * 2 - 35
				chainIdMul := new(big.Int).Mul(signer.ChainID(), big.NewInt(2))
				recoveryV = new(big.Int).Sub(v, chainIdMul)
				recoveryV.Sub(recoveryV, big.NewInt(35))
			}
		default:
			// Modern signer with legacy transaction
			if !tx.Protected() {
				recoveryV = new(big.Int).Sub(v, big.NewInt(27))
			} else {
				chainIdMul := new(big.Int).Mul(signer.ChainID(), big.NewInt(2))
				recoveryV = new(big.Int).Sub(v, chainIdMul)
				recoveryV.Sub(recoveryV, big.NewInt(35))
			}
		}
	} else {
		// Modern transaction types: V is already 0 or 1
		recoveryV = new(big.Int).Set(v)
	}

	// Validate recovery ID
	if recoveryV.BitLen() > 8 || recoveryV.Uint64() > 1 {
		return "", fmt.Errorf("invalid recovery ID: %d", recoveryV.Uint64())
	}

	// Create signature in the format expected by crypto.SigToPub
	rBytes := r.Bytes()
	sBytes := s.Bytes()
	sig := make([]byte, 65)
	copy(sig[32-len(rBytes):32], rBytes)
	copy(sig[64-len(sBytes):64], sBytes)
	sig[64] = byte(recoveryV.Uint64())

	// Recover the public key
	pub, err := crypto.SigToPub(sighash[:], sig)
	if err != nil {
		return "", fmt.Errorf("failed to recover public key: %w", err)
	}

	// Verify that the recovered public key matches the expected sender
	recoveredAddr := crypto.PubkeyToAddress(*pub)
	if recoveredAddr != sender {
		return "", fmt.Errorf("recovered address mismatch: got %s, expected %s", recoveredAddr.Hex(), sender.Hex())
	}

	// Convert the public key to compressed format
	compressedPub := crypto.CompressPubkey(pub)

	// Return the compressed public key as a hex string
	return common.Bytes2Hex(compressedPub), nil
}

// sendBlockToBridge sends block details with filtered receipts to the bridge API
func sendBlockToBridge(ctx context.Context, blockNum uint64, receiptsRoot common.Hash,
	filteredReceipts []ReceiptData) error {
	if ethereumConfig == nil || ethereumConfig.BridgePostBlockEP == "" {
		return nil
	}

	// Prepare request payload using the specified format
	payload := BlockRequest{
		BlockNumber:  fmt.Sprintf("%d", blockNum),
		OriginChain:  ethereumConfig.BridgeChain,
		ReceiptsRoot: receiptsRoot.Hex(),
		Receipts:     filteredReceipts,
	}

	// Marshal the payload to JSON
	jsonData, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal JSON payload: %w", err)
	}

	// Build the full URL for posting blocks
	url := ethereumConfig.BridgePostBlockEP

	// Send to the bridge API
	resp, err := sendToEndpoint(ctx, url, jsonData, ethereumConfig.BridgeTimeout)
	if err != nil {
		log.Error("Failed to send block to bridge API",
			"url", url,
			"block", blockNum,
			"err", err)
		return err
	}
	resp.Body.Close()

	log.Info("Successfully sent block to bridge API",
		"url", url,
		"block", blockNum,
		"receiptsRoot", receiptsRoot.Hex(),
		"receiptCount", len(filteredReceipts))

	return nil
}

// sendToEndpoint sends data to a single bridge endpoint
func sendToEndpoint(ctx context.Context, url string, jsonData []byte, timeout time.Duration) (*http.Response, error) {
	var req *http.Request
	var err error

	if jsonData != nil {
		// Create POST request with payload
		req, err = http.NewRequestWithContext(ctx, "POST", url, bytes.NewBuffer(jsonData))
		if err != nil {
			return nil, fmt.Errorf("failed to create HTTP request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
	} else {
		// Create GET request without payload
		req, err = http.NewRequestWithContext(ctx, "GET", url, nil)
		if err != nil {
			return nil, fmt.Errorf("failed to create HTTP request: %w", err)
		}
	}

	// Execute the request
	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to send HTTP request: %w", err)
	}

	// Check response status
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("API returned non-200 status: %d", resp.StatusCode)
	}

	return resp, nil
}

// SaveRangeToMemory appends a new range to recentRanges and crops the slice to keep
// only the latest BridgeMaxCachedRanges entries (the configurable in-RAM window).
func SaveRangeToMemory(start, end uint64, blocks []BlockRequest) {
	maxRanges := 4
	if ethereumConfig != nil && ethereumConfig.BridgeMaxCachedRanges > 0 {
		maxRanges = ethereumConfig.BridgeMaxCachedRanges
	}

	recentRangesMu.Lock()
	defer recentRangesMu.Unlock()
	recentRanges = append(recentRanges, FinalizedRange{
		StartBlock: start,
		EndBlock:   end,
		Blocks:     blocks,
	})
	if len(recentRanges) > maxRanges {
		recentRanges = recentRanges[len(recentRanges)-maxRanges:]
	}
}

// GetBridgeContractAddresses fetches the bridge address list once and returns it as
// a set for fast lookup. Used once per segment to avoid hammering the API per block.
func GetBridgeContractAddresses(ctx context.Context) (map[common.Address]struct{}, error) {
	addresses, err := getBridgeContractAddresses(ctx)
	if err != nil {
		return nil, err
	}
	set := make(map[common.Address]struct{}, len(addresses))
	for _, addr := range addresses {
		set[addr] = struct{}{}
	}
	return set, nil
}

// PrepareBlockPayload filters a block's receipts for relevant bridge transfers and
// returns the BlockRequest payload WITHOUT making any network request.
func PrepareBlockPayload(header *types.Header, receipts types.Receipts, transactions types.Transactions, bridgeAddrSet map[common.Address]struct{}) (*BlockRequest, error) {
	if header == nil {
		return nil, nil
	}
	blockNum := header.Number.Uint64()
	var filteredReceipts []ReceiptData

	if len(receipts) > 0 {
		signer := types.MakeSigner(chainConfig, header.Number, header.Time)
		for i, receipt := range receipts {
			if receipt == nil || len(receipt.Logs) == 0 {
				continue
			}
			for _, logEntry := range receipt.Logs {
				isDeposit, isBurn := classifyBridgeLog(logEntry, bridgeAddrSet)
				if !isDeposit && !isBurn {
					continue
				}
				if i >= len(transactions) {
					log.Warn("Bridge: Transaction index out of range",
						"receiptIndex", i,
						"transactionCount", len(transactions),
						"blockNumber", blockNum)
					continue
				}
				tx := transactions[i]
				from, err := types.Sender(signer, tx)
				if err != nil {
					log.Warn("Bridge: Failed to recover transaction sender",
						"txHash", tx.Hash().Hex(), "receiptIndex", i, "err", err)
					continue
				}
				pubKey, err := recoverPublicKey(signer, tx)
				if err != nil {
					log.Warn("Bridge: Failed to recover public key",
						"txHash", tx.Hash().Hex(), "receiptIndex", i, "err", err)
					continue
				}
				amount := new(big.Int).SetBytes(logEntry.Data)
				if amount.Sign() == 0 {
					// ERC-20 allows zero-value transfers; Cosmos rejects amount=0 in
					// MsgBridgeExchange.ValidateBasic, which would wedge the API drain.
					log.Debug("Bridge: Skipping zero-amount transfer",
						"token_contract", logEntry.Address.Hex(),
						"from", from.Hex(),
						"block", blockNum,
						"index", i,
						"isDeposit", isDeposit,
						"isBurn", isBurn)
					continue
				}

				filteredReceipts = append(filteredReceipts, ReceiptData{
					Contract:     logEntry.Address.Hex(),
					Owner:        from.Hex(),
					PublicKey:    pubKey,
					Amount:       amount.String(),
					ReceiptIndex: fmt.Sprintf("%d", i),
				})

				msg := "Bridge: ERC20 transfer involving bridge contract detected"
				if isDeposit {
					msg = "Bridge: ERC20 deposit to bridge detected"
				} else if isBurn {
					msg = "Bridge: ERC20 burn from bridge detected"
				}
				log.Info(msg,
					"token_contract", logEntry.Address.Hex(),
					"publicKey", pubKey,
					"from", from.Hex(),
					"amount", amount.String(),
					"block", blockNum,
					"index", i)
				break
			}
		}
	}

	return &BlockRequest{
		BlockNumber:  fmt.Sprintf("%d", blockNum),
		OriginChain:  ethereumConfig.BridgeChain,
		ReceiptsRoot: header.ReceiptHash.Hex(),
		Receipts:     filteredReceipts,
	}, nil
}

// CheckContinuityAndSend posts a downloaded segment to the bridge API while enforcing
// the Layer-2 continuity invariant (see §1.6 / Task 4). blockX is the first block of
// currentRange. On any handshake error or an uninitialized API it gracefully falls
// back to legacy stateless posting (no caching, no restart).
func CheckContinuityAndSend(ctx context.Context, blockX uint64, currentRange []BlockRequest) error {
	if len(currentRange) == 0 {
		return nil
	}

	apiLatest, ok, err := GetLastConfirmedBlock(ctx)
	if err != nil || !ok {
		// FAIL-SAFE: a handshake transport error or an uninitialized API both fall
		// back to legacy stateless posting (no caching, no restart). Single fallback
		// path; only the log line differs by cause.
		if err != nil {
			log.Warn("Bridge: handshake failed; falling back to legacy direct posting", "err", err)
		} else {
			log.Info("Bridge: API uninitialized; falling back to legacy direct posting")
		}
		return sendRangeDirectly(ctx, currentRange)
	}

	endBlock := parseUint(currentRange[len(currentRange)-1].BlockNumber)
	expectedLatest := blockX - 1

	// Case 1: sequential — API is exactly where we expect.
	if apiLatest == expectedLatest {
		if err := sendRangeDirectly(ctx, currentRange); err != nil {
			return err
		}
		SaveRangeToMemory(blockX, endBlock, currentRange)
		return nil
	}

	// Case 2: API is behind — try to heal from the in-RAM cache.
	if apiLatest < expectedLatest {
		// Snapshot the blocks to resend UNDER the lock, then release before any I/O (M2).
		// Also require the cache to CONTIGUOUSLY cover [apiLatest+1 .. expectedLatest];
		// a hole is treated like a deep gap (restart -> Layer 1 re-seed).
		toResend, covered := snapshotContiguousResend(apiLatest+1, expectedLatest)
		if !covered {
			// log.Crit terminates the process; the supervisor restarts geth and
			// receiptSyncOrigin (Layer 1) re-seeds origin from the API's progress.
			log.Crit("Bridge continuity mismatch: gap is wider than memory capacity or non-contiguous; restarting geth to re-seed",
				"apiLatest", apiLatest, "expected", expectedLatest)
		}

		// Lock released: send the resend snapshot, then the current range.
		if err := sendRangeDirectly(ctx, toResend); err != nil {
			return err
		}
		if err := sendRangeDirectly(ctx, currentRange); err != nil {
			return err
		}
		// ORDERING INVARIANT: cache the incoming range ONLY after the resend (never
		// before), otherwise we could evict a still-needed oldest range and trigger a
		// spurious deep-gap restart.
		SaveRangeToMemory(blockX, endBlock, currentRange)
		return nil
	}

	// Case 3: API is ahead.
	if apiLatest >= endBlock {
		// Fully ahead: the API has already committed this whole segment. Skip sending
		// and bypass caching (it will never be asked to resend these).
		log.Info("Bridge: API is fully ahead of segment, skipping", "apiLatest", apiLatest, "endBlock", endBlock)
		return nil
	}

	// Partially ahead: send only blocks > apiLatest and cache only those.
	var sentBlocks []BlockRequest
	for _, req := range currentRange {
		blockNum := parseUint(req.BlockNumber)
		if blockNum > apiLatest {
			if err := sendBlockToBridge(ctx, blockNum, common.HexToHash(req.ReceiptsRoot), req.Receipts); err != nil {
				return err
			}
			sentBlocks = append(sentBlocks, req)
		}
	}
	if len(sentBlocks) > 0 {
		SaveRangeToMemory(apiLatest+1, parseUint(sentBlocks[len(sentBlocks)-1].BlockNumber), sentBlocks)
	}
	return nil
}

// snapshotContiguousResend returns the cached blocks in [from..to] (inclusive), in
// ascending order, but ONLY if the cache contiguously covers the whole range. It
// holds recentRangesMu just long enough to copy (no network I/O under the lock, M2).
func snapshotContiguousResend(from, to uint64) (blocks []BlockRequest, ok bool) {
	if from > to {
		return nil, true
	}
	recentRangesMu.Lock()
	defer recentRangesMu.Unlock()
	next := from
	for _, r := range recentRanges {
		for _, req := range r.Blocks {
			bn := parseUint(req.BlockNumber)
			if bn < from || bn > to {
				continue
			}
			if bn != next {
				return nil, false // hole -> not contiguous
			}
			blocks = append(blocks, req)
			next++
		}
	}
	if next != to+1 {
		return nil, false // did not reach the end of the gap
	}
	return blocks, true
}

// sendRangeDirectly posts a range of blocks to the bridge API one block at a time,
// in order, stopping at the first error (legacy stateless path).
func sendRangeDirectly(ctx context.Context, rangeBlocks []BlockRequest) error {
	for _, req := range rangeBlocks {
		if err := sendBlockToBridge(ctx, parseUint(req.BlockNumber), common.HexToHash(req.ReceiptsRoot), req.Receipts); err != nil {
			return err
		}
	}
	return nil
}

// parseUint parses a decimal block number using the codebase standard (strconv),
// surfacing errors via the log rather than silently yielding 0.
func parseUint(s string) uint64 {
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		log.Error("Bridge: invalid block number in cached payload", "value", s, "err", err)
		return 0
	}
	return v
}
