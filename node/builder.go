package node

import (
	"context"
	"errors"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	buildertypes "github.com/ethereum/go-ethereum/core/types/builder"
	"github.com/ethereum/go-ethereum/miner/builderclient"
	"github.com/ethereum/go-ethereum/rpc"

	"github.com/bnb-chain/bsc-mev-sentry/log"
)

type Builder interface {
	ReportIssue(context.Context, buildertypes.BidIssue) error
}

// BuilderConfigurer is implemented by builders that can report the config they
// were created from. The allowlist uses it to reuse connections across reloads.
type BuilderConfigurer interface {
	Config() BuilderConfig
}

type BuilderConfig struct {
	Address common.Address
	URL     string
}

var errBuilderEndpointUnavailable = errors.New("builder endpoint unavailable")

// NewBuilder returns a Builder for config. It never returns nil: a key must stay
// in the allowlist even when its issue-reporting endpoint cannot be dialed, so
// the client is created lazily on the first ReportIssue and retried on failure.
func NewBuilder(config BuilderConfig) Builder {
	return &builder{cfg: config}
}

type builder struct {
	cfg BuilderConfig

	mu     sync.Mutex
	client *builderclient.Client
}

func (b *builder) Config() BuilderConfig {
	return b.cfg
}

func (b *builder) ReportIssue(ctx context.Context, issue buildertypes.BidIssue) error {
	cli, err := b.dial(ctx)
	if err != nil {
		return err
	}
	return cli.ReportIssue(ctx, &issue)
}

func (b *builder) dial(ctx context.Context) (*builderclient.Client, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.client != nil {
		return b.client, nil
	}
	if b.cfg.URL == "" {
		return nil, errBuilderEndpointUnavailable
	}
	cli, err := builderclient.DialOptions(ctx, b.cfg.URL, rpc.WithHTTPClient(client))
	if err != nil {
		log.Warnw("failed to dial builder", "address", b.cfg.Address, "url", b.cfg.URL, "err", err)
		return nil, err
	}
	b.client = cli
	return cli, nil
}
