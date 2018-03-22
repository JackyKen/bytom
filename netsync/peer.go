package netsync

import (
	"sync"
	"time"

	"github.com/bytom/common"
	"github.com/bytom/p2p"
	"gopkg.in/fatih/set.v0"
)

const (
	// BlockchainChannel is a channel for blocks and status updates
	// BlockchainChannel = byte(0x40)
	handshakeTimeout = 5 * time.Second
)

type peer struct {
	id string

	peer *p2p.Peer
	version  int         // Protocol version negotiated
	//forkDrop *time.Timer // Timed connection dropper if forks aren't validated in time

	head common.Hash
	height   uint64
	lock sync.RWMutex
	knownTxs    *set.Set // Set of transaction hashes known to be known by this peer
	knownBlocks *set.Set // Set of block hashes known to be known by this peer
}
//
//// SendTransactions sends transactions to the peer and includes the hashes
//// in its transaction hash set for future reference.
//func (p *peer) SendTransaction(tx *legacy.Tx) error {
//	p.knownTxs.Add(tx.ID.Byte32())
//	msg, err := NewTransactionNotifyMessage(tx)
//	if err != nil {
//		return err
//	}
//	// bcr.Switch.Broadcast(BlockchainChannel, struct{ BlockchainMessage }{msg})
//
//	if result := p.Send(BlockchainChannel, struct{ BlockchainMessage }{msg}); result == false {
//		return errors.New("peer send error")
//	}
//
//	return nil
//}
//
//// MarkTransaction marks a transaction as known for the peer, ensuring that it
//// will never be propagated to this particular peer.
//func (p *peer) MarkTransaction(hash common.Hash) {
//	// If we reached the memory allowance, drop a previously known transaction hash
//	for p.knownTxs.Size() >= maxKnownTxs {
//		p.knownTxs.Pop()
//	}
//	p.knownTxs.Add(hash)
//}

// peerSet represents the collection of active peers currently participating in
// the Ethereum sub-protocol.
type peerSet struct {
	peers map[string]*peer
	lock  sync.RWMutex
	//closed bool
}

// newPeerSet creates a new peer set to track the active participants.
func newPeerSet() *peerSet {
	return &peerSet{
		peers: make(map[string]*peer),
	}
}
