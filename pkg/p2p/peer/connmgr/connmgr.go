package connmgr

import (
	"errors"
	log "github.com/sirupsen/logrus"
	"gitlab.dusk.network/dusk-core/dusk-go/pkg/noded/config"
	"gitlab.dusk.network/dusk-core/dusk-go/pkg/p2p/peer/connmgr/event"
	"gitlab.dusk.network/dusk-core/dusk-go/pkg/p2p/wire/util"
	"net"
	"net/http"
	"strconv"
	"time"
)

var (
	// maxOutboundConn is the maximum number of active peers
	// that the connection manager will try to have
	maxOutboundConn = 10 //TODO: We have this already in cmd.Config

	// maxRetries is the maximum amount of successive retries that
	// we can have before we stop dialing that peer
	maxRetries = uint8(5)
)

// Connmgr manages pending/active/failed connections
type Connmgr struct {
	config        Config
	PendingList   map[string]*Request
	ConnectedList map[string]*Request
	actionch      chan func()
	conevtch      chan event.ConditionalEvent
}

// New creates a new connection manager
func New(cfg Config) *Connmgr {
	cm := &Connmgr{
		config:        cfg,
		PendingList:   make(map[string]*Request),
		ConnectedList: make(map[string]*Request),
		actionch:      make(chan func(), 300),
		conevtch:      make(chan event.ConditionalEvent),
	}

	go func() {

		ip, err := util.GetOutboundIP()
		if err != nil {
			log.WithField("prefix", "connmgr").Error("Failed to get local ip", err)
		}
		addrPort := ip.String() + ":" + strconv.FormatUint(uint64(config.EnvNetCfg.Peer.Port), 10)
		listener, err := net.Listen("tcp", addrPort)

		if err != nil {
			log.WithField("prefix", "connmgr").Fatalf("Failed to connect to %s: %s", addrPort, err)
		}

		defer func() {
			listener.Close()
		}()

		for {
			conn, err := listener.Accept()

			if err != nil {
				continue
			}
			go cm.config.OnAccept(conn)
		}

	}()

	return cm
}

//NewRequest will make a new peer connection.
//It gets the address from GetAddress func in config, dials it and assigns it to pending
func (c *Connmgr) NewRequest() {

	// Fetch address from newAddrs from Address Manager
	addr, err := c.config.GetAddress()
	// When newAddrs is empty an OutOfPeerAddr is triggered
	if err != nil {
		c.conevtch <- event.GetEvent(event.OutOfPeerAddr)
		return
	}

	// Empty request item
	r := &Request{}

	r.Addr = addr
	log.WithField("prefix", "connmgr").Infof("Connecting to peer %s", addr)

	// Will create connection from newAddrs and adds it to the ConnectedList
	c.Connect(r)
}

// Connect connects to a specific address
func (c *Connmgr) Connect(r *Request) error {
	r.Retries++

	conn, err := c.dial(r.Addr)
	if err != nil {
		c.pending(r)
		c.failed(r)
		return err
	}

	r.Conn = conn
	r.Inbound = true

	// r.Permanent is set by the caller. default is false
	// The permanent connections will be the ones that are hardcoded, e.g seed3.ngd.network

	return c.connected(r)
}

// Disconnect disconnects from a specific address
func (c *Connmgr) Disconnect(addr string) {

	// fetch from connected list
	r, ok := c.ConnectedList[addr]

	if !ok {
		// If not in connected, check pending
		r, ok = c.PendingList[addr]
	}

	c.disconnected(r)

}

// dial is used to dial up connections given the address in the form ip:port
func (c *Connmgr) dial(addr string) (net.Conn, error) {
	dialTimeout := 1 * time.Second
	conn, err := net.DialTimeout("tcp", addr, dialTimeout)
	if err != nil {
		if !isConnected() {
			return nil, errors.New("Failed to connect. Please check connection to the Internet")
		}
		return conn, err
	}
	return conn, nil
}

func (c *Connmgr) failed(r *Request) {

	c.actionch <- func() {
		// priority to check if it is permanent or inbound
		// if so then these peers are valuable in DUSK and so we will just retry another time
		if r.Inbound || r.Permanent {

			multiplier := time.Duration(r.Retries * 10)
			time.AfterFunc(multiplier*time.Second,
				func() {
					c.Connect(r)
				},
			)
			// if not then we should check if this request has had maxRetries
		} else if r.Retries > maxRetries {
			delete(c.PendingList, r.Addr)
			//go c.monitorThresholds()
			log.WithField("prefix", "connmgr").
				Warnf("%s has been tried the maximum amount of times", r.Addr)
			// As a NewRequest (monitor fills the ConnectedList) is asynchronous it could be faster than OnFail.
			// OnFail is too late to delete the failed connection from the Address Manager's newAddrs.
			// Effect is that an already tried and failed peer connection could be tried a second time.
			// TODO: low prio, but don't forget
			if c.config.OnFail != nil {
				c.config.OnFail(r.Addr)
			}
			// if not then call Connect on it again
		} else {
			go c.Connect(r)
		}
	}
}

// Disconnected is called when a peer disconnects.
// we take the addr from peer, which is also it's key in the map
// and we use it to remove it from the connectedList
func (c *Connmgr) disconnected(r *Request) error {
	errChan := make(chan error, 0)

	c.actionch <- func() {
		var err error

		if r == nil {
			err = errors.New("Request object is nil")
		}

		r2 := *r // dereference it, so that r.Addr is not lost on delete

		// if for some reason the underlying connection is not closed, close it
		r.Conn.Close()
		r.Conn = nil
		// if for some reason it is in pending list, remove it
		delete(c.PendingList, r.Addr)
		delete(c.ConnectedList, r.Addr)
		c.failed(&r2)
		errChan <- err
	}

	return <-errChan
}

// connected is called when the connection manager makes a successful connection.
func (c *Connmgr) connected(r *Request) error {
	errorChan := make(chan error, 0)

	c.actionch <- func() {
		var err error

		// This should not be the case, since we connected
		// Keeping it here to be safe
		if r == nil {
			err = errors.New("Request object as nil inside of the connected function")
		}

		// Reset retries to 0
		r.Retries = 0

		// Add to connectedList
		c.ConnectedList[r.Addr] = r

		// Remove from pending if it was there
		delete(c.PendingList, r.Addr)

		if c.config.OnConnection != nil {
			c.config.OnConnection(r.Conn, r.Addr)
		}

		//go c.monitorThresholds()

		if err != nil {
			log.Error("Error connected", err)
		}

		errorChan <- err
	}

	return <-errorChan
}

// Pending is synchronous, we do not want to continue with logic
// until we are certain it has been added to the pendingList
func (c *Connmgr) pending(r *Request) error {
	errChan := make(chan error, 0)

	c.actionch <- func() {
		var err error

		if r == nil {
			err = errors.New("Error : Request object is nil")
		}

		c.PendingList[r.Addr] = r
		errChan <- err
	}

	return <-errChan
}

// Run starts the Connection Manager as a daemon
func (c *Connmgr) Run() {
	go c.monEvtLoop()
	go c.loop()
}

func (c *Connmgr) loop() {
	for {
		select {
		case f := <-c.actionch:
			f()
		}
	}
}

// https://stackoverflow.com/questions/50056144/check-for-internet-connection-from-application
func isConnected() (ok bool) {
	_, err := http.Get("http://clients3.google.com/generate_204")
	if err != nil {
		return false
	}
	return true
}