package service

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"strconv"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	buildertypes "github.com/ethereum/go-ethereum/core/types/builder"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/crypto/kzg4844"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/stretchr/testify/require"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/tap"

	"github.com/bnb-chain/bsc-mev-sentry/node"
	mevpb "github.com/bnb-chain/bsc-mev-sentry/proto"
)

func sampleBidBlock() *buildertypes.BidBlock {
	return &buildertypes.BidBlock{
		Header: &types.Header{
			ParentHash: common.HexToHash("0x01"),
			Coinbase:   common.HexToAddress("0x02"),
			Root:       common.HexToHash("0x03"),
			TxHash:     common.HexToHash("0x04"),
			Number:     big.NewInt(10779180),
			GasLimit:   140_000_000,
			GasUsed:    12_345_678,
			Time:       1_780_000_000,
			Extra:      []byte("test"),
			BaseFee:    big.NewInt(1),
		},
		Transactions: []hexutil.Bytes{{0x01, 0x02}, {0x03, 0x04, 0x05}},
	}
}

// blobSidecar returns deterministic test data.
func blobSidecar(n int) *types.BlobSidecar {
	sc := &types.BlobSidecar{
		BlockNumber: big.NewInt(10779180),
		BlockHash:   common.HexToHash("0xbb"),
		TxIndex:     3,
		TxHash:      common.HexToHash("0xcc"),
	}
	for i := 0; i < n; i++ {
		var blob kzg4844.Blob
		var commit kzg4844.Commitment
		var proof kzg4844.Proof
		blob[0], blob[len(blob)-1] = byte(i+1), byte(i+2)
		commit[0], proof[0] = byte(i+3), byte(i+4)
		sc.Blobs = append(sc.Blobs, blob)
		sc.Commitments = append(sc.Commitments, commit)
		sc.Proofs = append(sc.Proofs, proof)
	}
	return sc
}

// Blob sidecars must survive both transport encodings.
func TestBidBlockSidecarRLPRoundtrip(t *testing.T) {
	original := sampleBidBlock()
	original.Sidecars = types.BlobSidecars{blobSidecar(4), blobSidecar(2)}

	encoded, err := rlp.EncodeToBytes(original)
	require.NoError(t, err)

	var decoded buildertypes.BidBlock
	require.NoError(t, rlp.DecodeBytes(encoded, &decoded))

	require.Equal(t, original.Hash(), decoded.Hash())
	require.Len(t, decoded.Sidecars, 2)
	require.Len(t, decoded.Sidecars[0].Blobs, 4)
	require.Len(t, decoded.Sidecars[1].Blobs, 2)
	for i := range original.Sidecars {
		require.Equal(t, original.Sidecars[i].Blobs, decoded.Sidecars[i].Blobs)
		require.Equal(t, original.Sidecars[i].Commitments, decoded.Sidecars[i].Commitments)
		require.Equal(t, original.Sidecars[i].Proofs, decoded.Sidecars[i].Proofs)
	}

	// The decoded block must remain valid JSON-RPC output.
	_, err = json.Marshal(buildertypes.BidBlockArgs{BidBlock: &decoded})
	require.NoError(t, err)

	// The signature must recover after the roundtrip.
	key, err := crypto.GenerateKey()
	require.NoError(t, err)
	sig, err := crypto.Sign(original.Hash().Bytes(), key)
	require.NoError(t, err)
	recovered, err := (&buildertypes.BidBlockArgs{BidBlock: &decoded, Signature: sig}).EcrecoverSender()
	require.NoError(t, err)
	require.Equal(t, crypto.PubkeyToAddress(key.PublicKey), recovered)
}

// mockValidator captures forwarded BidBlock arguments.
type mockValidator struct {
	gotArgs    buildertypes.BidBlockArgs
	gotBuilder common.Address
	gotHash    common.Hash
}

func (m *mockValidator) SendBidBlock(_ context.Context, args buildertypes.BidBlockArgs, builder common.Address, bidHash common.Hash) (common.Hash, error) {
	m.gotArgs, m.gotBuilder, m.gotHash = args, builder, bidHash
	return bidHash, nil
}
func (m *mockValidator) SendBid(context.Context, buildertypes.BidArgs, common.Address) (common.Hash, error) {
	return common.Hash{}, nil
}
func (m *mockValidator) GetBidBlockPermission(context.Context, common.Address) (*ethclient.BidBlockPermission, error) {
	return nil, nil
}
func (m *mockValidator) MevRunning() bool { return true }
func (m *mockValidator) HasBuilder(context.Context, common.Address) (bool, error) {
	return true, nil
}
func (m *mockValidator) BestBidGasFee(context.Context, common.Hash) (*big.Int, error) {
	return nil, nil
}
func (m *mockValidator) MevParams(context.Context) (*buildertypes.MevParams, error) {
	return nil, nil
}
func (m *mockValidator) BuilderFeeCeil() *big.Int { return nil }
func (m *mockValidator) GeneratePayBidTx(context.Context, buildertypes.BidArgs, common.Address, *big.Int) (hexutil.Bytes, error) {
	return nil, nil
}

// Both ingress paths must deliver equivalent BidBlock arguments.
func TestGRPCIngressMatchesJSONPath(t *testing.T) {
	key, err := crypto.GenerateKey()
	require.NoError(t, err)
	builderAddr := crypto.PubkeyToAddress(key.PublicKey)

	block := sampleBidBlock()
	block.Sidecars = types.BlobSidecars{blobSidecar(2)}
	sig, err := crypto.Sign(block.Hash().Bytes(), key)
	require.NoError(t, err)

	newSentry := func(v node.Validator) *MevSentry {
		return NewMevSentry(&Config{RPCTimeout: Duration(0)},
			map[string]node.Validator{"val-1": v},
			map[common.Address]node.Builder{builderAddr: nil})
	}

	// JSON uses an already-decoded wrapper.
	jsonVal := &mockValidator{}
	_, err = newSentry(jsonVal).SendBidBlock(context.Background(), BidBlockArgsWrapper{
		BidBlockArgs:      buildertypes.BidBlockArgs{BidBlock: block, Signature: sig},
		ValidatorHostName: "val-1",
	})
	require.NoError(t, err)

	// gRPC uses RLP bytes.
	grpcVal := &mockValidator{}
	encoded, err := rlp.EncodeToBytes(block)
	require.NoError(t, err)
	resp, err := (&BidBlockServer{sentry: newSentry(grpcVal)}).SendBidBlock(context.Background(), &mevpb.BidBlockRequest{
		BidBlockRlp:       encoded,
		Signature:         sig,
		ValidatorHostName: "val-1",
	})
	require.NoError(t, err)

	require.Equal(t, jsonVal.gotHash, grpcVal.gotHash)
	require.Equal(t, common.BytesToHash(resp.BidHash), jsonVal.gotHash)
	require.Equal(t, jsonVal.gotBuilder, grpcVal.gotBuilder)
	require.Equal(t, builderAddr, grpcVal.gotBuilder)
	require.Equal(t, jsonVal.gotArgs.Signature, grpcVal.gotArgs.Signature)
	require.Equal(t, jsonVal.gotArgs.BidBlock.Transactions, grpcVal.gotArgs.BidBlock.Transactions)
	require.Equal(t, jsonVal.gotArgs.BidBlock.Sidecars, grpcVal.gotArgs.BidBlock.Sidecars)
	require.Equal(t, jsonVal.gotArgs.BidBlock.Header.Hash(), grpcVal.gotArgs.BidBlock.Header.Hash())
}

// MEV error classes must remain distinguishable.
func TestToGRPCStatusMapping(t *testing.T) {
	cases := []struct {
		err  error
		want codes.Code
	}{
		{buildertypes.NewInvalidBidError("bad sig"), codes.InvalidArgument},
		{buildertypes.NewBidBlockPreSealVerifyError("verify"), codes.InvalidArgument},
		{buildertypes.ErrMevNotRunning, codes.Unavailable},
		{buildertypes.ErrMevBusy, codes.ResourceExhausted},
		{buildertypes.ErrMevNotInTurn, codes.FailedPrecondition},
		{buildertypes.NewBidBlockPermissionRevokedError("revoked"), codes.PermissionDenied},
		{buildertypes.NewBidBlockTooLateError("too late"), codes.DeadlineExceeded},
		{context.DeadlineExceeded, codes.DeadlineExceeded},
		{context.Canceled, codes.Canceled},
		{errors.New("plain"), codes.Internal},
	}
	for _, c := range cases {
		st, ok := status.FromError(toGRPCStatus(c.err))
		require.True(t, ok)
		require.Equal(t, c.want, st.Code(), "err=%v", c.err)
		if c.want == codes.Internal {
			require.Equal(t, "internal error", st.Message())
		}
	}

	// MEV business code must survive in status details.
	st, _ := status.FromError(toGRPCStatus(buildertypes.NewBidBlockTooLateError("x")))
	require.NotEmpty(t, st.Details())
	info, ok := st.Details()[0].(*errdetails.ErrorInfo)
	require.True(t, ok)
	require.Equal(t, strconv.Itoa(buildertypes.BidBlockTooLateError), info.Reason)
	require.Equal(t, strconv.Itoa(buildertypes.BidBlockTooLateError),
		errorCodeLabel(buildertypes.NewBidBlockTooLateError("x"), toGRPCStatus(buildertypes.NewBidBlockTooLateError("x"))))
	require.Equal(t, codes.Internal.String(),
		errorCodeLabel(errors.New("plain failure"), toGRPCStatus(errors.New("plain failure"))))
}

// Input errors must return InvalidArgument.
func TestGRPCHandlerErrorPaths(t *testing.T) {
	sentry := NewMevSentry(&Config{RPCTimeout: Duration(0)},
		map[string]node.Validator{}, map[common.Address]node.Builder{})
	h := &BidBlockServer{sentry: sentry}

	// empty validator_host_name
	_, err := h.SendBidBlock(context.Background(), &mevpb.BidBlockRequest{
		BidBlockRlp: []byte{0x01}, Signature: []byte{0x02},
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err))

	// invalid RLP
	_, err = h.SendBidBlock(context.Background(), &mevpb.BidBlockRequest{
		BidBlockRlp: []byte{0xff, 0xff}, Signature: []byte{0x02}, ValidatorHostName: "val-1",
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Equal(t, "invalid BidBlock rlp", status.Convert(err).Message())

	// well-formed block signed by an unregistered builder
	key, err2 := crypto.GenerateKey()
	require.NoError(t, err2)
	block := sampleBidBlock()
	sig, err2 := crypto.Sign(block.Hash().Bytes(), key)
	require.NoError(t, err2)
	encoded, err2 := rlp.EncodeToBytes(block)
	require.NoError(t, err2)
	_, err = h.SendBidBlock(context.Background(), &mevpb.BidBlockRequest{
		BidBlockRlp: encoded, Signature: sig, ValidatorHostName: "val-1",
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}

// Exercise health, interceptors, and shutdown on a real listener.
func TestStartGRPCServerEndToEnd(t *testing.T) {
	sentry := NewMevSentry(&Config{RPCTimeout: Duration(0)},
		map[string]node.Validator{}, map[common.Address]node.Builder{})
	svc, err := StartGRPCServer("127.0.0.1:0", sentry, make(chan struct{}, 2))
	require.NoError(t, err)

	conn, err := grpc.NewClient(svc.Addr(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Check overall and named health.
	hc := healthpb.NewHealthClient(conn)
	for _, svcName := range []string{"", "mev.v1.BidBlockService"} {
		resp, herr := hc.Check(ctx, &healthpb.HealthCheckRequest{Service: svcName})
		require.NoError(t, herr, "service=%q", svcName)
		require.Equal(t, healthpb.HealthCheckResponse_SERVING, resp.Status)
	}

	// Exercise the interceptor chain.
	client := mevpb.NewBidBlockServiceClient(conn)
	_, err = client.SendBidBlock(ctx, &mevpb.BidBlockRequest{
		BidBlockRlp: []byte{0xff}, Signature: []byte{0x01}, ValidatorHostName: "val-1",
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err))

	_, err = client.SendBidBlock(ctx, &mevpb.BidBlockRequest{
		BidBlockRlp: make([]byte, maxGRPCMsgSize+1), ValidatorHostName: "val-1",
	})
	require.Equal(t, codes.ResourceExhausted, status.Code(err))

	// Observe NOT_SERVING before the connection closes.
	watchCtx, watchCancel := context.WithCancel(ctx)
	defer watchCancel()
	w, err := hc.Watch(watchCtx, &healthpb.HealthCheckRequest{Service: ""})
	require.NoError(t, err)
	first, err := w.Recv()
	require.NoError(t, err)
	require.Equal(t, healthpb.HealthCheckResponse_SERVING, first.Status)

	shutdownDone := make(chan struct{})
	go func() {
		svc.Shutdown(2 * time.Second)
		close(shutdownDone)
	}()
	second, err := w.Recv()
	require.NoError(t, err)
	require.Equal(t, healthpb.HealthCheckResponse_NOT_SERVING, second.Status)
	watchCancel() // release the stream so GracefulStop can finish
	<-shutdownDone
}

// panicValidator panics during forwarding.
type panicValidator struct{ mockValidator }

func (p *panicValidator) SendBidBlock(context.Context, buildertypes.BidBlockArgs, common.Address, common.Hash) (common.Hash, error) {
	panic("boom")
}

// A panic must return Internal without stopping the server.
func TestGRPCRecoveryKeepsServing(t *testing.T) {
	key, err := crypto.GenerateKey()
	require.NoError(t, err)
	builderAddr := crypto.PubkeyToAddress(key.PublicKey)
	block := sampleBidBlock()
	sig, err := crypto.Sign(block.Hash().Bytes(), key)
	require.NoError(t, err)
	encoded, err := rlp.EncodeToBytes(block)
	require.NoError(t, err)

	sentry := NewMevSentry(&Config{RPCTimeout: Duration(0)},
		map[string]node.Validator{"val-1": &panicValidator{}},
		map[common.Address]node.Builder{builderAddr: nil})
	svc, err := StartGRPCServer("127.0.0.1:0", sentry, nil)
	require.NoError(t, err)
	defer svc.Shutdown(time.Second)

	conn, err := grpc.NewClient(svc.Addr(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	client := mevpb.NewBidBlockServiceClient(conn)
	req := &mevpb.BidBlockRequest{BidBlockRlp: encoded, Signature: sig, ValidatorHostName: "val-1"}
	_, err = client.SendBidBlock(ctx, req)
	require.Equal(t, codes.Internal, status.Code(err))

	// Health and later calls must still work.
	hc := healthpb.NewHealthClient(conn)
	resp, err := hc.Check(ctx, &healthpb.HealthCheckRequest{})
	require.NoError(t, err)
	require.Equal(t, healthpb.HealthCheckResponse_SERVING, resp.Status)
	_, err = client.SendBidBlock(ctx, req)
	require.Equal(t, codes.Internal, status.Code(err))
}

// blockingValidator waits for release before returning.
type blockingValidator struct {
	mockValidator
	entered chan struct{}
	release chan struct{}
}

func (b *blockingValidator) SendBidBlock(_ context.Context, _ buildertypes.BidBlockArgs, _ common.Address, bidHash common.Hash) (common.Hash, error) {
	b.entered <- struct{}{}
	<-b.release
	return bidHash, nil
}

// A full global limit rejects large bids across connections before decoding.
func TestGRPCConcurrencyLimitAcrossConnections(t *testing.T) {
	key, err := crypto.GenerateKey()
	require.NoError(t, err)
	builderAddr := crypto.PubkeyToAddress(key.PublicKey)
	block := sampleBidBlock()
	sig, err := crypto.Sign(block.Hash().Bytes(), key)
	require.NoError(t, err)
	encoded, err := rlp.EncodeToBytes(block)
	require.NoError(t, err)

	val := &blockingValidator{entered: make(chan struct{}, 1), release: make(chan struct{})}
	sentry := NewMevSentry(&Config{RPCTimeout: Duration(0)},
		map[string]node.Validator{"val-1": val},
		map[common.Address]node.Builder{builderAddr: nil})
	svc, err := StartGRPCServer("127.0.0.1:0", sentry, make(chan struct{}, 1))
	require.NoError(t, err)
	defer svc.Shutdown(time.Second)

	conn, err := grpc.NewClient(svc.Addr(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	defer conn.Close()
	secondConn, err := grpc.NewClient(svc.Addr(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	defer secondConn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client := mevpb.NewBidBlockServiceClient(conn)
	secondClient := mevpb.NewBidBlockServiceClient(secondConn)
	req := &mevpb.BidBlockRequest{BidBlockRlp: encoded, Signature: sig, ValidatorHostName: "val-1"}

	// Hold the only slot.
	firstDone := make(chan error, 1)
	go func() {
		_, callErr := client.SendBidBlock(ctx, req)
		firstDone <- callErr
	}()
	<-val.entered // semaphore held from here on

	// Reject a near-limit request on another connection without decoding or queueing.
	rejectStart := time.Now()
	_, err = secondClient.SendBidBlock(ctx, &mevpb.BidBlockRequest{
		BidBlockRlp: make([]byte, maxGRPCMsgSize-1024), ValidatorHostName: "val-1",
	})
	require.Equal(t, codes.ResourceExhausted, status.Code(err))
	require.Less(t, time.Since(rejectStart), 2*time.Second)

	// Health bypasses the limit.
	hc := healthpb.NewHealthClient(secondConn)
	resp, err := hc.Check(ctx, &healthpb.HealthCheckRequest{})
	require.NoError(t, err)
	require.Equal(t, healthpb.HealthCheckResponse_SERVING, resp.Status)

	close(val.release)
	require.NoError(t, <-firstDone)
}

// A full limit must reject without queueing.
func TestBidBlockAdmission(t *testing.T) {
	require.Equal(t, uint32(32), maxGRPCStreams(defaultGRPCConcurrency))
	require.Equal(t, uint32(80), maxGRPCStreams(64))

	sem := make(chan struct{}, 1)
	sem <- struct{}{} // slot held

	limit := admitBidBlock(nil, sem, 0)
	start := time.Now()
	_, err := limit(context.Background(), &tap.Info{
		FullMethodName: mevpb.BidBlockService_SendBidBlock_FullMethodName,
	})
	require.Equal(t, codes.ResourceExhausted, status.Code(err))
	require.Less(t, time.Since(start), 100*time.Millisecond, "must not queue")

	// Health and other methods keep working with the slot held.
	_, err = limit(context.Background(), &tap.Info{
		FullMethodName: "/grpc.health.v1.Health/Check",
	})
	require.NoError(t, err)

	// The gRPC-specific cap sheds on its own, even with the shared budget free.
	grpcSem := make(chan struct{}, 1)
	grpcSem <- struct{}{}
	limit = admitBidBlock(grpcSem, make(chan struct{}, 100), 0)
	_, err = limit(context.Background(), &tap.Info{
		FullMethodName: mevpb.BidBlockService_SendBidBlock_FullMethodName,
	})
	require.Equal(t, codes.ResourceExhausted, status.Code(err))

	// A rejected shared-budget acquisition must not leak the gRPC slot.
	freeGRPC := make(chan struct{}, 1)
	fullShared := make(chan struct{}, 1)
	fullShared <- struct{}{}
	limit = admitBidBlock(freeGRPC, fullShared, 0)
	_, err = limit(context.Background(), &tap.Info{
		FullMethodName: mevpb.BidBlockService_SendBidBlock_FullMethodName,
	})
	require.Equal(t, codes.ResourceExhausted, status.Code(err))
	require.Len(t, freeGRPC, 0, "gRPC slot must be released when the shared budget rejects")

	// Accepted streams hold both slots until their stream context is closed.
	freeShared := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	limit = admitBidBlock(freeGRPC, freeShared, 0)
	admittedCtx, err := limit(ctx, &tap.Info{
		FullMethodName: mevpb.BidBlockService_SendBidBlock_FullMethodName,
	})
	require.NoError(t, err)
	deadline, hasDeadline := admittedCtx.Deadline()
	require.True(t, hasDeadline)
	require.WithinDuration(t, time.Now().Add(defaultGRPCRequestTimeout), deadline, time.Second)
	require.Len(t, freeGRPC, 1)
	require.Len(t, freeShared, 1)
	cancel()
	require.Eventually(t, func() bool {
		return len(freeGRPC) == 0 && len(freeShared) == 0
	}, time.Second, time.Millisecond)

	// A server deadline releases slots even if the client sets no deadline.
	timedGRPC := make(chan struct{}, 1)
	timedShared := make(chan struct{}, 1)
	limit = admitBidBlock(timedGRPC, timedShared, Duration(20*time.Millisecond))
	timedCtx, err := limit(context.Background(), &tap.Info{
		FullMethodName: mevpb.BidBlockService_SendBidBlock_FullMethodName,
	})
	require.NoError(t, err)
	select {
	case <-timedCtx.Done():
		require.ErrorIs(t, timedCtx.Err(), context.DeadlineExceeded)
	case <-time.After(time.Second):
		t.Fatal("server deadline did not cancel the stream")
	}
	require.Eventually(t, func() bool {
		return len(timedGRPC) == 0 && len(timedShared) == 0
	}, time.Second, time.Millisecond)

	healthCtx, err := limit(context.Background(), &tap.Info{
		FullMethodName: "/grpc.health.v1.Health/Check",
	})
	require.NoError(t, err)
	_, hasDeadline = healthCtx.Deadline()
	require.False(t, hasDeadline, "health must bypass the BidBlock timeout")
}
