package bridge

import (
	"bytes"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/eth/ethconfig"
	"github.com/ethereum/go-ethereum/params"
)

var (
	testBridgeAddr = common.HexToAddress("0x972a7a92d92796a98801a8818bcf91f1648f2f68")
	testToken      = common.HexToAddress("0x00000000000000000000000000000000000000aa")
	testHolder     = common.HexToAddress("0x00000000000000000000000000000000000000bb")
)

// payloadFixture builds a mainnet-rules block whose i-th transaction is signed
// by a fresh key and whose i-th receipt carries logs[i].
func payloadFixture(t *testing.T, logs [][]*types.Log) (*types.Header, types.Receipts, types.Transactions) {
	t.Helper()
	prevEthereumConfig, prevChainConfig := ethereumConfig, chainConfig
	t.Cleanup(func() { ethereumConfig, chainConfig = prevEthereumConfig, prevChainConfig })
	ethereumConfig = &ethconfig.Config{BridgeChain: "ethereum"}
	chainConfig = params.MainnetChainConfig

	header := &types.Header{Number: big.NewInt(25_000_000), Time: 1_760_000_000}
	signer := types.MakeSigner(chainConfig, header.Number, header.Time)
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}

	var receipts types.Receipts
	var txs types.Transactions
	for i, l := range logs {
		tx, err := types.SignNewTx(key, signer, &types.DynamicFeeTx{
			ChainID:   chainConfig.ChainID,
			Nonce:     uint64(i),
			GasTipCap: big.NewInt(1),
			GasFeeCap: big.NewInt(1),
			Gas:       100_000,
			To:        &testToken,
		})
		if err != nil {
			t.Fatal(err)
		}
		txs = append(txs, tx)
		receipts = append(receipts, &types.Receipt{Logs: l})
	}
	return header, receipts, txs
}

func transferLog(to common.Address, data []byte, extraTopics ...common.Hash) *types.Log {
	topics := []common.Hash{TransferSignature, common.BytesToHash(testHolder.Bytes()), common.BytesToHash(to.Bytes())}
	return &types.Log{Address: testToken, Topics: append(topics, extraTopics...), Data: data}
}

func uint256Data(v *big.Int) []byte {
	return common.LeftPadBytes(v.Bytes(), 32)
}

func preparedAmounts(t *testing.T, logs [][]*types.Log) []string {
	t.Helper()
	header, receipts, txs := payloadFixture(t, logs)
	payload, err := PrepareBlockPayload(header, receipts, txs, map[common.Address]struct{}{testBridgeAddr: {}})
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, r := range payload.Receipts {
		out = append(out, r.Amount)
	}
	return out
}

func paddedData(v *big.Int, size int) []byte {
	return common.LeftPadBytes(v.Bytes(), size)
}

func burnLog(data []byte) *types.Log {
	return &types.Log{
		Address: testBridgeAddr,
		Topics:  []common.Hash{TransferSignature, common.BytesToHash(testHolder.Bytes()), common.BytesToHash(deadAddress.Bytes())},
		Data:    data,
	}
}

// Every case except the over-limit ones must produce exactly what the feeder
// produced before the amount guard: feeders of both versions vote together
// during a rollout, so any other difference splits votes on the same receipt.
func TestPrepareBlockPayloadAmountGuard(t *testing.T) {
	maxUint256 := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))
	maxDigits := new(big.Int).Sub(new(big.Int).Exp(big.NewInt(10), big.NewInt(78), nil), big.NewInt(1))
	overDigits := new(big.Int).Exp(big.NewInt(10), big.NewInt(78), nil)
	thousand := big.NewInt(1000)
	cases := []struct {
		name string
		log  *types.Log
		want []string
	}{
		{"canonical deposit", transferLog(testBridgeAddr, uint256Data(thousand)), []string{"1000"}},
		{"max uint256", transferLog(testBridgeAddr, uint256Data(maxUint256)), []string{maxUint256.String()}},
		{"31-byte encoding unchanged", transferLog(testBridgeAddr, paddedData(thousand, 31)), []string{"1000"}},
		{"33-byte leading-zero encoding unchanged", transferLog(testBridgeAddr, paddedData(thousand, 33)), []string{"1000"}},
		{"4 topics unchanged", transferLog(testBridgeAddr, uint256Data(thousand), common.Hash{1}), []string{"1000"}},
		{"78 digits above uint256 unchanged", transferLog(testBridgeAddr, paddedData(maxDigits, 33)), []string{maxDigits.String()}},
		{"79 digits dropped", transferLog(testBridgeAddr, paddedData(overDigits, 33)), nil},
		{"33 bytes of 0xff dropped", transferLog(testBridgeAddr, bytes.Repeat([]byte{0xff}, 33)), nil},
		{"zero amount dropped", transferLog(testBridgeAddr, uint256Data(big.NewInt(0))), nil},
		{"canonical burn", burnLog(uint256Data(big.NewInt(7))), []string{"7"}},
		{"over-limit burn dropped", burnLog(bytes.Repeat([]byte{0xff}, 33)), nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := preparedAmounts(t, [][]*types.Log{{tc.log}})
			if len(got) != len(tc.want) || (len(got) > 0 && got[0] != tc.want[0]) {
				t.Fatalf("amounts = %v, want %v", got, tc.want)
			}
		})
	}
}

// Log selection within a receipt must match earlier feeders: the first
// qualifying log wins, zero amounts fall through to the next log (as before),
// and an over-limit amount drops the whole receipt instead of falling through.
func TestPrepareBlockPayloadLogSelectionUnchanged(t *testing.T) {
	got := preparedAmounts(t, [][]*types.Log{
		{transferLog(testBridgeAddr, paddedData(big.NewInt(1), 31)), transferLog(testBridgeAddr, uint256Data(big.NewInt(5)))},
		{transferLog(testBridgeAddr, uint256Data(big.NewInt(0))), transferLog(testBridgeAddr, uint256Data(big.NewInt(6)))},
		{transferLog(testBridgeAddr, bytes.Repeat([]byte{0xff}, 33)), transferLog(testBridgeAddr, uint256Data(big.NewInt(8)))},
		{transferLog(testBridgeAddr, uint256Data(big.NewInt(9)))},
	})
	want := []string{"1", "6", "9"}
	if len(got) != len(want) {
		t.Fatalf("amounts = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("amounts = %v, want %v", got, want)
		}
	}
}
