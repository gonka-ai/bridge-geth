package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/eth/ethconfig"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
)

// Add these variables at the package level
var (
	// New variables for block tracking
	processedBlocks   = make(map[uint64]*BlockInfo) // map[blockNumber]*BlockInfo
	processedBlocksMu sync.RWMutex

	ethereumConfig *ethconfig.Config
	chainConfig    *params.ChainConfig
	configOnce     sync.Once
)

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

// Add new types and variables for block tracking
type BlockInfo struct {
	Number       uint64
	Hash         common.Hash
	ReceiptsRoot common.Hash
	Finalized    bool
	HasTransfers bool // Indicates if this block has relevant transfers
}

// getBridgeContractAddresses returns all bridge contract addresses, fetching them from API each time
func getBridgeContractAddresses(ctx context.Context) ([]common.Address, error) {
	if ethereumConfig == nil || ethereumConfig.BridgeGetAddressesEP == "" {
		return nil, fmt.Errorf("bridge API not configured")
	}

	// Build the full URL for fetching addresses with query parameter
	url := ethereumConfig.BridgeGetAddressesEP + "?chain=ethereum"

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
		OriginChain:  "ethereum",
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

// Store block information for later finalization
func StoreBlockInfo(number uint64, hash, receiptsRoot common.Hash) {
	processedBlocksMu.Lock()
	defer processedBlocksMu.Unlock()

	processedBlocks[number] = &BlockInfo{
		Number:       number,
		Hash:         hash,
		ReceiptsRoot: receiptsRoot,
		Finalized:    false,
		HasTransfers: false,
	}
	log.Debug("Stored block info for future finalization",
		"number", number,
		"hash", hash.Hex(),
		"receiptsRoot", receiptsRoot.Hex())
}

// ProcessBlocks processes blocks and sends all blocks to the bridge API
// If the block contains filtered transfers, those receipts are included in the request
func ProcessBlocks(receiptsRaw rlp.RawValue, header *types.Header, transactions types.Transactions) error {
    if ethereumConfig == nil || ethereumConfig.BridgePostBlockEP == "" || ethereumConfig.BridgeGetAddressesEP == "" {
        // No API configured, skip processing
        return nil
    }

    if header == nil {
        return nil
    }

    // Decode the receipts from RLP
    var receipts types.Receipts
    if len(receiptsRaw) > 0 {
        if err := rlp.DecodeBytes(receiptsRaw, &receipts); err != nil {
            return fmt.Errorf("failed to decode receipts RLP: %w", err)
        }
    }

    blockNum := header.Number.Uint64()
    ctx := context.Background()

	// Get the bridge contract addresses from API
	bridgeContractAddresses, err := getBridgeContractAddresses(ctx)
	if err != nil {
		log.Error("Failed to get bridge contract addresses", "err", err)
		return err
	}

	// If no bridge contracts are configured, skip processing
	if len(bridgeContractAddresses) == 0 {
		log.Info("Bridge: No bridge contracts configured, skipping block processing",
			"number", blockNum,
			"hash", header.Hash().Hex())
		return nil
	}

    log.Info("Bridge: Processing block",
		"number", blockNum,
		"hash", header.Hash().Hex(),
		"receipts_count", len(receipts),
		"transactions_count", len(transactions))

	// This will hold the filtered receipts
	var filteredReceipts []ReceiptData

	// If the block has receipts, filter them for relevant transfers
	if len(receipts) > 0 {
		// Create a signer to recover transaction senders
		signer := types.MakeSigner(chainConfig, header.Number, header.Time)

		// Iterate through block receipts to collect relevant transfers
		for i, receipt := range receipts {
			if receipt == nil || len(receipt.Logs) == 0 {
				continue
			}

			for _, logEntry := range receipt.Logs {
				// Check if this is a USDT transfer log
				if len(logEntry.Topics) < 3 || logEntry.Topics[0] != TransferSignature {
					continue
				}

				// Extract transfer details
				to := common.BytesToAddress(logEntry.Topics[2].Bytes())

				// Check if this transfer is to any of our bridge contracts
				isBridgeTransfer := false
				for _, bridgeAddr := range bridgeContractAddresses {
					if to == bridgeAddr {
						isBridgeTransfer = true
						break
					}
				}

				if !isBridgeTransfer {
					continue
				}

				// Get the transaction by index (receipt index = transaction index)
				if i >= len(transactions) {
					log.Warn("Bridge: Transaction index out of range",
						"receiptIndex", i,
						"transactionCount", len(transactions),
						"blockNumber", blockNum,
						"blockHash", header.Hash().Hex(),
						"receiptTxHash", receipt.TxHash.Hex())
					continue
				}

				tx := transactions[i]
				log.Info("Bridge: Found matching transaction for receipt",
					"receiptIndex", i,
					"txHash", tx.Hash().Hex(),
					"receiptTxHash", receipt.TxHash.Hex(),
					"blockNumber", blockNum)

				// Recover the sender and public key from the transaction
				from, err := types.Sender(signer, tx)
				if err != nil {
					log.Warn("Bridge: Failed to recover transaction sender",
						"txHash", tx.Hash().Hex(),
						"receiptIndex", i,
						"err", err)
					continue
				}

				// Get the full public key
				pubKey, err := recoverPublicKey(signer, tx)
				if err != nil {
					log.Warn("Bridge: Failed to recover public key",
						"txHash", tx.Hash().Hex(),
						"receiptIndex", i,
						"err", err)
					continue
				}

				amount := new(big.Int).SetBytes(logEntry.Data)

				// Add this receipt to our filtered list
				receiptData := ReceiptData{
					Contract:     logEntry.Address.Hex(), // Use the token contract address
					Owner:        from.Hex(),
					PublicKey:    pubKey,
					Amount:       amount.String(),
					ReceiptIndex: fmt.Sprintf("%d", i),
				}

				filteredReceipts = append(filteredReceipts, receiptData)

				log.Info("Bridge: ERC20 transfer involving bridge contract detected",
					"token_contract", logEntry.Address.Hex(),
					"publicKey", pubKey,
					"from", from.Hex(),
					"to", to.Hex(),
					"amount", amount.String(),
					"block", blockNum,
					"index", i)
				break
			}
		}
	}

	log.Info("Block processing complete",
		"number", blockNum,
		"filtered_receipts", len(filteredReceipts))

	// Send block to bridge (even if no relevant receipts were found)
	if err := sendBlockToBridge(ctx, blockNum, header.ReceiptHash, filteredReceipts); err != nil {
		return err
	}

	return nil
}
