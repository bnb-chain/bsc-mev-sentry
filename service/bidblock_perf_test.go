package service

import (
	"context"
	"encoding/json"
	"math/big"
	"net"
	"testing"
	"time"

	buildertypes "github.com/ethereum/go-ethereum/core/types/builder"
	"github.com/ethereum/go-ethereum/core/types/builder/mevpb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func TestBidBlockArgsJSONDecodeTiming(t *testing.T) {
	block := sampleBidBlock()
	block.Header.Difficulty = big.NewInt(1)
	data, err := json.Marshal(BidBlockArgsWrapper{
		BidBlockArgs:      buildertypes.BidBlockArgs{BidBlock: block, Signature: []byte{1, 2}},
		ValidatorHostName: "val-1",
	})
	require.NoError(t, err)

	var got BidBlockArgsWrapper
	require.NoError(t, json.Unmarshal(data, &got))
	require.Equal(t, "val-1", got.ValidatorHostName)
	require.Equal(t, []byte{1, 2}, []byte(got.Signature))
	require.Equal(t, block.Hash(), got.BidBlock.Hash())
	require.Equal(t, len(data), got.payloadBytes)
	require.Positive(t, got.decodeElapsed)
}

type decodeTimingServer struct {
	mevpb.UnimplementedBidBlockServiceServer
	got chan time.Duration
}

func (s *decodeTimingServer) SendBidBlock(ctx context.Context, _ *mevpb.BidBlockRequest) (*mevpb.BidBlockResponse, error) {
	s.got <- protoDecodeElapsed(ctx)
	return &mevpb.BidBlockResponse{}, nil
}

func TestProtoDecodeTiming(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	impl := &decodeTimingServer{got: make(chan time.Duration, 1)}
	srv := grpc.NewServer(grpc.ChainUnaryInterceptor(recoverPanic))
	srv.RegisterService(withProtoDecodeTiming(mevpb.BidBlockService_ServiceDesc), impl)
	go func() { _ = srv.Serve(lis) }()
	defer srv.Stop()

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = mevpb.NewBidBlockServiceClient(conn).SendBidBlock(ctx,
		&mevpb.BidBlockRequest{BidBlockRlp: make([]byte, 1<<20), ValidatorHostName: "val-1"})
	require.NoError(t, err)
	select {
	case elapsed := <-impl.got:
		require.Positive(t, elapsed)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}
