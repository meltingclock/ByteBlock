package signals

import (
	"context"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"

	dexv2 "github.com/meltingclock/biteblock_v1/internal/dex/v2"
)

type pairInfo struct {
	Token0, Token1 common.Address
	CreatedAtBlock uint64
	SeenAt         time.Time
}

type PairRegistry struct {
	mu     sync.RWMutex
	pairs  map[common.Address]pairInfo
	ttl    time.Duration
	dex    *dexv2.Registry
	fab    abi.ABI
	cancel context.CancelFunc
}

func NewPairRegistry(dex *dexv2.Registry, ttl time.Duration) *PairRegistry {
	fab, _ := abi.JSON(strings.NewReader(dexv2.FactoryABI))
	return &PairRegistry{
		pairs: make(map[common.Address]pairInfo),
		ttl:   ttl,
		dex:   dex,
		fab:   fab,
	}
}

func (pr *PairRegistry) Start(ctx context.Context, client ethereum.LogFilterer) {
	ctx, cancel := context.WithCancel(ctx)
	pr.cancel = cancel

	q := ethereum.FilterQuery{
		Addresses: []common.Address{pr.dex.Factory()},
		Topics:    [][]common.Hash{{pr.dex.TopicPairCreated()}},
	}

	log.Println("[signals] PairRegistry subscribing PairCreated")
	ch := make(chan types.Log, 1024)
	sub, err := client.SubscribeFilterLogs(ctx, q, ch)
	if err != nil {
		log.Printf("[signals] PairRegistry subscribe error: %v", err)
		return
	}

	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case err := <-sub.Err():
				log.Printf("[signals] PairRegistry sub err: %v", err)
				return
			case lg := <-ch:
				pr.handlePairCreated(lg)
			case <-ticker.C:
				pr.gc()

			}
		}
	}()
}

func (pr *PairRegistry) Stop() {
	if pr.cancel != nil {
		pr.cancel()
	}
}

func (pr *PairRegistry) handlePairCreated(lg types.Log) {
	if len(lg.Topics) < 3 {
		return
	}
	token0 := common.BytesToAddress(lg.Topics[1].Bytes()[12:])
	token1 := common.BytesToAddress(lg.Topics[2].Bytes()[12:])
	// Decode data for pair adddress (non-indexed)

	evt := pr.fab.Events["PairCreated"]
	// Only non-indexed args live in lg.Data [pair, <uint>]
	vals, err := evt.Inputs.NonIndexed().Unpack(lg.Data)
	if err != nil {
		log.Printf("[signals] PairCreated unpack err: %v", err)
		return
	}
	pair := vals[0].(common.Address)
	/*
		var out struct {
			Pair common.Address
			_    *big.Int
		}
		if err := pr.fab.UnpackIntoInterface(&out, "PairCreated", lg.Data); err != nil {
			log.Printf("[signals] decode PairCreated data err: %v", err)
			return
		}
	*/
	pi := pairInfo{Token0: token0, Token1: token1, CreatedAtBlock: lg.BlockNumber, SeenAt: time.Now()}
	pr.mu.Lock()
	pr.pairs[pair] = pi
	pr.mu.Unlock()
	log.Printf("[signals] PairCreated pair=%s token0=%s token1=%s block=%d",
		pair.Hex(), token0.Hex(), token1.Hex(), lg.BlockNumber)
}

func (pr *PairRegistry) gc() {
	exp := time.Now().Add(-pr.ttl)
	pr.mu.Lock()
	for p, info := range pr.pairs {
		if info.SeenAt.Before(exp) {
			delete(pr.pairs, p)
		}
	}
	pr.mu.Unlock()
}

func (pr *PairRegistry) Get(pair common.Address) (pairInfo, bool) {
	pr.mu.RLock()
	defer pr.mu.RUnlock()
	pi, ok := pr.pairs[pair]
	return pi, ok
}

// Helpful for status pages
func (pr *PairRegistry) Size() int { pr.mu.RLock(); defer pr.mu.RUnlock(); return len(pr.pairs) }
