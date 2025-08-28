package helpers

import (
	"fmt"
	"math/big"
	"strconv"
	"strings"

	"github.com/ethereum/go-ethereum/common"
)

// ETH to Wei conversion
func EthToWei(ethStr string) (*big.Int, error) {
	if ethStr == "" {
		return nil, fmt.Errorf("empty amount")
	}

	// Clean input
	ethStr = strings.TrimSuffix(strings.ToLower(ethStr), "eth")
	ethStr = strings.TrimSpace(ethStr)

	// Parse as float
	amount, err := strconv.ParseFloat(ethStr, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid amount: %s", ethStr)
	}

	if amount <= 0 {
		return nil, fmt.Errorf("amount must be positive")
	}

	// Convert to wei (multiply by 10^18)
	wei := new(big.Float).SetFloat64(amount)
	wei.Mul(wei, big.NewFloat(1e18))

	// Convert to big.Int
	result := new(big.Int)
	wei.Int(result)

	return result, nil
}

// Wei to ETH formatting
func FormatEth(wei *big.Int) string {
	if wei == nil {
		return "0"
	}

	// Convert wei to ETH (divide by 10^18)
	ethValue := new(big.Float).SetInt(wei)
	ethValue.Quo(ethValue, big.NewFloat(1e18))

	// Format with appropriate precision
	f, _ := ethValue.Float64()
	if f < 0.0001 {
		return fmt.Sprintf("%.8f", f)
	} else if f < 1 {
		return fmt.Sprintf("%.6f", f)
	} else if f < 100 {
		return fmt.Sprintf("%.4f", f)
	}
	return fmt.Sprintf("%.2f", f)
}

// Gwei conversions
func GweiToWei(gweiStr string) (*big.Int, error) {
	if gweiStr == "" {
		return nil, fmt.Errorf("empty gwei amount")
	}

	// Try to parse as integer first
	gwei, ok := new(big.Int).SetString(gweiStr, 10)
	if !ok {
		// Try as float
		gweiFloat, err := strconv.ParseFloat(gweiStr, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid gwei amount: %s", gweiStr)
		}
		// Convert float gwei to wei
		wei := new(big.Float).SetFloat64(gweiFloat * 1e9)
		result := new(big.Int)
		wei.Int(result)
		return result, nil
	}

	// Convert gwei to wei (1 gwei = 10^9 wei)
	return new(big.Int).Mul(gwei, big.NewInt(1000000000)), nil
}

func WeiToGwei(wei *big.Int) string {
	if wei == nil {
		return "0"
	}
	gwei := new(big.Int).Div(wei, big.NewInt(1000000000))
	return gwei.String()
}

// Quick conversion for hardcoded values
func Wei(ethAmount string) *big.Int {
	result, err := EthToWei(ethAmount)
	if err != nil {
		return big.NewInt(0)
	}
	return result
}

// Percentage parsing
func ParsePercentage(input string) (int, error) {
	input = strings.TrimSuffix(input, "%")
	input = strings.TrimSpace(input)

	percentage, err := strconv.Atoi(input)
	if err != nil {
		return 0, fmt.Errorf("invalid percentage: %s", input)
	}

	if percentage <= 0 || percentage > 100 {
		return 0, fmt.Errorf("percentage must be between 1 and 100")
	}

	return percentage, nil
}

// Token amount formatting with decimals
func FormatTokenAmount(amount *big.Int, decimals uint8) string {
	if amount == nil {
		return "0"
	}

	// Create divisor (10^decimals)
	divisor := new(big.Float).SetFloat64(1)
	for i := uint8(0); i < decimals; i++ {
		divisor.Mul(divisor, big.NewFloat(10))
	}

	// Divide amount by divisor
	result := new(big.Float).SetInt(amount)
	result.Quo(result, divisor)

	f, _ := result.Float64()
	if decimals <= 2 {
		return fmt.Sprintf("%.0f", f)
	} else if decimals <= 8 {
		return fmt.Sprintf("%.4f", f)
	}
	return fmt.Sprintf("%.6f", f)
}

// Calculate percentage
func CalculatePercentage(part, whole *big.Int) float64 {
	if whole == nil || whole.Sign() == 0 {
		return 0
	}

	percentage := new(big.Float).SetInt(part)
	percentage.Mul(percentage, big.NewFloat(100))
	percentage.Quo(percentage, new(big.Float).SetInt(whole))

	result, _ := percentage.Float64()
	return result
}

// Calculate slippage adjusted amount
func ApplySlippage(amount *big.Int, slippagePercent int) *big.Int {
	if amount == nil || slippagePercent < 0 || slippagePercent > 100 {
		return amount
	}

	// Calculate (100 - slippage) / 100
	multiplier := big.NewInt(100 - int64(slippagePercent))
	result := new(big.Int).Mul(amount, multiplier)
	result.Div(result, big.NewInt(100))

	return result
}

// Format address for display
func FormatAddress(addr common.Address) string {
	hex := addr.Hex()
	if len(hex) > 10 {
		return hex[:6] + "..." + hex[len(hex)-4:]
	}
	return hex
}

// Format transaction hash for display
func FormatTxHash(hash common.Hash) string {
	hex := hash.Hex()
	if len(hex) > 12 {
		return hex[:10] + "..." + hex[len(hex)-6:]
	}
	return hex
}

// EthToWeiBigInt converts ETH amount (as float) directly to *big.Int
func EthToWeiBigInt(ethAmount float64) *big.Int {
	wei := new(big.Float).SetFloat64(ethAmount * 1e18)
	result := new(big.Int)
	wei.Int(result)
	return result
}

// WeiFromUnits creates wei from amount and decimal places
// e.g., WeiFromUnits(10, 18) = 10 * 10^18 = 10 ETH
func WeiFromUnits(amount int64, decimals int) *big.Int {
	multiplier := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil)
	return new(big.Int).Mul(big.NewInt(amount), multiplier)
}

var (
	// Common ETH amounts in Wei
	Wei1ETH   = big.NewInt(1e18)                                    // 1 ETH (fits in int64)
	Wei10ETH  = new(big.Int).Mul(big.NewInt(10), big.NewInt(1e18))  // 10 ETH
	Wei100ETH = new(big.Int).Mul(big.NewInt(100), big.NewInt(1e18)) // 100 ETH

	// Common Gwei amounts
	GWei1   = big.NewInt(1e9)  // 1 Gwei
	GWei10  = big.NewInt(1e10) // 10 Gwei
	GWei100 = big.NewInt(1e11) // 100 Gwei
)
