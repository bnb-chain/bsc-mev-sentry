package registry

import (
	"context"
	"math/big"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ethereum/go-ethereum/common"
)

func TestClient_RejectsUninitializedRegistry(t *testing.T) {
	for _, tc := range []struct {
		name   string
		output []byte
	}{
		{"zero", make([]byte, 32)},
		{"missing", []byte{}},
		{"short", []byte{4}},
		{"trailing data", append(common.LeftPadBytes([]byte{4}, 32), 0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend := &fakeBackend{
				header:    &headerLite{Number: big.NewInt(1), Hash: common.Hash{1}},
				output:    packBuilders(t, nil),
				capOutput: tc.output,
			}
			snap, err := NewClient(backend, a3).Fetch(context.Background())
			require.Nil(t, snap)
			require.ErrorIs(t, err, ErrNotRegistry)
			require.Equal(t, "not_a_registry", ErrorClass(err))
		})
	}
}

func TestSyncer_RegistryProbeFailurePreservesPolicy(t *testing.T) {
	backend := &fakeBackend{
		header:    &headerLite{Number: big.NewInt(1), Hash: common.Hash{1}},
		output:    packBuilders(t, nil),
		capOutput: make([]byte, 32),
	}
	s, set, state := newSyncer(NewClient(backend, a3), Config{}, nil)

	// An implementation address must neither open service ports nor enable allow-all.
	require.Equal(t, ResultError, s.SyncOnce(context.Background()))
	require.False(t, set.AllowAll())
	require.Equal(t, "not_a_registry", state.Snapshot().LastError)
	select {
	case <-s.Ready():
		t.Fatal("uninitialized registry signaled service readiness")
	default:
	}

	// A valid, initialized proxy is allowed to have an empty list.
	backend.capOutput = common.LeftPadBytes([]byte{4}, 32)
	require.Equal(t, ResultEmpty, s.SyncOnce(context.Background()))
	require.True(t, set.AllowAll())
	select {
	case <-s.Ready():
	default:
		t.Fatal("initialized empty registry did not signal service readiness")
	}

	// A later probe failure preserves allow-all just like any other failed read.
	backend.capOutput = make([]byte, 32)
	require.Equal(t, ResultError, s.SyncOnce(context.Background()))
	require.True(t, set.AllowAll())

	backend.capOutput = common.LeftPadBytes([]byte{4}, 32)
	backend.output = packBuilders(t, []Builder{b(a1, "https://builder.example", "builder")})
	require.Equal(t, ResultApplied, s.SyncOnce(context.Background()))
	require.False(t, set.AllowAll())
	previous := state.Snapshot()

	// A failed probe must not replace a working list with an empty snapshot.
	backend.output = packBuilders(t, nil)
	backend.capOutput = make([]byte, 32)
	require.Equal(t, ResultError, s.SyncOnce(context.Background()))
	require.False(t, set.AllowAll())
	require.True(t, set.Allowed(a1))
	require.False(t, set.Allowed(a2))
	require.Equal(t, previous.Fingerprint, state.Snapshot().Fingerprint)
	require.Equal(t, previous.FetchedAt, state.Snapshot().FetchedAt)
}

func TestSyncer_RegistryProbeErrorsAreSanitized(t *testing.T) {
	backend := &fakeBackend{
		header: &headerLite{Number: big.NewInt(1), Hash: common.Hash{1}},
		output: packBuilders(t, nil),
		capErr: &url.Error{Op: "Post", URL: "http://private-validator.example", Err: context.DeadlineExceeded},
	}
	s, set, state := newSyncer(NewClient(backend, a3), Config{}, nil)
	require.Equal(t, ResultError, s.SyncOnce(context.Background()))
	require.Equal(t, "timeout", state.Snapshot().LastError)
	require.False(t, set.AllowAll())
}

func TestFallbackReader_UninitializedRegistryTriesNextSource(t *testing.T) {
	first := &fakeBackend{
		header:    &headerLite{Number: big.NewInt(10), Hash: common.Hash{10}},
		output:    packBuilders(t, nil),
		capOutput: make([]byte, 32),
	}
	second := &fakeBackend{
		header: &headerLite{Number: big.NewInt(11), Hash: common.Hash{11}},
		output: packBuilders(t, []Builder{b(a1, "https://builder.example", "builder")}),
	}
	reader := fallbackForTest(NewClient(first, a3), NewClient(second, a3))
	snap, err := reader.Fetch(context.Background())
	require.NoError(t, err)
	require.Equal(t, uint64(11), snap.BlockNumber)
	require.Equal(t, a1, snap.Builders[0].Addr)
	require.Equal(t, first.header.Hash, first.capHash)
	require.Equal(t, second.header.Hash, second.capHash)
}
