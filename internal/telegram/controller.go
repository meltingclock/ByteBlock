package telegram

import (
	"context"
	"crypto/ecdsa"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
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

	// NEW: execution related
	ethClient   *ethclient.Client
	dex         *v2.Registry
	privateKey  *ecdsa.PrivateKey
	walletAddr  common.Address
	routerABI   abi.ABI
	erc20ABI    abi.ABI
	positions   map[common.Address]*Position // Track open positions
	positionsMu sync.RWMutex

	// NEW: Safety settings (runtime configurable)
	honeypotCheckEnabled bool
	honeypotCheckMode    string                                  // "always", "smart", "never"
	trustedTokens        map[common.Address]bool                 // Skip check for these
	trustedDeployers     map[common.Address]bool                 // Skip check for tokens from these addresses
	checkCache           map[common.Address]*scanner.TokenSafety // Cache results
	//cacheMu              sync.RWMutex

	activeNet string // NEW: "ethereum" | "bsc" | "base"

}

type Position struct {
	Token       common.Address
	TokenAmount *big.Int
	EthSpent    *big.Int
	EntryPrice  *big.Int
	EntryBlock  uint64
	EntryTime   time.Time
	TxHash      common.Hash
}

func NewController(cfg *config.Config, path string) (*Controller, error) {
	if cfg.TELEGRAM_TOKEN == "" {
		return nil, fmt.Errorf("TELEGRAM_TOKEN is empty")
	}
	bot, err := tgbotapi.NewBotAPI(cfg.TELEGRAM_TOKEN)
	if err != nil {
		return nil, fmt.Errorf("telegram init: %w", err)
	}
	ctrl := &Controller{
		Bot:                  bot,
		Cfg:                  cfg,
		Path:                 path,
		allowedChatID:        cfg.TELEGRAM_CHAT_ID,
		activeNet:            "ethereum", // default to ethereum; could be made configurable
		honeypotCheckEnabled: cfg.HONEYPOT_CHECK_ENABLED,
		honeypotCheckMode:    cfg.HONEYPOT_CHECK_MODE,
		trustedTokens:        make(map[common.Address]bool),
		trustedDeployers:     make(map[common.Address]bool),
		checkCache:           make(map[common.Address]*scanner.TokenSafety),
		positions:            make(map[common.Address]*Position),
	}

	// Parse trusted tokens from config
	for _, addr := range cfg.TRUSTED_TOKENS {
		if common.IsHexAddress(addr) {
			ctrl.trustedTokens[common.HexToAddress(addr)] = true
			telemetry.Debugf("[config] loaded trusted token: %s", addr)
		}
	}

	// Parse trusted deployers from config
	for _, addr := range cfg.TRUSTED_DEPLOYERS {
		if common.IsHexAddress(addr) {
			ctrl.trustedDeployers[common.HexToAddress(addr)] = true
			telemetry.Debugf("[config] loaded trusted deployer: %s", addr)
		}
	}

	telemetry.Infof("[config] Safety mode: %s (enabled: %v)",
		ctrl.honeypotCheckMode, ctrl.honeypotCheckEnabled)

	return ctrl, nil
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
	c.ethClient = ethCl

	// Build DEX registry from preset (authorative for factory/router/WETH)
	dex := v2.NewRegistry(v2.Config{
		Network: v2.Network(strings.ToLower(c.activeNet)),
		Factory: p.Factory,
		Router:  p.Router,
		WETH:    p.WETH,
	})

	// Store dex registry
	c.dex = dex

	// Initialize router ABI
	routerABI, err := abi.JSON(strings.NewReader(v2.RouterABI))
	if err != nil {
		return fmt.Errorf("router ABI parse: %w", err)
	}
	c.routerABI = routerABI

	// Initialize ERC20 ABI (minimal)
	erc20ABIJson := `[
		{"constant":true,"inputs":[{"name":"_owner","type":"address"}],"name":"balanceOf","outputs":[{"name":"","type":"uint256"}],"type":"function"},
		{"constant":false,"inputs":[{"name":"_spender","type":"address"},{"name":"_value","type":"uint256"}],"name":"approve","outputs":[{"name":"","type":"bool"}],"type":"function"},
		{"constant":true,"inputs":[{"name":"_owner","type":"address"},{"name":"_spender","type":"address"}],"name":"allowance","outputs":[{"name":"","type":"uint256"}],"type":"function"},
		{"constant":false,"inputs":[{"name":"_to","type":"address"},{"name":"_value","type":"uint256"}],"name":"transfer","outputs":[{"name":"","type":"bool"}],"type":"function"}
	]`
	erc20ABI, _ := abi.JSON(strings.NewReader(erc20ABIJson))
	c.erc20ABI = erc20ABI

	// Load private key if configured
	if c.Cfg.PRIVATE_KEY != "" {
		privateKey, walletAddr, err := loadPrivateKey(c.Cfg.PRIVATE_KEY)
		if err != nil {
			c.reply(chatID, fmt.Sprintf("⚠️ Private key error: %v\nBot will run in watch-only mode", err))

		} else {
			c.privateKey = privateKey
			c.walletAddr = walletAddr

			// Check wallet balance
			balance, err := c.getETHBalance()
			if err != nil {
				c.reply(chatID, fmt.Sprintf("⚠️ Could not fetch wallet balance for `%s`: %v",
					walletAddr.Hex(), err))
			} else {
				c.reply(chatID, fmt.Sprintf("💰 Wallet: `%s`\nBalance: %s ETH",
					walletAddr.Hex(), formatEth(balance)))
			}
		}
	} else {
		c.reply(chatID, "⚠️ No private key configured. Running in watch-only mode.")
	}

	// Initialize positions map
	if c.positions == nil {
		c.positions = make(map[common.Address]*Position)
	}

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
						"🤖 *Execution*\n"+
						"/buy - Buy a token: \"buy <token_address> <eth_amount>\"\n"+
						"/position - List open positions\n"+
						"/sell - Sell a token: \"sell <token_address> <eth_amount>\"\n"+
						"/balance - Show wallet balance\n\n"+
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
			case strings.HasPrefix(text, "/buy"):
				if c.privateKey == nil {
					c.reply(chatID, "❌ No private key configured. Cannot execute trades.")
					break
				}

				parts := strings.Fields(text)
				if len(parts) < 3 {
					c.reply(chatID, "📝 Usage: /buy <token_address> <eth_amount>\nExample: /buy 0x... 0.1")
					break
				}

				tokenStr := parts[1]
				if !common.IsHexAddress(tokenStr) {
					c.reply(chatID, "❌ Invalid token address")
					break
				}
				token := common.HexToAddress(tokenStr)

				ethAmount, err := parseEthAmount(parts[2])
				if err != nil {
					c.reply(chatID, fmt.Sprintf("❌ Invalid amount: %v", err))
					break
				}

				// Check wallet balance
				balance, err := c.getETHBalance()
				if err != nil {
					c.reply(chatID, "❌ Could not check wallet balance")
					break
				}

				if balance.Cmp(ethAmount) < 0 {
					c.reply(chatID, fmt.Sprintf("❌ Insufficient balance\nNeeded: %s ETH\nHave: %s ETH",
						formatEth(ethAmount), formatEth(balance)))
					break
				}

				// ============ HONEYPOT CHECK ============
				c.reply(chatID, "🔍 Running safety analysis...")

				checker := scanner.NewHoneypotChecker(c.ethClient, c.dex)
				safety, err := checker.CheckToken(context.Background(), token)
				if err != nil {
					c.reply(chatID, fmt.Sprintf("⚠️ Safety check error: %v\nProceed with caution!", err))
					// Don't block trade, just warn
				} else {
					// Display safety report
					safetyEmoji := "🟢"
					recommendation := "SAFE TO TRADE"

					if safety.IsHoneypot {
						safetyEmoji = "🔴"
						recommendation = "DO NOT BUY - HONEYPOT!"
					} else if safety.SafetyScore < 40 {
						safetyEmoji = "🔴"
						recommendation = "HIGH RISK - NOT RECOMMENDED"
					} else if safety.SafetyScore < 70 {
						safetyEmoji = "🟡"
						recommendation = "MODERATE RISK - BE CAREFUL"
					}

					// Build safety report
					report := fmt.Sprintf(
						"%s *Safety Score: %d/100*\n"+
							"*%s*\n\n"+
							"*Token Info:*\n"+
							"Name: %s\n"+
							"Symbol: %s\n\n"+
							"*Trade Simulation:*\n"+
							"✅ Can Buy: %v\n"+
							"✅ Can Approve: %v\n"+
							"✅ Can Sell: %v\n\n"+
							"*Tax Analysis:*\n"+
							"Buy Tax: %.1f%%\n"+
							"Sell Tax: %.1f%%\n\n"+
							"*Contract Analysis:*\n"+
							"Owner: %v (Renounced: %v)\n"+
							"Has Mint: %v\n"+
							"Has Pause: %v\n"+
							"Has Blacklist: %v\n"+
							"Max Wallet: %v\n\n"+
							"*Liquidity:*\n"+
							"ETH in Pool: %s\n",
						safetyEmoji, safety.SafetyScore,
						recommendation,
						safety.Name, safety.Symbol,
						safety.CanBuy, safety.CanApprove, safety.CanSell,
						safety.BuyTax, safety.SellTax,
						safety.HasOwner, safety.IsRenounced,
						safety.HasMintFunction,
						safety.HasPauseFunction,
						safety.HasBlacklist,
						safety.MaxWalletLimit,
						formatEth(safety.LiquidityETH),
					)

					// Add risk factors if any
					if len(safety.RiskFactors) > 0 {
						report += "\n*Risk Factors:*\n"
						for _, risk := range safety.RiskFactors {
							report += fmt.Sprintf("⚠️ %s\n", risk)
						}
					}

					// Add simulation error if any
					if safety.SimulationError != "" {
						report += fmt.Sprintf("\n*Simulation Error:*\n%s\n", safety.SimulationError)
					}

					c.reply(chatID, report)

					// Block if honeypot detected
					if safety.IsHoneypot {
						c.reply(chatID, "🚨 *TRANSACTION BLOCKED*\nHoneypot detected! This token cannot be sold.")
						break
					}

					// Warn if risky
					if safety.SafetyScore < 40 {
						c.reply(chatID, "⚠️ *HIGH RISK TOKEN*\nUse /forcebuy to proceed anyway (not recommended)")
						break
					}

					if safety.SafetyScore < 70 {
						c.reply(chatID, "⚠️ *MODERATE RISK*\nProceed with caution. Reply 'yes' to continue.")
						// You could implement a confirmation flow here
					}
				}

				// Execute buy if safe
				c.reply(chatID, fmt.Sprintf("🔄 Buying with %s ETH...", formatEth(ethAmount)))

				txHash, err := c.executeBuy(token, ethAmount)
				if err != nil {
					c.reply(chatID, fmt.Sprintf("❌ Buy failed: %v", err))
					break
				}

				// Save position
				c.positionsMu.Lock()
				c.positions[token] = &Position{
					Token:     token,
					EthSpent:  ethAmount,
					EntryTime: time.Now(),
					TxHash:    txHash,
				}
				c.positionsMu.Unlock()

				c.reply(chatID, fmt.Sprintf(
					"✅ *Buy Executed!*\n"+
						"Token: `%s`\n"+
						"Amount: %s ETH\n"+
						"Tx: `%s`",
					token.Hex(), formatEth(ethAmount), txHash.Hex()))
			case strings.HasPrefix(text, "/forcebuy"):
				if c.privateKey == nil {
					c.reply(chatID, "❌ No private key configured. Cannot execute trades.")
					break
				}

				parts := strings.Fields(text)
				if len(parts) < 3 {
					c.reply(chatID, "📝 Usage: /buy <token_address> <eth_amount>\nExample: /buy 0x... 0.1")
					break
				}

				tokenStr := parts[1]
				if !common.IsHexAddress(tokenStr) {
					c.reply(chatID, "❌ Invalid token address")
					break
				}
				token := common.HexToAddress(tokenStr)

				ethAmount, err := parseEthAmount(parts[2])
				if err != nil {
					c.reply(chatID, fmt.Sprintf("❌ Invalid amount: %v", err))
					break
				}

				// Check wallet balance
				balance, err := c.getETHBalance()
				if err != nil {
					c.reply(chatID, "❌ Could not check wallet balance")
					break
				}

				if balance.Cmp(ethAmount) < 0 {
					c.reply(chatID, fmt.Sprintf("❌ Insufficient balance\nNeeded: %s ETH\nHave: %s ETH",
						formatEth(ethAmount), formatEth(balance)))
					break
				}

				c.reply(chatID, "⚠️ *WARNING: Bypassing all safety checks!*")

				c.reply(chatID, fmt.Sprintf("🔄 Buying with %s ETH...", formatEth(ethAmount)))

				txHash, err := c.executeBuy(token, ethAmount)
				if err != nil {
					c.reply(chatID, fmt.Sprintf("❌ Buy failed: %v", err))
					break
				}

				// Save position
				c.positionsMu.Lock()
				c.positions[token] = &Position{
					Token:     token,
					EthSpent:  ethAmount,
					EntryTime: time.Now(),
					TxHash:    txHash,
				}
				c.positionsMu.Unlock()

				c.reply(chatID, fmt.Sprintf(
					"✅ *Buy Executed!*\n"+
						"Token: `%s`\n"+
						"Amount: %s ETH\n"+
						"Tx: `%s`",
					token.Hex(), formatEth(ethAmount), txHash.Hex()))
			case strings.HasPrefix(text, "/check"):
				parts := strings.Fields(text)
				if len(parts) < 2 {
					c.reply(chatID, "📝 Usage: /check <token_address>")
					break
				}

				tokenStr := parts[1]
				if !common.IsHexAddress(tokenStr) {
					c.reply(chatID, "❌ Invalid token address")
					break
				}
				token := common.HexToAddress(tokenStr)

				c.reply(chatID, "🔍 Analyzing token safety...")

				checker := scanner.NewHoneypotChecker(c.ethClient, c.dex)
				safety, err := checker.CheckToken(context.Background(), token)
				if err != nil {
					c.reply(chatID, fmt.Sprintf("❌ Analysis failed: %v", err))
					break
				}

				// Generate quick report
				verdict := "✅ SAFE"
				if safety.IsHoneypot {
					verdict = "🔴 HONEYPOT"
				} else if safety.SafetyScore < 40 {
					verdict = "⚠️ HIGH RISK"
				} else if safety.SafetyScore < 70 {
					verdict = "🟡 MODERATE RISK"
				}

				quickReport := fmt.Sprintf(
					"*Token Safety Report*\n\n"+
						"Token: `%s`\n"+
						"Name: %s\n"+
						"Symbol: %s\n\n"+
						"*Verdict: %s*\n"+
						"Safety Score: %d/100\n\n"+
						"Can Sell: %v\n"+
						"Total Tax: %.1f%%\n"+
						"Liquidity: %s ETH\n",
					token.Hex()[:10]+"..."+token.Hex()[36:],
					safety.Name, safety.Symbol,
					verdict,
					safety.SafetyScore,
					safety.CanSell,
					safety.BuyTax+safety.SellTax,
					formatEth(safety.LiquidityETH),
				)

				if len(safety.RiskFactors) > 0 && len(safety.RiskFactors) <= 5 {
					quickReport += "\n*Main Risks:*\n"
					for _, risk := range safety.RiskFactors {
						quickReport += fmt.Sprintf("• %s\n", risk)
					}
				}

				c.reply(chatID, quickReport)
			case strings.HasPrefix(text, "/sell "):
				if c.privateKey == nil {
					c.reply(chatID, "❌ No private key configured. Cannot execute trades.")
					break
				}

				parts := strings.Fields(text)
				if len(parts) < 3 {
					c.reply(chatID, "📝 Usage: /sell <token_address> <percentage>\nExample: /sell 0x... 50")
					break
				}

				tokenStr := parts[1]
				if !common.IsHexAddress(tokenStr) {
					c.reply(chatID, "❌ Invalid token address")
					break
				}
				token := common.HexToAddress(tokenStr)

				percentage, err := parsePercentage(parts[2])
				if err != nil {
					c.reply(chatID, fmt.Sprintf("❌ Invalid percentage: %v", err))
					break
				}

				// Get token balance
				balance, err := c.getTokenBalance(token)
				if err != nil {
					c.reply(chatID, fmt.Sprintf("❌ Failed to get token balance: %v", err))
					break
				}

				if balance.Sign() == 0 {
					c.reply(chatID, "❌ No tokens to sell")
					break
				}

				// Calculate sell amount
				sellAmount := new(big.Int).Mul(balance, big.NewInt(int64(percentage)))
				sellAmount.Div(sellAmount, big.NewInt(100))

				c.reply(chatID, fmt.Sprintf("🔄 Selling %d%% of tokens...", percentage))

				// First approve the router if needed
				err = c.approveToken(token, sellAmount)
				if err != nil {
					c.reply(chatID, fmt.Sprintf("❌ Approval failed: %v", err))
					break
				}

				// Execute sell
				txHash, err := c.executeSell(token, sellAmount)
				if err != nil {
					c.reply(chatID, fmt.Sprintf("❌ Sell failed: %v", err))
					break
				}

				c.reply(chatID, fmt.Sprintf(
					"✅ *Sell Executed!*\n"+
						"Token: `%s`\n"+
						"Amount: %d%%\n"+
						"Tx: `%s`",
					token.Hex(), percentage, txHash.Hex()))
			case strings.HasPrefix(text, "/positions"), strings.HasPrefix(text, "/portfolio"):
				c.positionsMu.RLock()
				if len(c.positions) == 0 {
					c.positionsMu.RUnlock()
					c.reply(chatID, "📊 No open positions")
					break
				}

				msg := "📊 *Your Positions:*\n\n"
				for token, pos := range c.positions {
					// Get current balance
					balance, _ := c.getTokenBalance(token)

					msg += fmt.Sprintf(
						"Token: `%s`\n"+
							"Balance: %s\n"+
							"Entry: %s ETH\n"+
							"Time: %s\n"+
							"Tx: `%s`\n\n",
						token.Hex()[:10]+"..."+token.Hex()[36:],
						balance.String(),
						formatEth(pos.EthSpent),
						pos.EntryTime.Format("15:04:05"),
						pos.TxHash.Hex()[:10]+"...",
					)
				}
				c.positionsMu.RUnlock()

				c.reply(chatID, msg)
			case strings.HasPrefix(text, "/balance"):
				if c.walletAddr == (common.Address{}) {
					c.reply(chatID, "❌ No wallet configured")
					break
				}

				balance, err := c.getETHBalance()
				if err != nil {
					c.reply(chatID, "❌ Failed to get balance")
					break
				}

				c.reply(chatID, fmt.Sprintf(
					"💰 *Wallet Balance*\n"+
						"Address: `%s`\n"+
						"Balance: %s ETH",
					c.walletAddr.Hex(), formatEth(balance)))
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
					"*Configuration:*\n\n"+
						"*Network:* %s\n"+
						"*Wallet:* `%s`\n"+
						"*Private Key:* `%s`\n"+
						"*Identity Key:* %s\n"+
						"*Chat ID:* `%d`\n\n"+
						"*Safety Settings:*\n"+
						"Enabled: `%v`\n"+
						"Mode: `%s`\n"+
						"Trusted Tokens: `%d`\n"+
						"Trusted Deployers: `%d`\n"+
						"Min Liquidity: `%s ETH`",
					c.activeNet,
					c.Cfg.BOT_ADDRESS,
					redactedPK,
					idStatus,
					c.Cfg.TELEGRAM_CHAT_ID,
					c.honeypotCheckEnabled,
					c.honeypotCheckMode,
					len(c.trustedTokens),
					len(c.trustedDeployers),
					c.Cfg.MIN_LIQUIDITY_ETH,
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

func (c *Controller) executeBuy(token common.Address, ethAmount *big.Int) (common.Hash, error) {
	ctx := context.Background()

	nonce, err := c.ethClient.PendingNonceAt(ctx, c.walletAddr)
	if err != nil {
		return common.Hash{}, err
	}

	// Build swap data
	path := []common.Address{c.dex.WETH(), token}

	deadline := big.NewInt(time.Now().Add(5 * time.Minute).Unix())

	// Pack the swap function call
	// Using swapExactETHForTokensSupportingFeeOnTransferTokens for compatibility
	data, err := c.routerABI.Pack("swapExactETHForTokensSupportingFeeOnTransferTokens",
		big.NewInt(0), // amountOutMin (0 = accept any amount, add slippage later)
		path,
		c.walletAddr,
		deadline,
	)
	if err != nil {
		return common.Hash{}, fmt.Errorf("failed to pack swap data: %w", err)
	}

	// Get gas price with 20& boost for speed
	gasPrice, err := c.ethClient.SuggestGasPrice(ctx)
	if err != nil {
		return common.Hash{}, err
	}

	// Add 20% to gas price for faster inclusion
	gasPrice = new(big.Int).Mul(gasPrice, big.NewInt(120))
	gasPrice = new(big.Int).Div(gasPrice, big.NewInt(100))

	// Get chain ID
	chainID, err := c.ethClient.ChainID(ctx)
	if err != nil {
		return common.Hash{}, fmt.Errorf("failed to get chain ID: %w", err)
	}

	// Create Transaction
	tx := types.NewTransaction(
		nonce,
		c.dex.Router(),
		ethAmount,      // Value in ETH
		uint64(300000), // gas limit
		gasPrice,
		data,
	)

	// Sign Transaction
	signedTx, err := types.SignTx(tx, types.NewEIP155Signer(chainID), c.privateKey)
	if err != nil {
		return common.Hash{}, fmt.Errorf("failed to sign transaction: %w", err)
	}

	// Send Transaction
	err = c.ethClient.SendTransaction(context.Background(), signedTx)
	if err != nil {
		return common.Hash{}, fmt.Errorf("failed to send transaction: %w", err)
	}

	telemetry.Infof("[buy] sent tx: %s for token %s with %s ETH",
		signedTx.Hash().Hex(), token.Hex(), formatEth(ethAmount))

	return signedTx.Hash(), nil
}

func (c *Controller) approveToken(token common.Address, amount *big.Int) error {
	ctx := context.Background()

	// Check current allowance
	allowanceData, err := c.erc20ABI.Pack("allowance", c.walletAddr, c.dex.Router())
	if err != nil {
		return fmt.Errorf("failed to pack allowance call: %w", err)
	}

	result, err := c.ethClient.CallContract(ctx, ethereum.CallMsg{
		To:   &token,
		Data: allowanceData,
	}, nil)

	if err == nil && len(result) > 0 {
		currentAllowance := new(big.Int).SetBytes(result)
		if currentAllowance.Cmp(amount) >= 0 {
			// Already approved
			return nil
		}
	}

	// Need to approve
	nonce, err := c.ethClient.PendingNonceAt(ctx, c.walletAddr)
	if err != nil {
		return err
	}

	// Max approval for convenience
	maxApproval := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))

	approveData, err := c.erc20ABI.Pack("approve", c.dex.Router(), maxApproval)
	if err != nil {
		return err
	}

	gasPrice, _ := c.ethClient.SuggestGasPrice(ctx)
	chainID, _ := c.ethClient.ChainID(ctx)

	tx := types.NewTransaction(
		nonce,
		token,
		big.NewInt(0),  // no ETH value
		uint64(100000), // gas limit
		gasPrice,
		approveData,
	)

	signedTx, err := types.SignTx(tx, types.NewEIP155Signer(chainID), c.privateKey)
	if err != nil {
		return err
	}

	err = c.ethClient.SendTransaction(ctx, signedTx)
	if err != nil {
		return err
	}

	telemetry.Infof("[approve] sent approval tx: %s for token %s",
		signedTx.Hash().Hex(), token.Hex())

	// Wait for approval to be mined (simplified - production should use proper receipt waiting)
	time.Sleep(3 * time.Second)

	return nil
}

func (c *Controller) executeSell(token common.Address, amount *big.Int) (common.Hash, error) {
	ctx := context.Background()

	nonce, err := c.ethClient.PendingNonceAt(ctx, c.walletAddr)
	if err != nil {
		return common.Hash{}, err
	}

	// Build sweap path (token -> WETH)
	path := []common.Address{token, c.dex.WETH()}
	deadline := big.NewInt(time.Now().Add(5 * time.Minute).Unix())

	// Pack the swap function
	data, err := c.routerABI.Pack(
		"swapExactTokensForETHSupportingFeeOnTransferTokens",
		amount,
		big.NewInt(0), // amountOutMin (any) - add slippage protection later
		path,
		c.walletAddr,
		deadline,
	)
	if err != nil {
		return common.Hash{}, fmt.Errorf("failed to pack swap data: %w", err)
	}

	gasPrice, _ := c.ethClient.SuggestGasPrice(ctx)
	gasPrice = new(big.Int).Mul(gasPrice, big.NewInt(120))
	gasPrice = new(big.Int).Div(gasPrice, big.NewInt(100))

	chainID, _ := c.ethClient.ChainID(ctx)

	tx := types.NewTransaction(
		nonce,
		c.dex.Router(),
		big.NewInt(0),  // no ETH value
		uint64(300000), // gas limit
		gasPrice,
		data,
	)

	signedTx, err := types.SignTx(tx, types.NewEIP155Signer(chainID), c.privateKey)
	if err != nil {
		return common.Hash{}, fmt.Errorf("failed to sign transaction: %w", err)
	}

	err = c.ethClient.SendTransaction(ctx, signedTx)
	if err != nil {
		return common.Hash{}, fmt.Errorf("failed to send transaction: %w", err)
	}

	telemetry.Infof("[sell] sent tx: %s for token %s amount %s",
		signedTx.Hash().Hex(), token.Hex(), amount.String())

	return signedTx.Hash(), nil
}

// ------------- Helper Functions ---------------
func formatEth(wei *big.Int) string {
	if wei == nil {
		return "0"
	}
	// Convert wei to ETH (divide by 10^18)
	ethValue := new(big.Float).SetInt(wei)
	ethValue.Quo(ethValue, big.NewFloat(1e18))

	// Format with precision
	f, _ := ethValue.Float64()
	if f < 0.0001 {
		return fmt.Sprintf("%.8f", f)
	} else if f < 1 {
		return fmt.Sprintf("%.6f", f)
	} else if f < 100 {
		return fmt.Sprintf("%.4f", f)
	}
	return fmt.Sprintf("%.2f", f)
}

// parseEthAmount converts user input like "0.1" to wei
func parseEthAmount(input string) (*big.Int, error) {
	// Handle common shortcuts
	switch strings.ToLower(input) {
	case "all", "max":
		return nil, fmt.Errorf("use specific amount")
	}

	// Remove any "ETH" suffix
	input = strings.TrimSuffix(strings.ToLower(input), "eth")
	input = strings.TrimSpace(input)

	// Parse as float
	amount, err := strconv.ParseFloat(input, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid amount: %s", input)
	}

	if amount <= 0 {
		return nil, fmt.Errorf("amount must be positive")
	}

	// convert to wei (mul by 10^18)
	wei := new(big.Float).SetFloat64(amount)
	wei.Mul(wei, big.NewFloat(1e18))

	// Convert to big.Int
	result := new(big.Int)
	wei.Int(result)

	return result, nil
}

// parsePercentage converts "50" or "50%" to integer 50
func parsePercentage(input string) (int, error) {
	input = strings.TrimSuffix(input, "%")
	input = strings.TrimSpace(input)

	percentage, err := strconv.Atoi(input)
	if err != nil {
		return 0, fmt.Errorf("invalid percentage: %s", input)
	}

	if percentage <= 0 || percentage > 100 {
		return 0, fmt.Errorf("percentage must be between 1 and 100")
	}
	return percentage, nil
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

// getTokenBalance retrieves ERC20 token balance for wallet
func (c *Controller) getTokenBalance(token common.Address) (*big.Int, error) {
	// ERC20 balanceOf method
	data := common.FromHex("0x70a08231") // balanceOf(address)
	data = append(data, common.LeftPadBytes(c.walletAddr.Bytes(), 32)...)

	result, err := c.ethClient.CallContract(context.Background(), ethereum.CallMsg{
		To:   &token,
		Data: data,
	}, nil)
	if err != nil {
		return nil, err
	}

	if len(result) == 0 {
		return big.NewInt(0), nil
	}

	return new(big.Int).SetBytes(result), nil
}

// getETHBalance retrieves ETH balance for wallet
func (c *Controller) getETHBalance() (*big.Int, error) {
	return c.ethClient.BalanceAt(context.Background(), c.walletAddr, nil)
}

// loadPrivateKey loads and validates private key from config
func loadPrivateKey(privateKeyHex string) (*ecdsa.PrivateKey, common.Address, error) {
	if privateKeyHex == "" {
		return nil, common.Address{}, fmt.Errorf("private key is empty")
	}

	// Remove 0x prefix if present
	privateKeyHex = strings.TrimPrefix(privateKeyHex, "0x")

	privateKey, err := crypto.HexToECDSA(privateKeyHex)
	if err != nil {
		return nil, common.Address{}, fmt.Errorf("invalid private key: %w", err)
	}

	publicKey := privateKey.Public()
	publicKeyECDSA, ok := publicKey.(*ecdsa.PublicKey)
	if !ok {
		return nil, common.Address{}, fmt.Errorf("invalid public key type")
	}

	address := crypto.PubkeyToAddress(*publicKeyECDSA)

	return privateKey, address, nil
}
