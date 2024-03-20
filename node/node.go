package node

import (
	"context"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/go-co-op/gocron"

	"github.com/bnb-chain/bsc-mev-sentry/log"
)

type FullNode interface {
	ChainID() *big.Int
	PendingNonceAt(context.Context, common.Address) (uint64, error)
	Balance(context.Context, common.Address) (*big.Int, error)
}

type FullNodeConfig struct {
	URL string
}

func NewFullNode(config *FullNodeConfig) FullNode {
	cli, err := ethclient.DialOptions(context.Background(), config.URL, rpc.WithHTTPClient(client))
	if err != nil {
		log.Errorw("failed to dial validator", "url", config.URL, "err", err)
		return nil
	}

	f := &fullNode{
		cfg:       config,
		client:    cli,
		scheduler: gocron.NewScheduler(time.UTC),
	}

	if _, err := f.scheduler.Every(1).Hours().Do(func() {
		f.refresh()
	}); err != nil {
		log.Debugw("error while setting up scheduler", "err", err)
	}

	f.scheduler.StartAsync()

	return f
}

type fullNode struct {
	cfg    *FullNodeConfig
	client *ethclient.Client

	scheduler *gocron.Scheduler
	chainID   *big.Int
}

func (f *fullNode) ChainID() *big.Int {
	return f.chainID
}

func (f *fullNode) PendingNonceAt(ctx context.Context, account common.Address) (uint64, error) {
	return f.client.PendingNonceAt(ctx, account)
}

func (f *fullNode) Balance(ctx context.Context, account common.Address) (*big.Int, error) {
	return f.client.BalanceAt(ctx, account, nil)
}

func (f *fullNode) refresh() {
	chainID, err := f.client.ChainID(context.Background())
	if err != nil {
		log.Errorw("failed to fetch chain id", "err", err)
		return
	}

	f.chainID = chainID
}
