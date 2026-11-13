package node

import (
	"context"
	"errors"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
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

func newQuotaTestValidator(limit uint32) *validator {
	v := &validator{}
	v.mevParams.Store(&buildertypes.MevParams{MaxBidsPerBuilder: limit})
	return v
}

func TestBidBlockQuotaLimits(t *testing.T) {
	for _, tc := range []struct {
		name  string
		limit uint32
	}{
		{"two", 2},
		{"custom", 4},
		{"zero", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := newQuotaTestValidator(tc.limit)
			for i := uint32(0); i < tc.limit; i++ {
				finish, err := v.ReserveBidBlock(100, common.Address{}, common.Hash{31: byte(i)})
				require.NoError(t, err)
				finish(nil)
			}
			_, err := v.ReserveBidBlock(100, common.Address{}, common.Hash{31: 99})
			require.ErrorContains(t, err, "too many bids")
			var rpcErr rpc.Error
			require.ErrorAs(t, err, &rpcErr)
			require.Equal(t, -32000, rpcErr.ErrorCode())
		})
	}
}

func TestBidBlockQuotaDuplicatesAndRetries(t *testing.T) {
	v := newQuotaTestValidator(1)
	hash := common.HexToHash("0x01")
	finish, err := v.ReserveBidBlock(100, common.Address{}, hash)
	require.NoError(t, err)
	_, err = v.ReserveBidBlock(100, common.Address{}, hash)
	require.ErrorContains(t, err, "bid already exists") // still in-flight

	finish(context.DeadlineExceeded)
	finish, err = v.ReserveBidBlock(100, common.Address{}, hash)
	require.NoError(t, err) // same hash can retry after an ambiguous failure
	finish(nil)
	_, err = v.ReserveBidBlock(100, common.Address{}, hash)
	require.ErrorContains(t, err, "bid already exists") // succeeded
	_, err = v.ReserveBidBlock(100, common.Address{}, common.HexToHash("0x02"))
	require.ErrorContains(t, err, "too many bids")

	for _, other := range []struct {
		v       *validator
		block   uint64
		builder common.Address
	}{
		{newQuotaTestValidator(1), 100, common.Address{}},
		{v, 101, common.Address{}},
		{v, 100, common.HexToAddress("0x02")},
	} {
		finish, err := other.v.ReserveBidBlock(other.block, other.builder, hash)
		require.NoError(t, err)
		finish(nil)
	}
}

func TestBidBlockQuotaFailureDoesNotResetCount(t *testing.T) {
	v := newQuotaTestValidator(1)
	finish, err := v.ReserveBidBlock(100, common.Address{}, common.HexToHash("0x01"))
	require.NoError(t, err)
	finish(errors.New("connection lost"))
	_, err = v.ReserveBidBlock(100, common.Address{}, common.HexToHash("0x02"))
	require.ErrorContains(t, err, "too many bids")
}

func TestBidBlockQuotaConcurrentReservations(t *testing.T) {
	v := newQuotaTestValidator(2)
	var wg sync.WaitGroup
	var admitted atomic.Int32
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			finish, err := v.ReserveBidBlock(100, common.Address{}, common.Hash{31: byte(i)})
			if err == nil {
				admitted.Add(1)
				finish(nil)
			}
		}(i)
	}
	wg.Wait()
	require.EqualValues(t, 2, admitted.Load())
}

func TestBidBlockQuotaWithoutCachedParams(t *testing.T) {
	v := &validator{}
	for i := 0; i < 3; i++ {
		finish, err := v.ReserveBidBlock(100, common.Address{}, common.Hash{31: byte(i)})
		require.NoError(t, err) // no invented default of 0 or 2 before the first fetch
		finish(nil)
	}
	require.Empty(t, v.bidBlocks.entries)
	v.mevParams.Store(&buildertypes.MevParams{MaxBidsPerBuilder: 1})
	finish, err := v.ReserveBidBlock(100, common.Address{}, common.Hash{})
	require.NoError(t, err)
	finish(nil)
	_, err = v.ReserveBidBlock(100, common.Address{}, common.Hash{31: 1})
	require.ErrorContains(t, err, "too many bids")
}

func TestBidBlockQuotaFollowsCachedParams(t *testing.T) {
	v := newQuotaTestValidator(1)
	first, second := common.HexToHash("0x01"), common.HexToHash("0x02")
	finish, err := v.ReserveBidBlock(100, common.Address{}, first)
	require.NoError(t, err)
	finish(nil)
	_, err = v.ReserveBidBlock(100, common.Address{}, second)
	require.ErrorContains(t, err, "too many bids")

	v.mevParams.Store(&buildertypes.MevParams{MaxBidsPerBuilder: 3})
	finish, err = v.ReserveBidBlock(100, common.Address{}, second)
	require.NoError(t, err) // updated limit, without resetting existing counts
	finish(context.DeadlineExceeded)
	v.mevParams.Store(&buildertypes.MevParams{MaxBidsPerBuilder: 0})
	_, err = v.ReserveBidBlock(100, common.Address{}, second)
	require.ErrorContains(t, err, "too many bids") // zero also blocks failed-hash retries
	v.mevParams.Store(&buildertypes.MevParams{MaxBidsPerBuilder: 1})
	_, err = v.ReserveBidBlock(100, common.Address{}, common.HexToHash("0x03"))
	require.ErrorContains(t, err, "too many bids")
}

func TestBidBlockQuotaBuilderCannotExhaustSharedCache(t *testing.T) {
	var q bidBlockQuota
	now := time.Now()
	attacker, other := common.HexToAddress("0x01"), common.HexToAddress("0x02")
	for i := 0; i < maxBidBlockQuotaEntries; i++ {
		finish, err := q.reserve(2, uint64(i), attacker, common.Hash{}, now)
		require.NoError(t, err)
		finish(errors.New("block in future")) // downstream rejection must not poison other builders
	}
	require.Len(t, q.entries, maxBidBlockQuotaBuilderHeights)
	require.Equal(t, maxBidBlockQuotaBuilderHeights, q.builderHeights[attacker])
	finish, err := q.reserve(2, 100, other, common.Hash{}, now)
	require.NoError(t, err)
	finish(nil)
	require.Equal(t, 1, q.builderHeights[other])
	require.Len(t, q.entries, maxBidBlockQuotaBuilderHeights+1)
}

func TestBidBlockQuotaConcurrentHeightCapacity(t *testing.T) {
	v := newQuotaTestValidator(2)
	var wg sync.WaitGroup
	errors := make(chan error, 2*maxBidBlockQuotaBuilderHeights)
	for i := 0; i < cap(errors); i++ {
		wg.Add(1)
		go func(block uint64) {
			defer wg.Done()
			finish, err := v.ReserveBidBlock(block, common.Address{}, common.Hash{})
			if err == nil {
				finish(nil)
			}
			errors <- err
		}(uint64(i))
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		require.NoError(t, err)
	}
	require.Len(t, v.bidBlocks.entries, maxBidBlockQuotaBuilderHeights)
	require.Equal(t, maxBidBlockQuotaBuilderHeights, v.bidBlocks.builderHeights[common.Address{}])
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
	finish(context.DeadlineExceeded)
	require.Len(t, q.entries, maxBidBlockQuotaEntries)
	require.Zero(t, q.builderHeights[other])
	_, err = q.reserve(0, 100, other, common.Hash{}, now)
	require.ErrorContains(t, err, "too many bids") // capacity fallback must not bypass a zero limit
	_, err = q.reserve(1, 0, common.Address{}, common.Hash{}, now)
	require.ErrorContains(t, err, "bid already exists") // live records were not evicted
	_, err = q.reserve(1, 100, other, common.Hash{}, now.Add(bidBlockQuotaTTL))
	require.NoError(t, err)
	require.Len(t, q.entries, 1)
	require.Equal(t, map[common.Address]int{other: 1}, q.builderHeights)
}

func TestBidBlockQuotaLateCompletionDoesNotChangeNewEntry(t *testing.T) {
	var q bidBlockQuota
	now := time.Now()
	finish, err := q.reserve(1, 100, common.Address{}, common.Hash{}, now)
	require.NoError(t, err)
	_, err = q.reserve(1, 100, common.Address{}, common.Hash{}, now.Add(bidBlockQuotaTTL))
	require.NoError(t, err)
	require.Equal(t, 1, q.builderHeights[common.Address{}])
	finish(context.DeadlineExceeded)
	_, err = q.reserve(1, 100, common.Address{}, common.Hash{}, now.Add(bidBlockQuotaTTL))
	require.ErrorContains(t, err, "bid already exists")
}

func TestBidBlockQuotaEntryExpiryRestoresBuilderCapacity(t *testing.T) {
	var q bidBlockQuota
	now := time.Now()
	_, err := q.reserve(1, 100, common.Address{}, common.Hash{}, now)
	require.NoError(t, err)
	q.nextSweep = now.Add(2 * bidBlockQuotaTTL) // exercise per-entry expiry without a full sweep
	_, err = q.reserve(1, 100, common.Address{}, common.Hash{}, now.Add(bidBlockQuotaTTL))
	require.NoError(t, err)
	require.Len(t, q.entries, 1)
	require.Equal(t, 1, q.builderHeights[common.Address{}])
}
