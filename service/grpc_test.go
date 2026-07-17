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

// The gRPC path RLP-encodes on the builder and decodes on the sentry; the
// signing hash must survive the roundtrip or every signature breaks.
func TestBidBlockRLPRoundtripHash(t *testing.T) {
	original := sampleBidBlock()

	encoded, err := rlp.EncodeToBytes(original)
	require.NoError(t, err)

	var decoded buildertypes.BidBlock
	require.NoError(t, rlp.DecodeBytes(encoded, &decoded))

	require.Equal(t, original.Hash(), decoded.Hash())
	require.Equal(t, len(original.Transactions), len(decoded.Transactions))
	for i := range original.Transactions {
		require.Equal(t, original.Transactions[i], decoded.Transactions[i])
	}
}

// blobSidecar builds a sidecar with n blobs. Content is deterministic per
// index so roundtrip equality is meaningful (not all-zero).
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

// Blobs are the whole point of the optimization, so the sidecar RLP roundtrip
// and the JSON re-marshal (egress to validator) must both hold.
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

	// Step 1 forwards to the validator over JSON-RPC, so the RLP-decoded block
	// must still JSON-marshal cleanly for the egress path.
	_, err = json.Marshal(buildertypes.BidBlockArgs{BidBlock: &decoded})
	require.NoError(t, err)

	// A signature over the blob-carrying block must recover identically after
	// the roundtrip.
	key, err := crypto.GenerateKey()
	require.NoError(t, err)
	sig, err := crypto.Sign(original.Hash().Bytes(), key)
	require.NoError(t, err)
	recovered, err := (&buildertypes.BidBlockArgs{BidBlock: &decoded, Signature: sig}).EcrecoverSender()
	require.NoError(t, err)
	require.Equal(t, crypto.PubkeyToAddress(key.PublicKey), recovered)
}

// A builder signature made before RLP encoding must recover to the same
// address after the sentry decodes the payload.
func TestBidBlockSignatureSurvivesRLP(t *testing.T) {
	key, err := crypto.GenerateKey()
	require.NoError(t, err)
	builder := crypto.PubkeyToAddress(key.PublicKey)

	original := sampleBidBlock()
	sig, err := crypto.Sign(original.Hash().Bytes(), key)
	require.NoError(t, err)

	encoded, err := rlp.EncodeToBytes(original)
	require.NoError(t, err)
	var decoded buildertypes.BidBlock
	require.NoError(t, rlp.DecodeBytes(encoded, &decoded))

	args := buildertypes.BidBlockArgs{BidBlock: &decoded, Signature: sig}
	recovered, err := args.EcrecoverSender()
	require.NoError(t, err)
	require.Equal(t, builder, recovered)
}

// mockValidator captures the BidBlockArgs the sentry forwards, standing in for
// the real JSON-RPC egress.
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

// Both ingress paths must deliver equivalent BidBlockArgs to the validator:
// same signing hash, same builder, same tx bytes, same sidecars, same signature.
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

	// JSON path: args arrive as the already-decoded wrapper.
	jsonVal := &mockValidator{}
	_, err = newSentry(jsonVal).SendBidBlock(context.Background(), BidBlockArgsWrapper{
		BidBlockArgs:      buildertypes.BidBlockArgs{BidBlock: block, Signature: sig},
		ValidatorHostName: "val-1",
	})
	require.NoError(t, err)

	// gRPC path: args arrive as RLP bytes through the BuilderRelay handler.
	grpcVal := &mockValidator{}
	encoded, err := rlp.EncodeToBytes(block)
	require.NoError(t, err)
	resp, err := (&BuilderRelayServer{sentry: newSentry(grpcVal)}).SendBidBlock(context.Background(), &mevpb.BidBlockRequest{
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

// toGRPCStatus must give builders distinguishable codes per MEV error class.
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
	}

	// MEV business code must survive in status details.
	st, _ := status.FromError(toGRPCStatus(buildertypes.NewBidBlockTooLateError("x")))
	require.NotEmpty(t, st.Details())
	info, ok := st.Details()[0].(*errdetails.ErrorInfo)
	require.True(t, ok)
	require.Equal(t, strconv.Itoa(buildertypes.BidBlockTooLateError), info.Reason)
}

// Handler-level rejections before/after decode must come back as
// InvalidArgument, not Internal.
func TestGRPCHandlerErrorPaths(t *testing.T) {
	sentry := NewMevSentry(&Config{RPCTimeout: Duration(0)},
		map[string]node.Validator{}, map[common.Address]node.Builder{})
	h := &BuilderRelayServer{sentry: sentry}

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

// End-to-end over a real listener: health probes answer, the full interceptor
// chain runs, and graceful shutdown flips health to NOT_SERVING.
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

	// health: overall and named service both SERVING
	hc := healthpb.NewHealthClient(conn)
	for _, svcName := range []string{"", "mev.v1.BuilderRelay"} {
		resp, herr := hc.Check(ctx, &healthpb.HealthCheckRequest{Service: svcName})
		require.NoError(t, herr, "service=%q", svcName)
		require.Equal(t, healthpb.HealthCheckResponse_SERVING, resp.Status)
	}

	// business call through the real chain (recovery + concurrency interceptors)
	relay := mevpb.NewBuilderRelayClient(conn)
	_, err = relay.SendBidBlock(ctx, &mevpb.BidBlockRequest{
		BidBlockRlp: []byte{0xff}, Signature: []byte{0x01}, ValidatorHostName: "val-1",
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err))

	// Shutdown must flip health to NOT_SERVING BEFORE the server stops (so LBs
	// drain first). Watch observes the transition; a plain post-stop error
	// could not distinguish NOT_SERVING from the server just being gone.
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

// panicValidator triggers a handler panic once the request reaches forwarding.
type panicValidator struct{ mockValidator }

func (p *panicValidator) SendBidBlock(context.Context, buildertypes.BidBlockArgs, common.Address, common.Hash) (common.Hash, error) {
	panic("boom")
}

// A handler panic must surface as Internal while the server keeps serving.
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

	relay := mevpb.NewBuilderRelayClient(conn)
	req := &mevpb.BidBlockRequest{BidBlockRlp: encoded, Signature: sig, ValidatorHostName: "val-1"}
	_, err = relay.SendBidBlock(ctx, req)
	require.Equal(t, codes.Internal, status.Code(err))

	// process/server survived the panic: health answers, and a second call
	// still reaches the handler (panics again → Internal, not a dead conn).
	hc := healthpb.NewHealthClient(conn)
	resp, err := hc.Check(ctx, &healthpb.HealthCheckRequest{})
	require.NoError(t, err)
	require.Equal(t, healthpb.HealthCheckResponse_SERVING, resp.Status)
	_, err = relay.SendBidBlock(ctx, req)
	require.Equal(t, codes.Internal, status.Code(err))
}

// blockingValidator holds every forward until release is closed.
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

// With the semaphore full: a queued request must abort when its context ends,
// and health must keep answering (it bypasses the semaphore).
func TestGRPCConcurrencyLimitAndHealthBypass(t *testing.T) {
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
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	relay := mevpb.NewBuilderRelayClient(conn)
	req := &mevpb.BidBlockRequest{BidBlockRlp: encoded, Signature: sig, ValidatorHostName: "val-1"}

	// First call occupies the single semaphore slot and blocks in the validator.
	firstDone := make(chan error, 1)
	go func() {
		_, callErr := relay.SendBidBlock(ctx, req)
		firstDone <- callErr
	}()
	<-val.entered // semaphore held from here on

	// Second call queues on the semaphore; its deadline must free it.
	shortCtx, shortCancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer shortCancel()
	_, err = relay.SendBidBlock(shortCtx, req)
	require.Equal(t, codes.DeadlineExceeded, status.Code(err))

	// Health bypasses the semaphore and answers while the slot is held.
	hc := healthpb.NewHealthClient(conn)
	resp, err := hc.Check(ctx, &healthpb.HealthCheckRequest{})
	require.NoError(t, err)
	require.Equal(t, healthpb.HealthCheckResponse_SERVING, resp.Status)

	close(val.release)
	require.NoError(t, <-firstDone)
}

// The error metric must keep the MEV business code; converting to a gRPC
// status first would erase it (regression test for that exact bug).
func TestErrorCodeLabelKeepsBusinessCode(t *testing.T) {
	orig := buildertypes.ErrMevBusy
	final := toGRPCStatus(orig)
	require.Equal(t, strconv.Itoa(buildertypes.MevBusyError), errorCodeLabel(orig, final))

	invalid := buildertypes.NewInvalidBidError("invalid BidBlock rlp")
	require.Equal(t, strconv.Itoa(buildertypes.InvalidBidParamError), errorCodeLabel(invalid, toGRPCStatus(invalid)))

	plain := errors.New("plain failure")
	require.Equal(t, codes.Internal.String(), errorCodeLabel(plain, toGRPCStatus(plain)))
}

// Internal statuses must not leak raw internal error text to callers.
func TestInternalErrorsAreOpaque(t *testing.T) {
	st, ok := status.FromError(toGRPCStatus(errors.New("dial tcp 10.0.0.1:8545: connection refused")))
	require.True(t, ok)
	require.Equal(t, codes.Internal, st.Code())
	require.Equal(t, "internal error", st.Message())
}

func TestGRPCRejectsOversizedRequest(t *testing.T) {
	sentry := NewMevSentry(&Config{RPCTimeout: Duration(0)},
		map[string]node.Validator{}, map[common.Address]node.Builder{})
	svc, err := StartGRPCServer("127.0.0.1:0", sentry, make(chan struct{}, 1))
	require.NoError(t, err)
	defer svc.Shutdown(time.Second)

	conn, err := grpc.NewClient(svc.Addr(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = mevpb.NewBuilderRelayClient(conn).SendBidBlock(ctx, &mevpb.BidBlockRequest{
		BidBlockRlp:       make([]byte, maxGRPCMsgSize+1),
		ValidatorHostName: "val-1",
	})
	require.Equal(t, codes.ResourceExhausted, status.Code(err))
}

func TestGRPCShutdownDrainsInFlightBid(t *testing.T) {
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

	conn, err := grpc.NewClient(svc.Addr(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	callDone := make(chan error, 1)
	go func() {
		_, callErr := mevpb.NewBuilderRelayClient(conn).SendBidBlock(ctx, &mevpb.BidBlockRequest{
			BidBlockRlp: encoded, Signature: sig, ValidatorHostName: "val-1",
		})
		callDone <- callErr
	}()
	<-val.entered

	shutdownDone := make(chan struct{})
	go func() {
		svc.Shutdown(2 * time.Second)
		close(shutdownDone)
	}()
	select {
	case <-shutdownDone:
		t.Fatal("shutdown returned before the in-flight bid completed")
	case <-time.After(50 * time.Millisecond):
	}

	close(val.release)
	require.NoError(t, <-callDone)
	select {
	case <-shutdownDone:
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown did not finish after the in-flight bid completed")
	}
}
