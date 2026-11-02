package registry

import (
	"context"
	"errors"
	"math/big"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ethereum/go-ethereum/common"

	"github.com/bnb-chain/bsc-mev-sentry/node"
)

type readerFunc func(context.Context) (*Snapshot, error)

func (f readerFunc) Fetch(ctx context.Context) (*Snapshot, error) { return f(ctx) }

func fallbackForTest(sources ...Reader) *FallbackReader {
	return &FallbackReader{sources: sources, timeout: DefaultFetchTimeout}
}

func TestFallbackReader_TriesSourcesInOrderEveryPoll(t *testing.T) {
	var calls []int
	first := readerFunc(func(context.Context) (*Snapshot, error) {
		calls = append(calls, 1)
		if len(calls) == 1 {
			return nil, errors.New("first validator unavailable")
		}
		return snapOf(11, b(a1, "u1", "n")), nil
	})
	second := readerFunc(func(context.Context) (*Snapshot, error) {
		calls = append(calls, 2)
		return snapOf(10, b(a2, "u2", "n")), nil
	})
	third := readerFunc(func(context.Context) (*Snapshot, error) {
		t.Fatal("must stop at the first successful source")
		return nil, nil
	})
	r := fallbackForTest(first, second, third)

	snap, err := r.Fetch(context.Background())
	require.NoError(t, err)
	require.Equal(t, a2, snap.Builders[0].Addr)
	snap, err = r.Fetch(context.Background())
	require.NoError(t, err)
	require.Equal(t, a1, snap.Builders[0].Addr)
	require.Equal(t, []int{1, 2, 1}, calls, "each poll starts at the first configured source")
}

func TestFallbackReader_ReadsWholeSnapshotFromNextNode(t *testing.T) {
	first := &fakeBackend{
		header:  &headerLite{Number: big.NewInt(100), Hash: common.HexToHash("0x100")},
		callErr: errors.New("eth_call unavailable"),
	}
	second := &fakeBackend{
		header: &headerLite{Number: big.NewInt(101), Hash: common.HexToHash("0x101")},
		output: packBuilders(t, []Builder{b(a2, "u2", "n")}),
	}
	r := fallbackForTest(NewClient(first, a3), NewClient(second, a3))
	snap, err := r.Fetch(context.Background())
	require.NoError(t, err)
	require.Equal(t, first.header.Hash, first.gotHash)
	require.Equal(t, second.header.Hash, second.gotHash, "must resolve the second node's own finalized header")
	require.Equal(t, second.header.Hash, snap.BlockHash)
	require.Equal(t, uint64(101), snap.BlockNumber)
	require.Equal(t, a2, snap.Builders[0].Addr)
}

func TestFallbackReader_DialFailureTriesNextSource(t *testing.T) {
	r := NewFallbackReader([]string{"unsupported://validator"}, a1)
	r.sources = append(r.sources, NewClient(&fakeBackend{
		header: &headerLite{Number: big.NewInt(1), Hash: common.HexToHash("0x1")},
		output: packBuilders(t, []Builder{b(a2, "u2", "n")}),
	}, a1))
	t.Cleanup(r.Close)
	snap, err := r.Fetch(context.Background())
	require.NoError(t, err)
	require.Equal(t, a2, snap.Builders[0].Addr)
}

func TestFallbackReader_EmptySnapshotIsAuthoritative(t *testing.T) {
	r := fallbackForTest(
		readerFunc(func(context.Context) (*Snapshot, error) { return snapOf(10), nil }),
		readerFunc(func(context.Context) (*Snapshot, error) {
			t.Fatal("a decoded empty registry must not trigger fallback")
			return nil, nil
		}),
	)
	snap, err := r.Fetch(context.Background())
	require.NoError(t, err)
	require.Empty(t, snap.Builders)
}

func TestFallbackReader_TimeoutTriesNextSource(t *testing.T) {
	r := fallbackForTest(
		readerFunc(func(ctx context.Context) (*Snapshot, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}),
		readerFunc(func(ctx context.Context) (*Snapshot, error) {
			require.NoError(t, ctx.Err(), "next source needs a fresh attempt deadline")
			return snapOf(10), nil
		}),
	)
	r.timeout = 20 * time.Millisecond
	_, err := r.Fetch(context.Background())
	require.NoError(t, err)
}

func TestFallbackReader_CancellationStopsFallback(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := fallbackForTest(
		readerFunc(func(context.Context) (*Snapshot, error) {
			cancel()
			return nil, context.Canceled
		}),
		readerFunc(func(context.Context) (*Snapshot, error) {
			t.Fatal("must not contact another node after shutdown")
			return nil, nil
		}),
	)
	_, err := r.Fetch(ctx)
	require.ErrorIs(t, err, context.Canceled)
}

func TestFallbackReader_AllSourcesFailKeepsSetAndRedactsStatus(t *testing.T) {
	failure := readerFunc(func(context.Context) (*Snapshot, error) {
		return nil, &url.Error{Op: "Post", URL: "http://private-validator:8545", Err: ErrCall}
	})
	r := fallbackForTest(failure, failure)
	s, set, status := newSyncer(r, Config{}, map[common.Address]node.Builder{
		a4: node.NewBuilder(node.BuilderConfig{Address: a4}),
	})
	require.Equal(t, ResultError, s.SyncOnce(context.Background()))
	require.Equal(t, map[common.Address]bool{a4: true}, addrs(set))
	require.Equal(t, "eth_call_failed", status.Snapshot().LastError)
	require.Equal(t, "static", status.Snapshot().Source)
}
