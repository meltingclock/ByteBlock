package scanner

import (
	"context"
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"

	v2 "github.com/meltingclock/biteblock_v1/internal/dex/v2"
	"github.com/meltingclock/biteblock_v1/internal/helpers"
	"github.com/meltingclock/biteblock_v1/internal/telemetry"
)

// ProfitAnalyzer calculates potential profits from trades
type ProfitAnalyzer struct {
	ec         *ethclient.Client
	dex        *v2.Registry
	pairABI    abi.ABI
	routerABI  abi.ABI
	factoryABI abi.ABI
}

// ProfitAnalysis contains the results of profit calculation
type ProfitAnalysis struct {
	Token          common.Address
	BuyAmount      *big.Int // ETH in
	ExpectedTokens *big.Int // Tokens out
	SellProceeds   *big.Int // ETH back after immediate sell
	ProfitETH      *big.Int // Net profit/loss in ETH
	ProfitPercent  float64  // Profit percentage
	GasCost        *big.Int // Estimated gas cost in ETH
	NetProfit      *big.Int // Profit after gas
	IsProfitable   bool
}

// TradeSimulation contains details of a simulated trade
type TradeSimulation struct {
	Success     bool
	AmountIn    *big.Int
	AmountOut   *big.Int
	Path        []common.Address
	PriceImpact float64
	Reserves    [2]*big.Int
	Error       error
}

// NewProfitAnalyzer creates a new profit analyzer
func NewProfitAnalyzer(ec *ethclient.Client, dex *v2.Registry) (*ProfitAnalyzer, error) {
	pairABI, err := abi.JSON(strings.NewReader(PAIR_ABI))
	if err != nil {
		return nil, fmt.Errorf("parse pair ABI: %w", err)
	}

	routerABI, err := abi.JSON(strings.NewReader(v2.RouterABI))
	if err != nil {
		return nil, fmt.Errorf("parse router ABI: %w", err)
	}

	factoryABI, err := abi.JSON(strings.NewReader(v2.FactoryABI))
	if err != nil {
		return nil, fmt.Errorf("parse factory ABI: %w", err)
	}

	return &ProfitAnalyzer{
		ec:         ec,
		dex:        dex,
		pairABI:    pairABI,
		routerABI:  routerABI,
		factoryABI: factoryABI,
	}, nil
}

// AnalyzeTrade calculates potential profit from a trade
func (pa *ProfitAnalyzer) AnalyzeTrade(ctx context.Context, token common.Address, ethAmount *big.Int) (*ProfitAnalysis, error) {
	analysis := &ProfitAnalysis{
		Token:     token,
		BuyAmount: ethAmount,
	}

	// Get current gas price for cost calculation
	gasPrice, err := pa.ec.SuggestGasPrice(ctx)
	if err != nil {
		telemetry.Warnf("[analysis] failed to get gas price: %v", err)
		gasPrice = big.NewInt(30000000000) // 30 gwei fallback
	}

	// Estimate gas cost (buy + sell + approve)
	gasLimit := big.NewInt(600000) // ~300k for buy, 300k for sell
	analysis.GasCost = new(big.Int).Mul(gasPrice, gasLimit)

	// Simulate buy
	buyResult, err := pa.simulateBuy(ctx, token, ethAmount)
	if err != nil {
		return analysis, fmt.Errorf("simulate buy: %w", err)
	}

	if !buyResult.Success {
		analysis.IsProfitable = false
		return analysis, fmt.Errorf("buy simulation failed")
	}

	analysis.ExpectedTokens = buyResult.AmountOut

	// Simulate immediate sell
	sellResult, err := pa.simulateSell(ctx, token, buyResult.AmountOut)
	if err != nil {
		return analysis, fmt.Errorf("simulate sell: %w", err)
	}

	if !sellResult.Success {
		analysis.IsProfitable = false
		return analysis, fmt.Errorf("sell simulation failed")
	}

	analysis.SellProceeds = sellResult.AmountOut

	// Calculate profit
	analysis.ProfitETH = new(big.Int).Sub(analysis.SellProceeds, ethAmount)
	analysis.ProfitPercent = helpers.CalculatePercentage(analysis.ProfitETH, ethAmount)

	// Calculate net profit after gas
	analysis.NetProfit = new(big.Int).Sub(analysis.ProfitETH, analysis.GasCost)
	analysis.IsProfitable = analysis.NetProfit.Sign() > 0

	telemetry.Infof("[analysis] Token: %s, Buy: %s ETH, Sell: %s ETH, Profit: %s ETH (%.2f%%), Net: %s ETH",
		token.Hex(),
		helpers.FormatEth(ethAmount),
		helpers.FormatEth(analysis.SellProceeds),
		helpers.FormatEth(analysis.ProfitETH),
		analysis.ProfitPercent,
		helpers.FormatEth(analysis.NetProfit))

	return analysis, nil
}

// simulateBuy simulates buying tokens with ETH
func (pa *ProfitAnalyzer) simulateBuy(ctx context.Context, token common.Address, ethAmount *big.Int) (*TradeSimulation, error) {
	sim := &TradeSimulation{
		AmountIn: ethAmount,
		Path:     []common.Address{pa.dex.WETH(), token},
	}

	// Get pair reserves
	reserves, err := pa.getReserves(ctx, pa.dex.WETH(), token)
	if err != nil {
		sim.Error = err
		return sim, err
	}
	sim.Reserves = reserves

	// Calculate output using Uniswap formula
	amountOut := pa.getAmountOut(ethAmount, reserves[0], reserves[1])
	if amountOut == nil || amountOut.Sign() <= 0 {
		sim.Error = fmt.Errorf("invalid output amount")
		return sim, sim.Error
	}

	sim.AmountOut = amountOut
	sim.Success = true

	// Calculate price impact
	sim.PriceImpact = pa.calculatePriceImpact(ethAmount, reserves[0])

	return sim, nil
}

// simulateSell simulates selling tokens for ETH
func (pa *ProfitAnalyzer) simulateSell(ctx context.Context, token common.Address, tokenAmount *big.Int) (*TradeSimulation, error) {
	sim := &TradeSimulation{
		AmountIn: tokenAmount,
		Path:     []common.Address{token, pa.dex.WETH()},
	}

	// Get pair reserves
	reserves, err := pa.getReserves(ctx, token, pa.dex.WETH())
	if err != nil {
		sim.Error = err
		return sim, err
	}
	sim.Reserves = reserves

	// Calculate output using Uniswap formula
	amountOut := pa.getAmountOut(tokenAmount, reserves[0], reserves[1])
	if amountOut == nil || amountOut.Sign() <= 0 {
		sim.Error = fmt.Errorf("invalid output amount")
		return sim, sim.Error
	}

	sim.AmountOut = amountOut
	sim.Success = true

	// Calculate price impact
	sim.PriceImpact = pa.calculatePriceImpact(tokenAmount, reserves[0])

	return sim, nil
}

// getReserves fetches pair reserves
func (pa *ProfitAnalyzer) getReserves(ctx context.Context, token0, token1 common.Address) ([2]*big.Int, error) {
	// Get pair address
	pairAddress, err := pa.getPairAddress(ctx, token0, token1)
	if err != nil {
		return [2]*big.Int{}, err
	}

	if pairAddress == (common.Address{}) {
		return [2]*big.Int{}, fmt.Errorf("pair does not exist")
	}

	// Call getReserves on the pair
	data, err := pa.pairABI.Pack("getReserves")
	if err != nil {
		return [2]*big.Int{}, err
	}

	result, err := pa.ec.CallContract(ctx, ethereum.CallMsg{
		To:   &pairAddress,
		Data: data,
	}, nil)

	if err != nil {
		return [2]*big.Int{}, err
	}

	// Unpack results
	var reserves struct {
		Reserve0           *big.Int
		Reserve1           *big.Int
		BlockTimestampLast uint32
	}

	err = pa.pairABI.UnpackIntoInterface(&reserves, "getReserves", result)
	if err != nil {
		return [2]*big.Int{}, err
	}

	// Determine which reserve corresponds to which token
	token0Address, err := pa.getToken0(ctx, pairAddress)
	if err != nil {
		return [2]*big.Int{}, err
	}

	if token0Address == token0 {
		return [2]*big.Int{reserves.Reserve0, reserves.Reserve1}, nil
	}
	return [2]*big.Int{reserves.Reserve1, reserves.Reserve0}, nil
}

// getPairAddress gets the pair address for two tokens
func (pa *ProfitAnalyzer) getPairAddress(ctx context.Context, token0, token1 common.Address) (common.Address, error) {
	data, err := pa.factoryABI.Pack("getPair", token0, token1)
	if err != nil {
		return common.Address{}, err
	}

	factory := pa.dex.Factory()
	result, err := pa.ec.CallContract(ctx, ethereum.CallMsg{
		To:   &factory,
		Data: data,
	}, nil)

	if err != nil {
		return common.Address{}, err
	}

	var pairAddress common.Address
	err = pa.factoryABI.UnpackIntoInterface(&pairAddress, "getPair", result)
	if err != nil {
		return common.Address{}, err
	}

	return pairAddress, nil
}

// getToken0 gets the token0 address from a pair
func (pa *ProfitAnalyzer) getToken0(ctx context.Context, pair common.Address) (common.Address, error) {
	data, err := pa.pairABI.Pack("token0")
	if err != nil {
		return common.Address{}, err
	}

	result, err := pa.ec.CallContract(ctx, ethereum.CallMsg{
		To:   &pair,
		Data: data,
	}, nil)

	if err != nil {
		return common.Address{}, err
	}

	var token0 common.Address
	err = pa.pairABI.UnpackIntoInterface(&token0, "token0", result)
	if err != nil {
		return common.Address{}, err
	}

	return token0, nil
}

// getAmountOut calculates output amount using Uniswap V2 formula
func (pa *ProfitAnalyzer) getAmountOut(amountIn, reserveIn, reserveOut *big.Int) *big.Int {
	if amountIn == nil || amountIn.Sign() <= 0 {
		return nil
	}
	if reserveIn == nil || reserveIn.Sign() <= 0 || reserveOut == nil || reserveOut.Sign() <= 0 {
		return nil
	}

	// Uniswap V2 formula with 0.3% fee
	// amountOut = (amountIn * 997 * reserveOut) / (reserveIn * 1000 + amountIn * 997)

	amountInWithFee := new(big.Int).Mul(amountIn, big.NewInt(997))
	numerator := new(big.Int).Mul(amountInWithFee, reserveOut)
	denominator := new(big.Int).Mul(reserveIn, big.NewInt(1000))
	denominator.Add(denominator, amountInWithFee)

	if denominator.Sign() == 0 {
		return nil
	}

	return new(big.Int).Div(numerator, denominator)
}

// calculatePriceImpact calculates the price impact of a trade
func (pa *ProfitAnalyzer) calculatePriceImpact(amountIn, reserveIn *big.Int) float64 {
	if reserveIn == nil || reserveIn.Sign() == 0 {
		return 100.0 // Max impact if no reserves
	}

	// Price impact = (amountIn / reserveIn) * 100
	impact := new(big.Float).SetInt(amountIn)
	reserve := new(big.Float).SetInt(reserveIn)

	impact.Quo(impact, reserve)
	impact.Mul(impact, big.NewFloat(100))

	result, _ := impact.Float64()
	return result
}

// EstimateOptimalBuyAmount calculates the optimal buy amount for maximum profit
func (pa *ProfitAnalyzer) EstimateOptimalBuyAmount(ctx context.Context, token common.Address, maxETH *big.Int) (*big.Int, error) {
	// Start with small amounts and increase
	testAmounts := []*big.Int{
		helpers.EthToWeiBigInt(1e16), // 0.01 ETH
		helpers.EthToWeiBigInt(5e16), // 0.05 ETH
		helpers.EthToWeiBigInt(1e17), // 0.1 ETH
		helpers.EthToWeiBigInt(5e17), // 0.5 ETH
		helpers.EthToWeiBigInt(1e18), // 1 ETH
		helpers.EthToWeiBigInt(5e18), // 5 ETH
		helpers.EthToWeiBigInt(1e19), // 10 ETH
	}

	var bestAmount *big.Int
	var bestProfit *big.Int

	for _, amount := range testAmounts {
		if amount.Cmp(maxETH) > 0 {
			break
		}

		analysis, err := pa.AnalyzeTrade(ctx, token, amount)
		if err != nil {
			continue
		}

		if analysis.IsProfitable && (bestProfit == nil || analysis.NetProfit.Cmp(bestProfit) > 0) {
			bestAmount = amount
			bestProfit = analysis.NetProfit
		}
	}

	if bestAmount == nil {
		return nil, fmt.Errorf("no profitable amount found")
	}

	return bestAmount, nil
}

// Minimal Pair ABI for getReserves and token addresses
const PAIR_ABI = `[
	{
		"constant": true,
		"inputs": [],
		"name": "getReserves",
		"outputs": [
			{"internalType": "uint112", "name": "_reserve0", "type": "uint112"},
			{"internalType": "uint112", "name": "_reserve1", "type": "uint112"},
			{"internalType": "uint32", "name": "_blockTimestampLast", "type": "uint32"}
		],
		"payable": false,
		"stateMutability": "view",
		"type": "function"
	},
	{
		"constant": true,
		"inputs": [],
		"name": "token0",
		"outputs": [{"internalType": "address", "name": "", "type": "address"}],
		"payable": false,
		"stateMutability": "view",
		"type": "function"
	},
	{
		"constant": true,
		"inputs": [],
		"name": "token1",
		"outputs": [{"internalType": "address", "name": "", "type": "address"}],
		"payable": false,
		"stateMutability": "view",
		"type": "function"
	}
]`
