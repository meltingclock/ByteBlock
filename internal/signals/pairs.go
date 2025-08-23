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
	byTok  map[string]common.Address // NEW: tokenA|tokenB -> pair (canonical order)
	ttl    time.Duration
	dex    *dexv2.Registry
	fab    abi.ABI
	cancel context.CancelFunc
	client ethereum.LogFilterer
}

func NewPairRegistry(dex *dexv2.Registry, ttl time.Duration) *PairRegistry {
	fab, _ := abi.JSON(strings.NewReader(dexv2.FactoryABI))
	return &PairRegistry{
		pairs: make(map[common.Address]pairInfo),
		byTok: make(map[string]common.Address), // NEW: tokenA|tokenB -> pair (canonical order)
		ttl:   ttl,
		dex:   dex,
		fab:   fab,
	}
}

func (pr *PairRegistry) Start(ctx context.Context, client ethereum.LogFilterer) {
	pr.client = client
	ctx, cancel := context.WithCancel(ctx)
	pr.cancel = cancel

	q := ethereum.FilterQuery{
		Addresses: []common.Address{pr.dex.Factory()},
		Topics:    [][]common.Hash{{pr.dex.TopicPairCreated()}},
	}

	/*
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
	*/
	go func() {
		backoff := time.Millisecond * 500
		for {
			if ctx.Err() != nil {
				return
			}
			log.Println("[signals] PairRegistry subscribing PairCreated")
			ch := make(chan types.Log, 1024)
			sub, err := client.SubscribeFilterLogs(ctx, q, ch)
			if err != nil {
				log.Printf("[signals] PairRegistry subscribe error: %v", err)
				select {
				case <-ctx.Done():
					return
				case <-time.After(backoff):
					if backoff < time.Second*8 {
						backoff *= 2
					}
					continue
				}
			}
			// Active stream
			backoff = time.Millisecond * 500
			ticker := time.NewTicker(time.Minute)
			for {
				select {
				case <-ctx.Done():
					defer ticker.Stop()
					defer sub.Unsubscribe()
					return
				case err := <-sub.Err():
					log.Printf("[signals] PairRegistry sub err: %v", err)
					// break inner loop to resubscribe
					defer ticker.Stop()
					defer sub.Unsubscribe()
					goto RESUB
				case lg := <-ch:
					pr.handlePairCreated(lg)
				case <-ticker.C:
					pr.gc()
				}
			}
		RESUB:
			// loop to resubscribe with backoff
			if backoff < time.Second*8 {
				backoff *= 2
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
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
	// canonical key: token0 < token1 already holds for V2, but keep generic
	k := tokKey(token0, token1)
	pr.byTok[k] = pair
	pr.mu.Unlock()
	log.Printf("[signals] PairCreated pair=%s token0=%s token1=%s block=%d",
		pair.Hex(), token0.Hex(), token1.Hex(), lg.BlockNumber)

	// BACKFILL: catch Mint that happened in the same tx/block.
	if pr.client != nil {
		go func(block uint64, p common.Address) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			if evt, _ := BackfillMint(ctx, pr.client, p, block, block); evt != nil {
				log.Printf("[liquidity][mined/backfill] pair=%s sender=%s amount0=%s amount1=%s block=%d",
					p.Hex(), evt.Sender.Hex(), evt.Amount0.String(), evt.Amount1.String(), evt.Log.BlockNumber)
				return
			}
			// small cushion
			if evt, _ := BackfillMint(ctx, pr.client, p, block, block+1); evt != nil {
				log.Printf("[liquidity][mined/backfill+] pair=%s sender=%s amount0=%s amount1=%s block=%d",
					p.Hex(), evt.Sender.Hex(), evt.Amount0.String(), evt.Amount1.String(), evt.Log.BlockNumber)
				return
			}
		}(lg.BlockNumber, pair)
	}
}

func tokKey(a, b common.Address) string {
	la := strings.ToLower(a.Hex())
	lb := strings.ToLower(b.Hex())
	if la < lb {
		return la + "|" + lb
	}
	return lb + "|" + la
}

func (pr *PairRegistry) gc() {
	exp := time.Now().Add(-pr.ttl)
	pr.mu.Lock()
	for p, info := range pr.pairs {
		if info.SeenAt.Before(exp) {
			delete(pr.pairs, p)
			delete(pr.byTok, tokKey(info.Token0, info.Token1))
		}
	}
	pr.mu.Unlock()
}

// GetByTokens returns a cached pair for (a,b) regardless of order.
func (pr *PairRegistry) GetByTokens(a, b common.Address) (common.Address, bool) {
	pr.mu.RLock()
	defer pr.mu.RUnlock()
	p, ok := pr.byTok[tokKey(a, b)]
	return p, ok
}

func (pr *PairRegistry) Get(pair common.Address) (pairInfo, bool) {
	pr.mu.RLock()
	defer pr.mu.RUnlock()
	pi, ok := pr.pairs[pair]
	return pi, ok
}

// Helpful for status pages
func (pr *PairRegistry) Size() int { pr.mu.RLock(); defer pr.mu.RUnlock(); return len(pr.pairs) }

// Pairs returns a snapshot of known pair addresses.
func (pr *PairRegistry) Pairs() []common.Address {
	pr.mu.RLock()
	defer pr.mu.RUnlock()
	out := make([]common.Address, 0, len(pr.pairs))
	for p := range pr.pairs {
		out = append(out, p)
	}
	return out
}
