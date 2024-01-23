package node

import (
	"context"
	"crypto/tls"
	"math/big"
	"net"
	"net/http"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/go-co-op/gocron"

	"github.com/bnb-chain/bsc-mev-sentry/log"
)

var (
	dialer = &net.Dialer{
		Timeout:   time.Second,
		KeepAlive: 60 * time.Second,
	}

	transport = &http.Transport{
		DialContext:         dialer.DialContext,
		MaxIdleConnsPerHost: 50,
		MaxConnsPerHost:     50,
		IdleConnTimeout:     90 * time.Second,
		TLSClientConfig:     &tls.Config{InsecureSkipVerify: true},
	}

	client = &http.Client{
		Timeout:   5 * time.Second,
		Transport: transport,
	}
)

type Validator interface {
	SendBid(context.Context, types.BidArgs) (common.Hash, error)
	MevRunning() bool
}

type ValidatorConfig struct {
	PrivateURL     string
	PublicHostName string
}

func NewValidator(config *ValidatorConfig) Validator {
	cli, err := ethclient.DialOptions(context.Background(), config.PrivateURL, rpc.WithHTTPClient(client))
	if err != nil {
		log.Errorw("failed to dial validator", "url", config.PrivateURL, "err", err)
		return nil
	}

	v := &validator{
		cfg:       config,
		client:    cli,
		scheduler: gocron.NewScheduler(time.UTC),
	}

	if _, err := v.scheduler.Every(1).Hours().Do(func() {
		v.refresh()
	}); err != nil {
		log.Debugw("error while setting up scheduler", "err", err)
	}

	v.scheduler.StartAsync()

	return v
}

type validator struct {
	cfg    *ValidatorConfig
	client *ethclient.Client

	scheduler  *gocron.Scheduler
	chainID    *big.Int
	mevRunning bool
}

func (n *validator) SendBid(ctx context.Context, args types.BidArgs) (common.Hash, error) {
	return n.client.SendBid(ctx, args)
}

func (n *validator) MevRunning() bool {
	return n.mevRunning
}

func (n *validator) refresh() {
	mevRunning, err := n.client.MevRunning(context.Background())
	if err != nil {
		log.Errorw("failed to fetch mev running status", "err", err)
	}

	n.mevRunning = mevRunning
}
