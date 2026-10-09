package node

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types/builder/mevpb"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rpc"
)

const (
	maxBidBlockGRPCMessageSize = 2 * params.MaxBlockSize
	mevErrorDomain             = "mev.bnbchain.org"
)

// ErrGRPCNotConfigured is returned when a gRPC BidBlock targets a validator
// without GRPCURL. gRPC submissions never fall back to JSON-RPC.
var ErrGRPCNotConfigured = errors.New("validator gRPC endpoint not configured")

type bidBlockGRPCClient struct {
	conn   *grpc.ClientConn
	client mevpb.BidBlockServiceClient
}

func newBidBlockGRPCClient(target string) (*bidBlockGRPCClient, error) {
	conn, err := grpc.NewClient(
		target,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.MaxCallSendMsgSize(maxBidBlockGRPCMessageSize)),
	)
	if err != nil {
		return nil, fmt.Errorf("dial validator gRPC target %s: %w", target, err)
	}
	// Connect now so the first BidBlock does not pay for the handshake.
	conn.Connect()
	return &bidBlockGRPCClient{conn: conn, client: mevpb.NewBidBlockServiceClient(conn)}, nil
}

// SendBidBlock forwards the builder's RLP bytes as received, without re-encoding.
func (c *bidBlockGRPCClient) SendBidBlock(ctx context.Context, bidBlockRLP, signature []byte) (common.Hash, error) {
	response, err := c.client.SendBidBlock(ctx, &mevpb.BidBlockRequest{
		BidBlockRlp: bidBlockRLP,
		Signature:   signature,
	})
	if err != nil {
		return common.Hash{}, err
	}
	if len(response.BidHash) != common.HashLength {
		return common.Hash{}, fmt.Errorf("invalid BidBlock hash length %d", len(response.BidHash))
	}
	return common.BytesToHash(response.BidHash), nil
}

type grpcMEVError struct {
	code    int
	message string
	cause   error
}

func (e *grpcMEVError) Error() string  { return e.message }
func (e *grpcMEVError) ErrorCode() int { return e.code }
func (e *grpcMEVError) Unwrap() error  { return e.cause }

var _ rpc.Error = (*grpcMEVError)(nil)

// fromGRPCStatus restores MEV error codes from the validator. Other statuses
// keep their code but drop the message, which can carry internal addresses.
func fromGRPCStatus(err error) error {
	st, ok := status.FromError(err)
	if !ok {
		return err
	}
	for _, detail := range st.Details() {
		info, ok := detail.(*errdetails.ErrorInfo)
		if !ok || info.Domain != mevErrorDomain {
			continue
		}
		code, parseErr := strconv.Atoi(info.Reason)
		if parseErr == nil {
			return &grpcMEVError{code: code, message: st.Message(), cause: err}
		}
	}
	return status.Errorf(st.Code(), "validator gRPC call failed: %s", st.Code())
}
