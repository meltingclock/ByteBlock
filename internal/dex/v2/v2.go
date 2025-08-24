// Replace your internal/dex/v2/v2.go with this clean version

package v2

import (
	"context"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"

	"github.com/ethereum/go-ethereum/accounts/abi"
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
}

type Registry struct {
	mu     sync.RWMutex
	cfg    Config
	topics struct {
		PairCreated common.Hash
	}
	selectors struct {
		AddLiquidityETH [4]byte
		AddLiquidity    [4]byte

		SwapExactETHForTokens                                 [4]byte
		SwapETHForExactTokens                                 [4]byte
		SwapExactTokensForETH                                 [4]byte
		SwapTokensForExactETH                                 [4]byte
		SwapExactTokensForTokens                              [4]byte
		SwapTokensForExactTokens                              [4]byte
		SwapExactETHForTokensSupportingFeeOnTransferTokens    [4]byte
		SwapExactTokensForETHSupportingFeeOnTransferTokens    [4]byte
		SwapExactTokensForTokensSupportingFeeOnTransferTokens [4]byte
	}

	// Reverse lookup 4-byte -> meta (using the old system for compatibility)
	sels map[[4]byte]SigMeta
}

type SigMeta struct {
	Kind SigKind // Uses SigKind from selectors.go
	Name string
	Sel  [4]byte
}

func (r *Registry) WETH() common.Address {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.cfg.WETH
}

// ClassifyTransaction - fast path for transaction classification
func (r *Registry) ClassifyTransaction(data []byte) SigKind {
	return Classify(data) // Use the optimized function from selectors.go
}

// IsLiquidityFunction checks if transaction is adding/removing liquidity
func (r *Registry) IsLiquidityFunction(data []byte) bool {
	kind := Classify(data)
	switch kind {
	case SigAddLiquidity, SigAddLiquidityETH,
		SigRemoveLiquidity, SigRemoveLiquidityETH,
		SigRemoveLiquidityETHSupportingFeeOnTransferTokens,
		SigRemoveLiquidityWithPermit, SigRemoveLiquidityETHWithPermit,
		SigRemoveLiquidityETHWithPermitSupportingFeeOnTransferTokens:
		return true
	default:
		return false
	}
}

// IsSwapFunction checks if transaction is a token swap
func (r *Registry) IsSwapFunction(data []byte) bool {
	kind := Classify(data)
	switch kind {
	case SigSwapExactETHForTokens, SigSwapETHForExactTokens,
		SigSwapExactTokensForETH, SigSwapTokensForExactETH,
		SigSwapExactTokensForTokens, SigSwapTokensForExactTokens,
		SigSwapExactETHForTokensSupportingFeeOnTransferTokens,
		SigSwapExactTokensForETHSupportingFeeOnTransferTokens,
		SigSwapExactTokensForTokensSupportingFeeOnTransferTokens:
		return true
	default:
		return false
	}
}

/*
// ExtractSwapInfo - get swap details for analysis
func (r *Registry) ExtractSwapInfo(data []byte) (*SwapParams, error) {
	return DecodeSwapParams(data) // Use function from selectors.go
}
*/

// ExtractLiquidityInfo - get liquidity details for analysis
func (r *Registry) ExtractLiquidityInfo(data []byte) (interface{}, error) {
	kind := Classify(data)
	switch kind {
	case SigAddLiquidityETH:
		return DecodeAddLiquidityETH(data) // Use function from selectors.go
	case SigAddLiquidity:
		return DecodeAddLiquidity(data) // Use function from selectors.go
	default:
		return nil, fmt.Errorf("not a liquidity function")
	}
}

func NewRegistry(cfg Config) *Registry {
	r := &Registry{cfg: cfg}
	r.sels = make(map[[4]byte]SigMeta)
	r.topics.PairCreated = keccak("PairCreated(address,address,address,uint256)")

	// Hardcoded selectors for backward compatibility
	r.selectors.AddLiquidityETH = fourBytes("f305d719")
	r.selectors.AddLiquidity = fourBytes("e8e33700")

	// Parse router ABI and populate the legacy sels map
	rab, _ := abi.JSON(strings.NewReader(RouterABI))
	add := func(name string, kind SigKind, store *[4]byte) {
		if m, ok := rab.Methods[name]; ok {
			id := idTo4(any(m.ID))
			if store != nil {
				*store = id
			}
			r.sels[id] = SigMeta{Kind: kind, Name: name, Sel: id}
		}
	}

	// Populate legacy map (for backward compatibility)
	add("addLiquidityETH", SigAddLiquidityETH, &r.selectors.AddLiquidityETH)
	add("addLiquidity", SigAddLiquidity, &r.selectors.AddLiquidity)
	add("swapExactETHForTokens", SigSwapExactETHForTokens, &r.selectors.SwapExactETHForTokens)
	add("swapETHForExactTokens", SigSwapETHForExactTokens, &r.selectors.SwapETHForExactTokens)
	add("swapExactTokensForETH", SigSwapExactTokensForETH, &r.selectors.SwapExactTokensForETH)
	add("swapTokensForExactETH", SigSwapTokensForExactETH, &r.selectors.SwapTokensForExactETH)
	add("swapExactTokensForTokens", SigSwapExactTokensForTokens, &r.selectors.SwapExactTokensForTokens)
	add("swapTokensForExactTokens", SigSwapTokensForExactTokens, &r.selectors.SwapTokensForExactTokens)

	// Fee-on-transfer variants
	add("swapExactETHForTokensSupportingFeeOnTransferTokens", SigSwapExactETHForTokensSupportingFeeOnTransferTokens, &r.selectors.SwapExactETHForTokensSupportingFeeOnTransferTokens)
	add("swapExactTokensForETHSupportingFeeOnTransferTokens", SigSwapExactTokensForETHSupportingFeeOnTransferTokens, &r.selectors.SwapExactTokensForETHSupportingFeeOnTransferTokens)
	add("swapExactTokensForTokensSupportingFeeOnTransferTokens", SigSwapExactTokensForTokensSupportingFeeOnTransferTokens, &r.selectors.SwapExactTokensForTokensSupportingFeeOnTransferTokens)

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

func (r *Registry) LookupSelector(sel [4]byte) (SigMeta, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	m, ok := r.sels[sel]
	return m, ok
}

func (r *Registry) LookupSelectorFromData(data []byte) (SigMeta, bool) {
	if len(data) < 4 {
		return SigMeta{}, false
	}

	kind := Classify(data) // Use the fast classify function
	if kind == SigUnknown {
		return SigMeta{}, false
	}

	var selector [4]byte
	copy(selector[:], data[:4])

	// Create SigMeta from the classified kind
	return SigMeta{
		Kind: kind,
		Name: KindToName(kind), // Use the function from selectors.go
		Sel:  selector,
	}, true
}

func (r *Registry) TopicPairCreated() common.Hash { return r.topics.PairCreated }
func (r *Registry) SelAddLiquidityETH() [4]byte   { return r.selectors.AddLiquidityETH }
func (r *Registry) SelAddLiquidity() [4]byte      { return r.selectors.AddLiquidity }

// ABIs (minimal fragments) - UNCHANGED
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
		 "stateMutability":"nonpayable","type":"function"},

		{"inputs":[
			{"internalType":"uint256","name":"amountOutMin","type":"uint256"},
			{"internalType":"address[]","name":"path","type":"address[]"},
			{"internalType":"address","name":"to","type":"address"},
			{"internalType":"uint256","name":"deadline","type":"uint256"}],
		 "name":"swapExactETHForTokens","outputs":[{"internalType":"uint256[]","name":"amounts","type":"uint256[]"}],
		 "stateMutability":"payable","type":"function"},
		{"inputs":[
			{"internalType":"uint256","name":"amountOut","type":"uint256"},
			{"internalType":"address[]","name":"path","type":"address[]"},
			{"internalType":"address","name":"to","type":"address"},
			{"internalType":"uint256","name":"deadline","type":"uint256"}],
		 "name":"swapETHForExactTokens","outputs":[{"internalType":"uint256[]","name":"amounts","type":"uint256[]"}],
		 "stateMutability":"payable","type":"function"},
		{"inputs":[
			{"internalType":"uint256","name":"amountIn","type":"uint256"},
			{"internalType":"uint256","name":"amountOutMin","type":"uint256"},
			{"internalType":"address[]","name":"path","type":"address[]"},
			{"internalType":"address","name":"to","type":"address"},
			{"internalType":"uint256","name":"deadline","type":"uint256"}],
		 "name":"swapExactTokensForETH","outputs":[{"internalType":"uint256[]","name":"amounts","type":"uint256[]"}],
		 "stateMutability":"nonpayable","type":"function"},
		{"inputs":[
			{"internalType":"uint256","name":"amountOut","type":"uint256"},
			{"internalType":"uint256","name":"amountInMax","type":"uint256"},
			{"internalType":"address[]","name":"path","type":"address[]"},
			{"internalType":"address","name":"to","type":"address"},
			{"internalType":"uint256","name":"deadline","type":"uint256"}],
		 "name":"swapTokensForExactETH","outputs":[{"internalType":"uint256[]","name":"amounts","type":"uint256[]"}],
		 "stateMutability":"nonpayable","type":"function"},
		{"inputs":[
			{"internalType":"uint256","name":"amountIn","type":"uint256"},
			{"internalType":"uint256","name":"amountOutMin","type":"uint256"},
			{"internalType":"address[]","name":"path","type":"address[]"},
			{"internalType":"address","name":"to","type":"address"},
			{"internalType":"uint256","name":"deadline","type":"uint256"}],
		 "name":"swapExactTokensForTokens","outputs":[{"internalType":"uint256[]","name":"amounts","type":"uint256[]"}],
		 "stateMutability":"nonpayable","type":"function"},
		{"inputs":[
			{"internalType":"uint256","name":"amountOut","type":"uint256"},
			{"internalType":"uint256","name":"amountInMax","type":"uint256"},
			{"internalType":"address[]","name":"path","type":"address[]"},
			{"internalType":"address","name":"to","type":"address"},
			{"internalType":"uint256","name":"deadline","type":"uint256"}],
		 "name":"swapTokensForExactTokens","outputs":[{"internalType":"uint256[]","name":"amounts","type":"uint256[]"}],
		 "stateMutability":"nonpayable","type":"function"},

		{"inputs":[
			{"internalType":"uint256","name":"amountOutMin","type":"uint256"},
			{"internalType":"address[]","name":"path","type":"address[]"},
			{"internalType":"address","name":"to","type":"address"},
			{"internalType":"uint256","name":"deadline","type":"uint256"}],
		 "name":"swapExactETHForTokensSupportingFeeOnTransferTokens","outputs":[],
		 "stateMutability":"payable","type":"function"},
		{"inputs":[
			{"internalType":"uint256","name":"amountIn","type":"uint256"},
			{"internalType":"uint256","name":"amountOutMin","type":"uint256"},
			{"internalType":"address[]","name":"path","type":"address[]"},
			{"internalType":"address","name":"to","type":"address"},
			{"internalType":"uint256","name":"deadline","type":"uint256"}],
		 "name":"swapExactTokensForETHSupportingFeeOnTransferTokens","outputs":[],
		 "stateMutability":"nonpayable","type":"function"},
		{"inputs":[
			{"internalType":"uint256","name":"amountIn","type":"uint256"},
			{"internalType":"uint256","name":"amountOutMin","type":"uint256"},
			{"internalType":"address[]","name":"path","type":"address[]"},
			{"internalType":"address","name":"to","type":"address"},
			{"internalType":"uint256","name":"deadline","type":"uint256"}],
		 "name":"swapExactTokensForTokensSupportingFeeOnTransferTokens","outputs":[],
		 "stateMutability":"nonpayable","type":"function"}
	]`
)

// Helper functions - UNCHANGED
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

func Keccak(sig string) common.Hash   { return keccak(sig) }
func FourBytes(hexStr string) [4]byte { return fourBytes(hexStr) }

func idTo4(id any) [4]byte {
	var out [4]byte
	switch v := id.(type) {
	case [4]byte:
		out = v
	case []byte:
		copy(out[:], v[:4])
	default:
		// Leave zeros; shouldn't happen
	}
	return out
}

func (cfg Config) Validate() error {
	if (cfg.Factory == (common.Address{})) || (cfg.Router == (common.Address{})) || (cfg.WETH == (common.Address{})) {
		return fmt.Errorf("v2.Config: factory/router/WETH must be set")
	}
	return nil
}

func (r *Registry) Update(ctx context.Context, cfg Config) {
	r.mu.Lock()
	r.cfg = cfg
	r.mu.Unlock()
}
