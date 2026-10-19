package registry

import (
	"bytes"
	"sort"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/bnb-chain/bsc-mev-sentry/node"
)

// Builder mirrors the contract's Builder struct. The abi tags bind the tuple
// components ("addr", "url", "name") when unpacking eth_call results.
type Builder struct {
	Addr common.Address `abi:"addr"`
	URL  string         `abi:"url"`
	Name string         `abi:"name"`
}

// Snapshot is one successful read of the registry at a specific block.
type Snapshot struct {
	Builders    []Builder
	BlockNumber uint64
	BlockHash   common.Hash
	FetchedAt   time.Time
	// Fingerprint is keccak256 over the sorted (addr, url, name) rows. Two
	// sentries that report the same fingerprint hold the same set.
	Fingerprint common.Hash
}

// Fingerprint computes a deterministic digest of a builder list.
func Fingerprint(builders []Builder) common.Hash {
	sorted := make([]Builder, len(builders))
	copy(sorted, builders)
	sort.Slice(sorted, func(i, j int) bool {
		return bytes.Compare(sorted[i].Addr.Bytes(), sorted[j].Addr.Bytes()) < 0
	})
	var buf bytes.Buffer
	for _, b := range sorted {
		buf.Write(b.Addr.Bytes())
		buf.WriteByte('|')
		buf.WriteString(b.URL)
		buf.WriteByte('|')
		buf.WriteString(b.Name)
		buf.WriteByte('\n')
	}
	return crypto.Keccak256Hash(buf.Bytes())
}

// Merge derives the effective allowlist from every registry entry plus
// ExtraBuilders not already present. The result is sorted by address so callers
// can diff it.
func Merge(base []Builder, extra []node.BuilderConfig) []node.BuilderConfig {
	out := make(map[common.Address]node.BuilderConfig, len(base)+len(extra))
	for _, b := range base {
		out[b.Addr] = node.BuilderConfig{Address: b.Addr, URL: b.URL}
	}
	for _, e := range extra {
		if _, exists := out[e.Address]; exists {
			continue // registry entry wins over the local copy
		}
		out[e.Address] = e
	}
	list := make([]node.BuilderConfig, 0, len(out))
	for _, cfg := range out {
		list = append(list, cfg)
	}
	sort.Slice(list, func(i, j int) bool {
		return bytes.Compare(list[i].Address.Bytes(), list[j].Address.Bytes()) < 0
	})
	return list
}

// Bootstrap derives the allowlist to serve before the first successful registry
// read: the static [[Builders]] plus ExtraBuilders, so local additions are
// accepted from process start.
func Bootstrap(cfg *Config, static []node.BuilderConfig) []node.BuilderConfig {
	base := make([]Builder, 0, len(static))
	for _, s := range static {
		base = append(base, Builder{Addr: s.Address, URL: s.URL})
	}
	return Merge(base, cfg.ExtraBuilders)
}

// Diff returns addresses present in next but not prev, and vice versa.
func Diff(prev, next []Builder) (added, removed []common.Address) {
	p := make(map[common.Address]struct{}, len(prev))
	for _, b := range prev {
		p[b.Addr] = struct{}{}
	}
	n := make(map[common.Address]struct{}, len(next))
	for _, b := range next {
		n[b.Addr] = struct{}{}
		if _, ok := p[b.Addr]; !ok {
			added = append(added, b.Addr)
		}
	}
	for _, b := range prev {
		if _, ok := n[b.Addr]; !ok {
			removed = append(removed, b.Addr)
		}
	}
	return added, removed
}
