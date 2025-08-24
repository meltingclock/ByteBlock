package telegram

import (
	"context"
	"fmt"
	"math/big"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"

	"github.com/meltingclock/biteblock_v1/internal/config"
	v2 "github.com/meltingclock/biteblock_v1/internal/dex/v2"
	"github.com/meltingclock/biteblock_v1/internal/mempool"
	"github.com/meltingclock/biteblock_v1/internal/scanner"
	"github.com/meltingclock/biteblock_v1/internal/signals"
	"github.com/meltingclock/biteblock_v1/internal/telemetry"
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
	// start the MintWatcher
	startMintWatcher(wctx, ethCl, pairReg)
	liq := signals.NewLiquidityAnalyzer(ethCl, dex, pairReg)

	scan := scanner.New(ethCl, dex, scanner.Config{
		RequireWethPair:   true,
		MinEthLiquidity:   wei("0.3"),                // start conservative; tune later
		MinTokenLiquidity: nil,                       // set when we want non-ETH pairs enforced
		AllowCreators:     map[common.Address]bool{}, // empty = allow all
		DenyCreators:      map[common.Address]bool{}, // can be filled later via telegram
		Deadline:          250 * time.Millisecond,
	})

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
		if tx.To() != nil && *tx.To() == dex.Router() {
			if meta, ok := dex.LookupSelectorFromData(tx.Data()); !ok {
				// Unknown function on router - skip fast
				return nil
			} else {
				// log meta.Name/meta.Kind for visibility
				telemetry.Debugf("[router] %s -> %s", tx.Hash(), meta.Name)
			}
			if sig, _ := liq.AnalyzePending(ctx, tx); sig != nil {
				// ⬇️ fast safety scan first
				rep, err := scan.Run(ctx, sig)
				if err != nil {
					telemetry.Warnf("[scanner][error]: %v", err)
					return nil
				}
				if !rep.Pass {
					telemetry.Debugf("[scan][reject] hash=%s reasons=%v fn=%s ethWei=%v a=%v b=%v",
						sig.Hash.Hex(), rep.Reasons, rep.Function, rep.ETHInWei, rep.AmountA, rep.AmountB,
					)
					return nil
				}
				telemetry.Debugf("[scan][pass] hash=%s fn=%s ethWei=%v a=%v b=%v",
					sig.Hash.Hex(), rep.Function, rep.ETHInWei, rep.AmountA, rep.AmountB,
				)

				// 👇 Console log (pending/liquidity)
				telemetry.Debugf("[liquidity][pending] hash=%s kind=%s from=%s router=%s pair=%s t0=%s t1=%s nonce=%d val=%s",
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

				// NEW: run fast safety scan
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
				// --- Receipt correlator (background; non-blocking) ---
				go func(h common.Hash, pair common.Address) {
					ctx2, cancel := context.WithTimeout(context.Background(), 60*time.Second)
					defer cancel()

					rcpt, err := ethCl.TransactionReceipt(ctx2, h)
					if err != nil {
						return // receipt not ready or RPC hiccup
					}
					if evt, ok := signals.FindMintInReceipt(rcpt, pair); ok {
						telemetry.Debugf("[liquidity][mined/receipt] pair=%s sender=%s amount0=%s amount1=%s block=%d",
							pair.Hex(), evt.Sender.Hex(), evt.Amount0.String(), evt.Amount1.String(), rcpt.BlockNumber)
						c.reply(chatID, fmt.Sprintf(
							"✅ *Liquidity mined*\npair: `%s`\nsender: `%s`\namount0: `%s`\namount1: `%s`\nblock: `%d`",
							pair.Hex(), evt.Sender.Hex(), evt.Amount0.String(), evt.Amount1.String(), rcpt.BlockNumber,
						))
					}
				}(sig.Hash, sig.Pair)
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

// Watches Mint(sender, amount0, amount1) on all known pairs to confirm mined liquidity.
func startMintWatcher(ctx context.Context, ec *ethclient.Client, pr *signals.PairRegistry) {
	const pairABI = `[
	  {"anonymous":false,"inputs":[
	    {"indexed":true,"name":"sender","type":"address"},
	    {"indexed":false,"name":"amount0","type":"uint256"},
	    {"indexed":false,"name":"amount1","type":"uint256"}],
	   "name":"Mint","type":"event"}]`

	ab, _ := abi.JSON(strings.NewReader(pairABI))
	mintEvt := ab.Events["Mint"]
	mintTopic := mintEvt.ID

	go func() {
		var (
			sub     ethereum.Subscription
			logsCh  chan types.Log
			lastN   int
			backoff = 500 * time.Millisecond
		)
		for {
			if ctx.Err() != nil {
				if sub != nil {
					sub.Unsubscribe()
				}
				return
			}

			pairs := pr.Pairs()
			if len(pairs) == 0 {
				time.Sleep(1 * time.Second)
				continue
			}

			// Resubscribe only when the set size grows (cheap heuristic).
			if sub != nil && len(pairs) == lastN {
				time.Sleep(3 * time.Second)
				continue
			}
			if sub != nil {
				sub.Unsubscribe()
			}

			q := ethereum.FilterQuery{
				Addresses: pairs,
				Topics:    [][]common.Hash{{mintTopic}},
			}
			logsCh = make(chan types.Log, 1024)
			s, err := ec.SubscribeFilterLogs(ctx, q, logsCh)
			if err != nil {
				telemetry.Warnf("[signals] Mint subscribe error: %v", err)
				time.Sleep(backoff)
				if backoff < 8*time.Second {
					backoff *= 2
				}
				continue
			}
			sub = s
			lastN = len(pairs)
			backoff = 500 * time.Millisecond
			telemetry.Infof("[signals] Mint watcher subscribed to %d pairs", lastN)

			ticker := time.NewTicker(1 * time.Second)
			defer ticker.Stop()

			for {
				select {
				case <-ctx.Done():
					sub.Unsubscribe()
					return
				case err := <-sub.Err():
					telemetry.Warnf("[signals] Mint sub err: %v", err)
					// break to outer loop and resubscribe
					goto RESUB
				case lg := <-logsCh:
					// topic[1] = sender; data = amount0, amount1
					sender := common.BytesToAddress(lg.Topics[1].Bytes()[12:])
					vals, err := mintEvt.Inputs.NonIndexed().Unpack(lg.Data)
					if err != nil || len(vals) < 2 {
						telemetry.Debugf("[signals] Mint unpack err: %v", err)
						continue
					}
					amount0 := vals[0].(*big.Int)
					amount1 := vals[1].(*big.Int)
					if false {
						telemetry.Infof("[liquidity][mined] pair=%s sender=%s amount0=%s amount1=%s block=%d",
							lg.Address.Hex(), sender.Hex(), amount0.String(), amount1.String(), lg.BlockNumber)
					}
				case <-ticker.C:
					// If the number of pairs grewm resubscribe to include new addresses
					if pr.Size() != lastN {
						sub.Unsubscribe()
						goto RESUB
					}
				}
			}
		RESUB:
			// loop and resubscribe with (bounded) backoff
			if backoff < 8*time.Second {
				backoff *= 2
			}
			time.Sleep(backoff)
		}
	}()
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
						"/debug on|off – enable/disable debug logs\n"+
						"/trace on|off – enable/disable very noisy logs\n"+
						"/tail [n] – show last n log lines (default 50)\n"+
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
			case strings.HasPrefix(text, "/debug "):
				arg := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(text, "/debug")))
				on := arg == "on" || arg == "1" || arg == "true"
				telemetry.EnableDebug(on)
				c.reply(chatID, fmt.Sprintf("✅ debug: %v", on))
			case strings.HasPrefix(text, "/trace "):
				arg := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(text, "/trace")))
				on := arg == "on" || arg == "1" || arg == "true"
				telemetry.EnableTrace(on)
				c.reply(chatID, fmt.Sprintf("✅ trace: %v", on))
			case strings.HasPrefix(text, "/tail "):
				n := 50
				parts := strings.Fields(text)
				if len(parts) > 1 {
					fmt.Sscan(parts[1], &n)
					if n <= 0 {
						n = 50
					}
					if n > 500 {
						n = 500
					} // avoid flooding telegram
				}
				lines := telemetry.Tail(n)
				if len(lines) == 0 {
					c.reply(chatID, "ℹ️ log buffer empty")
					break
				}
				// Telegram messages max ~4096 chars; chunk if needed
				var buf strings.Builder
				for _, ln := range lines {
					if buf.Len()+len(ln)+1 > 3500 { // conservative
						c.reply(chatID, "```\n"+buf.String()+"\n```")
						buf.Reset()
					}
					buf.WriteString(ln)
					buf.WriteByte('\n')
				}
				if buf.Len() > 0 {
					c.reply(chatID, "```\n"+buf.String()+"\n```")
				}
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

func wei(s string) *big.Int {
	// naive decimal parser for small constants; or just use big.NewInt for hardcoded values
	f, ok := new(big.Float).SetString(s)
	if !ok {
		return big.NewInt(0)
	}
	ethWei := new(big.Float).Mul(f, big.NewFloat(1e18))
	out := new(big.Int)
	ethWei.Int(out)
	return out
}
