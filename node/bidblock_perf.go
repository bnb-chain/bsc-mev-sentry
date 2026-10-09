package node

import (
	"encoding/json"
	"time"

	buildertypes "github.com/ethereum/go-ethereum/core/types/builder"
)

// The RPC client invokes MarshalJSON exactly where it normally encodes args.
// No separate pre-encoding pass or change to the JSON-RPC envelope is needed.
// Timing and bytes cover only the args object, not the envelope/outer scanner.
type measuredBidBlockArgs struct {
	args  buildertypes.BidBlockArgs
	stats *encodeStats
}

type encodeStats struct {
	elapsed time.Duration
	bytes   int
}

func (a measuredBidBlockArgs) MarshalJSON() ([]byte, error) {
	start := time.Now()
	data, err := json.Marshal(a.args)
	a.stats.elapsed = time.Since(start)
	a.stats.bytes = len(data)
	return data, err
}
