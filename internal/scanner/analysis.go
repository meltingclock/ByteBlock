package scanner

import (
	"math/big"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
	v2 "github.com/meltingclock/biteblock_v1/internal/dex/v2"
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
}
