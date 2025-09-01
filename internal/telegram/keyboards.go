package telegram

import (
	"fmt"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// MainMenuKeyboard with dynamic status indicators
func MainMenuKeyboard(autoBuyEnabled, safetyEnabled, isRunning bool) tgbotapi.InlineKeyboardMarkup {
	autoBuyText := "🔴 Auto-Buy OFF"
	if autoBuyEnabled {
		autoBuyText = "🟢 Auto-Buy ON"
	}

	safetyText := "🔴 Safety OFF"
	if safetyEnabled {
		safetyText = "🟢 Safety ON"
	}

	runningText := "▶️ Start Bot"
	if isRunning {
		runningText = "⏸️ Stop Bot"
	}

	return tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("💰 Positions", "menu_positions"),
			tgbotapi.NewInlineKeyboardButtonData("📊 Status", "status"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData(autoBuyText, "toggle_autobuy"),
			tgbotapi.NewInlineKeyboardButtonData(safetyText, "toggle_safety"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("⚙️ Settings", "menu_settings"),
			tgbotapi.NewInlineKeyboardButtonData("💳 Wallet", "wallet"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData(runningText, "toggle_bot"),
			tgbotapi.NewInlineKeyboardButtonData("🔄 Refresh", "menu_main"),
		),
	)
}

// SettingsKeyboard for quick parameter changes
func SettingsKeyboard(currentNetwork string) tgbotapi.InlineKeyboardMarkup {
	return tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("💰 Buy Amount", "set_buyamount"),
			tgbotapi.NewInlineKeyboardButtonData("⛽ Gas Settings", "set_gas"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("💧 Min Liquidity", "set_minliq"),
			tgbotapi.NewInlineKeyboardButtonData("📉 Slippage", "set_slippage"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData(fmt.Sprintf("🌐 Network: %s", currentNetwork), "set_network"),
			tgbotapi.NewInlineKeyboardButtonData("📦 Bundles", "set_bundles"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🔑 Trusted Tokens", "manage_trusted"),
			tgbotapi.NewInlineKeyboardButtonData("« Back", "menu_main"),
		),
	)
}

// QuickAmountKeyboard for setting values
func QuickAmountKeyboard(settingType string) tgbotapi.InlineKeyboardMarkup {
	switch settingType {
	case "buyamount":
		return tgbotapi.NewInlineKeyboardMarkup(
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("0.05 ETH", "setval_buyamount_0.05"),
				tgbotapi.NewInlineKeyboardButtonData("0.1 ETH", "setval_buyamount_0.1"),
				tgbotapi.NewInlineKeyboardButtonData("0.25 ETH", "setval_buyamount_0.25"),
			),
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("0.5 ETH", "setval_buyamount_0.5"),
				tgbotapi.NewInlineKeyboardButtonData("1.0 ETH", "setval_buyamount_1"),
				tgbotapi.NewInlineKeyboardButtonData("2.0 ETH", "setval_buyamount_2"),
			),
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("« Back", "menu_settings"),
			),
		)
	case "gas":
		return tgbotapi.NewInlineKeyboardMarkup(
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("30 gwei", "setval_gas_30"),
				tgbotapi.NewInlineKeyboardButtonData("50 gwei", "setval_gas_50"),
				tgbotapi.NewInlineKeyboardButtonData("75 gwei", "setval_gas_75"),
			),
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("100 gwei", "setval_gas_100"),
				tgbotapi.NewInlineKeyboardButtonData("150 gwei", "setval_gas_150"),
				tgbotapi.NewInlineKeyboardButtonData("200 gwei", "setval_gas_200"),
			),
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("« Back", "menu_settings"),
			),
		)
	case "slippage":
		return tgbotapi.NewInlineKeyboardMarkup(
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("5%", "setval_slippage_5"),
				tgbotapi.NewInlineKeyboardButtonData("10%", "setval_slippage_10"),
				tgbotapi.NewInlineKeyboardButtonData("15%", "setval_slippage_15"),
			),
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("20%", "setval_slippage_20"),
				tgbotapi.NewInlineKeyboardButtonData("30%", "setval_slippage_30"),
				tgbotapi.NewInlineKeyboardButtonData("50%", "setval_slippage_50"),
			),
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("« Back", "menu_settings"),
			),
		)
	case "minliq":
		return tgbotapi.NewInlineKeyboardMarkup(
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("0.5 ETH", "setval_minliq_0.5"),
				tgbotapi.NewInlineKeyboardButtonData("1 ETH", "setval_minliq_1"),
				tgbotapi.NewInlineKeyboardButtonData("2 ETH", "setval_minliq_2"),
			),
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("5 ETH", "setval_minliq_5"),
				tgbotapi.NewInlineKeyboardButtonData("10 ETH", "setval_minliq_10"),
				tgbotapi.NewInlineKeyboardButtonData("20 ETH", "setval_minliq_20"),
			),
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("« Back", "menu_settings"),
			),
		)
	default:
		return tgbotapi.NewInlineKeyboardMarkup()
	}
}

// NetworkKeyboard for network selection
func NetworkKeyboard() tgbotapi.InlineKeyboardMarkup {
	return tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🔷 Ethereum", "setval_network_ethereum"),
			tgbotapi.NewInlineKeyboardButtonData("🟡 BSC", "setval_network_bsc"),
			tgbotapi.NewInlineKeyboardButtonData("🔵 Base", "setval_network_base"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("« Back", "menu_settings"),
		),
	)
}

// Quickbuykeyboard returns preset buy amounts
func QuickbuyKeyboard(tokenAddress string) tgbotapi.InlineKeyboardMarkup {
	return tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("0.1 ETH", fmt.Sprintf("buy_%s_0.1", tokenAddress)),
			tgbotapi.NewInlineKeyboardButtonData("0.5 ETH", fmt.Sprintf("buy_%s_0.5", tokenAddress)),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("1.0 ETH", fmt.Sprintf("buy_%s_1", tokenAddress)),
			tgbotapi.NewInlineKeyboardButtonData("2.0 ETH", fmt.Sprintf("buy_%s_2", tokenAddress)),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("✏️ Custom Amount", fmt.Sprintf("buy_custom_%s", tokenAddress)),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🔍 Check Safety", fmt.Sprintf("check_%s", tokenAddress)),
			tgbotapi.NewInlineKeyboardButtonData("« Back", "menu_main"),
		),
	)
}

// QuickSellKeyboard returns preset sell percentages for a position
func QuickSellKeyboard(tokenAddress string) tgbotapi.InlineKeyboardMarkup {
	return tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("25%", fmt.Sprintf("sell_%s_25", tokenAddress)),
			tgbotapi.NewInlineKeyboardButtonData("50%", fmt.Sprintf("sell_%s_50", tokenAddress)),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("75%", fmt.Sprintf("sell_%s_75", tokenAddress)),
			tgbotapi.NewInlineKeyboardButtonData("100%", fmt.Sprintf("sell_%s_100", tokenAddress)),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("« Back", "menu_positions"),
		),
	)
}

// PositionCardKeyboard returns action buttons for a specific position
func PositionCardKeyboard(tokenAddress string) tgbotapi.InlineKeyboardMarkup {
	return tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("📈 Sell", fmt.Sprintf("position_sell_%s", tokenAddress)),
			tgbotapi.NewInlineKeyboardButtonData("📊 Chart", fmt.Sprintf("position_chart_%s", tokenAddress)),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🎯 Buy More", fmt.Sprintf("position_buy_%s", tokenAddress)),
			tgbotapi.NewInlineKeyboardButtonData("⚡ Set SL/TP", fmt.Sprintf("position_sltp_%s", tokenAddress)),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🔄 Refresh", fmt.Sprintf("position_refresh_%s", tokenAddress)),
			tgbotapi.NewInlineKeyboardButtonData("« Back", "menu_positions"),
		),
	)
}

// ConfirmationKeyboard for yes/no actions
func ConfirmationKeyboard(action string) tgbotapi.InlineKeyboardMarkup {
	return tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("✅ Confirm", fmt.Sprintf("confirm_%s", action)),
			tgbotapi.NewInlineKeyboardButtonData("❌ Cancel", "cancel"),
		),
	)
}

// LiquidityAlertKeyboard for when liquidity is detected
func LiquidityAlertKeyboard(tokenAddress string) tgbotapi.InlineKeyboardMarkup {
	return tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🚀 Buy 0.1 ETH", fmt.Sprintf("instant_buy_%s_0.1", tokenAddress)),
			tgbotapi.NewInlineKeyboardButtonData("🚀 Buy 0.5 ETH", fmt.Sprintf("instant_buy_%s_0.5", tokenAddress)),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🎯 Custom Buy", fmt.Sprintf("buy_custom_%s", tokenAddress)),
			tgbotapi.NewInlineKeyboardButtonData("🔍 Check Safety", fmt.Sprintf("check_%s", tokenAddress)),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("📊 View on DexScreener", fmt.Sprintf("https://dexscreener.com/ethereum/%s", tokenAddress)),
		),
	)
}
