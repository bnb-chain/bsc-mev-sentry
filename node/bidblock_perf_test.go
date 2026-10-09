package node

import (
	"context"
	"encoding/json"
	"math/big"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	buildertypes "github.com/ethereum/go-ethereum/core/types/builder"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/stretchr/testify/require"
)

type perfMevAPI struct {
	mu   sync.Mutex
	got  buildertypes.BidBlockArgs
	fail bool
}

func (a *perfMevAPI) SendBidBlock(_ context.Context, args buildertypes.BidBlockArgs) (common.Hash, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.got = args
	if a.fail {
		return common.Hash{}, buildertypes.NewBidBlockPreSealVerifyError("zero root")
	}
	return common.HexToHash("0x123"), nil
}

func TestMeasuredBidBlockForward(t *testing.T) {
	args := buildertypes.BidBlockArgs{BidBlock: &buildertypes.BidBlock{Header: &types.Header{Number: big.NewInt(1), Difficulty: big.NewInt(1)}}, Signature: []byte{1, 2}}
	want, err := json.Marshal(args)
	require.NoError(t, err)
	stats := new(encodeStats)
	got, err := json.Marshal(measuredBidBlockArgs{args: args, stats: stats})
	require.NoError(t, err)
	require.JSONEq(t, string(want), string(got))
	require.Equal(t, len(want), stats.bytes)
	api := new(perfMevAPI)
	server := rpc.NewServer()
	defer server.Stop()
	require.NoError(t, server.RegisterName("mev", api))
	httpServer := httptest.NewServer(server)
	defer httpServer.Close()
	client, err := ethclient.Dial(httpServer.URL)
	require.NoError(t, err)
	defer client.Close()
	v := &validator{client: client}
	hash, err := v.SendBidBlock(context.Background(), args, common.Address{}, args.BidBlock.Hash())
	require.NoError(t, err)
	require.Equal(t, common.HexToHash("0x123"), hash)
	api.mu.Lock()
	gotSignature := api.got.Signature
	api.fail = true
	api.mu.Unlock()
	require.Equal(t, args.Signature, gotSignature)
	hash, err = v.SendBidBlock(context.Background(), args, common.Address{}, args.BidBlock.Hash())
	require.Error(t, err)
	var rpcErr rpc.Error
	require.ErrorAs(t, err, &rpcErr)
	require.Equal(t, buildertypes.BidBlockPreSealVerifyError, rpcErr.ErrorCode())
	require.Zero(t, hash)
}
