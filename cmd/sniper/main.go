
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/spf13/viper"

	"github.com/meltingclock/biteblock_v1/internal/config"
	"github.com/meltingclock/biteblock_v1/internal/mempool"
	"github.com/meltingclock/biteblock_v1/internal/telegram"
)

func loadConfig() (string, error) {
	v := viper.New()
	v.SetConfigName("config")
	v.SetConfigType("yaml")
	v.AddConfigPath(".")
	v.AddConfigPath("./biteblock_0.0.2") // fallback if run from repo root

	v.AutomaticEnv()
	v.SetEnvPrefix("BITEBLOCK")
	_ = v.ReadInConfig()

	// Accept both WSS_URL and WSSURL for flexibility
	wss := v.GetString("WSS_URL")
	if wss == "" {
		wss = v.GetString("WSSURL")
	}
	if wss == "" {
		// final fallback: HTTPS_URL can be upgraded if it's wss-capable
		wss = os.Getenv("WSS_URL")
	}
	if wss == "" {
		return "", nil
	}
	return wss, nil
}

func main() {
	
	// load YAML config (create if missing)
	cfg, err := config.Load("config.yml")
	if err != nil {
		log.Fatalf("config error: %v", err)
	}

	// If TELEGRAM_TOKEN present, run in Telegram-control mode
	if cfg.TELEGRAM_TOKEN != "" {
		root, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()

		ctrl, err := telegram.NewController(cfg, "config.yml")
		if err != nil {
			log.Fatalf("telegram init: %v", err)
		}
		log.Println("telegram mode: listening for commands")
		if err := ctrl.Start(root); err != nil {
			log.Fatalf("telegram loop error: %v", err)
		}
		return
	}

	wss, err := loadConfig()
	if err != nil {
		log.Fatalf("config error: %v", err)
	}
	if wss == "" {
		log.Println("WSS_URL not set in config.yml or env; exiting")
		return
	}

	// quick connectivity sanity check to fail fast
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rpc, err := ethclient.DialContext(ctx, wss)
	if err != nil {
		log.Fatalf("failed to dial WSS: %v", err)
	}
	rpc.Close()

	// parent ctx with cancel on signals
	root, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	w := mempool.NewWatcher(wss, func(ctx context.Context, tx *types.Transaction) error {
		// (see below) This is a small trick to avoid circular import in code snippet;
		// We'll rebind to types.Transaction using a type alias via build tag or shim.
		return nil
	})
	// Replace the above with a handler inlined below:
	w = mempool.NewWatcher(wss, func(ctx context.Context, tx *types.Transaction) error {
		from, _ := mempool.SendTxVerifier(tx)
		log.Printf("[tx] hash=%s from=%s to=%s nonce=%d val=%s",
			tx.Hash().Hex(), from.Hex(),
			addrToHex(tx.To()), tx.Nonce(), tx.Value().String())
		return nil
	})

	if err := w.Start(root); err != nil {
		log.Fatalf("watcher start error: %v", err)
	}
	log.Println("sniper started; listening for pending transactions")

	<-root.Done()
	log.Println("shutting down...")
	w.Wait()
	log.Println("bye")
}

// helpers
// avoid nil pointer in To()
func addrToHex(to *common.Address) string {
	if to == nil {
		return "<contract-creation>"
	}
	return to.Hex()
}
