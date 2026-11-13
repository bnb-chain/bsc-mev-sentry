package node

import (
	"fmt"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	buildertypes "github.com/ethereum/go-ethereum/core/types/builder"
)

const (
	bidBlockQuotaTTL        = time.Minute
	maxBidBlockQuotaEntries = 4096
)

type bidBlockQuotaKey struct {
	block   uint64
	builder common.Address
}

type bidBlockQuotaEntry struct {
	expires time.Time
	// True means in-flight or successful. Failed attempts can retry the same
	// hash without consuming another slot; an RPC failure does not prove that
	// the validator did not receive the bid.
	bids map[common.Hash]bool
}

type bidBlockQuota struct {
	mu        sync.Mutex
	entries   map[bidBlockQuotaKey]*bidBlockQuotaEntry
	nextSweep time.Time
}

// Match the validator's generic JSON-RPC duplicate/quota errors on both ingress paths.
type bidBlockQuotaError string

func (e bidBlockQuotaError) Error() string  { return string(e) }
func (e bidBlockQuotaError) ErrorCode() int { return -32000 }

// ReserveBidBlock shares a quota across JSON-RPC and gRPC for this validator.
// Call finish with the forwarding result. Distinct attempts keep their quota
// even on failure, but the same failed hash remains retryable.
func (n *validator) ReserveBidBlock(block uint64, builder common.Address, hash common.Hash) (finish func(error), err error) {
	params := n.mevParams.Load()
	if params == nil {
		// The existing async refresh may not have completed yet. Do not invent
		// a limit that could reject valid bids; the validator still enforces it.
		return func(error) {}, nil
	}
	return n.bidBlocks.reserve(params.MaxBidsPerBuilder, block, builder, hash, time.Now())
}

func (q *bidBlockQuota) reserve(limit uint32, block uint64, builder common.Address, hash common.Hash, now time.Time) (func(error), error) {
	q.mu.Lock()
	defer q.mu.Unlock()

	if !now.Before(q.nextSweep) {
		for key, entry := range q.entries {
			if !now.Before(entry.expires) {
				delete(q.entries, key)
			}
		}
		q.nextSweep = now.Add(time.Second)
	}
	key := bidBlockQuotaKey{block, builder}
	entry := q.entries[key]
	if entry != nil && !now.Before(entry.expires) {
		delete(q.entries, key)
		entry = nil
	}
	if entry != nil && entry.bids[hash] {
		return nil, bidBlockQuotaError("bid already exists")
	}
	exists := false
	if entry != nil {
		_, exists = entry.bids[hash]
	}
	if limit == 0 || (!exists && entry != nil && uint64(len(entry.bids)) >= uint64(limit)) {
		return nil, bidBlockQuotaError(fmt.Sprintf("too many bids: exceeded limit of %d bids per builder per block", limit))
	}
	if entry == nil {
		// Never evict live records to make room: that would reset their quota.
		if len(q.entries) >= maxBidBlockQuotaEntries {
			return nil, buildertypes.ErrMevBusy
		}
		if q.entries == nil {
			q.entries = make(map[bidBlockQuotaKey]*bidBlockQuotaEntry)
		}
		entry = &bidBlockQuotaEntry{expires: now.Add(bidBlockQuotaTTL), bids: make(map[common.Hash]bool)}
		q.entries[key] = entry
	}
	entry.bids[hash] = true
	return func(err error) {
		if err != nil {
			q.mu.Lock()
			defer q.mu.Unlock()
			if q.entries[key] == entry {
				entry.bids[hash] = false
			}
		}
	}, nil
}
