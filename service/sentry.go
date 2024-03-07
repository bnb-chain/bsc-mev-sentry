package service

import (
	"context"
	"errors"
	"math/big"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/bnb-chain/bsc-mev-sentry/account"
	"github.com/bnb-chain/bsc-mev-sentry/log"
	"github.com/bnb-chain/bsc-mev-sentry/node"
	"github.com/bnb-chain/bsc-mev-sentry/utils"
)

var (
	namespace = "bsc_mev_sentry"

	apiLatencyHist = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace,
		Subsystem: "api",
		Name:      "latency",
		Buckets:   prometheus.ExponentialBuckets(0.01, 3, 15),
	}, []string{"method"})
)

type Config struct {
	// HTTPListenAddr define the address sentry service listen on
	HTTPListenAddr string
	// RPCConcurrency limits simultaneous requests
	RPCConcurrency int64
	// RPCTimeout rpc request timeout
	RPCTimeout Duration
}

type MevSentry struct {
	timeout Duration

	account    account.Account
	validators map[string]node.Validator       // hostname -> validator
	builders   map[common.Address]node.Builder // address -> builder
	fullNode   node.FullNode
}

func NewMevSentry(cfg *Config,
	account account.Account,
	validators map[string]node.Validator,
	builders map[common.Address]node.Builder,
	fullNode node.FullNode) *MevSentry {
	s := &MevSentry{
		timeout:    cfg.RPCTimeout,
		account:    account,
		validators: validators,
		builders:   builders,
		fullNode:   fullNode,
	}

	return s
}

func (s *MevSentry) SendBid(ctx context.Context, args types.BidArgs) (common.Hash, error) {
	method := "mev_sendBid"
	start := time.Now()
	defer recordLatency(method, start)
	defer timeoutCancel(&ctx, s.timeout)()

	hostname := rpc.PeerInfoFromContext(ctx).HTTP.Host
	if strings.Contains(hostname, ":") {
		hostname = hostname[:strings.Index(hostname, ":")]
	}

	validator, ok := s.validators[hostname]
	if !ok {
		log.Errorw("validator not found", "hostname", hostname)
		return common.Hash{}, errors.New("validator hostname not found")
	}

	builder, err := args.EcrecoverSender()
	if err != nil {
		log.Errorw("failed to parse bid signature", "err", err)
		return common.Hash{}, err
	}

	if args.RawBid.BuilderFee != nil && args.RawBid.BuilderFee.Cmp(big.NewInt(0)) > 0 {
		payBidTx, er := s.account.PayBidTx(ctx, s.fullNode, builder, args.RawBid.BuilderFee)
		if er != nil {
			log.Errorw("failed to create pay bid tx", "err", err)
			return common.Hash{}, err
		}

		args.PayBidTx = payBidTx
	}

	return validator.SendBid(ctx, args)
}

func (s *MevSentry) BestBidGasFee(ctx context.Context, parentHash common.Hash) (*big.Int, error) {
	method := "mev_bestBidGasFee"
	start := time.Now()
	defer recordLatency(method, start)
	defer timeoutCancel(&ctx, s.timeout)()

	hostname := rpc.PeerInfoFromContext(ctx).HTTP.Host
	if strings.Contains(hostname, ":") {
		hostname = hostname[:strings.Index(hostname, ":")]
	}

	validator, ok := s.validators[hostname]
	if !ok {
		log.Errorw("validator not found", "hostname", hostname)
		return nil, errors.New("validator hostname not found")
	}

	return validator.BestBidGasFee(ctx, parentHash)
}

func (s *MevSentry) Params(ctx context.Context) (*types.MevParams, error) {
	method := "mev_params"
	start := time.Now()
	defer recordLatency(method, start)
	defer timeoutCancel(&ctx, s.timeout)()

	hostname := rpc.PeerInfoFromContext(ctx).HTTP.Host
	if strings.Contains(hostname, ":") {
		hostname = hostname[:strings.Index(hostname, ":")]
	}

	validator, ok := s.validators[hostname]
	if !ok {
		log.Errorw("validator not found", "hostname", hostname)
		return nil, errors.New("validator hostname not found")
	}

	return validator.MevParams(ctx)
}

func (s *MevSentry) Running(ctx context.Context) (bool, error) {
	method := "mev_running"
	start := time.Now()
	defer recordLatency(method, start)
	defer timeoutCancel(&ctx, s.timeout)()

	hostname := rpc.PeerInfoFromContext(ctx).HTTP.Host
	if strings.Contains(hostname, ":") {
		hostname = hostname[:strings.Index(hostname, ":")]
	}

	validator, ok := s.validators[hostname]
	if !ok {
		log.Errorw("validator not found", "hostname", hostname)
		return false, errors.New("validator hostname not found")
	}

	return validator.MevRunning(), nil
}

func (s *MevSentry) ReportIssue(ctx context.Context, args types.BidIssue) error {
	method := "mev_reportIssue"
	start := time.Now()
	defer recordLatency(method, start)
	defer timeoutCancel(&ctx, s.timeout)()

	var builder node.Builder
	var ok bool

	builder, ok = s.builders[args.Builder]
	if !ok {
		log.Errorw("builder not found", "address", args.Builder)
		return errors.New("builder not found")
	}

	return builder.ReportIssue(ctx, args)
}

func recordLatency(method string, start time.Time) {
	apiLatencyHist.WithLabelValues(method).Observe(float64(time.Since(start).Milliseconds()))
}

func nilCancel() {
}

func timeoutCancel(ctx *context.Context, timeout Duration) func() {
	if timeout > 0 {
		var cancel func()
		*ctx, cancel = context.WithTimeout(*ctx, time.Duration(timeout))
		return cancel
	}

	return nilCancel
}

type Duration time.Duration

func (d Duration) MarshalText() ([]byte, error) {
	return utils.UnsafeStringToBytes(time.Duration(d).String()), nil
}

func (d *Duration) UnmarshalText(text []byte) error {
	dd, err := time.ParseDuration(utils.UnsafeBytesToString(text))
	*d = Duration(dd)
	return err
}
