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
	"github.com/meltingclock/biteblock_v1/internal/telemetry"
)

type ProfitAnalyzer struct {
	ec        *ethclient.Client
	dex       *v2.Registry
	pairABI   abi.ABI
	routerABI abi.ABI
}

type ProfitAnalysis struct {
	Token          common.Address
	BuyAmount      *big.Int // ETH in
	ExpectedTokens *big.Int // Tokens out
	SellProceeds   *big.Int // ETH back after immediate sell
	GasCost        *big.Int // ETH spent on gas
	NetProfit      *big.Int // Profit after gas
	ProfitPercent  float64  // ROI percentage
	IsProfitable   bool
	MinETHRequired *big.Int // Minimum ETH to break even
	OptimalAmount  *big.Int // Optimal amount of tokens to buy
}

func NewProfitAnalyzer(ec *ethclient.Client, dex *v2.Registry) *ProfitAnalyzer {
	pairABI, _ := abi.JSON(strings.NewReader(PAIR_ABI))
	routerABI, _ := abi.JSON(strings.NewReader(v2.RouterABI))

	return &ProfitAnalyzer{
		ec:        ec,
		dex:       dex,
		pairABI:   pairABI,
		routerABI: routerABI,
	}
}

// AnalyzeProfitability calculates potential profit for a trade
func (p *ProfitAnalyzer) AnalyzeProfitability(
	ctx context.Context,
	token common.Address,
	buyAmountETH *big.Int,
	gasPrice *big.Int,
) (*ProfitAnalysis, error) {

	analysis := &ProfitAnalysis{
		Token:     token,
		BuyAmount: buyAmountETH,
	}

	// Get pair address
	pair, err := p.getPairAddress(ctx, token)
	if err != nil {
		return nil, fmt.Errorf("get pair address: %w", err)
	}

	// Get reserves
	reserves, err := p.getReserves(ctx, pair, token)
	if err != nil {
		return nil, fmt.Errorf("get reserves: %w", err)
	}

	// Calculate expected tokens from buy
	tokensOut := p.calculateSwapOutput(
		buyAmountETH,
		reserves.ReserveETH,
		reserves.ReserveToken,
		true, // ETH -> Token
	)
	analysis.ExpectedTokens = tokensOut

	// Calculate ETH from immediate sell
	ethBack := p.calculateSwapOutput(
		tokensOut,
		reserves.ReserveToken,
		reserves.ReserveETH,
		false, // Token -> ETH
	)
	analysis.SellProceeds = ethBack

	// Calculate gas costs (2 transactions: approve + swap)
	gasLimit := big.NewInt(300000) // Approximate for swap
	analysis.GasCost = new(big.Int).Mul(gasLimit, gasPrice)

	// Calculate net profit
	grossProfit := new(big.Int).Sub(ethBack, buyAmountETH)
	analysis.NetProfit = new(big.Int).Sub(grossProfit, analysis.GasCost)

	// Calculate profit percentage
	if buyAmountETH.Sign() > 0 {
		profitRatio := new(big.Float).SetInt(analysis.NetProfit)
		buyFloat := new(big.Float).SetInt(buyAmountETH)
		profitRatio.Quo(profitRatio, buyFloat)
		profitRatio.Mul(profitRatio, big.NewFloat(100))
		analysis.ProfitPercent, _ = profitRatio.Float64()
	}

	// Determine if profitable
	analysis.IsProfitable = analysis.NetProfit.Sign() > 0

	// Find minimum ETH for break-even
	analysis.MinETHRequired = p.findBreakEvenAmount(ctx, reserves, gasPrice)

	// Find optimal buy amount (maximize absolute profit)
	analysis.OptimalAmount = p.findOptimalAmount(ctx, reserves, gasPrice)

	telemetry.Debugf("[profit] token: %s, buy: %s ETH, sell: %s ETH, gas: %s ETH, net: %s ETH (%.2f%%)",
		token.Hex()[:10],
		formatEth(buyAmountETH),
		formatEth(ethBack),
		formatEth(analysis.GasCost),
		formatEth(analysis.NetProfit),
		analysis.ProfitPercent,
	)

	return analysis, nil
}

// calculateSwapOutput uses Uniswap V2 formula: x * y = k
func (p *ProfitAnalyzer) calculateSwapOutput(
	amountIn *big.Int,
	reserveIn *big.Int,
	reserveOut *big.Int,
	isBuy bool, // true for ETH -> Token, false for Token -> ETH
) *big.Int {
	if amountIn.Sign() == 0 || reserveIn.Sign() == 0 || reserveOut.Sign() == 0 {
		return big.NewInt(0)
	}

	// Apply fee (0.3% for Uniswap V2)
	amountInWithFee := new(big.Int).Mul(amountIn, big.NewInt(997))
	numerator := new(big.Int).Mul(amountInWithFee, reserveOut)
	denominator := new(big.Int).Mul(reserveIn, big.NewInt(1000))
	denominator.Add(denominator, amountInWithFee)

	amountOut := new(big.Int).Div(numerator, denominator)

	return amountOut
}

// findBreakEvenAmount binary searches for minimum profitable amount
func (p *ProfitAnalyzer) findBreakEvenAmount(
	ctx context.Context,
	reserves *Reserves,
	gasPrice *big.Int,
) *big.Int {
	// Binary search between 0.001 ETH and 1 ETH
	low := big.NewInt(1000000000000000)     // 0.001 ETH
	high := big.NewInt(1000000000000000000) // 1 ETH

	for i := 0; i < 20; i++ { // Max 20 iterations
		mid := new(big.Int).Add(low, high)
		mid.Div(mid, big.NewInt(2))

		// Calculate profit at this amount
		tokensOut := p.calculateSwapOutput(mid, reserves.ReserveETH, reserves.ReserveToken, true)
		ethBack := p.calculateSwapOutput(tokensOut, reserves.ReserveToken, reserves.ReserveETH, false)

		gasLimit := big.NewInt(300000)
		gasCost := new(big.Int).Mul(gasLimit, gasPrice)

		grossProfit := new(big.Int).Sub(ethBack, mid)
		netProfit := new(big.Int).Sub(grossProfit, gasCost)

		if netProfit.Sign() > 0 {
			high = mid
		} else {
			low = mid
		}

		// Check if we've converged
		diff := new(big.Int).Sub(high, low)
		if diff.Cmp(big.NewInt(1000000000000)) < 0 { // Less than 0.000001 ETH difference
			break
		}
	}

	return high
}

// findOptimalAmount finds the amount that maximizes absolute profit
func (p *ProfitAnalyzer) findOptimalAmount(
	ctx context.Context,
	reserves *Reserves,
	gasPrice *big.Int,
) *big.Int {
	// Use calculus to find optimal amount
	// For Uniswap V2: Optimal = sqrt(k * gasPrice * 1000 / 997) - reserveETH
	// This is derived from maximizing profit function

	k := new(big.Int).Mul(reserves.ReserveETH, reserves.ReserveToken)
	gasLimit := big.NewInt(300000)
	gasCost := new(big.Int).Mul(gasLimit, gasPrice)

	// Approximate calculation
	numerator := new(big.Int).Mul(k, gasCost)
	numerator.Mul(numerator, big.NewInt(1000))
	numerator.Div(numerator, big.NewInt(997))

	// Use Newton's method for square root
	optimal := p.sqrt(numerator)
	optimal.Sub(optimal, reserves.ReserveETH)

	// Ensure it's positive and reasonable
	if optimal.Sign() <= 0 {
		optimal = big.NewInt(100000000000000000) // Default to 0.1 ETH
	}

	maxBuy := big.NewInt(10000000000000000000) // Cap at 10 ETH
	if optimal.Cmp(maxBuy) > 0 {
		optimal = maxBuy
	}

	return optimal
}

// sqrt calculates square root using Newton's method
func (p *ProfitAnalyzer) sqrt(n *big.Int) *big.Int {
	if n.Sign() <= 0 {
		return big.NewInt(0)
	}

	x := new(big.Int).Set(n)
	y := new(big.Int).Div(n, big.NewInt(2))

	for x.Cmp(y) > 0 {
		x.Set(y)
		y.Add(y, new(big.Int).Div(n, y))
		y.Div(y, big.NewInt(2))
	}

	return x
}

type Reserves struct {
	ReserveETH   *big.Int
	ReserveToken *big.Int
	BlockNumber  uint64
}

func (p *ProfitAnalyzer) getReserves(ctx context.Context, pair common.Address, token common.Address) (*Reserves, error) {
	// Call getReserves on pair
	data := common.FromHex("0x0902f1ac") // getReserves()
	result, err := p.ec.CallContract(ctx, ethereum.CallMsg{
		To:   &pair,
		Data: data,
	}, nil)

	if err != nil {
		return nil, err
	}

	if len(result) < 96 { // 3 * 32 bytes
		return nil, fmt.Errorf("invalid reserves data")
	}

	reserve0 := new(big.Int).SetBytes(result[0:32])
	reserve1 := new(big.Int).SetBytes(result[32:64])

	// Determine which is ETH
	token0Data := common.FromHex("0x0dfe1681") // token0()
	token0Result, _ := p.ec.CallContract(ctx, ethereum.CallMsg{
		To:   &pair,
		Data: token0Data,
	}, nil)

	reserves := &Reserves{}
	if len(token0Result) >= 32 {
		token0 := common.BytesToAddress(token0Result[12:32])
		if token0 == p.dex.WETH() {
			reserves.ReserveETH = reserve0
			reserves.ReserveToken = reserve1
		} else {
			reserves.ReserveETH = reserve1
			reserves.ReserveToken = reserve0
		}
	}

	return reserves, nil
}

func (p *ProfitAnalyzer) getPairAddress(ctx context.Context, token common.Address) (common.Address, error) {
	// Get pair from factory
	factoryABI, _ := abi.JSON(strings.NewReader(v2.FactoryABI))
	data, err := factoryABI.Pack("getPair", token, p.dex.WETH())
	if err != nil {
		return common.Address{}, err
	}

	result, err := p.ec.CallContract(ctx, ethereum.CallMsg{
		To:   &p.dex.Factory(),
		Data: data,
	}, nil)

	if err != nil {
		return common.Address{}, err
	}

	if len(result) < 32 {
		return common.Address{}, fmt.Errorf("no pair found")
	}

	return common.BytesToAddress(result[12:32]), nil
}

const PAIR_ABI = `[
	{"constant":true,"inputs":[],"name":"getReserves","outputs":[
		{"name":"_reserve0","type":"uint112"},
		{"name":"_reserve1","type":"uint112"},
		{"name":"_blockTimestampLast","type":"uint32"}
	],"type":"function"},
	{"constant":true,"inputs":[],"name":"token0","outputs":[{"name":"","type":"address"}],"type":"function"},
	{"constant":true,"inputs":[],"name":"token1","outputs":[{"name":"","type":"address"}],"type":"function"}
]`
