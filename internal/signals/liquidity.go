package signals

import (
	"bytes"
	"context"
	"math/big"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	dexv2 "github.com/meltingclock/biteblock_v1/internal/dex/v2"
)

type LiquidityKind int

const (
	LiqAddETH LiquidityKind = iota
	LiqAddTokens
)

type LiquiditySignal struct {
	Pair          common.Address
	Token0        common.Address
	Token1        common.Address
	Router        common.Address
	Hash          common.Hash
	From          common.Address
	Kind          LiquidityKind
	ChainID       *big.Int
	FirstSeenUnix int64
}

type LiquidityAnalyzer struct {
	dex          *dexv2.Registry
	router       abi.ABI
	factory      abi.ABI
	ec           *ethclient.Client
	pairs        *PairRegistry
	initCodeHash common.Hash
}

func (k LiquidityKind) String() string {
	switch k {
	case LiqAddETH:
		return "addLiquidityETH"
	case LiqAddTokens:
		return "addLiquidity"
	default:
		return "unknown"
	}
}

func NewLiquidityAnalyzer(ec *ethclient.Client, dex *dexv2.Registry, pr *PairRegistry) *LiquidityAnalyzer {
	rab, _ := abi.JSON(strings.NewReader(dexv2.RouterABI))
	fab, _ := abi.JSON(strings.NewReader(dexv2.FactoryABI))
	return &LiquidityAnalyzer{dex: dex, router: rab, factory: fab, ec: ec, pairs: pr, initCodeHash: dex.InitCodeHash()}

}

func (la *LiquidityAnalyzer) IsRouter(to *common.Address) bool {
	return to != nil && *to == la.dex.Router()
}

func (la *LiquidityAnalyzer) AnalyzePending(ctx context.Context, tx *types.Transaction) (*LiquiditySignal, error) {
	// fast router gate
	if tx.To() == nil || !la.IsRouter(tx.To()) {
		return nil, nil
	}

	// 1) quick 4-byte selector check
	data := tx.Data()
	if len(data) < 4 {
		return nil, nil
	}
	var sel [4]byte
	copy(sel[:], data[:4])

	isAddETH := sel == la.dex.SelAddLiquidityETH()
	isAdd := sel == la.dex.SelAddLiquidity()
	if !(isAddETH || isAdd) {
		return nil, nil
	}

	// 2) decode minimal params
	if isAddETH {
		// addLiquidityETH(address, token, ...)
		args, err := la.router.Methods["addLiquidityETH"].Inputs.Unpack(data[4:])
		if err != nil {
			return nil, nil
		}
		token := args[0].(common.Address)
		// Need token pair with WETH
		//weth := guessWETH(la.dex.Config().Network) // implement per-net
		weth := la.dex.WETH()
		pair, t0, t1, err := la.getPair(ctx, token, weth)
		if err != nil {
			return nil, nil
		}

		s := &LiquiditySignal{
			Pair: pair, Token0: t0, Token1: t1,
			Router: la.dex.Router(),
			Hash:   tx.Hash(), From: senderOrZero(tx),
			Kind: LiqAddETH, ChainID: tx.ChainId(), FirstSeenUnix: time.Now().Unix(),
		}
		return s, nil
	}

	// addLiquidity(address, tokenA,address tokenB,...)
	args, err := la.router.Methods["addLiquidity"].Inputs.Unpack(data[4:])
	if err != nil {
		return nil, nil
	}
	tokenA := args[0].(common.Address)
	tokenB := args[1].(common.Address)
	pair, t0, t1, err := la.getPair(ctx, tokenA, tokenB)
	if err != nil {
		return nil, nil
	}

	s := &LiquiditySignal{
		Pair: pair, Token0: t0, Token1: t1,
		Router: la.dex.Router(),
		Hash:   tx.Hash(), From: senderOrZero(tx),
		Kind: LiqAddTokens, ChainID: tx.ChainId(), FirstSeenUnix: time.Now().Unix(),
	}
	return s, nil
}

// Update calculatePairAddress to be a method
func (la *LiquidityAnalyzer) calculatePairAddress(token0, token1 common.Address) common.Address {
	// Ensure token0 < token1 (canonical ordering)
	if bytes.Compare(token0.Bytes(), token1.Bytes()) > 0 {
		token0, token1 = token1, token0
	}

	// Calculate CREATE2 address
	salt := crypto.Keccak256(append(token0.Bytes(), token1.Bytes()...))

	data := []byte{0xff}
	data = append(data, la.dex.Factory().Bytes()...)
	data = append(data, salt...)
	data = append(data, la.initCodeHash.Bytes()...)

	hash := crypto.Keccak256(data)
	return common.BytesToAddress(hash[12:])
}

func (la *LiquidityAnalyzer) getPair(ctx context.Context, a, b common.Address) (pair, token0, token1 common.Address, _ error) {
	// First try local cache from PairRegistry
	// (We don't know the pair addr if PairCreated not mined yet; but for most forks, getPair returns address(0) until mined)
	// Fall back to on-chain getPair.
	// Optional: try cache first (adjust method name to your PairRegistry)
	if strings.ToLower(a.Hex()) < strings.ToLower(b.Hex()) {
		token0, token1 = a, b
	} else {
		token0, token1 = b, a
	}

	// First try local cache from PairRegistry
	if la.pairs != nil {
		if p, ok := la.pairs.GetByTokens(a, b); ok {
			return p, token0, token1, nil
		}
	}

	input, err := la.factory.Pack("getPair", a, b)
	if err != nil {
		return common.Address{}, common.Address{}, common.Address{}, err
	}
	callMsg := ethereum.CallMsg{To: ptr(la.dex.Factory()), Data: input}
	out, err := la.ec.CallContract(ctx, callMsg, nil)
	if err != nil {
		// If call fails, calculate deterministic address
		calculated := la.calculatePairAddress(token0, token1)
		return calculated, token0, token1, nil
	}
	var addr common.Address
	if err := la.factory.UnpackIntoInterface(&addr, "getPair", out); err != nil {
		return common.Address{}, common.Address{}, common.Address{}, err
	}

	// If pair doesn't exist yet (returns 0x0), calculate it
	if addr == (common.Address{}) {
		addr = la.calculatePairAddress(token0, token1)
	}
	return addr, b, a, nil
}

func senderOrZero(tx *types.Transaction) common.Address {
	s, err := types.Sender(types.LatestSignerForChainID(tx.ChainId()), tx)
	if err != nil {
		return common.Address{}
	}
	return s
}

func ptr[T any](v T) *T {
	return &v
}

/*
// quick placeholder; replace with your configured WETH per net
func guessWETH(n dexv2.Network) common.Address {
	switch n {
	case dexv2.Ethereum:
		return common.HexToAddress("0xC02aaA39b223FE8D0A0e5C4F27eAD9083C756Cc2") // WETH on Ethereum mainnet
	case dexv2.Base:
		return common.HexToAddress("0x4200000000000000000000000000000000000006") // WETH on Base
	case dexv2.BSC:
		return common.HexToAddress("0xBB4CdB9CBd36B01bD1cBaEBF2De08d9173bc095c") // WBNB on BSC
	default:
		return common.Address{} // unknown network, return zero address
	}
}
*/
