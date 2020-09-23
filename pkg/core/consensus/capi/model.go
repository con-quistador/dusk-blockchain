package capi

import (
	"github.com/dusk-network/dusk-blockchain/pkg/util/nativeutils/sortedset"
	"time"

	"github.com/dusk-network/dusk-blockchain/pkg/p2p/wire/message"
)

// EventQueueJSON is used as JSON rapper for eventQueue fields
type EventQueueJSON struct {
	ID        int              `storm:"id,increment"` // primary key with auto increment
	Round     uint64           `json:"round"`
	Step      uint8            `json:"step"`
	Message   *message.Message `json:"message"`
	UpdatedAt time.Time        `json:"updated_at"`
}

// RoundInfoJSON is used as JSON wrapper for round info fields
type RoundInfoJSON struct {
	ID        uint64    `storm:"id" json:"round"`
	Step      uint8     `json:"step"`
	UpdatedAt time.Time `json:"updated_at"`
	Method    string    `json:"method"`
	Name      string    `json:"name"`
}

type PeerJSON struct {
	ID       string    `storm:"id"`
	LastSeen time.Time `storm:"index"`
}

type Member struct {
	PublicKeyBLS []byte  `json:"bls_key"`
	Stakes       []Stake `json:"stakes"`
}

// Stake represents the Provisioner's stake
type Stake struct {
	Amount      uint64 `json:"amount"`
	StartHeight uint64 `json:"start_height"`
	EndHeight   uint64 `json:"end_height"`
}

type ProvisionerJSON struct {
	ID      uint64        `storm:"id" json:"id"`
	Set     sortedset.Set `json:"set"`
	Members []*Member     `json:"members"`
}
