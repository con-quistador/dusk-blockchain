package factory

import (
	"time"

	"github.com/dusk-network/dusk-blockchain/pkg/core/consensus"
	"github.com/dusk-network/dusk-blockchain/pkg/core/consensus/agreement"
	"github.com/dusk-network/dusk-blockchain/pkg/core/consensus/reduction"
	"github.com/dusk-network/dusk-blockchain/pkg/core/consensus/selection"
	"github.com/dusk-network/dusk-blockchain/pkg/util/nativeutils/eventbus"
	"github.com/dusk-network/dusk-blockchain/pkg/util/nativeutils/rpcbus"
	"github.com/dusk-network/dusk-wallet/key"
	log "github.com/sirupsen/logrus"
)

// ConsensusFactory is responsible for initializing the consensus processes
// with the proper parameters. It subscribes to the initialization topic and,
// upon reception of a message, will start all of the components related to
// consensus. It should also contain all the relevant information for the
// processes it intends to start up.
type ConsensusFactory struct {
	eventBus *eventbus.EventBus
	rpcBus   *rpcbus.RPCBus

	key.ConsensusKeys
	timerLength time.Duration
}

// New returns an initialized ConsensusFactory.
func New(eventBus *eventbus.EventBus, rpcBus *rpcbus.RPCBus, timerLength time.Duration, keys key.ConsensusKeys) *ConsensusFactory {
	return &ConsensusFactory{
		eventBus:      eventBus,
		rpcBus:        rpcBus,
		ConsensusKeys: keys,
		timerLength:   timerLength,
	}
}

// StartConsensus will wait for a message to come in, and then proceed to
// start the consensus components.
func (c *ConsensusFactory) StartConsensus() {
	log.WithField("process", "factory").Info("Starting consensus")
	sel := selection.NewFactory(c.eventBus, c.timerLength)
	red := reduction.NewFactory(c.eventBus, c.rpcBus, c.ConsensusKeys, c.timerLength)
	agr := agreement.NewFactory(c.eventBus, c.ConsensusKeys)

	consensus.Start(c.eventBus, c.ConsensusKeys, sel, red, agr)
	log.WithField("process", "factory").Info("Consensus Started")
}
