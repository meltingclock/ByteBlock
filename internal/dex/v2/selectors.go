package v2

/*
// Step A: table‑driven V2 selector coverage + lightweight decoders for the hot path.
//
// This file introduces:
//  • A selector → SigKind registry (precomputed at init)
//  • Minimal decoders for the fields we actually need on the hot path
//  • Public helpers to classify calldata and extract swap/liquidity params fast
//
// Integration notes (no logic changes elsewhere):
//  • Call Classify(input) to get the SigKind.
//  • For swaps, call DecodeSwapParams(input) to obtain path[0], path[last],
//    amountIn/amountOutMin (as applicable), recipient and deadline.
//  • For addLiquidity, call DecodeAddLiquidity* for token addresses + desired/min amounts.
//
// Performance: single pass over calldata, no heap allocations in steady state.

import (
	"errors"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// -----------------------------
// Public API
// -----------------------------

// If these already exist elsewhere in your repo, keep ONE definition only.
// (Delete the duplicates here.)

type SigKind int

const (
	SigUnknown SigKind = iota
	// Liquidity
	SigAddLiquidityETH
	SigAddLiquidity
	SigRemoveLiquidity
	SigRemoveLiquidityETH
	SigRemoveLiquidityETHSupportingFeeOnTransferTokens
	SigRemoveLiquidityWithPermit
	SigRemoveLiquidityETHWithPermit
	SigRemoveLiquidityETHWithPermitSupportingFeeOnTransferTokens
	// Swaps — exact/for, ETH in/out, with/without SupportingFeeOnTransferTokens
	SigSwapExactETHForTokens
	SigSwapETHForExactTokens
	SigSwapExactTokensForETH
	SigSwapTokensForExactETH
	SigSwapExactTokensForTokens
	SigSwapTokensForExactTokens
	SigSwapExactETHForTokensSupportingFeeOnTransferTokens
	SigSwapExactTokensForETHSupportingFeeOnTransferTokens
	SigSwapExactTokensForTokensSupportingFeeOnTransferTokens
)

// SwapParams captures only what the scanner/executor hot-path needs.
type SwapParams struct {
	// For ETH-in variants, AmountIn is 0 here (comes from msg.value); we still decode AmountOutMin.
	AmountIn     *big.Int
	AmountOutMin *big.Int
	Path0        common.Address // first token in path
	PathN        common.Address // last token in path
	To           common.Address
	Deadline     *big.Int
}

// AddLiquidityParams captures the minimal fields for addLiquidity.
type AddLiquidityParams struct {
	TokenA         common.Address
	TokenB         common.Address
	AmountADesired *big.Int
	AmountBDesired *big.Int
	AmountAMin     *big.Int
	AmountBMin     *big.Int
	To             common.Address
	Deadline       *big.Int
}

// AddLiquidityETHParams captures the minimal fields for addLiquidityETH.
type AddLiquidityETHParams struct {
	Token          common.Address
	AmountTokenDes *big.Int
	AmountTokenMin *big.Int
	AmountETHMin   *big.Int
	To             common.Address
	Deadline       *big.Int
}

// Classify returns the SigKind for the calldata (or SigUnknown).
func Classify(input []byte) SigKind {
	if len(input) < 4 {
		return SigUnknown
	}
	var k [4]byte
	copy(k[:], input[:4])
	if kind, ok := selectorToKind[k]; ok {
		return kind
	}
	return SigUnknown
}

// DecodeSwapParams decodes minimal swap params for supported swap selectors.
// It returns an error for unknown/unsupported signatures or malformed calldata.
func DecodeSwapParams(input []byte) (SwapParams, error) {
	kind := Classify(input)
	switch kind {
	case SigSwapExactETHForTokens, SigSwapExactETHForTokensSupportingFeeOnTransferTokens:
		// swapExactETHForTokens(uint amountOutMin, address[] path, address to, uint deadline)
		amountOutMin, ok := readUint(input, 0)
		if !ok {
			return SwapParams{}, errShort
		}
		first, last, to, deadline, err := decodePathToDeadline(input, 1)
		if err != nil {
			return SwapParams{}, err
		}
		return SwapParams{AmountIn: big0(), AmountOutMin: amountOutMin, Path0: first, PathN: last, To: to, Deadline: deadline}, nil

	case SigSwapETHForExactTokens:
		// swapETHForExactTokens(uint amountOut, address[] path, address to, uint deadline)
		amountOut, ok := readUint(input, 0)
		if !ok {
			return SwapParams{}, errShort
		}
		first, last, to, deadline, err := decodePathToDeadline(input, 1)
		if err != nil {
			return SwapParams{}, err
		}
		return SwapParams{AmountIn: big0(), AmountOutMin: amountOut, Path0: first, PathN: last, To: to, Deadline: deadline}, nil

	case SigSwapExactTokensForETH, SigSwapExactTokensForETHSupportingFeeOnTransferTokens:
		// swapExactTokensForETH(uint amountIn, uint amountOutMin, address[] path, address to, uint deadline)
		amountIn, ok := readUint(input, 0)
		if !ok {
			return SwapParams{}, errShort
		}
		amountOutMin, ok := readUint(input, 1)
		if !ok {
			return SwapParams{}, errShort
		}
		first, last, to, deadline, err := decodePathToDeadline(input, 2)
		if err != nil {
			return SwapParams{}, err
		}
		return SwapParams{AmountIn: amountIn, AmountOutMin: amountOutMin, Path0: first, PathN: last, To: to, Deadline: deadline}, nil

	case SigSwapTokensForExactETH:
		// swapTokensForExactETH(uint amountOut, uint amountInMax, address[] path, address to, uint deadline)
		amountOut, ok := readUint(input, 0)
		if !ok {
			return SwapParams{}, errShort
		}
		_, ok = readUint(input, 1)
		if !ok {
			return SwapParams{}, errShort
		} // amountInMax not needed
		first, last, to, deadline, err := decodePathToDeadline(input, 2)
		if err != nil {
			return SwapParams{}, err
		}
		return SwapParams{AmountIn: nil, AmountOutMin: amountOut, Path0: first, PathN: last, To: to, Deadline: deadline}, nil

	case SigSwapExactTokensForTokens, SigSwapExactTokensForTokensSupportingFeeOnTransferTokens:
		// swapExactTokensForTokens(uint amountIn, uint amountOutMin, address[] path, address to, uint deadline)
		amountIn, ok := readUint(input, 0)
		if !ok {
			return SwapParams{}, errShort
		}
		amountOutMin, ok := readUint(input, 1)
		if !ok {
			return SwapParams{}, errShort
		}
		first, last, to, deadline, err := decodePathToDeadline(input, 2)
		if err != nil {
			return SwapParams{}, err
		}
		return SwapParams{AmountIn: amountIn, AmountOutMin: amountOutMin, Path0: first, PathN: last, To: to, Deadline: deadline}, nil

	case SigSwapTokensForExactTokens:
		// swapTokensForExactTokens(uint amountOut, uint amountInMax, address[] path, address to, uint deadline)
		amountOut, ok := readUint(input, 0)
		if !ok {
			return SwapParams{}, errShort
		}
		_, ok = readUint(input, 1)
		if !ok {
			return SwapParams{}, errShort
		}
		first, last, to, deadline, err := decodePathToDeadline(input, 2)
		if err != nil {
			return SwapParams{}, err
		}
		return SwapParams{AmountIn: nil, AmountOutMin: amountOut, Path0: first, PathN: last, To: to, Deadline: deadline}, nil
	}
	return SwapParams{}, errUnknown
}

// DecodeAddLiquidity decodes addLiquidity(tokenA, tokenB, amountADesired, amountBDesired, amountAMin, amountBMin, to, deadline)
func DecodeAddLiquidity(input []byte) (AddLiquidityParams, error) {
	if Classify(input) != SigAddLiquidity {
		return AddLiquidityParams{}, errUnknown
	}
	// 8 static words after selector
	tokenA, ok := readAddress(input, 0)
	if !ok {
		return AddLiquidityParams{}, errShort
	}
	tokenB, ok := readAddress(input, 1)
	if !ok {
		return AddLiquidityParams{}, errShort
	}
	ad, ok := readUint(input, 2)
	if !ok {
		return AddLiquidityParams{}, errShort
	}
	bd, ok := readUint(input, 3)
	if !ok {
		return AddLiquidityParams{}, errShort
	}
	amin, ok := readUint(input, 4)
	if !ok {
		return AddLiquidityParams{}, errShort
	}
	bmin, ok := readUint(input, 5)
	if !ok {
		return AddLiquidityParams{}, errShort
	}
	to, ok := readAddress(input, 6)
	if !ok {
		return AddLiquidityParams{}, errShort
	}
	dl, ok := readUint(input, 7)
	if !ok {
		return AddLiquidityParams{}, errShort
	}
	return AddLiquidityParams{TokenA: tokenA, TokenB: tokenB, AmountADesired: ad, AmountBDesired: bd, AmountAMin: amin, AmountBMin: bmin, To: to, Deadline: dl}, nil
}

// DecodeAddLiquidityETH decodes addLiquidityETH(token, amountTokenDesired, amountTokenMin, amountETHMin, to, deadline)
func DecodeAddLiquidityETH(input []byte) (AddLiquidityETHParams, error) {
	if Classify(input) != SigAddLiquidityETH {
		return AddLiquidityETHParams{}, errUnknown
	}
	token, ok := readAddress(input, 0)
	if !ok {
		return AddLiquidityETHParams{}, errShort
	}
	ad, ok := readUint(input, 1)
	if !ok {
		return AddLiquidityETHParams{}, errShort
	}
	amin, ok := readUint(input, 2)
	if !ok {
		return AddLiquidityETHParams{}, errShort
	}
	emin, ok := readUint(input, 3)
	if !ok {
		return AddLiquidityETHParams{}, errShort
	}
	to, ok := readAddress(input, 4)
	if !ok {
		return AddLiquidityETHParams{}, errShort
	}
	dl, ok := readUint(input, 5)
	if !ok {
		return AddLiquidityETHParams{}, errShort
	}
	return AddLiquidityETHParams{Token: token, AmountTokenDes: ad, AmountTokenMin: amin, AmountETHMin: emin, To: to, Deadline: dl}, nil
}

// -----------------------------
// Selector registry
// -----------------------------

var selectorToKind map[[4]byte]SigKind

func init() {
	selectorToKind = make(map[[4]byte]SigKind, len(sigList))
	for _, s := range sigList {
		selectorToKind[keccak4(s.sig)] = s.kind
	}
}

type sigEntry struct {
	sig  string
	kind SigKind
}

var sigList = []sigEntry{
	// Liquidity
	{"addLiquidity(address,address,uint256,uint256,uint256,uint256,address,uint256)", SigAddLiquidity},
	{"addLiquidityETH(address,uint256,uint256,uint256,address,uint256)", SigAddLiquidityETH},
	{"removeLiquidity(address,address,uint256,uint256,uint256,address,uint256)", SigRemoveLiquidity},
	{"removeLiquidityETH(address,uint256,uint256,uint256,address,uint256)", SigRemoveLiquidityETH},
	{"removeLiquidityETHSupportingFeeOnTransferTokens(address,uint256,uint256,address,uint256)", SigRemoveLiquidityETHSupportingFeeOnTransferTokens},
	{"removeLiquidityWithPermit(address,address,uint256,uint256,uint256,address,uint256,bool,uint8,bytes32,bytes32)", SigRemoveLiquidityWithPermit},
	{"removeLiquidityETHWithPermit(address,uint256,uint256,uint256,address,uint256,bool,uint8,bytes32,bytes32)", SigRemoveLiquidityETHWithPermit},
	{"removeLiquidityETHWithPermitSupportingFeeOnTransferTokens(address,uint256,uint256,address,uint256,bool,uint8,bytes32,bytes32)", SigRemoveLiquidityETHWithPermitSupportingFeeOnTransferTokens},
	// Swaps
	{"swapExactETHForTokens(uint256,address[],address,uint256)", SigSwapExactETHForTokens},
	{"swapETHForExactTokens(uint256,address[],address,uint256)", SigSwapETHForExactTokens},
	{"swapExactTokensForETH(uint256,uint256,address[],address,uint256)", SigSwapExactTokensForETH},
	{"swapTokensForExactETH(uint256,uint256,address[],address,uint256)", SigSwapTokensForExactETH},
	{"swapExactTokensForTokens(uint256,uint256,address[],address,uint256)", SigSwapExactTokensForTokens},
	{"swapTokensForExactTokens(uint256,uint256,address[],address,uint256)", SigSwapTokensForExactTokens},
	{"swapExactETHForTokensSupportingFeeOnTransferTokens(uint256,address[],address,uint256)", SigSwapExactETHForTokensSupportingFeeOnTransferTokens},
	{"swapExactTokensForETHSupportingFeeOnTransferTokens(uint256,uint256,address[],address,uint256)", SigSwapExactTokensForETHSupportingFeeOnTransferTokens},
	{"swapExactTokensForTokensSupportingFeeOnTransferTokens(uint256,uint256,address[],address,uint256)", SigSwapExactTokensForTokensSupportingFeeOnTransferTokens},
}

// -----------------------------
// Internal helpers (bounds‑checked, no panics)
// -----------------------------

var (
	errShort   = errors.New("calldata too short")
	errUnknown = errors.New("unknown or unsupported selector")
)

func keccak4(s string) [4]byte {
	h := crypto.Keccak256([]byte(s))
	var out [4]byte
	copy(out[:], h[:4])
	return out
}

// readWord returns the 32‑byte slot index i (after the 4‑byte selector).
func readWord(input []byte, i int) ([]byte, bool) {
	off := 4 + 32*i
	if off+32 > len(input) {
		return nil, false
	}
	return input[off : off+32], true
}

func readUint(input []byte, i int) (*big.Int, bool) {
	w, ok := readWord(input, i)
	if !ok {
		return nil, false
	}
	return new(big.Int).SetBytes(w), true
}

func readAddress(input []byte, i int) (common.Address, bool) {
	w, ok := readWord(input, i)
	if !ok {
		return common.Address{}, false
	}
	// address = right‑most 20 bytes of the 32‑byte word
	return common.BytesToAddress(w[12:32]), true
}

func readOffset(input []byte, i int) (int, bool) {
	w, ok := readWord(input, i)
	if !ok {
		return 0, false
	}
	off := new(big.Int).SetBytes(w).Int64()
	if off < 0 {
		return 0, false
	}
	abs := int(off) + 4 // offset is from start of params (after selector)
	if abs > len(input) {
		return 0, false
	}
	return abs, true
}

// decodePathToDeadline reads a dynamic address[] at param index `pathIdx`, then static `to`, `deadline`.
// Returns path first & last addresses (only), recipient and deadline.
func decodePathToDeadline(input []byte, pathIdx int) (first, last, to common.Address, deadline *big.Int, err error) {
	pathOff, ok := readOffset(input, pathIdx)
	if !ok {
		return first, last, to, nil, errShort
	}
	// length word at pathOff
	if pathOff+32 > len(input) {
		return first, last, to, nil, errShort
	}
	pathLen := new(big.Int).SetBytes(input[pathOff : pathOff+32]).Int64()
	if pathLen <= 0 {
		return first, last, to, nil, errors.New("path length <= 0")
	}
	// First element @ pathOff+32, each encoded as 32‑byte word with address right‑aligned
	firstWord := pathOff + 32
	lastWord := firstWord + int(pathLen-1)*32
	if lastWord+32 > len(input) {
		return first, last, to, nil, errShort
	}
	first = common.BytesToAddress(input[firstWord+12 : firstWord+32])
	last = common.BytesToAddress(input[lastWord+12 : lastWord+32])

	toAddr, ok := readAddress(input, pathIdx+1)
	if !ok {
		return first, last, to, nil, errShort
	}
	dl, ok := readUint(input, pathIdx+2)
	if !ok {
		return first, last, to, nil, errShort
	}
	return first, last, toAddr, dl, nil
}

func big0() *big.Int { return new(big.Int) }
*/
