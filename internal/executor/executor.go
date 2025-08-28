package execution

import (
	"context"
	"crypto/ecdsa"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"

	"github.com/meltingclock/biteblock_v1/internal/bundle"
	v2 "github.com/meltingclock/biteblock_v1/internal/dex/v2"
	"github.com/meltingclock/biteblock_v1/internal/helpers"
	"github.com/meltingclock/biteblock_v1/internal/telemetry"
)

// TradeExecutor handles all trade execution logic
type TradeExecutor struct {
	client     *ethclient.Client
	privateKey *ecdsa.PrivateKey
	walletAddr common.Address
	dex        *v2.Registry
	routerABI  abi.ABI
	erc20ABI   abi.ABI
	bundler    *bundle.Bundler

	// Position tracking
	positions   map[common.Address]*Position
	positionsMu sync.RWMutex
}

// Position represents an open trading position
type Position struct {
	Token       common.Address
	TokenAmount *big.Int
	EthSpent    *big.Int
	EntryPrice  *big.Int
	EntryBlock  uint64
	EntryTime   time.Time
	TxHash      common.Hash
}

// TradeConfig contains parameters for trade execution
type TradeConfig struct {
	GasBoostPercent int      // Percentage to boost gas price
	MaxGasPrice     *big.Int // Maximum gas price in wei
	SlippagePercent int      // Slippage tolerance
	DeadlineSeconds int64    // Transaction deadline in seconds
	UseBundles      bool     // Use Flashbots bundles
	BribeAmount     *big.Int // Miner bribe for bundles
}

// NewTradeExecutor creates a new trade executor
func NewTradeExecutor(
	client *ethclient.Client,
	privateKey *ecdsa.PrivateKey,
	walletAddr common.Address,
	dex *v2.Registry,
) (*TradeExecutor, error) {

	routerABI, err := abi.JSON(strings.NewReader(v2.RouterABI))
	if err != nil {
		return nil, fmt.Errorf("parse router ABI: %w", err)
	}

	erc20ABI, err := abi.JSON(strings.NewReader(ERC20_ABI))
	if err != nil {
		return nil, fmt.Errorf("parse ERC20 ABI: %w", err)
	}

	chainID, err := client.ChainID(context.Background())
	if err != nil {
		return nil, fmt.Errorf("get chain ID: %w", err)
	}

	// Initialize bundler if on mainnet
	var bundler *bundle.Bundler
	if chainID.Int64() == 1 {
		bundler, err = bundle.NewBundler(client, privateKey, chainID)
		if err != nil {
			telemetry.Warnf("[executor] bundler init failed: %v", err)
		}
	}

	return &TradeExecutor{
		client:     client,
		privateKey: privateKey,
		walletAddr: walletAddr,
		dex:        dex,
		routerABI:  routerABI,
		erc20ABI:   erc20ABI,
		bundler:    bundler,
		positions:  make(map[common.Address]*Position),
	}, nil
}

// ExecuteBuy executes a token purchase
func (te *TradeExecutor) ExecuteBuy(
	ctx context.Context,
	token common.Address,
	ethAmount *big.Int,
	config TradeConfig,
) (common.Hash, error) {

	// Validate inputs
	if err := helpers.ValidateAmount(ethAmount); err != nil {
		return common.Hash{}, fmt.Errorf("invalid ETH amount: %w", err)
	}

	if _, err := helpers.ValidateAddress(token.Hex()); err != nil {
		return common.Hash{}, fmt.Errorf("invalid token address: %w", err)
	}

	// Check wallet balance
	balance, err := te.client.BalanceAt(ctx, te.walletAddr, nil)
	if err != nil {
		return common.Hash{}, fmt.Errorf("get balance: %w", err)
	}

	// Need funds for buy + gas
	gasReserve := big.NewInt(1e16) // 0.01 ETH for gas
	required := new(big.Int).Add(ethAmount, gasReserve)

	if balance.Cmp(required) < 0 {
		return common.Hash{}, fmt.Errorf("insufficient balance: have %s, need %s",
			helpers.FormatEth(balance), helpers.FormatEth(required))
	}

	// Get and validate gas price
	gasPrice, err := te.calculateGasPrice(ctx, config)
	if err != nil {
		return common.Hash{}, fmt.Errorf("calculate gas price: %w", err)
	}

	// Build transaction
	tx, err := te.buildBuyTransaction(ctx, token, ethAmount, gasPrice, config)
	if err != nil {
		return common.Hash{}, fmt.Errorf("build transaction: %w", err)
	}

	// Execute with bundle or normal send
	var txHash common.Hash
	if config.UseBundles && te.bundler != nil {
		txHash, err = te.executeWithBundle(ctx, tx, config.BribeAmount)
	} else {
		txHash, err = te.sendTransaction(ctx, tx)
	}

	if err != nil {
		return common.Hash{}, fmt.Errorf("execute transaction: %w", err)
	}

	// Track position
	te.trackPosition(token, ethAmount, txHash)

	telemetry.Infof("[executor] buy executed: token=%s, amount=%s ETH, tx=%s",
		token.Hex(), helpers.FormatEth(ethAmount), txHash.Hex())

	return txHash, nil
}

// ExecuteSell executes a token sale
func (te *TradeExecutor) ExecuteSell(
	ctx context.Context,
	token common.Address,
	tokenAmount *big.Int,
	config TradeConfig,
) (common.Hash, error) {

	// Validate inputs
	if err := helpers.ValidateAmount(tokenAmount); err != nil {
		return common.Hash{}, fmt.Errorf("invalid token amount: %w", err)
	}

	// Check token balance
	balance, err := te.getTokenBalance(ctx, token)
	if err != nil {
		return common.Hash{}, fmt.Errorf("get token balance: %w", err)
	}

	if balance.Cmp(tokenAmount) < 0 {
		return common.Hash{}, fmt.Errorf("insufficient token balance")
	}

	// Ensure approval
	if err := te.ensureApproval(ctx, token, tokenAmount); err != nil {
		return common.Hash{}, fmt.Errorf("approval failed: %w", err)
	}

	// Get gas price
	gasPrice, err := te.calculateGasPrice(ctx, config)
	if err != nil {
		return common.Hash{}, fmt.Errorf("calculate gas price: %w", err)
	}

	// Build transaction
	tx, err := te.buildSellTransaction(ctx, token, tokenAmount, gasPrice, config)
	if err != nil {
		return common.Hash{}, fmt.Errorf("build transaction: %w", err)
	}

	// Execute
	txHash, err := te.sendTransaction(ctx, tx)
	if err != nil {
		return common.Hash{}, fmt.Errorf("send transaction: %w", err)
	}

	// Update position
	te.updatePositionAfterSell(token, tokenAmount)

	telemetry.Infof("[executor] sell executed: token=%s, amount=%s, tx=%s",
		token.Hex(), tokenAmount.String(), txHash.Hex())

	return txHash, nil
}

// buildBuyTransaction creates a buy transaction
func (te *TradeExecutor) buildBuyTransaction(
	ctx context.Context,
	token common.Address,
	ethAmount *big.Int,
	gasPrice *big.Int,
	config TradeConfig,
) (*types.Transaction, error) {

	nonce, err := te.client.PendingNonceAt(ctx, te.walletAddr)
	if err != nil {
		return nil, err
	}

	// Build swap path
	path := []common.Address{te.dex.WETH(), token}
	deadline := big.NewInt(time.Now().Unix() + config.DeadlineSeconds)

	// Calculate minimum output with slippage
	amountOutMin := big.NewInt(0) // TODO: Calculate from reserves

	// Pack swap data
	// Using FeeOnTransferTokens variant for compatibility
	data, err := te.routerABI.Pack(
		"swapExactETHForTokensSupportingFeeOnTransferTokens",
		amountOutMin,
		path,
		te.walletAddr,
		deadline,
	)
	if err != nil {
		return nil, err
	}

	// Get chain ID
	chainID, err := te.client.ChainID(ctx)
	if err != nil {
		return nil, err
	}

	// Create transaction
	tx := types.NewTransaction(
		nonce,
		te.dex.Router(),
		ethAmount,
		uint64(300000), // Gas limit
		gasPrice,
		data,
	)

	// Sign transaction
	signedTx, err := types.SignTx(tx, types.NewEIP155Signer(chainID), te.privateKey)
	if err != nil {
		return nil, err
	}

	return signedTx, nil
}

// buildSellTransaction creates a sell transaction
func (te *TradeExecutor) buildSellTransaction(
	ctx context.Context,
	token common.Address,
	tokenAmount *big.Int,
	gasPrice *big.Int,
	config TradeConfig,
) (*types.Transaction, error) {

	nonce, err := te.client.PendingNonceAt(ctx, te.walletAddr)
	if err != nil {
		return nil, err
	}

	// Build swap path
	path := []common.Address{token, te.dex.WETH()}
	deadline := big.NewInt(time.Now().Unix() + config.DeadlineSeconds)

	// Calculate minimum output with slippage
	amountOutMin := big.NewInt(0) // TODO: Calculate from reserves

	// Pack swap data
	data, err := te.routerABI.Pack(
		"swapExactTokensForETHSupportingFeeOnTransferTokens",
		tokenAmount,
		amountOutMin,
		path,
		te.walletAddr,
		deadline,
	)
	if err != nil {
		return nil, err
	}

	// Get chain ID
	chainID, err := te.client.ChainID(ctx)
	if err != nil {
		return nil, err
	}

	// Create transaction
	tx := types.NewTransaction(
		nonce,
		te.dex.Router(),
		big.NewInt(0),  // No ETH value
		uint64(300000), // Gas limit
		gasPrice,
		data,
	)

	// Sign transaction
	signedTx, err := types.SignTx(tx, types.NewEIP155Signer(chainID), te.privateKey)
	if err != nil {
		return nil, err
	}

	return signedTx, nil
}

// calculateGasPrice calculates optimal gas price
func (te *TradeExecutor) calculateGasPrice(ctx context.Context, config TradeConfig) (*big.Int, error) {
	gasPrice, err := te.client.SuggestGasPrice(ctx)
	if err != nil {
		return nil, err
	}

	// Apply boost
	if config.GasBoostPercent > 0 {
		boost := big.NewInt(100 + int64(config.GasBoostPercent))
		gasPrice = new(big.Int).Mul(gasPrice, boost)
		gasPrice = new(big.Int).Div(gasPrice, big.NewInt(100))
	}

	// Check max limit
	if config.MaxGasPrice != nil && gasPrice.Cmp(config.MaxGasPrice) > 0 {
		return nil, fmt.Errorf("gas price %s exceeds max %s",
			helpers.WeiToGwei(gasPrice), helpers.WeiToGwei(config.MaxGasPrice))
	}

	return gasPrice, nil
}

// sendTransaction sends a signed transaction
func (te *TradeExecutor) sendTransaction(ctx context.Context, tx *types.Transaction) (common.Hash, error) {
	err := te.client.SendTransaction(ctx, tx)
	if err != nil {
		return common.Hash{}, err
	}
	return tx.Hash(), nil
}

// executeWithBundle executes transaction using Flashbots bundle
func (te *TradeExecutor) executeWithBundle(ctx context.Context, tx *types.Transaction, bribeAmount *big.Int) (common.Hash, error) {
	if te.bundler == nil {
		return te.sendTransaction(ctx, tx)
	}

	// Create bundle
	bundle, err := te.bundler.CreateSniperBundle(ctx, tx, bribeAmount)
	if err != nil {
		telemetry.Warnf("[executor] bundle creation failed: %v", err)
		return te.sendTransaction(ctx, tx)
	}

	// Simulate bundle
	sim, err := te.bundler.SimulateBundle(ctx, bundle)
	if err != nil || !sim.Success {
		telemetry.Warnf("[executor] bundle simulation failed")
		return te.sendTransaction(ctx, tx)
	}

	// Send bundle to multiple blocks
	currentBlock, _ := te.client.BlockNumber(ctx)
	for i := uint64(1); i <= 3; i++ {
		bundle.BlockNumber = new(big.Int).SetUint64(currentBlock + i)
		_, err := te.bundler.SendBundle(ctx, bundle)
		if err != nil {
			telemetry.Errorf("[executor] bundle send failed: %v", err)
		}
	}

	return tx.Hash(), nil
}

// ensureApproval ensures the router has approval to spend tokens
func (te *TradeExecutor) ensureApproval(ctx context.Context, token common.Address, amount *big.Int) error {
	// Check current allowance
	allowance, err := te.getAllowance(ctx, token)
	if err != nil {
		return err
	}

	if allowance.Cmp(amount) >= 0 {
		return nil // Already approved
	}

	// Approve max uint256 for convenience
	maxApproval := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))

	data, err := te.erc20ABI.Pack("approve", te.dex.Router(), maxApproval)
	if err != nil {
		return err
	}

	nonce, _ := te.client.PendingNonceAt(ctx, te.walletAddr)
	gasPrice, _ := te.client.SuggestGasPrice(ctx)
	chainID, _ := te.client.ChainID(ctx)

	tx := types.NewTransaction(
		nonce,
		token,
		big.NewInt(0),
		uint64(100000),
		gasPrice,
		data,
	)

	signedTx, err := types.SignTx(tx, types.NewEIP155Signer(chainID), te.privateKey)
	if err != nil {
		return err
	}

	err = te.client.SendTransaction(ctx, signedTx)
	if err != nil {
		return err
	}

	// Wait for confirmation (simplified)
	time.Sleep(3 * time.Second)

	return nil
}

// Helper functions

func (te *TradeExecutor) getTokenBalance(ctx context.Context, token common.Address) (*big.Int, error) {
	data, err := te.erc20ABI.Pack("balanceOf", te.walletAddr)
	if err != nil {
		return nil, err
	}

	result, err := te.client.CallContract(ctx, ethereum.CallMsg{
		To:   &token,
		Data: data,
	}, nil)

	if err != nil {
		return nil, err
	}

	if len(result) == 0 {
		return big.NewInt(0), nil
	}

	return new(big.Int).SetBytes(result), nil
}

func (te *TradeExecutor) getAllowance(ctx context.Context, token common.Address) (*big.Int, error) {
	data, err := te.erc20ABI.Pack("allowance", te.walletAddr, te.dex.Router())
	if err != nil {
		return nil, err
	}

	result, err := te.client.CallContract(ctx, ethereum.CallMsg{
		To:   &token,
		Data: data,
	}, nil)

	if err != nil {
		return nil, err
	}

	if len(result) == 0 {
		return big.NewInt(0), nil
	}

	return new(big.Int).SetBytes(result), nil
}

func (te *TradeExecutor) trackPosition(token common.Address, ethSpent *big.Int, txHash common.Hash) {
	te.positionsMu.Lock()
	defer te.positionsMu.Unlock()

	te.positions[token] = &Position{
		Token:     token,
		EthSpent:  ethSpent,
		EntryTime: time.Now(),
		TxHash:    txHash,
	}
}

func (te *TradeExecutor) updatePositionAfterSell(token common.Address, amount *big.Int) {
	te.positionsMu.Lock()
	defer te.positionsMu.Unlock()

	if pos, exists := te.positions[token]; exists {
		if pos.TokenAmount != nil {
			pos.TokenAmount = new(big.Int).Sub(pos.TokenAmount, amount)
			if pos.TokenAmount.Sign() <= 0 {
				delete(te.positions, token)
			}
		}
	}
}

// GetPositions returns all open positions
func (te *TradeExecutor) GetPositions() map[common.Address]*Position {
	te.positionsMu.RLock()
	defer te.positionsMu.RUnlock()

	positions := make(map[common.Address]*Position)
	for k, v := range te.positions {
		positions[k] = v
	}
	return positions
}

// GetETHBalance returns wallet ETH balance
func (te *TradeExecutor) GetETHBalance(ctx context.Context) (*big.Int, error) {
	return te.client.BalanceAt(ctx, te.walletAddr, nil)
}

// Minimal ERC20 ABI
const ERC20_ABI = `[
	{
		"constant": true,
		"inputs": [{"name": "_owner", "type": "address"}],
		"name": "balanceOf",
		"outputs": [{"name": "", "type": "uint256"}],
		"type": "function"
	},
	{
		"constant": false,
		"inputs": [
			{"name": "_spender", "type": "address"},
			{"name": "_value", "type": "uint256"}
		],
		"name": "approve",
		"outputs": [{"name": "", "type": "bool"}],
		"type": "function"
	},
	{
		"constant": true,
		"inputs": [
			{"name": "_owner", "type": "address"},
			{"name": "_spender", "type": "address"}
		],
		"name": "allowance",
		"outputs": [{"name": "", "type": "uint256"}],
		"type": "function"
	}
]`
