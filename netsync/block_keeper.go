package netsync

import (
	"errors"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/bytom/p2p"
	"github.com/bytom/protocol"
	"github.com/bytom/protocol/bc"
	"github.com/bytom/protocol/bc/legacy"
	"gopkg.in/fatih/set.v0"
)

const (
	maxKnownTxs    = 32768 // Maximum transactions hashes to keep in the known list (prevent DOS)
	maxKnownBlocks = 1024  // Maximum block hashes to keep in the known list (prevent DOS)
)

type BlockRequestMessage struct {
	Height  uint64
	RawHash [32]byte
}

type blockKeeperPeer struct {
	mtx     sync.RWMutex
	height  uint64
	hash    *bc.Hash
	peer    *p2p.Peer
	version int // Protocol version negotiated

	knownTxs    *set.Set // Set of transaction hashes known to be known by this peer
	knownBlocks *set.Set // Set of block hashes known to be known by this peer

}

func newBlockKeeperPeer(height uint64, hash *bc.Hash) *blockKeeperPeer {
	return &blockKeeperPeer{
		height:      height,
		hash:        hash,
		knownTxs:    set.New(),
		knownBlocks: set.New(),
	}

}

func (p *blockKeeperPeer) GetStatus() (height uint64, hash *bc.Hash) {
	p.mtx.RLock()
	defer p.mtx.RUnlock()
	return p.height, p.hash
}

func (p *blockKeeperPeer) SetStatus(height uint64, hash *bc.Hash) {
	p.mtx.Lock()
	defer p.mtx.Unlock()

	p.height = height
	p.hash = hash
}

type pendingResponse struct {
	block *legacy.Block
	src   *p2p.Peer
}

//TODO: add retry mechanism
type blockKeeper struct {
	mtx           sync.RWMutex
	chainHeight   uint64
	maxPeerHeight uint64
	chainUpdateCh <-chan struct{}
	peerUpdateCh  chan struct{}
	done          chan bool

	chain            *protocol.Chain
	sw               *p2p.Switch
	peers            map[string]*blockKeeperPeer
	pendingProcessCh chan *pendingResponse
}

func newBlockKeeper(chain *protocol.Chain, sw *p2p.Switch) *blockKeeper {
	chainHeight := chain.Height()
	bk := &blockKeeper{
		chainHeight:   chainHeight,
		maxPeerHeight: uint64(0),
		chainUpdateCh: chain.BlockWaiter(chainHeight + 1),
		peerUpdateCh:  make(chan struct{}, 1000),
		done:          make(chan bool, 1),

		chain:            chain,
		sw:               sw,
		peers:            make(map[string]*blockKeeperPeer),
		pendingProcessCh: make(chan *pendingResponse),
	}
	go bk.blockProcessWorker()
	//go bk.blockRequestWorker()
	return bk
}

func (bk *blockKeeper) Stop() {
	bk.done <- true
}

func (bk *blockKeeper) AddBlock(block *legacy.Block, src *p2p.Peer) {
	bk.pendingProcessCh <- &pendingResponse{block: block, src: src}
}

func (bk *blockKeeper) IsCaughtUp() bool {
	bk.mtx.RLock()
	defer bk.mtx.RUnlock()
	return bk.chainHeight >= bk.maxPeerHeight
}

func (bk *blockKeeper) RemovePeer(peerID string) {
	bk.mtx.Lock()
	delete(bk.peers, peerID)
	bk.mtx.Unlock()
	log.WithField("ID", peerID).Info("Delete peer from blockKeeper")
}

func (bk *blockKeeper) requestBlockByHash(peerID string, hash *bc.Hash) error {
	peer := bk.sw.Peers().Get(peerID)
	if peer == nil {
		return errors.New("can't find peer in peer pool")
	}
	msg := &BlockRequestMessage{RawHash: hash.Byte32()}
	peer.TrySend(BlockchainChannel, struct{ BlockchainMessage }{msg})
	return nil
}

func (bk *blockKeeper) requestBlockByHeight(peerID string, height uint64) error {
	peer := bk.sw.Peers().Get(peerID)
	if peer == nil {
		return errors.New("can't find peer in peer pool")
	}
	msg := &BlockRequestMessage{Height: height}
	peer.TrySend(BlockchainChannel, struct{ BlockchainMessage }{msg})
	return nil
}

func (bk *blockKeeper) SetPeerHeight(peerID string, height uint64, hash *bc.Hash) {
	bk.mtx.Lock()
	defer bk.mtx.Unlock()

	if height > bk.maxPeerHeight {
		bk.maxPeerHeight = height
		bk.peerUpdateCh <- struct{}{}
	}

	if peer, ok := bk.peers[peerID]; ok {
		peer.SetStatus(height, hash)
		return
	}
	if peer := bk.peers[peerID]; peer == nil {
		peer := newBlockKeeperPeer(height, hash)
		bk.peers[peerID] = peer
		bk.MarkBlock(peerID, hash.Byte32())
		log.WithFields(log.Fields{"ID": peerID, "Height": height}).Info("Add new peer to blockKeeper")
		return
	}
	bk.peers[peerID].height = height
	bk.peers[peerID].hash = hash
	bk.MarkBlock(peerID, hash.Byte32())
}

func (bk *blockKeeper) RequestBlockByHeight(height uint64) {
	bk.mtx.RLock()
	defer bk.mtx.RUnlock()

	for peerID, peer := range bk.peers {
		if peerHeight, _ := peer.GetStatus(); peerHeight > bk.chainHeight {
			bk.requestBlockByHeight(peerID, height)
		}
	}
}

func (bk *blockKeeper) blockRequestWorker() {
	for {
		select {
		case <-bk.chainUpdateCh:
			chainHeight := bk.chain.Height()
			bk.mtx.Lock()
			if bk.chainHeight < chainHeight {
				bk.chainHeight = chainHeight
			}
			bk.chainUpdateCh = bk.chain.BlockWaiter(bk.chainHeight + 1)
			bk.mtx.Unlock()

		case <-bk.peerUpdateCh:
			bk.mtx.RLock()
			chainHeight := bk.chainHeight
			maxPeerHeight := bk.maxPeerHeight
			bk.mtx.RUnlock()

			for i := chainHeight + 1; i <= maxPeerHeight; i++ {
				bk.RequestBlockByHeight(i)
				waiter := bk.chain.BlockWaiter(i)
				retryTicker := time.Tick(15 * time.Second)

			retryLoop:
				for {
					select {
					case <-waiter:
						break retryLoop
					case <-retryTicker:
						bk.RequestBlockByHeight(i)
					}
				}
			}

		case <-bk.done:
			return
		}
	}
}

func (bk *blockKeeper) blockProcessWorker() {
	for pendingResponse := range bk.pendingProcessCh {

		block := pendingResponse.block
		blockHash := block.Hash()
		isOrphan, err := bk.chain.ProcessBlock(block)
		if err != nil {
			bk.sw.AddScamPeer(pendingResponse.src)
			log.WithField("hash", blockHash.String()).Errorf("blockKeeper fail process block %v", err)
			continue
		}
		log.WithFields(log.Fields{
			"height":   block.Height,
			"hash":     blockHash.String(),
			"isOrphan": isOrphan,
		}).Info("blockKeeper processed block")

		if isOrphan {
			bk.requestBlockByHash(pendingResponse.src.Key, &block.PreviousBlockHash)
		}
	}
}

// BestPeer retrieves the known peer with the currently highest total difficulty.
func (bk *blockKeeper) BestPeer() *p2p.Peer {
	bk.mtx.RLock()
	defer bk.mtx.RUnlock()

	var (
		bestPeer   *p2p.Peer
		bestHeight uint64
	)

	for _, p := range bk.peers {
		if bestPeer == nil || p.height > bestHeight {
			bestPeer, bestHeight = p.peer, p.height
		}
	}
	return bestPeer
}

// MarkTransaction marks a transaction as known for the peer, ensuring that it
// will never be propagated to this particular peer.
func (bk *blockKeeper) MarkTransaction(peer string, hash [32]byte) {
	// If we reached the memory allowance, drop a previously known transaction hash
	for bk.peers[peer].knownTxs.Size() >= maxKnownTxs {
		bk.peers[peer].knownTxs.Pop()
	}
	bk.peers[peer].knownTxs.Add(hash)
}

// MarkBlock marks a block as known for the peer, ensuring that the block will
// never be propagated to this particular peer.
func (bk *blockKeeper) MarkBlock(peer string, hash [32]byte) {
	// If we reached the memory allowance, drop a previously known block hash
	for bk.peers[peer].knownBlocks.Size() >= maxKnownBlocks {
		bk.peers[peer].knownBlocks.Pop()
	}
	bk.peers[peer].knownBlocks.Add(hash)
}

// PeersWithoutTx retrieves a list of peers that do not have a given transaction
// in their set of known hashes.
func (bk *blockKeeper) PeersWithoutTx(hash [32]byte) []*p2p.Peer {
	list := make([]*p2p.Peer, 0, len(bk.peers))
	for _, p := range bk.peers {
		if !p.knownTxs.Has(hash) {
			list = append(list, p.peer)
		}
	}
	return list
}

// PeersWithoutBlock retrieves a list of peers that do not have a given block in
// their set of known hashes.
func (bk *blockKeeper) PeersWithoutBlock(hash [32]byte) []*p2p.Peer {
	list := make([]*p2p.Peer, 0, len(bk.peers))
	for _, p := range bk.peers {
		if !p.knownBlocks.Has(hash) {
			list = append(list, p.peer)
		}
	}
	return list
}
