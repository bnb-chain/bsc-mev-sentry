package registry

import (
	"context"
	"errors"
	"fmt"
	"sync"
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
	ResultUnchanged Result = "unchanged" // same non-empty fingerprint as last time
	ResultEmpty     Result = "empty"     // registry decoded to zero builders; allow-all mode
	ResultError     Result = "error"     // read failed; previous allowlist and mode kept
)

// Syncer periodically reads the registry and pushes the merged allowlist to the
// sentry. A failed read never changes the allowlist or the mode. A non-empty
// registry selects allowlist mode; an empty one selects allow-all mode, which
// keeps the last non-empty allowlist for builder URLs. Misconfigured addresses
// surface as ErrNoCode (no code at address) and are treated as failures.
type Syncer struct {
	reader   Reader
	interval time.Duration
	extra    []node.BuilderConfig
	contract common.Address

	builders *service.BuilderSet
	status   *service.RegistryState

	last   *Snapshot
	listed []Builder // last non-empty registry set
	retry  time.Duration

	ready     chan struct{}
	readyOnce sync.Once
}

// NewSyncer wires a reader to the sentry's allowlist and status holder.
func NewSyncer(cfg *Config, reader Reader, builders *service.BuilderSet, status *service.RegistryState) *Syncer {
	s := &Syncer{
		reader:   reader,
		interval: cfg.pollInterval(),
		extra:    cfg.ExtraBuilders,
		contract: cfg.ContractAddress,
		builders: builders,
		status:   status,
		ready:    make(chan struct{}),
	}
	s.retry = s.minRetry()
	return s
}

// Ready is closed after the first successful read, empty or not.
func (s *Syncer) Ready() <-chan struct{} { return s.ready }

// Run syncs immediately, then schedules each next read by the last result
// until ctx is done.
func (s *Syncer) Run(ctx context.Context) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			timer.Reset(s.nextDelay(s.SyncOnce(ctx)))
		}
	}
}

// nextDelay backs off after failures, polls quickly in allow-all mode, and
// otherwise waits the configured interval.
func (s *Syncer) nextDelay(r Result) time.Duration {
	if r == ResultError {
		d := s.retry
		s.retry = min(2*s.retry, min(MaxRetryInterval, s.interval))
		return d
	}
	s.retry = s.minRetry()
	if s.builders.AllowAll() {
		return min(AllowAllPollInterval, s.interval)
	}
	return s.interval
}

func (s *Syncer) minRetry() time.Duration { return min(MinRetryInterval, s.interval) }

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

	var result Result
	switch {
	case len(snap.Builders) == 0:
		s.applyEmpty(snap)
		result = ResultEmpty
	case s.last != nil && s.last.Fingerprint == snap.Fingerprint:
		result = ResultUnchanged
	default:
		s.applyList(snap)
		result = ResultApplied
	}
	s.last = snap

	s.status.RecordSuccess(snap.BlockNumber, snap.BlockHash, snap.FetchedAt, snap.Fingerprint, s.builders.Len())
	metrics.RegistrySyncedBlock.Set(float64(snap.BlockNumber))
	metrics.RegistryBuilderCount.Set(float64(s.builders.Len()))
	s.readyOnce.Do(func() { close(s.ready) })
	return s.record(result)
}

// applyEmpty enters or stays in allow-all mode. Logged at error level on every
// read, since either all MEV keys were removed on purpose or the registry was
// emptied by mistake, and builder addresses are unchecked meanwhile.
func (s *Syncer) applyEmpty(snap *Snapshot) {
	entered := s.builders.SetAllowAll(true)
	metrics.RegistryAllowAll.Set(1)
	if entered && s.listed == nil {
		// No non-empty set seen yet: ExtraBuilders still need URLs for mev_reportIssue.
		s.builders.Replace(Merge(nil, s.extra))
	}
	msg := "registry is empty; allow-all mode continues, builder addresses are not checked"
	if entered {
		msg = "registry is empty; entering allow-all mode, builder addresses are not checked"
	}
	log.Errorw(msg, "block", snap.BlockNumber, "contract", s.contract)
}

// applyList installs a non-empty registry set and leaves allow-all mode.
func (s *Syncer) applyList(snap *Snapshot) {
	effective := Merge(snap.Builders, s.extra)
	s.builders.Replace(effective)
	if s.builders.SetAllowAll(false) {
		log.Warnw("registry is non-empty; leaving allow-all mode",
			"block", snap.BlockNumber, "contract", s.contract)
	}
	metrics.RegistryAllowAll.Set(0)

	added, removed := Diff(s.listed, snap.Builders)
	log.Infow("registry allowlist applied",
		"block", snap.BlockNumber,
		"fingerprint", snap.Fingerprint.TerminalString(),
		"registry", len(snap.Builders),
		"effective", len(effective),
		"added", summarize(added),
		"removed", summarize(removed))
	s.listed = snap.Builders
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
