package helpers

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
)

// Consolidated ABI definitions
const (
	// Standard ERC20 functions
	ERC20_ABI = `[
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
		},
		{
			"constant": true,
			"inputs": [],
			"name": "name",
			"outputs": [{"name": "", "type": "string"}],
			"type": "function"
		},
		{
			"constant": true,
			"inputs": [],
			"name": "symbol", 
			"outputs": [{"name": "", "type": "string"}],
			"type": "function"
		},
		{
			"constant": true,
			"inputs": [],
			"name": "decimals",
			"outputs": [{"name": "", "type": "uint8"}],
			"type": "function"
		},
		{
			"constant": true,
			"inputs": [],
			"name": "totalSupply",
			"outputs": [{"name": "", "type": "uint256"}],
			"type": "function"
		},
		{
			"constant": true,
			"inputs": [],
			"name": "owner",
			"outputs": [{"name": "", "type": "address"}],
			"type": "function"
		}
	]`

	// Uniswap V2 Pair functions
	PAIR_ABI = `[
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
		},
		{
			"anonymous":false,
			"inputs":[
				{"indexed":true,"name":"sender","type":"address"},
				{"indexed":false,"name":"amount0","type":"uint256"},
				{"indexed":false,"name":"amount1","type":"uint256"}
			],
			"name":"Mint","type":"event"
		}
	]`
)

var (
	erc20ABI abi.ABI
	pairABI  abi.ABI
)

func init() {
	var err error
	erc20ABI, err = abi.JSON(strings.NewReader(ERC20_ABI))
	if err != nil {
		panic(fmt.Sprintf("failed to parse ERC20 ABI: %v", err))
	}

	pairABI, err = abi.JSON(strings.NewReader(PAIR_ABI))
	if err != nil {
		panic(fmt.Sprintf("failed to parse Pair ABI: %v", err))
	}
}

// GetERC20ABI returns the parsed ERC20 ABI
func GetERC20ABI() abi.ABI {
	return erc20ABI
}

// GetPairABI returns the parsed Pair ABI
func GetPairABI() abi.ABI {
	return pairABI
}

// TokenInfo holds basic token information
type TokenInfo struct {
	Address     common.Address
	Name        string
	Symbol      string
	Decimals    uint8
	TotalSupply *big.Int
	Owner       common.Address
	HasOwner    bool
	IsRenounced bool
}

// PairReserves holds pair reserve information
type PairReserves struct {
	Reserve0           *big.Int
	Reserve1           *big.Int
	BlockTimestampLast uint32
	Token0             common.Address
	Token1             common.Address
}

// GetPairAddress gets the pair address for two tokens using factory
func GetPairAddress(ctx context.Context, client *ethclient.Client, factory common.Address, tokenA, tokenB common.Address) (common.Address, error) {
	factoryABI, err := abi.JSON(strings.NewReader(v2.FactoryABI))
	if err != nil {
		return common.Address{}, err
	}

	data, err := factoryABI.Pack("getPair", tokenA, tokenB)
	if err != nil {
		return common.Address{}, err
	}

	result, err := client.CallContract(ctx, ethereum.CallMsg{
		To:   &factory,
		Data: data,
	}, nil)

	if err != nil {
		return common.Address{}, err
	}

	var pairAddress common.Address
	err = factoryABI.UnpackIntoInterface(&pairAddress, "getPair", result)
	if err != nil {
		return common.Address{}, err
	}

	return pairAddress, nil
}

// GetPairReserves fetches pair reserves and token addresses
func GetPairReserves(ctx context.Context, client *ethclient.Client, pairAddress common.Address) (*PairReserves, error) {
	if pairAddress == (common.Address{}) {
		return nil, fmt.Errorf("pair does not exist")
	}

	// Get reserves
	reservesData, err := pairABI.Pack("getReserves")
	if err != nil {
		return nil, err
	}

	reservesResult, err := client.CallContract(ctx, ethereum.CallMsg{
		To:   &pairAddress,
		Data: reservesData,
	}, nil)

	if err != nil {
		return nil, err
	}

	var reserves struct {
		Reserve0           *big.Int
		Reserve1           *big.Int
		BlockTimestampLast uint32
	}

	err = pairABI.UnpackIntoInterface(&reserves, "getReserves", reservesResult)
	if err != nil {
		return nil, err
	}

	// Get token0
	token0Data, err := pairABI.Pack("token0")
	if err != nil {
		return nil, err
	}

	token0Result, err := client.CallContract(ctx, ethereum.CallMsg{
		To:   &pairAddress,
		Data: token0Data,
	}, nil)

	if err != nil {
		return nil, err
	}

	var token0 common.Address
	err = pairABI.UnpackIntoInterface(&token0, "token0", token0Result)
	if err != nil {
		return nil, err
	}

	// Get token1
	token1Data, err := pairABI.Pack("token1")
	if err != nil {
		return nil, err
	}

	token1Result, err := client.CallContract(ctx, ethereum.CallMsg{
		To:   &pairAddress,
		Data: token1Data,
	}, nil)

	if err != nil {
		return nil, err
	}

	var token1 common.Address
	err = pairABI.UnpackIntoInterface(&token1, "token1", token1Result)
	if err != nil {
		return nil, err
	}

	return &PairReserves{
		Reserve0:           reserves.Reserve0,
		Reserve1:           reserves.Reserve1,
		BlockTimestampLast: reserves.BlockTimestampLast,
		Token0:             token0,
		Token1:             token1,
	}, nil
}

// GetOrderedReserves returns reserves in order matching the provided tokens
func GetOrderedReserves(ctx context.Context, client *ethclient.Client, pairAddress, tokenA, tokenB common.Address) ([2]*big.Int, error) {
	reserves, err := GetPairReserves(ctx, client, pairAddress)
	if err != nil {
		return [2]*big.Int{}, err
	}

	// Return reserves in order: tokenA reserve, tokenB reserve
	if reserves.Token0 == tokenA {
		return [2]*big.Int{reserves.Reserve0, reserves.Reserve1}, nil
	}
	return [2]*big.Int{reserves.Reserve1, reserves.Reserve0}, nil
}

// GetTokenInfo retrieves comprehensive token information
func GetTokenInfo(ctx context.Context, client *ethclient.Client, tokenAddress common.Address) (*TokenInfo, error) {
	info := &TokenInfo{
		Address: tokenAddress,
	}

	// Get name
	if nameData, err := erc20ABI.Pack("name"); err == nil {
		if result, err := client.CallContract(ctx, ethereum.CallMsg{
			To: &tokenAddress, Data: nameData}, nil); err == nil && len(result) > 0 {
			_ = erc20ABI.UnpackIntoInterface(&info.Name, "name", result)
		}
	}

	// Get symbol
	if symbolData, err := erc20ABI.Pack("symbol"); err == nil {
		if result, err := client.CallContract(ctx, ethereum.CallMsg{
			To: &tokenAddress, Data: symbolData}, nil); err == nil && len(result) > 0 {
			_ = erc20ABI.UnpackIntoInterface(&info.Symbol, "symbol", result)
		}
	}

	// Get decimals
	if decimalsData, err := erc20ABI.Pack("decimals"); err == nil {
		if result, err := client.CallContract(ctx, ethereum.CallMsg{
			To: &tokenAddress, Data: decimalsData}, nil); err == nil && len(result) > 0 {
			_ = erc20ABI.UnpackIntoInterface(&info.Decimals, "decimals", result)
		}
	}

	// Get total supply
	if supplyData, err := erc20ABI.Pack("totalSupply"); err == nil {
		if result, err := client.CallContract(ctx, ethereum.CallMsg{
			To: &tokenAddress, Data: supplyData}, nil); err == nil && len(result) > 0 {
			_ = erc20ABI.UnpackIntoInterface(&info.TotalSupply, "totalSupply", result)
		}
	}

	// Get owner (if exists)
	if ownerData, err := erc20ABI.Pack("owner"); err == nil {
		if result, err := client.CallContract(ctx, ethereum.CallMsg{
			To: &tokenAddress, Data: ownerData}, nil); err == nil && len(result) >= 32 {
			info.Owner = common.BytesToAddress(result[12:32])
			zeroAddr := common.HexToAddress("0x0000000000000000000000000000000000000000")
			deadAddr := common.HexToAddress("0x000000000000000000000000000000000000dEaD")

			if info.Owner == zeroAddr || info.Owner == deadAddr {
				info.IsRenounced = true
				info.HasOwner = false
			} else {
				info.HasOwner = true
			}
		}
	}

	return info, nil
}

// GetTokenBalance gets the balance of a token for an address
func GetTokenBalance(ctx context.Context, client *ethclient.Client, tokenAddress, holderAddress common.Address) (*big.Int, error) {
	data, err := erc20ABI.Pack("balanceOf", holderAddress)
	if err != nil {
		return nil, err
	}

	result, err := client.CallContract(ctx, ethereum.CallMsg{
		To:   &tokenAddress,
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

// GetTokenAllowance gets the allowance of a token for owner->spender
func GetTokenAllowance(ctx context.Context, client *ethclient.Client, tokenAddress, owner, spender common.Address) (*big.Int, error) {
	data, err := erc20ABI.Pack("allowance", owner, spender)
	if err != nil {
		return nil, err
	}

	result, err := client.CallContract(ctx, ethereum.CallMsg{
		To:   &tokenAddress,
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

// CalculateAmountOut calculates output amount using Uniswap V2 formula
func CalculateAmountOut(amountIn, reserveIn, reserveOut *big.Int) *big.Int {
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

// CalculatePriceImpact calculates the price impact of a trade
func CalculatePriceImpact(amountIn, reserveIn *big.Int) float64 {
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
