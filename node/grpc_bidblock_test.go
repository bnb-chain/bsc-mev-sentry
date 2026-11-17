package node

import (
	"context"
	"errors"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/ethereum/go-ethereum/common"
	buildertypes "github.com/ethereum/go-ethereum/core/types/builder"
	"github.com/ethereum/go-ethereum/core/types/builder/mevpb"
	"github.com/ethereum/go-ethereum/rpc"
)

type fakeValidatorGRPC struct {
	mevpb.UnimplementedBidBlockServiceServer
	got *mevpb.BidBlockRequest
	err error
}

func (f *fakeValidatorGRPC) SendBidBlock(_ context.Context, req *mevpb.BidBlockRequest) (*mevpb.BidBlockResponse, error) {
	f.got = req
	return &mevpb.BidBlockResponse{BidHash: common.HexToHash("0xabc").Bytes()}, f.err
}

func TestSendBidBlockRLP(t *testing.T) {
	fake := &fakeValidatorGRPC{}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv := grpc.NewServer()
	mevpb.RegisterBidBlockServiceServer(srv, fake)
	go func() { _ = srv.Serve(lis) }()
	defer srv.Stop()
	c, err := newBidBlockGRPCClient(lis.Addr().String())
	require.NoError(t, err)
	defer c.conn.Close()
	v := &validator{grpc: c}

	hash, err := v.SendBidBlockRLP(context.Background(), []byte{0xc0, 1}, []byte{9}, common.Address{}, common.Hash{})
	require.NoError(t, err)
	require.Equal(t, common.HexToHash("0xabc"), hash)
	require.Equal(t, []byte{0xc0, 1}, fake.got.BidBlockRlp)

	st, _ := status.New(codes.DeadlineExceeded, "too late").WithDetails(&errdetails.ErrorInfo{
		Reason: strconv.Itoa(buildertypes.BidBlockTooLateError), Domain: mevErrorDomain})
	fake.err = st.Err()
	_, err = v.SendBidBlockRLP(context.Background(), []byte{0xc0}, nil, common.Address{}, common.Hash{})
	var rpcErr rpc.Error
	require.ErrorAs(t, err, &rpcErr)
	require.Equal(t, buildertypes.BidBlockTooLateError, rpcErr.ErrorCode())

	_, err = (&validator{}).SendBidBlockRLP(context.Background(), nil, nil, common.Address{}, common.Hash{})
	require.ErrorIs(t, err, ErrGRPCNotConfigured)
}

func TestBidBlockQuota(t *testing.T) {
	v := &validator{}
	hash := common.HexToHash("0x01")
	for range 2 {
		finish, err := v.ReserveBidBlock(100, common.Address{}, hash)
		require.NoError(t, err) // no local limit before the first parameter fetch
		finish(nil)
	}
	v.mevParams.Store(&buildertypes.MevParams{MaxBidsPerBuilder: 1})
	finish, err := v.ReserveBidBlock(100, common.Address{}, hash)
	require.NoError(t, err)
	_, err = v.ReserveBidBlock(100, common.Address{}, hash)
	require.ErrorContains(t, err, "bid already exists") // still in-flight

	finish(context.DeadlineExceeded)
	finish, err = v.ReserveBidBlock(100, common.Address{}, hash)
	require.NoError(t, err) // same hash can retry after an ambiguous failure
	finish(context.DeadlineExceeded)
	_, err = v.ReserveBidBlock(100, common.Address{}, hash)
	require.ErrorContains(t, err, "bid already exists") // at most one retry
	_, err = v.ReserveBidBlock(100, common.Address{}, common.HexToHash("0x02"))
	require.ErrorContains(t, err, "too many bids")
	v.mevParams.Store(&buildertypes.MevParams{MaxBidsPerBuilder: 2})
	finish, err = v.ReserveBidBlock(100, common.Address{}, common.HexToHash("0x02"))
	require.NoError(t, err) // follow refreshed validator parameters
	finish(bidBlockQuotaError("bid already exists"))
	_, err = v.ReserveBidBlock(100, common.Address{}, common.HexToHash("0x02"))
	require.ErrorContains(t, err, "bid already exists") // terminal rejection stays blocked
	v.mevParams.Store(&buildertypes.MevParams{MaxBidsPerBuilder: 0})
	_, err = v.ReserveBidBlock(101, common.Address{}, hash)
	require.ErrorContains(t, err, "too many bids")
}

func TestBidBlockQuotaRetryableErrors(t *testing.T) {
	st, err := status.New(codes.DeadlineExceeded, "too late").WithDetails(&errdetails.ErrorInfo{
		Reason: strconv.Itoa(buildertypes.BidBlockTooLateError), Domain: mevErrorDomain})
	require.NoError(t, err)
	for _, tc := range []struct {
		err   error
		retry bool
	}{
		{nil, false},
		{bidBlockQuotaError("bid already exists"), false},
		{fromGRPCStatus(st.Err()), false},
		{errBidBlockTimeout, true},
		{&net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}, true},
		{fromGRPCStatus(status.Error(codes.Unavailable, "connection lost")), true},
	} {
		require.Equal(t, tc.retry, retryableBidBlockError(tc.err), "%v", tc.err)
	}
}

func TestBidBlockQuotaBuilderCannotExhaustSharedCache(t *testing.T) {
	var q bidBlockQuota
	now := time.Now()
	attacker, other := common.HexToAddress("0x01"), common.HexToAddress("0x02")
	for i := 0; i < maxBidBlockQuotaBuilderHeights; i++ {
		finish, err := q.reserve(2, uint64(i), attacker, common.Hash{}, now)
		require.NoError(t, err)
		finish(errors.New("block in future")) // downstream rejection must not poison other builders
	}
	require.Len(t, q.entries, maxBidBlockQuotaBuilderHeights)
	require.Equal(t, maxBidBlockQuotaBuilderHeights, q.builderHeights[attacker])
	for range 2 {
		_, err := q.reserve(2, maxBidBlockQuotaBuilderHeights, attacker, common.Hash{}, now)
		require.ErrorContains(t, err, "too many target heights")
	}
	finish, err := q.reserve(2, 100, other, common.Hash{}, now)
	require.NoError(t, err)
	finish(nil)
	require.Equal(t, 1, q.builderHeights[other])
	require.Len(t, q.entries, maxBidBlockQuotaBuilderHeights+1)
}

func TestBidBlockQuotaBoundedAndExpires(t *testing.T) {
	var q bidBlockQuota
	now := time.Now()
	for i := 0; i < maxBidBlockQuotaEntries; i++ {
		builder := common.Address{19: byte(i / maxBidBlockQuotaBuilderHeights)}
		_, err := q.reserve(1, uint64(i), builder, common.Hash{}, now)
		require.NoError(t, err)
	}
	require.Len(t, q.entries, maxBidBlockQuotaEntries)
	other := common.HexToAddress("0xff")
	finish, err := q.reserve(1, 100, other, common.Hash{}, now)
	require.NoError(t, err) // a full shared cache defers enforcement to the validator
	finish(nil)
	require.Len(t, q.entries, maxBidBlockQuotaEntries)
	require.Zero(t, q.builderHeights[other])
	_, err = q.reserve(1, 0, common.Address{}, common.Hash{}, now)
	require.ErrorContains(t, err, "bid already exists") // live records were not evicted
	_, err = q.reserve(1, 100, other, common.Hash{}, now.Add(bidBlockQuotaTTL))
	require.NoError(t, err)
	require.Len(t, q.entries, 1)
	require.Equal(t, map[common.Address]int{other: 1}, q.builderHeights)
}
