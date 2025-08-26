package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/meltingclock/biteblock_v1/internal/config"
	"github.com/meltingclock/biteblock_v1/internal/telegram"
	"github.com/meltingclock/biteblock_v1/internal/telemetry"
)

func main() {
	// Load config (creates config.yml if missing)
	telemetry.Start()
	defer telemetry.Stop()

	// Ctrl-C / SIGTERM handling
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	/*
		cfg, err := config.Load(config.DefaultPath)
		if err != nil {
			log.Fatalf("config load: %v", err)
		}

		// Optional: fail-fast if token missing (you can relax this if you want)
		if err := cfg.Validate(); err != nil {
			log.Fatalf("config validation: %v", err)
		}

		// Ctrl-C / SIGTERM handling
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()

		// Telegram controller owns network presets + mempool + signals
		ctrl, err := telegram.NewController(cfg, config.DefaultPath)
		if err != nil {
			log.Fatalf("telegram init: %v", err)
		}

		telemetry.Infof("telegram mode: listening for commands")
		if err := ctrl.Start(ctx); err != nil {
			log.Fatalf("controller error: %v", err)
		}
	*/
	runWithTokenWait(ctx)
}

func runWithTokenWait(ctx context.Context) {
	configPath := config.DefaultPath

	for {
		select {
		case <-ctx.Done():
			telemetry.Infof("Shutting down...")
			return
		default:
			// Try to load config
			cfg, err := config.Load(configPath)
			if err != nil {
				log.Printf("Config load error: %v", err)
				time.Sleep(5 * time.Second)
				continue
			}

			// Check if token exists
			if cfg.TELEGRAM_TOKEN == "" {
				log.Println("⏳ Waiting for Telegram token...")
				log.Println("📝 Please add TELEGRAM_TOKEN to config.yml")
				log.Printf("📁 Config location: %s\n", configPath)

				// Watch for config changes
				if waitForToken(ctx, configPath) {
					continue // Retry loading
				}
				return // Context cancelled
			}

			// Token found! Start the bot
			telemetry.Infof("✅ Telegram token found, starting bot...")

			ctrl, err := telegram.NewController(cfg, configPath)
			if err != nil {
				log.Printf("❌ Controller init failed: %v", err)
				log.Println("⏳ Retrying in 10 seconds...")
				time.Sleep(10 * time.Second)
				continue
			}

			// Run the bot
			if err := ctrl.Start(ctx); err != nil {
				log.Printf("Controller error: %v", err)

				// Check if it's a token error
				if isTokenError(err) {
					log.Println("❌ Token appears invalid, please check and update config.yml")
					cfg.TELEGRAM_TOKEN = "" // Clear invalid token
					_ = config.Save(configPath, cfg)
					continue
				}

				// Other error, retry
				time.Sleep(5 * time.Second)
				continue
			}

			return // Normal exit
		}
	}
}

// waitForToken monitors config file for changes
func waitForToken(ctx context.Context, configPath string) bool {
	// Get initial file info
	initialInfo, err := os.Stat(configPath)
	if err != nil {
		// Config doesn't exist, create it
		cfg := config.Default()
		_ = config.Save(configPath, cfg)
		initialInfo, _ = os.Stat(configPath)
	}

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return false // Shutdown requested
		case <-ticker.C:
			// Check if file has been modified
			currentInfo, err := os.Stat(configPath)
			if err != nil {
				continue
			}

			// File was modified
			if currentInfo.ModTime().After(initialInfo.ModTime()) {
				telemetry.Infof("📝 Config file changed, checking for token...")
				return true // Try loading again
			}

			// Also check environment variable
			if os.Getenv("TELEGRAM_TOKEN") != "" {
				telemetry.Infof("📝 Token found in environment variable")
				return true
			}
		}
	}
}

// isTokenError checks if error is related to invalid token
func isTokenError(err error) bool {
	errStr := err.Error()
	return strings.Contains(errStr, "401") ||
		strings.Contains(errStr, "Unauthorized") ||
		strings.Contains(errStr, "Invalid token") ||
		strings.Contains(errStr, "telegram init")
}
