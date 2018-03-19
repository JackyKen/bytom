package node

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	_ "net/http/pprof"
	"sync"
	"time"

	"github.com/kr/secureheader"
	log "github.com/sirupsen/logrus"
	cmn "github.com/tendermint/tmlibs/common"
	dbm "github.com/tendermint/tmlibs/db"

	bc "github.com/bytom/blockchain"
	"github.com/bytom/blockchain/accesstoken"
	"github.com/bytom/blockchain/account"
	"github.com/bytom/blockchain/asset"
	"github.com/bytom/blockchain/pseudohsm"
	"github.com/bytom/blockchain/txdb"
	"github.com/bytom/blockchain/txfeed"
	w "github.com/bytom/blockchain/wallet"
	cfg "github.com/bytom/config"
	"github.com/bytom/env"
	"github.com/bytom/errors"
	"github.com/bytom/netsync"
	"github.com/bytom/protocol"
	"github.com/bytom/types"
	"github.com/bytom/util/browser"
)

const (
	httpReadTimeout          = 2 * time.Minute
	httpWriteTimeout         = time.Hour
	webAddress               = "http://127.0.0.1:9888"
	expireReservationsPeriod = time.Second
)

type Node struct {
	cmn.BaseService

	// config
	config *cfg.Config

	// network
	// privKey crypto.PrivKeyEd25519 // local node's p2p key
	// sw       *p2p.Switch           // p2p connections
	// addrBook *p2p.AddrBook         // known peers
	syncManager *netsync.SyncManager

	evsw       types.EventSwitch // pub/sub for services
	blockStore *txdb.Store
	bcReactor  *bc.BlockchainReactor
	accounts   *account.Manager
	assets     *asset.Registry
}

func NewNodeDefault(config *cfg.Config) *Node {
	return NewNode(config)
}

func RedirectHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/" {
			http.Redirect(w, req, "/dashboard/", http.StatusFound)
			return
		}
		next.ServeHTTP(w, req)
	})
}

type waitHandler struct {
	h  http.Handler
	wg sync.WaitGroup
}

func (wh *waitHandler) Set(h http.Handler) {
	wh.h = h
	wh.wg.Done()
}

func (wh *waitHandler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	wh.wg.Wait()
	wh.h.ServeHTTP(w, req)
}

func rpcInit(h *bc.BlockchainReactor, config *cfg.Config, accessTokens *accesstoken.CredentialStore) {
	// The waitHandler accepts incoming requests, but blocks until its underlying
	// handler is set, when the second phase is complete.
	var coreHandler waitHandler
	coreHandler.wg.Add(1)
	mux := http.NewServeMux()
	mux.Handle("/", &coreHandler)

	var handler http.Handler = mux

	if config.Auth.Disable == false {
		handler = bc.AuthHandler(handler, accessTokens)
	}
	handler = RedirectHandler(handler)

	secureheader.DefaultConfig.PermitClearLoopback = true
	secureheader.DefaultConfig.HTTPSRedirect = false
	secureheader.DefaultConfig.Next = handler

	server := &http.Server{
		// Note: we should not set TLSConfig here;
		// we took care of TLS with the listener in maybeUseTLS.
		Handler:      secureheader.DefaultConfig,
		ReadTimeout:  httpReadTimeout,
		WriteTimeout: httpWriteTimeout,
		// Disable HTTP/2 for now until the Go implementation is more stable.
		// https://github.com/golang/go/issues/16450
		// https://github.com/golang/go/issues/17071
		TLSNextProto: map[string]func(*http.Server, *tls.Conn, http.Handler){},
	}
	listenAddr := env.String("LISTEN", config.ApiAddress)
	log.WithField("api address:", config.ApiAddress).Info("Rpc listen")
	listener, err := net.Listen("tcp", *listenAddr)
	if err != nil {
		cmn.Exit(cmn.Fmt("Failed to register tcp port: %v", err))
	}

	// The `Serve` call has to happen in its own goroutine because
	// it's blocking and we need to proceed to the rest of the core setup after
	// we call it.
	go func() {
		if err := server.Serve(listener); err != nil {
			log.WithField("error", errors.Wrap(err, "Serve")).Error("Rpc server")
		}
	}()
	coreHandler.Set(h)
}

func NewNode(config *cfg.Config) *Node {
	ctx := context.Background()

	// Get store
	txDB := dbm.NewDB("txdb", config.DBBackend, config.DBDir())
	store := txdb.NewStore(txDB)

	tokenDB := dbm.NewDB("accesstoken", config.DBBackend, config.DBDir())
	accessTokens := accesstoken.NewStore(tokenDB)

	// privKey := crypto.GenPrivKeyEd25519()

	// Make event switch
	eventSwitch := types.NewEventSwitch()
	_, err := eventSwitch.Start()
	if err != nil {
		cmn.Exit(cmn.Fmt("Failed to start switch: %v", err))
	}

	genesisBlock := cfg.GenerateGenesisBlock()

	txPool := protocol.NewTxPool()
	chain, err := protocol.NewChain(genesisBlock.Hash(), store, txPool)
	if err != nil {
		cmn.Exit(cmn.Fmt("Failed to create chain structure: %v", err))
	}

	if chain.BestBlockHash() == nil {
		if err := chain.SaveBlock(genesisBlock); err != nil {
			cmn.Exit(cmn.Fmt("Failed to save genesisBlock to store: %v", err))
		}
		if err := chain.ConnectBlock(genesisBlock); err != nil {
			cmn.Exit(cmn.Fmt("Failed to connect genesisBlock to chain: %v", err))
		}
	}

	var accounts *account.Manager = nil
	var assets *asset.Registry = nil
	var wallet *w.Wallet = nil
	var txFeed *txfeed.Tracker = nil

	txFeedDB := dbm.NewDB("txfeeds", config.DBBackend, config.DBDir())
	txFeed = txfeed.NewTracker(txFeedDB, chain)

	if err = txFeed.Prepare(ctx); err != nil {
		log.WithField("error", err).Error("start txfeed")
		return nil
	}

	hsm, err := pseudohsm.New(config.KeysDir())
	if err != nil {
		cmn.Exit(cmn.Fmt("initialize HSM failed: %v", err))
	}

	if !config.Wallet.Disable {
		xpubs, _ := hsm.ListKeys()
		walletDB := dbm.NewDB("wallet", config.DBBackend, config.DBDir())
		accounts = account.NewManager(walletDB, chain)
		assets = asset.NewRegistry(walletDB, chain)
		wallet, err = w.NewWallet(walletDB, accounts, assets, chain, xpubs)
		if err != nil {
			log.WithField("error", err).Error("init NewWallet")
		}
		// Clean up expired UTXO reservations periodically.
		go accounts.ExpireReservations(ctx, expireReservationsPeriod)
	}
	syncManager, _ := netsync.NewSyncManager(config, chain, txPool, accounts, true)

	bcReactor := bc.NewBlockchainReactor(chain, accounts, assets, hsm, wallet, txFeed, accessTokens)

	// sw.AddReactor("BLOCKCHAIN", bcReactor)

	rpcInit(bcReactor, config, accessTokens)
	bcReactor.OnStart()

	// run the profile server
	profileHost := config.ProfListenAddress
	if profileHost != "" {
		// Profiling bytomd programs.see (https://blog.golang.org/profiling-go-programs)
		// go tool pprof http://profileHose/debug/pprof/heap
		go func() {
			http.ListenAndServe(profileHost, nil)
		}()
	}

	node := &Node{
		config: config,

		// privKey: privKey,
		// sw:       sw,
		// addrBook: addrBook,
		syncManager: syncManager,
		evsw:        eventSwitch,
		bcReactor:   bcReactor,
		blockStore:  store,
		accounts:    accounts,
		assets:      assets,
	}
	node.BaseService = *cmn.NewBaseService(nil, "Node", node)
	return node
}

// Lanch web broser or not
func lanchWebBroser(lanch bool) {
	if lanch {
		log.Info("Launching System Browser with :", webAddress)
		if err := browser.Open(webAddress); err != nil {
			log.Error(err.Error())
			return
		}
	}
}

func (n *Node) OnStart() error {
	n.syncManager.Start(20)
	lanchWebBroser(!n.config.Web.Closed)
	return nil
}

func (n *Node) OnStop() {
	n.BaseService.OnStop()

	log.Info("Stopping Node")
	// TODO: gracefully disconnect from peers.

}

func (n *Node) RunForever() {
	// Sleep forever and then...
	cmn.TrapSignal(func() {
		n.Stop()
	})
}

func (n *Node) EventSwitch() types.EventSwitch {
	return n.evsw
}

func (n *Node) SyncManager() *netsync.SyncManager {
	return n.syncManager
}

// func (n *Node) makeNodeInfo() *p2p.NodeInfo {
// 	nodeInfo := &p2p.NodeInfo{
// 		PubKey:  n.privKey.PubKey().Unwrap().(crypto.PubKeyEd25519),
// 		Moniker: n.config.Moniker,
// 		Network: "bytom",
// 		Version: version.Version,
// 		Other: []string{
// 			cmn.Fmt("wire_version=%v", wire.Version),
// 			cmn.Fmt("p2p_version=%v", p2p.Version),
// 		},
// 	}

// 	if !n.sw.IsListening() {
// 		return nodeInfo
// 	}

// 	p2pListener := n.sw.Listeners()[0]
// 	p2pHost := p2pListener.ExternalAddress().IP.String()
// 	p2pPort := p2pListener.ExternalAddress().Port
// 	//rpcListenAddr := n.config.RPC.ListenAddress

// 	// We assume that the rpcListener has the same ExternalAddress.
// 	// This is probably true because both P2P and RPC listeners use UPnP,
// 	// except of course if the rpc is only bound to localhost
// 	nodeInfo.ListenAddr = cmn.Fmt("%v:%v", p2pHost, p2pPort)
// 	//nodeInfo.Other = append(nodeInfo.Other, cmn.Fmt("rpc_addr=%v", rpcListenAddr))
// 	return nodeInfo
// }

//------------------------------------------------------------------------------

//------------------------------------------------------------------------------
