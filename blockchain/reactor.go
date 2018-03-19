package blockchain

import (
	"context"
	"net/http"

	"github.com/bytom/blockchain/accesstoken"
	"github.com/bytom/blockchain/account"
	"github.com/bytom/blockchain/asset"
	"github.com/bytom/blockchain/pseudohsm"
	"github.com/bytom/blockchain/txfeed"
	"github.com/bytom/blockchain/wallet"
	"github.com/bytom/encoding/json"
	"github.com/bytom/p2p"
	"github.com/bytom/protocol"
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

//BlockchainReactor handles long-term catchup syncing.
type BlockchainReactor struct {
	p2p.BaseReactor

	chain         *protocol.Chain
	wallet        *wallet.Wallet
	accounts      *account.Manager
	assets        *asset.Registry
	accessTokens  *accesstoken.CredentialStore
	txFeedTracker *txfeed.Tracker
	// blockKeeper   *blockKeeper
	// txPool       *protocol.TxPool
	hsm *pseudohsm.HSM
	// mining     *cpuminer.CPUMiner
	// miningPool *miningpool.MiningPool
	mux *http.ServeMux
	// sw      *p2p.Switch
	handler http.Handler
	evsw    types.EventSwitch
	// miningEnable bool
}

func (bcr *BlockchainReactor) info(ctx context.Context) (map[string]interface{}, error) {
	return map[string]interface{}{
		"is_configured": false,
		"version":       "0.001",
		"build_commit":  "----",
		"build_date":    "------",
		"build_config":  "---------",
	}, nil
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

// NewBlockchainReactor returns the reactor of whole blockchain.
func NewBlockchainReactor(chain *protocol.Chain, accounts *account.Manager, assets *asset.Registry, hsm *pseudohsm.HSM, wallet *wallet.Wallet, txfeeds *txfeed.Tracker, accessTokens *accesstoken.CredentialStore) *BlockchainReactor {
	bcr := &BlockchainReactor{
		chain:    chain,
		wallet:   wallet,
		accounts: accounts,
		assets:   assets,
		// blockKeeper:   newBlockKeeper(chain, sw),
		// txPool:        txPool,
		// mining:        cpuminer.NewCPUMiner(chain, accounts, txPool),
		// miningPool:    miningpool.NewMiningPool(chain, accounts, txPool),
		mux: http.NewServeMux(),
		// sw:            sw,
		hsm:           hsm,
		txFeedTracker: txfeeds,
		accessTokens:  accessTokens,
		// miningEnable:  miningEnable,
	}
	bcr.BaseReactor = *p2p.NewBaseReactor("BlockchainReactor", bcr)
	return bcr
}

// OnStart implements BaseService
func (bcr *BlockchainReactor) OnStart() error {
	bcr.BaseReactor.OnStart()
	bcr.BuildHandler()

	return nil
}

// OnStop implements BaseService
func (bcr *BlockchainReactor) OnStop() {
	bcr.BaseReactor.OnStop()
}

// GetChannels implements Reactor
func (bcr *BlockchainReactor) GetChannels() []*p2p.ChannelDescriptor {
	return []*p2p.ChannelDescriptor{
		&p2p.ChannelDescriptor{
			ID:                BlockchainChannel,
			Priority:          5,
			SendQueueCapacity: 100,
		},
	}
}
