package service

import (
	"context"
	"fmt"
	"net"
	"runtime/debug"
	"strconv"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"

	buildertypes "github.com/ethereum/go-ethereum/core/types/builder"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/rpc"

	"github.com/bnb-chain/bsc-mev-sentry/log"
	"github.com/bnb-chain/bsc-mev-sentry/metrics"
	mevpb "github.com/bnb-chain/bsc-mev-sentry/proto"
)

// maxGRPCMsgSize bounds inbound BidBlock payloads. A full-blob BidBlock is
// ~1MB of raw bytes today; 16MB leaves room for future blob-count increases
// without silently hitting gRPC's 4MB default.
const maxGRPCMsgSize = 16 * 1024 * 1024

// BuilderRelayServer serves BEP-675 BidBlock traffic over gRPC. The RLP
// payload keeps blobs as raw bytes end to end, skipping the hex inflation and
// JSON parse cost of the mev_sendBidBlock JSON-RPC path.
type BuilderRelayServer struct {
	mevpb.UnimplementedBuilderRelayServer
	sentry *MevSentry
}

// SendBidBlock decodes the RLP BidBlock and hands it to the same core logic
// as the JSON-RPC handler (ecrecover, allowlist, routing, forwarding).
func (b *BuilderRelayServer) SendBidBlock(ctx context.Context, req *mevpb.BidBlockRequest) (*mevpb.BidBlockResponse, error) {
	method := "grpc_mev_sendBidBlock"
	start := time.Now()
	defer recordLatency(method, start)
	defer timeoutCancel(&ctx, b.sentry.timeout)()

	var bidBlock buildertypes.BidBlock
	decodeStart := time.Now()
	if err := rlp.DecodeBytes(req.BidBlockRlp, &bidBlock); err != nil {
		log.Errorw("failed to decode bid block rlp", "err", err)
		metrics.ApiErrorCounter.WithLabelValues(method, strconv.Itoa(buildertypes.InvalidBidParamError)).Inc()
		return nil, status.Errorf(codes.InvalidArgument, "invalid BidBlock rlp: %v", err)
	}
	decodeElapsed := time.Since(decodeStart)

	args := BidBlockArgsWrapper{
		BidBlockArgs: buildertypes.BidBlockArgs{
			BidBlock:  &bidBlock,
			Signature: req.Signature,
		},
		ValidatorHostName: req.ValidatorHostName,
	}

	bidHash, err := b.sentry.sendBidBlock(ctx, args)
	if err != nil {
		if rpcErr, ok := err.(rpc.Error); ok {
			metrics.ApiErrorCounter.WithLabelValues(method, strconv.Itoa(rpcErr.ErrorCode())).Inc()
		}
		return nil, toGRPCStatus(err)
	}

	log.Debugw("[BID BLOCK GRPC]",
		"block", bidBlock.Header.Number,
		"hash", bidHash.TerminalString(),
		"txs", len(bidBlock.Transactions),
		"sidecars", len(bidBlock.Sidecars),
		"payloadKB", len(req.BidBlockRlp)/1024,
		"decodeMs", decodeElapsed.Milliseconds(),
		"totalMs", time.Since(start).Milliseconds())
	return &mevpb.BidBlockResponse{BidHash: bidHash.Bytes()}, nil
}

// toGRPCStatus maps sentry errors onto gRPC codes: invalid-bid errors are the
// caller's fault (InvalidArgument), everything else is Internal.
func toGRPCStatus(err error) error {
	if rpcErr, ok := err.(rpc.Error); ok && rpcErr.ErrorCode() == buildertypes.InvalidBidParamError {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	return status.Error(codes.Internal, err.Error())
}

// recoveryInterceptor turns a handler panic into an Internal error instead of
// crashing the process (grpc-go does not recover handler panics; one would
// take down the JSON-RPC path sharing this process). Remote input flows into
// RLP decoding and downstream logic, so this is the process-level defense
// boundary — it recovers ordinary panics only, not OOM or fatal runtime
// errors. Mirrors ginutils.PanicRecovery on the JSON path. It must sit
// outermost in the interceptor chain. Only the stack is logged, never the
// request payload.
func recoveryInterceptor(ctx context.Context, req any, info *grpc.UnaryServerInfo,
	handler grpc.UnaryHandler) (resp any, err error) {
	defer func() {
		if r := recover(); r != nil {
			log.Errorw("grpc handler panic", "method", info.FullMethod, "panic", r,
				"stack", string(debug.Stack()))
			err = status.Error(codes.Internal, "internal error")
		}
	}()
	return handler(ctx, req)
}

// concurrencyInterceptor bounds in-flight requests with a process-wide
// semaphore shared with the JSON path's gin ConcurrencyLimiter, so the two
// listeners together respect one RPCConcurrency budget. Acquisition waits in
// line (matching the gin middleware) but aborts if the caller's context ends.
// Per-connection MaxConcurrentStreams is NOT sufficient here: a client can
// open more connections to bypass it.
func concurrencyInterceptor(sem chan struct{}) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler) (any, error) {
		if sem != nil {
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return nil, status.FromContextError(ctx.Err()).Err()
			}
		}
		return handler(ctx, req)
	}
}

// StartGRPCServer starts the BuilderRelay gRPC endpoint on addr. It runs
// alongside the JSON-RPC listener; both feed the same MevSentry core. sem is
// the shared concurrency semaphore (nil = unlimited) — pass the same channel
// the gin middleware uses so total process concurrency stays bounded.
func StartGRPCServer(addr string, sentry *MevSentry, sem chan struct{}) (*grpc.Server, error) {
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("grpc listen on %s: %w", addr, err)
	}

	srv := grpc.NewServer(
		grpc.MaxRecvMsgSize(maxGRPCMsgSize),
		grpc.MaxSendMsgSize(maxGRPCMsgSize),
		// recovery outermost so it also covers the other interceptors.
		grpc.ChainUnaryInterceptor(recoveryInterceptor, concurrencyInterceptor(sem)),
	)
	mevpb.RegisterBuilderRelayServer(srv, &BuilderRelayServer{sentry: sentry})

	// health.NewServer already reports SERVING for the empty (overall) probe;
	// register the named service too for probes configured with a service name.
	hs := health.NewServer()
	hs.SetServingStatus(mevpb.BuilderRelay_ServiceDesc.ServiceName, healthpb.HealthCheckResponse_SERVING)
	healthpb.RegisterHealthServer(srv, hs)

	go func() {
		log.Infow("grpc builder relay listening", "addr", addr)
		if err := srv.Serve(lis); err != nil {
			log.Errorw("grpc server stopped", "err", err)
		}
	}()
	return srv, nil
}
