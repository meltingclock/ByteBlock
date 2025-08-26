package bundle

import (
	"context"
	"crypto/ecdsa"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/meltingclock/biteblock_v1/internal/telemetry"
)

type Bundler struct {
	client       *ethclient.Client
	privateKey   *ecdsa.PrivateKey
	flashbotsKey *ecdsa.PrivateKey
	endpoint     string
	chainID      *big.Int
}

type Bundle struct {
	Transactions []*types.Transaction
	BlockNumber  *big.Int
	MinTimestamp uint64
	MaxTimestamp uint64
}

type BundleResponse struct {
	BundleHash string `json:"bundleHash"`
}

type SimulationResponse struct {
	Success          bool   `json:"success"`
	Error            string `json:"error,omitempty"`
	StateBlockNumber uint64 `json:"stateBlockNumber"`
	TotalGasUsed     uint64 `json:"totalGasUsed"`
	Results          []struct {
		TxHash   string `json:"txHash"`
		GasUsed  uint64 `json:"gasUsed"`
		GasPrice string `json:"gasPrice"`
		Error    string `json:"error,omitempty"`
	} `json:"results"`
}

func NewBundler(client *ethclient.Client, privateKey *ecdsa.PrivateKey, chainID *big.Int) (*Bundler, error) {
	// Generate a new key for Flashbots authentication
	flashbotsKey, err := crypto.GenerateKey()
	if err != nil {
		return nil, fmt.Errorf("generate flashbots key: %w", err)
	}

	endpoint := "https://relay.flashbots.net"
	if chainID.Int64() == 5 { // Goerli
		endpoint = "https://relay-goerli.flashbots.net"
	}
	return &Bundler{
		client:       client,
		privateKey:   privateKey,
		flashbotsKey: flashbotsKey,
		endpoint:     endpoint,
		chainID:      chainID,
	}, nil
}

// SendBundle sends a bundle to Flashbots
func (b *Bundler) SendBundle(ctx context.Context, bundle *Bundle) (string, error) {
	// Get current block
	currentBlock, err := b.client.BlockNumber(ctx)
	if err != nil {
		return "", fmt.Errorf("get block number: %w", err)
	}

	if bundle.BlockNumber == nil {
		bundle.BlockNumber = new(big.Int).SetUint64(currentBlock + 1)
	}

	// Prepare bundle payload
	var txs []string
	for _, tx := range bundle.Transactions {
		data, err := tx.MarshalBinary()
		if err != nil {
			return "", fmt.Errorf("marshal tx: %w", err)
		}
		txs = append(txs, hexutil.Encode(data))
	}

	params := map[string]interface{}{
		"txs":         txs,
		"blockNumber": hexutil.EncodeBig(bundle.BlockNumber),
	}

	if bundle.MinTimestamp > 0 {
		params["minTimestamp"] = bundle.MinTimestamp
	}
	if bundle.MaxTimestamp > 0 {
		params["maxTimestamp"] = bundle.MaxTimestamp
	}

	// Send bundle
	result, err := b.callFlashbots(ctx, "eth_sendBundle", []interface{}{params})
	if err != nil {
		return "", fmt.Errorf("send bundle: %w", err)
	}

	var resp BundleResponse
	if err := json.Unmarshal(result, &resp); err != nil {
		return "", fmt.Errorf("parse response: %w", err)
	}

	telemetry.Infof("[bundle] sent bundle %s for block %d", resp.BundleHash, bundle.BlockNumber.Uint64())
	return resp.BundleHash, nil
}

// SimulateBundle simulates a bundle before sending
func (b *Bundler) SimulateBundle(ctx context.Context, bundle *Bundle) (*SimulationResponse, error) {
	// Get current block
	currentBlock, err := b.client.BlockNumber(ctx)
	if err != nil {
		return nil, fmt.Errorf("get block number: %w", err)
	}

	// Prepare bundle payload
	var txs []string
	for _, tx := range bundle.Transactions {
		data, err := tx.MarshalBinary()
		if err != nil {
			return nil, fmt.Errorf("marshal tx: %w", err)
		}
		txs = append(txs, hexutil.Encode(data))
	}

	params := map[string]interface{}{
		"txs":              txs,
		"blockNumber":      hexutil.EncodeBig(new(big.Int).SetUint64(currentBlock + 1)),
		"stateBlockNumber": "latest",
	}

	result, err := b.callFlashbots(ctx, "eth_callBundle", []interface{}{params})
	if err != nil {
		return nil, fmt.Errorf("simulate bundle: %w", err)
	}

	var resp SimulationResponse
	if err := json.Unmarshal(result, &resp); err != nil {
		return nil, fmt.Errorf("parse simulation: %w", err)
	}

	return &resp, nil
}

// CreateSniperBundle creates a bundle with optional bribe
func (b *Bundler) CreateSniperBundle(
	ctx context.Context,
	swapTx *types.Transaction,
	bribeAmount *big.Int,
) (*Bundle, error) {

	bundle := &Bundle{
		Transactions: []*types.Transaction{swapTx},
	}

	// Add bribe transaction if specified
	if bribeAmount != nil && bribeAmount.Sign() > 0 {
		// Get nonce for bribe tx
		nonce, err := b.client.PendingNonceAt(ctx, crypto.PubkeyToAddress(b.privateKey.PublicKey))
		if err != nil {
			return nil, fmt.Errorf("get nonce: %w", err)
		}

		// Flashbots coinbase address
		coinbase := common.HexToAddress("0xDAFEA492D9c6733ae3d56b7Ed1ADB60692c98Bc5")

		// Create bribe tx
		bribeTx := types.NewTransaction(
			nonce+1, // Next nonce after swap
			coinbase,
			bribeAmount,
			2100,
			swapTx.GasPrice(), // Match gas price of swap
			nil,
		)

		// Sign bribe transaction
		signer := types.NewEIP155Signer(b.chainID)
		signedBribe, err := types.SignTx(bribeTx, signer, b.privateKey)
		if err != nil {
			return nil, fmt.Errorf("sign bribe: %w", err)
		}

		bundle.Transactions = append(bundle.Transactions, signedBribe)
		telemetry.Debugf("[bundle] added bribe: %s ETH", formatEth(bribeAmount))
	}

	return bundle, nil
}

// callFlashbots makes authenticated RPC calls to Flashbots relay
func (b *Bundler) callFlashbots(ctx context.Context, method string, params []interface{}) (json.RawMessage, error) {
	// Create request
	payload := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  method,
		"params":  params,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	// Create HTTP request
	req, err := http.NewRequestWithContext(ctx, "POST", b.endpoint, strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}

	// Add Flashbots authentication header
	signature, err := b.signFlashbots(body)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Flashbots-Signature", signature)

	// Send request
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// Parse response
	var result struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	if result.Error != nil {
		return nil, fmt.Errorf("flashbots error %d: %s", result.Error.Code, result.Error.Message)
	}

	return result.Result, nil
}

// signFlashbots creates signature for Flashbots authentication
func (b *Bundler) signFlashbots(body []byte) (string, error) {
	// Hash the body
	hasher := crypto.Keccak256Hash(body)

	// Sign with Flashbots key
	sig, err := crypto.Sign(hasher.Bytes(), b.flashbotsKey)
	if err != nil {
		return "", err
	}

	// Format: address:signature
	addr := crypto.PubkeyToAddress(b.flashbotsKey.PublicKey)
	return fmt.Sprintf("%s:0x%s", addr.Hex(), hex.EncodeToString(sig)), nil
}

func formatEth(wei *big.Int) string {
	if wei == nil {
		return "0"
	}
	eth := new(big.Float).SetInt(wei)
	eth.Quo(eth, big.NewFloat(1e18))
	f, _ := eth.Float64()
	return fmt.Sprintf("%.4f", f)
}
