package node

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/rpc"
)

const (
	bidBlockQuotaTTL               = time.Minute
	maxBidBlockQuotaEntries        = 4096
	maxBidBlockQuotaBuilderHeights = 256
)

type bidBlockQuotaKey struct {
	block   uint64
	builder common.Address
}

type bidBlockQuotaEntry struct {
	expires time.Time
	bids    map[common.Hash]bidBlockQuotaBid
}

type bidBlockQuotaBid struct {
	blocked  bool // in-flight, completed, or retry budget exhausted
	attempts uint8
}

type bidBlockQuota struct {
	mu             sync.Mutex
	entries        map[bidBlockQuotaKey]*bidBlockQuotaEntry
	builderHeights map[common.Address]int
	nextSweep      time.Time
}

// Match the validator's generic JSON-RPC duplicate/quota errors on both ingress paths.
type bidBlockQuotaError string

func (e bidBlockQuotaError) Error() string  { return string(e) }
func (e bidBlockQuotaError) ErrorCode() int { return -32000 }

// ReserveBidBlock shares a quota across JSON-RPC and gRPC for this validator.
// Call finish with the forwarding result. Distinct attempts keep their quota
// even on failure; an ambiguous transport failure permits one same-hash retry.
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
				q.forget(key)
			}
		}
		q.nextSweep = now.Add(time.Second)
	}
	key := bidBlockQuotaKey{block, builder}
	entry := q.entries[key]
	if entry != nil && !now.Before(entry.expires) {
		q.forget(key)
		entry = nil
	}
	var bid bidBlockQuotaBid
	exists := false
	if entry != nil {
		bid, exists = entry.bids[hash]
		if bid.blocked {
			return nil, bidBlockQuotaError("bid already exists")
		}
	}
	if limit == 0 || (!exists && entry != nil && uint64(len(entry.bids)) >= uint64(limit)) {
		return nil, bidBlockQuotaError(fmt.Sprintf("too many bids: exceeded limit of %d bids per builder per block", limit))
	}
	if entry == nil {
		// Reject only this builder at its height cap. A full shared cache still
		// defers to the validator rather than rejecting unrelated builders.
		if q.builderHeights[builder] >= maxBidBlockQuotaBuilderHeights {
			return nil, bidBlockQuotaError("too many target heights for builder")
		}
		if len(q.entries) >= maxBidBlockQuotaEntries {
			return func(error) {}, nil
		}
		if q.entries == nil {
			q.entries = make(map[bidBlockQuotaKey]*bidBlockQuotaEntry)
			q.builderHeights = make(map[common.Address]int)
		}
		entry = &bidBlockQuotaEntry{expires: now.Add(bidBlockQuotaTTL), bids: make(map[common.Hash]bidBlockQuotaBid)}
		q.entries[key] = entry
		q.builderHeights[builder]++
	}
	bid.blocked = true
	bid.attempts++
	entry.bids[hash] = bid
	return func(err error) {
		if bid.attempts == 1 && retryableBidBlockError(err) {
			q.mu.Lock()
			defer q.mu.Unlock()
			if q.entries[key] == entry && entry.bids[hash] == bid {
				bid.blocked = false
				entry.bids[hash] = bid
			}
		}
	}, nil
}

func retryableBidBlockError(err error) bool {
	var rpcErr rpc.Error
	if err == nil || errors.As(err, &rpcErr) {
		return false // includes MEV TooLate mapped to gRPC DeadlineExceeded
	}
	if errors.Is(err, errBidBlockTimeout) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	switch status.Code(err) {
	case codes.Unavailable, codes.DeadlineExceeded, codes.Canceled:
		return true
	default:
		return false
	}
}

// forget is called with q.mu held and removes both sides of the accounting.
func (q *bidBlockQuota) forget(key bidBlockQuotaKey) {
	delete(q.entries, key)
	q.builderHeights[key.builder]--
	if q.builderHeights[key.builder] == 0 {
		delete(q.builderHeights, key.builder)
	}
}
