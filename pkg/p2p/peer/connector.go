// This Source Code Form is subject to the terms of the MIT License.
// If a copy of the MIT License was not distributed with this
// file, you can obtain one at https://opensource.org/licenses/MIT.
//
// Copyright (c) DUSK NETWORK. All rights reserved.

package peer

import (
	"bytes"
	"context"
	"errors"
	"net"
	"sync"
	"time"

	"github.com/dusk-network/dusk-blockchain/pkg/config"
	"github.com/dusk-network/dusk-blockchain/pkg/core/consensus/capi"
	"github.com/dusk-network/dusk-blockchain/pkg/p2p/wire/message"
	"github.com/dusk-network/dusk-blockchain/pkg/p2p/wire/protocol"
	"github.com/dusk-network/dusk-blockchain/pkg/p2p/wire/topics"
	"github.com/dusk-network/dusk-blockchain/pkg/util/nativeutils/eventbus"
	"github.com/sirupsen/logrus"
	log "github.com/sirupsen/logrus"
)

const (
	defaultDialTimeout    = 5
	defaultMaxConnections = 50
	peerCountTime         = 30 * time.Second
)

var plog = logrus.WithField("process", "peer_conn")

type connectFunc func(context.Context, *Reader, *Writer)

// Connector is responsible for accepting incoming connection requests, and
// establishing outward connections with desired peers.
type Connector struct {
	eventBus      eventbus.Broker
	gossip        *protocol.Gossip
	readerFactory *ReaderFactory

	l net.Listener

	lock     sync.RWMutex
	registry map[string]struct{}

	services protocol.ServiceFlag

	connectFunc connectFunc
}

// NewConnector creates a new peer connector, and spawns a goroutine that will
// accept incoming connection requests on the current address, with the given port.
func NewConnector(eb eventbus.Broker, gossip *protocol.Gossip, port string,
	processor *MessageProcessor, services protocol.ServiceFlag,
	connectFunc connectFunc) *Connector {
	addrPort := ":" + port

	listener, err := net.Listen("tcp", addrPort)
	if err != nil {
		plog.WithError(err).
			Panic("could not establish a listener")
	}

	c := &Connector{
		eventBus:      eb,
		gossip:        gossip,
		readerFactory: NewReaderFactory(processor),
		l:             listener,
		registry:      make(map[string]struct{}),
		services:      services,
		connectFunc:   connectFunc,
	}

	processor.Register(topics.Addr, c.ProcessNewAddress)

	go func(c *Connector) {
		for {
			conn, err := c.l.Accept()
			if err != nil {
				plog.WithField("r_addr", conn.RemoteAddr().String()).
					WithError(err).
					Warnln("error accepting conn request")
				return
			}

			c.acceptConnection(conn)
		}
	}(c)

	if config.Get().API.Enabled {
		go c.logPeerCount()
	}

	return c
}

// Close the listener.
func (c *Connector) Close() error {
	return c.l.Close()
}

func (c *Connector) logPeerCount() {
	ticker := time.NewTicker(peerCountTime)

	for {
		<-ticker.C

		store := capi.GetStormDBInstance()

		for addr := range c.registry {
			// save count
			peerCount := capi.PeerCount{
				ID:       addr,
				LastSeen: time.Now(),
			}

			if err := store.Save(&peerCount); err != nil {
				log.Error("failed to save peerCount into StormDB")
			}
		}
	}
}

// ProcessNewAddress will handle a new Addr message from the network.
// Satisfies the peer.ProcessorFunc interface.
func (c *Connector) ProcessNewAddress(srcPeerID string, m message.Message) ([]bytes.Buffer, error) {
	maxConn := config.Get().Network.MaxConnections
	if maxConn == 0 {
		maxConn = defaultMaxConnections
	}

	if c.GetConnectionsCount() > maxConn {
		return nil, errors.New("max amount of connections reached")
	}

	a := m.Payload().(message.Addr)
	return nil, c.Connect(a.NetAddr)
}

// Connect dials a connection with its string, then on succession
// we pass the connection and the address to the OnConn method.
func (c *Connector) Connect(addr string) error {
	conn, err := c.Dial(addr)
	if err != nil {
		return err
	}

	c.proposeConnection(conn)
	return nil
}

// Dial dials up a connection, given its address string.
func (c *Connector) Dial(addr string) (net.Conn, error) {
	t := config.Get().Timeout.TimeoutDial
	if t == 0 {
		t = defaultDialTimeout
	}

	dialTimeout := time.Duration(t) * time.Second

	conn, err := net.DialTimeout("tcp", addr, dialTimeout)
	if err != nil {
		return nil, err
	}

	return conn, nil
}

func (c *Connector) acceptConnection(conn net.Conn) {
	pConn := NewConnection(conn, c.gossip)
	peerReader := c.readerFactory.SpawnReader(pConn)
	raddr := conn.RemoteAddr().String()

	if err := peerReader.Accept(c.services); err != nil {
		plog.WithField("r_addr", raddr).
			WithError(err).
			WithField("type", "inbound").
			Warnln("error performing handshake")
		return
	}

	plog.WithField("r_addr", raddr).WithField("type", "inbound").
		Infoln("peer_connection established")

	c.addPeer(peerReader.Addr())

	peerWriter := NewWriter(pConn, c.eventBus)

	go func() {
		c.connectFunc(context.Background(), peerReader, peerWriter)
		c.removePeer(peerReader.Addr())
	}()
}

func (c *Connector) proposeConnection(conn net.Conn) {
	pConn := NewConnection(conn, c.gossip)
	peerWriter := NewWriter(pConn, c.eventBus)

	if err := peerWriter.Connect(c.services); err != nil {
		plog.WithField("r_addr", conn.RemoteAddr().String()).
			WithField("type", "outbound").
			WithError(err).Warnln("error performing handshake")
		return
	}

	address := peerWriter.Addr()

	plog.WithField("r_addr", address).WithField("type", "outbound").
		Infoln("peer_connection established")

	peerReader := c.readerFactory.SpawnReader(pConn)

	c.addPeer(peerWriter.Addr())

	go func() {
		c.connectFunc(context.Background(), peerReader, peerWriter)
		c.removePeer(peerWriter.Addr())
	}()
}

func (c *Connector) addPeer(address string) {
	c.lock.Lock()
	defer c.lock.Unlock()

	c.registry[address] = struct{}{}
}

func (c *Connector) removePeer(address string) {
	c.lock.Lock()
	defer c.lock.Unlock()

	delete(c.registry, address)

	if config.Get().API.Enabled {
		go func() {
			peerCount := capi.PeerCount{
				ID: address,
			}

			store := capi.GetStormDBInstance()

			// delete count
			if err := store.Delete(&peerCount); err != nil {
				plog.Error("failed to Delete peerCount into StormDB")
			}
		}()
	}

	// Ensure we are still above the minimum connections threshold.
	if len(c.registry) < config.Get().Network.MinimumConnections {
		buf := new(bytes.Buffer)
		if err := topics.Prepend(buf, topics.GetAddrs); err != nil {
			plog.WithError(err).
				Panic("could not create topic buffer")
		}

		c.eventBus.Publish(topics.Gossip, message.New(topics.GetAddrs, *buf))
	}
}

// GetConnectionsCount returns the amount of active connections the node has.
func (c *Connector) GetConnectionsCount() int {
	c.lock.RLock()
	defer c.lock.RUnlock()

	return len(c.registry)
}
