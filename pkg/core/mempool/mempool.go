// This Source Code Form is subject to the terms of the MIT License.
// If a copy of the MIT License was not distributed with this
// file, you can obtain one at https://opensource.org/licenses/MIT.
//
// Copyright (c) DUSK NETWORK. All rights reserved.

package mempool

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/dusk-network/dusk-blockchain/pkg/util/diagnostics"
	"golang.org/x/time/rate"

	"github.com/dusk-network/dusk-blockchain/pkg/config"
	"github.com/dusk-network/dusk-blockchain/pkg/core/consensus"
	"github.com/dusk-network/dusk-blockchain/pkg/core/data/block"
	"github.com/dusk-network/dusk-blockchain/pkg/core/data/ipc/transactions"
	"github.com/dusk-network/dusk-blockchain/pkg/core/database"
	"github.com/dusk-network/dusk-blockchain/pkg/p2p/wire/encoding"
	"github.com/dusk-network/dusk-blockchain/pkg/p2p/wire/message"
	"github.com/dusk-network/dusk-blockchain/pkg/p2p/wire/topics"
	"github.com/dusk-network/dusk-blockchain/pkg/util/nativeutils/eventbus"
	"github.com/dusk-network/dusk-blockchain/pkg/util/nativeutils/rpcbus"
	"github.com/dusk-network/dusk-protobuf/autogen/go/node"
	logger "github.com/sirupsen/logrus"
	"google.golang.org/grpc"
)

var log = logger.WithFields(logger.Fields{"process": "mempool"})

const (
	idleTime        = 20 * time.Second
	backendHashmap  = "hashmap"
	backendDiskpool = "diskpool"
)

var (
	// ErrCoinbaseTxNotAllowed coinbase tx must be built by block generator only.
	ErrCoinbaseTxNotAllowed = errors.New("coinbase tx not allowed")
	// ErrAlreadyExists transaction with same txid already exists in.
	ErrAlreadyExists = errors.New("already exists")
	// ErrDoubleSpending transaction uses outputs spent in other mempool txs.
	ErrDoubleSpending = errors.New("double-spending in mempool")
)

// Mempool is a storage for the chain transactions that are valid according to the
// current chain state and can be included in the next block.
type Mempool struct {
	getMempoolTxsChan       <-chan rpcbus.Request
	getMempoolTxsBySizeChan <-chan rpcbus.Request
	sendTxChan              <-chan rpcbus.Request

	// verified txs to be included in next block.
	verified Pool

	pendingPropagation chan TxDesc

	// the collector to listen for new accepted blocks.
	acceptedBlockChan <-chan block.Block

	// used by tx verification procedure.
	latestBlockTimestamp int64

	eventBus *eventbus.EventBus

	// the magic function that knows best what is valid chain Tx.
	verifier transactions.UnconfirmedTxProber

	limiter *rate.Limiter
}

// checkTx is responsible to determine if a tx is valid or not.
// Among the other checks, the underlying verifier also checks double spending.
func (m *Mempool) checkTx(tx transactions.ContractCall) error {
	ctx, cancel := context.WithTimeout(context.Background(),
		time.Duration(config.Get().RPC.Rusk.ContractTimeout)*time.Millisecond)
	defer cancel()

	// check if external verifyTx is provided
	if err := m.verifier.VerifyTransaction(ctx, tx); err != nil {
		return err
	}

	return nil
}

// NewMempool instantiates and initializes node mempool.
func NewMempool(db database.DB, eventBus *eventbus.EventBus, rpcBus *rpcbus.RPCBus, verifier transactions.UnconfirmedTxProber, srv *grpc.Server) *Mempool {
	log.Infof("create instance")

	l := log.WithField("backend_type", config.Get().Mempool.PoolType).
		WithField("max_size_mb", config.Get().Mempool.MaxSizeMB)

	getMempoolTxsChan := make(chan rpcbus.Request, 1)
	if err := rpcBus.Register(topics.GetMempoolTxs, getMempoolTxsChan); err != nil {
		log.WithError(err).Error("failed to register topics.GetMempoolTxs")
	}

	getMempoolTxsBySizeChan := make(chan rpcbus.Request, 1)
	if err := rpcBus.Register(topics.GetMempoolTxsBySize, getMempoolTxsBySizeChan); err != nil {
		log.WithError(err).Error("failed to register topics.GetMempoolTxsBySize")
	}

	sendTxChan := make(chan rpcbus.Request, 1)
	if err := rpcBus.Register(topics.SendMempoolTx, sendTxChan); err != nil {
		log.WithError(err).Error("failed to register topics.SendMempoolTx")
	}

	acceptedBlockChan, _ := consensus.InitAcceptedBlockUpdate(eventBus)

	// Enable rate limiter from config
	cfg := config.Get().Mempool

	var limiter *rate.Limiter

	if len(cfg.PropagateTimeout) > 0 {
		timeout, err := time.ParseDuration(cfg.PropagateTimeout)
		if err != nil {
			log.WithError(err).Fatal("could not parse mempool propagation timeout")
		}

		burst := cfg.PropagateBurst
		if burst == 0 {
			burst = 1
		}

		limiter = rate.NewLimiter(rate.Every(timeout), int(burst))

		l = l.WithField("propagate_timeout", cfg.PropagateTimeout).
			WithField("propagate_burst", burst)
	}

	m := &Mempool{
		eventBus:                eventBus,
		latestBlockTimestamp:    math.MinInt32,
		acceptedBlockChan:       acceptedBlockChan,
		getMempoolTxsChan:       getMempoolTxsChan,
		getMempoolTxsBySizeChan: getMempoolTxsBySizeChan,
		sendTxChan:              sendTxChan,
		verifier:                verifier,
		limiter:                 limiter,
		pendingPropagation:      make(chan TxDesc, 1000),
	}

	// Setting the pool where to cache verified transactions.
	// The pool is normally a Hashmap
	m.verified = m.newPool()

	// Perform cleanup as background process.
	go cleanupAcceptedTxs(m.verified, db)

	l.Info("running")

	if srv != nil {
		node.RegisterMempoolServer(srv, m)
	}

	return m
}

// Run spawns the mempool lifecycle routines.
func (m *Mempool) Run(ctx context.Context) {
	// Main Loop
	go m.Loop(ctx)

	// Loop to drain pendingPropagation and try to propagate transaction
	go m.propagateLoop(ctx)
}

// Loop listens for GetMempoolTxs request and topics.AcceptedBlock events.
func (m *Mempool) Loop(ctx context.Context) {
	ticker := time.NewTicker(idleTime)
	defer ticker.Stop()

	for {
		select {
		// rpcbus methods.
		case r := <-m.sendTxChan:
			// TODO: This handler should be deleted once new wallet is integrated
			go handleRequest(r, m.processSendMempoolTxRequest, "SendTx")
		case r := <-m.getMempoolTxsChan:
			handleRequest(r, m.processGetMempoolTxsRequest, "GetMempoolTxs")
		case r := <-m.getMempoolTxsBySizeChan:
			handleRequest(r, m.processGetMempoolTxsBySizeRequest, "GetMempoolTxsBySize")
		case b := <-m.acceptedBlockChan:
			m.onBlock(b)
		case <-ticker.C:
			m.onIdle()
		case <-ctx.Done():
			m.OnClose()
			log.Info("main_loop terminated")
			return
		}

		ticker.Reset(idleTime)
	}
}

func (m *Mempool) propagateLoop(ctx context.Context) {
	for {
		select {
		case t := <-m.pendingPropagation:
			// Ensure we propagate at proper rate
			if m.limiter != nil {
				if err := m.limiter.Wait(ctx); err != nil {
					log.WithError(err).Error("failed to limit rate")
				}
			}

			txid, err := t.tx.CalculateHash()
			if err != nil {
				log.WithError(err).Error("failed to calc hash")
				continue
			}

			if config.Get().Kadcast.Enabled {
				// Broadcast full transaction data in kadcast
				err = m.kadcastTx(t)
			} else {
				// Advertise the transaction hash to gossip network via "Inventory Vectors"
				err = m.advertiseTx(txid)
			}

			if err != nil {
				log.WithField("txid", hex.EncodeToString(txid)).WithError(err).Error("failed to propagate")
			}

		// Mempool terminating
		case <-ctx.Done():
			log.Info("propagate_loop terminated")
			return
		}
	}
}

// ProcessTx processes a Transaction wire message.
func (m *Mempool) ProcessTx(srcPeerID string, msg message.Message) ([]bytes.Buffer, error) {
	maxSizeBytes := config.Get().Mempool.MaxSizeMB * 1000 * 1000
	if m.verified.Size() > maxSizeBytes {
		log.WithField("max_size_mb", maxSizeBytes).
			WithField("alloc_size", m.verified.Size()/1000).
			Warn("mempool is full, dropping transaction")
		return nil, errors.New("mempool is full, dropping transaction")
	}

	var h byte
	if len(msg.Header()) > 0 {
		h = msg.Header()[0]
	}

	t := TxDesc{
		tx:        msg.Payload().(transactions.ContractCall),
		received:  time.Now(),
		size:      uint(len(msg.Id())),
		kadHeight: h,
	}

	start := time.Now()
	txid, err := m.processTx(t)
	elapsed := time.Since(start)

	if err != nil {
		log.WithError(err).
			WithField("txid", toHex(txid)).
			WithField("txtype", t.tx.Type()).
			WithField("txsize", t.size).
			WithField("duration", elapsed.Microseconds()).
			WithField("kad_h", h).
			Error("failed to accept transaction")
	} else {
		log.WithField("txid", toHex(txid)).
			WithField("txtype", t.tx.Type()).
			WithField("txsize", t.size).
			WithField("duration", elapsed.Microseconds()).
			Trace("accepted transaction")
	}

	return nil, err
}

// processTx ensures all transaction rules are satisfied before adding the tx
// into the verified pool.
func (m *Mempool) processTx(t TxDesc) ([]byte, error) {
	txid, err := t.tx.CalculateHash()
	if err != nil {
		return txid, fmt.Errorf("hash err: %s", err.Error())
	}

	log.WithField("txid", txid).
		Trace("ensuring transaction rules satisfied")

	if t.tx.Type() == transactions.Distribute {
		// coinbase tx should be built by block generator only
		return txid, ErrCoinbaseTxNotAllowed
	}

	// expect it is not already a verified tx
	if m.verified.Contains(txid) {
		return txid, ErrAlreadyExists
	}

	// execute tx verification procedure
	if err := m.checkTx(t.tx); err != nil {
		return txid, fmt.Errorf("verification err - %v", err)
	}

	// if consumer's verification passes, mark it as verified
	t.verified = time.Now()

	// we've got a valid transaction pushed
	if err := m.verified.Put(t); err != nil {
		return txid, fmt.Errorf("store err - %v", err)
	}

	// queue transaction for (re)propagation
	go func() {
		m.pendingPropagation <- t
	}()

	return txid, nil
}

func (m *Mempool) onBlock(b block.Block) {
	m.latestBlockTimestamp = b.Header.Timestamp
	m.removeAccepted(b)
}

// removeAccepted to clean up all txs from the mempool that have been already
// added to the chain.
//
// Instead of doing a full DB scan, here we rely on the latest accepted block to
// update.
//
// The passed block is supposed to be the last one accepted.
func (m *Mempool) removeAccepted(b block.Block) {
	if m.verified.Len() == 0 {
		// Empty pool then no need for cleanup
		return
	}

	l := log.WithField("blk_height", b.Header.Height).
		WithField("blk_txs_count", len(b.Txs)).
		WithField("alloc_size", int64(m.verified.Size())/1000).
		WithField("txs_count", m.verified.Len())

	for _, tx := range b.Txs {
		hash, err := tx.CalculateHash()
		if err != nil {
			log.WithError(err).Panic("could not calculate tx hash")
		}

		_ = m.verified.Delete(hash)
	}

	l.Info("processing_block_completed")
}

// TODO: Get rid of stuck/expired transactions.
func (m *Mempool) onIdle() {
	log.
		WithField("alloc_size", int64(m.verified.Size())/1000).
		WithField("txs_count", m.verified.Len()).Info("process_on_idle")
}

func (m *Mempool) newPool() Pool {
	cfg := config.Get().Mempool

	var p Pool

	switch cfg.PoolType {
	case backendHashmap:
		p = &HashMap{
			lock:     &sync.RWMutex{},
			Capacity: cfg.HashMapPreallocTxs,
		}
	case backendDiskpool:
		p = new(buntdbPool)
	default:
		p = &HashMap{
			lock:     &sync.RWMutex{},
			Capacity: cfg.HashMapPreallocTxs,
		}
	}

	if err := p.Create(cfg.DiskPoolDir); err != nil {
		log.WithField("pool", cfg.PoolType).WithError(err).Fatal("failed to create pool")
	}

	return p
}

// processGetMempoolTxsRequest retrieves current state of the mempool of the verified but
// still unaccepted txs.
// Called by P2P on InvTypeMempoolTx msg.
func (m Mempool) processGetMempoolTxsRequest(r rpcbus.Request) (interface{}, error) {
	// Read inputs
	params := r.Params.(bytes.Buffer)
	filterTxID := params.Bytes()
	outputTxs := make([]transactions.ContractCall, 0)

	// If we are looking for a specific tx, just look it up by key.
	if len(filterTxID) == 32 {
		tx := m.verified.Get(filterTxID)
		if tx == nil {
			return outputTxs, nil
		}

		outputTxs = append(outputTxs, tx)
		return outputTxs, nil
	}

	// When filterTxID is empty, mempool returns all verified txs sorted
	// by fee from highest to lowest
	err := m.verified.RangeSort(func(k txHash, t TxDesc) (bool, error) {
		outputTxs = append(outputTxs, t.tx)
		return false, nil
	})
	if err != nil {
		return nil, err
	}

	return outputTxs, err
}

// uType translates the node.TxType into transactions.TxType.
func uType(t node.TxType) (transactions.TxType, error) {
	switch t {
	case node.TxType_STANDARD:
		return transactions.Tx, nil
	case node.TxType_DISTRIBUTE:
		return transactions.Distribute, nil
	case node.TxType_BID:
		return transactions.Bid, nil
	case node.TxType_STAKE:
		return transactions.Stake, nil
	case node.TxType_WITHDRAWFEES:
		return transactions.WithdrawFees, nil
	case node.TxType_WITHDRAWSTAKE:
		return transactions.WithdrawStake, nil
	case node.TxType_WITHDRAWBID:
		return transactions.WithdrawBid, nil
	case node.TxType_SLASH:
		return transactions.Slash, nil
	default:
		return transactions.Tx, errors.New("unknown transaction type")
	}
}

// SelectTx will return a view of the mempool, with optional filters applied.
func (m Mempool) SelectTx(ctx context.Context, req *node.SelectRequest) (*node.SelectResponse, error) {
	txs := make([]transactions.ContractCall, 0)

	switch {
	case len(req.Id) == 64:
		// If we want a tx with a certain ID, we can simply look it up
		// directly
		hash, err := hex.DecodeString(req.Id)
		if err != nil {
			return nil, err
		}

		tx := m.verified.Get(hash)
		if tx == nil {
			return nil, errors.New("tx not found")
		}

		txs = append(txs, tx)
	case len(req.Types) > 0:
		for _, t := range req.Types {
			trType, err := uType(t)
			if err != nil {
				// most likely an unsupported type. We just ignore it
				continue
			}

			txs = append(txs, m.verified.FilterByType(trType)...)
		}
	default:
		txs = m.verified.Clone()
	}

	resp := &node.SelectResponse{Result: make([]*node.Tx, len(txs))}

	for i, tx := range txs {
		txid, err := tx.CalculateHash()
		if err != nil {
			return nil, err
		}

		resp.Result[i] = &node.Tx{
			Type: node.TxType(tx.Type()),
			Id:   hex.EncodeToString(txid),
			// LockTime: tx.LockTime(),
		}
	}

	return resp, nil
}

// GetUnconfirmedBalance will return the amount of DUSK that is in the mempool
// for a given key.
func (m Mempool) GetUnconfirmedBalance(ctx context.Context, req *node.GetUnconfirmedBalanceRequest) (*node.BalanceResponse, error) {
	txs := m.verified.Clone()

	balance, err := m.verifier.CalculateBalance(ctx, req.Vk, txs)
	if err != nil {
		return nil, err
	}

	return &node.BalanceResponse{LockedBalance: balance}, nil
}

// processGetMempoolTxsBySizeRequest returns a subset of verified mempool txs which
// 1. contains only highest fee txs
// 2. has total txs size not bigger than maxTxsSize (request param)
// Called by BlockGenerator on generating a new candidate block.
func (m Mempool) processGetMempoolTxsBySizeRequest(r rpcbus.Request) (interface{}, error) {
	// Read maxTxsSize param
	var maxTxsSize uint32

	params := r.Params.(bytes.Buffer)
	if err := encoding.ReadUint32LE(&params, &maxTxsSize); err != nil {
		return bytes.Buffer{}, err
	}

	txs := make([]transactions.ContractCall, 0)

	var totalSize uint32

	err := m.verified.RangeSort(func(k txHash, t TxDesc) (bool, error) {
		var done bool
		totalSize += uint32(t.size)

		if totalSize <= maxTxsSize {
			txs = append(txs, t.tx)
		} else {
			done = true
		}

		return done, nil
	})
	if err != nil {
		return bytes.Buffer{}, err
	}

	return txs, err
}

// processSendMempoolTxRequest utilizes rpcbus to allow submitting a tx to mempool with.
func (m Mempool) processSendMempoolTxRequest(r rpcbus.Request) (interface{}, error) {
	tx := r.Params.(transactions.ContractCall)

	buf := new(bytes.Buffer)
	if err := transactions.Marshal(buf, tx); err != nil {
		return nil, err
	}

	t := TxDesc{tx: tx, received: time.Now(), size: uint(buf.Len()), kadHeight: config.KadcastInitialHeight}
	return m.processTx(t)
}

// Send Inventory message to all peers.
//nolint:unparam
func (m *Mempool) advertiseTx(txID []byte) error {
	msg := &message.Inv{}
	msg.AddItem(message.InvTypeMempoolTx, txID)

	// TODO: can we simply encode the message directly on a topic carrying buffer?
	buf := new(bytes.Buffer)
	if err := msg.Encode(buf); err != nil {
		log.Panic(err)
	}

	if err := topics.Prepend(buf, topics.Inv); err != nil {
		log.Panic(err)
	}

	packet := message.New(topics.Inv, *buf)
	errList := m.eventBus.Publish(topics.Gossip, packet)

	diagnostics.LogPublishErrors("mempool.go, topics.Gossip, topics.Inv", errList)
	return nil
}

// kadcastTx (re)propagates transaction in kadcast network.
func (m *Mempool) kadcastTx(t TxDesc) error {
	if t.kadHeight > config.KadcastInitialHeight {
		return errors.New("invalid kadcast height")
	}

	/// repropagate
	buf := new(bytes.Buffer)
	if err := transactions.Marshal(buf, t.tx); err != nil {
		return err
	}

	if err := topics.Prepend(buf, topics.Tx); err != nil {
		return err
	}

	msg := message.NewWithHeader(topics.Tx, *buf, []byte{t.kadHeight})

	m.eventBus.Publish(topics.Kadcast, msg)
	return nil
}

// OnClose performs mempool cleanup procedure. It's called on canceling mempool
// context.
func (m *Mempool) OnClose() {
	// Closing diskpool backend commits changes to file and close it.
	m.verified.Close()
}

func toHex(id []byte) string {
	enc := hex.EncodeToString(id[:])
	return enc
}

// TODO: handlers should just return []transactions.ContractCall, and the
// caller should be left to format the data however they wish.
func handleRequest(r rpcbus.Request, handler func(r rpcbus.Request) (interface{}, error), name string) {
	result, err := handler(r)
	if err != nil {
		log.
			WithError(err).
			WithField("name", name).Errorf("mempool failed to process request")
		r.RespChan <- rpcbus.Response{Err: err}
		return
	}

	r.RespChan <- rpcbus.Response{Resp: result, Err: nil}
}

// cleanupAcceptedTxs discards any transactions that were accepted into
// blockchain while node was offline.
func cleanupAcceptedTxs(pool Pool, db database.DB) {
	if db == nil {
		return
	}

	deleteList := make([]txHash, 0)

	_ = pool.Range(func(k txHash, t TxDesc) error {
		_ = db.View(func(t database.Transaction) error {
			// TODO: FetchBlockTxByHash should be replaced with FetchTxExists
			_, _, _, err := t.FetchBlockTxByHash(k[:])
			if err == nil {
				// transaction already accepted.
				deleteList = append(deleteList, k)
			}

			return nil
		})

		return nil
	})

	// BuntDB does not currently support deleting a key while in the process of
	// iterating. As a workaround you'll need to delete keys following the
	// completion of the iterator.
	for _, txhash := range deleteList {
		if err := pool.Delete(txhash[:]); err != nil {
			log.WithError(err).WithField("txid", hex.EncodeToString(txhash[:])).Warn("could not delete tx")
		}
	}

	if len(deleteList) > 0 {
		log.WithField("len", len(deleteList)).Info("clean up redundant transactions")
	}
}
