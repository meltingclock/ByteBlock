# Make this Makefile robust no matter where you invoke `make` from
MAKEFILE_DIR := $(dir $(abspath $(lastword $(MAKEFILE_LIST))))
ENVFILE      := $(MAKEFILE_DIR).env

# anvil: forks mainnet if MAINNET_RPC is set, else warns and starts plain chain
anvil:
	@echo "Starting anvil (2s blocks)…"
	@sh -c ' \
	  if [ -f "$(ENVFILE)" ]; then . "$(ENVFILE)"; fi; \
	  if [ -n "$$MAINNET_RPC" ]; then \
	    echo "[anvil] forking: $$MAINNET_RPC"; \
	    exec anvil --fork-url "$$MAINNET_RPC" --block-time 2 --chain-id 1; \
	  else \
	    echo "[anvil] WARNING: MAINNET_RPC not set -> starting PLAIN anvil (no Uniswap at 0x7a25...)"; \
	    exec anvil --block-time 2 --chain-id 1; \
	  fi \
	'

# optional explicit targets
anvil-fork:
	@echo "Starting anvil (2s blocks)…"
	@sh -c ' \
	  if [ -f "$(ENVFILE)" ]; then . "$(ENVFILE)"; fi; \
	  : $${MAINNET_RPC:?Set MAINNET_RPC in .env or env}; \
	  exec anvil --fork-url "$$MAINNET_RPC" --block-time 2 --chain-id 1 \
	'

anvil-local:
	@echo "Starting anvil (2s blocks)…"
	@anvil --block-time 2 --chain-id 1

deps:
	forge install foundry-rs/forge-std --no-commit

build:
	forge build -vvv

pass:
	@sh -c ' \
	  echo "[make] cwd=$$PWD"; \
	  echo "[make] ENVFILE=$(ENVFILE)"; \
	  if [ -f "$(ENVFILE)" ]; then . "$(ENVFILE)"; else echo "[make] .env not found at $(ENVFILE)"; exit 2; fi; \
	  : $${RPC_URL:?RPC_URL is required}; \
	  : $${PRIVATE_KEY:?PRIVATE_KEY is required}; \
	  ETH_LIQ_WEI=$${ETH_LIQ_WEI:-500000000000000000} \
	  forge script script/LiquidityV2.s.sol:LiquidityV2Script \
	    --rpc-url "$$RPC_URL" --private-key "$$PRIVATE_KEY" --broadcast -vvvv \
	'

reject:
	@sh -c ' \
	  if [ -f "$(ENVFILE)" ]; then . "$(ENVFILE)"; else echo "[make] .env not found at $(ENVFILE)"; exit 2; fi; \
	  : $${RPC_URL:?RPC_URL is required}; \
	  : $${PRIVATE_KEY:?PRIVATE_KEY is required}; \
	  ETH_LIQ_WEI=50000000000000000 \
	  forge script script/LiquidityV2.s.sol:LiquidityV2Script \
	    --rpc-url "$$RPC_URL" --private-key "$$PRIVATE_KEY" --broadcast -vvvv \
	'

loop-mix:
	@sh -c ' \
	  if [ -f "$(ENVFILE)" ]; then . "$(ENVFILE)"; else echo "[make] .env not found at $(ENVFILE)"; exit 2; fi; \
	  : $${RPC_URL:?RPC_URL is required}; \
	  : $${PRIVATE_KEY:?PRIVATE_KEY is required}; \
	  while true; do \
	    echo "== PASS =="; \
	    ETH_LIQ_WEI=500000000000000000 \
	    forge script script/LiquidityV2.s.sol:LiquidityV2Script \
	      --rpc-url "$$RPC_URL" --private-key "$$PRIVATE_KEY" --broadcast >/dev/null 2>&1 || true; \
	    sleep 4; \
	    echo "== REJECT =="; \
	    ETH_LIQ_WEI=50000000000000000 \
	    forge script script/LiquidityV2.s.sol:LiquidityV2Script \
	      --rpc-url "$$RPC_URL" --private-key "$$PRIVATE_KEY" --broadcast >/dev/null 2>&1 || true; \
	    sleep 4; \
	  done \
	'

