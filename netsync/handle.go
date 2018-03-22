package netsync

import (
	"strings"
	"sync"
	log "github.com/sirupsen/logrus"
	dbm "github.com/tendermint/tmlibs/db"

	"github.com/bytom/blockchain/account"
	cfg "github.com/bytom/config"
	"github.com/bytom/netsync/fetcher"
	"github.com/bytom/p2p"
	"github.com/bytom/protocol"
	core "github.com/bytom/protocol"
	"github.com/bytom/protocol/bc"
	"github.com/bytom/protocol/bc/legacy"
	"github.com/bytom/version"
	"github.com/tendermint/go-crypto"
	"github.com/tendermint/go-wire"
	cmn "github.com/tendermint/tmlibs/common"
)

const (
	//forceSyncCycle      = 10 * time.Second // Time interval to force syncs, even if few peers are available
	//minDesiredPeerCount = 5                // Amount of peers desired to start syncing
)

type SyncManager struct {
	networkId uint64
	sw        *p2p.Switch
	addrBook  *p2p.AddrBook // known peers

	// fastSync  uint32 // Flag whether fast sync is enabled (gets disabled if we already have blocks)
	// acceptTxs uint32 // Flag whether we're considered synchronised (enables transaction processing)
	// txpool  *protocol.TxPool
	privKey crypto.PrivKeyEd25519 // local node's p2p key
	// txpool      txPool
	// blockchain  *core.BlockChain
	// chainconfig *params.ChainConfig
	maxPeers int
	chain    *core.Chain
	txPool   *core.TxPool
	//downloader *downloader.Downloader
	fetcher *fetcher.Fetcher
	peers *peerSet
	blockKeeper  *blockKeeper

	// SubProtocols []p2p.Protocol

	// eventMux      *event.TypeMux
	// txCh          chan core.TxPreEvent
	// txSub         event.Subscription
	// minedBlockSub *event.TypeMuxSubscription
	newBlockCh chan *bc.Hash

	// channels for fetcher, syncer, txsyncLoop
	newPeerCh chan *peer
	// txsyncCh    chan *txsync
	quitSync    chan struct{}
	noMorePeers chan struct{}
	config      *cfg.Config
	// wait group is used for graceful shutdowns during downloading
	// and processing
	wg sync.WaitGroup
}

// NewProtocolManager returns a new ethereum sub protocol manager. The Ethereum sub protocol manages peers capable
// with the ethereum network.
func NewSyncManager(config *cfg.Config, chain *protocol.Chain, txPool *protocol.TxPool, accounts *account.Manager, miningEnable bool) (*SyncManager, error) {
	// privKey := crypto.GenPrivKeyEd25519()
	// Create the protocol manager with the base fields
	manager := &SyncManager{
		// networkId: networkId,
		// eventMux:    mux,
		txPool: txPool,
		chain:  chain,
		// chainconfig: config,
		privKey: crypto.GenPrivKeyEd25519(),
		config:  config,
		//sw:         sw,
		//addrBook:   addrBook,
		//newBlockCh: newBlockCh,
		//fetcher:    fetcher,
		peers:       newPeerSet(),
		newPeerCh:   make(chan *peer),
		noMorePeers: make(chan struct{}),
		// txsyncCh:    make(chan *txsync),
		quitSync: make(chan struct{}),
	}

	//validator := func(header *types.Header) error {
	//	return engine.VerifyHeader(blockchain, header, true)
	//}
	heighter := func() uint64 {
		return chain.Height()
	}
	inserter := func(block *legacy.Block) (bool, error) {
		//// If fast sync is running, deny importing weird blocks
		//if atomic.LoadUint32(&manager.fastSync) == 1 {
		//	log.Warn("Discarded bad propagated block", "number", blocks[0].Number(), "hash", blocks[0].Hash())
		//	return 0, nil
		//}
		//atomic.StoreUint32(&manager.acceptTxs, 1) // Mark initial sync done on any fetcher import
		return manager.chain.ProcessBlock(block)
	}

	manager.fetcher = fetcher.New(chain.GetBlockByHash, manager.BroadcastMineBlock, heighter, inserter, manager.removePeer)

	manager.newBlockCh = make(chan *bc.Hash, maxNewBlockChSize)

	trustHistoryDB := dbm.NewDB("trusthistory", config.DBBackend, config.DBDir())

	manager.sw = p2p.NewSwitch(config.P2P, trustHistoryDB)

	protocolReactor := NewProtocalReactor(chain, txPool, accounts, manager.sw, config.Mining, manager.newBlockCh, manager.fetcher)
	manager.blockKeeper=protocolReactor.blockKeeper
	manager.sw.AddReactor("PROTOCOL", protocolReactor)
	// Optionally, start the pex reactor
	//var addrBook *p2p.AddrBook
	if config.P2P.PexReactor {
		manager.addrBook = p2p.NewAddrBook(config.P2P.AddrBookFile(), config.P2P.AddrBookStrict)
		pexReactor := p2p.NewPEXReactor(manager.addrBook)
		manager.sw.AddReactor("PEX", pexReactor)
	}

	return manager, nil
}

// Defaults to tcp
func protocolAndAddress(listenAddr string) (string, string) {
	p, address := "tcp", listenAddr
	parts := strings.SplitN(address, "://", 2)
	if len(parts) == 2 {
		p, address = parts[0], parts[1]
	}
	return p, address
}

func (self *SyncManager) makeNodeInfo() *p2p.NodeInfo {
	nodeInfo := &p2p.NodeInfo{
		PubKey:  self.privKey.PubKey().Unwrap().(crypto.PubKeyEd25519),
		Moniker: self.config.Moniker,
		Network: "bytom",
		Version: version.Version,
		Other: []string{
			cmn.Fmt("wire_version=%v", wire.Version),
			cmn.Fmt("p2p_version=%v", p2p.Version),
		},
	}

	if !self.sw.IsListening() {
		return nodeInfo
	}

	p2pListener := self.sw.Listeners()[0]
	p2pHost := p2pListener.ExternalAddress().IP.String()
	p2pPort := p2pListener.ExternalAddress().Port
	//rpcListenAddr := n.config.RPC.ListenAddress

	// We assume that the rpcListener has the same ExternalAddress.
	// This is probably true because both P2P and RPC listeners use UPnP,
	// except of course if the rpc is only bound to localhost
	nodeInfo.ListenAddr = cmn.Fmt("%v:%v", p2pHost, p2pPort)
	//nodeInfo.Other = append(nodeInfo.Other, cmn.Fmt("rpc_addr=%v", rpcListenAddr))
	return nodeInfo
}

func (self *SyncManager) netStart() error {
	// Create & add listener
	p, address := protocolAndAddress(self.config.P2P.ListenAddress)

	l := p2p.NewDefaultListener(p, address, self.config.P2P.SkipUPNP, nil)

	self.sw.AddListener(l)

	// Start the switch
	self.sw.SetNodeInfo(self.makeNodeInfo())
	self.sw.SetNodePrivKey(self.privKey)
	_, err := self.sw.Start()
	if err != nil {
		return err
	}

	// If seeds exist, add them to the address book and dial out
	if self.config.P2P.Seeds != "" {
		// dial out
		seeds := strings.Split(self.config.P2P.Seeds, ",")
		if err := self.DialSeeds(seeds); err != nil {
			return err
		}
	}

	return nil
}

func (self *SyncManager) Start(maxPeers int) {
	self.maxPeers = maxPeers
	self.netStart()
	// broadcast transactions
	// self.txCh = make(chan core.TxPreEvent, txChanSize)
	// self.txSub = self.txpool.SubscribeTxPreEvent(self.txCh)
	go self.txBroadcastLoop()

	// // broadcast mined blocks
	// self.minedBlockSub = self.eventMux.Subscribe(core.NewMinedBlockEvent{})
	go self.minedBroadcastLoop()

	// // start sync handlers
	go self.syncer()
	// go self.txsyncLoop()
}

func (self *SyncManager) txBroadcastLoop() {
	newTxCh := self.txPool.GetNewTxCh()

	for {
		select {
		case newTx := <-newTxCh:
			self.BroadcastTx(newTx)

		case <-self.quitSync:
			return
		}
	}
}

func (self *SyncManager) minedBroadcastLoop() {
	for {
		select {
		case blockHash := <-self.newBlockCh:
			//fmt.Println(blockHash)
			block, err := self.chain.GetBlockByHash(blockHash)
			if err != nil {
				log.Errorf("Error get block from newBlockCh %v", err)
			}
			//log.WithFields(log.Fields{"Hash": blockHash, "height": block.Height}).Info("Boardcast my new block")
			self.BroadcastMineBlock(block)
		case <-self.quitSync:
			return
		}
	}
}

// BroadcastTransaction broadcats `BlockStore` transaction.
func (self *SyncManager) BroadcastTx(tx *legacy.Tx) error {
	msg, err := NewTransactionNotifyMessage(tx)
	if err != nil {
		return err
	}
	peers := self.blockKeeper.PeersWithoutTx(tx.ID.Byte32())
	self.sw.BroadcastX(BlockchainChannel, peers, struct{ BlockchainMessage }{msg})
	return nil
}

// BroadcastBlock will either propagate a block to a subset of it's peers, or
// will only announce it's availability (depending what's requested).
func (self *SyncManager) BroadcastMineBlock(block *legacy.Block) {
	hash := block.Hash().Byte32()
	peers := self.blockKeeper.PeersWithoutBlock(hash)

	msg, _ := NewMineBlockMessage(block)
	//if err != nil {
	//	return err
	//}
	// If propagation is requested, send to a subset of the peer
	//if propagate {
	// Calculate the TD of the block (it's not imported yet, so block.Td is not valid)
	//var td *big.Int
	//if parent := pm.blockchain.GetBlock(block.ParentHash(), block.NumberU64()-1); parent != nil {
	//	td = new(big.Int).Add(block.Difficulty(), pm.blockchain.GetTd(block.ParentHash(), block.NumberU64()-1))
	//} else {
	//	log.Error("Propagating dangling block", "number", block.Number(), "hash", hash)
	//	return
	//}
	// Send the block to a subset of our peers
	//transfer := peers[:int(math.Sqrt(float64(len(peers))))]
	self.sw.BroadcastX(BlockchainChannel, peers, struct{ BlockchainMessage }{msg})

	//for _, peer := range transfer {
	//		peer.SendNewBlock(block, td)
	//	}

	//log.WithFields(log.Fields{"Hash": block.Hash().String(), "height": block.Height}).Info("Boardcast my new block")

	//log.Trace("Propagated block", "hash", hash, "recipients", len(transfer), "duration", common.PrettyDuration(time.Since(block.ReceivedAt)))
	//return nil
	//}
	//// Otherwise if the block is indeed in out own chain, announce it
	//if pm.blockchain.HasBlock(hash, block.NumberU64()) {
	//	for _, peer := range peers {
	//		peer.SendNewBlockHashes([]common.Hash{hash}, []uint64{block.NumberU64()})
	//	}
	//	log.Trace("Announced block", "hash", hash, "recipients", len(peers), "duration", common.PrettyDuration(time.Since(block.ReceivedAt)))
	//}
}

func (self *SyncManager) NodeInfo() *p2p.NodeInfo {
	return self.sw.NodeInfo()
}

func (self *SyncManager) DialSeeds(seeds []string) error {
	return self.sw.DialSeeds(self.addrBook, seeds)
}

func (self *SyncManager) Stop(maxPeers int) {

	self.sw.Stop()
}

// Add a Listener to accept inbound peer connections.
// Add listeners before starting the Node.
// The first listener is the primary listener (in NodeInfo)
func (self *SyncManager) AddListener(l p2p.Listener) {
	self.sw.AddListener(l)
}

func (self *SyncManager) Switch() *p2p.Switch {
	return self.sw
}

func (self *SyncManager) removePeer(id string) {
	// Short circuit if the peer was already removed
	peers := self.sw.Peers()
	if peers == nil {
		return
	}

	peer := peers.Get(id)
	if peer == nil {
		return
	}

	peers.Remove(peer)
	log.Debug("Removing bytom peer", "peer", id)

	// Unregister the peer from the downloader and Ethereum peer set
	//pm.downloader.UnregisterPeer(id)
	//if err := pm.peers.Unregister(id); err != nil {
	//	log.Error("Peer removal failed", "peer", id, "err", err)
	//}
	// Hard disconnect at the networking layer
	//TODO
	if peer != nil {
		//peer.Peer.Disconnect(p2p.DiscUselessPeer)
		peer.CloseConn()
	}
}
