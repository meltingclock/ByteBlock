# Biteblock mempool test with Anvil

This shows how to run the bot against a local Anvil node and verify it captures **pending** transactions.

## 1) Start Anvil with block interval (to keep tx pending briefly)

```bash
anvil --block-time 5 --host 127.0.0.1 --port 8545
# WS is exposed at ws://127.0.0.1:8545
```

- `--block-time 5` produces a new block every 5 seconds, giving a small pending window.
- Alternatively, you can use `--block-time 0` and create pending by **sending multiple tx** in a burst or by using a lower gas tip than the current base + tip (more brittle locally).

## 2) Point the bot to Anvil

Set `WSS_URL`:
```yaml
WSS_URL: ws://127.0.0.1:8545
DEBUG: true
```

Run:
```bash
go run ./cmd/sniper
```

## 3) Generate pending transactions

Open a second terminal and use Foundry `cast` (or your own script) to send some tx:

### Option A: Simple ETH transfers (legacy)
```bash
cast rpc anvil_setBalance 0xf39fd6e51aad88f6f4ce6ab8827279cfffb92266 0x21e19e0c9bab2400000
cast send --from 0xf39fd6e51aad88f6f4ce6ab8827279cfffb92266 \
  0x1000000000000000000000000000000000000000 --value 0.01ether --legacy --gas-price 1
```

### Option B: This command works
```bash
export ETH_RPC_URL=http://127.0.0.1:8545
cast send \
  --from 0xf39fd6e51aad88f6f4ce6ab8827279cfffb92266 \
  --unlocked \
  0x1000000000000000000000000000000000000000 \
  --value 0.01ether
```

### Option C: Burst a few tx quickly
```bash
for i in $(seq 1 10); do
  cast send --from 0xf39fd6e51aad88f6f4ce6ab8827279cfffb92266 \
    0x1000000000000000000000000000000000000000 --value 0.001ether --legacy --gas-price 1 &
done
wait
```

Because the block time is 5s, these tx will appear as **pending** before the next block is mined. The bot should log lines like:
```
[ev=newPendingTx] 0x... nonce=... val=... gas=...
```

### Option D: Send a tx with a low tip so it waits longer (EIP-1559)
```bash
cast send --from 0xf39fd6e51aad88f6f4ce6ab8827279cfffb92266 \
  0x1000000000000000000000000000000000000000 --value 0.001ether \
  --gas-limit 21000 --max-fee-per-gas 5gwei --max-priority-fee-per-gas 1gwei
```

## 4) Notes
- If you want even more pending time, increase `--block-time` to 10–15 seconds.
- If you want to mine manually for deterministic tests, you can set a very large block time and call:
  ```bash
  cast rpc evm_mine
  ```
  to seal a block on demand.

## 5) Anvil test Mainnet fork 
- Start Anvil (Mainnet fork) with a block timer 
```bash
export MAINNET_RPC="https://YOUR_MAINNET_RPC"
anvil --fork-url "$MAINNET_RPC" --block-time 6 --chain-id 31337
```

- --block-time 6 mines a block every 6s → your mempool watcher gets a pending window.
- Anvil prints 10 funded accounts. Copy Account #0’s private key and address; we’ll use it.

### Configure watcher to Anvil
- RPC/WSS: ws://127.0.0.1:8545
- Factory (Uniswap V2): 0x5C69bEe701ef814a2B6a3EDD4B1652CB9cc5aA6f
- Router (Uniswap V2): 0x7a250d5630B4cF539739dF2C5dAcb4c659F2488D
- start the bot 
...
[signals] PairRegistry subscribing PairCreated
[mempool] subscribed to newPendingTransactions

- These are mainnet addresses, valid on the fork. No deployment needed.

### Create a tiny ERC20 token to test 

```bash
forge init tkn && cd tkn
cat > src/TestToken.sol <<'EOF'
// SPDX-License-Identifier: MIT
pragma solidity ^0.8.20;

contract TestToken {
    string public name = "TestToken";
    string public symbol = "TST";
    uint8  public decimals = 18;
    mapping(address => uint256) public balanceOf;
    mapping(address => mapping(address => uint256)) public allowance;

    function mint(address to, uint256 amount) external { balanceOf[to] += amount; }
    function approve(address spender, uint256 amount) external returns (bool) {
        allowance[msg.sender][spender] = amount; return true;
    }
    function transfer(address to, uint256 amount) external returns (bool) {
        require(balanceOf[msg.sender] >= amount, "bal");
        balanceOf[msg.sender] -= amount; balanceOf[to] += amount; return true;
    }
    function transferFrom(address from, address to, uint256 amount) external returns (bool) {
        require(balanceOf[from] >= amount, "bal");
        uint256 a = allowance[from][msg.sender]; require(a >= amount, "allow");
        if (a != type(uint256).max) allowance[from][msg.sender] = a - amount;
        balanceOf[from] -= amount; balanceOf[to] += amount; return true;
    }
}
EOF
forge build
```
### Deploy token to Anvil fork 

```bash
export RPC_URL=http://127.0.0.1:8545
export PK=<ANVIL_ACCOUNT0_PRIVATE_KEY>
export ME=<ANVIL_ACCOUNT0_ADDRESS>

forge create --rpc-url $RPC_URL --broadcast --private-key $PK src/TestToken.sol:TestToken
```
- Output: Deployer: 0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266
          Deployed to: 0xA96aA8D425260B5514300a0b640313b4F727B0CE
          Transaction hash: 0xaf777e86dc7d226e73f7144cf8f5e9a0d70088cac3498282ddcfc226b899534f  
          
- Save the token address
- Approve router
- Mint yourself tokens:

```bash
export TKN=0xYourTokenAddress
export ROUTER=0x7a250d5630B4cF539739dF2C5dAcb4c659F2488D
cast send "$TKN" "approve(address,uint256)" "$ROUTER" "1000000e18"   --rpc-url "$RPC_URL" --private-key "$PK"

or

cast send "$TKN" "approve(address,uint256)" "$ROUTER" 1000000000000000000000000 \
  --rpc-url "$RPC_URL" --private-key "$PK"

cast send "$TKN" "mint(address,uint256)" "$ME" 1000000ether \
  --rpc-url "$RPC_URL" --private-key "$PK"
```

- AddLiquidity 

```bash
export DEADLINE=$(($(date +%s) + 1800))
cast send "$ROUTER" \
 "addLiquidityETH(address,uint256,uint256,uint256,address,uint256)" \
 "$TKN" 100000ether 0 0 "$ME" "$DEADLINE" \
 --value 10ether \
 --rpc-url "$RPC_URL" \
 --private-key "$PK"
```

