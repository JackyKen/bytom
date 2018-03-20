package netsync

import (
	"net/http"
	"reflect"
	"time"

	log "github.com/sirupsen/logrus"
	cmn "github.com/tendermint/tmlibs/common"

	"github.com/bytom/blockchain/accesstoken"
	"github.com/bytom/blockchain/account"
	"github.com/bytom/blockchain/asset"
	"github.com/bytom/blockchain/pseudohsm"
	"github.com/bytom/blockchain/txfeed"
	"github.com/bytom/blockchain/wallet"
	"github.com/bytom/encoding/json"
	"github.com/bytom/mining/cpuminer"
	"github.com/bytom/mining/miningpool"
	"github.com/bytom/p2p"
	"github.com/bytom/p2p/trust"
	"github.com/bytom/protocol"
	"github.com/bytom/protocol/bc/legacy"
	"github.com/bytom/types"
)

const (
	// BlockchainChannel is a channel for blocks and status updates
	BlockchainChannel = byte(0x40)

	defaultChannelCapacity      = 100
	trySyncIntervalMS           = 100
	statusUpdateIntervalSeconds = 10
	maxBlockchainResponseSize   = 22020096 + 2
	crosscoreRPCPrefix          = "/rpc/"
)

const (
	// SUCCESS indicates the rpc calling is successful.
	SUCCESS = "success"
	// FAIL indicated the rpc calling is failed.
	FAIL = "fail"
)

// Response describes the response standard.
type Response struct {
	Status string      `json:"status,omitempty"`
	Msg    string      `json:"msg,omitempty"`
	Data   interface{} `json:"data,omitempty"`
}

//NewSuccessResponse success response
func NewSuccessResponse(data interface{}) Response {
	return Response{Status: SUCCESS, Data: data}
}

//NewErrorResponse error response
func NewErrorResponse(err error) Response {
	return Response{Status: FAIL, Msg: err.Error()}
}

//ProtocalReactor handles long-term catchup syncing.
type ProtocalReactor struct {
	p2p.BaseReactor

	chain         *protocol.Chain
	wallet        *wallet.Wallet
	accounts      *account.Manager
	assets        *asset.Registry
	accessTokens  *accesstoken.CredentialStore
	txFeedTracker *txfeed.Tracker
	blockKeeper   *blockKeeper
	txPool        *protocol.TxPool
	hsm           *pseudohsm.HSM
	mining        *cpuminer.CPUMiner
	miningPool    *miningpool.MiningPool
	mux           *http.ServeMux
	sw            *p2p.Switch
	handler       http.Handler
	evsw          types.EventSwitch
	miningEnable  bool
}

func maxBytes(h http.Handler) http.Handler {
	const maxReqSize = 1e7 // 10MB
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		// A block can easily be bigger than maxReqSize, but everything
		// else should be pretty small.
		if req.URL.Path != crosscoreRPCPrefix+"signer/sign-block" {
			req.Body = http.MaxBytesReader(w, req.Body, maxReqSize)
		}
		h.ServeHTTP(w, req)
	})
}

// Used as a request object for api queries
type requestQuery struct {
	Filter       string        `json:"filter,omitempty"`
	FilterParams []interface{} `json:"filter_params,omitempty"`
	SumBy        []string      `json:"sum_by,omitempty"`
	PageSize     int           `json:"page_size"`

	// AscLongPoll and Timeout are used by /list-transactions
	// to facilitate notifications.
	AscLongPoll bool          `json:"ascending_with_long_poll,omitempty"`
	Timeout     json.Duration `json:"timeout"`

	// After is a completely opaque cursor, indicating that only
	// items in the result set after the one identified by `After`
	// should be included. It has no relationship to time.
	After string `json:"after"`

	// These two are used for time-range queries like /list-transactions
	StartTimeMS uint64 `json:"start_time,omitempty"`
	EndTimeMS   uint64 `json:"end_time,omitempty"`

	// This is used for point-in-time queries like /list-balances
	// TODO(bobg): Different request structs for endpoints with different needs
	TimestampMS uint64 `json:"timestamp,omitempty"`

	// This is used for filtering results from /list-access-tokens
	// Value must be "client" or "network"
	Type string `json:"type"`

	// Aliases is used to filter results from /mockshm/list-keys
	Aliases []string `json:"aliases,omitempty"`
}

// Used as a response object for api queries
type page struct {
	Items    interface{}  `json:"items"`
	Next     requestQuery `json:"next"`
	LastPage bool         `json:"last_page"`
	After    string       `json:"after"`
}

// NewProtocalReactor returns the reactor of whole blockchain.
func NewProtocalReactor(chain *protocol.Chain, txPool *protocol.TxPool, accounts *account.Manager, sw *p2p.Switch, miningEnable bool) *ProtocalReactor {
	pr := &ProtocalReactor{
		chain: chain,
		// wallet:        wallet,
		// accounts:      accounts,
		// assets:        assets,
		blockKeeper: newBlockKeeper(chain, sw),
		txPool:      txPool,
		mining:      cpuminer.NewCPUMiner(chain, accounts, txPool),
		miningPool:  miningpool.NewMiningPool(chain, accounts, txPool),
		// mux:           http.NewServeMux(),
		sw: sw,
		// accessTokens:  accessTokens,
		miningEnable: miningEnable,
	}
	pr.BaseReactor = *p2p.NewBaseReactor("ProtocalReactor", pr)
	return pr
}

// OnStart implements BaseService
func (pr *ProtocalReactor) OnStart() error {
	pr.BaseReactor.OnStart()

	if pr.miningEnable {
		pr.mining.Start()
	}
	go pr.syncRoutine()
	return nil
}

// OnStop implements BaseService
func (pr *ProtocalReactor) OnStop() {
	pr.BaseReactor.OnStop()
	if pr.miningEnable {
		pr.mining.Stop()
	}
	pr.blockKeeper.Stop()
}

// GetChannels implements Reactor
func (pr *ProtocalReactor) GetChannels() []*p2p.ChannelDescriptor {
	return []*p2p.ChannelDescriptor{
		&p2p.ChannelDescriptor{
			ID:                BlockchainChannel,
			Priority:          5,
			SendQueueCapacity: 100,
		},
	}
}

// AddPeer implements Reactor by sending our state to peer.
func (pr *ProtocalReactor) AddPeer(peer *p2p.Peer) {
	peer.Send(BlockchainChannel, struct{ BlockchainMessage }{&StatusRequestMessage{}})
}

// RemovePeer implements Reactor by removing peer from the pool.
func (pr *ProtocalReactor) RemovePeer(peer *p2p.Peer, reason interface{}) {
	pr.blockKeeper.RemovePeer(peer.Key)
}

// Receive implements Reactor by handling 4 types of messages (look below).
func (pr *ProtocalReactor) Receive(chID byte, src *p2p.Peer, msgBytes []byte) {
	var tm *trust.TrustMetric
	key := src.Connection().RemoteAddress.IP.String()
	if tm = pr.sw.TrustMetricStore.GetPeerTrustMetric(key); tm == nil {
		log.Errorf("Can't get peer trust metric")
		return
	}

	_, msg, err := DecodeMessage(msgBytes)
	if err != nil {
		log.Errorf("Error decoding messagek %v", err)
		return
	}
	log.WithFields(log.Fields{"peerID": src.Key, "msg": msg}).Info("Receive request")

	switch msg := msg.(type) {
	case *BlockRequestMessage:
		var block *legacy.Block
		var err error
		if msg.Height != 0 {
			block, err = pr.chain.GetBlockByHeight(msg.Height)
		} else {
			block, err = pr.chain.GetBlockByHash(msg.GetHash())
		}
		if err != nil {
			log.Errorf("Fail on BlockRequestMessage get block: %v", err)
			return
		}
		response, err := NewBlockResponseMessage(block)
		if err != nil {
			log.Errorf("Fail on BlockRequestMessage create resoinse: %v", err)
			return
		}
		src.TrySend(BlockchainChannel, struct{ BlockchainMessage }{response})

	case *BlockResponseMessage:
		pr.blockKeeper.AddBlock(msg.GetBlock(), src)

	case *StatusRequestMessage:
		block := pr.chain.BestBlock()
		src.TrySend(BlockchainChannel, struct{ BlockchainMessage }{NewStatusResponseMessage(block)})

	case *StatusResponseMessage:
		pr.blockKeeper.SetPeerHeight(src.Key, msg.Height, msg.GetHash())

	case *TransactionNotifyMessage:
		tx := msg.GetTransaction()
		src.MarkTransaction(tx.ID.Byte32())
		if err := pr.chain.ValidateTx(tx); err != nil {
			pr.sw.AddScamPeer(src)
		}

	default:
		log.Error(cmn.Fmt("Unknown message type %v", reflect.TypeOf(msg)))
	}
}

// Handle messages from the poolReactor telling the reactor what to do.
// NOTE: Don't sleep in the FOR_LOOP or otherwise slow it down!
// (Except for the SYNC_LOOP, which is the primary purpose and must be synchronous.)
func (pr *ProtocalReactor) syncRoutine() {
	statusUpdateTicker := time.NewTicker(statusUpdateIntervalSeconds * time.Second)

	for {
		select {
		case _ = <-statusUpdateTicker.C:
			go pr.BroadcastStatusResponse()

			if pr.miningEnable {
				// mining if and only if block sync is finished
				if pr.blockKeeper.IsCaughtUp() {
					pr.mining.Start()
				} else {
					pr.mining.Stop()
				}
			}
		case <-pr.Quit:
			return
		}
	}
}

// BroadcastStatusResponse broadcasts `BlockStore` height.
func (pr *ProtocalReactor) BroadcastStatusResponse() {
	block := pr.chain.BestBlock()
	pr.Switch.Broadcast(BlockchainChannel, struct{ BlockchainMessage }{NewStatusResponseMessage(block)})
}

// BroadcastTransaction broadcats `BlockStore` transaction.
func (pr *ProtocalReactor) BroadcastTransaction(tx *legacy.Tx) error {
	msg, err := NewTransactionNotifyMessage(tx)
	if err != nil {
		return err
	}
	pr.Switch.Broadcast(BlockchainChannel, struct{ BlockchainMessage }{msg})
	return nil
}
