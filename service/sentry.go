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
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/tredeske/u/ustrings"

	"github.com/bnb-chain/bsc-mev-sentry/account"
	"github.com/bnb-chain/bsc-mev-sentry/log"
	"github.com/bnb-chain/bsc-mev-sentry/node"
	"github.com/bnb-chain/bsc-mev-sentry/syncutils"
)

var (
	namespace = "bsc_mev_sentry"

	apiLatencyHist = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace,
		Subsystem: "api",
		Name:      "latency",
		Buckets:   prometheus.ExponentialBuckets(0.01, 3, 15),
	}, []string{"method"})

	apiErrorCounter = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: "api",
		Name:      "error",
	}, []string{"method", "code"})

	accountError = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: "account",
		Name:      "error",
	}, []string{"reason"})

	chainError = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: "chainRPC",
		Name:      "error",
	})
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
	chainRPC   node.ChainRPC
}

func NewMevSentry(cfg *Config,
	account account.Account,
	validators map[string]node.Validator,
	builders map[common.Address]node.Builder,
	chain node.ChainRPC,
) *MevSentry {
	s := &MevSentry{
		timeout:    cfg.RPCTimeout,
		account:    account,
		validators: validators,
		builders:   builders,
		chainRPC:   chain,
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
				apiErrorCounter.WithLabelValues(method, strconv.Itoa(rpcErr.ErrorCode())).Inc()
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

	builder, err := args.EcrecoverSender()
	if err != nil {
		log.Errorw("failed to parse bid signature", "err", err)
		err = types.NewInvalidBidError(fmt.Sprintf("invalid signature:%v", err))
		return
	}

	payBidTx, err := s.GeneratePayBidTx(ctx, builder, args.RawBid.BuilderFee)
	if err != nil {
		log.Errorw("failed to create pay bid tx", "err", err)
		err = newSentryError("failed to create pay bid tx")
		return
	}

	args.PayBidTx = payBidTx

	return validator.SendBid(ctx, args)
}

func (s *MevSentry) GeneratePayBidTx(ctx context.Context, builder common.Address, builderFee *big.Int) (hexutil.Bytes, error) {
	// take pay bid tx as block tag
	var (
		amount  = big.NewInt(0)
		balance *big.Int
		nonce   uint64
	)

	err := syncutils.BatchRun(
		func() error {
			var er error
			balance, er = s.chainRPC.Balance(ctx, s.account.Address())
			if er != nil {
				return er
			}

			return nil
		},
		func() error {
			var er error
			nonce, er = s.chainRPC.PendingNonceAt(ctx, s.account.Address())
			if er != nil {
				return er
			}

			return nil
		})

	if err != nil {
		chainError.Inc()
		log.Errorw("failed to query sentry balance or nonce", "err", err)
		return nil, err
	}

	if builderFee != nil {
		amount = builderFee
	}

	if balance.Cmp(amount) < 0 {
		accountError.WithLabelValues("insufficient_balance").Inc()
		log.Errorw("insufficient balance", "balance", balance.Uint64(), "builderFee", builderFee.Uint64())
		return nil, errors.New("insufficient balance")
	}

	tx := types.NewTx(&types.LegacyTx{
		Nonce:    nonce,
		GasPrice: big.NewInt(0),
		Gas:      25000,
		To:       &builder,
		Value:    amount,
	})

	signedTx, err := s.account.SignTx(tx, amount)
	if err != nil {
		log.Errorw("failed to sign pay bid tx", "err", err)
		return nil, err
	}

	payBidTx, err := signedTx.MarshalBinary()
	if err != nil {
		log.Errorw("failed to marshal pay bid tx", "err", err)
		return nil, err
	}

	return payBidTx, nil
}

func (s *MevSentry) BestBidGasFee(ctx context.Context, parentHash common.Hash) (fee *big.Int, err error) {
	method := "mev_bestBidGasFee"
	start := time.Now()
	defer recordLatency(method, start)
	defer timeoutCancel(&ctx, s.timeout)()
	defer func() {
		if err != nil {
			if rpcErr, ok := err.(rpc.Error); ok {
				apiErrorCounter.WithLabelValues(method, strconv.Itoa(rpcErr.ErrorCode())).Inc()
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
				apiErrorCounter.WithLabelValues(method, strconv.Itoa(rpcErr.ErrorCode())).Inc()
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
				apiErrorCounter.WithLabelValues(method, strconv.Itoa(rpcErr.ErrorCode())).Inc()
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
				apiErrorCounter.WithLabelValues(method, strconv.Itoa(rpcErr.ErrorCode())).Inc()
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
	return ustrings.UnsafeStringToBytes(time.Duration(d).String()), nil
}

func (d *Duration) UnmarshalText(text []byte) error {
	dd, err := time.ParseDuration(ustrings.UnsafeBytesToString(text))
	*d = Duration(dd)
	return err
}
