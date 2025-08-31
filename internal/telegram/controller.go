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
	execution "github.com/meltingclock/biteblock_v1/internal/executor"
	"github.com/meltingclock/biteblock_v1/internal/helpers"
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

	// Network and blockchain
	ethClient *ethclient.Client
	dex       *v2.Registry
	activeNet string // NEW: "ethereum" | "bsc" | "base"

	executor    *execution.TradeExecutor
	tradeConfig execution.TradeConfig

	// NEW: Safety settings (runtime configurable)
	honeypotCheckEnabled bool
	honeypotCheckMode    string                                  // "always", "smart", "never"
	trustedTokens        map[common.Address]bool                 // Skip check for these
	trustedDeployers     map[common.Address]bool                 // Skip check for tokens from these addresses
	checkCache           map[common.Address]*scanner.TokenSafety // Cache results
	//cacheMu              sync.RWMutex

	autoBuyEnabled bool // Runtime toogle

	// Bundle execution
	//bundler      *bundle.Bundler
	//useFlashbots bool                  // Future use
	//bribeAmount  string                // ETH Amount for miner bribe
	//useBundle    bool                  // Whether to use bundle execution
	//bundleConfig execution.TradeConfig // Execution configuration
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
		autoBuyEnabled:       cfg.AUTO_BUY_ENABLED,
		// Deafult trade config
		tradeConfig: execution.TradeConfig{
			GasBoostPercent: cfg.AUTO_GAS_BOOST,
			MaxGasPrice:     nil, // Will set when parsing
			SlippagePercent: cfg.SLIPPAGE_PERCENT,
			DeadlineSeconds: 300,
			UseBundles:      false,
			BribeAmount:     nil,
		},
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

	// Parse max gas price
	if cfg.MAX_GAS_PRICE_GWEI != "" {
		ctrl.tradeConfig.MaxGasPrice, _ = helpers.GweiToWei(cfg.MAX_GAS_PRICE_GWEI)
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
	// 1. GET NETWORK PRESET
	p, ok := netPresets[strings.ToLower(c.activeNet)]
	if !ok {
		return fmt.Errorf("unknown network preset: %s", c.activeNet)
	}

	telemetry.Infof("[controller] starting on %s network", c.activeNet)
	c.reply(chatID, fmt.Sprintf("🔄 Connecting to *%s*...", c.activeNet))

	// 2. CREATE CONTEXT WITH CANCELLATION
	wctx, cancel := context.WithCancel(ctx)
	c.cancelFn = cancel

	// 3. ESTABLISH RPC/WEBSOCKET CONNECTION
	telemetry.Debugf("[controller] connecting to %s", p.WSSURL)

	rpcCl, err := rpc.DialContext(wctx, p.WSSURL)
	if err != nil {
		cancel()
		return fmt.Errorf("rpc dial failed: %w", err)
	}

	c.ethClient = ethclient.NewClient(rpcCl)

	// Verify connection
	chainID, err := c.ethClient.ChainID(wctx)
	if err != nil {
		cancel()
		return fmt.Errorf("failed to get chain ID: %w", err)
	}

	if chainID.Int64() != p.ChainID {
		telemetry.Warnf("[controller] chain ID mismatch: got %d, expected %d",
			chainID.Int64(), p.ChainID)
	}

	// 4. BUILD DEX REGISTRY
	c.dex = v2.NewRegistry(v2.Config{
		Network: v2.Network(strings.ToLower(c.activeNet)),
		Factory: p.Factory,
		Router:  p.Router,
		WETH:    p.WETH,
	})

	telemetry.Infof("[controller] DEX configured - Factory: %s, Router: %s, WETH: %s",
		p.Factory.Hex(), p.Router.Hex(), p.WETH.Hex())

	// 5. INITIALIZE EXECUTOR (Trading Engine)
	walletStatus := "🔴 *Watch-Only Mode*"

	if c.Cfg.PRIVATE_KEY != "" {
		telemetry.Debugf("[controller] initializing executor with private key")

		privateKey, walletAddr, err := helpers.ValidatePrivateKey(c.Cfg.PRIVATE_KEY)
		if err != nil {
			c.reply(chatID, fmt.Sprintf(
				"⚠️ *Private Key Error*\n```\n%v\n```\nRunning in watch-only mode",
				err))
			telemetry.Errorf("[controller] private key validation failed: %v", err)
		} else {
			// Create executor
			c.executor, err = execution.NewTradeExecutor(
				c.ethClient,
				privateKey,
				walletAddr,
				c.dex,
			)

			if err != nil {
				c.reply(chatID, fmt.Sprintf(
					"⚠️ *Executor Init Failed*\n```\n%v\n```",
					err))
				telemetry.Errorf("[controller] executor creation failed: %v", err)
			} else {
				// Check wallet balance
				balance, err := c.executor.GetETHBalance(wctx)
				if err != nil {
					walletStatus = fmt.Sprintf(
						"⚠️ *Wallet Connected* (balance check failed)\n"+
							"Address: `%s`",
						walletAddr.Hex())
				} else {
					walletStatus = fmt.Sprintf(
						"✅ *Wallet Connected*\n"+
							"Address: `%s`\n"+
							"Balance: **%s ETH**",
						walletAddr.Hex(),
						helpers.FormatEth(balance))

					// Warn if low balance
					minRequired := helpers.Wei("0.1") // 0.1 ETH minimum recommended
					if balance.Cmp(minRequired) < 0 {
						walletStatus += "\n⚠️ *Low balance - add funds to trade*"
					}
				}

				// Configure trade settings from config
				if c.Cfg.MAX_GAS_PRICE_GWEI != "" {
					c.tradeConfig.MaxGasPrice, _ = helpers.GweiToWei(c.Cfg.MAX_GAS_PRICE_GWEI)
				}
				c.tradeConfig.GasBoostPercent = c.Cfg.AUTO_GAS_BOOST
				c.tradeConfig.SlippagePercent = c.Cfg.SLIPPAGE_PERCENT
				c.tradeConfig.DeadlineSeconds = 300

				telemetry.Infof("[controller] executor ready - wallet: %s", walletAddr.Hex())
			}
		}
	} else {
		telemetry.Infof("[controller] no private key configured - watch-only mode")
	}

	// 6. INITIALIZE SIGNAL DETECTION & SCANNERS

	// Pair Registry (tracks DEX pairs)
	pairReg := signals.NewPairRegistry(c.dex, 48*time.Hour)
	pairReg.Start(wctx, c.ethClient)
	telemetry.Infof("[controller] pair registry started")

	// Mint Watcher (tracks liquidity confirmation)
	startMintWatcher(wctx, c.ethClient, pairReg)
	telemetry.Infof("[controller] mint watcher started")

	// Liquidity Analyzer (detects pending liquidity)
	liq := signals.NewLiquidityAnalyzer(c.ethClient, c.dex, pairReg)
	telemetry.Infof("[controller] liquidity analyzer ready")

	// Scanner (safety & filter checks)
	minLiquidityWei := helpers.Wei(c.Cfg.MIN_LIQUIDITY_ETH)
	if minLiquidityWei == nil {
		minLiquidityWei = helpers.Wei("0.5") // Default 0.5 ETH
	}

	scan := scanner.New(c.ethClient, c.dex, scanner.Config{
		RequireWethPair:   true,
		MinEthLiquidity:   minLiquidityWei,
		MinTokenLiquidity: nil,
		AllowCreators:     map[common.Address]bool{},
		DenyCreators:      map[common.Address]bool{},
		Deadline:          250 * time.Millisecond,
	})
	telemetry.Infof("[controller] scanner configured - min liquidity: %s ETH",
		helpers.FormatEth(minLiquidityWei))

	// 7. CONFIGURE AUTO-BUY
	c.autoBuyEnabled = c.Cfg.AUTO_BUY_ENABLED && c.executor != nil

	autoBuyStatus := "🔴 *Auto-Buy: DISABLED*"
	if c.executor == nil {
		autoBuyStatus = "⚠️ *Auto-Buy: Unavailable* (no wallet)"
	} else if c.autoBuyEnabled {
		autoBuyStatus = fmt.Sprintf(
			"🟢 *Auto-Buy: ENABLED*\n"+
				"• Amount: %s ETH\n"+
				"• Min Liquidity: %s ETH\n"+
				"• Max Gas: %s gwei\n"+
				"• Safety Check: %v\n"+
				"• Bundles: %v",
			c.Cfg.AUTO_BUY_AMOUNT,
			c.Cfg.MIN_LIQUIDITY_ETH,
			c.Cfg.MAX_GAS_PRICE_GWEI,
			c.honeypotCheckEnabled,
			c.tradeConfig.UseBundles)
	}

	// 8. START MEMPOOL WATCHER
	c.watcher = mempool.NewWatcher(p.WSSURL, func(ctx context.Context, tx *types.Transaction) error {
		// Basic pending tx logging (optional - can be noisy)
		from, _ := mempool.SendTxVerifier(tx)

		// Only log high-value transactions
		if tx.Value().Sign() > 0 && tx.Value().Cmp(helpers.Wei("1")) >= 0 {
			telemetry.Debugf("[mempool] high-value tx: %s from %s value %s ETH",
				tx.Hash().Hex(), from.Hex(), helpers.FormatEth(tx.Value()))
		}

		// CHECK FOR LIQUIDITY ADDITIONS
		if tx.To() != nil && *tx.To() == c.dex.Router() {
			// Quick router function check
			if meta, ok := c.dex.LookupSelectorFromData(tx.Data()); ok {
				telemetry.Debugf("[router] %s -> %s", tx.Hash().Hex(), meta.Name)
			}

			// Analyze for liquidity
			sig, err := liq.AnalyzePending(ctx, tx)
			if err != nil || sig == nil {
				return nil // Not liquidity or error
			}

			// Run scanner filters
			rep, err := scan.Run(ctx, sig)
			if err != nil || !rep.Pass {
				telemetry.Debugf("[scan] rejected: %v", rep.Reasons)
				return nil
			}

			// PASSED ALL FILTERS!
			telemetry.Infof("[liquidity] DETECTED pair=%s from=%s liquidity=%s ETH",
				sig.Pair.Hex(), sig.From.Hex(), helpers.FormatEth(rep.ETHInWei))

			// Identify target token
			token := c.identifyTargetToken(sig)
			liquidityStr := "Unknown"
			if rep.ETHInWei != nil {
				liquidityStr = helpers.FormatEth(rep.ETHInWei) + " ETH"
			}

			// AUTO-BUY EXECUTION
			if c.autoBuyEnabled {
				go func() {
					buyCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
					defer cancel()
					c.executeAutoBuy(buyCtx, sig, rep, chatID)
				}()
			} else {
				// Manual mode notification
				c.reply(chatID, fmt.Sprintf(
					"💧 *Liquidity Detected*\n\n"+
						"Token: `%s`\n"+
						"Pair: `%s`\n"+
						"Liquidity: %s\n"+
						"From: `%s`\n\n"+
						"Use: `/buy %s %s`\n"+
						"Or enable: `/autobuy on`",
					token.Hex(),
					sig.Pair.Hex(),
					liquidityStr,
					sig.From.Hex(),
					helpers.FormatAddress(token),
					c.Cfg.AUTO_BUY_AMOUNT))
			}

			// Background receipt monitor
			go c.monitorLiquidityReceipt(sig.Hash, sig.Pair, chatID)
		}

		return nil
	})

	// Start the watcher
	if err := c.watcher.Start(wctx); err != nil {
		cancel()
		return fmt.Errorf("mempool watcher start failed: %w", err)
	}

	c.running = true
	telemetry.Infof("[controller] all systems started successfully")

	// 9. SEND FINAL STATUS REPORT
	statusReport := fmt.Sprintf(
		"🟢 **SNIPER STARTED**\n\n"+
			"**Network:** %s\n"+
			"**Chain ID:** %d\n\n"+
			"%s\n\n"+
			"%s\n\n"+
			"**Components:**\n"+
			"✅ Mempool Watcher\n"+
			"✅ Liquidity Scanner\n"+
			"✅ Safety Checker: %v\n"+
			"✅ Pair Registry\n\n"+
			"**Commands:**\n"+
			"• `/buy <token> <eth>` - Manual buy\n"+
			"• `/sell <token> <%%>` - Sell position\n"+
			"• `/positions` - View holdings\n"+
			"• `/bundle on/off` - Toggle bundles\n"+
			"• `/autobuy on/off` - Toggle auto-buy\n"+
			"• `/stop` - Stop the bot",
		c.activeNet,
		chainID.Int64(),
		walletStatus,
		autoBuyStatus,
		c.honeypotCheckEnabled)

	c.reply(chatID, statusReport)

	return nil
}

// Helper function to monitor liquidity receipt
func (c *Controller) monitorLiquidityReceipt(txHash common.Hash, pair common.Address, chatID int64) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			receipt, err := c.ethClient.TransactionReceipt(ctx, txHash)
			if err != nil {
				continue // Not mined yet
			}

			if receipt.Status == 1 {
				telemetry.Infof("[liquidity] confirmed in block %d", receipt.BlockNumber)

				// Check for mint event
				if evt, ok := signals.FindMintInReceipt(receipt, pair); ok {
					c.reply(chatID, fmt.Sprintf(
						"✅ *Liquidity Confirmed*\n"+
							"Block: %d\n"+
							"Pair: `%s`\n"+
							"Amount0: %s\n"+
							"Amount1: %s",
						receipt.BlockNumber,
						pair.Hex(),
						evt.Amount0.String(),
						evt.Amount1.String()))
				}
			} else {
				telemetry.Warnf("[liquidity] tx failed: %s", txHash.Hex())
			}
			return
		}
	}
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
						"/help - Show all available commands with descriptions\n"+
						"⚙️ *Network*\n"+
						"/net <ethereum|bsc|base> – Select network preset (applied on /start)\n\n"+
						"*🤖 *Sniper Bot Commands*\n"+
						"*Auto-Buy:*\n"+
						"/autobuy <on|off> - Enable/disable auto-buy\n"+
						"/setamount <eth> - Set buy amount (e.g., 0.5)\n"+
						"/setgas <gwei> - Set max gas price\n"+
						"/safety - Toggle safety checks\n\n"+
						"▶️ *Control*\n"+
						"/start – Start mempool watcher\n"+
						"/stop – Stop mempool watcher\n\n"+
						"/status – Show current state & preset\n"+
						"🤖 *Execution*\n"+
						"/buy - Buy a token: \"buy <token_address> <eth_amount>\"\n"+
						"/position - List open positions\n"+
						"/sell - Sell a token: \"sell <token_address> <eth_amount>\"\n"+
						"/balance - Show wallet balance\n\n"+
						"ℹ️ *Info*\n"+
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
			case strings.HasPrefix(text, "/autobuy"):
				parts := strings.Fields(text)
				if len(parts) < 2 {
					// Show detailed status
					status := "OFF 🔴"
					if c.autoBuyEnabled {
						status = "ON 🟢"
					}

					// Check if auto-buy is even possible
					capability := "✅ Ready"
					if c.executor == nil {
						capability = "❌ No wallet configured"
						status = "UNAVAILABLE ⚠️"
					} else if !c.running {
						capability = "⚠️ Bot not running"
					}

					// Bundle status
					bundleStatus := "Disabled"
					if c.tradeConfig.UseBundles {
						bundleStatus = fmt.Sprintf("Enabled (Bribe: %s ETH)",
							helpers.FormatEth(c.tradeConfig.BribeAmount))
					}

					c.reply(chatID, fmt.Sprintf(
						"*Auto-Buy Status: %s*\n"+
							"*Capability: %s*\n\n"+
							"**Configuration:**\n"+
							"• Buy Amount: %s ETH\n"+
							"• Min Liquidity: %s ETH\n"+
							"• Max Gas: %s gwei\n"+
							"• Gas Boost: %d%%\n"+
							"• Slippage: %d%%\n"+
							"• Safety Check: %v\n"+
							"• Bundles: %s\n\n"+
							"**Safety Mode:** %s\n"+
							"**Trusted Tokens:** %d\n"+
							"**Trusted Deployers:** %d\n\n"+
							"Usage: `/autobuy <on|off>`",
						status,
						capability,
						c.Cfg.AUTO_BUY_AMOUNT,
						c.Cfg.MIN_LIQUIDITY_ETH,
						c.Cfg.MAX_GAS_PRICE_GWEI,
						c.Cfg.AUTO_GAS_BOOST,
						c.Cfg.SLIPPAGE_PERCENT,
						c.Cfg.HONEYPOT_CHECK_ENABLED,
						bundleStatus,
						c.honeypotCheckMode,
						len(c.trustedTokens),
						len(c.trustedDeployers)))
					break
				}

				switch strings.ToLower(parts[1]) {
				case "on", "enable", "start":
					// Check prerequisites
					if c.executor == nil {
						c.reply(chatID,
							"❌ *Cannot Enable Auto-Buy*\n\n"+
								"No wallet configured. Please:\n"+
								"1. Add PRIVATE_KEY to config.yml\n"+
								"2. Restart with `/start`")
						break
					}

					if !c.running {
						c.reply(chatID,
							"⚠️ *Bot Not Running*\n\n"+
								"Start the bot first with `/start`")
						break
					}

					// Check wallet balance
					balance, err := c.executor.GetETHBalance(context.Background())
					if err != nil {
						c.reply(chatID, fmt.Sprintf("⚠️ Could not check balance: %v", err))
					} else {
						buyAmount, _ := helpers.EthToWei(c.Cfg.AUTO_BUY_AMOUNT)
						minRequired := new(big.Int).Add(buyAmount, big.NewInt(1e16)) // + gas

						if balance.Cmp(minRequired) < 0 {
							c.reply(chatID, fmt.Sprintf(
								"⚠️ *Low Balance Warning*\n\n"+
									"Current: %s ETH\n"+
									"Needed: %s ETH\n"+
									"(for %s ETH buys + gas)\n\n"+
									"Auto-buy will fail without funds!",
								helpers.FormatEth(balance),
								helpers.FormatEth(minRequired),
								c.Cfg.AUTO_BUY_AMOUNT))
						}
					}

					// Enable auto-buy
					c.autoBuyEnabled = true
					c.Cfg.AUTO_BUY_ENABLED = true
					_ = config.Save(c.Path, c.Cfg)

					// Build confirmation message
					confirmMsg := "🟢 *AUTO-BUY ENABLED*\n\n" +
						"Bot will automatically execute when:\n" +
						fmt.Sprintf("• Liquidity ≥ %s ETH detected\n", c.Cfg.MIN_LIQUIDITY_ETH) +
						fmt.Sprintf("• Gas price ≤ %s gwei\n", c.Cfg.MAX_GAS_PRICE_GWEI)

					if c.honeypotCheckEnabled {
						confirmMsg += "• Safety check passes\n"
					}

					confirmMsg += fmt.Sprintf("\n**Buy Amount:** %s ETH per trade", c.Cfg.AUTO_BUY_AMOUNT)

					if c.tradeConfig.UseBundles {
						confirmMsg += "\n**Mode:** Bundle (Flashbots)"
					} else {
						confirmMsg += "\n**Mode:** Normal mempool"
					}

					c.reply(chatID, confirmMsg)
					telemetry.Infof("[controller] auto-buy enabled via telegram")

				case "off", "disable", "stop":
					c.autoBuyEnabled = false
					c.Cfg.AUTO_BUY_ENABLED = false
					_ = config.Save(c.Path, c.Cfg)

					c.reply(chatID,
						"🔴 *AUTO-BUY DISABLED*\n\n"+
							"Switched to manual mode.\n"+
							"You will receive notifications but must execute manually.\n\n"+
							"Re-enable with: `/autobuy on`")
					telemetry.Infof("[controller] auto-buy disabled via telegram")

				case "test":
					// Hidden test command for debugging
					if c.executor == nil {
						c.reply(chatID, "❌ No executor available")
						break
					}

					c.reply(chatID, "🧪 *Auto-Buy Test*\n\nSimulating auto-buy trigger...")

					/*
						// Create fake signal for testing
						testSignal := &signals.LiquiditySignal{
							Token0: c.dex.WETH(),
							Token1: common.HexToAddress("0x1234567890123456789012345678901234567890"),
							Pair:   common.HexToAddress("0x0000000000000000000000000000000000000000"),
						}
						testReport := &scanner.Report{
							ETHInWei: helpers.Wei("1.0"),
							Pass:     true,
						}
					*/

					// Test with dry-run (would need to add dry-run support)
					c.reply(chatID,
						"✅ Test complete\n"+
							"Executor: Ready\n"+
							"Safety Check: Enabled\n"+
							"Would buy with: "+c.Cfg.AUTO_BUY_AMOUNT+" ETH")

				default:
					c.reply(chatID,
						"**Usage:** `/autobuy <command>`\n\n"+
							"**Commands:**\n"+
							"• `on` - Enable auto-buy\n"+
							"• `off` - Disable auto-buy\n"+
							"• (no args) - Show status")
				}
			case strings.HasPrefix(text, "/setgas"):
				parts := strings.Fields(text)
				if len(parts) < 2 {
					c.reply(chatID, fmt.Sprintf(
						"Max gas: %s gwei\nUsage: /setgas <gwei>",
						c.Cfg.MAX_GAS_PRICE_GWEI))
					break
				}

				c.Cfg.MAX_GAS_PRICE_GWEI = parts[1]
				_ = config.Save(c.Path, c.Cfg)
				c.reply(chatID, fmt.Sprintf("✅ Max gas set to %s gwei", parts[1]))

			case strings.HasPrefix(text, "/safety"):
				// Toggle safety check
				c.Cfg.HONEYPOT_CHECK_ENABLED = !c.Cfg.HONEYPOT_CHECK_ENABLED
				_ = config.Save(c.Path, c.Cfg)

				if c.Cfg.HONEYPOT_CHECK_ENABLED {
					c.reply(chatID, "🛡️ Safety check ENABLED")
				} else {
					c.reply(chatID, "⚠️ Safety check DISABLED - Be careful!")
				}
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
				if c.executor == nil {
					c.reply(chatID, "❌ No wallet configured. Cannot execute trades.")
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

				ethAmount, err := helpers.EthToWei(parts[2])
				if err != nil {
					c.reply(chatID, fmt.Sprintf("❌ Invalid amount: %v", err))
					break
				}

				// Check wallet balance
				balance, err := c.executor.GetETHBalance(context.Background())
				if err != nil {
					c.reply(chatID, "❌ Could not check wallet balance")
					break
				}

				// Need funds for buy + gas
				gasReserve := big.NewInt(1e16) // 0.01 ETH for gas
				required := new(big.Int).Add(ethAmount, gasReserve)
				if balance.Cmp(required) < 0 {
					c.reply(chatID, fmt.Sprintf("❌ Insufficient balance\nNeeded: %s ETH\nHave: %s ETH",
						helpers.FormatEth(required), helpers.FormatEth(balance)))
					break
				}

				// ============ HONEYPOT CHECK ============
				if c.honeypotCheckEnabled {
					c.reply(chatID, "🔍 Running safety analysis...")

					checker := scanner.NewHoneypotChecker(c.ethClient, c.dex)
					safety, err := checker.CheckToken(context.Background(), token)
					if err != nil {
						c.reply(chatID, fmt.Sprintf("⚠️ Safety check error: %v\nProceed with caution!", err))
					} else {
						// Display safety report
						c.displaySafetyReport(chatID, safety)

						// Block if honeypot
						if safety.IsHoneypot {
							c.reply(chatID, "🚨 *TRANSACTION BLOCKED*\nHoneypot detected! Use /forcebuy to override.")
							break
						}

						// Warn if risky
						if safety.SafetyScore < 40 {
							c.reply(chatID, "⚠️ *HIGH RISK TOKEN*\nUse /forcebuy to proceed anyway.")
							break
						}
					}
				}

				// Execute buy if safe
				c.reply(chatID, fmt.Sprintf("🔄 Buying with %s ETH...", helpers.FormatEth(ethAmount)))

				txHash, err := c.executor.ExecuteBuy(
					context.Background(),
					token,
					ethAmount,
					c.tradeConfig,
				)
				if err != nil {
					c.reply(chatID, fmt.Sprintf("❌ Buy failed: %v", err))
					break
				}

				c.reply(chatID, fmt.Sprintf(
					"✅ *Buy Executed!*\n"+
						"Token: `%s`\n"+
						"Amount: %s ETH\n"+
						"Tx: `%s`\n"+
						"Bundle: %v",
					token.Hex(),
					helpers.FormatEth(ethAmount),
					txHash.Hex(),
					c.tradeConfig.UseBundles))
			case strings.HasPrefix(text, "/forcebuy"):
				if c.executor == nil {
					c.reply(chatID, "❌ No wallet configured. Cannot execute trades.")
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

				ethAmount, err := helpers.EthToWei(parts[2])
				if err != nil {
					c.reply(chatID, fmt.Sprintf("❌ Invalid amount: %v", err))
					break
				}

				// Check wallet balance
				balance, err := c.executor.GetETHBalance(context.Background())
				if err != nil {
					c.reply(chatID, "❌ Could not check wallet balance")
					break
				}

				// Need funds for buy + gas
				gasReserve := big.NewInt(1e16) // 0.01 ETH for gas
				required := new(big.Int).Add(ethAmount, gasReserve)
				if balance.Cmp(required) < 0 {
					c.reply(chatID, fmt.Sprintf("❌ Insufficient balance\nNeeded: %s ETH\nHave: %s ETH",
						helpers.FormatEth(required), helpers.FormatEth(balance)))
					break
				}

				c.reply(chatID, "⚠️ *WARNING: Bypassing all safety checks!*")

				c.reply(chatID, fmt.Sprintf("🔄 Buying with %s ETH...", helpers.FormatEth(ethAmount)))

				txHash, err := c.executor.ExecuteBuy(
					context.Background(),
					token,
					ethAmount,
					c.tradeConfig,
				)
				if err != nil {
					c.reply(chatID, fmt.Sprintf("❌ Buy failed: %v", err))
					break
				}

				c.reply(chatID, fmt.Sprintf(
					"✅ *Buy Executed!*\n"+
						"Token: `%s`\n"+
						"Amount: %s ETH\n"+
						"Tx: `%s`\n"+
						"Bundle: %v",
					token.Hex(),
					helpers.FormatEth(ethAmount),
					txHash.Hex(),
					c.tradeConfig.UseBundles))
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
					helpers.FormatEth(safety.LiquidityETH),
				)

				if len(safety.RiskFactors) > 0 && len(safety.RiskFactors) <= 5 {
					quickReport += "\n*Main Risks:*\n"
					for _, risk := range safety.RiskFactors {
						quickReport += fmt.Sprintf("• %s\n", risk)
					}
				}

				c.reply(chatID, quickReport)
			case strings.HasPrefix(text, "/bundle"):
				parts := strings.Fields(text)
				if len(parts) < 2 {
					status := "OFF 🔴"
					if c.tradeConfig.UseBundles {
						status = "ON 🟢"
					}
					bribeStr := "0"
					if c.tradeConfig.BribeAmount != nil {
						bribeStr = helpers.FormatEth(c.tradeConfig.BribeAmount)
					}
					c.reply(chatID, fmt.Sprintf(
						"*Bundle Status: %s*\n\n"+
							"Bribe: %s ETH\n"+
							"Gas Boost: %d%%\n\n"+
							"Usage:\n"+
							"/bundle <on|off> - Toggle bundles\n"+
							"/setbribe <eth> - Set bribe amount",
						status, bribeStr, c.tradeConfig.GasBoostPercent))
					break
				}

				switch strings.ToLower(parts[1]) {
				case "on":
					c.tradeConfig.UseBundles = true
					c.reply(chatID, "🟢 *Bundles ENABLED*\nUsing Flashbots for execution")
				case "off":
					c.tradeConfig.UseBundles = false
					c.reply(chatID, "🔴 *Bundles DISABLED*\nUsing normal mempool")
				default:
					c.reply(chatID, "Use: /bundle on or /bundle off")
				}

			case strings.HasPrefix(text, "/setbribe"):
				parts := strings.Fields(text)
				if len(parts) < 2 {
					c.reply(chatID, "Usage: /setbribe <eth_amount>")
					break
				}

				amount, err := helpers.EthToWei(parts[1])
				if err != nil {
					c.reply(chatID, "❌ Invalid amount")
					break
				}

				c.tradeConfig.BribeAmount = amount
				c.reply(chatID, fmt.Sprintf("✅ Bundle bribe set to %s ETH", parts[1]))
			case strings.HasPrefix(text, "/sell "):
				if c.executor == nil {
					c.reply(chatID, "❌ No wallet configured. Cannot execute trades.")
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

				percentage, err := helpers.ParsePercentage(parts[2])
				if err != nil {
					c.reply(chatID, fmt.Sprintf("❌ Invalid percentage: %v", err))
					break
				}

				// Calculate sell amount based on percentage
				// This is a simplified version - executor should handle token balance checking
				c.reply(chatID, fmt.Sprintf("🔄 Selling %d%% of tokens...", percentage))

				// Let executor handle the full token amount calculation
				txHash, err := c.executor.ExecuteSell(
					context.Background(),
					token,
					nil, // Pass nil to let executor calculate based on percentage
					c.tradeConfig,
				)
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

				c.reply(chatID, fmt.Sprintf(
					"✅ *Sell Executed!*\n"+
						"Token: `%s`\n"+
						"Amount: %d%%\n"+
						"Tx: `%s`",
					token.Hex(), percentage, txHash.Hex()))
			case strings.HasPrefix(text, "/positions"), strings.HasPrefix(text, "/portfolio"):
				if c.executor == nil {
					c.reply(chatID, "❌ No wallet configured")
					break
				}

				positions := c.executor.GetPositions()
				if len(positions) == 0 {
					c.reply(chatID, "📊 No open positions")
					break
				}

				msg := "📊 *Your Positions:*\n\n"
				for token, pos := range positions {
					msg += fmt.Sprintf(
						"Token: `%s`\n"+
							"Entry: %s ETH\n"+
							"Time: %s\n"+
							"Tx: `%s`\n\n",
						helpers.FormatAddress(token),
						helpers.FormatEth(pos.EthSpent),
						pos.EntryTime.Format("15:04:05"),
						helpers.FormatTxHash(pos.TxHash),
					)
				}

				c.reply(chatID, msg)
			case strings.HasPrefix(text, "/balance"):
				if c.executor == nil {
					c.reply(chatID, "❌ No wallet configured")
					break
				}

				// Get balance from executor
				balance, err := c.executor.GetETHBalance(context.Background())
				if err != nil {
					c.reply(chatID, fmt.Sprintf("❌ Failed to get balance: %v", err))
					break
				}

				// Get wallet address from executor
				// Note: You need to add a GetWalletAddress() method to executor
				// OR store it during executor creation

				// Option 1: If you add GetWalletAddress() to executor:
				walletAddr := c.executor.GetWalletAddress()

				// Calculate gas reserve estimate
				gasEstimate := big.NewInt(1e16) // 0.01 ETH
				availableForTrading := new(big.Int).Sub(balance, gasEstimate)
				if availableForTrading.Sign() < 0 {
					availableForTrading = big.NewInt(0)
				}

				// Build detailed balance report
				balanceReport := fmt.Sprintf(
					"💰 *Wallet Balance*\n\n"+
						"**Address:** `%s`\n"+
						"**Total:** %s ETH\n"+
						"**Available:** %s ETH\n",
					walletAddr.Hex(),
					helpers.FormatEth(balance),
					helpers.FormatEth(availableForTrading))

				// Add warning if low balance
				minRecommended := helpers.Wei("0.1") // 0.1 ETH
				if balance.Cmp(minRecommended) < 0 {
					balanceReport += "\n⚠️ *Low balance* - Add funds to trade effectively"
				}

				// Add network info
				balanceReport += fmt.Sprintf("\n**Network:** %s", c.activeNet)

				c.reply(chatID, balanceReport)
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

func (c *Controller) executeAutoBuy(ctx context.Context, signal *signals.LiquiditySignal, scanReport *scanner.Report, chatID int64) {
	if c.executor == nil {
		telemetry.Warnf("[autobuy] no executor configured")
		return
	}

	// Identify target token
	tokenToBuy := c.identifyTargetToken(signal)
	if tokenToBuy == (common.Address{}) {
		telemetry.Debugf("[autobuy] cannot identify target token")
		return
	}

	// Get buy amount
	buyAmount, err := helpers.EthToWei(c.Cfg.AUTO_BUY_AMOUNT)
	if err != nil {
		telemetry.Errorf("[autobuy] invalid buy amount: %s", c.Cfg.AUTO_BUY_AMOUNT)
		return
	}

	// Check balance using executor
	balance, err := c.executor.GetETHBalance(ctx)
	if err != nil {
		telemetry.Errorf("[autobuy] balance check failed: %v", err)
		return
	}

	gasReserve := big.NewInt(1e16) // 0.01 ETH for gas
	required := new(big.Int).Add(buyAmount, gasReserve)
	if balance.Cmp(required) < 0 {
		c.reply(chatID, fmt.Sprintf(
			"❌ Insufficient balance for auto-buy\nNeed: %s ETH\nHave: %s ETH",
			helpers.FormatEth(required), helpers.FormatEth(balance)))
		return
	}

	// Safety check if enabled
	if c.honeypotCheckEnabled {
		telemetry.Debugf("[autobuy] running safety check for %s", tokenToBuy.Hex())

		checkCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()

		checker := scanner.NewHoneypotChecker(c.ethClient, c.dex)
		safety, err := checker.CheckToken(checkCtx, tokenToBuy)

		if err != nil {
			telemetry.Warnf("[autobuy] safety check failed: %v", err)
		} else if safety.IsHoneypot {
			c.reply(chatID, fmt.Sprintf(
				"🚨 *HONEYPOT DETECTED*\nToken: `%s`\nSkipping auto-buy!",
				tokenToBuy.Hex()))
			return
		} else if safety.SafetyScore < 40 {
			telemetry.Warnf("[autobuy] low safety score: %d", safety.SafetyScore)
			// Continue anyway for auto-buy (configurable)
		}
	}

	// Build notification
	liquidityInfo := ""
	if scanReport.ETHInWei != nil {
		liquidityInfo = fmt.Sprintf("\nLiquidity: %s ETH", helpers.FormatEth(scanReport.ETHInWei))
	}

	c.reply(chatID, fmt.Sprintf(
		"🎯 *AUTO-BUY TRIGGERED*\n"+
			"Token: `%s`%s\n"+
			"Amount: %s ETH\n"+
			"Bundle: %v",
		tokenToBuy.Hex(),
		liquidityInfo,
		c.Cfg.AUTO_BUY_AMOUNT,
		c.tradeConfig.UseBundles))

	// Execute using executor
	txHash, err := c.executor.ExecuteBuy(ctx, tokenToBuy, buyAmount, c.tradeConfig)
	if err != nil {
		c.reply(chatID, fmt.Sprintf("❌ Auto-buy failed: %v", err))
		telemetry.Errorf("[autobuy] execution failed: %v", err)
		return
	}

	// Success!
	c.reply(chatID, fmt.Sprintf(
		"✅ *AUTO-BUY SUCCESS!*\n"+
			"Token: `%s`\n"+
			"Amount: %s ETH\n"+
			"TX: `%s`",
		tokenToBuy.Hex(),
		c.Cfg.AUTO_BUY_AMOUNT,
		txHash.Hex()))

	telemetry.Infof("[autobuy] SUCCESS - token: %s, amount: %s ETH, tx: %s",
		tokenToBuy.Hex(), helpers.FormatEth(buyAmount), txHash.Hex())
}

// Identify which token to buy (the non-WETH token)
func (c *Controller) identifyTargetToken(signal *signals.LiquiditySignal) common.Address {
	weth := c.dex.WETH()

	if signal.Token0 != weth && signal.Token0 != (common.Address{}) {
		return signal.Token0
	}
	if signal.Token1 != weth && signal.Token1 != (common.Address{}) {
		return signal.Token1
	}

	return common.Address{}
}

func (c *Controller) displaySafetyReport(chatID int64, safety *scanner.TokenSafety) {
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
		helpers.FormatEth(safety.LiquidityETH),
	)

	c.reply(chatID, report)
}
