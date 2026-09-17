package service

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/bnb-chain/bsc-mev-sentry/node"
)

// BuilderSet is the sentry allowlist. Reads are lock-free; Replace swaps the
// whole map atomically so a request never observes a half-updated set.
//
// It is deliberately a separate type from MevSentry: rpc.RegisterName exposes
// every exported method of the registered receiver, so a mutating method on
// MevSentry would become a public RPC.
type BuilderSet struct {
	m atomic.Pointer[map[common.Address]node.Builder]
}

// NewBuilderSet wraps an initial map. Nil is treated as empty.
func NewBuilderSet(initial map[common.Address]node.Builder) *BuilderSet {
	if initial == nil {
		initial = map[common.Address]node.Builder{}
	}
	s := &BuilderSet{}
	s.m.Store(&initial)
	return s
}

// Lookup returns the builder for addr and whether it is allowlisted.
func (s *BuilderSet) Lookup(addr common.Address) (node.Builder, bool) {
	b, ok := (*s.m.Load())[addr]
	return b, ok
}

// Len returns the number of allowlisted keys.
func (s *BuilderSet) Len() int {
	return len(*s.m.Load())
}

// Addresses returns a copy of the allowlisted addresses (unordered).
func (s *BuilderSet) Addresses() []common.Address {
	cur := *s.m.Load()
	out := make([]common.Address, 0, len(cur))
	for a := range cur {
		out = append(out, a)
	}
	return out
}

// Replace installs a new allowlist built from configs. Builders whose address
// and URL are unchanged are carried over so their connections survive the swap.
func (s *BuilderSet) Replace(configs []node.BuilderConfig) {
	cur := *s.m.Load()
	next := make(map[common.Address]node.Builder, len(configs))
	for _, cfg := range configs {
		if existing, ok := cur[cfg.Address]; ok {
			if c, hasCfg := existing.(node.BuilderConfigurer); hasCfg && c.Config() == cfg {
				next[cfg.Address] = existing
				continue
			}
		}
		next[cfg.Address] = node.NewBuilder(cfg)
	}
	s.m.Store(&next)
}

// RegistryStatus is the read-only view exposed as mev_registryStatus. Operators
// compare Fingerprint and BlockNumber across sentries to confirm convergence.
type RegistryStatus struct {
	Enabled      bool           `json:"enabled"`
	Source       string         `json:"source"` // "static" until the first successful registry read, then "registry"
	Contract     common.Address `json:"contract,omitempty"`
	BlockNumber  uint64         `json:"blockNumber,omitempty"`
	BlockHash    common.Hash    `json:"blockHash,omitempty"`
	FetchedAt    time.Time      `json:"fetchedAt,omitempty"`
	Fingerprint  common.Hash    `json:"fingerprint,omitempty"`
	BuilderCount int            `json:"builderCount"`
	// LastError is a short class such as "eth_call_failed", never the raw error:
	// raw RPC errors embed the node URL, which must not reach builders.
	LastError   string    `json:"lastError,omitempty"`
	LastErrorAt time.Time `json:"lastErrorAt,omitempty"`
}

// RegistryState holds the mutable status behind RegistryStatus. Like BuilderSet
// it lives outside MevSentry so its setters are not exposed over RPC.
type RegistryState struct {
	mu sync.RWMutex
	st RegistryStatus
}

// NewRegistryState returns a state for a disabled or not-yet-synced registry.
func NewRegistryState(enabled bool, contract common.Address, staticCount int) *RegistryState {
	return &RegistryState{st: RegistryStatus{
		Enabled:      enabled,
		Source:       "static",
		Contract:     contract,
		BuilderCount: staticCount,
	}}
}

func (r *RegistryState) RecordSuccess(block uint64, hash common.Hash, at time.Time, fp common.Hash, count int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.st.Source = "registry"
	r.st.BlockNumber = block
	r.st.BlockHash = hash
	r.st.FetchedAt = at
	r.st.Fingerprint = fp
	r.st.BuilderCount = count
	// A successful read supersedes earlier failures; error history lives in metrics.
	r.st.LastError = ""
	r.st.LastErrorAt = time.Time{}
}

// RecordError stores an error class (see registry.ErrorClass). Callers must not
// pass raw error text.
func (r *RegistryState) RecordError(class string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.st.LastError = class
	r.st.LastErrorAt = time.Now()
}

// Snapshot returns a copy of the current status.
func (r *RegistryState) Snapshot() RegistryStatus {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.st
}
