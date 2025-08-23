package v2

import (
	"context"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

type Network string

const (
	Ethereum Network = "ethereum"
	Base     Network = "base"
	BSC      Network = "bsc"
)

type Config struct {
	Network Network
	Factory common.Address
	Router  common.Address
	WETH    common.Address
	// Optional: more routers/factories per net in the future
}

type Registry struct {
	mu     sync.RWMutex
	cfg    Config
	topics struct {
		PairCreated common.Hash
	}
	selectors struct {
		AddLiquidityETH [4]byte // 0xf305d719
		AddLiquidity    [4]byte // 0xe8e33700
	}
}

func (r *Registry) WETH() common.Address {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.cfg.WETH
}

func NewRegistry(cfg Config) *Registry {
	r := &Registry{cfg: cfg}
	r.topics.PairCreated = keccak("PairCreated(address,address,address,uint256)")

	r.selectors.AddLiquidityETH = fourBytes("f305d719")
	r.selectors.AddLiquidity = fourBytes("e8e33700")
	return r
}

func (r *Registry) Config() Config {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.cfg
}

func (r *Registry) Router() common.Address {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.cfg.Router
}

func (r *Registry) Factory() common.Address {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.cfg.Factory
}

func (r *Registry) TopicPairCreated() common.Hash { return r.topics.PairCreated }
func (r *Registry) SelAddLiquidityETH() [4]byte   { return r.selectors.AddLiquidityETH }
func (r *Registry) SelAddLiquidity() [4]byte      { return r.selectors.AddLiquidity }

// ABIs (minimal fragments)
const (
	FactoryABI = `[
		{"anonymous":false,"inputs":[
			{"indexed":true,"name":"token0","type":"address"},
			{"indexed":true,"name":"token1","type":"address"},
			{"indexed":false,"name":"pair","type":"address"},
			{"indexed":false,"name":"","type":"uint256"}],
		 "name":"PairCreated","type":"event"},
		{"inputs":[{"name":"tokenA","type":"address"},{"name":"tokenB","type":"address"}],
		 "name":"getPair","outputs":[{"type":"address"}],"stateMutability":"view","type":"function"}
	]`

	RouterABI = `[
		{"inputs":[
			{"internalType":"address","name":"token","type":"address"},
			{"internalType":"uint256","name":"amountTokenDesired","type":"uint256"},
			{"internalType":"uint256","name":"amountTokenMin","type":"uint256"},
			{"internalType":"uint256","name":"amountETHMin","type":"uint256"},
			{"internalType":"address","name":"to","type":"address"},
			{"internalType":"uint256","name":"deadline","type":"uint256"}],
		 "name":"addLiquidityETH","outputs":[
			{"internalType":"uint256","name":"amountToken","type":"uint256"},
			{"internalType":"uint256","name":"amountETH","type":"uint256"},
			{"internalType":"uint256","name":"liquidity","type":"uint256"}],
		 "stateMutability":"payable","type":"function"},

		{"inputs":[
			{"internalType":"address","name":"tokenA","type":"address"},
			{"internalType":"address","name":"tokenB","type":"address"},
			{"internalType":"uint256","name":"amountADesired","type":"uint256"},
			{"internalType":"uint256","name":"amountBDesired","type":"uint256"},
			{"internalType":"uint256","name":"amountAMin","type":"uint256"},
			{"internalType":"uint256","name":"amountBMin","type":"uint256"},
			{"internalType":"address","name":"to","type":"address"},
			{"internalType":"uint256","name":"deadline","type":"uint256"}],
		 "name":"addLiquidity","outputs":[
			{"internalType":"uint256","name":"amountA","type":"uint256"},
			{"internalType":"uint256","name":"amountB","type":"uint256"},
			{"internalType":"uint256","name":"liquidity","type":"uint256"}],
		 "stateMutability":"nonpayable","type":"function"}
	]`
)

func keccak(sig string) common.Hash {
	return crypto.Keccak256Hash([]byte(sig))
}
func fourBytes(hexStr string) [4]byte {
	hexStr = strings.TrimPrefix(hexStr, "0x")
	b, _ := hex.DecodeString(hexStr)
	var a [4]byte
	copy(a[:], b[:4])
	return a
}

func (cfg Config) Validate() error {
	if (cfg.Factory == (common.Address{})) || (cfg.Router == (common.Address{})) || (cfg.WETH == (common.Address{})) {
		return fmt.Errorf("v2.Config: factory/router/WETH must be set")
	}
	return nil
}

// (Optional future) Allow dynamic updates.
func (r *Registry) Update(ctx context.Context, cfg Config) {
	r.mu.Lock()
	r.cfg = cfg
	r.mu.Unlock()
}
