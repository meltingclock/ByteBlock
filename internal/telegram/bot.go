package telegram

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"

	"github.com/meltingclock/biteblock_v1/internal/config"
	v2 "github.com/meltingclock/biteblock_v1/internal/dex/v2"
	"github.com/meltingclock/biteblock_v1/internal/mempool"
	"github.com/meltingclock/biteblock_v1/internal/signals"
)

type NetPreset struct {
	WSSURL  string
	Factory common.Address
	Router  common.Address
	WETH    common.Address
	ChainID int64
}

// Fill the Base V2 factory/router with whatever DEX you target (Sushi V2/Pancake V2 on Base/etc.)
var netPresets = map[string]NetPreset{
	"ethereum": {
		WSSURL:  "ws://127.0.0.1:8545",
		Factory: common.HexToAddress("0x5C69bEe701ef814a2B6a3EDD4B1652CB9cc5aA6f"), // Uniswap V2
		Router:  common.HexToAddress("0x7a250d5630B4cF539739dF2C5dAcb4c659F2488D"), // Uniswap V2
		WETH:    common.HexToAddress("0xC02aaA39b223FE8D0A0e5C4F27eAD9083C756Cc2"),
		ChainID: 1,
	},
	"bsc": {
		WSSURL:  "wss://bsc-ws-node.nariox.org:443",
		Factory: common.HexToAddress("0xBCfCcbde45cE874adCB698cC183deBcF17952812"), // Pancake V2
		Router:  common.HexToAddress("0x10ED43C718714eb63d5aA57B78B54704E256024E"), // Pancake V2
		WETH:    common.HexToAddress("0xBB4CdB9CBd36B01bD1cBaEBF2De08d9173bc095c"), // WBNB
		ChainID: 56,
	},
	"base": {
		WSSURL:  "wss://base-mainnet.g.alchemy.com/v2/<KEY>",
		Factory: common.HexToAddress("0x<BASE_V2_FACTORY>"),
		Router:  common.HexToAddress("0x<BASE_V2_ROUTER>"),
		WETH:    common.HexToAddress("0x4200000000000000000000000000000000000006"),
		ChainID: 8453,
	},
}

type Controller struct {
	Bot  *tgbotapi.BotAPI
	Cfg  *config.Config
	Path string

	allowedChatID int64

	// mempool watcher lifecycle
	watcher  *mempool.Watcher
	cancelFn context.CancelFunc
	running  bool

	activeNet string // NEW: "ethereum" | "bsc" | "base"
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
		activeNet:     "ethereum", // default to ethereum; could be made configurable
	}, nil
}

func (c *Controller) reply(chatID int64, text string) {
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = "Markdown"
	_, _ = c.Bot.Send(msg)
}

func (c *Controller) startOnActivePreset(ctx context.Context, chatID int64) error {
	p, ok := netPresets[strings.ToLower(c.activeNet)]
	if !ok {
		return fmt.Errorf("unknown network preset: %s", c.activeNet)
	}

	wctx, cancel := context.WithCancel(ctx)
	c.cancelFn = cancel

	// RPC/ETH clients for analyzers
	rpcCl, err := rpc.DialContext(wctx, p.WSSURL)
	if err != nil {
		return fmt.Errorf("rpc dial: %w", err)
	}
	ethCl := ethclient.NewClient(rpcCl)

	// Build DEX registry from preset (authorative for factory/router/WETH)
	dex := v2.NewRegistry(v2.Config{
		Network: v2.Network(strings.ToLower(c.activeNet)),
		Factory: p.Factory,
		Router:  p.Router,
		WETH:    p.WETH,
	})

	// Canonical PairCreated subscription + pending addLiquidity analyzer
	pairReg := signals.NewPairRegistry(dex, 48*time.Hour)
	pairReg.Start(wctx, ethCl)
	liq := signals.NewLiquidityAnalyzer(ethCl, dex, pairReg)

	// Mempool watcher (your existing pending-tx card + new Liquidity card)
	c.watcher = mempool.NewWatcher(p.WSSURL, func(ctx context.Context, tx *types.Transaction) error {
		from, _ := mempool.SendTxVerifier(tx)
		to := "<contract-creation>"
		if tx.To() != nil {
			to = tx.To().Hex()
		}

		if tx.Value().Sign() > 0 {
			c.reply(chatID, fmt.Sprintf(
				"⛓ *Pending tx*\n`%s`\nfrom: `%s`\nto: `%s`\nnonce: %d\nvalue: %s wei",
				tx.Hash().Hex(), from.Hex(), to, tx.Nonce(), tx.Value().String(),
			))
		}

		// Liquidity detection vs preset router
		if tx.To() != nil && strings.EqualFold(tx.To().Hex(), dex.Router().Hex()) {
			if sig, _ := liq.AnalyzePending(ctx, tx); sig != nil {
				// 👇 Console log (pending/liquidity)
				log.Printf("[liquidity][pending] hash=%s kind=%s from=%s router=%s pair=%s t0=%s t1=%s nonce=%d val=%s",
					sig.Hash.Hex(),
					sig.Kind.String(),
					sig.From.Hex(),
					sig.Router.Hex(),
					sig.Pair.Hex(),
					sig.Token0.Hex(),
					sig.Token1.Hex(),
					tx.Nonce(),
					tx.Value().String(),
				)
				c.reply(chatID, fmt.Sprintf(
					"💧 *Liquidity detected*\n`%s`\nkind: `%s`\nfrom: `%s`\nrouter: `%s`\npair: `%s`\n"+
						"token0: `%s`\ntoken1: `%s`\nnonce: %d\nvalue: %s wei",
					sig.Hash.Hex(),
					sig.Kind.String(),
					sig.From.Hex(),
					sig.Router.Hex(),
					sig.Pair.Hex(),
					sig.Token0.Hex(),
					sig.Token1.Hex(),
					tx.Nonce(),
					tx.Value().String(),
				))
			}
		}
		return nil
	})

	if err := c.watcher.Start(wctx); err != nil {
		cancel()
		return err
	}
	c.running = True()
	c.reply(chatID, fmt.Sprintf("🟢 *Sniper started* on *%s*\nWSS: `%s`", c.activeNet, p.WSSURL))
	return nil
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
						"⚙️ *Network*\n"+
						"/net <ethereum|bsc|base> – Select network preset (applied on /start)\n\n"+
						"▶️ *Control*\n"+
						"/start – Start mempool watcher\n"+
						"/stop – Stop mempool watcher\n\n"+
						"ℹ️ *Info*\n"+
						"/status – Show current state & preset\n"+
						"/show_config – Show non-secret config\n"+
						"/whoami – Show your Telegram chat ID\n"+
						"/set_chat <id> – restrict bot to a specific chat ID\n")
			case strings.HasPrefix(text, "/net "):
				arg := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(text, "/net")))
				if _, ok := netPresets[arg]; !ok {
					c.reply(chatID, "❌ Unknown network. Use: ethereum, bsc, base")
					break
				}
				if strings.EqualFold(c.activeNet, arg) {
					c.reply(chatID, "ℹ️ Already selected: *"+arg+"*")
					break
				}
				c.activeNet = arg
				c.reply(chatID, "✅ Selected network: *"+arg+"*.\nSend /start to apply.")
			case strings.HasPrefix(text, "/start"):
				if c.running {
					c.reply(chatID, "ℹ️ Already running.")
					break
				}
				if err := c.startOnActivePreset(ctx, chatID); err != nil {
					c.reply(chatID, "❌ start error: "+err.Error())
				}
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
				p, ok := netPresets[strings.ToLower(c.activeNet)]
				if !ok {
					c.reply(chatID, fmt.Sprintf("State: *%s*\nActive net: *%s* (unknown preset)", state, c.activeNet))
					break
				}
				c.reply(chatID, fmt.Sprintf(
					"State: *%s*\nActive net: *%s*\nWSS: `%s`\nFactory: `%s`\nRouter: `%s`\nWETH: `%s`",
					state, c.activeNet, p.WSSURL, p.Factory.Hex(), p.Router.Hex(), p.WETH.Hex(),
				))
			case strings.HasPrefix(text, "/show_config"):
				redactedPK := "not set"
				if c.Cfg.PRIVATE_KEY != "" && len(c.Cfg.PRIVATE_KEY) >= 10 {
					redactedPK = c.Cfg.PRIVATE_KEY[:6] + "…" + c.Cfg.PRIVATE_KEY[len(c.Cfg.PRIVATE_KEY)-4:]
				} else if c.Cfg.PRIVATE_KEY != "" {
					redactedPK = "set"
				}
				idStatus := "not set"
				if c.Cfg.IDENTITY_KEY != "" {
					idStatus = "set"
				}

				c.reply(chatID, fmt.Sprintf(
					"*Config*\nActive net: *%s*\nBOT_ADDRESS: `%s`\nPRIVATE_KEY: `%s`\nIDENTITY_KEY: %s\nCHAT_ID: `%d`",
					c.activeNet, c.Cfg.BOT_ADDRESS, redactedPK, idStatus, c.Cfg.TELEGRAM_CHAT_ID,
				))
			case strings.HasPrefix(text, "/whoami"):
				c.reply(chatID, fmt.Sprintf("Your chat ID: `%d`", chatID))
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
			default:
				// ignore non-commands to reduce noise
			}
		}
	}
}

// helper so we can flip to true with a function (avoids shadow warning)
func True() bool { return true }
