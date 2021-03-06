// Package gossip implements simple protocol to send new validated messages to all peers and ignore old or not valid messages.
package gossip

import (
	"sync"

	"github.com/libonomy/libonomy-light/common/types"
	"github.com/libonomy/libonomy-light/common/util"
	"github.com/libonomy/libonomy-light/log"
	"github.com/libonomy/libonomy-light/p2p/config"
	"github.com/libonomy/libonomy-light/p2p/metrics"
	"github.com/libonomy/libonomy-light/p2p/p2pcrypto"
	"github.com/libonomy/libonomy-light/p2p/service"
	"github.com/libonomy/libonomy-light/priorityq"
)

const oldMessageCacheSize = 10000
const propagateHandleBufferSize = 5000 // number of MessageValidation that we allow buffering, above this number protocols will get stuck

// Interface for the underlying p2p layer
type baseNetwork interface {
	SendMessage(peerPubkey p2pcrypto.PublicKey, protocol string, payload []byte) error
	SubscribePeerEvents() (conn chan p2pcrypto.PublicKey, disc chan p2pcrypto.PublicKey)
	ProcessGossipProtocolMessage(sender p2pcrypto.PublicKey, protocol string, data service.Data, validationCompletedChan chan service.MessageValidation) error
}

type prioQ interface {
	Write(prio priorityq.Priority, m interface{}) error
	Read() (interface{}, error)
	Close()
}

// Protocol runs the gossip protocol using the given peers and service.
type Protocol struct {
	log.Log

	config          config.SwarmConfig
	net             baseNetwork
	localNodePubkey p2pcrypto.PublicKey

	peers      map[p2pcrypto.PublicKey]*peer
	peersMutex sync.RWMutex

	shutdown chan struct{}

	oldMessageQ *types.DoubleCache

	propagateQ chan service.MessageValidation
	pq         prioQ
	priorities map[string]priorityq.Priority
}

// NewProtocol creates a new gossip protocol instance.
func NewProtocol(config config.SwarmConfig, base baseNetwork, localNodePubkey p2pcrypto.PublicKey, log2 log.Log) *Protocol {
	// intentionally not subscribing to peers events so that the channels won't block in case executing Start delays
	return &Protocol{
		Log:             log2,
		config:          config,
		net:             base,
		localNodePubkey: localNodePubkey,
		peers:           make(map[p2pcrypto.PublicKey]*peer),
		shutdown:        make(chan struct{}),
		oldMessageQ:     types.NewDoubleCache(oldMessageCacheSize), // todo : remember to drain this
		propagateQ:      make(chan service.MessageValidation, propagateHandleBufferSize),
		pq:              priorityq.New(propagateHandleBufferSize),
		priorities:      make(map[string]priorityq.Priority),
	}
}

// sender is an interface for peer's p2p layer
type sender interface {
	SendMessage(peerPubkey p2pcrypto.PublicKey, protocol string, payload []byte) error
}

// peer is a struct storing peer's state
type peer struct {
	log.Log
	pubkey p2pcrypto.PublicKey
	net    sender
}

func newPeer(net sender, pubkey p2pcrypto.PublicKey, log log.Log) *peer {
	return &peer{
		log,
		pubkey,
		net,
	}
}

// Close stops all protocol routines.
func (prot *Protocol) Close() {
	close(prot.shutdown)
}

// markMessageAsOld adds the message's hash to the old messages queue so that the message won't be processed in case received again.
// Returns true if message was already processed before
func (prot *Protocol) markMessageAsOld(h types.Hash12) bool {
	ok := prot.oldMessageQ.GetOrInsert(h)
	return ok
}

// send a message to all the peers.
func (prot *Protocol) propagateMessage(payload []byte, h types.Hash12, nextProt string, exclude p2pcrypto.PublicKey) {
	//TODO soon : don't wait for mesaage to send and if we finished sending last message one of the peers send the next message to him.
	// limit the number of simultaneous sends. *consider other messages (mainly sync)
	prot.peersMutex.RLock()
	peers := make([]p2pcrypto.PublicKey, 0, len(prot.peers))
	for p := range prot.peers {
		peers = append(peers, p)
	}
	prot.peersMutex.RUnlock()
	var wg sync.WaitGroup
peerLoop:
	for _, p := range peers {
		if exclude == p {
			continue peerLoop
		}
		wg.Add(1)
		go func(pubkey p2pcrypto.PublicKey) {
			// TODO: replace peer ?
			err := prot.net.SendMessage(pubkey, nextProt, payload)
			if err != nil {
				prot.With().Warning("Failed sending", log.String("protocol", nextProt), h.Field("hash"), log.String("to", pubkey.String()), log.Err(err))
			}
			wg.Done()
		}(p)
	}
	wg.Wait()
}

// Broadcast is the actual broadcast procedure - process the message internally and loop on peers and add the message to their queues
func (prot *Protocol) Broadcast(payload []byte, nextProt string) error {
	prot.Log.Debug("Broadcasting message from type %s", nextProt)
	return prot.processMessage(prot.localNodePubkey, nextProt, service.DataBytes{Payload: payload})
	//todo: should this ever return error ? then when processMessage should return error ?. should it block?
}

// Start a loop that process peers events
func (prot *Protocol) Start() {
	peerConn, peerDisc := prot.net.SubscribePeerEvents() // this was start blocks until we registered.
	go prot.eventLoop(peerConn, peerDisc)
	go prot.propagationEventLoop() // TODO consider running several consumers
}

func (prot *Protocol) addPeer(peer p2pcrypto.PublicKey) {
	prot.peersMutex.Lock()
	prot.peers[peer] = newPeer(prot.net, peer, prot.Log)
	prot.Log.With().Info("adding peer", log.String("peer", peer.String()))
	prot.peersMutex.Unlock()
}

func (prot *Protocol) removePeer(peer p2pcrypto.PublicKey) {
	prot.peersMutex.Lock()
	delete(prot.peers, peer)
	prot.Log.With().Info("deleting peer", log.String("peer", peer.String()))
	prot.peersMutex.Unlock()
}

func (prot *Protocol) processMessage(sender p2pcrypto.PublicKey, protocol string, msg service.Data) error {
	h := types.CalcMessageHash12(msg.Bytes(), protocol)

	isOld := prot.markMessageAsOld(h)
	if isOld {
		metrics.OldGossipMessages.With(metrics.ProtocolLabel, protocol).Add(1)
		// todo : - have some more metrics for termination
		// todo	: - maybe tell the peer we got this message already?
		// todo : - maybe block this peer since he sends us old messages
		prot.Log.With().Debug("old_gossip_message", log.String("from", sender.String()), log.String("protocol", protocol), log.String("hash", util.Bytes2Hex(h[:])))
		return nil
	}

	prot.Log.Event().Debug("new_gossip_message", log.String("from", sender.String()), log.String("protocol", protocol), log.String("hash", util.Bytes2Hex(h[:])))
	metrics.NewGossipMessages.With("protocol", protocol).Add(1)
	return prot.net.ProcessGossipProtocolMessage(sender, protocol, msg, prot.propagateQ)
}

func (prot *Protocol) handlePQ() {
	for {
		mi, err := prot.pq.Read()
		if err != nil {
			prot.With().Info("priority queue was closed, existing", log.Err(err))
			return
		}
		m, ok := mi.(service.MessageValidation)
		if !ok {
			prot.Error("could not convert to message validation, ignoring message")
			continue
		}
		h := types.CalcMessageHash12(m.Message(), m.Protocol())
		prot.Log.With().Debug("new_gossip_message_relay", log.String("protocol", m.Protocol()), log.String("hash", util.Bytes2Hex(h[:])))
		prot.propagateMessage(m.Message(), h, m.Protocol(), m.Sender())
	}
}

func (prot *Protocol) getPriority(protoName string) priorityq.Priority {
	v, exist := prot.priorities[protoName]
	if !exist {
		prot.With().Warning("note: no priority found for protocol", log.String("protoName", protoName))
		return priorityq.Low
	}

	return v
}

// pushes messages that passed validation into the priority queue
func (prot *Protocol) propagationEventLoop() {
	go prot.handlePQ()

	for {
		select {
		case msgV := <-prot.propagateQ:
			if err := prot.pq.Write(prot.getPriority(msgV.Protocol()), msgV); err != nil {
				prot.With().Error("fatal: could not write to priority queue", log.Err(err), log.String("protocol", msgV.Protocol()))
			}
			metrics.PropagationQueueLen.Set(float64(len(prot.propagateQ)))

		case <-prot.shutdown:
			prot.pq.Close()
			prot.Error("propagate event loop stopped: protocol shutdown")
			return
		}
	}
}

// Relay processes a message, if the message is new, it is passed for the protocol to validate and then propagated.
func (prot *Protocol) Relay(sender p2pcrypto.PublicKey, protocol string, msg service.Data) error {
	return prot.processMessage(sender, protocol, msg)
}

func (prot *Protocol) eventLoop(peerConn, peerDisc chan p2pcrypto.PublicKey) {
	// TODO: replace with p2p.Peers
	defer prot.Info("Gossip protocol shutdown")
	for {
		select {
		case peer, ok := <-peerConn:
			if !ok {
				return
			}
			go prot.addPeer(peer)
		case peer, ok := <-peerDisc:
			if !ok {
				return
			}
			go prot.removePeer(peer)
		case <-prot.shutdown:
			return
		}
	}
}

// peersCount returns the number of peers known to the protocol, used for testing only
func (prot *Protocol) peersCount() int {
	prot.peersMutex.RLock()
	cnt := len(prot.peers)
	prot.peersMutex.RUnlock()
	return cnt
}

// hasPeer returns whether or not a peer is known to the protocol, used for testing only
func (prot *Protocol) hasPeer(key p2pcrypto.PublicKey) bool {
	prot.peersMutex.RLock()
	_, ok := prot.peers[key]
	prot.peersMutex.RUnlock()
	return ok
}

// SetPriority sets the priority for protoName in the queue.
func (prot *Protocol) SetPriority(protoName string, priority priorityq.Priority) {
	prot.priorities[protoName] = priority
}
