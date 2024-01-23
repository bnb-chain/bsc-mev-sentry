package node

import (
	"context"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"

	"github.com/bnb-chain/bsc-mev-sentry/log"
)

type Builder interface {
	ReportIssue(context.Context, types.BidIssue) error
}

type BuilderConfig struct {
	Address common.Address
	URL     string
}

func NewBuilder(config *BuilderConfig) Builder {
	cli, err := ethclient.DialOptions(context.Background(), config.URL, rpc.WithHTTPClient(client))
	if err != nil {
		log.Errorw("failed to dial builder", "url", config.URL, "err", err)
		return nil
	}

	return &builder{
		cfg:    config,
		client: cli,
	}
}

type builder struct {
	cfg    *BuilderConfig
	client *ethclient.Client
}

func (b *builder) ReportIssue(ctx context.Context, issue types.BidIssue) error {
	return b.client.ReportIssue(ctx, &issue)
}
