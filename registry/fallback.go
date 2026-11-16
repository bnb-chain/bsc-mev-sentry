package registry

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

// FallbackReader tries sources from the beginning on every Fetch. Each attempt
// resolves and reads a complete snapshot from one node. Like Syncer, it is used
// by a single goroutine; Close must be called after that goroutine stops.
type FallbackReader struct {
	sources []Reader
	timeout time.Duration
}

// NewFallbackReader connects lazily so an unavailable first source cannot stop
// startup or prevent later sources from being tried. RPC clients are reused.
func NewFallbackReader(urls []string, contract common.Address) *FallbackReader {
	r := &FallbackReader{timeout: DefaultFetchTimeout}
	for _, url := range urls {
		r.sources = append(r.sources, &rpcReader{url: url, contract: contract})
	}
	return r
}

func (r *FallbackReader) Fetch(ctx context.Context) (*Snapshot, error) {
	if len(r.sources) == 0 {
		return nil, errors.New("registry: no RPC sources configured")
	}
	var errs []error
	for i, source := range r.sources {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		attemptCtx, cancel := context.WithTimeout(ctx, r.timeout)
		snap, err := source.Fetch(attemptCtx)
		cancel()
		if err != nil {
			errs = append(errs, fmt.Errorf("source %d: %w", i+1, err))
			continue
		}
		return snap, nil
	}
	return nil, fmt.Errorf("registry: all RPC sources failed: %w", errors.Join(errs...))
}

func (r *FallbackReader) Close() {
	for _, source := range r.sources {
		if closer, ok := source.(interface{ Close() }); ok {
			closer.Close()
		}
	}
}

type rpcReader struct {
	url      string
	contract common.Address
	client   *Client
}

func (r *rpcReader) Fetch(ctx context.Context) (*Snapshot, error) {
	if r.client == nil {
		client, err := Dial(ctx, r.url, r.contract)
		if err != nil {
			return nil, err
		}
		r.client = client
	}
	snap, err := r.client.Fetch(ctx)
	if err != nil {
		return nil, fmt.Errorf("registry: read %s: %w", r.url, err)
	}
	return snap, nil
}

func (r *rpcReader) Close() {
	if r.client != nil {
		r.client.Close()
	}
}
