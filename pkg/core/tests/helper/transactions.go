package helper

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"gitlab.dusk.network/dusk-core/dusk-go/pkg/core/transactions"
)

// RandomSliceOfTxs returns a random slice of transactions for testing
func RandomSliceOfTxs(t *testing.T) []transactions.Transaction {
	var txs []transactions.Transaction

	txs = append(txs, RandomCoinBaseTx(t, false))

	for i := 0; i < 10; i++ {

		txs = append(txs, RandomStandardTx(t, false))
		txs = append(txs, RandomTLockTx(t, false))

		stake, err := RandomStakeTx(t, false)
		assert.Nil(t, err)
		txs = append(txs, stake)

		bid, err := RandomBidTx(t, false)
		assert.Nil(t, err)
		txs = append(txs, bid)
	}

	return txs
}

// RandomBidTx returns a random bid transaction for testing
func RandomBidTx(t *testing.T, malformed bool) (*transactions.Bid, error) {

	var numInputs, numOutputs = 23, 34
	var lock uint64 = 20000
	var fee uint64 = 20
	var M = RandomSlice(t, 32)

	if malformed {
		M = RandomSlice(t, 12)
	}

	tx, err := transactions.NewBid(0, lock, fee, M)
	if err != nil {
		return tx, err
	}

	// Inputs
	tx.Inputs = RandomInputs(t, numInputs, malformed)

	// Outputs
	tx.Outputs = RandomOutputs(t, numOutputs, malformed)

	return tx, err
}

// RandomCoinBaseTx returns a random coinbase transaction for testing
func RandomCoinBaseTx(t *testing.T, malformed bool) *transactions.Coinbase {

	proof := RandomSlice(t, 2000)
	key := RandomSlice(t, 32)
	address := RandomSlice(t, 32)

	tx := transactions.NewCoinbase(proof, key, address)

	return tx
}

// RandomTLockTx returns a random timelock transaction for testing
func RandomTLockTx(t *testing.T, malformed bool) *transactions.TimeLock {

	var numInputs, numOutputs = 23, 34
	var lock uint64 = 20000
	var fee uint64 = 20

	tx := transactions.NewTimeLock(0, lock, fee)

	// Inputs
	tx.Inputs = RandomInputs(t, numInputs, malformed)

	// Outputs
	tx.Outputs = RandomOutputs(t, numOutputs, malformed)

	return tx
}

// RandomStandardTx returns a random standard tx for testing
func RandomStandardTx(t *testing.T, malformed bool) *transactions.Standard {

	var numInputs, numOutputs = 10, 10
	var fee uint64 = 20

	tx := transactions.NewStandard(0, fee)

	// Inputs
	tx.Inputs = RandomInputs(t, numInputs, malformed)

	// Outputs
	tx.Outputs = RandomOutputs(t, numOutputs, malformed)

	return tx
}

// RandomStakeTx returns a random stake tx for testing
func RandomStakeTx(t *testing.T, malformed bool) (*transactions.Stake, error) {

	var numInputs, numOutputs = 23, 34
	var lock uint64 = 20000
	var fee uint64 = 20

	edKey := RandomSlice(t, 32)
	blsKey := RandomSlice(t, 33)

	tx, err := transactions.NewStake(0, lock, fee, edKey, blsKey)
	if err != nil {
		return tx, err
	}

	// Inputs
	tx.Inputs = RandomInputs(t, numInputs, malformed)

	// Outputs
	tx.Outputs = RandomOutputs(t, numOutputs, malformed)

	return tx, nil
}