package telegram

import (
	"context"
	"fmt"
	"strings"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"

	"github.com/meltingclock/biteblock_v1/internal/config"
	"github.com/meltingclock/biteblock_v1/internal/mempool"
)

type Controller struct {
	Bot  *tgbotapi.BotAPI
	Cfg  *config.Config
	Path string

	allowedChatID int64

	// mempool watcher lifecycle
	watcher  *mempool.Watcher
	cancelFn context.CancelFunc
	running  bool
}

func NewController(cfg *config.Config, path string) (*Controller, error) {
	if cfg.TELEGRAM_TOKEN == "" {
		return nil, fmt.Errorf("TELEGRAM_TOKEN is empty")
	}
	bot, err := tgbotapi.NewBotAPI(cfg.TELEGRAM_TOKEN)
	if err != nil {
		return nil, fmt.Errorf("telegram init: %w", err)
	}
	return &Controller{
		Bot:           bot,
		Cfg:           cfg,
		Path:          path,
		allowedChatID: cfg.TELEGRAM_CHAT_ID,
	}, nil
}

func (c *Controller) reply(chatID int64, text string) {
	msg := tgbotapi.NewMessage(chatID, text)
	//msg.ParseMode = "MarkdownV2"
	_, _ = c.Bot.Send(msg)
}

func (c *Controller) Start(ctx context.Context) error {
	u := tgbotapi.NewUpdate(0)
	u.Timeout = 30
	updates := c.Bot.GetUpdatesChan(u)

	c.reply(c.allowedChatID, "🤖 *Biteblock* ready. Use /help for commands.")

	for {
		select {
		case <-ctx.Done():
			return nil
		case update := <-updates:
			if update.Message == nil {
				continue
			}
			chatID := update.Message.Chat.ID
			// allow only configured chat
			if c.allowedChatID != 0 && chatID != c.allowedChatID {
				continue
			}
			text := strings.TrimSpace(update.Message.Text)
			switch {
			case strings.HasPrefix(text, "/help"), strings.HasPrefix(text, "/commands"):
				c.reply(chatID,
					"*Available Commands:*\n\n"+
						"⚙️ *Configuration*\n"+
						"/set_chat <id> – Restrict bot to a specific chat ID\n"+
						"/set_wss <wss-url> – Set Ethereum WebSocket endpoint\n"+
						"/set_addr <0x...> – Set bot address\n"+
						"/set_pk <hex> – Set private key (saved in config.yml)\n\n"+
						"▶️ *Control*\n"+
						"/start – Start mempool watcher\n"+
						"/stop – Stop mempool watcher\n\n"+
						"ℹ️ *Info*\n"+
						"/whoami – Show your Telegram chat ID\n"+
						"/status – Show current state (running/stopped)\n"+
						"/show_config – Show current config values\n"+
						"/help – Show this help menu")

			case strings.HasPrefix(text, "/help"):
				c.reply(chatID, "*Commands*\n/set_wss <wss-url>\n/set_chat <id>\n/set_pk <hex>\n/set_addr <0x...>\n/start\n/stop\n/status\n/show_config")
			case strings.HasPrefix(text, "/set_wss "):
				arg := strings.TrimSpace(strings.TrimPrefix(text, "/set_wss"))
				arg = strings.TrimSpace(arg)
				if arg == "" || (!strings.HasPrefix(arg, "ws://") && !strings.HasPrefix(arg, "wss://")) {
					c.reply(chatID, "❌ Provide a valid *ws://* or *wss://* URL")
					continue
				}
				c.Cfg.WSS_URL = arg
				_ = config.Save(c.Path, c.Cfg)
				c.reply(chatID, "✅ WSS_URL updated.")
			case strings.HasPrefix(text, "/set_chat "):
				arg := strings.TrimSpace(strings.TrimPrefix(text, "/set_chat"))
				var id int64
				fmt.Sscan(arg, &id)
				if id == 0 {
					c.reply(chatID, "❌ Provide a valid numeric chat ID")
					continue
				}
				c.Cfg.TELEGRAM_CHAT_ID = id
				c.allowedChatID = id
				_ = config.Save(c.Path, c.Cfg)
				c.reply(chatID, fmt.Sprintf("✅ Allowed chat set to %d", id))
			case strings.HasPrefix(text, "/set_pk "):
				arg := strings.TrimSpace(strings.TrimPrefix(text, "/set_pk"))
				if !strings.HasPrefix(arg, "0x") && len(arg) < 64 {
					c.reply(chatID, "❌ Provide a hex private key")
					continue
				}
				c.Cfg.PRIVATE_KEY = arg
				_ = config.Save(c.Path, c.Cfg)
				c.reply(chatID, "✅ Private key saved (hidden).")
			case strings.HasPrefix(text, "/set_addr "):
				arg := strings.TrimSpace(strings.TrimPrefix(text, "/set_addr"))
				if !common.IsHexAddress(arg) {
					c.reply(chatID, "❌ Provide a valid *0x* address")
					continue
				}
				c.Cfg.BOT_ADDRESS = common.HexToAddress(arg).Hex()
				_ = config.Save(c.Path, c.Cfg)
				c.reply(chatID, "✅ Address saved.")
			case strings.HasPrefix(text, "/start"):
				if c.running {
					c.reply(chatID, "ℹ️ Already running.")
					continue
				}
				if c.Cfg.WSS_URL == "" {
					c.reply(chatID, "❌ Set *WSS_URL* first with /set_wss <url>")
					continue
				}
				// create context for watcher
				wctx, cancel := context.WithCancel(ctx)
				c.cancelFn = cancel
				c.watcher = mempool.NewWatcher(c.Cfg.WSS_URL, func(ctx context.Context, tx *types.Transaction) error {
					from, _ := mempool.SendTxVerifier(tx)
					to := "<contract-creation>"
					if tx.To() != nil {
						to = tx.To().Hex()
					}
					// lightweight throttled log to chat
					if tx.Value().Sign() > 0 {
						c.reply(chatID, fmt.Sprintf("⛓ *Pending tx*\n`%s`\nfrom: `%s`\nto: `%s`\nnonce: %d\nvalue: %s wei",
							tx.Hash().Hex(), from.Hex(), to, tx.Nonce(), tx.Value().String()))
					}
					return nil
				})
				if err := c.watcher.Start(wctx); err != nil {
					c.reply(chatID, "❌ start error: "+err.Error())
					cancel()
					continue
				}
				c.running = True()
				c.reply(chatID, "🟢 *Sniper started*. Listening for pending tx...")
			case strings.HasPrefix(text, "/stop"):
				if !c.running {
					c.reply(chatID, "ℹ️ Not running.")
					continue
				}
				c.cancelFn()
				go func() {
					c.watcher.Wait()
					c.running = false
					c.reply(chatID, "🔴 Stopped.")
				}()
			case strings.HasPrefix(text, "/status"):
				state := "stopped"
				if c.running {
					state = "running"
				}
				c.reply(chatID, fmt.Sprintf("State: *%s*\nWSS_URL: `%s`", state, c.Cfg.WSS_URL))
			case strings.HasPrefix(text, "/show_config"):
				redactedPK := ""
				if c.Cfg.PRIVATE_KEY != "" {
					redactedPK = c.Cfg.PRIVATE_KEY[:6] + "…" + c.Cfg.PRIVATE_KEY[len(c.Cfg.PRIVATE_KEY)-4:]
				}
				c.reply(chatID, fmt.Sprintf(
					"*Config*\nWSS_URL: `%s`\nBOT_ADDRESS: `%s`\nPRIVATE_KEY: `%s`\nCHAT_ID: `%d`",
					c.Cfg.WSS_URL, c.Cfg.BOT_ADDRESS, redactedPK, c.Cfg.TELEGRAM_CHAT_ID,
				))
			default:
				// ignore non-commands to reduce noise
			}
		}
	}
}

// helper so we can flip to true with a function (avoids shadow warning)
func True() bool { return true }
