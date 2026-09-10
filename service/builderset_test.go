package service

import (
	"context"
	"sync"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	buildertypes "github.com/ethereum/go-ethereum/core/types/builder"
	"github.com/stretchr/testify/require"

	"github.com/bnb-chain/bsc-mev-sentry/node"
)

type stubBuilder struct{ cfg node.BuilderConfig }

func (s *stubBuilder) ReportIssue(context.Context, buildertypes.BidIssue) error { return nil }
func (s *stubBuilder) Config() node.BuilderConfig                               { return s.cfg }

func TestBuilderSet_LookupAndReplace(t *testing.T) {
	x := common.HexToAddress("0x1")
	y := common.HexToAddress("0x2")
	set := NewBuilderSet(map[common.Address]node.Builder{x: &stubBuilder{node.BuilderConfig{Address: x, URL: "ux"}}})

	_, ok := set.Lookup(x)
	require.True(t, ok)
	_, ok = set.Lookup(y)
	require.False(t, ok)
	require.Equal(t, 1, set.Len())

	set.Replace([]node.BuilderConfig{{Address: x, URL: "ux"}, {Address: y, URL: "uy"}})
	bx, _ := set.Lookup(x)
	require.IsType(t, &stubBuilder{}, bx, "unchanged config keeps the existing builder instance")
	_, ok = set.Lookup(y)
	require.True(t, ok)
	require.Equal(t, 2, set.Len())

	set.Replace(nil)
	require.Equal(t, 0, set.Len())
	_, ok = set.Lookup(x)
	require.False(t, ok)
}

func TestBuilderSet_ConcurrentReadersDuringReplace(t *testing.T) {
	addr := common.HexToAddress("0xaa")
	set := NewBuilderSet(map[common.Address]node.Builder{addr: &stubBuilder{}})

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					// Must never panic or observe a torn map; presence may flip, that is fine.
					set.Lookup(addr)
					set.Len()
				}
			}
		}()
	}
	for i := 0; i < 200; i++ {
		if i%2 == 0 {
			set.Replace([]node.BuilderConfig{{Address: addr, URL: "u"}})
		} else {
			set.Replace(nil)
		}
	}
	close(stop)
	wg.Wait()
}

func TestMevSentry_RegistryStatusReflectsState(t *testing.T) {
	set := NewBuilderSet(nil)
	state := NewRegistryState(true, common.HexToAddress("0xc0de"), 0)
	s := NewMevSentryWithSet(&Config{}, nil, set, state)

	st := s.RegistryStatus()
	require.True(t, st.Enabled)
	require.Equal(t, "static", st.Source)
	require.Equal(t, 0, st.BuilderCount)

	set.Replace([]node.BuilderConfig{{Address: common.HexToAddress("0x1"), URL: "u"}})
	state.RecordSuccess(42, common.HexToHash("0xbeef"), st.FetchedAt, common.HexToHash("0xf1"), 1)
	st = s.RegistryStatus()
	require.Equal(t, "registry", st.Source)
	require.Equal(t, uint64(42), st.BlockNumber)
	require.Equal(t, 1, st.BuilderCount)
}

func TestNewMevSentry_StaticPathStillWorks(t *testing.T) {
	addr := common.HexToAddress("0x1")
	s := NewMevSentry(&Config{}, nil, map[common.Address]node.Builder{addr: nil})
	_, ok := s.builders.Lookup(addr)
	require.True(t, ok)
	require.False(t, s.RegistryStatus().Enabled)
	require.Equal(t, 1, s.RegistryStatus().BuilderCount)
}
