// This Source Code Form is subject to the terms of the MIT License.
// If a copy of the MIT License was not distributed with this
// file, you can obtain one at https://opensource.org/licenses/MIT.
//
// Copyright (c) DUSK NETWORK. All rights reserved.

package chain

import (
	"bytes"
	"context"
	"errors"
	"sync"

	"github.com/dusk-network/dusk-blockchain/pkg/config"
	"github.com/dusk-network/dusk-blockchain/pkg/core/consensus"
	"github.com/dusk-network/dusk-blockchain/pkg/core/consensus/capi"
	"github.com/dusk-network/dusk-blockchain/pkg/core/consensus/user"
	"github.com/dusk-network/dusk-blockchain/pkg/core/data/block"
	"github.com/dusk-network/dusk-blockchain/pkg/core/data/ipc/transactions"
	"github.com/dusk-network/dusk-blockchain/pkg/core/database"
	"github.com/dusk-network/dusk-blockchain/pkg/core/loop"
	"github.com/dusk-network/dusk-blockchain/pkg/core/verifiers"
	"github.com/dusk-network/dusk-blockchain/pkg/p2p/wire/message"
	"github.com/dusk-network/dusk-blockchain/pkg/p2p/wire/topics"
	"github.com/dusk-network/dusk-blockchain/pkg/util"
	"github.com/dusk-network/dusk-blockchain/pkg/util/diagnostics"
	"github.com/dusk-network/dusk-blockchain/pkg/util/nativeutils/eventbus"
	"github.com/dusk-network/dusk-blockchain/pkg/util/nativeutils/rpcbus"
	"github.com/dusk-network/dusk-protobuf/autogen/go/node"
	"github.com/sirupsen/logrus"
	logger "github.com/sirupsen/logrus"
	"google.golang.org/grpc"
)

var (
	errInvalidStateHash = errors.New("invalid state hash")
	log                 = logger.WithFields(logger.Fields{"process": "chain"})
)

// ErrBlockAlreadyAccepted block already known by blockchain state.
var ErrBlockAlreadyAccepted = errors.New("discarded block from the past")

// TODO: This Verifier/Loader interface needs to be re-evaluated and most likely
// renamed. They don't make too much sense on their own (the `Loader` also
// appends blocks, and allows for fetching data from the DB), and potentially
// cause some clutter in the structure of the `Chain`.

// Verifier performs checks on the blockchain and potentially new incoming block.
type Verifier interface {
	// PerformSanityCheck on first N blocks and M last blocks.
	PerformSanityCheck(startAt uint64, firstBlocksAmount uint64, lastBlockAmount uint64) error
	// SanityCheckBlock will verify whether a block is valid according to the rules of the consensus.
	SanityCheckBlock(prevBlock block.Block, blk block.Block) error
}

// Loader is an interface which abstracts away the storage used by the Chain to
// store the blockchain.
type Loader interface {
	// LoadTip of the chain.
	LoadTip() (*block.Block, error)
	// Clear removes everything from the DB.
	Clear() error
	// Close the Loader and finalizes any pending connection.
	Close(driver string) error
	// Height returns the current height as stored in the loader.
	Height() (uint64, error)
	// BlockAt returns the block at a given height.
	BlockAt(uint64) (block.Block, error)
	// Append a block on the storage.
	Append(*block.Block) error
}

// Chain represents the nodes blockchain.
// This struct will be aware of the current state of the node.
type Chain struct {
	eventBus *eventbus.EventBus
	rpcBus   *rpcbus.RPCBus
	db       database.DB

	// loader abstracts away the persistence aspect of Block operations.
	loader Loader

	// verifier performs verifications on the block.
	verifier Verifier

	// current blockchain tip of local state.
	lock sync.RWMutex
	tip  *block.Block

	// Current set of provisioners.
	p *user.Provisioners

	// Consensus loop.
	loop              *loop.Consensus
	stopConsensusChan chan struct{}
	loopID            uint64

	// Syncing related things.
	*synchronizer
	highestSeen uint64

	// rusk client.
	proxy transactions.Proxy

	ctx context.Context
}

// New returns a new chain object. It accepts the EventBus (for messages coming
// from (remote) consensus components.
func New(ctx context.Context, db database.DB, eventBus *eventbus.EventBus, rpcBus *rpcbus.RPCBus,
	loader Loader, verifier Verifier, srv *grpc.Server, proxy transactions.Proxy, loop *loop.Consensus) (*Chain, error) {
	chain := &Chain{
		eventBus:          eventBus,
		rpcBus:            rpcBus,
		db:                db,
		loader:            loader,
		verifier:          verifier,
		proxy:             proxy,
		ctx:               ctx,
		loop:              loop,
		stopConsensusChan: make(chan struct{}),
	}

	chain.synchronizer = newSynchronizer(db, chain)

	provisioners, err := proxy.Executor().GetProvisioners(ctx)
	if err != nil {
		log.WithError(err).Error("Error in getting provisioners")
		return nil, err
	}

	chain.p = &provisioners

	prevBlock, err := loader.LoadTip()
	if err != nil {
		return nil, err
	}

	chain.tip = prevBlock

	if srv != nil {
		node.RegisterChainServer(srv, chain)
	}

	return chain, nil
}

// ProcessBlockFromNetwork will handle blocks incoming from the network.
// It will allow the chain to enter sync mode if it detects that we are behind,
// which will cancel the running consensus loop and attempt to reach the new
// chain tip.
// Satisfies the peer.ProcessorFunc interface.
func (c *Chain) ProcessBlockFromNetwork(srcPeerID string, m message.Message) ([]bytes.Buffer, error) {
	blk := m.Payload().(block.Block)

	c.lock.Lock()
	defer c.lock.Unlock()

	l := log.WithField("recv_blk_h", blk.Header.Height).WithField("curr_h", c.tip.Header.Height)

	var kh byte = 255
	if len(m.Header()) > 0 {
		kh = m.Header()[0]
		l = l.WithField("kad_h", kh)
	}

	l.Trace("block received")

	switch {
	case blk.Header.Height == c.tip.Header.Height:
		{
			// Check if we already accepted this block
			if bytes.Equal(blk.Header.Hash, c.tip.Header.Hash) {
				l.WithError(ErrBlockAlreadyAccepted).Debug("failed block processing")
				return nil, nil
			}

			// Try to fallback
			if err := c.tryFallback(blk); err != nil {
				l.WithError(err).Error("failed fallback procedure")
			}

			return nil, nil
		}
	case blk.Header.Height < c.tip.Header.Height:
		l.WithError(ErrBlockAlreadyAccepted).Debug("failed block processing")
		return nil, nil
	}

	if blk.Header.Height > c.highestSeen {
		c.highestSeen = blk.Header.Height
	}

	return c.synchronizer.processBlock(srcPeerID, c.tip.Header.Height, blk, kh)
}

// TryNextConsecutiveBlockOutSync is the processing path for accepting a block
// from the network during out-of-sync state.
func (c *Chain) TryNextConsecutiveBlockOutSync(blk block.Block, kadcastHeight byte) error {
	log.WithField("height", blk.Header.Height).Trace("accepting sync block")
	return c.acceptBlock(blk)
}

// TryNextConsecutiveBlockInSync is the processing path for accepting a block
// from the network during in-sync state. Returns err if the block is not valid.
func (c *Chain) TryNextConsecutiveBlockInSync(blk block.Block, kadcastHeight byte) error {
	// Make an attempt to accept a new block. If succeeds, we could safely restart the Consensus Loop.
	// If not, peer reputation score should be decreased.
	if err := c.acceptSuccessiveBlock(blk, kadcastHeight); err != nil {
		return err
	}

	// Consensus needs a fresh restart so that it is initialized with most
	// recent round update which is Chain tip and the list of active Provisioners.
	if err := c.RestartConsensus(); err != nil {
		log.WithError(err).Error("failed to start consensus loop")
	}

	return nil
}

// TryNextConsecutiveBlockIsValid makes an attempt to validate a blk without
// changing any state.
// returns error if the block is invalid to current blockchain tip.
func (c *Chain) TryNextConsecutiveBlockIsValid(blk block.Block) error {
	fields := logger.Fields{
		"event":    "check_block",
		"height":   blk.Header.Height,
		"hash":     util.StringifyBytes(blk.Header.Hash),
		"curr_h":   c.tip.Header.Height,
		"prov_num": c.p.Set.Len(),
	}

	l := log.WithFields(fields)

	return c.isValidBlock(blk, l)
}

// ProcessSyncTimerExpired called by outsync timer when a peer does not provide GetData response.
// It implements transition back to inSync state.
// strPeerAddr is the address of the peer initiated the syncing but failed to deliver.
func (c *Chain) ProcessSyncTimerExpired(strPeerAddr string) error {
	log.WithField("curr", c.tip.Header.Height).
		WithField("src_addr", strPeerAddr).Warn("sync timer expired")

	c.lock.Lock()
	defer c.lock.Unlock()

	if err := c.RestartConsensus(); err != nil {
		log.WithError(err).Warn("sync timer could not restart consensus loop")
	}

	log.WithField("state", "inSync").Traceln("change sync state")

	c.state = c.inSync
	return nil
}

// acceptSuccessiveBlock will accept a block which directly follows the chain
// tip, and advertises it to the node's peers.
func (c *Chain) acceptSuccessiveBlock(blk block.Block, kadcastHeight byte) error {
	log.WithField("height", blk.Header.Height).Trace("accepting succeeding block")

	if err := c.acceptBlock(blk); err != nil {
		return err
	}

	if blk.Header.Height > c.highestSeen {
		c.highestSeen = blk.Header.Height
	}

	if err := c.propagateBlock(blk, kadcastHeight); err != nil {
		log.WithError(err).Error("block propagation failed")
		return err
	}

	return nil
}

func (c *Chain) runStateTransition(tipBlk, blk block.Block) error {
	var (
		respStateHash       []byte
		provisionersUpdated user.Provisioners
		err                 error
		provisionersCount   int

		fields = logger.Fields{
			"event":      "accept_block",
			"height":     blk.Header.Height,
			"hash":       util.StringifyBytes(blk.Header.Hash),
			"curr_h":     c.tip.Header.Height,
			"block_time": blk.Header.Timestamp - tipBlk.Header.Timestamp,
			"txs_count":  len(blk.Txs),
		}

		l = log.WithFields(fields)
	)

	if err = c.sanityCheckStateHash(); err != nil {
		return err
	}

	provisionersCount = c.p.Set.Len()
	l.WithField("prov", provisionersCount).Info("run state transition")

	switch blk.Header.Certificate.Step {
	case 3:
		// Finalized block. first iteration consensus agreement.
		provisionersUpdated, respStateHash, err = c.proxy.Executor().Finalize(c.ctx,
			blk.Txs,
			tipBlk.Header.StateHash,
			blk.Header.Height,
			config.BlockGasLimit)
		if err != nil {
			l.WithError(err).Error("Error in executing the state transition")
			return err
		}
	default:
		// Tentative block. non-first iteration consensus agreement.
		provisionersUpdated, respStateHash, err = c.proxy.Executor().Accept(c.ctx,
			blk.Txs,
			tipBlk.Header.StateHash,
			blk.Header.Height,
			config.BlockGasLimit)
		if err != nil {
			l.WithError(err).Error("Error in executing the state transition")
			return err
		}
	}

	// Sanity check to ensure accepted block state_hash is the same as the one Finalize/Accept returned.
	if !bytes.Equal(respStateHash, blk.Header.StateHash) {
		log.WithField("rusk", util.StringifyBytes(respStateHash)).WithField("node", util.StringifyBytes(blk.Header.StateHash)).WithError(errInvalidStateHash).Error("inconsistency with state_hash")

		return errInvalidStateHash
	}

	// Update the provisioners.
	// blk.Txs may bring new provisioners to the current state
	c.p = &provisionersUpdated

	l.WithField("prov", c.p.Set.Len()).WithField("added", c.p.Set.Len()-provisionersCount).WithField("state_hash", util.StringifyBytes(respStateHash)).
		Info("state transition completed")

	return nil
}

// sanityCheckStateHash ensures most recent local statehash and rusk statehash are the same.
func (c *Chain) sanityCheckStateHash() error {
	// Ensure that both (co-deployed) services node and rusk are on the same
	// state. If not, we should trigger a recovery procedure so both are
	// always synced up.
	ruskStateHash, err := c.proxy.Executor().GetStateRoot(c.ctx)
	if err != nil {
		return err
	}

	nodeStateHash := c.tip.Header.StateHash

	if !bytes.Equal(nodeStateHash, ruskStateHash) || len(nodeStateHash) == 0 {
		log.WithField("rusk", util.StringifyBytes(ruskStateHash)).
			WithError(errInvalidStateHash).
			WithField("node", util.StringifyBytes(nodeStateHash)).
			Error("check state_hash failed")

		return errInvalidStateHash
	}

	return nil
}

func (c *Chain) isValidBlock(blk block.Block, l *logrus.Entry) error {
	l.Debug("verifying block")
	// Check that stateless and stateful checks pass
	if err := c.verifier.SanityCheckBlock(*c.tip, blk); err != nil {
		l.WithError(err).Error("block verification failed")
		return err
	}

	// Check the certificate
	// This check should avoid a possible race condition between accepting two blocks
	// at the same height, as the probability of the committee creating two valid certificates
	// for the same round is negligible.
	l.Debug("verifying block certificate")

	var err error
	if err = verifiers.CheckBlockCertificate(*c.p, blk, c.tip.Header.Seed); err != nil {
		l.WithError(err).Error("certificate verification failed")
		return err
	}

	return nil
}

// acceptBlock will accept a block if
// 1. We have not seen it before
// 2. All stateless and stateful checks are true
// Returns nil, if checks passed and block was successfully saved.
func (c *Chain) acceptBlock(blk block.Block) error {
	fields := logger.Fields{
		"event":    "accept_block",
		"height":   blk.Header.Height,
		"hash":     util.StringifyBytes(blk.Header.Hash),
		"curr_h":   c.tip.Header.Height,
		"prov_num": c.p.Set.Len(),
	}

	l := log.WithFields(fields)
	var err error

	// 1. Ensure block fields and certificate are valid
	if err = c.isValidBlock(blk, l); err != nil {
		l.WithError(err).Error("invalid block error")
		return err
	}

	// 2. Perform State Transition to update Contract Storage with Tentative or Finalized state.
	if err = c.runStateTransition(*c.tip, blk); err != nil {
		l.WithError(err).Error("execute state transition failed")
		return err
	}

	// 3. Store the approved block and update in-memory chain tip
	l.Debug("storing block")

	if err := c.loader.Append(&blk); err != nil {
		l.WithError(err).Error("block storing failed")
		return err
	}

	c.tip = &blk

	// 5. Perform all post-events on accepting a block
	c.postAcceptBlock(blk, l)

	return nil
}

// postAcceptBlock performs all post-events on accepting a block.
func (c *Chain) postAcceptBlock(blk block.Block, l *logrus.Entry) {
	// 1. Notify other subsystems for the accepted block
	// Subsystems listening for this topic:
	// mempool.Mempool
	l.Debug("notifying internally")

	msg := message.New(topics.AcceptedBlock, blk)
	errList := c.eventBus.Publish(topics.AcceptedBlock, msg)

	// 2. Clear obsolete Candidate blocks
	if err := c.db.Update(func(t database.Transaction) error {
		return t.ClearCandidateMessages()
	}); err != nil {
		// failure here should not be treated as critical
		l.WithError(err).Warn("candidate deletion failed")
	}

	// 3. Update Storm DB
	if config.Get().API.Enabled {
		go c.storeStakesInStormDB(blk.Header.Height)
	}

	diagnostics.LogPublishErrors("chain/chain.go, topics.AcceptedBlock", errList)
	l.Debug("procedure ended")
}

// VerifyCandidateBlock can be used as a callback for the consensus in order to
// verify potential winning candidates.
func (c *Chain) VerifyCandidateBlock(blk block.Block) error {
	var chainTip block.Block

	c.lock.Lock()
	chainTip = c.tip.Copy().(block.Block)
	c.lock.Unlock()

	// We first perform a quick check on the Block Header and
	if err := c.verifier.SanityCheckBlock(chainTip, blk); err != nil {
		return err
	}

	return c.proxy.Executor().VerifyStateTransition(c.ctx, blk.Txs, config.BlockGasLimit, blk.Header.Height)
}

// ExecuteStateTransition calls Rusk ExecuteStateTransitiongrpc method.
func (c *Chain) ExecuteStateTransition(ctx context.Context, txs []transactions.ContractCall, blockHeight uint64) ([]transactions.ContractCall, []byte, error) {
	return c.proxy.Executor().ExecuteStateTransition(c.ctx, txs, config.BlockGasLimit, blockHeight)
}

// propagateBlock send inventory message to all peers in gossip network or rebroadcast block in kadcast network.
func (c *Chain) propagateBlock(b block.Block, kadcastHeight byte) error {
	// Disable gossiping messages if kadcast mode
	if config.Get().Kadcast.Enabled {
		log.WithField("blk_height", b.Header.Height).
			WithField("kadcast_h", kadcastHeight).Trace("propagate block")
		return c.kadcastBlock(b, kadcastHeight)
	}

	log.WithField("blk_height", b.Header.Height).Trace("propagate block")

	msg := &message.Inv{}

	msg.AddItem(message.InvTypeBlock, b.Header.Hash)

	buf := new(bytes.Buffer)
	if err := msg.Encode(buf); err != nil {
		// TODO: shall this really panic ?
		log.Panic(err)
	}

	if err := topics.Prepend(buf, topics.Inv); err != nil {
		// TODO: shall this really panic ?
		log.Panic(err)
	}

	m := message.New(topics.Inv, *buf)
	errList := c.eventBus.Publish(topics.Gossip, m)

	diagnostics.LogPublishErrors("chain/chain.go, topics.Gossip, topics.Inv", errList)
	return nil
}

func (c *Chain) kadcastBlock(blk block.Block, kadcastHeight byte) error {
	buf := new(bytes.Buffer)
	if err := message.MarshalBlock(buf, &blk); err != nil {
		return err
	}

	if err := topics.Prepend(buf, topics.Block); err != nil {
		return err
	}

	c.eventBus.Publish(topics.Kadcast,
		message.NewWithHeader(topics.Block, *buf, []byte{kadcastHeight}))
	return nil
}

func (c *Chain) getRoundUpdate() consensus.RoundUpdate {
	return consensus.RoundUpdate{
		Round:           c.tip.Header.Height + 1,
		P:               c.p.Copy(),
		Seed:            c.tip.Header.Seed,
		Hash:            c.tip.Header.Hash,
		LastCertificate: c.tip.Header.Certificate,
	}
}

// GetSyncProgress returns how close the node is to being synced to the tip,
// as a percentage value.
func (c *Chain) GetSyncProgress(_ context.Context, e *node.EmptyRequest) (*node.SyncProgressResponse, error) {
	return &node.SyncProgressResponse{Progress: float32(c.CalculateSyncProgress())}, nil
}

// CalculateSyncProgress of the node.
func (c *Chain) CalculateSyncProgress() float64 {
	c.lock.RLock()
	defer c.lock.RUnlock()

	if c.highestSeen == 0 {
		return 0.0
	}

	progressPercentage := (float64(c.tip.Header.Height) / float64(c.highestSeen)) * 100
	if progressPercentage > 100 {
		progressPercentage = 100
	}

	return progressPercentage
}

// RebuildChain will delete all blocks except for the genesis block,
// to allow for a full re-sync.
// NOTE: This function no longer does anything, but is still here to conform to the
// ChainServer interface, for GRPC communications.
func (c *Chain) RebuildChain(_ context.Context, e *node.EmptyRequest) (*node.GenericResponse, error) {
	return &node.GenericResponse{Response: "Unimplemented"}, nil
}

func (c *Chain) storeStakesInStormDB(blkHeight uint64) {
	store := capi.GetStormDBInstance()
	members := make([]*capi.Member, len(c.p.Members))
	i := 0

	for _, v := range c.p.Members {
		var stakes []capi.Stake

		for _, s := range v.Stakes {
			stake := capi.Stake{
				Amount:      s.Amount,
				StartHeight: s.StartHeight,
				EndHeight:   s.EndHeight,
			}

			stakes = append(stakes, stake)
		}

		member := capi.Member{
			PublicKeyBLS: v.PublicKeyBLS,
			Stakes:       stakes,
		}

		members[i] = &member
		i++
	}

	provisioner := capi.ProvisionerJSON{
		ID:      blkHeight,
		Set:     c.p.Set,
		Members: members,
	}

	err := store.Save(&provisioner)
	if err != nil {
		log.Warn("Could not store provisioners on memoryDB")
	}
}
