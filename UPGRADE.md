
# Biteblock upgrade: Go 1.23 & robust mempool watcher

This package was updated to target **Go 1.23** and to add a resilient mempool subscription with auto‑reconnect, worker pool processing, and graceful shutdown.

## What changed
- `go.mod` -> `go 1.23.0` plus `go toolchain go1.23.0` hint.
- `internal/mempool/mempool.go` rewritten:
  - Uses `rpc.EthSubscribe(..., "newPendingTransactions")` + `TransactionByHash` to fetch full transactions.
  - Exponential backoff reconnects on dropped websockets.
  - Bounded channel and worker pool (`Workers` configurable) to avoid memory bloat.
  - Graceful shutdown via context and `Wait()`.
- `cmd/sniper/main.go`:
  - Loads `WSS_URL` from `config.yml` or env (`BITEBLOCK_WSS_URL`).
  - Fast dial check, clean signal handling.
  - Logs each pending tx: hash, from, to, nonce, value.

## Run
```bash
go 1.23
go mod tidy
WSS_URL=wss://eth-mainnet.g.alchemy.com/v2/YOUR_KEY go run ./cmd/sniper
```

## Extend
Implement richer `OnTx` logic in `cmd/sniper/main.go` (e.g., filter for liquidity events, decode calldata, route to a simulator, etc.).
