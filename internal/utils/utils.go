package utils

import (
	"log"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

func SendTxVerifier(tx *types.Transaction) (common.Address, error) {
	if tx == nil {
		log.Println("tx is nil")
		return common.Address{}, nil
	}
	signer, err := types.Sender(types.LatestSignerForChainID(tx.ChainId()), tx)
	return signer, err
}