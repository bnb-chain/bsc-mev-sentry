package service

import (
	"context"
	"encoding/json"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	buildertypes "github.com/ethereum/go-ethereum/core/types/builder"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/crypto/kzg4844"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/stretchr/testify/require"

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
