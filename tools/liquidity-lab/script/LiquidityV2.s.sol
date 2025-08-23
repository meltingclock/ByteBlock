// SPDX-License-Identifier: MIT
pragma solidity ^0.8.20;

import "forge-std/Script.sol";
import "forge-std/console2.sol";

interface IUniswapV2Router02 {
    function addLiquidityETH(
        address token,
        uint amountTokenDesired,
        uint amountTokenMin,
        uint amountETHMin,
        address to,
        uint deadline
    ) external payable returns (uint amountToken, uint amountETH, uint liquidity);
}

interface IERC20 {
    function balanceOf(address) external view returns (uint256);
    function allowance(address owner, address spender) external view returns (uint256);
    function approve(address spender, uint256 value) external returns (bool);
    function transfer(address to, uint256 value) external returns (bool);
    function transferFrom(address from, address to, uint256 value) external returns (bool);
}

contract TestToken is IERC20 {
    string public name = "TestToken";
    string public symbol = "TST";
    uint8  public decimals = 18;

    uint256 public totalSupply;
    mapping(address => uint256)                      public balanceOf;
    mapping(address => mapping(address => uint256))  public allowance;

    event Transfer(address indexed from, address indexed to, uint256 value);
    event Approval(address indexed owner, address indexed spender, uint256 value);

    function mint(address to, uint256 amt) external {
        totalSupply += amt;
        balanceOf[to] += amt;
        emit Transfer(address(0), to, amt);
    }

    function approve(address spender, uint256 value) external returns (bool) {
        allowance[msg.sender][spender] = value;
        emit Approval(msg.sender, spender, value);
        return true;
    }

    function transfer(address to, uint256 amt) external returns (bool) {
        require(balanceOf[msg.sender] >= amt, "bal");
        balanceOf[msg.sender] -= amt;
        balanceOf[to] += amt;
        emit Transfer(msg.sender, to, amt);
        return true;
    }

    function transferFrom(address from, address to, uint256 amt) external returns (bool) {
        require(balanceOf[from] >= amt, "bal");
        uint256 a = allowance[from][msg.sender];
        require(a >= amt, "allowance");
        // update state
        allowance[from][msg.sender] = a - amt;
        balanceOf[from] -= amt;
        balanceOf[to] += amt;
        emit Transfer(from, to, amt);
        return true;
    }
}

contract LiquidityV2Script is Script {
    // Mainnet Uniswap V2 (OK for mainnet-fork)
    address constant DEFAULT_ROUTER = 0x7a250d5630B4cF539739dF2C5dAcb4c659F2488D;
    address constant DEFAULT_WETH   = 0xC02aaA39b223FE8D0A0e5C4F27eAD9083C756Cc2; // not used directly here

    function run() external {
        // Robust env reads (avoid vm.envOr overload issue)
        address router          = _envAddressOr("ROUTER",            DEFAULT_ROUTER);
        uint256 ethLiqWei       = _envUintOr   ("ETH_LIQ_WEI",       500_000_000_000_000_000); // 0.5 ETH
        uint256 tknLiqDesired   = _envUintOr   ("TKN_LIQ_DESIRED",   200_000 * 1e18);
        uint256 deadlineSeconds = _envUintOr   ("DEADLINE_SECS",     600);
        bool    useExistingToken= _envBoolOr   ("USE_EXISTING_TOKEN", false);
        address existingToken   = _envAddressOr("TKN",               address(0));

        vm.startBroadcast(); // uses --private-key / PRIVATE_KEY

        // Deploy or reuse token
        address tokenAddr;
        if (useExistingToken) {
            require(existingToken != address(0), "TKN not set");
            tokenAddr = existingToken;
            console2.log("Using existing token:", tokenAddr);
        } else {
            TestToken t = new TestToken();
            tokenAddr = address(t);
            t.mint(msg.sender, 1_000_000 * 1e18);
            console2.log("Deployed TestToken at:", tokenAddr);
        }

        // Approve router and add liquidity (token/ETH)
        IERC20(tokenAddr).approve(router, type(uint256).max);

        uint256 deadline = block.timestamp + deadlineSeconds;
        (uint256 aTok, uint256 aEth, uint256 lp) =
            IUniswapV2Router02(router).addLiquidityETH{ value: ethLiqWei }(
                tokenAddr, tknLiqDesired, 0, 0, msg.sender, deadline
            );

        // safer logging (no mixed overloads)
        console2.log("addLiquidityETH filled");
        console2.log("token (wei)", aTok);
        console2.log("eth   (wei)", aEth);
        console2.log("LP tokens",   lp);

        vm.stopBroadcast();
    }

    // ---------- helpers ----------
    function _envAddressOr(string memory key, address def) internal returns (address) {
        (bool ok, bytes memory data) =
            address(vm).staticcall(abi.encodeWithSignature("envAddress(string)", key));
        if (ok && data.length >= 32) return abi.decode(data, (address));
        return def;
    }
    function _envUintOr(string memory key, uint256 def) internal returns (uint256) {
        (bool ok, bytes memory data) =
            address(vm).staticcall(abi.encodeWithSignature("envUint(string)", key));
        if (ok && data.length >= 32) return abi.decode(data, (uint256));
        return def;
    }
    function _envBoolOr(string memory key, bool def) internal returns (bool) {
        (bool ok, bytes memory data) =
            address(vm).staticcall(abi.encodeWithSignature("envBool(string)", key));
        if (ok && data.length >= 32) return abi.decode(data, (bool));
        return def;
    }
}


