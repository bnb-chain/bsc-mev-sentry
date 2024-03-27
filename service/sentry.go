package service

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/tredeske/u/ustrings"

	"github.com/bnb-chain/bsc-mev-sentry/log"
	"github.com/bnb-chain/bsc-mev-sentry/metrics"
	"github.com/bnb-chain/bsc-mev-sentry/node"
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

	validators map[string]node.Validator       // hostname -> validator
	builders   map[common.Address]node.Builder // address -> builder
}

func NewMevSentry(cfg *Config,
	validators map[string]node.Validator,
	builders map[common.Address]node.Builder,
) *MevSentry {
	s := &MevSentry{
		timeout:    cfg.RPCTimeout,
		validators: validators,
		builders:   builders,
	}

	return s
}

func (s *MevSentry) SendBid(ctx context.Context, args types.BidArgs) (bidHash common.Hash, err error) {
	method := "mev_sendBid"
	start := time.Now()
	defer recordLatency(method, start)
	defer timeoutCancel(&ctx, s.timeout)()
	defer func() {
		if err != nil {
			if rpcErr, ok := err.(rpc.Error); ok {
				metrics.ApiErrorCounter.WithLabelValues(method, strconv.Itoa(rpcErr.ErrorCode())).Inc()
			}
		}
	}()

	hostname := rpc.PeerInfoFromContext(ctx).HTTP.Host
	if strings.Contains(hostname, ":") {
		hostname = hostname[:strings.Index(hostname, ":")]
	}

	validator, ok := s.validators[hostname]
	if !ok {
		log.Errorw("validator not found", "hostname", hostname)
		err = types.NewInvalidBidError("validator hostname not found")
		return
	}

	bidFeeCeil := big.NewInt(int64(validator.BidFeeCeil()))

	if args.RawBid.BuilderFee.Cmp(bidFeeCeil) > 0 {
		log.Errorw("bid fee exceeds the ceiling", "fee", args.RawBid.BuilderFee, "ceiling", bidFeeCeil.Uint64())
		err = types.NewInvalidBidError(fmt.Sprintf("bid fee exceeds the ceiling %v", bidFeeCeil))
		return
	}

	builder, err := args.EcrecoverSender()
	if err != nil {
		log.Errorw("failed to parse bid signature", "err", err)
		err = types.NewInvalidBidError(fmt.Sprintf("invalid signature:%v", err))
		return
	}

	payBidTx, err := validator.GeneratePayBidTx(ctx, builder, args.RawBid.BuilderFee)
	if err != nil {
		log.Errorw("failed to create pay bid tx", "err", err)
		err = newSentryError("failed to create pay bid tx")
		return
	}

	args.PayBidTx = payBidTx

	return validator.SendBid(ctx, args)
}

func (s *MevSentry) BestBidGasFee(ctx context.Context, parentHash common.Hash) (fee *big.Int, err error) {
	method := "mev_bestBidGasFee"
	start := time.Now()
	defer recordLatency(method, start)
	defer timeoutCancel(&ctx, s.timeout)()
	defer func() {
		if err != nil {
			if rpcErr, ok := err.(rpc.Error); ok {
				metrics.ApiErrorCounter.WithLabelValues(method, strconv.Itoa(rpcErr.ErrorCode())).Inc()
			}
		}
	}()

	hostname := rpc.PeerInfoFromContext(ctx).HTTP.Host
	if strings.Contains(hostname, ":") {
		hostname = hostname[:strings.Index(hostname, ":")]
	}

	validator, ok := s.validators[hostname]
	if !ok {
		log.Errorw("validator not found", "hostname", hostname)
		err = types.NewInvalidBidError("validator hostname not found")
		return
	}

	fee, err = validator.BestBidGasFee(ctx, parentHash)
	return
}

func (s *MevSentry) Params(ctx context.Context) (param *types.MevParams, err error) {
	method := "mev_params"
	start := time.Now()
	defer recordLatency(method, start)
	defer timeoutCancel(&ctx, s.timeout)()
	defer func() {
		if err != nil {
			if rpcErr, ok := err.(rpc.Error); ok {
				metrics.ApiErrorCounter.WithLabelValues(method, strconv.Itoa(rpcErr.ErrorCode())).Inc()
			}
		}
	}()

	hostname := rpc.PeerInfoFromContext(ctx).HTTP.Host
	if strings.Contains(hostname, ":") {
		hostname = hostname[:strings.Index(hostname, ":")]
	}

	validator, ok := s.validators[hostname]
	if !ok {
		log.Errorw("validator not found", "hostname", hostname)
		err = types.NewInvalidBidError("validator hostname not found")
		return
	}

	param, err = validator.MevParams(ctx)
	return
}

func (s *MevSentry) Running(ctx context.Context) (running bool, err error) {
	method := "mev_running"
	start := time.Now()
	defer recordLatency(method, start)
	defer timeoutCancel(&ctx, s.timeout)()
	defer timeoutCancel(&ctx, s.timeout)()
	defer func() {
		if err != nil {
			if rpcErr, ok := err.(rpc.Error); ok {
				metrics.ApiErrorCounter.WithLabelValues(method, strconv.Itoa(rpcErr.ErrorCode())).Inc()
			}
		}
	}()

	hostname := rpc.PeerInfoFromContext(ctx).HTTP.Host
	if strings.Contains(hostname, ":") {
		hostname = hostname[:strings.Index(hostname, ":")]
	}

	validator, ok := s.validators[hostname]
	if !ok {
		log.Errorw("validator not found", "hostname", hostname)
		err = types.NewInvalidBidError("validator hostname not found")
		return
	}

	return validator.MevRunning(), nil
}

func (s *MevSentry) ReportIssue(ctx context.Context, issue types.BidIssue) (err error) {
	method := "mev_reportIssue"
	start := time.Now()
	defer recordLatency(method, start)
	defer timeoutCancel(&ctx, s.timeout)()
	defer func() {
		if err != nil {
			if rpcErr, ok := err.(rpc.Error); ok {
				metrics.ApiErrorCounter.WithLabelValues(method, strconv.Itoa(rpcErr.ErrorCode())).Inc()
			}
		}
	}()

	var builder node.Builder
	var ok bool

	builder, ok = s.builders[issue.Builder]
	if !ok {
		log.Errorw("builder not found", "address", issue.Builder)
		err = errors.New("builder not found")
		return
	}

	log.Debugw("report issue", "builder", builder, "issue", issue)

	err = builder.ReportIssue(ctx, issue)
	return
}

func recordLatency(method string, start time.Time) {
	metrics.ApiLatencyHist.WithLabelValues(method).Observe(float64(time.Since(start).Milliseconds()))
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
	return ustrings.UnsafeStringToBytes(time.Duration(d).String()), nil
}

func (d *Duration) UnmarshalText(text []byte) error {
	dd, err := time.ParseDuration(ustrings.UnsafeBytesToString(text))
	*d = Duration(dd)
	return err
}
