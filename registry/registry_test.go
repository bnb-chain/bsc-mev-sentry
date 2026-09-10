package registry

import (
	"context"
	"errors"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/stretchr/testify/require"

	"github.com/bnb-chain/bsc-mev-sentry/node"
	"github.com/bnb-chain/bsc-mev-sentry/service"
)

var (
	a1 = common.HexToAddress("0x0000000000000000000000000000000000000a01")
	a2 = common.HexToAddress("0x0000000000000000000000000000000000000a02")
	a3 = common.HexToAddress("0x0000000000000000000000000000000000000a03")
	a4 = common.HexToAddress("0x0000000000000000000000000000000000000a04")
)

func b(addr common.Address, url, name string) Builder {
	return Builder{Addr: addr, URL: url, Name: name}
}

// ---------------------------------------------------------------------------
// Fingerprint / Merge / Diff
// ---------------------------------------------------------------------------

func TestFingerprint_OrderIndependentAndContentSensitive(t *testing.T) {
	x := []Builder{b(a1, "u1", "n"), b(a2, "u2", "n")}
	y := []Builder{b(a2, "u2", "n"), b(a1, "u1", "n")}
	require.Equal(t, Fingerprint(x), Fingerprint(y), "order must not matter")

	z := []Builder{b(a1, "u1-changed", "n"), b(a2, "u2", "n")}
	require.NotEqual(t, Fingerprint(x), Fingerprint(z), "url change must be visible")

	w := []Builder{b(a1, "u1", "other"), b(a2, "u2", "n")}
	require.NotEqual(t, Fingerprint(x), Fingerprint(w), "name change must be visible")
	require.NotEqual(t, Fingerprint(nil), Fingerprint(x))
}

func TestMerge_ExtraAndBlocked(t *testing.T) {
	base := []Builder{b(a1, "r1", "n"), b(a2, "r2", "n"), b(a3, "r3", "n")}
	extra := []node.BuilderConfig{
		{Address: a4, URL: "x4"}, // new: added
		{Address: a1, URL: "x1"}, // already in registry: registry URL wins
		{Address: a3, URL: "x3"}, // blocked below: must not resurrect
	}
	blocked := []common.Address{a2, a3}

	got := Merge(base, extra, blocked)

	require.Equal(t, []node.BuilderConfig{
		{Address: a1, URL: "r1"},
		{Address: a4, URL: "x4"},
	}, got)
}

func TestMerge_EmptyInputs(t *testing.T) {
	require.Empty(t, Merge(nil, nil, nil))
	require.Equal(t, []node.BuilderConfig{{Address: a1, URL: "x"}}, Merge(nil, []node.BuilderConfig{{Address: a1, URL: "x"}}, nil))
}

func TestDiff(t *testing.T) {
	prev := []Builder{b(a1, "", ""), b(a2, "", "")}
	next := []Builder{b(a2, "", ""), b(a3, "", "")}
	added, removed := Diff(prev, next)
	require.Equal(t, []common.Address{a3}, added)
	require.Equal(t, []common.Address{a1}, removed)

	added, removed = Diff(nil, next)
	require.Len(t, added, 2)
	require.Empty(t, removed)
}

// ---------------------------------------------------------------------------
// Client: ABI round trip through a fake backend
// ---------------------------------------------------------------------------

type fakeBackend struct {
	header    *headerLite
	headerErr error
	output    []byte
	callErr   error
	gotHash   common.Hash
	gotTo     common.Address
	gotNumber *big.Int
}

func (f *fakeBackend) HeaderByNumber(_ context.Context, number *big.Int) (*headerLite, error) {
	f.gotNumber = number
	return f.header, f.headerErr
}

func (f *fakeBackend) CallContractAtHash(_ context.Context, msg ethereum.CallMsg, blockHash common.Hash) ([]byte, error) {
	f.gotHash = blockHash
	f.gotTo = *msg.To
	return f.output, f.callErr
}

func packBuilders(t *testing.T, builders []Builder) []byte {
	t.Helper()
	out, err := registryABI.Methods["getBuilders"].Outputs.Pack(builders)
	require.NoError(t, err)
	return out
}

func TestClient_FetchDecodesContractOutput(t *testing.T) {
	contract := common.HexToAddress("0x00000000000000000000000000000000000c0de")
	want := []Builder{
		b(a1, "https://dublin.builder.example", "blockrazor"),
		b(a2, "https://puissant-builder.48.club", "48club"),
	}
	fb := &fakeBackend{
		header: &headerLite{Number: big.NewInt(52_000_000), Hash: common.HexToHash("0xabc")},
		output: packBuilders(t, want),
	}
	c := NewClient(fb, contract, rpc.FinalizedBlockNumber)

	snap, err := c.Fetch(context.Background())
	require.NoError(t, err)
	require.Equal(t, want, snap.Builders)
	require.Equal(t, uint64(52_000_000), snap.BlockNumber)
	require.Equal(t, common.HexToHash("0xabc"), snap.BlockHash)
	require.Equal(t, Fingerprint(want), snap.Fingerprint)
	require.False(t, snap.FetchedAt.IsZero())

	// Pinned to the resolved block hash, addressed to the contract, using the finalized tag.
	require.Equal(t, common.HexToHash("0xabc"), fb.gotHash)
	require.Equal(t, contract, fb.gotTo)
	require.Equal(t, rpc.FinalizedBlockNumber.Int64(), fb.gotNumber.Int64())
}

func TestClient_FetchErrors(t *testing.T) {
	contract := common.HexToAddress("0xc0de")
	hdr := &headerLite{Number: big.NewInt(1), Hash: common.HexToHash("0x1")}

	_, err := NewClient(&fakeBackend{headerErr: errors.New("rpc down")}, contract, rpc.LatestBlockNumber).Fetch(context.Background())
	require.ErrorContains(t, err, "resolve block tag")

	_, err = NewClient(&fakeBackend{header: hdr, callErr: errors.New("boom")}, contract, rpc.LatestBlockNumber).Fetch(context.Background())
	require.ErrorContains(t, err, "eth_call")

	// No code at address: eth_call returns empty output, not an error.
	_, err = NewClient(&fakeBackend{header: hdr, output: nil}, contract, rpc.LatestBlockNumber).Fetch(context.Background())
	require.ErrorContains(t, err, "ContractAddress")

	_, err = NewClient(&fakeBackend{header: hdr, output: []byte{1, 2, 3}}, contract, rpc.LatestBlockNumber).Fetch(context.Background())
	require.ErrorContains(t, err, "decode")
}

func TestClient_FetchEmptyRegistryIsNotAnError(t *testing.T) {
	fb := &fakeBackend{
		header: &headerLite{Number: big.NewInt(1), Hash: common.HexToHash("0x1")},
		output: packBuilders(t, []Builder{}),
	}
	snap, err := NewClient(fb, common.Address{1}, rpc.LatestBlockNumber).Fetch(context.Background())
	require.NoError(t, err)
	require.Empty(t, snap.Builders) // the syncer decides what to do with it
}

// ---------------------------------------------------------------------------
// Syncer semantics
// ---------------------------------------------------------------------------

type fakeReader struct {
	snaps []*Snapshot
	errs  []error
	i     int
}

func (f *fakeReader) Fetch(context.Context) (*Snapshot, error) {
	defer func() { f.i++ }()
	var err error
	if f.i < len(f.errs) {
		err = f.errs[f.i]
	}
	var s *Snapshot
	if f.i < len(f.snaps) {
		s = f.snaps[f.i]
	}
	return s, err
}

func snapOf(block uint64, builders ...Builder) *Snapshot {
	return &Snapshot{Builders: builders, BlockNumber: block, Fingerprint: Fingerprint(builders)}
}

func newSyncer(reader Reader, cfg Config, staticSet map[common.Address]node.Builder) (*Syncer, *service.BuilderSet, *service.RegistryState) {
	set := service.NewBuilderSet(staticSet)
	state := service.NewRegistryState(true, cfg.ContractAddress, len(staticSet))
	return NewSyncer(&cfg, reader, set, state), set, state
}

func addrs(set *service.BuilderSet) map[common.Address]bool {
	out := map[common.Address]bool{}
	for _, a := range set.Addresses() {
		out[a] = true
	}
	return out
}

func TestSyncer_FirstReadReplacesStaticSet(t *testing.T) {
	static := map[common.Address]node.Builder{a4: node.NewBuilder(node.BuilderConfig{Address: a4, URL: "static"})}
	reader := &fakeReader{snaps: []*Snapshot{snapOf(100, b(a1, "u1", "n"), b(a2, "u2", "n"))}}
	s, set, state := newSyncer(reader, Config{}, static)

	require.Equal(t, "static", state.Snapshot().Source)
	require.Equal(t, ResultApplied, s.SyncOnce(context.Background()))

	require.Equal(t, map[common.Address]bool{a1: true, a2: true}, addrs(set), "static a4 is gone once registry is authoritative")
	st := state.Snapshot()
	require.Equal(t, "registry", st.Source)
	require.Equal(t, uint64(100), st.BlockNumber)
	require.Equal(t, 2, st.BuilderCount)
	require.Equal(t, Fingerprint([]Builder{b(a1, "u1", "n"), b(a2, "u2", "n")}), st.Fingerprint)
}

func TestSyncer_ErrorAndEmptyKeepPreviousSet(t *testing.T) {
	static := map[common.Address]node.Builder{a4: node.NewBuilder(node.BuilderConfig{Address: a4})}
	reader := &fakeReader{
		snaps: []*Snapshot{nil, snapOf(5), snapOf(6, b(a1, "u", "n")), nil, snapOf(8)},
		errs:  []error{errors.New("rpc timeout"), nil, nil, errors.New("again"), nil},
	}
	s, set, state := newSyncer(reader, Config{}, static)

	// 1) error before any success: static set stays
	require.Equal(t, ResultError, s.SyncOnce(context.Background()))
	require.Equal(t, map[common.Address]bool{a4: true}, addrs(set))
	require.Equal(t, "static", state.Snapshot().Source)
	require.Contains(t, state.Snapshot().LastError, "rpc timeout")

	// 2) empty before any success: static set stays
	require.Equal(t, ResultEmpty, s.SyncOnce(context.Background()))
	require.Equal(t, map[common.Address]bool{a4: true}, addrs(set))

	// 3) success: registry applied
	require.Equal(t, ResultApplied, s.SyncOnce(context.Background()))
	require.Equal(t, map[common.Address]bool{a1: true}, addrs(set))

	// 4) error after success: registry set stays, source remains "registry"
	require.Equal(t, ResultError, s.SyncOnce(context.Background()))
	require.Equal(t, map[common.Address]bool{a1: true}, addrs(set))
	require.Equal(t, "registry", state.Snapshot().Source)

	// 5) empty after success: registry set stays
	require.Equal(t, ResultEmpty, s.SyncOnce(context.Background()))
	require.Equal(t, map[common.Address]bool{a1: true}, addrs(set))
}

func TestSyncer_UnchangedFingerprintDoesNotRebuild(t *testing.T) {
	same := []Builder{b(a1, "u1", "n")}
	reader := &fakeReader{snaps: []*Snapshot{snapOf(10, same...), snapOf(11, same...)}}
	s, set, _ := newSyncer(reader, Config{}, nil)

	require.Equal(t, ResultApplied, s.SyncOnce(context.Background()))
	before, _ := set.Lookup(a1)
	require.Equal(t, ResultUnchanged, s.SyncOnce(context.Background()))
	after, _ := set.Lookup(a1)
	require.Same(t, before, after, "same fingerprint must not touch the allowlist")
}

func TestSyncer_AppliesExtraAndBlocked(t *testing.T) {
	cfg := Config{
		ExtraBuilders:   []node.BuilderConfig{{Address: a3, URL: "mine"}},
		BlockedBuilders: []common.Address{a2},
	}
	reader := &fakeReader{snaps: []*Snapshot{snapOf(1, b(a1, "u1", "n"), b(a2, "u2", "n"))}}
	s, set, state := newSyncer(reader, cfg, nil)

	require.Equal(t, ResultApplied, s.SyncOnce(context.Background()))
	require.Equal(t, map[common.Address]bool{a1: true, a3: true}, addrs(set))
	require.Equal(t, 2, state.Snapshot().BuilderCount, "status reports the effective count, not the raw registry count")
}

func TestSyncer_ChangeReplacesOnlyChangedBuilders(t *testing.T) {
	reader := &fakeReader{snaps: []*Snapshot{
		snapOf(1, b(a1, "u1", "n"), b(a2, "u2", "n")),
		snapOf(2, b(a1, "u1", "n"), b(a2, "u2-new", "n"), b(a3, "u3", "n")),
	}}
	s, set, _ := newSyncer(reader, Config{}, nil)

	require.Equal(t, ResultApplied, s.SyncOnce(context.Background()))
	keep, _ := set.Lookup(a1)
	replace, _ := set.Lookup(a2)

	require.Equal(t, ResultApplied, s.SyncOnce(context.Background()))
	keep2, _ := set.Lookup(a1)
	replace2, _ := set.Lookup(a2)
	_, hasA3 := set.Lookup(a3)

	require.Same(t, keep, keep2, "unchanged (addr,url) reuses the existing builder")
	require.NotSame(t, replace, replace2, "changed url gets a fresh builder")
	require.True(t, hasA3)
}

// ---------------------------------------------------------------------------
// Config
// ---------------------------------------------------------------------------

func TestConfig_Validate(t *testing.T) {
	require.NoError(t, (&Config{}).Validate(), "disabled config needs nothing")

	c := Config{Enabled: true}
	require.ErrorContains(t, c.Validate(), "ContractAddress")
	c.ContractAddress = a1
	require.ErrorContains(t, c.Validate(), "RPCURL")
	c.RPCURL = "http://localhost:8545"
	require.NoError(t, c.Validate())
	c.BlockTag = "pending"
	require.ErrorContains(t, c.Validate(), "BlockTag")

	require.Equal(t, DefaultPollInterval, (&Config{}).pollInterval())
	require.Equal(t, DefaultBlockTag, (&Config{}).blockTag())
}
