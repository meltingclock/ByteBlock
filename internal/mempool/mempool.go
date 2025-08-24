package mempool

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/meltingclock/biteblock_v1/internal/telemetry"
)

type TxHandler func(context.Context, *types.Transaction) error

type Watcher struct {
	WSSURL      string
	OnTx        TxHandler
	Workers     int
	DialTimeout time.Duration

	// internal
	wg sync.WaitGroup
}

func NewWatcher(wssURL string, onTx TxHandler) *Watcher {
	return &Watcher{
		WSSURL:      wssURL,
		OnTx:        onTx,
		Workers:     8,
		DialTimeout: 10 * time.Second,
	}
}

func (w *Watcher) Start(ctx context.Context) error {
	if w.WSSURL == "" {
		return errors.New("WSSURL is empty")
	}
	if w.Workers <= 0 {
		w.Workers = 1
	}

	// worker pool for processing
	txCh := make(chan *types.Transaction, 1024)
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		var wg sync.WaitGroup
		for i := 0; i < w.Workers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					select {
					case <-ctx.Done():
						return
					case tx := <-txCh:
						if tx == nil {
							return
						}
						if w.OnTx != nil {
							_ = w.OnTx(ctx, tx) // controller-provided callback
						}
					}
				}
			}()
		}
		<-ctx.Done()
		close(txCh)
		wg.Wait()
	}()

	// subscription loop
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		w.runLoop(ctx, txCh)
	}()

	return nil
}

/*
// --- NEW helper: start PairCreated subscription ---
func (w *Watcher) startDEXSignals(ctx context.Context, factory, router common.Address) {
	// build local registry/analyzer once used by the OnTx wrapper
	// we’ll also set up a background PairCreated subscriber here.
	// To avoid pulling more files into your mempool package,
	// keep this function minimal if you're not yet adding the other packages.
	// If you DO add internal/dex/v2 and internal/signals packages now, wire them here.
}

func (w *Watcher) Stop() {
	if w.cancelPairs != nil {
		w.cancelPairs()
	}
	if w.logEC != nil {
		w.logEC.Close()
	}
}
*/

func (w *Watcher) Wait() {
	w.wg.Wait()
}

func (w *Watcher) runLoop(ctx context.Context, txCh chan<- *types.Transaction) {
	var attempt int
	for {
		if ctx.Err() != nil {
			return
		}
		err := w.subscribeOnce(ctx, txCh)
		if err == nil || ctx.Err() != nil {
			return
		}

		// backoff
		delayMs := 500 * (1 << uint(min(attempt, 6)))
		if delayMs > 8000 {
			delayMs = 8000
		}
		delay := time.Duration(delayMs) * time.Millisecond
		telemetry.Warnf("[mempool] subscribe error: %v; reconnecting in %s", err, delay)
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		attempt++
	}
}

func (w *Watcher) subscribeOnce(ctx context.Context, txCh chan<- *types.Transaction) error {
	dialCtx, cancel := context.WithTimeout(ctx, w.DialTimeout)
	defer cancel()

	rpcClient, err := rpc.DialContext(dialCtx, w.WSSURL)
	if err != nil {
		return err
	}
	defer rpcClient.Close()

	ethCl := ethclient.NewClient(rpcClient)
	defer ethCl.Close()

	// Subscribe to pending transaction hashes
	hashes := make(chan common.Hash, 8192) // was 4096; bump a bit
	sub, err := rpcClient.EthSubscribe(ctx, hashes, "newPendingTransactions")
	if err != nil {
		return err
	}
	defer sub.Unsubscribe()

	// NEW: fan-out fetchers to resolve hashes -> tx concurrently
	fetchers := max(2, w.Workers/2)              // tune: start with 4–8
	txOut := make(chan *types.Transaction, 2048) // stage between fetchers and workers

	/*
		// --- 1) Prefer FULL pending tx bodies (Geth / some nodes) ---
		fullTxs := make(chan *types.Transaction, 4096)
		sub, err := rpcClient.EthSubscribe(ctx, fullTxs, "newPendingTransactions", true)
		if err == nil {
			log.Printf("[mempool] subscribed to newPendingTransactions (full bodies)")
			defer sub.Unsubscribe()
			for {
				select {
				case <-ctx.Done():
					return nil
				case err := <-sub.Err():
					if err != nil {
						return err
					}
					return nil
				case tx := <-fullTxs:
					if tx == nil {
						continue
					}
					select {
					case txCh <- tx:
					default:
						log.Printf("[mempool] txCh full (%d); dropping %s", len(txCh), tx.Hash().Hex())
					}
				}
			}
		}
	*/

	var fg sync.WaitGroup
	fg.Add(fetchers)
	for i := 0; i < fetchers; i++ {
		go func() {
			defer fg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case h := <-hashes:
					tx, _, err := ethCl.TransactionByHash(ctx, h)
					if err != nil {
						continue
					}
					select {
					case txOut <- tx:
					default:
						// drop if saturated (or use a non-blocking spillover)
					}
				}
			}
		}()
	}

	defer func() { // drain + join fetchers
		close(txOut)
		fg.Wait()
	}()
	telemetry.Infof("[mempool] subscribed to newPendingTransactions")

	// forward to your existing worker pool
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-sub.Err():
			if err != nil {
				return err
			}
			return nil
		case tx := <-txOut:
			select {
			case txCh <- tx:
			default:
				telemetry.Warnf("[mempool] txCh full (%d); dropping %s", len(txCh), tx.Hash().Hex())
			}
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// SendTxVerifier returns the sender derived from the tx signature.
func SendTxVerifier(tx *types.Transaction) (common.Address, error) {
	if tx == nil {
		return common.Address{}, nil
	}
	signer, err := types.Sender(types.LatestSignerForChainID(tx.ChainId()), tx)
	return signer, err
}
