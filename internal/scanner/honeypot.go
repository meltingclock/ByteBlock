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

type HoneypotChecker struct {
	ec        *ethclient.Client
	router    common.Address
	routerABI abi.ABI
	weth      common.Address
}

func NewHoneypotChecker(ec *ethclient.Client, dex *v2.Registry) *HoneypotChecker {
	routerABI, _ := abi.JSON(strings.NewReader(v2.RouterABI))
	return &HoneypotChecker{
		ec:        ec,
		router:    dex.Router(),
		routerABI: routerABI,
		weth:      dex.WETH(),
	}
}

type TokenSafety struct {
	Token        common.Address
	CanBuy       bool
	CanSell      bool
	BuyTax       float64
	SellTax      float64
	IsHoneypot   bool
	SafetyScore  int // 0-100, higher is safer
	HasOwner     bool
	OwnerAddress common.Address
}

func (h *HoneypotChecker) CheckToken(ctx context.Context, token common.Address) (*TokenSafety, error) {
	safety := &TokenSafety{
		Token:       token,
		SafetyScore: 0,
		IsHoneypot:  true, // Start pessimistic
	}

	// 1. Check contract exists
	code, err := h.ec.CodeAt(ctx, token, nil)
	if err != nil || len(code) == 0 {
		return safety, fmt.Errorf("no contract address")
	}

	// 2, Check for owner
	ownerData, _ := h.ec.CallContract(ctx, ethereum.CallMsg{
		To:   &token,
		Data: common.FromHex("0x8da5cb5b"), // owner()
	}, nil)

	if len(ownerData) >= 32 {
		safety.OwnerAddress = common.BytesToAddress(ownerData[12:32])
		safety.HasOwner = safety.OwnerAddress != common.HexToAddress("0x0")
		if !safety.HasOwner {
			safety.SafetyScore += 20 // Renounced ownership is good
		}
	}

	// 3. Simulate buy transaction
	testAmount := big.NewInt(1e16) // 0.01 ETH
	buyData, _ := h.routerABI.Pack("swapExactETHForTokens",
		big.NewInt(0), // amountOutMin
		[]common.Address{h.weth, token},
		common.HexToAddress("0x0000000000000000000000000000000000000001"),
		new(big.Int).SetInt64(1999999999), // far future deadline
	)

	buyResult, buyErr := h.ec.CallContract(ctx, ethereum.CallMsg{
		From:  common.HexToAddress("0x0000000000000000000000000000000000000001"),
		To:    &h.router,
		Value: testAmount,
		Data:  buyData,
	}, nil)

	safety.CanBuy = (buyErr == nil && len(buyResult) > 0)
	if safety.CanBuy {
		safety.SafetyScore += 30

		// 4. Decode buy result to get token amount
		var amounts []*big.Int
		if err := h.routerABI.UnpackIntoInterface(&amounts, "swapExactETHForTokens", buyResult); err == nil && len(amounts) > 1 {
			tokenAmount := amounts[1]

			// 5. Simulate sell transaction
			sellData, _ := h.routerABI.Pack("swapExactTokensForETH",
				tokenAmount,
				big.NewInt(0), // amountOutMin
				[]common.Address{token, h.weth},
				common.HexToAddress("0x0000000000000000000000000000000000000001"),
				new(big.Int).SetInt64(1999999999),
			)

			sellResult, sellErr := h.ec.CallContract(ctx, ethereum.CallMsg{
				From: common.HexToAddress("0x0000000000000000000000000000000000000001"),
				To:   &h.router,
				Data: sellData,
			}, nil)

			safety.CanSell = (sellErr == nil && len(sellResult) > 0)
			if safety.CanSell {
				safety.SafetyScore += 40
				safety.IsHoneypot = false

				// Calculate Tax
				var sellAmounts []*big.Int
				if err := h.routerABI.UnpackIntoInterface(&sellAmounts, "swapExactTokensForETH", sellResult); err == nil && len(sellAmounts) > 1 {
					ethBack := sellAmounts[1]
					// Simple tax calculation
					loss := new(big.Int).Sub(testAmount, ethBack)
					taxPercent := new(big.Int).Mul(loss, big.NewInt(100))
					taxPercent.Div(taxPercent, testAmount)
					safety.SellTax = float64(taxPercent.Int64())

					if safety.SellTax > 50 {
						safety.SafetyScore -= 30
						safety.IsHoneypot = true // too high tax = honeypot
					} else if safety.SellTax > 10 {
						safety.SafetyScore -= 10 // moderate tax
					}
				}
			}
		}
	}

	// 6. Check for suspicious functions in bytecode
	codeHex := common.Bytes2Hex(code)
	suspiciousSelectors := map[string]string{
		"0ec7dd7c": "enableTrading",
		"39509351": "increaseAllowance",
		"dd62ed3e": "allowance",
		"c9567bf9": "openTrading",
		"6ddd1713": "swapEnabled",
	}

	for sel, name := range suspiciousSelectors {
		if strings.Contains(codeHex, sel) {
			safety.SafetyScore -= 5
			telemetry.Debugf("[honeypot] found suspicious function: %s", name)
		}
	}

	return safety, nil
}
