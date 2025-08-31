package main

import (
	"context"
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

	runWithTokenWait(ctx)

	telemetry.Infof("Main exiting gracefully..")
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
				telemetry.Errorf("Config load error: %v", err) // Changed to telemetry
				time.Sleep(5 * time.Second)
				continue
			}

			// Check if token exists
			if cfg.TELEGRAM_TOKEN == "" {
				telemetry.Infof("⏳ Waiting for Telegram token...") // Changed to telemetry
				telemetry.Infof("📝 Please add TELEGRAM_TOKEN to config.yml")
				telemetry.Infof("📁 Config location: %s", configPath)

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
				telemetry.Errorf("❌ Controller init failed: %v", err) // Changed to telemetry
				telemetry.Infof("⏳ Retrying in 10 seconds...")
				time.Sleep(10 * time.Second)
				continue
			}

			// Run the bot - THIS IS THE KEY CHANGE
			// ctrl.Start() should block until the bot stops
			telemetry.Infof("Starting controller...")
			if err := ctrl.Start(ctx); err != nil {
				telemetry.Errorf("Controller error: %v", err)

				// Check if it's a token error
				if isTokenError(err) {
					telemetry.Errorf("❌ Token appears invalid, please check and update config.yml")
					cfg.TELEGRAM_TOKEN = "" // Clear invalid token
					_ = config.Save(configPath, cfg)
					continue
				}

				// Other error, retry
				time.Sleep(5 * time.Second)
				continue
			}

			// DON'T RETURN HERE! The bot stopped, but we might want to restart
			telemetry.Infof("Controller stopped, checking if we should restart...")

			// Check if context is done (user pressed Ctrl+C)
			select {
			case <-ctx.Done():
				return // Exit cleanly
			default:
				// Controller stopped for other reason, maybe restart?
				telemetry.Warnf("Controller stopped unexpectedly, restarting in 5 seconds...")
				time.Sleep(5 * time.Second)
				continue // Loop back to try again
			}
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
