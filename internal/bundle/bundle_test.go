package bundle_test

import (
	"context"
	"crypto/ecdsa"
	"fmt"
	"log"
	"math/big"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"

	"github.com/meltingclock/biteblock_v1/internal/bundle"
)

// TestBundleOnAnvil tests bundle functionality on local Anvil
func TestBundleOnAnvil(t *testing.T) {
	// Skip if not explicitly enabled
	if testing.Short() {
		t.Skip("Skipping Anvil integration test")
	}

	client, err := ethclient.Dial("http://127.0.0.1:8545")
	if err != nil {
		t.Skipf("Anvil not running: %v", err)
	}

	// Use Anvil's first account
	privateKey, err := crypto.HexToECDSA("ac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80")
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	chainID, _ := client.ChainID(ctx)

	// Create bundler
	bundler, err := bundle.NewBundler(client, privateKey, chainID)
	if err != nil {
		t.Fatal(err)
	}

	// Test CreateSniperBundle
	t.Run("CreateSniperBundle", func(t *testing.T) {
		// Create a test swap transaction
		address := crypto.PubkeyToAddress(privateKey.PublicKey)
		nonce, _ := client.PendingNonceAt(ctx, address)
		gasPrice, _ := client.SuggestGasPrice(ctx)

		swapTx := types.NewTransaction(
			nonce,
			common.HexToAddress("0x7a250d5630B4cF539739dF2C5dAcb4c659F2488D"), // Mock router
			big.NewInt(1e17), // 0.1 ETH
			300000,
			gasPrice,
			[]byte{0x12, 0x34}, // Mock calldata
		)

		signedSwap, _ := types.SignTx(swapTx, types.NewEIP155Signer(chainID), privateKey)

		// Create sniper bundle with bribe
		bribeAmount := big.NewInt(1e15) // 0.001 ETH
		sniperBundle, err := bundler.CreateSniperBundle(ctx, signedSwap, bribeAmount)
		if err != nil {
			t.Fatalf("Failed to create sniper bundle: %v", err)
		}

		// Verify bundle structure
		if len(sniperBundle.Transactions) != 2 {
			t.Errorf("Expected 2 transactions in bundle, got %d", len(sniperBundle.Transactions))
		}

		// Verify bribe transaction
		if len(sniperBundle.Transactions) == 2 {
			bribeTx := sniperBundle.Transactions[1]
			if bribeTx.Value().Cmp(bribeAmount) != 0 {
				t.Errorf("Bribe amount mismatch: expected %s, got %s",
					bribeAmount.String(), bribeTx.Value().String())
			}
		}

		t.Logf("Created sniper bundle with %d transactions", len(sniperBundle.Transactions))
	})

	// Test bundle statistics
	t.Run("GetStats", func(t *testing.T) {
		stats := bundler.GetStats()
		if stats == nil {
			t.Error("GetStats returned nil")
		}
		t.Logf("Bundler stats: %+v", stats)
	})

	// Test simulation (will fail on Anvil but tests the error handling)
	t.Run("SimulateBundle", func(t *testing.T) {
		testBundle := createTestBundle(t, client, privateKey, chainID)

		// This will fail since Anvil doesn't have builders, but we test the flow
		sim, err := bundler.SimulateBundle(ctx, testBundle)
		if err == nil && sim != nil {
			t.Logf("Unexpected simulation success on Anvil")
		} else {
			t.Logf("Expected simulation failure on Anvil: %v", err)
		}
	})

	// Test mock simulation and sending
	t.Run("MockBundleFlow", func(t *testing.T) {
		testBundle := createTestBundle(t, client, privateKey, chainID)

		// Mock simulation
		sim := mockSimulateBundle(ctx, client, privateKey, testBundle)
		if !sim.Success {
			t.Errorf("Mock simulation failed: %s", sim.Error)
		}
		t.Logf("Mock simulation success - Gas: %d, Profit: %s", sim.TotalGasUsed, sim.CoinbaseDiff)

		// Mock send
		err := mockSendBundle(ctx, client, testBundle)
		if err != nil {
			t.Errorf("Mock send failed: %v", err)
		}
	})
}

func createTestBundle(t *testing.T, client *ethclient.Client, privateKey *ecdsa.PrivateKey, chainID *big.Int) *bundle.Bundle {
	ctx := context.Background()

	fromAddr := crypto.PubkeyToAddress(privateKey.PublicKey)
	nonce, _ := client.PendingNonceAt(ctx, fromAddr)
	gasPrice, _ := client.SuggestGasPrice(ctx)

	// Main transaction
	tx1 := types.NewTransaction(
		nonce,
		common.HexToAddress("0x742d35Cc6634C0532925a3b844Bc9e7595f0bEb0"),
		big.NewInt(1e15), // 0.001 ETH
		21000,
		gasPrice,
		nil,
	)
	signedTx1, _ := types.SignTx(tx1, types.NewEIP155Signer(chainID), privateKey)

	// Bribe transaction
	tx2 := types.NewTransaction(
		nonce+1,
		common.HexToAddress("0x0000000000000000000000000000000000000001"),
		big.NewInt(1e14), // 0.0001 ETH
		21000,
		gasPrice,
		nil,
	)
	signedTx2, _ := types.SignTx(tx2, types.NewEIP155Signer(chainID), privateKey)

	return &bundle.Bundle{
		Transactions: []*types.Transaction{signedTx1, signedTx2},
	}
}

func mockSimulateBundle(ctx context.Context, client *ethclient.Client, privateKey *ecdsa.PrivateKey, b *bundle.Bundle) *bundle.SimulationResponse {
	var totalGas uint64

	for _, tx := range b.Transactions {
		msg := ethereum.CallMsg{
			From:     crypto.PubkeyToAddress(privateKey.PublicKey),
			To:       tx.To(),
			Value:    tx.Value(),
			Data:     tx.Data(),
			GasPrice: tx.GasPrice(),
		}

		gas, err := client.EstimateGas(ctx, msg)
		if err != nil {
			return &bundle.SimulationResponse{
				Success: false,
				Error:   fmt.Sprintf("estimation failed: %v", err),
			}
		}
		totalGas += gas
	}

	profit := "0"
	if len(b.Transactions) > 1 {
		lastTx := b.Transactions[len(b.Transactions)-1]
		if lastTx.Value() != nil {
			profit = lastTx.Value().String()
		}
	}

	return &bundle.SimulationResponse{
		Success:      true,
		TotalGasUsed: totalGas,
		CoinbaseDiff: profit,
	}
}

func mockSendBundle(ctx context.Context, client *ethclient.Client, b *bundle.Bundle) error {
	for i, tx := range b.Transactions {
		err := client.SendTransaction(ctx, tx)
		if err != nil {
			return fmt.Errorf("tx %d failed: %w", i, err)
		}
		log.Printf("Sent tx %d: %s", i, tx.Hash().Hex())
	}
	time.Sleep(2 * time.Second) // Wait for mining
	return nil
}
