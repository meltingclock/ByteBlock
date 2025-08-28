package helpers

import (
	"crypto/ecdsa"
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// ValidateAddress checks if an address is valid
func ValidateAddress(address string) (common.Address, error) {
	if !common.IsHexAddress(address) {
		return common.Address{}, fmt.Errorf("invalid address format: %s", address)
	}

	addr := common.HexToAddress(address)

	// Check if it's the zero address
	if addr == (common.Address{}) {
		return common.Address{}, fmt.Errorf("zero address not allowed")
	}

	return addr, nil
}

// ValidateAmount checks if amount is positive and within reasonable bounds
func ValidateAmount(amount *big.Int) error {
	if amount == nil {
		return fmt.Errorf("amount is nil")
	}

	if amount.Sign() <= 0 {
		return fmt.Errorf("amount must be positive")
	}

	// Max reasonable amount (1 million ETH)
	maxAmount := new(big.Int).Mul(big.NewInt(1000000), big.NewInt(1e18))
	if amount.Cmp(maxAmount) > 0 {
		return fmt.Errorf("amount exceeds maximum allowed")
	}

	return nil
}

// ValidatePrivateKey validates and returns the private key
func ValidatePrivateKey(privateKeyHex string) (*ecdsa.PrivateKey, common.Address, error) {
	if privateKeyHex == "" {
		return nil, common.Address{}, fmt.Errorf("private key is empty")
	}

	// Remove 0x prefix if present
	privateKeyHex = strings.TrimPrefix(privateKeyHex, "0x")

	// Check length (64 hex chars = 32 bytes)
	if len(privateKeyHex) != 64 {
		return nil, common.Address{}, fmt.Errorf("invalid private key length")
	}

	privateKey, err := crypto.HexToECDSA(privateKeyHex)
	if err != nil {
		return nil, common.Address{}, fmt.Errorf("invalid private key: %w", err)
	}

	publicKey := privateKey.Public()
	publicKeyECDSA, ok := publicKey.(*ecdsa.PublicKey)
	if !ok {
		return nil, common.Address{}, fmt.Errorf("invalid public key type")
	}

	address := crypto.PubkeyToAddress(*publicKeyECDSA)

	return privateKey, address, nil
}

// ValidateTokenPair ensures tokens are different and not zero
func ValidateTokenPair(token0, token1 common.Address) error {
	if token0 == (common.Address{}) || token1 == (common.Address{}) {
		return fmt.Errorf("token addresses cannot be zero")
	}

	if token0 == token1 {
		return fmt.Errorf("token addresses must be different")
	}

	return nil
}

// ValidateGasPrice ensures gas price is within reasonable bounds
func ValidateGasPrice(gasPrice *big.Int, maxGasPrice *big.Int) error {
	if gasPrice == nil {
		return fmt.Errorf("gas price is nil")
	}

	if gasPrice.Sign() <= 0 {
		return fmt.Errorf("gas price must be positive")
	}

	// Min gas price (1 gwei)
	minGasPrice := big.NewInt(1000000000)
	if gasPrice.Cmp(minGasPrice) < 0 {
		return fmt.Errorf("gas price too low")
	}

	// Check against max if provided
	if maxGasPrice != nil && gasPrice.Cmp(maxGasPrice) > 0 {
		return fmt.Errorf("gas price exceeds maximum: %s > %s", gasPrice.String(), maxGasPrice.String())
	}

	return nil
}

// ValidateSlippage ensures slippage is reasonable
func ValidateSlippage(slippagePercent int) error {
	if slippagePercent < 0 || slippagePercent > 50 {
		return fmt.Errorf("slippage must be between 0 and 50 percent")
	}
	return nil
}

// ValidateMinLiquidity checks if liquidity meets minimum requirements
func ValidateMinLiquidity(liquidityETH *big.Int, minRequired *big.Int) (bool, string) {
	if liquidityETH == nil {
		return false, "liquidity amount is nil"
	}

	if minRequired == nil {
		return true, "" // No minimum requirement
	}

	if liquidityETH.Cmp(minRequired) < 0 {
		ethValue := FormatEth(liquidityETH)
		minValue := FormatEth(minRequired)
		return false, fmt.Sprintf("insufficient liquidity: %s ETH < %s ETH minimum", ethValue, minValue)
	}

	return true, ""
}

// IsWETHPair checks if one of the tokens is WETH
func IsWETHPair(token0, token1, weth common.Address) bool {
	return token0 == weth || token1 == weth
}

// GetNonWETHToken returns the token that is not WETH
func GetNonWETHToken(token0, token1, weth common.Address) common.Address {
	if token0 != weth && token0 != (common.Address{}) {
		return token0
	}
	if token1 != weth && token1 != (common.Address{}) {
		return token1
	}
	return common.Address{}
}

// IsDeadAddress checks if address is a burn address
func IsDeadAddress(addr common.Address) bool {
	deadAddresses := []common.Address{
		common.HexToAddress("0x0000000000000000000000000000000000000000"),
		common.HexToAddress("0x000000000000000000000000000000000000dEaD"),
		common.HexToAddress("0x0000000000000000000000000000000000000001"),
	}

	for _, dead := range deadAddresses {
		if addr == dead {
			return true
		}
	}

	return false
}
