package netsync

import (
	"strings"
	"sync"

	dbm "github.com/tendermint/tmlibs/db"

	"github.com/bytom/blockchain/account"
	cfg "github.com/bytom/config"
	"github.com/bytom/p2p"
	"github.com/bytom/protocol"
	core "github.com/bytom/protocol"
	"github.com/bytom/protocol/bc/legacy"
	"github.com/bytom/version"
	crypto "github.com/tendermint/go-crypto"
	wire "github.com/tendermint/go-wire"
	cmn "github.com/tendermint/tmlibs/common"
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
	// downloader *downloader.Downloader
	// fetcher    *fetcher.Fetcher
	peers *peerSet

	// SubProtocols []p2p.Protocol

	// eventMux      *event.TypeMux
	// txCh          chan core.TxPreEvent
	// txSub         event.Subscription
	// minedBlockSub *event.TypeMuxSubscription

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
	trustHistoryDB := dbm.NewDB("trusthistory", config.DBBackend, config.DBDir())

	sw := p2p.NewSwitch(config.P2P, trustHistoryDB)

	protocolReactor := NewProtocalReactor(chain, txPool, accounts, sw, config.Mining)

	sw.AddReactor("PROTOCOL", protocolReactor)
	// Optionally, start the pex reactor
	var addrBook *p2p.AddrBook
	if config.P2P.PexReactor {
		addrBook = p2p.NewAddrBook(config.P2P.AddrBookFile(), config.P2P.AddrBookStrict)
		pexReactor := p2p.NewPEXReactor(addrBook)
		sw.AddReactor("PEX", pexReactor)
	}
	// privKey := crypto.GenPrivKeyEd25519()
	// Create the protocol manager with the base fields
	manager := &SyncManager{
		// networkId: networkId,
		// eventMux:    mux,
		txPool: txPool,
		// blockchain:  blockchain,
		// chainconfig: config,
		privKey:  crypto.GenPrivKeyEd25519(),
		config:   config,
		sw:       sw,
		addrBook: addrBook,

		peers:       newPeerSet(),
		newPeerCh:   make(chan *peer),
		noMorePeers: make(chan struct{}),
		// txsyncCh:    make(chan *txsync),
		quitSync: make(chan struct{}),
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
	// go self.minedBroadcastLoop()

	// // start sync handlers
	// go self.syncer()
	// go self.txsyncLoop()
}

func (self *SyncManager) txBroadcastLoop() {
	newTxCh := self.txPool.GetNewTxCh()

	for {
		select {
		case newTx := <-newTxCh:
			self.BroadcastTx(newTx)

		// // Err() channel will be closed when unsubscribing.
		// case <-self.txSub.Err():
		// 	return
		case <-self.quitSync:
			return
		}
	}
}

// BroadcastTx will propagate a transaction to all peers which are not known to
// already have the given transaction.
func (self *SyncManager) BroadcastTx(tx *legacy.Tx) {
	// Broadcast transaction to a batch of peers not knowing about it
	peers := self.peers.PeersWithoutTx(tx.ID.Byte32())
	//FIXME include this again: peers = peers[:int(math.Sqrt(float64(len(peers))))]
	for _, peer := range peers {
		peer.SendTransaction(tx)
	}
	// log.Trace("Broadcast transaction", "hash", hash, "recipients", len(peers))
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
