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
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/meltingclock/biteblock_v1/internal/helpers"
	"github.com/meltingclock/biteblock_v1/internal/telemetry"
)

type Builder struct {
	Name     string
	Endpoint string
	Enabled  bool
}

// Common builders for Ethereum mainnet
var MainnetBuilders = []Builder{
	{Name: "Flashbots", Endpoint: "https://relay.flashbots.net", Enabled: true},
	{Name: "Titan", Endpoint: "https://rpc.titanbuilder.xyz", Enabled: true},
	{Name: "BeaverBuild", Endpoint: "https://rpc.beaverbuild.org", Enabled: true},
	{Name: "BuildAI", Endpoint: "https://buildai.net", Enabled: true},
	{Name: "Rsync", Endpoint: "https://rsync-builder.xyz", Enabled: true},
}

// Goerli/Sepolia test builders
var TestnetBuilders = []Builder{
	{Name: "Flashbots-Goerli", Endpoint: "https://relay-goerli.flashbots.net", Enabled: true},
}

type Bundler struct {
	client       *ethclient.Client
	privateKey   *ecdsa.PrivateKey
	flashbotsKey *ecdsa.PrivateKey
	builders     []Builder
	chainID      *big.Int
	mu           sync.RWMutex

	// Metrics
	submissions map[string]int // Track successful submissions per builder
	simulations map[string]int // Track simulations per builder
}

type Bundle struct {
	Transactions []*types.Transaction
	BlockNumber  *big.Int
	MinTimestamp uint64
	MaxTimestamp uint64
}

type BundleResponse struct {
	BundleHash string `json:"bundleHash"`
	Builder    string // Track which builder accepted
}

type SimulationResponse struct {
	Success          bool   `json:"success"`
	Error            string `json:"error,omitempty"`
	StateBlockNumber uint64 `json:"stateBlockNumber"`
	TotalGasUsed     uint64 `json:"totalGasUsed"`
	CoinbaseDiff     string `json:"coinbaseDiff"` // Profit to validator
	GasFees          string `json:"gasFees"`
	Results          []struct {
		TxHash   string `json:"txHash"`
		GasUsed  uint64 `json:"gasUsed"`
		GasPrice string `json:"gasPrice"`
		Error    string `json:"error,omitempty"`
		Revert   string `json:"revert,omitempty"`
	} `json:"results"`
}

func NewBundler(client *ethclient.Client, privateKey *ecdsa.PrivateKey, chainID *big.Int) (*Bundler, error) {
	// Generate a new key for Flashbots authentication
	flashbotsKey, err := crypto.GenerateKey()
	if err != nil {
		return nil, fmt.Errorf("generate flashbots key: %w", err)
	}

	var builders []Builder
	switch chainID.Int64() {
	case 1: // Mainnet
		builders = MainnetBuilders
	case 5, 11155111: // Goerli, Sepolia
		builders = TestnetBuilders
	default:
		// Local/unknown - no builders
		builders = []Builder{}
		telemetry.Warnf("[bundler] no builders configured for chain %d", chainID.Int64())
	}

	return &Bundler{
		client:       client,
		privateKey:   privateKey,
		flashbotsKey: flashbotsKey,
		builders:     builders,
		chainID:      chainID,
		submissions:  make(map[string]int),
		simulations:  make(map[string]int),
	}, nil
}

func (b *Bundler) SimulateBundle(ctx context.Context, bundle *Bundle) (*SimulationResponse, error) {
	if len(b.builders) == 0 {
		return nil, fmt.Errorf("no builders configured")
	}

	// Get current block for simulation
	currentBlock, err := b.client.BlockNumber(ctx)
	if err != nil {
		return nil, fmt.Errorf("get block number: %w", err)
	}

	if bundle.BlockNumber == nil {
		bundle.BlockNumber = new(big.Int).SetUint64(currentBlock + 1)
	}

	// Try simulation with each builder
	var bestSim *SimulationResponse
	var lastError error

	for _, builder := range b.builders {
		if !builder.Enabled {
			continue
		}

		telemetry.Debugf("[bundler] simulating with %s", builder.Name)

		sim, err := b.simulateWithBuilder(ctx, bundle, builder)
		if err != nil {
			telemetry.Debugf("[bundler] %s simulation failed: %v", builder.Name, err)
			lastError = err
			continue
		}

		if sim.Success {
			b.mu.Lock()
			b.simulations[builder.Name]++
			b.mu.Unlock()

			// Compare profit (coinbase diff) to find best
			if bestSim == nil || b.compareSims(sim, bestSim) > 0 {
				bestSim = sim
			}
		}
	}
	if bestSim != nil {
		return bestSim, nil
	}

	return nil, fmt.Errorf("all simulations failed: %v", lastError)
}

// SendBundle sends a bundle to Flashbots
func (b *Bundler) SendBundle(ctx context.Context, bundle *Bundle) ([]BundleResponse, error) {
	if len(b.builders) == 0 {
		return nil, fmt.Errorf("no builders configured")
	}

	// Sim first to ensure bundle is valid
	sim, err := b.SimulateBundle(ctx, bundle)
	if err != nil {
		return nil, fmt.Errorf("pre-send simulation failed: %w", err)
	}

	if !sim.Success {
		return nil, fmt.Errorf("bundle simulation failed: %w", err)
	}

	telemetry.Infof("[bundler] simulation successful, gas: %d, profit: %s",
		sim.TotalGasUsed, sim.CoinbaseDiff)

	// Get current block
	currentBlock, err := b.client.BlockNumber(ctx)
	if err != nil {
		return nil, fmt.Errorf("get block number: %w", err)
	}

	if bundle.BlockNumber == nil {
		bundle.BlockNumber = new(big.Int).SetUint64(currentBlock + 1)
	}

	// Send to all builders in parallel
	var wg sync.WaitGroup
	responses := make([]BundleResponse, 0)
	respChan := make(chan BundleResponse, len(b.builders))

	for _, builder := range b.builders {
		if !builder.Enabled {
			continue
		}

		wg.Add(1)
		go func(bldr Builder) {
			defer wg.Done()

			resp, err := b.sendToBuilder(ctx, bundle, bldr)
			if err != nil {
				telemetry.Debugf("[bundler] %s submission failed: %v", bldr.Name, err)
				return
			}

			resp.Builder = bldr.Name
			respChan <- resp

			b.mu.Lock()
			b.submissions[bldr.Name]++
			b.mu.Unlock()

			telemetry.Infof("[bundler] submitted to %s: %s", bldr.Name, resp.BundleHash)
		}(builder)
	}

	// Wait for all submissions to complete
	go func() {
		wg.Wait()
		close(respChan)
	}()

	// Collect responses
	for resp := range respChan {
		responses = append(responses, resp)
	}

	if len(responses) == 0 {
		return nil, fmt.Errorf("all bundle submissions failed")
	}

	return responses, nil
}

// SendToMultipleBlocks sends bundle targeting multiple consecutive blocks
func (b *Bundler) SendToMultipleBlocks(ctx context.Context, bundle *Bundle, numBlocks int) error {
	currentBlock, err := b.client.BlockNumber(ctx)
	if err != nil {
		return fmt.Errorf("get block number: %w", err)
	}

	telemetry.Infof("[bundler] sending bundle to %d blocks starting from %d",
		numBlocks, currentBlock+1)

	for i := 0; i < numBlocks; i++ {
		bundleCopy := *bundle
		bundleCopy.BlockNumber = new(big.Int).SetUint64(currentBlock + uint64(i) + 1)

		responses, err := b.SendBundle(ctx, &bundleCopy)
		if err != nil {
			telemetry.Warnf("[bundler] failed for block %d: %v",
				bundleCopy.BlockNumber.Uint64(), err)
			continue
		}

		telemetry.Infof("[bundler] sent to %d builders for block %d",
			len(responses), bundleCopy.BlockNumber.Uint64())
	}

	return nil
}

// Helper: simulate with specific builder
func (b *Bundler) simulateWithBuilder(ctx context.Context, bundle *Bundle, builder Builder) (*SimulationResponse, error) {
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
		"blockNumber":      hexutil.EncodeBig(bundle.BlockNumber),
		"stateBlockNumber": "latest",
	}

	result, err := b.callBuilder(ctx, "eth_callBundle", []interface{}{params}, builder)
	if err != nil {
		return nil, err
	}

	var resp SimulationResponse
	if err := json.Unmarshal(result, &resp); err != nil {
		return nil, fmt.Errorf("parse simulation: %w", err)
	}

	return &resp, nil
}

// Helper: send to specific builder
func (b *Bundler) sendToBuilder(ctx context.Context, bundle *Bundle, builder Builder) (BundleResponse, error) {
	// Prepare bundle payload
	var txs []string
	for _, tx := range bundle.Transactions {
		data, err := tx.MarshalBinary()
		if err != nil {
			return BundleResponse{}, fmt.Errorf("marshal tx: %w", err)
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

	result, err := b.callBuilder(ctx, "eth_sendBundle", []interface{}{params}, builder)
	if err != nil {
		return BundleResponse{}, err
	}

	var resp BundleResponse
	if err := json.Unmarshal(result, &resp); err != nil {
		return BundleResponse{}, fmt.Errorf("parse response: %w", err)
	}

	return resp, nil
}

// Generic builder RPC call
func (b *Bundler) callBuilder(ctx context.Context, method string, params []interface{}, builder Builder) (json.RawMessage, error) {
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
	req, err := http.NewRequestWithContext(ctx, "POST", builder.Endpoint, strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}

	// Add authentication header (Flashbots style - other builders may differ)
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
		return nil, fmt.Errorf("builder error %d: %s", result.Error.Code, result.Error.Message)
	}

	return result.Result, nil
}

// signFlashbots creates signature for Flashbots authentication
func (b *Bundler) signFlashbots(body []byte) (string, error) {
	hasher := crypto.Keccak256Hash(body)
	sig, err := crypto.Sign(hasher.Bytes(), b.flashbotsKey)
	if err != nil {
		return "", err
	}
	addr := crypto.PubkeyToAddress(b.flashbotsKey.PublicKey)
	return fmt.Sprintf("%s:0x%s", addr.Hex(), hex.EncodeToString(sig)), nil
}

// Compare simulations by profit
func (b *Bundler) compareSims(sim1, sim2 *SimulationResponse) int {
	profitA := new(big.Int)
	profitB := new(big.Int)

	if sim1.CoinbaseDiff != "" {
		profitA, _ = new(big.Int).SetString(sim1.CoinbaseDiff, 10)
	}
	if sim2.CoinbaseDiff != "" {
		profitB, _ = new(big.Int).SetString(sim2.CoinbaseDiff, 10)
	}

	return profitA.Cmp(profitB)
}

// GetStats returns bundler statistics
func (b *Bundler) GetStats() map[string]interface{} {
	b.mu.RLock()
	defer b.mu.RUnlock()

	return map[string]interface{}{
		"builders":    len(b.builders),
		"submissions": b.submissions,
		"simulations": b.simulations,
	}
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
		telemetry.Debugf("[bundle] added bribe: %s ETH", helpers.FormatEth(bribeAmount))
	}

	return bundle, nil
}

/*

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
*/
