package node

import (
	"context"
	"net"
	"strconv"
	"testing"

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
