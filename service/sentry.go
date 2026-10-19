package service

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"time"

	"github.com/tredeske/u/ustrings"

	"github.com/ethereum/go-ethereum/common"
	buildertypes "github.com/ethereum/go-ethereum/core/types/builder"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"

	"github.com/bnb-chain/bsc-mev-sentry/log"
	"github.com/bnb-chain/bsc-mev-sentry/metrics"
	"github.com/bnb-chain/bsc-mev-sentry/node"
)

type Config struct {
	// HTTPListenAddr define the address sentry service listen on
	HTTPListenAddr string
	// Empty disables the BidBlockService gRPC endpoint.
	GRPCListenAddr string
	// RPCConcurrency limits simultaneous requests
	RPCConcurrency int64
	// GRPCConcurrency optionally overrides the gRPC BidBlock limit.
	// Zero uses the default.
	GRPCConcurrency int64
	// RPCTimeout bounds an RPC. gRPC uses a safe default when unset.
	RPCTimeout Duration
}

// Default limit for in-flight gRPC BidBlocks.
const defaultGRPCConcurrency = 16

type MevSentry struct {
	timeout         Duration
	grpcConcurrency int64

	validators map[string]node.Validator // hostname -> validator
	builders   *BuilderSet               // allowlist; swapped atomically by the registry syncer
	registry   *RegistryState            // read-only status behind mev_registryStatus
}

// NewMevSentry builds a sentry over a static allowlist.
func NewMevSentry(cfg *Config,
	validators map[string]node.Validator,
	builders map[common.Address]node.Builder,
) *MevSentry {
	return NewMevSentryWithSet(cfg, validators, NewBuilderSet(builders),
		NewRegistryState(false, common.Address{}, len(builders)))
}

// NewMevSentryWithSet builds a sentry whose allowlist can be replaced at runtime
// through the shared BuilderSet (see registry.Syncer).
func NewMevSentryWithSet(cfg *Config,
	validators map[string]node.Validator,
	builders *BuilderSet,
	registry *RegistryState,
) *MevSentry {
	return &MevSentry{
		timeout:         cfg.RPCTimeout,
		grpcConcurrency: cfg.GRPCConcurrency,
		validators:      validators,
		builders:        builders,
		registry:        registry,
	}
}

// RegistryStatus reports where the current allowlist came from and at which block.
// Exposed as mev_registryStatus so operators can verify sentries have converged.
func (s *MevSentry) RegistryStatus() RegistryStatus {
	st := s.registry.Snapshot()
	st.BuilderCount = s.builders.Len()
	return st
}

// BidArgsWrapper Override the BidArgs type to add validator host name
type BidArgsWrapper struct {
	buildertypes.BidArgs
	ValidatorHostName string `json:"validatorHostName,omitempty"`
}

func (s *MevSentry) SendBid(ctx context.Context, args BidArgsWrapper) (bidHash common.Hash, err error) {
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

	builder, err := args.EcrecoverSender()
	if err != nil {
		log.Errorw("failed to parse bid signature", "err", err)
		err = buildertypes.NewInvalidBidError(fmt.Sprintf("invalid signature:%v", err))
		return
	} else if _, ok := s.builders.Lookup(builder); !ok {
		log.Errorw("builder not registered", "address", builder)
		err = buildertypes.NewInvalidBidError("builder not registered")
		return
	}
	log.Debugw("[BID RECEIVED]", "block", args.RawBid.BlockNumber, "builder", builder, "hash", args.RawBid.Hash().TerminalString())

	validator, err := s.validatorFromRequest(ctx, args.ValidatorHostName)
	if err != nil {
		return
	}

	bidFeeCeil := validator.BuilderFeeCeil()

	if args.RawBid.BuilderFee != nil && bidFeeCeil != nil {
		if args.RawBid.BuilderFee.Cmp(bidFeeCeil) > 0 {
			log.Errorw("bid fee exceeds the ceiling", "fee", args.RawBid.BuilderFee, "ceiling", bidFeeCeil.Uint64())
			err = buildertypes.NewInvalidBidError(fmt.Sprintf("bid fee exceeds the ceiling %v", bidFeeCeil))
			return
		}
	}

	gstart := time.Now()
	payBidTx, err := validator.GeneratePayBidTx(ctx, args.BidArgs, builder, args.RawBid.BuilderFee)
	if err != nil {
		log.Errorw("failed to create pay bid tx", "err", err)
		err = newSentryError("failed to create pay bid tx")
		return
	}
	log.Debugw("GeneratePayBidTx", "block", args.RawBid.BlockNumber, "builder", builder, "hash", args.RawBid.Hash().TerminalString(), "elapsed", time.Since(gstart).Milliseconds())

	args.PayBidTx = payBidTx
	args.PayBidTxGasUsed = node.PayBidTxGasUsed

	bidHash, err = validator.SendBid(ctx, args.BidArgs, builder)
	if err == nil {
		log.Debugw("[BID SENT]",
			"block", args.RawBid.BlockNumber,
			"builder", builder,
			"hash", args.RawBid.Hash().TerminalString(),
			"txs", len(args.RawBid.Txs),
			"handlerUs", time.Since(start).Microseconds())
	}
	return bidHash, err
}

// BidBlockArgsWrapper adds validator routing to BidBlockArgs.
type BidBlockArgsWrapper struct {
	buildertypes.BidBlockArgs
	ValidatorHostName string `json:"validatorHostName,omitempty"`
}

// SendBidBlock forwards a BidBlock without generating a PayBidTx.
func (s *MevSentry) SendBidBlock(ctx context.Context, args BidBlockArgsWrapper) (bidHash common.Hash, err error) {
	method := "mev_sendBidBlock"
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

	bidHash, err = s.sendBidBlock(ctx, args, nil)
	if err == nil {
		log.Debugw("[BID BLOCK JSON]",
			"block", args.BidBlock.Header.Number,
			"hash", bidHash.TerminalString(),
			"txs", len(args.BidBlock.Transactions),
			"sidecars", len(args.BidBlock.Sidecars),
			"txBytes", bidBlockTxBytes(args.BidBlock),
			"handlerUs", time.Since(start).Microseconds())
	}
	return bidHash, err
}

// sendBidBlock is shared by JSON-RPC and gRPC. Each ingress forwards over its own
// transport: a non-nil bidBlockRLP is relayed to the validator over gRPC as is.
func (s *MevSentry) sendBidBlock(ctx context.Context, args BidBlockArgsWrapper, bidBlockRLP []byte) (bidHash common.Hash, err error) {
	if args.BidBlock == nil || args.BidBlock.Header == nil {
		log.Errorw("empty bid block or header")
		err = buildertypes.NewInvalidBidError("empty BidBlock or Header")
		return
	}
	signingHash := args.BidBlock.Hash()
	builder, err := args.EcrecoverSender()
	if err != nil {
		log.Errorw("failed to parse bid block signature", "err", err)
		err = buildertypes.NewInvalidBidError(fmt.Sprintf("invalid signature:%v", err))
		return
	} else if _, ok := s.builders.Lookup(builder); !ok {
		log.Errorw("builder not registered", "address", builder)
		err = buildertypes.NewInvalidBidError("builder not registered")
		return
	}
	log.Debugw("[BID BLOCK RECEIVED]", "block", args.BidBlock.Header.Number, "builder", builder, "hash", signingHash.TerminalString())

	validator, err := s.validatorFromRequest(ctx, args.ValidatorHostName)
	if err != nil {
		return
	}

	log.Debugw("[BID BLOCK SENT]", "block", args.BidBlock.Header.Number, "builder", builder, "hash", signingHash.TerminalString())
	if bidBlockRLP != nil {
		return validator.SendBidBlockRLP(ctx, bidBlockRLP, args.Signature, builder, signingHash)
	}
	return validator.SendBidBlock(ctx, args.BidBlockArgs, builder, signingHash)
}

// GetBidBlockPermissionArgs adds validator routing to a builder address.
type GetBidBlockPermissionArgs struct {
	Builder           common.Address `json:"builder"`
	ValidatorHostName string         `json:"validatorHostName,omitempty"`
}

func (s *MevSentry) GetBidBlockPermission(ctx context.Context, args GetBidBlockPermissionArgs) (result *ethclient.BidBlockPermission, err error) {
	method := "mev_getBidBlockPermission"
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

	validator, err := s.validatorFromRequest(ctx, args.ValidatorHostName)
	if err != nil {
		return
	}

	return validator.GetBidBlockPermission(ctx, args.Builder)
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

	validator, err := s.validatorFromRequest(ctx, "")
	if err != nil {
		return
	}

	fee, err = validator.BestBidGasFee(ctx, parentHash)
	return
}

func (s *MevSentry) Params(ctx context.Context) (param *buildertypes.MevParams, err error) {
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

	validator, err := s.validatorFromRequest(ctx, "")
	if err != nil {
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
	defer func() {
		if err != nil {
			if rpcErr, ok := err.(rpc.Error); ok {
				metrics.ApiErrorCounter.WithLabelValues(method, strconv.Itoa(rpcErr.ErrorCode())).Inc()
			}
		}
	}()

	validator, err := s.validatorFromRequest(ctx, "")
	if err != nil {
		return
	}

	return validator.MevRunning(), nil
}

func (s *MevSentry) HasBuilder(ctx context.Context, builder common.Address) (has bool, err error) {
	method := "mev_hasBuilder"
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

	validator, err := s.validatorFromRequest(ctx, "")
	if err != nil {
		return
	}

	return validator.HasBuilder(ctx, builder)
}

func (s *MevSentry) ReportIssue(ctx context.Context, issue buildertypes.BidIssue) (err error) {
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

	builder, ok = s.builders.Lookup(issue.Builder)
	if !ok {
		log.Errorw("builder url not found", "address", issue.Builder, "issue", issue)
		err = errors.New("builder not found")
		return
	}

	log.Debugw("report issue", "builder", builder, "issue", issue)

	err = builder.ReportIssue(ctx, issue)
	return
}

func recordLatency(method string, start time.Time) {
	metrics.ApiLatencyHist.WithLabelValues(method).Observe(float64(time.Since(start)) / float64(time.Millisecond))
}

func bidBlockTxBytes(bb *buildertypes.BidBlock) int {
	n := 0
	for _, tx := range bb.Transactions {
		n += len(tx)
	}
	return n
}

// validatorFromRequest resolves explicit or HTTP Host routing.
func (s *MevSentry) validatorFromRequest(ctx context.Context, validatorHostName string) (node.Validator, error) {
	hostname := rpc.PeerInfoFromContext(ctx).HTTP.Host
	if strings.Contains(hostname, ":") {
		hostname = hostname[:strings.Index(hostname, ":")]
	}

	if validatorHostName != "" {
		log.Debugw("hostname override", "from", hostname, "to", validatorHostName)
		hostname = validatorHostName
	} else {
		log.Debugw("hostname from context", "hostname", hostname)
	}

	validator, ok := s.validators[hostname]
	if !ok {
		log.Errorw("validator not found", "hostname", hostname)
		return nil, buildertypes.NewInvalidBidError("validator hostname not found")
	}
	return validator, nil
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
