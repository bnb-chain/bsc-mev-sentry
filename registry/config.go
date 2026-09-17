// Package registry synchronizes the sentry builder allowlist from the on-chain
// BuilderKeyRegistry contract (good-will-alliance/contracts/builder-key-registry).
package registry

import (
	"errors"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/bnb-chain/bsc-mev-sentry/node"
	"github.com/bnb-chain/bsc-mev-sentry/service"
)

const (
	DefaultPollInterval = 15 * time.Second
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
	// PollInterval between reads. Zero uses DefaultPollInterval. Reads are always
	// taken at the finalized block, so a change is visible one poll after finality.
	PollInterval service.Duration
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

// EffectivePollInterval is PollInterval with the default applied.
func (c *Config) EffectivePollInterval() time.Duration { return c.pollInterval() }
