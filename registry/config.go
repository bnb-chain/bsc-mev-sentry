// Package registry synchronizes the sentry builder allowlist from the on-chain
// BuilderKeyRegistry contract (good-will-alliance/contracts/builder-key-registry).
package registry

import (
	"errors"
	"fmt"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/rpc"

	"github.com/bnb-chain/bsc-mev-sentry/node"
	"github.com/bnb-chain/bsc-mev-sentry/service"
)

const (
	DefaultPollInterval = 15 * time.Second
	DefaultBlockTag     = "finalized"
	// DefaultFetchTimeout bounds one registry read (header + eth_call).
	DefaultFetchTimeout = 10 * time.Second
)

// Config is the [Registry] section of the sentry config file.
//
// Effective allowlist:
//
//	(registry set, or the static [[Builders]] until the first successful read)
//	∪ ExtraBuilders − BlockedBuilders
type Config struct {
	// Enabled turns on periodic synchronization. When false the static
	// [[Builders]] list is used exactly as before.
	Enabled bool
	// ContractAddress is the BuilderKeyRegistry proxy address on this network.
	ContractAddress common.Address
	// RPCURL is the node used for eth_call. It should be the validator's own
	// node: a third-party RPC could serve a forged builder set.
	RPCURL string
	// PollInterval between reads. Zero uses DefaultPollInterval.
	PollInterval service.Duration
	// BlockTag is "finalized" (default), "safe" or "latest".
	BlockTag string
	// ExtraBuilders are always accepted in addition to the registry set.
	ExtraBuilders []node.BuilderConfig
	// BlockedBuilders are always rejected, even if present in the registry.
	BlockedBuilders []common.Address
}

func (c *Config) Validate() error {
	if !c.Enabled {
		return nil
	}
	if c.ContractAddress == (common.Address{}) {
		return errors.New("registry: ContractAddress is required")
	}
	if c.RPCURL == "" {
		return errors.New("registry: RPCURL is required")
	}
	if _, err := ParseBlockTag(c.blockTag()); err != nil {
		return err
	}
	if c.PollInterval < 0 {
		return errors.New("registry: PollInterval must be positive")
	}
	return nil
}

func (c *Config) pollInterval() time.Duration {
	if c.PollInterval <= 0 {
		return DefaultPollInterval
	}
	return time.Duration(c.PollInterval)
}

func (c *Config) blockTag() string {
	if c.BlockTag == "" {
		return DefaultBlockTag
	}
	return c.BlockTag
}

// ParseBlockTag accepts "latest", "safe" and "finalized".
func ParseBlockTag(tag string) (rpc.BlockNumber, error) {
	switch tag {
	case "latest":
		return rpc.LatestBlockNumber, nil
	case "safe":
		return rpc.SafeBlockNumber, nil
	case "finalized":
		return rpc.FinalizedBlockNumber, nil
	default:
		return 0, fmt.Errorf("registry: unsupported BlockTag %q (want latest, safe or finalized)", tag)
	}
}
