// This Source Code Form is subject to the terms of the MIT License.
// If a copy of the MIT License was not distributed with this
// file, you can obtain one at https://opensource.org/licenses/MIT.
//
// Copyright (c) DUSK NETWORK. All rights reserved.

package genesis

import (
	"github.com/dusk-network/dusk-blockchain/pkg/config"
	"github.com/dusk-network/dusk-blockchain/pkg/core/data/block"
	"github.com/dusk-network/dusk-blockchain/pkg/core/data/ipc/transactions"
)

// Generate a genesis block. The constitution of the block depends on the passed
// config.
func Generate(c Config) *block.Block {
	// TODO: Populate this with real txs data from Rusk Transfer and Stake Contract
	if c.Transactions == nil {
		c.Transactions = make([]transactions.ContractCall, 0)
		c.Transactions = append(c.Transactions, transactions.MockTx())
	}

	h := &block.Header{
		Version:       0,
		Timestamp:     c.timestamp,
		Height:        0,
		PrevBlockHash: make([]byte, 32),
		TxRoot:        nil,
		Seed:          c.seed,
		Certificate:   block.EmptyCertificate(),
		StateHash:     make([]byte, 32),
	}

	b := &block.Block{
		Header: h,
		Txs:    c.Transactions,
	}

	// Set root and hash, since they have changed because of the adding of txs.
	root, err := b.CalculateRoot()
	if err != nil {
		panic(err)
	}

	b.Header.TxRoot = root

	hash, err := b.CalculateHash()
	if err != nil {
		panic(err)
	}

	b.Header.Hash = hash
	return b
}

// Decode marshals a genesis block into a buffer.
func Decode() *block.Block {
	cfg, err := GetPresetConfig(config.Get().General.Network)
	if err != nil {
		panic(err)
	}

	return Generate(cfg)
}
