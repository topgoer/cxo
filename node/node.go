package node

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/skycoin/skycoin/src/cipher"

	"github.com/skycoin/cxo/data"
	"github.com/skycoin/cxo/skyobject"

	"github.com/skycoin/cxo/node/gnet"
	"github.com/skycoin/cxo/node/log"
)

var (
	// ErrTimeout occurs when a request that waits response tooks too long
	ErrTimeout = errors.New("timeout")
	// ErrSubscriptionRejected means that remote peer rejects our subscription
	ErrSubscriptionRejected = errors.New("subscription rejected by remote peer")
	// ErrNilConnection means that you tries to subscribe or request list of
	// feeds from a nil-connection
	ErrNilConnection = errors.New("subscribe to nil connection")
	// ErrUnexpectedResponse occurs if a remote peer sends any unexpected
	// response for our request
	ErrUnexpectedResponse = errors.New("unexpected response")
	// ErrNonPublicPeer occurs if a remote peer can't give us list of
	// feeds because it is not public
	ErrNonPublicPeer = errors.New(
		"request list of feeds from non-public peer")
)

type fillRoot struct {
	root  *skyobject.Root     // filling the root to send forward
	c     *gnet.Conn          // from which the root received
	await skyobject.Reference // waiting for
}

// A Node represents CXO P2P node
// that includes RPC server if enabled
// by configs
type Node struct {
	// logger of the server
	log.Logger

	src msgSource

	// configuratios
	conf Config

	// database
	db data.DB

	// skyobject
	so *skyobject.Container

	// feeds
	fmx   sync.RWMutex
	feeds map[cipher.PubKey]map[*gnet.Conn]struct{}

	// pending subscriptions
	// (while a node subscribes to feed of another node
	// the first node sends SubscrieMsg and waits for
	// accept or reject
	pmx     sync.Mutex
	pending map[*gnet.Conn]map[cipher.PubKey]struct{}

	rmx   sync.RWMutex
	roots []*fillRoot // filling up

	// request/response replies
	rpmx      sync.Mutex
	responses map[uint32]chan Msg

	// connections
	pool *gnet.Pool
	rpc  *RPC // rpc server

	// closing
	quit  chan struct{}
	quito sync.Once

	done  chan struct{} // when quit done
	doneo sync.Once

	await sync.WaitGroup
}

// NewNode creates new Node instnace using given
// configurations. The functions creates database and
// Container of skyobject instances internally
func NewNode(sc Config) (s *Node, err error) {
	s, err = NewNodeReg(sc, nil)
	return
}

// NewNodeReg creates new Node instance using given
// skyobject.Registry to create container
func NewNodeReg(sc Config, reg *skyobject.Registry) (s *Node,
	err error) {

	// database

	var db data.DB
	if sc.InMemoryDB {
		db = data.NewMemoryDB()
	} else {
		if sc.DataDir != "" {
			if err = initDataDir(sc.DataDir); err != nil {
				return
			}
		}
		if db, err = data.NewDriveDB(sc.DBPath); err != nil {
			return
		}
	}

	// container

	var so *skyobject.Container
	so = skyobject.NewContainer(db, reg)

	// node instance

	s = new(Node)

	s.Logger = log.NewLoggerByConfig(sc.Log)
	s.conf = sc

	s.db = db

	s.so = so
	s.feeds = make(map[cipher.PubKey]map[*gnet.Conn]struct{})

	s.pending = make(map[*gnet.Conn]map[cipher.PubKey]struct{})

	s.responses = make(map[uint32]chan Msg)

	// fill up feeds from database
	for _, pk := range s.db.Feeds() {
		s.feeds[pk] = make(map[*gnet.Conn]struct{})
	}

	if sc.Config.Logger == nil {
		sc.Config.Logger = s.Logger // use the same logger
	}

	// gnet related callbacks
	if ch := sc.Config.OnCreateConnection; ch == nil {
		sc.Config.OnCreateConnection = s.connectHandler
	} else {
		sc.Config.OnCreateConnection = func(c *gnet.Conn) {
			s.connectHandler(c)
			ch(c)
		}
	}
	if dh := sc.Config.OnCloseConnection; dh == nil {
		sc.Config.OnCloseConnection = s.disconnectHandler
	} else {
		sc.Config.OnCloseConnection = func(c *gnet.Conn) {
			s.disconnectHandler(c)
			dh(c)
		}
	}
	if dc := sc.Config.OnDial; dc == nil {
		sc.Config.OnDial = s.onDial
	} else {
		sc.Config.OnDial = func(c *gnet.Conn, err error) error {
			if err = dc(c, err); err != nil {
				return err
			}
			return s.onDial(c, err)
		}
	}

	if s.pool, err = gnet.NewPool(sc.Config); err != nil {
		s = nil
		return
	}

	if sc.EnableRPC {
		s.rpc = newRPC(s)
	}

	s.quit = make(chan struct{})
	s.done = make(chan struct{})

	if err = s.start(); err != nil {
		s.Close()
		s = nil
	}
	return
}

// Start wes deprecated. This methods does nothing.
//
// Deprecated: just remove Start() invokation
func (*Node) Start() (_ error) {
	///
	return
}

func (s *Node) start() (err error) {
	s.Debugf(`starting node:
    data dir:             %s

    max connections:      %d
    max message size:     %d

    dial timeout:         %v
    read timeout:         %v
    write timeout:        %v

    ping interval:        %v

    read queue:           %d
    write queue:          %d

    redial timeout:       %d
    max redial timeout:   %d
    dials limit:          %d

    read buffer:          %d
    write buffer:         %d

    TLS:                  %v

    enable RPC:           %v
    RPC address:          %s
    listening address:    %s
    enable listening:     %v
    remote close:         %t

    in-memory DB:         %v
    DB path:              %s

    gc interval:          %v

    debug:                %#v
`,
		s.conf.DataDir,
		s.conf.MaxConnections,
		s.conf.MaxMessageSize,

		s.conf.DialTimeout,
		s.conf.ReadTimeout,
		s.conf.WriteTimeout,

		s.conf.PingInterval,

		s.conf.ReadQueueLen,
		s.conf.WriteQueueLen,

		s.conf.RedialTimeout,
		s.conf.MaxRedialTimeout,
		s.conf.DialsLimit,

		s.conf.ReadBufferSize,
		s.conf.WriteBufferSize,

		s.conf.TLSConfig != nil,

		s.conf.EnableRPC,
		s.conf.RPCAddress,
		s.conf.Listen,
		s.conf.EnableListener,
		s.conf.RemoteClose,

		s.conf.InMemoryDB,
		s.conf.DBPath,

		s.conf.GCInterval,

		s.conf.Log.Debug,
	)

	// start listener
	if s.conf.EnableListener == true {
		if err = s.pool.Listen(s.conf.Listen); err != nil {
			return
		}
		s.Print("listen on ", s.pool.Address())
	}

	// start rpc listener if need
	if s.conf.EnableRPC == true {
		if err = s.rpc.Start(s.conf.RPCAddress); err != nil {
			s.pool.Close()
			return
		}
		s.Print("rpc listen on ", s.rpc.Address())
	}

	if s.conf.PingInterval > 0 {
		s.await.Add(1)
		go s.pingsLoop()
	}

	if s.conf.GCInterval > 0 {
		s.await.Add(1)
		go s.gcLoop()
	}

	return
}

// Close the Node
func (s *Node) Close() (err error) {

	s.quito.Do(func() {
		close(s.quit)
	})

	err = s.pool.Close()

	if s.conf.EnableRPC {
		s.rpc.Close()
	}

	s.await.Wait()

	// we have to close boltdb once
	s.doneo.Do(func() {

		// clean up database
		for feed := range s.feeds {
			s.so.RemoveNonFullRoots(feed)
		}
		s.so.GC(true) // true == preserve root objects

		// close database after all, otherwise, it panics
		s.db.Close()

		// close the Quiting channel
		close(s.done)
	})

	return
}

func maxDuration(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}

func (s *Node) pingsLoop() {
	defer s.await.Done()

	tk := time.NewTicker(s.conf.PingInterval)
	defer tk.Stop()

	for {
		select {
		case <-tk.C:
			now := time.Now()
			for _, c := range s.pool.Connections() {
				md := maxDuration(now.Sub(c.LastRead()), now.Sub(c.LastWrite()))
				if md < s.conf.PingInterval {
					continue
				}
				s.sendPingMsg(c)
			}
		case <-s.quit:
			return
		}
	}
}

func (s *Node) gcLoop() {
	defer s.await.Done()

	tk := time.NewTicker(s.conf.GCInterval)
	defer tk.Stop()

	s.Debug("start GC loop ", s.conf.GCInterval)
	for {
		select {
		case <-tk.C:
			tp := time.Now()
			s.Debug("GC pause")
			s.so.GC(false)
			s.Debug("GC done ", time.Now().Sub(tp))
		case <-s.quit:
			return
		}
	}

}

// send a message to given connection
func (s *Node) sendMessage(c *gnet.Conn, msg Msg) (ok bool) {
	return s.sendEncodedMessage(c, fmt.Sprintf("%T", msg), Encode(msg))
}

func (s *Node) sendEncodedMessage(c *gnet.Conn, name string,
	msg []byte) (ok bool) {

	// the name argument used for Debug logs

	s.Debugf("send message %s to %s", name, c.Address())

	select {
	case c.SendQueue() <- msg:
		ok = true
	case <-c.Closed():
	default:
		s.Printf("[ERR] %s send queue full", c.Address())
		c.Close()
	}
	return
}

func boolString(t bool, ts, fs string) string {
	if t {
		return ts
	}
	return fs
}

func (s *Node) connectHandler(c *gnet.Conn) {
	s.Debugf("got new %s connection %s %s",
		boolString(c.IsIncoming(), "incoming", "outgoing"),
		boolString(c.IsIncoming(), "from", "to"),
		c.Address())
	// handle
	s.await.Add(1)
	go s.handleConnection(c)
}

func (s *Node) disconnectHandler(c *gnet.Conn) {
	s.Debugf("closed connection %s", c.Address())
}

// delete connection from feeds
func (s *Node) deleteConnFromFeeds(c *gnet.Conn) {
	s.fmx.Lock()
	defer s.fmx.Unlock()

	for _, cs := range s.feeds {
		delete(cs, c)
	}
}

// delete connection from pendings
func (s *Node) deleteConnFromPending(c *gnet.Conn) {
	s.pmx.Lock()
	defer s.pmx.Unlock()

	delete(s.pending, c)
}

// close a connection removing associated resources
func (s *Node) close(c *gnet.Conn) {
	s.deleteConnFromFeeds(c)
	s.deleteConnFromPending(c)
	c.Close()
}

func (s *Node) handleConnection(c *gnet.Conn) {
	s.Debug("handle connection ", c.Address())
	defer s.Debug("stop handling connection", c.Address())

	defer s.await.Done()
	defer s.close(c)

	var (
		closed  = c.Closed()
		receive = c.ReceiveQueue()

		data []byte
		msg  Msg

		err error
	)

	for {
		select {
		case <-closed:
			return
		case data = <-receive:
			if msg, err = Decode(data); err != nil {
				s.Printf("[ERR] %s decoding message: %v", c.Address(), err)
				return
			}
			s.handleMsg(c, msg)
		}
	}

}

func shortHex(a string) string {
	return string([]byte(a)[:7])
}

func (s *Node) subscribeConn(c *gnet.Conn, feed cipher.PubKey) (accept,
	already bool) {

	s.fmx.Lock()
	defer s.fmx.Unlock()

	if cs, ok := s.feeds[feed]; ok {
		if _, already = cs[c]; already {
			return
		}
		// call OnSubscribeRemote callback and check out its reply
		if callback := s.conf.OnSubscribeRemote; callback != nil {
			if reject := callback(c, feed); reject != nil {
				s.Debugln("remote subscription rejected by OnSubscribeRemote:",
					reject)
				return // false, false
			}
		}
		cs[c], accept = struct{}{}, true
	}

	return // no such feed
}

func (s *Node) sendLastFullRoot(c *gnet.Conn, feed cipher.PubKey) (sent bool) {
	if full := s.so.LastFullRoot(feed); full != nil {
		sent = s.sendRootMsg(c, feed, full.Encode())
	}
	return
}

func (s *Node) handleSubscribeMsg(c *gnet.Conn, msg *SubscribeMsg) {
	s.Debugln("handleSubscribeMsg", c.Address(), shortHex(msg.Feed.Hex()))

	// (1) subscribe if the Node shares feed and send AcceptSubscriptionMsg back
	//     and send latest full root of the feed if has
	// (2) send AcceptSubscriptionMsg back if the connection already
	//     subscibed to the feed
	// (3) send  RejectSubscriptionMsg if the Node doesn't share feed
	if accept, already := s.subscribeConn(c, msg.Feed); already == true {
		// (2)
		s.sendAcceptSubscriptionMsg(c, msg.ID(), msg.Feed)
		return
	} else if accept == true {
		// (1)
		if s.sendAcceptSubscriptionMsg(c, msg.ID(), msg.Feed) {
			s.sendLastFullRoot(c, msg.Feed)
		}
		return
	}
	s.sendRejectSubscriptionMsg(c, msg.ID(), msg.Feed) // (3)
}

func (s *Node) handleUnsubscribeMsg(c *gnet.Conn, msg *UnsubscribeMsg) {
	s.Debugln("handleUnsubscribeMsg", c.Address(), shortHex(msg.Feed.Hex()))

	// just unsubscribe if subscribed
	s.fmx.Lock()
	defer s.fmx.Unlock()

	if cs, ok := s.feeds[msg.Feed]; ok {
		if _, ok = cs[c]; ok {
			// trigger OnUnsubscribeRemote callback only
			// if we have the subcription from the
			// remote peer
			if callack := s.conf.OnUnsubscribeRemote; callack != nil {
				callack(c, msg.Feed)
			}
			delete(cs, c)
		}
	}
}

// the function deletes given conn->feed from pendings
// and returns true if there was
func (s *Node) deleteConnFeedFromPending(c *gnet.Conn,
	feed cipher.PubKey) (ok bool) {

	s.pmx.Lock()
	defer s.pmx.Unlock()

	var cf map[cipher.PubKey]struct{}
	if cf, ok = s.pending[c]; !ok {
		return // no such conn->feed in pending
	}
	if _, ok = cf[feed]; !ok {
		return // no such conn->feed in pending
	}
	if len(cf) == 1 {
		delete(s.pending, c)
		return
	}
	delete(cf, feed)
	return
}

func (s *Node) onDial(c *gnet.Conn, _ error) (_ error) {
	if val := c.Value(); val == nil {
		return
	} else if rs, ok := val.([]cipher.PubKey); !ok {
		s.Debugf("wrong type of associated Value of gnet.Conn (%s): %T",
			c.Address(), val)
	} else {
		for _, feed := range rs {
			s.addToPending(c, feed)     // TODO: pending ?
			s.sendSubscribeMsg(c, feed) // resubscribe
		}
	}
	return
}

func (s *Node) addToResubscriptions(c *gnet.Conn, feed cipher.PubKey) {
	if val := c.Value(); val == nil {
		c.SetValue([]cipher.PubKey{feed})
		return
	} else if rs, ok := val.([]cipher.PubKey); !ok {
		s.Debugf("wrong type of associated Value of gnet.Conn (%s): %T",
			c.Address(), val)
	} else {
		c.SetValue(append(rs, feed))
	}
}

func (s *Node) handleAcceptSubscriptionMsg(c *gnet.Conn,
	msg *AcceptSubscriptionMsg) {

	s.Debugln("handleAcceptSubscriptionMsg", c.Address(),
		shortHex(msg.Feed.Hex()))

	// if subscription had been accepted then we
	// need to subscribe remote peer our side

	// But (!) we must not subscribe a remote peer if we
	// receive an AcceptSubscriptionMsg but we didn't send
	// SubscribeMsg to the remote peer before

	if !s.deleteConnFeedFromPending(c, msg.Feed) {
		s.Debug("unexpected AcceptSubscriptionMsg from ", c.Address())
		return
	}

	// subscribe the remote peer to the subscription
	if ok, _ := s.subscribeConn(c, msg.Feed); ok {
		// susbcribeConn returns (accept, alreaady) where
		// already is false if accept is true and vise versa;
		// thus if the ok is true then we can ignore already,
		// because it is false
		s.sendLastFullRoot(c, msg.Feed)

		// add the subscription to list of resubscribtions
		// if connection fails
		s.addToResubscriptions(c, msg.Feed)

		// call OnSubscriptionAccepted callback
		if callback := s.conf.OnSubscriptionAccepted; callback != nil {
			callback(c, msg.Feed)
		}
	}

	// else -> seems the feed was removed from the node

}

func delFromListOfFeeds(list []cipher.PubKey,
	feed cipher.PubKey) []cipher.PubKey {

	var i int
	for _, x := range list {
		if x == feed {
			continue // delete
		}
		list[i] = x
		i++
	}
	return list[:i]
}

func (s *Node) removeFromResubscriptions(c *gnet.Conn, feed cipher.PubKey) {
	if val := c.Value(); val == nil {
		return
	} else if rs, ok := val.([]cipher.PubKey); !ok {
		s.Debugf("wrong type of associated Value of gnet.Conn (%s): %T",
			c.Address(), val)
	} else {
		c.SetValue(delFromListOfFeeds(rs, feed))
	}
}

func (s *Node) handleRejectSubscriptionMsg(c *gnet.Conn,
	msg *RejectSubscriptionMsg) {

	// remove from pending and call OnSubscriptionRejected callback;
	// remove from resubscriptions

	if !s.deleteConnFeedFromPending(c, msg.Feed) {
		s.Debug("unexpected RejectSubscriptionMsg from ", c.Address())
		return
	}

	s.removeFromResubscriptions(c, msg.Feed)

	if callback := s.conf.OnSubscriptionRejected; callback != nil {
		callback(c, msg.Feed)
	}
}

func (s *Node) sendToFeed(feed cipher.PubKey, msg Msg, except *gnet.Conn) {

	var (
		data = Encode(msg)            // encode once
		name = fmt.Sprintf("%T", msg) // name for debug logs
	)

	s.fmx.RLock()
	defer s.fmx.RUnlock()

	for c := range s.feeds[feed] {
		if c == except {
			continue
		}
		s.sendEncodedMessage(c, name, data) // send many times the same slice
	}
}

func (s *Node) addNonFullRoot(root *skyobject.Root,
	c *gnet.Conn) (fl *fillRoot) {

	fl = &fillRoot{root, c, skyobject.Reference{}}
	s.roots = append(s.roots, fl)
	return
}

func (s *Node) delNonFullRoot(root *skyobject.Root) {
	for i, fl := range s.roots {
		if fl.root == root {
			copy(s.roots[i:], s.roots[i+1:])
			s.roots[len(s.roots)-1] = nil // set to nil for golang GC
			s.roots = s.roots[:len(s.roots)-1]
			return
		}
	}
	return
}

func (s *Node) hasFeed(pk cipher.PubKey) (yep bool) {
	s.fmx.RLock()
	defer s.fmx.RUnlock()

	_, yep = s.feeds[pk]
	return
}

func (s *Node) handleRootMsg(c *gnet.Conn, msg *RootMsg) {
	if !s.hasFeed(msg.Feed) {
		s.Debug("reject root: not subscribed")
		return
	}

	root, err := s.so.AddRootPack(&msg.RootPack)
	if err != nil {
		if err == data.ErrRootAlreadyExists {
			s.Debug("reject root: alredy have this root")
			return
		}
		s.Print("[ERR] error appending root: ", err)
		return
	}

	// callback
	if orr := s.conf.OnRootReceived; orr != nil {
		orr(s.Container().wrapRoot(root))
	}

	if root.IsFull() {

		// callback
		if orf := s.conf.OnRootFilled; orf != nil {
			orf(s.Container().wrapRoot(root))
		}

		s.sendToFeed(msg.Feed, msg, c)
		return
	}

	s.rmx.Lock()
	defer s.rmx.Unlock()

	fl := s.addNonFullRoot(root, c)
	if !root.HasRegistry() {
		if !s.sendRequestRegistryMsg(c, root.RegistryReference()) {
			s.delNonFullRoot(root) // sending error (connection closed)
		}
		return
	}

	err = root.WantFunc(func(ref skyobject.Reference) error {
		if !s.sendRequestDataMsg(c, ref) {
			s.delNonFullRoot(root) // sending error (connection closed)
		} else {
			fl.await = ref // keep last requested reference
		}
		return skyobject.ErrStopRange
	})
	if err != nil {
		s.Print("[ERR] unexpected error: ", err)
	}

}

func (s *Node) handleRequestRegistryMsg(c *gnet.Conn,
	msg *RequestRegistryMsg) {

	if encReg, ok := s.db.Get(cipher.SHA256(msg.Ref)); ok {
		s.sendRegistryMsg(c, encReg)
	}
}

func (s *Node) handleRegistryMsg(c *gnet.Conn, msg *RegistryMsg) {
	reg, err := skyobject.DecodeRegistry(msg.Reg)
	if err != nil {
		s.Print("[ERR] error decoding received registry:", err)
		return
	}

	if !s.so.WantRegistry(reg.Reference()) {
		return // don't want the registry
	}

	s.so.AddRegistry(reg)

	s.rmx.Lock()
	defer s.rmx.Unlock()

	var i int // index for deleting
	for _, fl := range s.roots {

		if fl.root.RegistryReference() == reg.Reference() {

			if fl.root.IsFull() {

				// callback
				if orf := s.conf.OnRootFilled; orf != nil {
					orf(s.Container().wrapRoot(fl.root))
				}

				s.sendToFeed(fl.root.Pub(), s.src.NewRootMsg(
					fl.root.Pub(),    // feed
					fl.root.Encode(), // root pack
				), fl.c)
				continue // delete

			}

			var sent bool
			err = fl.root.WantFunc(func(ref skyobject.Reference) error {
				if sent = s.sendRequestDataMsg(c, ref); sent {
					fl.await = ref
				}
				return skyobject.ErrStopRange
			})
			if err != nil {
				s.Print("[ERR] unexpected error: ", err)
				continue // delete
			}
			if !sent {
				continue // delete
			}
		}
		s.roots[i] = fl
		i++

	}
	s.roots = s.roots[:i]
}

func (s *Node) handleRequestDataMsg(c *gnet.Conn, msg *RequestDataMsg) {
	if data, ok := s.so.Get(msg.Ref); ok {
		s.sendDataMsg(c, data)
	}
}

func (s *Node) handleDataMsg(c *gnet.Conn, msg *DataMsg) {
	hash := skyobject.Reference(cipher.SumSHA256(msg.Data))

	s.rmx.Lock()
	defer s.rmx.Unlock()

	// does the Server really want the data
	var want bool
	for _, fl := range s.roots {
		if fl.await == hash {
			want = true
			break
		}
	}
	if !want {
		return // doesn't want the data
	}
	s.so.Set(hash, msg.Data) // save

	// check filling
	var i int // index for deleting
	for _, fl := range s.roots {

		if fl.await == hash {

			if fl.root.IsFull() {

				// callback
				if orf := s.conf.OnRootFilled; orf != nil {
					orf(s.Container().wrapRoot(fl.root))
				}

				s.sendToFeed(fl.root.Pub(), s.src.NewRootMsg(
					fl.root.Pub(),    // feed
					fl.root.Encode(), // root pack
				), fl.c)
				continue // delete

			}

			var sent bool
			err := fl.root.WantFunc(func(ref skyobject.Reference) error {
				if sent = s.sendRequestDataMsg(c, ref); sent {
					fl.await = ref
				}
				return skyobject.ErrStopRange
			})
			if err != nil {
				s.Print("[ERR] unexpected error: ", err)
				continue // delete
			}
			if !sent {
				continue // delete
			}

		}
		s.roots[i] = fl
		i++

	}
	s.roots = s.roots[:i]
}

func (s *Node) handleRequestListOfFeedsMsg(c *gnet.Conn,
	x *RequestListOfFeedsMsg) {

	if s.conf.PublicServer == true {
		s.sendListOfFeedsMsg(c, x.ID(), s.Feeds())
	} else {
		s.sendNonPublicServerMsg(c, x.ID()) // reject
	}
}

func (s *Node) handlePingMsg(c *gnet.Conn) {
	s.sendPongMsg(c)
}

func (s *Node) handleMsg(c *gnet.Conn, msg Msg) {
	s.Debugf("handle message %T from %s", msg, c.Address())

	switch x := msg.(type) {

	//
	// subscribe/unsubscribe
	//

	// subscribe/unsubscribe
	case *SubscribeMsg:
		s.handleSubscribeMsg(c, x)
	case *UnsubscribeMsg:
		s.handleUnsubscribeMsg(c, x)

	// relies for subscribing
	case *AcceptSubscriptionMsg:
		s.handleAcceptSubscriptionMsg(c, x)
	case *RejectSubscriptionMsg:
		s.handleRejectSubscriptionMsg(c, x)

	//
	// root, data, registry, requests
	//

	// root
	case *RootMsg:
		s.handleRootMsg(c, x)

	// registry
	case *RequestRegistryMsg:
		s.handleRequestRegistryMsg(c, x)
	case *RegistryMsg:
		s.handleRegistryMsg(c, x)

	//data
	case *RequestDataMsg:
		s.handleRequestDataMsg(c, x)
	case *DataMsg:
		s.handleDataMsg(c, x)

	//
	// public servers
	//

	case *RequestListOfFeedsMsg:
		s.handleRequestListOfFeedsMsg(c, x)
	case *ListOfFeedsMsg:
		// do ntohing (handled at the bottom of this method)
	case *NonPublicServerMsg:
		// do ntohing (handled at the bottom of this method)

	//
	// ping / pong
	//

	// ping/pong
	case *PingMsg:
		s.handlePingMsg(c)
	case *PongMsg:
		// do nothing

	// critical
	default:
		s.Printf("[CRIT] unhandled message type %T", msg)
	}

	// the msg is not request that need identified response
	if msg.ResponseFor() == 0 {
		return
	}

	// process responses after handling

	var rc chan Msg
	var ok bool
	if rc, ok = s.takeWaitingForResponse(msg.ResponseFor()); ok {
		rc <- msg
	}
}

//
// Public methods of the Node
//

// Pool returns underlying *gnet.Pool.
// It returns nil if the Node is not started
// yet. Use methods of this Pool to manipulate
// connections: Dial, Connection, Connections,
// Address, etc
func (s *Node) Pool() *gnet.Pool {

	// locks: no

	return s.pool
}

func (s *Node) addFeed(feed cipher.PubKey) (already bool) {
	s.fmx.Lock()
	defer s.fmx.Unlock()

	if _, already = s.feeds[feed]; !already {
		s.so.AddFeed(feed)
		s.feeds[feed] = make(map[*gnet.Conn]struct{})
	}
	return
}

func (s *Node) addToPending(c *gnet.Conn, feed cipher.PubKey) {
	s.pmx.Lock()
	defer s.pmx.Unlock()

	var ps map[cipher.PubKey]struct{}
	var ok bool
	if ps, ok = s.pending[c]; !ok {
		ps = make(map[cipher.PubKey]struct{})
		s.pending[c] = ps
	}
	ps[feed] = struct{}{} // add anyway
}

// is given connection already susbcribed to given feed
func (s *Node) isAlreadySusbcribed(c *gnet.Conn,
	feed cipher.PubKey) (yep bool) {

	s.fmx.RLock()
	defer s.fmx.RUnlock()

	if cs, ok := s.feeds[feed]; ok {
		_, yep = cs[c]
	}
	return
}

// Subscribe to given feed. If given connection is nil, then this subscription
// is local. Otherwise, it subscribes to a remote peer. To handle result use
// (Config).OnAcceptSubsctiption and OnDeniedSubscription callbacks. The
// connection must be from the gnet.Pool of the Node. To subscribe to the same
// feed of many remote peers call the method many times for every connection
// you want. To make the server subscribed to a feed (even if it is not
// conencted to any remote peer) call this method with nil. To obtain
// *gnet.Conn use (*Node).Pool() methods like
// (*net.Pool).Connection(address string) (*gnet.Conn). This method
// never sends any mesages if given peer already subscribed to given
// feed
func (s *Node) Subscribe(c *gnet.Conn, feed cipher.PubKey) {

	// locks: s.fmx Lock/Unlock and Rlock/RUnlock
	//        s.pmx Lock/Unlock
	//
	// see below for orders

	// subscribe the Node to the feed, create feed in database if not exists
	//
	// lock: s.fmx Lock/Unlock
	//
	already := s.addFeed(feed)
	// just return if we don't want to subscribe to feed of a remote peer
	if c == nil {
		return
	}
	// don't send the message if remote peer already subscribed to
	// the feed (don't subscribe twice); if the already is true then
	// we already have the feed and it's possible the c is subscribed
	// to the feed already; but if the already is false, then this
	// feed is fresh and we don't have any subscribed remote peer
	//
	// locks: s.fmx RLock/RUnlock
	//
	if already && s.isAlreadySusbcribed(c, feed) {
		return // don't subscribe twice
	}
	// add conn->feed to pendings
	//
	// locks: s.pmx Lock/Unlock
	//
	s.addToPending(c, feed)
	// send SubscribeMsg
	s.sendSubscribeMsg(c, feed)
	return
}

// delte (any connection)->feed from all pending subscriptions
func (s *Node) deleteFeedFromPending(feed cipher.PubKey) {
	s.pmx.Lock()
	defer s.pmx.Unlock()

	for c, ps := range s.pending {
		delete(ps, feed)
		if len(ps) == 0 {
			delete(s.pending, c)
		}
	}
}

// delte all filling root objects of a feed
func (s *Node) deleteFeedFromFilling(feed cipher.PubKey) {
	s.rmx.Lock()
	defer s.rmx.Unlock()

	var i int
	for _, fl := range s.roots {
		if fl.root.Pub() == feed {
			continue // delete
		}
		i++
		s.roots[i] = fl
	}
	s.roots = s.roots[:i]
}

// delete a feed and all associated resources without sending UnsubscribeMsg
// to peers; the sending is not palced in the method to unlock fmx mutex
func (s *Node) deleteFeed(feed cipher.PubKey) (cs map[*gnet.Conn]struct{}) {
	s.fmx.Lock()
	defer s.fmx.Unlock()

	var ok bool
	if cs, ok = s.feeds[feed]; ok {
		delete(s.feeds, feed)
		s.deleteFeedFromPending(feed)
		s.deleteFeedFromFilling(feed)
		s.so.DelFeed(feed) // delete from database
	}
	return
}

// total unsubscribing; delete given feed and all associated resources,
// send UnsubscribeMsg to peers that share the feed
func (s *Node) unsubscribe(feed cipher.PubKey) {
	// we can't use sendToFeed here
	var (
		msg   Msg = s.src.NewUnsubscribeMsg(feed)
		unsub     = Encode(msg)
		name      = fmt.Sprintf("%T", msg)
	)
	for peer := range s.deleteFeed(feed) {
		s.sendEncodedMessage(peer, name, unsub)
	}
}

func (s *Node) deleteConnFeedFromFeeds(c *gnet.Conn, feed cipher.PubKey) {

	s.fmx.Lock()
	defer s.fmx.Unlock()

	if cs, ok := s.feeds[feed]; ok {
		delete(cs, c)
	}
}

// Unsubscribe from a feed of a remote peer or from all remote peers and
// locally too if given gnet.Conn is nil. Given *gnet.Conn must be from
// *gnet.Pool of this Node. Unsubscribe with nil removes feed from
// underlying database and the Node stops sharing the feed
func (s *Node) Unsubscribe(c *gnet.Conn, feed cipher.PubKey) {

	// locks: s.fmx Lock/Unlock
	//        s.pmx Lock/Unlock
	//        s.rmx Lock/Unlock
	//
	// see blow for lock orders

	if c == nil {
		// (1)
		// locks: s.fmx Lock/Unlock
		//        s.pmx Lock/Unlock (under fmx)
		//        s.rmx Lock/Unlock (under fmx)
		s.unsubscribe(feed)
		return
	}

	// (2)
	// locks: s.pmx Lock/Unlock
	//        s.fmx Lock/Unlock

	// 1. remove the conn->feed from pendings
	s.deleteConnFeedFromPending(c, feed)
	// 2. remove the conn from s.feeds->feed
	s.deleteConnFeedFromFeeds(c, feed)
	// 3. send UnsubscribeMsg to peer
	s.sendUnsubscribeMsg(c, feed)
}

// TODO: + Want per root of a feed

// Want returns lits of objects related to given
// feed that the server hasn't got but knows about
func (s *Node) Want(feed cipher.PubKey) (wn []cipher.SHA256) {

	// locks: no (skyobject)

	set := make(map[skyobject.Reference]struct{})
	s.so.WantFeed(feed, func(k skyobject.Reference) error {
		set[k] = struct{}{}
		return nil
	})
	if len(set) == 0 {
		return
	}
	wn = make([]cipher.SHA256, 0, len(set))
	for k := range set {
		wn = append(wn, cipher.SHA256(k))
	}
	return
}

// TODO: + Got per root of a feed

// Got returns lits of objects related to given
// feed that the server has got
func (s *Node) Got(feed cipher.PubKey) (gt []cipher.SHA256) {

	// locks: no (skyobject)

	set := make(map[skyobject.Reference]struct{})
	s.so.GotFeed(feed, func(k skyobject.Reference) error {
		set[k] = struct{}{}
		return nil
	})
	if len(set) == 0 {
		return
	}
	gt = make([]cipher.SHA256, 0, len(set))
	for k := range set {
		gt = append(gt, cipher.SHA256(k))
	}
	return
}

// Feeds the server share
func (s *Node) Feeds() (fs []cipher.PubKey) {

	// locks: s.fmx RLock/RUnlock

	s.fmx.RLock()
	defer s.fmx.RUnlock()

	if len(s.feeds) == 0 {
		return
	}
	fs = make([]cipher.PubKey, 0, len(s.feeds))
	for f := range s.feeds {
		fs = append(fs, f)
	}
	return
}

// Quiting returns cahnnel that closed
// when the Node closed
func (s *Node) Quiting() <-chan struct{} {
	return s.done // when quit done
}

//
// request response
//

func (s *Node) addWaitingForResponse(id uint32, rc chan Msg) {
	s.rpmx.Lock()
	defer s.rpmx.Unlock()

	s.responses[id] = rc
}

func (s *Node) takeWaitingForResponse(id uint32) (rc chan Msg, ok bool) {
	s.rpmx.Lock()
	defer s.rpmx.Unlock()

	if rc, ok = s.responses[id]; ok {
		delete(s.responses, id)
	}
	return
}

func (s *Node) sendMsgAndWaitForResponse(c *gnet.Conn,
	msg Msg, timeout time.Duration) (response Msg, err error) {

	var (
		tm *time.Timer
		tc <-chan time.Time
		rc = make(chan Msg, 1) // don't block sender
	)

	if timeout > 0 {
		tm = time.NewTimer(timeout)
		defer tm.Stop()
		tc = tm.C
	}

	s.addWaitingForResponse(msg.ID(), rc)
	defer s.takeWaitingForResponse(msg.ID())

	s.sendMessage(c, msg)

	select {
	case <-tc:
		err = ErrTimeout
	case response = <-rc:
	}
	return
}

// SubscribeResponse is similar to subscribe but it requires non-nil connection
// and waits for reply from remote peer. It waits for response
// Config.ResponseTimeout. Unlike Subsribe it can subscribe twice or
// many times sending mesages and waiting response
func (s *Node) SubscribeResponse(c *gnet.Conn, feed cipher.PubKey) error {

	// locks: s.fmx  Lock/Unlock
	//        s.pmx  Lock/Unlock
	//        s.rpmx Lock/Unlock (twice)

	return s.SubscribeResponseTimeout(c, feed, s.conf.ResponseTimeout)
}

// SubscribeResponseTimeout uses provided timeout instead of configured
func (s *Node) SubscribeResponseTimeout(c *gnet.Conn, feed cipher.PubKey,
	timeout time.Duration) (err error) {

	// locks: s.fmx  Lock/Unlock
	//        s.pmx  Lock/Unlock
	//        s.rpmx Lock/Unlock (twice)

	if c == nil {
		err = ErrNilConnection
		return
	}

	// add feed
	s.addFeed(feed)

	// add to pending to make handling by handleAcceptSusbcriptionMsg
	// successful
	s.addToPending(c, feed)

	var response Msg
	response, err = s.sendMsgAndWaitForResponse(c,
		s.src.NewSubscribeMsg(feed),
		timeout)
	if err != nil {
		// delete from pending to not subscribe the connection on
		// timeout error; but this way remote peer can subscribe the
		// node anyway;
		// TODO: to send UnsusbcribeMsg or not to send, that
		//       is the fucking question (c) Hamlet
		s.deleteConnFeedFromPending(c, feed)
		return
	}

	// look at response
	typ := response.MsgType()
	if typ == RejectSubscriptionMsgType {
		err = ErrSubscriptionRejected
		return
	} else if typ == AcceptSubscriptionMsgType {
		return // nil
	}

	s.Debug("unexpected response for subscription: ", typ.String())
	err = ErrUnexpectedResponse
	return
}

// ListOfFeedsResponse reuqests list of feeds of a public server (peer).
// It receive connection to the server that should not be nil and must be
// form connections pool of this Node. It returns error if the server is
// not public or if the server not responding (timeout errror).
func (s *Node) ListOfFeedsResponse(c *gnet.Conn) ([]cipher.PubKey, error) {

	// locks: s.rpmx Lock/Unlock (twice)

	return s.ListOfFeedsResponseTimeout(c, s.conf.ResponseTimeout)
}

// ListOfFeedsResponseTimeout uses provided timeout instead of configured
func (s *Node) ListOfFeedsResponseTimeout(c *gnet.Conn,
	timeout time.Duration) (list []cipher.PubKey, err error) {

	// locks: s.rpmx Lock/Unlock (twice)

	if c == nil {
		err = ErrNilConnection
		return
	}

	var response Msg
	response, err = s.sendMsgAndWaitForResponse(c,
		s.src.NewRequestListOfFeedsMsg(),
		timeout)
	if err != nil {
		return
	}

	// look at response
	typ := response.MsgType()
	if typ == NonPublicServerMsgType {
		err = ErrNonPublicPeer
		return
	} else if typ == ListOfFeedsMsgType {
		list = response.(*ListOfFeedsMsg).List
		return
	}

	s.Debug("unexpected response for list of feeds requesting: ", typ.String())
	err = ErrUnexpectedResponse
	return

}

// RPCAddress returns address of RPC listener or an empty
// stirng if disabled
func (s *Node) RPCAddress() (address string) {
	if s.rpc != nil {
		address = s.rpc.Address()
	}
	return
}
