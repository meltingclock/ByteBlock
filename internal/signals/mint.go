package signals

import (
	"context"
	"math/big"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

const pairABIJSON = `[
  {"anonymous":false,"inputs":[
    {"indexed":true,"internalType":"address","name":"sender","type":"address"},
    {"indexed":false,"internalType":"uint256","name":"amount0","type":"uint256"},
    {"indexed":false,"internalType":"uint256","name":"amount1","type":"uint256"}],
   "name":"Mint","type":"event"}
]`

var (
	pairABI   abi.ABI
	topicMint common.Hash
)

func init() {
	pab, _ := abi.JSON(strings.NewReader(pairABIJSON))
	pairABI = pab
	topicMint = pairABI.Events["Mint"].ID
}

type MintEvent struct {
	Sender  common.Address
	Amount0 *big.Int
	Amount1 *big.Int
	Log     types.Log
}

func MintTopic() common.Hash {
	return topicMint
}

// ParseMintLog returns a decoded Mint if the log matches the Mint topic/address.
func ParseMintLog(lg types.Log) (*MintEvent, bool) {
	if len(lg.Topics) == 0 || lg.Topics[0] != topicMint {
		return nil, false
	}
	// indexed: sender (topics[1]); non-indexed: amount0, amount1
	sender := common.Address{}
	if len(lg.Topics) > 1 {
		sender = common.BytesToAddress(lg.Topics[1].Bytes()[12:])
	}
	var out struct {
		Amount0 *big.Int
		Amount1 *big.Int
	}
	if err := pairABI.UnpackIntoInterface(&out, "Mint", lg.Data); err != nil {
		return nil, false
	}
	return &MintEvent{
		Sender:  sender,
		Amount0: out.Amount0,
		Amount1: out.Amount1,
		Log:     lg,
	}, true
}

// BackfillMint looks for a Mint on `pair` in [fromBlock, toBlock].
func BackfillMint(ctx context.Context, lf ethereum.LogFilterer, pair common.Address, fromBlock, toBlock uint64) (*MintEvent, error) {
	q := ethereum.FilterQuery{
		FromBlock: new(big.Int).SetUint64(fromBlock),
		ToBlock:   new(big.Int).SetUint64(toBlock),
		Addresses: []common.Address{pair},
		Topics:    [][]common.Hash{{topicMint}},
	}
	// ethclient.Client implements FilterLogs via embedded interface
	logs, err := lf.FilterLogs(ctx, q)
	if err != nil || len(logs) == 0 {
		return nil, err
	}
	for _, lg := range logs {
		if evt, ok := ParseMintLog(lg); ok {
			return evt, nil
		}
	}
	return nil, nil
}

// FindMintInReceipt scans a tx receipt logs for the first Mint emitted by `pair`.
func FindMintInReceipt(rcpt *types.Receipt, pair common.Address) (*MintEvent, bool) {
	if rcpt == nil || len(rcpt.Logs) == 0 {
		return nil, false
	}
	for _, pl := range rcpt.Logs {
		if pl == nil || pl.Address != pair {
			continue
		}
		if evt, ok := ParseMintLog(*pl); ok {
			return evt, true
		}
	}
	return nil, false
}

// WithTimeout helper
func withTimeout(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	return context.WithTimeout(parent, d)
}
