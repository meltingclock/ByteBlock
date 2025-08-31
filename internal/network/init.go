package network

import "github.com/ethereum/go-ethereum/common"

type NetPreset struct {
	WSSURL       string
	Factory      common.Address
	Router       common.Address
	WETH         common.Address
	ChainID      int64
	InitCodeHash string
}

var Network = map[string]NetPreset{
	"ethereum": {
		WSSURL:       "ws://127.0.0.1:8545",
		Factory:      common.HexToAddress("0x5C69bEe701ef814a2B6a3EDD4B1652CB9cc5aA6f"), // Uniswap V2
		Router:       common.HexToAddress("0x7a250d5630B4cF539739dF2C5dAcb4c659F2488D"), // Uniswap V2
		WETH:         common.HexToAddress("0xC02aaA39b223FE8D0A0e5C4F27eAD9083C756Cc2"),
		ChainID:      1,
		InitCodeHash: UniswapV2InitCode, // Uniswap V2
	},
	"bsc": {
		WSSURL:       "wss://bsc-ws-node.nariox.org:443",
		Factory:      common.HexToAddress("0xBCfCcbde45cE874adCB698cC183deBcF17952812"), // Pancake V2
		Router:       common.HexToAddress("0x10ED43C718714eb63d5aA57B78B54704E256024E"), // Pancake V2
		WETH:         common.HexToAddress("0xBB4CdB9CBd36B01bD1cBaEBF2De08d9173bc095c"), // WBNB
		ChainID:      56,
		InitCodeHash: PancakeV2InitCode, // PancakeSwap V2
	},
	"base": {
		WSSURL:       "wss://base-mainnet.g.alchemy.com/v2/<KEY>",
		Factory:      common.HexToAddress("0x8909Dc15e40173Ff4699343b6eB8132c65e18eC6"), // BaseSwap/Uniswap V2
		Router:       common.HexToAddress("0x4752ba5DBc23f44D87826276BF6Fd6b1C372aD24"), // BaseSwap router
		WETH:         common.HexToAddress("0x4200000000000000000000000000000000000006"),
		ChainID:      8453,
		InitCodeHash: BaseSwapInitCode, // Same as Uniswap
	},
}

// Common init code hashes for reference:
const (
	// Ethereum Mainnet
	UniswapV2InitCode = "96e8ac4277198ff8b6f785478aa9a39f403cb768dd02cbee326c3e7da348845f"
	SushiSwapInitCode = "e18a34eb0e04b04f7a0ac29a6e80748dca96319b42c54d679cb821dca90c6303"

	// BSC
	PancakeV2InitCode = "00fb7f630766e6a796048ea87d01acd3068e8ff67d078148a3fa3f4a84f69bd5"
	PancakeV2NewCode  = "57224589c67f3f30a6b0d7a1b54cf3153ab84563bc609ef41dfb34f8b2974d2d" // New factory

	// Base
	BaseSwapInitCode  = "96e8ac4277198ff8b6f785478aa9a39f403cb768dd02cbee326c3e7da348845f"
	AerodromeInitCode = "1f8a01833e85f3f29ddeda7b29d89e2a931f094c9f52479f53aef02b86e1bb6e"
)
