package mempool

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/meltingclock/biteblock_v1/internal/telemetry"
)

type TxHandler func(context.Context, *types.Transaction) error

// Watcher - enhanced with performance tuning parameters
type Watcher struct {
	WSSURL      string
	OnTx        TxHandler
	Workers     int // Processing workers (CPU-bound work)
	DialTimeout time.Duration

	// NEW: Performance tuning parameters
	HashQueueSize int // Buffer for incoming hashes from Websocket
	TxQueueSize   int // Buffer for fetched transactions
	FetchWorkers  int // RPC fetch workers (I/O-bound work)

	// internal state
	wg       sync.WaitGroup
	metrics  watcherMetrics
	stopping int32 // Atomic flag for graceful shutdown
}

// Metrics structure for monitoring performance
type watcherMetrics struct {
	hashesReceived   int64 // Hashes received from Websocket
	txsProcessed     int64 // Transactions succesfully processed
	txsFetched       int64 // Transactions succesfully fetched
	fetchErrors      int64 // RPC calls failures
	processingErrors int64 // Errors in OnTx callback
	droppedHashes    int64 // Hashes dropped due to queue overflow
	droppedTxs       int64 // Transactions dropped due to queue overflow
}

func NewWatcher(wssURL string, onTx TxHandler) *Watcher {
	numCPU := runtime.NumCPU()

	return &Watcher{
		WSSURL:        wssURL,
		OnTx:          onTx,
		Workers:       min(8, numCPU*2),  // Conservative for processing
		FetchWorkers:  min(16, numCPU*4), // More for I/O-bound fetching
		DialTimeout:   10 * time.Second,
		HashQueueSize: 16384,
		TxQueueSize:   4096,
	}
}

func (w *Watcher) Start(ctx context.Context) error {
	if w.WSSURL == "" {
		return errors.New("WSSURL is empty")
	}
	if w.Workers <= 0 {
		w.Workers = 1
	}
	if w.FetchWorkers <= 0 {
		w.FetchWorkers = 4
	}

	// Pipeline channels
	// Stage 1: Websocket -> Hash Queue
	hashCh := make(chan common.Hash, w.HashQueueSize)

	// Stage 2: Hash Queue -> Fetch Workers -> TX Queue
	txCh := make(chan *types.Transaction, w.TxQueueSize)

	// Start metrics reporter (runs every 30 seconds)
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		w.metricsLoop(ctx)
	}()

	// Start processing workers (Stage 3: TX Queue -> OnTx callback)
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		w.processWorkers(ctx, txCh)
	}()

	// Start fetch workers (Stage 2: Hash Queue -> RPC -> TX Queue)
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		w.fetchWorkers(ctx, hashCh, txCh)
	}()

	// subscription loop
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		w.runSubscription(ctx, hashCh)
	}()

	return nil
}

// fetchWorkers - Stage 2: Convert hashes to full transactions in parallel
func (w *Watcher) fetchWorkers(ctx context.Context, hashCh <-chan common.Hash, txCh chan<- *types.Transaction) {
	// Create pool of RPC clients for parallel fetching
	clients := make([]*ethclient.Client, w.FetchWorkers)

	// Initialize each client
	for i := range clients {
		dialCtx, cancel := context.WithTimeout(ctx, w.DialTimeout)
		rpcClient, err := rpc.DialContext(dialCtx, w.WSSURL)
		cancel()

		if err != nil {
			telemetry.Errorf("[mempool] fetch worker %d dial failed: %v", i, err)
			continue
		}
		clients[i] = ethclient.NewClient(rpcClient)
	}
	defer func() {
		for _, client := range clients {
			if client != nil {
				client.Close()
			}
		}
		close(txCh)
	}()

	// Start fetch workers
	var fetchWg sync.WaitGroup
	for i := 0; i < w.FetchWorkers; i++ {
		if clients[i] == nil {
			continue // Skip failed client initialization
		}
		fetchWg.Add(1)
		go func(clientIdx int) {
			defer fetchWg.Done()
			client := clients[clientIdx]

			for {
				select {
				case <-ctx.Done():
					return
				case hash, ok := <-hashCh:
					if !ok {
						return // Channel closed
					}

					// Fetch transactions with timeout
					fetchCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
					tx, _, err := client.TransactionByHash(fetchCtx, hash)
					cancel()

					if err != nil {
						atomic.AddInt64(&w.metrics.fetchErrors, 1)
						telemetry.Tracef("[mempool] fetch error for %s: %v", hash.Hex(), err)
						continue
					}
					atomic.AddInt64(&w.metrics.txsFetched, 1)

					// Non-blocking send to processing queue
					select {
					case txCh <- tx:
						// Success
					default:
						atomic.AddInt64(&w.metrics.droppedTxs, 1)
						telemetry.Tracef("[mempool] tx queue full, dropping %s", tx.Hash().Hex())

					}
				}
			}
		}(i)
	}
	fetchWg.Wait() // Wait for all fetch workers to complete
}

// processWorkers - Stage 3: Process transactions in parallel
func (w *Watcher) processWorkers(ctx context.Context, txCh <-chan *types.Transaction) {
	var processWg sync.WaitGroup

	// Start processing workers
	for i := 0; i < w.Workers; i++ {
		processWg.Add(1)
		go func(workerID int) {
			defer processWg.Done()

			for {
				select {
				case <-ctx.Done():
					return
				case tx, ok := <-txCh:
					if !ok {
						return // Channel closed
					}

					// Call your existing OnTx callback
					if w.OnTx != nil {
						if err := w.OnTx(ctx, tx); err != nil {
							atomic.AddInt64(&w.metrics.processingErrors, 1)
							telemetry.Tracef("[mempool] process error for %s: %v", tx.Hash().Hex(), err)
						}
					}

					atomic.AddInt64(&w.metrics.txsProcessed, 1)
				}
			}
		}(i)
	}

	processWg.Wait() // Wait for all processing workers to complete
}

// metricsLoop - Reports performance statistics every 30 seconds
func (w *Watcher) metricsLoop(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	var lastProcessed, lastFetched int64

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Read current metrics atomically
			processed := atomic.LoadInt64(&w.metrics.txsProcessed)
			fetched := atomic.LoadInt64(&w.metrics.txsFetched)
			fetchErrors := atomic.LoadInt64(&w.metrics.fetchErrors)
			droppedHashes := atomic.LoadInt64(&w.metrics.droppedHashes)
			droppedTxs := atomic.LoadInt64(&w.metrics.droppedTxs)

			// Calculate rates
			processRate := processed - lastProcessed
			fetchRate := fetched - lastFetched

			// Log performance summary
			telemetry.Infof("[mempool] processed: %d (+%d/30s), fetched: %d (+%d/30s), fetch_errors: %d, dropped_hashes: %d, dropped_txs: %d",
				processed, processRate, fetched, fetchRate, fetchErrors, droppedHashes, droppedTxs)

			lastProcessed = processed
			lastFetched = fetched
		}
	}
}

// runSubscription - Stage 1: WebSocket subscription with auto-reconnect
func (w *Watcher) runSubscription(ctx context.Context, hashCh chan<- common.Hash) {
	defer close(hashCh) // Signal fetch workers to shut down

	var attempt int
	for {
		if ctx.Err() != nil || atomic.LoadInt32(&w.stopping) == 1 {
			return
		}

		err := w.subscribeOnce(ctx, hashCh)
		if err == nil || ctx.Err() != nil {
			return
		}

		// Exponential backoff with jitter for reconnection
		delayMs := 500 * (1 << uint(min(attempt, 6))) // Cap at 2^6 = 64
		if delayMs > 8000 {
			delayMs = 8000 // Max 8 second delay
		}
		delay := time.Duration(delayMs) * time.Millisecond

		telemetry.Warnf("[mempool] subscribe error (attempt %d): %v; reconnecting in %s",
			attempt+1, err, delay)

		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		attempt++
	}
}

// subscribeOnce - Single WebSocket subscription attempt
func (w *Watcher) subscribeOnce(ctx context.Context, hashCh chan<- common.Hash) error {
	dialCtx, cancel := context.WithTimeout(ctx, w.DialTimeout)
	defer cancel()

	rpcClient, err := rpc.DialContext(dialCtx, w.WSSURL)
	if err != nil {
		return err
	}
	defer rpcClient.Close()

	// Subscribe to pending transaction hashes
	hashes := make(chan common.Hash, w.HashQueueSize)
	sub, err := rpcClient.EthSubscribe(ctx, hashes, "newPendingTransactions")
	if err != nil {
		return err
	}
	defer sub.Unsubscribe()

	telemetry.Infof("[mempool] subscribed to newPendingTransactions")

	// Forward hashes to the processing pipeline
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-sub.Err():
			if err != nil {
				return err
			}
			return nil
		case hash := <-hashes:
			atomic.AddInt64(&w.metrics.hashesReceived, 1)

			// Non-blocking send to prevent WebSocket blocking
			select {
			case hashCh <- hash:
				// Success
			default:
				atomic.AddInt64(&w.metrics.droppedHashes, 1)
				// Don't log each drop to avoid log spam
			}
		}
	}
}

// Wait for graceful shutdown
func (w *Watcher) Wait() {
	atomic.StoreInt32(&w.stopping, 1)
	w.wg.Wait()
}

// Helper function
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// SendTxVerifier - unchanged from your existing code
func SendTxVerifier(tx *types.Transaction) (common.Address, error) {
	if tx == nil {
		return common.Address{}, nil
	}
	signer, err := types.Sender(types.LatestSignerForChainID(tx.ChainId()), tx)
	return signer, err
}
