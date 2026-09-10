package registry

import (
	"context"
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
	ResultEmpty     Result = "empty"     // registry returned zero builders; ignored
	ResultError     Result = "error"     // read failed; previous allowlist kept
)

// Syncer periodically reads the registry and pushes the merged allowlist to the
// sentry. It never shrinks the allowlist on failure: errors and empty results
// leave the previous set in place.
type Syncer struct {
	reader   Reader
	interval time.Duration
	extra    []node.BuilderConfig
	blocked  []common.Address
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
		blocked:  cfg.BlockedBuilders,
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
		log.Warnw("registry sync failed; keeping current allowlist", "err", err)
		s.status.RecordError(err)
		return s.record(ResultError)
	}
	if len(snap.Builders) == 0 {
		log.Warnw("registry returned no builders; keeping current allowlist",
			"block", snap.BlockNumber, "contract", s.contract)
		s.status.RecordError(errEmptyRegistry)
		return s.record(ResultEmpty)
	}
	if s.last != nil && s.last.Fingerprint == snap.Fingerprint {
		s.status.RecordSuccess(snap.BlockNumber, snap.BlockHash, snap.FetchedAt, snap.Fingerprint, s.builders.Len())
		return s.record(ResultUnchanged)
	}

	effective := Merge(snap.Builders, s.extra, s.blocked)
	s.builders.Replace(effective)

	var prev []Builder
	if s.last != nil {
		prev = s.last.Builders
	}
	added, removed := Diff(prev, snap.Builders)
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
	return s.record(ResultApplied)
}

func (s *Syncer) record(r Result) Result {
	metrics.RegistrySyncTotal.WithLabelValues(string(r)).Inc()
	if r == ResultApplied || r == ResultUnchanged {
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

type registryError string

func (e registryError) Error() string { return string(e) }

const errEmptyRegistry registryError = "registry returned no builders"
