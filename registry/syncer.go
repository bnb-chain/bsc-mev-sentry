package registry

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/bnb-chain/bsc-mev-sentry/log"
	"github.com/bnb-chain/bsc-mev-sentry/metrics"
	"github.com/bnb-chain/bsc-mev-sentry/node"
	"github.com/bnb-chain/bsc-mev-sentry/service"
)

// Result classifies one sync attempt. It is also the metrics label.
type Result string

const (
	ResultApplied   Result = "applied"   // registry changed; allowlist replaced
	ResultUnchanged Result = "unchanged" // same fingerprint as last time
	ResultEmpty     Result = "empty"     // registry decoded to zero builders; applied (allowlist = ExtraBuilders only)
	ResultError     Result = "error"     // read failed; previous allowlist kept
)

// Syncer periodically reads the registry and pushes the merged allowlist to the
// sentry. A failed read never changes the allowlist. A successfully decoded
// registry is authoritative even when empty: an admin removing the last key must
// take effect. Misconfigured addresses do not look like an empty set; they
// surface as ErrNoCode (no code at address) and are treated as failures.
type Syncer struct {
	reader   Reader
	interval time.Duration
	extra    []node.BuilderConfig
	contract common.Address

	builders *service.BuilderSet
	status   *service.RegistryState

	last *Snapshot
}

// NewSyncer wires a reader to the sentry's allowlist and status holder.
func NewSyncer(cfg *Config, reader Reader, builders *service.BuilderSet, status *service.RegistryState) *Syncer {
	return &Syncer{
		reader:   reader,
		interval: cfg.pollInterval(),
		extra:    cfg.ExtraBuilders,
		contract: cfg.ContractAddress,
		builders: builders,
		status:   status,
	}
}

// Run performs an immediate sync, then one per interval until ctx is done.
func (s *Syncer) Run(ctx context.Context) {
	s.SyncOnce(ctx)
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.SyncOnce(ctx)
		}
	}
}

// SyncOnce reads the registry once and applies it if it changed.
func (s *Syncer) SyncOnce(ctx context.Context) Result {
	snap, err := s.reader.Fetch(ctx)
	if err != nil {
		// Full error (may include the node URL) goes to the log only; the public
		// status RPC gets the class.
		log.Warnw("registry sync failed; keeping current allowlist", "err", err)
		s.status.RecordError(ErrorClass(err))
		return s.record(ResultError)
	}
	if s.last != nil && s.last.Fingerprint == snap.Fingerprint {
		s.status.RecordSuccess(snap.BlockNumber, snap.BlockHash, snap.FetchedAt, snap.Fingerprint, s.builders.Len())
		metrics.RegistrySyncedBlock.Set(float64(snap.BlockNumber))
		return s.record(ResultUnchanged)
	}

	effective := Merge(snap.Builders, s.extra)
	s.builders.Replace(effective)

	var prev []Builder
	if s.last != nil {
		prev = s.last.Builders
	}
	added, removed := Diff(prev, snap.Builders)
	result := ResultApplied
	if len(snap.Builders) == 0 {
		// Loud, because it is either a deliberate network-wide shutdown of MEV
		// bids or an admin mistake; either way operators should see it.
		result = ResultEmpty
		log.Warnw("registry is empty; allowlist reduced to ExtraBuilders",
			"block", snap.BlockNumber, "contract", s.contract, "effective", len(effective))
	}
	log.Infow("registry allowlist applied",
		"block", snap.BlockNumber,
		"fingerprint", snap.Fingerprint.TerminalString(),
		"registry", len(snap.Builders),
		"effective", len(effective),
		"added", summarize(added),
		"removed", summarize(removed))

	s.last = snap
	s.status.RecordSuccess(snap.BlockNumber, snap.BlockHash, snap.FetchedAt, snap.Fingerprint, len(effective))
	metrics.RegistrySyncedBlock.Set(float64(snap.BlockNumber))
	metrics.RegistryBuilderCount.Set(float64(len(effective)))
	return s.record(result)
}

func (s *Syncer) record(r Result) Result {
	metrics.RegistrySyncTotal.WithLabelValues(string(r)).Inc()
	if r != ResultError {
		metrics.RegistryLastSuccess.SetToCurrentTime()
	}
	return r
}

// summarize keeps change logs readable: the initial load adds every key at once,
// which is not worth printing address by address.
func summarize(addrs []common.Address) any {
	const maxListed = 10
	if len(addrs) <= maxListed {
		return addrs
	}
	return fmt.Sprintf("%d addresses (first %d: %v)", len(addrs), maxListed, addrs[:maxListed])
}

// ErrorClass maps a fetch error to a short, URL-free label safe to expose over RPC.
func ErrorClass(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrNoCode):
		return "no_code_at_address"
	case errors.Is(err, ErrDecode):
		return "decode_failed"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, ErrResolveBlock):
		return "resolve_block_failed"
	case errors.Is(err, ErrCall):
		return "eth_call_failed"
	default:
		return "fetch_failed"
	}
}
