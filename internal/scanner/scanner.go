package scanner

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
	dexv2 "github.com/meltingclock/biteblock_v1/internal/dex/v2"
	"github.com/meltingclock/biteblock_v1/internal/signals"
)

// --------- Public API -------------

type Config struct {
	RequireWethPair   bool     // reject if neither token == WETH
	MinEthLiquidity   *big.Int // for addLiquidityETH: tx.Value() must be >= this (wei)
	MinTokenLiquidity *big.Int // for addLiquidity(token,token) when one side is WETH (amountDesired)
	// Lists (simple, in-memory for now - Later expose via Telegram)
	AllowCreators map[common.Address]bool
	DenyCreators  map[common.Address]bool

	// Per-scan time budget
	Deadline time.Duration // e.g., 250 * time.Millisecond
}

type Report struct {
	Pass    bool
	Reasons []string

	// quick facts (useful for logs)
	ETHInWei *big.Int // if addLiquidityETH detected (from tx.Value)
	AmountA  *big.Int // parsed from calldata when possible
	AmountB  *big.Int
	Function string // "addLiquidityETH" or "addLiquidity" or "unknown"
}

type Scanner struct {
	ec      *ethclient.Client
	dex     *dexv2.Registry
	router  abi.ABI
	factory abi.ABI
	cfg     Config
}

func New(ec *ethclient.Client, dex *dexv2.Registry, cfg Config) *Scanner {
	rab, _ := abi.JSON(strings.NewReader(dexv2.RouterABI))
	fab, _ := abi.JSON(strings.NewReader(dexv2.FactoryABI))
	return &Scanner{
		ec: ec, dex: dex, router: rab, factory: fab, cfg: cfg,
	}
}

// Run executes fast, cheap checks under a bounded context deadline.
// It returns a Report with pass=false and Reasons filled on failure.
func (s *Scanner) Run(parent context.Context, sig *signals.LiquiditySignal) (*Report, error) {
	if sig == nil {
		return nil, errors.New("nil signal")
	}
	ctx := parent
	if s.cfg.Deadline > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(parent, s.cfg.Deadline)
		defer cancel()
	}

	r := &Report{Pass: true}

	// 0) Allow/Deny creator lists
	if s.cfg.DenyCreators[sig.From] {
		r.Pass = false
		r.Reasons = append(r.Reasons, "creator is on deny list")
		return r, nil
	}
	if len(s.cfg.AllowCreators) > 0 && !s.cfg.AllowCreators[sig.From] {
		// If an allowlist is defined, require membership
		r.Pass = false
		r.Reasons = append(r.Reasons, "creator is not on allow list")
		return r, nil
	}

	// 1) WETH pair requirement
	weth := s.dex.WETH()
	isWethPair := (sig.Token0 == weth) || (sig.Token1 == weth)
	if s.cfg.RequireWethPair && !isWethPair {
		r.Pass = false
		r.Reasons = append(r.Reasons, "non-weth-pair")
		return r, nil
	}

	// 2) Fetch the pending tx to read value + calldata amounts (fast path)
	tx, _, err := s.ec.TransactionByHash(ctx, sig.Hash)
	if err != nil {
		// network transient-fail soft. Let it pass but mark unknown
		r.Reasons = append(r.Reasons, "tx-fetch-failed")
	}

	// 3) Function + min-liquidity gates
	if tx != nil {
		data := tx.Data()
		if len(data) >= 4 {
			var sel [4]byte
			copy(sel[:], data[:4])

			switch {
			case sel == s.dex.SelAddLiquidityETH():
				r.Function = "addLiquidityETH"
				// ETH value must pass minimum
				r.ETHInWei = new(big.Int).Set(tx.Value())
				if s.cfg.MinEthLiquidity != nil && r.ETHInWei.Cmp(s.cfg.MinEthLiquidity) < 0 {
					r.Pass = false
					r.Reasons = append(r.Reasons, "min-eth-liquidity")
					return r, nil
				}
			case sel == s.dex.SelAddLiquidity():
				r.Function = "addLiquidity"
				// Try to decode amountADesired/amountBDesired (positions 2 & 3)
				args, decErr := s.router.Methods["addLiquidity"].Inputs.Unpack(data[4:])
				if decErr == nil && len(args) >= 4 {
					// tokenA, tokenB, amountADesired, amountBDesired, ...
					a := args[2].(*big.Int)
					b := args[3].(*big.Int)
					r.AmountA, r.AmountB = a, b
					// Only enforce min when one side is WETH (otherwise skip here)
					if isWethPair && s.cfg.MinTokenLiquidity != nil {
						// pick the side that corresponds to WETH pair
						// tokenA == Token0? depends on order; require either >= min
						if a.Cmp(s.cfg.MinTokenLiquidity) < 0 && b.Cmp(s.cfg.MinTokenLiquidity) < 0 {
							r.Pass = false
							r.Reasons = append(r.Reasons, "min-token-liquidity")
							return r, nil
						}
					}
				}
			default:
				// unknown function-let pass to next stages (router-guard already applied upstream)
				r.Function = "unknown"
			}
		}
	}

	// 4) Owner/renounce probe (best-effort; non-fatal if unknown)
	// Try owner(): 0x8da5cb5b
	owner, hasOwner, _ := tryOwner(ctx, s.ec, otherToken(weth, sig.Token0, sig.Token1))
	if hasOwner && owner == (common.Address{}) {
		// renounced; treat as waeak positive
	} else if hasOwner {
		// owner present; do not fail yet-this becomes a policy toggle later
	}

	// 5) Bytecode flags (presence-only; non-fatal)
	// Look for function selectors frequently used in honeypots / restrictive tokens.
	// These are best-effort signals and DO NOT auto-fail in phase 1.
	_ = s // (reserved; add when this check turn on)

	return r, nil
}

// ---------- helpers -------------

// Tries to call owner(); returns (addr, hasOwner, err)
func tryOwner(ctx context.Context, ec *ethclient.Client, token common.Address) (common.Address, bool, error) {
	if (token == common.Address{}) {
		return common.Address{}, false, nil
	}
	// owner() -> 0x8da5cb5b
	data, _ := hex.DecodeString("8da5cb5b")
	out, err := ec.CallContract(ctx, ethereum.CallMsg{
		To: &token, Data: data,
	}, nil)
	if err != nil || len(out) == 0 {
		return common.Address{}, false, err
	}
	if len(out) >= 32 {
		// Last 20 bytes of 32
		return common.BytesToAddress(out[12:32]), true, nil
	}
	return common.Address{}, true, fmt.Errorf("short owner() output")
}

func otherToken(weth, a, b common.Address) common.Address {
	if a == weth {
		return b
	}
	if b == weth {
		return a
	}
	return common.Address{}
}
