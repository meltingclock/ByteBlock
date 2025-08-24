package v2

import (
	"errors"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// SigKind classifies well-known v2 router functions for fast switching
// This is the SINGLE source of truth for all selector types
type SigKind int

const (
	SigUnknown SigKind = iota

	// Liquidity management
	SigAddLiquidityETH
	SigAddLiquidity
	SigRemoveLiquidity
	SigRemoveLiquidityETH
	SigRemoveLiquidityETHSupportingFeeOnTransferTokens
	SigRemoveLiquidityWithPermit
	SigRemoveLiquidityETHWithPermit
	SigRemoveLiquidityETHWithPermitSupportingFeeOnTransferTokens

	// Standard swaps
	SigSwapExactETHForTokens
	SigSwapETHForExactTokens
	SigSwapExactTokensForETH
	SigSwapTokensForExactETH
	SigSwapExactTokensForTokens
	SigSwapTokensForExactTokens

	// Fee-on-transfer variants (critical for many tokens)
	SigSwapExactETHForTokensSupportingFeeOnTransferTokens
	SigSwapExactTokensForETHSupportingFeeOnTransferTokens
	SigSwapExactTokensForTokensSupportingFeeOnTransferTokens
)

// SwapParams contains only essential fields needed for trading decisions
type SwapParams struct {
	AmountIn     *big.Int       // Input amount (0 for ETH-in variants - comes from msg.value)
	AmountOutMin *big.Int       // Minimum output (slippage protection)
	Path0        common.Address // First token in swap path
	PathN        common.Address // Last token in swap path
	To           common.Address // Recipient address
	Deadline     *big.Int       // Transaction deadline
}

// AddLiquidityParams for token-token liquidity additions
type AddLiquidityParams struct {
	TokenA         common.Address // First token
	TokenB         common.Address // Second token
	AmountADesired *big.Int       // Desired amount of tokenA
	AmountBDesired *big.Int       // Desired amount of tokenB
	AmountAMin     *big.Int       // Minimum tokenA (slippage protection)
	AmountBMin     *big.Int       // Minimum tokenB (slippage protection)
	To             common.Address // LP token recipient
	Deadline       *big.Int       // Transaction deadline
}

// AddLiquidityETHParams for token-ETH liquidity additions
type AddLiquidityETHParams struct {
	Token          common.Address // Token address
	AmountTokenDes *big.Int       // Desired token amount
	AmountTokenMin *big.Int       // Minimum token amount
	AmountETHMin   *big.Int       // Minimum ETH amount
	To             common.Address // LP token recipient
	Deadline       *big.Int       // Transaction deadline
}

// Global variables - shared across the package
var (
	selectorToKind map[[4]byte]SigKind
	errShort       = errors.New("calldata too short")
	errUnknown     = errors.New("unknown or unsupported selector")
)

func init() {
	initSelectorRegistry()
}

// Classify returns the SigKind for transaction calldata (hot path function)
func Classify(input []byte) SigKind {
	if len(input) < 4 {
		return SigUnknown
	}

	var selector [4]byte
	copy(selector[:], input[:4])

	if kind, exists := selectorToKind[selector]; exists {
		return kind
	}
	return SigUnknown
}

// DecodeSwapParams extracts swap parameters with zero allocations in steady state
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

		return SwapParams{
			AmountIn:     big.NewInt(0), // ETH amount comes from msg.value
			AmountOutMin: amountOutMin,
			Path0:        first,
			PathN:        last,
			To:           to,
			Deadline:     deadline,
		}, nil

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

		return SwapParams{
			AmountIn:     big.NewInt(0),
			AmountOutMin: amountOut, // For "exact out" swaps, this is the exact amount
			Path0:        first,
			PathN:        last,
			To:           to,
			Deadline:     deadline,
		}, nil

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

		return SwapParams{
			AmountIn:     amountIn,
			AmountOutMin: amountOutMin,
			Path0:        first,
			PathN:        last,
			To:           to,
			Deadline:     deadline,
		}, nil

	case SigSwapTokensForExactETH:
		// swapTokensForExactETH(uint amountOut, uint amountInMax, address[] path, address to, uint deadline)
		amountOut, ok := readUint(input, 0)
		if !ok {
			return SwapParams{}, errShort
		}
		// Skip amountInMax (we don't need it for analysis)
		_, ok = readUint(input, 1)
		if !ok {
			return SwapParams{}, errShort
		}

		first, last, to, deadline, err := decodePathToDeadline(input, 2)
		if err != nil {
			return SwapParams{}, err
		}

		return SwapParams{
			AmountIn:     nil, // Unknown for "exact out" swaps
			AmountOutMin: amountOut,
			Path0:        first,
			PathN:        last,
			To:           to,
			Deadline:     deadline,
		}, nil

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

		return SwapParams{
			AmountIn:     amountIn,
			AmountOutMin: amountOutMin,
			Path0:        first,
			PathN:        last,
			To:           to,
			Deadline:     deadline,
		}, nil

	case SigSwapTokensForExactTokens:
		// swapTokensForExactTokens(uint amountOut, uint amountInMax, address[] path, address to, uint deadline)
		amountOut, ok := readUint(input, 0)
		if !ok {
			return SwapParams{}, errShort
		}
		// Skip amountInMax
		_, ok = readUint(input, 1)
		if !ok {
			return SwapParams{}, errShort
		}

		first, last, to, deadline, err := decodePathToDeadline(input, 2)
		if err != nil {
			return SwapParams{}, err
		}

		return SwapParams{
			AmountIn:     nil,
			AmountOutMin: amountOut,
			Path0:        first,
			PathN:        last,
			To:           to,
			Deadline:     deadline,
		}, nil
	}

	return SwapParams{}, errUnknown
}

// DecodeAddLiquidity extracts addLiquidity parameters
func DecodeAddLiquidity(input []byte) (AddLiquidityParams, error) {
	if Classify(input) != SigAddLiquidity {
		return AddLiquidityParams{}, errUnknown
	}

	// addLiquidity has 8 parameters: tokenA, tokenB, amountADesired, amountBDesired, amountAMin, amountBMin, to, deadline
	tokenA, ok := readAddress(input, 0)
	if !ok {
		return AddLiquidityParams{}, errShort
	}
	tokenB, ok := readAddress(input, 1)
	if !ok {
		return AddLiquidityParams{}, errShort
	}
	amountADesired, ok := readUint(input, 2)
	if !ok {
		return AddLiquidityParams{}, errShort
	}
	amountBDesired, ok := readUint(input, 3)
	if !ok {
		return AddLiquidityParams{}, errShort
	}
	amountAMin, ok := readUint(input, 4)
	if !ok {
		return AddLiquidityParams{}, errShort
	}
	amountBMin, ok := readUint(input, 5)
	if !ok {
		return AddLiquidityParams{}, errShort
	}
	to, ok := readAddress(input, 6)
	if !ok {
		return AddLiquidityParams{}, errShort
	}
	deadline, ok := readUint(input, 7)
	if !ok {
		return AddLiquidityParams{}, errShort
	}

	return AddLiquidityParams{
		TokenA:         tokenA,
		TokenB:         tokenB,
		AmountADesired: amountADesired,
		AmountBDesired: amountBDesired,
		AmountAMin:     amountAMin,
		AmountBMin:     amountBMin,
		To:             to,
		Deadline:       deadline,
	}, nil
}

// DecodeAddLiquidityETH extracts addLiquidityETH parameters
func DecodeAddLiquidityETH(input []byte) (AddLiquidityETHParams, error) {
	if Classify(input) != SigAddLiquidityETH {
		return AddLiquidityETHParams{}, errUnknown
	}

	// addLiquidityETH has 6 parameters: token, amountTokenDesired, amountTokenMin, amountETHMin, to, deadline
	token, ok := readAddress(input, 0)
	if !ok {
		return AddLiquidityETHParams{}, errShort
	}
	amountTokenDesired, ok := readUint(input, 1)
	if !ok {
		return AddLiquidityETHParams{}, errShort
	}
	amountTokenMin, ok := readUint(input, 2)
	if !ok {
		return AddLiquidityETHParams{}, errShort
	}
	amountETHMin, ok := readUint(input, 3)
	if !ok {
		return AddLiquidityETHParams{}, errShort
	}
	to, ok := readAddress(input, 4)
	if !ok {
		return AddLiquidityETHParams{}, errShort
	}
	deadline, ok := readUint(input, 5)
	if !ok {
		return AddLiquidityETHParams{}, errShort
	}

	return AddLiquidityETHParams{
		Token:          token,
		AmountTokenDes: amountTokenDesired,
		AmountTokenMin: amountTokenMin,
		AmountETHMin:   amountETHMin,
		To:             to,
		Deadline:       deadline,
	}, nil
}

// KindToName converts SigKind to readable string - used by v2.go
func KindToName(kind SigKind) string {
	switch kind {
	case SigAddLiquidityETH:
		return "addLiquidityETH"
	case SigAddLiquidity:
		return "addLiquidity"
	case SigSwapExactETHForTokens:
		return "swapExactETHForTokens"
	case SigSwapETHForExactTokens:
		return "swapETHForExactTokens"
	case SigSwapExactTokensForETH:
		return "swapExactTokensForETH"
	case SigSwapTokensForExactETH:
		return "swapTokensForExactETH"
	case SigSwapExactTokensForTokens:
		return "swapExactTokensForTokens"
	case SigSwapTokensForExactTokens:
		return "swapTokensForExactTokens"
	case SigSwapExactETHForTokensSupportingFeeOnTransferTokens:
		return "swapExactETHForTokensSupportingFeeOnTransferTokens"
	case SigSwapExactTokensForETHSupportingFeeOnTransferTokens:
		return "swapExactTokensForETHSupportingFeeOnTransferTokens"
	case SigSwapExactTokensForTokensSupportingFeeOnTransferTokens:
		return "swapExactTokensForTokensSupportingFeeOnTransferTokens"
	case SigRemoveLiquidity:
		return "removeLiquidity"
	case SigRemoveLiquidityETH:
		return "removeLiquidityETH"
	case SigRemoveLiquidityETHSupportingFeeOnTransferTokens:
		return "removeLiquidityETHSupportingFeeOnTransferTokens"
	case SigRemoveLiquidityWithPermit:
		return "removeLiquidityWithPermit"
	case SigRemoveLiquidityETHWithPermit:
		return "removeLiquidityETHWithPermit"
	case SigRemoveLiquidityETHWithPermitSupportingFeeOnTransferTokens:
		return "removeLiquidityETHWithPermitSupportingFeeOnTransferTokens"
	default:
		return "unknown"
	}
}

// -----------------------------------------------------------------------------
// Internal implementation - optimized for speed and safety
// -----------------------------------------------------------------------------

// Signature registry - covers all Uniswap V2 functions and variants
type sigEntry struct {
	sig  string
	kind SigKind
}

var sigList = []sigEntry{
	// Liquidity functions
	{"addLiquidity(address,address,uint256,uint256,uint256,uint256,address,uint256)", SigAddLiquidity},
	{"addLiquidityETH(address,uint256,uint256,uint256,address,uint256)", SigAddLiquidityETH},
	{"removeLiquidity(address,address,uint256,uint256,uint256,address,uint256)", SigRemoveLiquidity},
	{"removeLiquidityETH(address,uint256,uint256,uint256,address,uint256)", SigRemoveLiquidityETH},
	{"removeLiquidityETHSupportingFeeOnTransferTokens(address,uint256,uint256,address,uint256)", SigRemoveLiquidityETHSupportingFeeOnTransferTokens},
	{"removeLiquidityWithPermit(address,address,uint256,uint256,uint256,address,uint256,bool,uint8,bytes32,bytes32)", SigRemoveLiquidityWithPermit},
	{"removeLiquidityETHWithPermit(address,uint256,uint256,uint256,address,uint256,bool,uint8,bytes32,bytes32)", SigRemoveLiquidityETHWithPermit},
	{"removeLiquidityETHWithPermitSupportingFeeOnTransferTokens(address,uint256,uint256,address,uint256,bool,uint8,bytes32,bytes32)", SigRemoveLiquidityETHWithPermitSupportingFeeOnTransferTokens},

	// Standard swap functions
	{"swapExactETHForTokens(uint256,address[],address,uint256)", SigSwapExactETHForTokens},
	{"swapETHForExactTokens(uint256,address[],address,uint256)", SigSwapETHForExactTokens},
	{"swapExactTokensForETH(uint256,uint256,address[],address,uint256)", SigSwapExactTokensForETH},
	{"swapTokensForExactETH(uint256,uint256,address[],address,uint256)", SigSwapTokensForExactETH},
	{"swapExactTokensForTokens(uint256,uint256,address[],address,uint256)", SigSwapExactTokensForTokens},
	{"swapTokensForExactTokens(uint256,uint256,address[],address,uint256)", SigSwapTokensForExactTokens},

	// Fee-on-transfer variants (critical for many modern tokens)
	{"swapExactETHForTokensSupportingFeeOnTransferTokens(uint256,address[],address,uint256)", SigSwapExactETHForTokensSupportingFeeOnTransferTokens},
	{"swapExactTokensForETHSupportingFeeOnTransferTokens(uint256,uint256,address[],address,uint256)", SigSwapExactTokensForETHSupportingFeeOnTransferTokens},
	{"swapExactTokensForTokensSupportingFeeOnTransferTokens(uint256,uint256,address[],address,uint256)", SigSwapExactTokensForTokensSupportingFeeOnTransferTokens},
}

// Initialize the selector registry at startup
func initSelectorRegistry() {
	selectorToKind = make(map[[4]byte]SigKind, len(sigList))
	for _, entry := range sigList {
		selector := keccak4(entry.sig)
		selectorToKind[selector] = entry.kind
	}
}

// Fast keccak256 hash to 4-byte selector
func keccak4(signature string) [4]byte {
	hash := crypto.Keccak256([]byte(signature))
	var selector [4]byte
	copy(selector[:], hash[:4])
	return selector
}

// Bounds-checked calldata reading functions - no panics, no allocations

func readWord(input []byte, paramIndex int) ([]byte, bool) {
	offset := 4 + 32*paramIndex // Skip 4-byte selector, then 32-byte params
	if offset+32 > len(input) {
		return nil, false
	}
	return input[offset : offset+32], true
}

func readUint(input []byte, paramIndex int) (*big.Int, bool) {
	word, ok := readWord(input, paramIndex)
	if !ok {
		return nil, false
	}
	return new(big.Int).SetBytes(word), true
}

func readAddress(input []byte, paramIndex int) (common.Address, bool) {
	word, ok := readWord(input, paramIndex)
	if !ok {
		return common.Address{}, false
	}
	// Address is right-aligned in 32-byte word (last 20 bytes)
	return common.BytesToAddress(word[12:32]), true
}

func readOffset(input []byte, paramIndex int) (int, bool) {
	word, ok := readWord(input, paramIndex)
	if !ok {
		return 0, false
	}
	offset := new(big.Int).SetBytes(word).Int64()
	if offset < 0 {
		return 0, false
	}
	// Offset is relative to start of parameters (after 4-byte selector)
	absoluteOffset := int(offset) + 4
	if absoluteOffset > len(input) {
		return 0, false
	}
	return absoluteOffset, true
}

// Decode dynamic address array and extract first/last addresses + following static params
func decodePathToDeadline(input []byte, pathParamIndex int) (first, last, to common.Address, deadline *big.Int, err error) {
	// Get offset to dynamic array
	pathOffset, ok := readOffset(input, pathParamIndex)
	if !ok {
		return first, last, to, nil, errShort
	}

	// Read array length
	if pathOffset+32 > len(input) {
		return first, last, to, nil, errShort
	}
	arrayLength := new(big.Int).SetBytes(input[pathOffset : pathOffset+32]).Int64()
	if arrayLength <= 0 {
		return first, last, to, nil, errors.New("empty path array")
	}

	// Calculate positions of first and last elements
	firstElementOffset := pathOffset + 32
	lastElementOffset := firstElementOffset + int(arrayLength-1)*32

	// Bounds check
	if lastElementOffset+32 > len(input) {
		return first, last, to, nil, errShort
	}

	// Extract addresses (right-aligned in 32-byte words)
	first = common.BytesToAddress(input[firstElementOffset+12 : firstElementOffset+32])
	last = common.BytesToAddress(input[lastElementOffset+12 : lastElementOffset+32])

	// Read following static parameters: to, deadline
	toAddr, ok := readAddress(input, pathParamIndex+1)
	if !ok {
		return first, last, to, nil, errShort
	}
	deadlineValue, ok := readUint(input, pathParamIndex+2)
	if !ok {
		return first, last, to, nil, errShort
	}

	return first, last, toAddr, deadlineValue, nil
}
