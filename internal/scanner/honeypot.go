package scanner

import (
	"context"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
	v2 "github.com/meltingclock/biteblock_v1/internal/dex/v2"
	"github.com/meltingclock/biteblock_v1/internal/helpers"
	"github.com/meltingclock/biteblock_v1/internal/telemetry"
)

type HoneypotChecker struct {
	ec        *ethclient.Client
	router    common.Address
	routerABI abi.ABI
	weth      common.Address
	factory   common.Address

	// Cache with TTL
	cache    map[common.Address]*cacheEntry
	cacheMu  sync.RWMutex
	cacheTTL time.Duration
}

type cacheEntry struct {
	safety    *TokenSafety
	timestamp time.Time
}

type TokenSafety struct {
	Token       common.Address
	Symbol      string
	Name        string
	Decimals    uint8
	TotalSupply *big.Int

	// Core safety checks
	CanBuy      bool
	CanSell     bool
	CanApprove  bool
	IsHoneypot  bool
	SafetyScore int     // 0-100, higher is safer
	BuyTax      float64 // Percentage
	SellTax     float64 // Percentage
	TransferTax float64 // Percentage

	// Ownership analysis
	HasOwner         bool
	OwnerAddress     common.Address
	IsRenounced      bool
	HasMintFunction  bool
	HasPauseFunction bool
	HasBlacklist     bool
	MaxWalletLimit   bool

	// Liquidity analysis
	LiquidityETH    *big.Int
	LiquidityTokens *big.Int
	LiquidityLocked bool
	LockedUntil     time.Time

	// Risk factors
	RiskFactors []string

	// Simulation results
	SimulationError string

	// Add timestamp for caching
	CheckedAt int64 // Unix timestamp of when checks was performed
}

func NewHoneypotChecker(ec *ethclient.Client, dex *v2.Registry) *HoneypotChecker {
	routerABI, _ := abi.JSON(strings.NewReader(v2.RouterABI))
	h := &HoneypotChecker{
		ec:        ec,
		router:    dex.Router(),
		routerABI: routerABI,
		weth:      dex.WETH(),
		factory:   dex.Factory(),
		cache:     make(map[common.Address]*cacheEntry),
		cacheTTL:  5 * time.Minute, // Cache for 5 minutes
	}

	go h.cleanupCache()

	return h
}

func (h *HoneypotChecker) cleanupCache() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		h.cacheMu.Lock()
		now := time.Now()
		for addr, entry := range h.cache {
			if now.Sub(entry.timestamp) > h.cacheTTL {
				delete(h.cache, addr)
			}
		}
		h.cacheMu.Unlock()
	}

}

func (h *HoneypotChecker) CheckToken(ctx context.Context, token common.Address) (*TokenSafety, error) {
	// Check cache first
	h.cacheMu.RLock()
	if entry, exists := h.cache[token]; exists {
		if time.Since(entry.timestamp) < h.cacheTTL {
			h.cacheMu.RUnlock()
			telemetry.Debugf("[honeypot] cache hit for %s", token.Hex())
			return entry.safety, nil
		}
	}
	h.cacheMu.RUnlock()

	safety := &TokenSafety{
		Token:       token,
		SafetyScore: 100,
		RiskFactors: []string{},
		IsHoneypot:  false,
		CheckedAt:   time.Now().Unix(),
	}

	// Set timeout for all checks
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	// Run checks in parallel for speed
	var wg sync.WaitGroup
	//r mu sync.Mutex

	// 1. Basic token info
	wg.Add(1)
	go func() {
		defer wg.Done()
		h.getTokenInfo(ctx, safety)
	}()

	// 2. Ownership check
	wg.Add(1)
	go func() {
		defer wg.Done()
		h.checkOwnership(ctx, safety)
	}()

	// 3. Dangerous functions check
	wg.Add(1)
	go func() {
		defer wg.Done()
		h.checkDangerousFunctions(ctx, safety)
	}()

	// Wait for parallel checks
	wg.Wait()

	// 4. Simulate trades (sequential - depends on other checks)
	h.simulateTrades(ctx, safety)

	// 5. Check liquidity
	h.checkLiquidity(ctx, safety)

	// 6. Calculate final score
	h.calculateFinalScore(safety)

	// Cache the result
	h.cacheMu.Lock()
	h.cache[token] = &cacheEntry{
		safety:    safety,
		timestamp: time.Now(),
	}
	h.cacheMu.Unlock()

	return safety, nil
}

// Quick safety check for auto-buy (lightweight version)
func (h *HoneypotChecker) QuickCheck(ctx context.Context, token common.Address) (bool, error) {
	// Check cache first
	h.cacheMu.RLock()
	if entry, exists := h.cache[token]; exists {
		if time.Since(entry.timestamp) < h.cacheTTL {
			h.cacheMu.RUnlock()
			return !entry.safety.IsHoneypot && entry.safety.SafetyScore >= 40, nil
		}
	}
	h.cacheMu.RUnlock()

	// Quick checks only
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	// Check for contract code
	code, err := h.ec.CodeAt(ctx, token, nil)
	if err != nil || len(code) == 0 {
		return false, fmt.Errorf("no contract code")
	}

	// Quick honeypot pattern check
	codeHex := common.Bytes2Hex(code)
	dangerousPatterns := []string{
		"b515566a", // addBots
		"273123b7", // delBots
		"e47d6060", // isBlacklisted
	}

	for _, pattern := range dangerousPatterns {
		if strings.Contains(codeHex, pattern) {
			return false, nil
		}
	}
	return true, nil
}

// getTokenInfo retrieves basic token information
func (h *HoneypotChecker) getTokenInfo(ctx context.Context, safety *TokenSafety) {
	// Try to get token info
	symbolData, err := h.ec.CallContract(ctx, ethereum.CallMsg{
		To:   &safety.Token,
		Data: common.FromHex("0x95d89b41"), // symbol()
	}, nil)
	if err == nil && len(symbolData) > 0 {
		safety.Symbol = strings.TrimSpace(string(symbolData))
	}

	// Try to get token name
	nameData, err := h.ec.CallContract(ctx, ethereum.CallMsg{
		To:   &safety.Token,
		Data: common.FromHex("0x06fdde03"), // name()
	}, nil)
	if err == nil && len(nameData) > 0 {
		safety.Name = strings.TrimSpace(string(nameData))
	}

	// Try to get decimals
	decimalsData, err := h.ec.CallContract(ctx, ethereum.CallMsg{
		To:   &safety.Token,
		Data: common.FromHex("0x313ce567"), // decimals()
	}, nil)
	if err == nil && len(decimalsData) > 0 {
		safety.Decimals = uint8(new(big.Int).SetBytes(decimalsData).Uint64())
	}

	// Try to get total supply
	supplyData, err := h.ec.CallContract(ctx, ethereum.CallMsg{
		To:   &safety.Token,
		Data: common.FromHex("0x18160ddd"), // totalSupply()
	}, nil)
	if err == nil && len(supplyData) > 0 {
		safety.TotalSupply = new(big.Int).SetBytes(supplyData)
	}
}

// CheckOwnership checks token ownership status
func (h *HoneypotChecker) checkOwnership(ctx context.Context, safety *TokenSafety) {
	// Check for owner() function
	ownerData, err := h.ec.CallContract(ctx, ethereum.CallMsg{
		To:   &safety.Token,
		Data: common.FromHex("0x8da5cb5b"), // owner()
	}, nil)

	if err == nil && len(ownerData) >= 32 {
		safety.OwnerAddress = common.BytesToAddress(ownerData[12:32])
		zeroAddr := common.HexToAddress("0x0000000000000000000000000000000000000000")
		deadAddr := common.HexToAddress("0x000000000000000000000000000000000000dEaD")

		if safety.OwnerAddress == zeroAddr || safety.OwnerAddress == deadAddr {
			safety.IsRenounced = true
			safety.HasOwner = false
			telemetry.Debugf("[honeypot] ownership renounced for %s", safety.Token.Hex())
		} else {
			safety.HasOwner = true
			safety.SafetyScore -= 10 // Deduct points for active ownership
			safety.RiskFactors = append(safety.RiskFactors, "has_owner")
			telemetry.Debugf("[honeypot] owner found: %s", safety.OwnerAddress.Hex())
		}
	}
}

// checkDangerousFunctions scans for risky functions in bytecode
func (h *HoneypotChecker) checkDangerousFunctions(ctx context.Context, safety *TokenSafety) {
	code, err := h.ec.CodeAt(ctx, safety.Token, nil)
	if err != nil || len(code) == 0 {
		safety.SafetyScore = 0
		safety.IsHoneypot = true
		safety.RiskFactors = append(safety.RiskFactors, "no_contract_code")
		return
	}

	codeHex := common.Bytes2Hex(code)

	// Define dangerous functions selectors
	dangerousFunctions := map[string]struct {
		name     string
		risk     int
		critical bool
	}{
		"ffb2c479": {"setMaxWallet", 15, false},      // Max wallet limits
		"f2fde38b": {"transferOwnership", 10, false}, // Can change owner
		"e8078d94": {"openTrading", 20, true},        // Trading can be disabled
		"c9567bf9": {"openTrading", 20, true},        // Alternative openTrading
		"8da5cb5b": {"owner", 5, false},              // Has owner function
		"5c975abb": {"paused", 25, true},             // Can be paused
		"8456cb59": {"pause", 25, true},              // Pause function
		"3f4ba83a": {"unpause", 25, true},            // Unpause function
		"40c10f19": {"mint", 30, true},               // Can mint new tokens
		"42966c68": {"burn", 5, false},               // Can burn tokens
		"79cc6790": {"burnFrom", 10, false},          // Can burn from addresses
		"a9059cbb": {"transfer", 0, false},           // Normal transfer (expected)
		"23b872dd": {"transferFrom", 0, false},       // Normal transferFrom (expected)
		"095ea7b3": {"approve", 0, false},            // Normal approve (expected)
		"dd62ed3e": {"allowance", 0, false},          // Normal allowance (expected)
		"70a08231": {"balanceOf", 0, false},          // Normal balanceOf (expected)
		"18160ddd": {"totalSupply", 0, false},        // Normal totalSupply (expected)
		"06fdde03": {"name", 0, false},               // Normal name (expected)
		"95d89b41": {"symbol", 0, false},             // Normal symbol (expected)
		"313ce567": {"decimals", 0, false},           // Normal decimals (expected)

		// Honeypot-specific functions
		"b515566a": {"addBots", 50, true},                // Bot blacklist
		"273123b7": {"delBots", 50, true},                // Bot whitelist
		"e47d6060": {"isBlacklisted", 50, true},          // Blacklist check
		"3b124fe7": {"_taxFee", 10, false},               // Tax fee variable
		"cf0848f7": {"excludeFromFee", 15, false},        // Fee exclusion
		"437823ec": {"includeInFee", 15, false},          // Fee inclusion
		"4a74bb02": {"swapAndLiquifyEnabled", 10, false}, // Swap control
		"c0246668": {"excludeFromMaxTx", 15, false},      // Transaction limit control
		"f11a24d3": {"buyTax", 10, false},                // Buy tax
		"7d1db4a5": {"maxTx", 20, true},                  // Max transaction
		"8f9a55c0": {"maxWallet", 20, true},              // Max wallet
	}

	// Check for each dangerous function
	for selector, info := range dangerousFunctions {
		if strings.Contains(codeHex, selector) {
			if info.risk > 0 {
				safety.SafetyScore -= info.risk
				safety.RiskFactors = append(safety.RiskFactors, info.name)

				if info.critical {
					telemetry.Warnf("[honeypot] CRITICAL function found: %s in %s",
						info.name, safety.Token.Hex())
				} else {
					telemetry.Debugf("[honeypot] risky function found: %s in %s",
						info.name, safety.Token.Hex())
				}
			}

			// Set specific flags
			switch info.name {
			case "mint":
				safety.HasMintFunction = true
			case "pause", "paused", "unpause":
				safety.HasPauseFunction = true
			case "addBots", "delBots", "isBlacklisted":
				safety.HasBlacklist = true
				safety.IsHoneypot = true // Blacklist = automatic honeypot
			case "maxWallet", "setMaxWallet", "maxTx":
				safety.MaxWalletLimit = true
			}
		}
	}

	// Check for specific honeypot patterns
	honeypotPatterns := []struct {
		pattern string
		name    string
		risk    int
	}{
		{"736e697065726368656172676573", "snipercharges", 50},
		{"626c61636b6c697374", "blacklist", 50},
		{"77686974656c697374", "whitelist", 30},
		{"616e746920626f74", "anti bot", 40},
		{"616e746920736e697065", "anti snipe", 40},
	}

	for _, pattern := range honeypotPatterns {
		if strings.Contains(codeHex, pattern.pattern) {
			safety.SafetyScore -= pattern.risk
			safety.RiskFactors = append(safety.RiskFactors, pattern.name)
			telemetry.Warnf("[honeypot] pattern detected: %s", pattern.name)
		}
	}
}

// simulateTrades simulates buy and sell to detect honeypots
func (h *HoneypotChecker) simulateTrades(ctx context.Context, safety *TokenSafety) {
	// Test with small amount (0.001 ETH)
	testAmount := big.NewInt(1e15) // 0.001 ETH in wei

	// Use a test address (not zero address, as some contracts check for it)
	testAddr := common.HexToAddress("0x0000000000000000000000000000000000000001")

	// Simulate buy
	buyPath := []common.Address{h.weth, safety.Token}
	deadline := big.NewInt(time.Now().Add(5 * time.Minute).Unix())

	buyData, err := h.routerABI.Pack(
		"swapExactETHForTokens",
		big.NewInt(0), // amountOutMin
		buyPath,
		testAddr,
		deadline,
	)
	if err != nil {
		safety.SimulationError = fmt.Sprintf("pack buy failed: %v", err)
		safety.SafetyScore -= 20
		return
	}

	// Call buy simulation
	buyResult, buyErr := h.ec.CallContract(ctx, ethereum.CallMsg{
		From:  testAddr,
		To:    &h.router,
		Value: testAmount,
		Data:  buyData,
	}, nil)

	if buyErr != nil {
		safety.CanBuy = false
		safety.SimulationError = fmt.Sprintf("buy simulation failed: %v", buyErr)
		safety.SafetyScore -= 30
		safety.RiskFactors = append(safety.RiskFactors, "cannot_buy")
		telemetry.Debugf("[honeypot] buy simulation failed for %s: %v", safety.Token.Hex(), buyErr)
	} else {
		safety.CanBuy = true

		// Decode buy result to get token amount
		var amounts []*big.Int
		err := h.routerABI.UnpackIntoInterface(&amounts, "swapExactETHForTokens", buyResult)
		if err == nil && len(amounts) > 1 {
			tokenAmount := amounts[len(amounts)-1]

			// Calculate buy tax
			// This is simplified - in production we will calculate expected amount properly
			if tokenAmount.Sign() > 0 {
				// Simulate sell
				sellPath := []common.Address{safety.Token, h.weth}

				sellData, err := h.routerABI.Pack(
					"swapExactTokensForETH",
					tokenAmount,
					big.NewInt(0), // amountOutMin
					sellPath,
					testAddr,
					deadline,
				)
				if err != nil {
					safety.SimulationError = fmt.Sprintf("pack sell failed: %v", err)
					safety.SafetyScore -= 20
					return
				}

				// First simulate approve (some honeypots block here)
				approveData := common.FromHex("0x095ea7b3") // approve(address,uint256)
				approveData = append(approveData, common.LeftPadBytes(h.router.Bytes(), 32)...)
				approveData = append(approveData, common.LeftPadBytes(tokenAmount.Bytes(), 32)...)

				_, approveErr := h.ec.CallContract(ctx, ethereum.CallMsg{
					From: testAddr,
					To:   &safety.Token,
					Data: approveData,
				}, nil)

				if approveErr != nil {
					safety.CanApprove = false
					safety.IsHoneypot = true
					safety.SafetyScore = 0
					safety.RiskFactors = append(safety.RiskFactors, "cannot_approve")
					safety.SimulationError = fmt.Sprintf("approve blocked: %v", approveErr)
					telemetry.Warnf("[honeypot] HONEYPOT! Cannot approve %s", safety.Token.Hex())
					return
				}
				safety.CanApprove = true

				// Now simulate sell
				sellResult, sellErr := h.ec.CallContract(ctx, ethereum.CallMsg{
					From: testAddr,
					To:   &h.router,
					Data: sellData,
				}, nil)

				if sellErr != nil {
					safety.CanSell = false
					safety.IsHoneypot = true
					safety.SafetyScore = 0
					safety.RiskFactors = append(safety.RiskFactors, "cannot_sell")
					safety.SimulationError = fmt.Sprintf("sell blocked: %v", sellErr)
					telemetry.Warnf("[honeypot] HONEYPOT DETECTED! Cannot sell %s", safety.Token.Hex())
				} else {
					safety.CanSell = true

					// Decode sell result to calculate tax
					var sellAmounts []*big.Int
					err := h.routerABI.UnpackIntoInterface(&sellAmounts, "swapExactTokensForETH", sellResult)
					if err == nil && len(sellAmounts) > 1 {
						ethBack := sellAmounts[len(sellAmounts)-1]

						// Calculate round-trip tax
						loss := new(big.Int).Sub(testAmount, ethBack)
						if loss.Sign() > 0 {
							taxPercent := new(big.Int).Mul(loss, big.NewInt(100))
							taxPercent.Div(taxPercent, testAmount)
							totalTax := float64(taxPercent.Int64())

							// Rough estimate: half buy, half sell
							safety.BuyTax = totalTax / 2
							safety.SellTax = totalTax / 2

							if totalTax > 50 {
								safety.IsHoneypot = true
								safety.SafetyScore = 0
								safety.RiskFactors = append(safety.RiskFactors, "excessive_tax")
								telemetry.Warnf("[honeypot] Excessive tax detected: %.1f%%", totalTax)
							} else if totalTax > 25 {
								safety.SafetyScore -= 30
								safety.RiskFactors = append(safety.RiskFactors, "high_tax")
							} else if totalTax > 10 {
								safety.SafetyScore -= 15
								safety.RiskFactors = append(safety.RiskFactors, "moderate_tax")
							} else if totalTax < 10 {
								safety.SafetyScore -= 5
								safety.RiskFactors = append(safety.RiskFactors, "low_tax")
							}
						}
					}
				}
			}
		}
	}
}

// checkLiquidity analyzes liquidity pool
func (h *HoneypotChecker) checkLiquidity(ctx context.Context, safety *TokenSafety) {
	// Get pair address
	factoryABI, _ := abi.JSON(strings.NewReader(v2.FactoryABI))
	getPairData, err := factoryABI.Pack("getPair", safety.Token, h.weth)
	if err != nil {
		return
	}

	pairResult, err := h.ec.CallContract(ctx, ethereum.CallMsg{
		To:   &h.factory,
		Data: getPairData,
	}, nil)
	if err != nil || len(pairResult) < 32 {
		return
	}

	pairAddress := common.BytesToAddress(pairResult[12:32])
	if pairAddress == (common.Address{}) {
		safety.SafetyScore -= 50
		safety.RiskFactors = append(safety.RiskFactors, "no_liquidity_pool")
		return
	}

	// Get reserves
	reservesData := common.FromHex("0x0902f1ac") // getReserves()
	reservesResult, err := h.ec.CallContract(ctx, ethereum.CallMsg{
		To:   &pairAddress,
		Data: reservesData,
	}, nil)

	if err == nil && len(reservesResult) >= 64 {
		reserve0 := new(big.Int).SetBytes(reservesResult[0:32])
		reserve1 := new(big.Int).SetBytes(reservesResult[32:64])

		// Determine which reserve is WETH
		token0Data := common.FromHex("0x0dfe1681") // token0()
		token0Result, _ := h.ec.CallContract(ctx, ethereum.CallMsg{
			To:   &pairAddress,
			Data: token0Data,
		}, nil)

		if len(token0Result) >= 32 {
			token0 := common.BytesToAddress(token0Result[12:32])
			if token0 == h.weth {
				safety.LiquidityETH = reserve0
				safety.LiquidityTokens = reserve1
			} else {
				safety.LiquidityETH = reserve1
				safety.LiquidityTokens = reserve0
			}

			// Check minimum liquidity (0.5 ETH minimum)
			minLiqudity := big.NewInt(5e17) // 0.5 ETH
			if safety.LiquidityETH.Cmp(minLiqudity) < 0 {
				safety.SafetyScore -= 40
				safety.RiskFactors = append(safety.RiskFactors, "low_liquidity")
				telemetry.Debugf("[honeypot] Low liquidity: %s ETH", helpers.FormatEth(safety.LiquidityETH))
			}
		}
	}
}

// calculateFinalScore computes final safety assessment
func (h *HoneypotChecker) calculateFinalScore(safety *TokenSafety) {
	// Ensure score is within bounds
	if safety.SafetyScore < 0 {
		safety.SafetyScore = 0
	}
	if safety.SafetyScore > 100 {
		safety.SafetyScore = 100
	}

	// FFinal honeypot determination
	if safety.IsHoneypot {
		safety.SafetyScore = 0
		return
	}

	// Check critical factors
	if !safety.CanSell {
		safety.IsHoneypot = true
		safety.SafetyScore = 0
	} else if safety.HasBlacklist {
		safety.IsHoneypot = true
		safety.SafetyScore = 0
	} else if safety.SafetyScore < 30 {
		// Too many risk factors
		safety.IsHoneypot = true
	}

	telemetry.Infof("[honeypot] Final score for %s: %d/100 (honeypot: %v)",
		safety.Token.Hex(), safety.SafetyScore, safety.IsHoneypot)
}
