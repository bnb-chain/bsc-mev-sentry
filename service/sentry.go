package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"time"

	"github.com/tredeske/u/ustrings"
	"golang.org/x/sync/errgroup"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"

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

// BidArgsWrapper Override the BidArgs type to add validator host name
type BidArgsWrapper struct {
	types.BidArgs
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
		err = types.NewInvalidBidError(fmt.Sprintf("invalid signature:%v", err))
		return
	} else if _, ok := s.builders[builder]; !ok {
		log.Errorw("builder not registered", "address", builder)
		err = types.NewInvalidBidError("builder not registered")
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
			err = types.NewInvalidBidError(fmt.Sprintf("bid fee exceeds the ceiling %v", bidFeeCeil))
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

	log.Debugw("[BID SENT]", "block", args.RawBid.BlockNumber, "builder", builder, "hash", args.RawBid.Hash().TerminalString())
	return validator.SendBid(ctx, args.BidArgs, builder)
}

// BidBlockArgsWrapper wraps BidBlockArgs with a validator routing hint,
// mirroring BidArgsWrapper for the legacy SendBid path.
type BidBlockArgsWrapper struct {
	types.BidBlockArgs
	ValidatorHostName string `json:"validatorHostName,omitempty"`

	// decodeElapsed / payloadBytes are populated by UnmarshalJSON. The RPC
	// framework pays the full ingress JSON-decode cost (header + every tx as
	// hex + blob sidecars, ~256KB of hex per blob) BEFORE SendBidBlock is
	// entered, so without this it is invisible to every existing log.
	decodeElapsed time.Duration
	payloadBytes  int
}

// UnmarshalJSON decodes the wrapper in two passes:
//
//  1. Structural scan: walk the JSON once and capture Header / Transactions /
//     Sidecars as json.RawMessage leaves. No hex decode happens here; cost is
//     dominated by byte scanning to find field boundaries (~2-3ms for ~1.7MB).
//
//  2. Parallel leaf decode: the actual hex→binary work runs concurrently —
//     each blob sidecar gets its own goroutine (typical N ≤ 6, each ~3ms),
//     and transactions share a 2-worker pool. Header and signature are tiny
//     and decoded inline.
//
// EcrecoverSender still needs the typed BidBlock (Hash → rlpHash), so the
// decode itself is unavoidable; the savings come from running its dominant
// hex-decode in parallel.
func (w *BidBlockArgsWrapper) UnmarshalJSON(input []byte) error {
	start := time.Now()

	// Pass 1a: top-level fields. RawMessage copies the bytes it captures, so
	// the leaves stay valid after json's input buffer is reused.
	var top struct {
		BidBlock          json.RawMessage `json:"BidBlock"`
		Signature         json.RawMessage `json:"signature"`
		ValidatorHostName string          `json:"validatorHostName,omitempty"`
	}
	if err := json.Unmarshal(input, &top); err != nil {
		return err
	}

	// Pass 1b: inside BidBlock — pull out header / transactions / sidecars
	// as raw leaves for parallel decoding below.
	var bb struct {
		Header       json.RawMessage   `json:"header"`
		Transactions []json.RawMessage `json:"transactions"`
		Sidecars     []json.RawMessage `json:"sidecars,omitempty"`
	}
	if err := json.Unmarshal(top.BidBlock, &bb); err != nil {
		return err
	}

	bidBlock := &types.BidBlock{
		Transactions: make([]hexutil.Bytes, len(bb.Transactions)),
		Sidecars:     make(types.BlobSidecars, len(bb.Sidecars)),
	}

	// Header & signature are small — decode inline before fanning out.
	if len(bb.Header) > 0 {
		var hdr types.Header
		if err := json.Unmarshal(bb.Header, &hdr); err != nil {
			return fmt.Errorf("decode header: %w", err)
		}
		bidBlock.Header = &hdr
	}
	var sig hexutil.Bytes
	if len(top.Signature) > 0 {
		if err := sig.UnmarshalJSON(top.Signature); err != nil {
			return fmt.Errorf("decode signature: %w", err)
		}
	}

	g := new(errgroup.Group)

	// Sidecars: fan-out one goroutine per sidecar. N is small (typically ≤ 6)
	// and each is expensive (~3ms of hex→binary), so a pool would only add
	// scheduling cost.
	for i, sc := range bb.Sidecars {
		i, sc := i, sc
		g.Go(func() error {
			var s types.BlobSidecar
			if err := json.Unmarshal(sc, &s); err != nil {
				return fmt.Errorf("decode sidecar %d: %w", i, err)
			}
			bidBlock.Sidecars[i] = &s
			return nil
		})
	}

	// Transactions: 2-worker pool. Per-tx work is small (~0.05ms); sidecar
	// fan-out is the wall-clock floor anyway.
	const numTxWorkers = 2
	if n := len(bb.Transactions); n > 0 {
		workers := numTxWorkers
		if n < workers {
			workers = n
		}
		txCh := make(chan int, n)
		for i := 0; i < n; i++ {
			txCh <- i
		}
		close(txCh)
		for k := 0; k < workers; k++ {
			g.Go(func() error {
				for idx := range txCh {
					var b hexutil.Bytes
					if err := b.UnmarshalJSON(bb.Transactions[idx]); err != nil {
						return fmt.Errorf("decode tx %d: %w", idx, err)
					}
					bidBlock.Transactions[idx] = b
				}
				return nil
			})
		}
	}

	if err := g.Wait(); err != nil {
		return err
	}

	w.BidBlockArgs = types.BidBlockArgs{
		BidBlock:  bidBlock,
		Signature: sig,
	}
	w.ValidatorHostName = top.ValidatorHostName
	w.decodeElapsed = time.Since(start)
	w.payloadBytes = len(input)
	return nil
}

// SendBidBlock receives a BidBlock from a builder and proxies it to the target
// validator. Unlike SendBid, no PayBidTx is generated — the zero-simulate path
// requires pure transparent forwarding.
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

	if args.BidBlock == nil || args.BidBlock.Header == nil {
		log.Errorw("empty bid block or header")
		err = types.NewInvalidBidError("empty BidBlock or Header")
		return
	}
	// EcrecoverSender triggers BidBlock.Hash() = rlpHash(header + all txs +
	// blob sidecars). The hash is cached, so this first call pays the full
	// rlp-encode + keccak over the whole payload (blobs included); the later
	// Hash() calls in the log lines are free.
	ecStart := time.Now()
	builder, err := args.EcrecoverSender()
	ecElapsed := time.Since(ecStart)
	if err != nil {
		log.Errorw("failed to parse bid block signature", "err", err)
		err = types.NewInvalidBidError(fmt.Sprintf("invalid signature:%v", err))
		return
	} else if _, ok := s.builders[builder]; !ok {
		log.Errorw("builder not registered", "address", builder)
		err = types.NewInvalidBidError("builder not registered")
		return
	}
	log.Debugw("[BID BLOCK RECEIVED]", "block", args.BidBlock.Header.Number, "builder", builder, "hash", args.BidBlock.Hash().TerminalString())

	validator, err := s.validatorFromRequest(ctx, args.ValidatorHostName)
	if err != nil {
		return
	}

	log.Debugw("[BID BLOCK SENT]", "block", args.BidBlock.Header.Number, "builder", builder, "hash", args.BidBlock.Hash().TerminalString())
	// validator.SendBidBlock -> ethclient.CallContext re-marshals the entire
	// BidBlockArgs (blobs -> hex again) to JSON before the HTTP send, so
	// forwardUs bundles egress-encode + network + validator handler + resp.
	fwdStart := time.Now()
	bidHash, err = validator.SendBidBlock(ctx, args.BidBlockArgs, builder)
	fwdElapsed := time.Since(fwdStart)
	log.Debugw("[BID BLOCK TIMING]",
		"block", args.BidBlock.Header.Number,
		"hash", args.BidBlock.Hash().TerminalString(),
		"txs", len(args.BidBlock.Transactions),
		"sidecars", len(args.BidBlock.Sidecars),
		"payloadKB", args.payloadBytes/1024,
		"decodeUs", args.decodeElapsed.Microseconds(),
		"ecrecoverUs", ecElapsed.Microseconds(),
		"forwardUs", fwdElapsed.Microseconds(),
		"totalUs", time.Since(start).Microseconds())
	return bidHash, err
}

// GetBidBlockPermissionArgs wraps the bare builder address with a validator
// routing hint, mirroring BidArgsWrapper / BidBlockArgsWrapper for the
// SendBid / SendBidBlock paths.
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
		log.Errorw("builder url not found", "address", issue.Builder, "issue", issue)
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

// validatorFromRequest resolves the target validator for an RPC call.
// validatorHostName, when non-empty, overrides the HTTP Host-based routing —
// used by SendBid / SendBidBlock so a builder can pick the validator
// explicitly via the wrapper's ValidatorHostName field. Other RPCs pass "".
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
		return nil, types.NewInvalidBidError("validator hostname not found")
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
