package service

import (
	"context"
	"errors"
	"fmt"
	"net"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
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

// maxGRPCMsgSize bounds inbound BidBlock payloads. A 6-blob BidBlock is ~1MB
// of raw bytes; a future 16-blob block would be ~2.5MB. 8MB = protocol maximum
// plus generous headroom, while capping how much a single request can make the
// transport buffer. NOTE: this and MaxConcurrentStreams are the only limits
// that act BEFORE protobuf decode — the interceptor semaphore runs after the
// message is already in memory, so rate/connection limits at the LB/firewall
// layer remain necessary for full ingress protection.
const maxGRPCMsgSize = 8 * 1024 * 1024

// maxGRPCConcurrentStreams caps pre-interceptor request decoding per HTTP/2
// connection. The business semaphore remains the process-wide limit; this
// transport-level cap is defense in depth because interceptors run only after
// protobuf has already materialized the request.
const maxGRPCConcurrentStreams = 32

// relayMethodPrefix scopes the concurrency limit to business RPCs; health and
// reflection must never queue behind bid traffic or LB probes would time out
// exactly when the process is overloaded, causing cascading removal.
const relayMethodPrefix = "/mev.v1.BuilderRelay/"

// grpcSendBidBlockMetric is the metric method label for SendBidBlock — shared
// by the handler and the recovery interceptor so panics and ordinary errors
// land under one label.
const grpcSendBidBlockMetric = "grpc_mev_sendBidBlock"

// grpcMethodLabel maps a gRPC full method name to its metric label.
func grpcMethodLabel(fullMethod string) string {
	if fullMethod == mevpb.BuilderRelay_SendBidBlock_FullMethodName {
		return grpcSendBidBlockMetric
	}
	return fullMethod
}

// BuilderRelayServer serves BEP-675 BidBlock traffic over gRPC. The RLP
// payload keeps blobs as raw bytes on ingress, skipping the hex inflation and
// JSON parse cost before the current JSON-RPC validator forwarding step.
type BuilderRelayServer struct {
	mevpb.UnimplementedBuilderRelayServer
	sentry *MevSentry
}

// SendBidBlock decodes the RLP BidBlock and hands it to the same core logic
// as the JSON-RPC handler (ecrecover, allowlist, routing, forwarding).
func (b *BuilderRelayServer) SendBidBlock(ctx context.Context, req *mevpb.BidBlockRequest) (resp *mevpb.BidBlockResponse, err error) {
	method := grpcSendBidBlockMetric
	start := time.Now()
	defer recordLatency(method, start)
	defer timeoutCancel(&ctx, b.sentry.timeout)()
	// The body returns raw business errors; this defer counts each failure
	// (keyed by the MEV business code when present, else the final gRPC code)
	// BEFORE converting to a gRPC status — converting first would erase the
	// business code from the metric.
	defer func() {
		if err != nil {
			orig := err
			final := toGRPCStatus(orig)
			if status.Code(final) == codes.Internal {
				log.Errorw("grpc send bid block failed", "err", orig)
			}
			err = final
			metrics.ApiErrorCounter.WithLabelValues(method, errorCodeLabel(orig, final)).Inc()
		}
	}()

	// Routing needs an explicit target: gRPC has no HTTP Host fallback.
	// Validate before paying for the RLP decode.
	host := strings.TrimSpace(req.ValidatorHostName)
	if host == "" {
		return nil, buildertypes.NewInvalidBidError("validator_host_name is required")
	}

	var bidBlock buildertypes.BidBlock
	decodeStart := time.Now()
	if err := rlp.DecodeBytes(req.BidBlockRlp, &bidBlock); err != nil {
		log.Errorw("failed to decode bid block rlp", "err", err)
		return nil, buildertypes.NewInvalidBidError("invalid BidBlock rlp")
	}
	decodeElapsed := time.Since(decodeStart)

	args := BidBlockArgsWrapper{
		BidBlockArgs: buildertypes.BidBlockArgs{
			BidBlock:  &bidBlock,
			Signature: req.Signature,
		},
		ValidatorHostName: host,
	}

	bidHash, err := b.sentry.sendBidBlock(ctx, args)
	if err != nil {
		return nil, err // raw business error; the defer above converts + counts
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

// toGRPCStatus maps sentry/validator errors onto gRPC codes so builders can
// tell retryable congestion from permanent rejection. The original MEV
// business code is preserved in status details (ErrorInfo).
func toGRPCStatus(err error) error {
	if s, ok := status.FromError(err); ok && s.Code() != codes.Unknown {
		return err // already a grpc status (e.g. from validation above)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return status.Error(codes.DeadlineExceeded, err.Error())
	}
	if errors.Is(err, context.Canceled) {
		return status.Error(codes.Canceled, err.Error())
	}

	var rpcErr rpc.Error
	if !errors.As(err, &rpcErr) {
		// Unknown internal failure: don't leak validator RPC text or internal
		// details to the caller; the original error is already logged.
		return status.Error(codes.Internal, "internal error")
	}

	var code codes.Code
	switch rpcErr.ErrorCode() {
	case buildertypes.InvalidBidParamError, buildertypes.InvalidPayBidTxError,
		buildertypes.BidBlockPreSealVerifyError:
		code = codes.InvalidArgument
	case buildertypes.MevNotRunningError:
		code = codes.Unavailable
	case buildertypes.MevBusyError:
		code = codes.ResourceExhausted
	case buildertypes.MevNotInTurnError:
		code = codes.FailedPrecondition
	case buildertypes.BidBlockPermissionRevokedError:
		code = codes.PermissionDenied
	case buildertypes.BidBlockTooLateError:
		code = codes.DeadlineExceeded
	default:
		// rpc.Error with a code outside the MEV set: same non-disclosure rule.
		return status.Error(codes.Internal, "internal error")
	}

	// Business rejections carry safe, actionable messages — keep them.
	st := status.New(code, err.Error())
	if detailed, derr := st.WithDetails(&errdetails.ErrorInfo{
		Reason: strconv.Itoa(rpcErr.ErrorCode()),
		Domain: "mev.bnbchain.org",
	}); derr == nil {
		st = detailed
	}
	return st.Err()
}

// errorCodeLabel picks the metric label for a failure: the MEV business code
// from the ORIGINAL error when present (conversion to a gRPC status erases
// it), else the final gRPC code string.
func errorCodeLabel(orig, final error) string {
	var rpcErr rpc.Error
	if errors.As(orig, &rpcErr) {
		return strconv.Itoa(rpcErr.ErrorCode())
	}
	return status.Code(final).String()
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
			metrics.ApiErrorCounter.WithLabelValues(grpcMethodLabel(info.FullMethod), "panic").Inc()
			err = status.Error(codes.Internal, "internal error")
		}
	}()
	return handler(ctx, req)
}

// concurrencyInterceptor bounds in-flight BuilderRelay requests with a
// process-wide semaphore shared with the JSON path's gin ConcurrencyLimiter,
// so the two listeners together respect one RPCConcurrency budget. Health and
// other non-business RPCs bypass the semaphore. Acquisition waits in line
// (matching the gin middleware) but aborts if the caller's context ends.
// Per-connection MaxConcurrentStreams is NOT sufficient here: a client can
// open more connections to bypass it.
func concurrencyInterceptor(sem chan struct{}) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler) (any, error) {
		if sem != nil && strings.HasPrefix(info.FullMethod, relayMethodPrefix) {
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				err := status.FromContextError(ctx.Err()).Err()
				metrics.ApiErrorCounter.WithLabelValues(grpcMethodLabel(info.FullMethod), status.Code(err).String()).Inc()
				return nil, err
			}
		}
		return handler(ctx, req)
	}
}

// GRPCService owns the BuilderRelay server lifecycle.
type GRPCService struct {
	srv    *grpc.Server
	health *health.Server
	addr   string
}

// Addr returns the bound listen address (useful with ":0" in tests).
func (g *GRPCService) Addr() string { return g.addr }

// Shutdown flips health to NOT_SERVING so LBs drain first, then waits up to
// timeout for in-flight RPCs before forcing the server down.
func (g *GRPCService) Shutdown(timeout time.Duration) {
	g.health.Shutdown()
	done := make(chan struct{})
	go func() {
		g.srv.GracefulStop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		g.srv.Stop()
	}
}

// StartGRPCServer starts the BuilderRelay gRPC endpoint on addr. It runs
// alongside the JSON-RPC listener; both feed the same MevSentry core. sem is
// the shared concurrency semaphore (nil = unlimited) — pass the same channel
// the gin middleware uses so total process concurrency stays bounded.
func StartGRPCServer(addr string, sentry *MevSentry, sem chan struct{}) (*GRPCService, error) {
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("grpc listen on %s: %w", addr, err)
	}

	opts := []grpc.ServerOption{
		grpc.MaxRecvMsgSize(maxGRPCMsgSize),
		grpc.MaxSendMsgSize(maxGRPCMsgSize),
		grpc.MaxConcurrentStreams(maxGRPCConcurrentStreams),
		// recovery outermost so it also covers the other interceptors.
		grpc.ChainUnaryInterceptor(recoveryInterceptor, concurrencyInterceptor(sem)),
	}
	srv := grpc.NewServer(opts...)
	mevpb.RegisterBuilderRelayServer(srv, &BuilderRelayServer{sentry: sentry})

	// health.NewServer already reports SERVING for the empty (overall) probe;
	// register the named service too for probes configured with a service name.
	hs := health.NewServer()
	hs.SetServingStatus(mevpb.BuilderRelay_ServiceDesc.ServiceName, healthpb.HealthCheckResponse_SERVING)
	healthpb.RegisterHealthServer(srv, hs)

	g := &GRPCService{srv: srv, health: hs, addr: lis.Addr().String()}
	go func() {
		log.Infow("grpc builder relay listening", "addr", g.addr)
		if err := srv.Serve(lis); err != nil {
			// Unexpected listener death: mark NOT_SERVING so probes fail fast;
			// the JSON-RPC path keeps the process alive.
			hs.Shutdown()
			log.Errorw("grpc server stopped", "err", err)
		}
	}()
	return g, nil
}
