package mempool

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
	v2 "github.com/meltingclock/biteblock_v1/internal/dex/v2"
	"github.com/meltingclock/biteblock_v1/internal/signals"
)

type TxHandler func(context.Context, *types.Transaction) error

type Watcher struct {
	WSSURL      string
	OnTx        TxHandler
	Workers     int
	DialTimeout time.Duration

	// internal
	wg sync.WaitGroup

	// --- NEW: long-lived client for logs + DEX signal plumbing ---
	logEC       *ethclient.Client
	cancelPairs context.CancelFunc
	dexRouter   common.Address // optional: for IsRouter fast check
	// liqAnalyzer *signals.LiquidityAnalyzer
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

	// --- NEW: long-lived client for logs/abi calls used by signals ---
	dialCtx, cancel := context.WithTimeout(ctx, w.DialTimeout)
	defer cancel()
	rpc2, err := rpc.DialContext(dialCtx, w.WSSURL)
	if err != nil {
		return err
	}
	w.logEC = ethclient.NewClient(rpc2)

	// --- NEW: DEX v2 config (put these in your config later) ---
	// UniswapV2 (ETH mainnet) as example; replace per your target chain
	factory := common.HexToAddress("0x5C69bEe701ef814a2B6a3EDD4B1652CB9cc5aA6f")
	router := common.HexToAddress("0x7a250d5630B4cF539739dF2C5dAcb4c659F2488D")
	w.dexRouter = router

	// Start PairCrated registry subscription (in its own goroutine)
	// NOTE: I'm keeping the actual PairRegistry & LiquidityAnalyzer
	// inisde the OnTx wrapper below to minimize edits here. if you prefer,
	// you can store them on Wather fields instead.
	w.startDEXSignals(ctx, factory, router)

	// keep original handler (may be nil)
	orig := w.OnTx

	// Build the analyzer objects once (require adding new packages)
	dex := v2.NewRegistry(v2.Config{
		Network: v2.Ethereum, // specify the network
		Factory: factory,
		Router:  w.dexRouter,
	})
	pairReg := signals.NewPairRegistry(dex, 48*time.Hour)
	// Start PairCreated subscription on the long-lived client
	prCtx, cancelPairs := context.WithCancel(ctx)
	w.cancelPairs = cancelPairs
	pairReg.Start(prCtx, w.logEC)

	liq := signals.NewLiquidityAnalyzer(w.logEC, dex, pairReg)

	// Decorate OnTx with liquidity detection
	w.OnTx = func(ctx context.Context, tx *types.Transaction) error {
		if tx == nil {
			if orig != nil {
				return orig(ctx, tx)
			}
			return nil
		}
		// fast address gate (only care if To() == router)
		to := tx.To()
		if to != nil && *to == w.dexRouter {
			// analyze for addLiquidity* selectors and decode minimal params
			sig, _ := liq.AnalyzePending(ctx, tx)
			if sig != nil {
				log.Printf("[signals] liquidity(pending) pair=%s token0=%s token1=%s from=%s kind=%d",
					sig.Pair.Hex(), sig.Token0.Hex(), sig.Token1.Hex(), sig.From.Hex(), sig.Kind)
				// TODO next: push `sig` into scanner/strategy/executor pipeline
			}
		}
		if orig != nil {
			return orig(ctx, tx)
		}
		return nil
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
							_ = w.OnTx(ctx, tx)
						}
					}
				}
			}()
		}
		<-ctx.Done()
		close(txCh)
		wg.Wait()
	}()

	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		w.runLoop(ctx, txCh)
	}()

	return nil
}

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
		log.Printf("[mempool] subscribe error: %v; reconnecting in %s", err, delay)
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
	hashes := make(chan common.Hash, 4096)
	sub, err := rpcClient.EthSubscribe(ctx, hashes, "newPendingTransactions")
	if err != nil {
		return err
	}
	defer sub.Unsubscribe()
	log.Printf("[mempool] subscribed to newPendingTransactions")

	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-sub.Err():
			if err != nil {
				return err
			}
			return nil
		case h := <-hashes:
			// fetch the full tx
			tx, _, err := ethCl.TransactionByHash(ctx, h)
			if err != nil {
				// ignore transient not found / dropped
				continue
			}
			select {
			case txCh <- tx:
			default:
				// channel full -> drop oldest behavior is not trivial; log and skip
				log.Printf("[mempool] txCh full; dropping tx %s", h.Hex())
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
