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
	DefaultPollInterval = 10 * time.Minute
	// AllowAllPollInterval applies while the registry is empty, so a refilled
	// registry ends allow-all mode quickly.
	AllowAllPollInterval = 15 * time.Second
	// Failed reads, including the first one at startup, retry with exponential
	// backoff between these bounds.
	MinRetryInterval = 2 * time.Second
	MaxRetryInterval = time.Minute
	// DefaultFetchTimeout bounds one RPC source attempt (dial + header + eth_call).
	DefaultFetchTimeout = 10 * time.Second
)

// Config is the [Registry] section of the sentry config file.
//
// Effective allowlist: registry set ∪ ExtraBuilders. An empty registry switches
// the sentry to allow-all mode. The static [[Builders]] list is not used.
type Config struct {
	// Enabled turns on periodic synchronization. When false the static
	// [[Builders]] list is used exactly as before.
	Enabled bool
	// ContractAddress is the BuilderKeyRegistry proxy address on this network.
	ContractAddress common.Address
	// RPCURL optionally supplies the first node to try before Validators.PrivateURL
	// in configuration order. All sources must be trusted nodes on the same chain.
	RPCURL string
	// PollInterval between reads of a non-empty registry. Zero uses
	// DefaultPollInterval. Reads are always taken at the finalized block, so a
	// change is visible one poll after finality.
	PollInterval service.Duration
	// ExtraBuilders are always accepted in addition to the registry set.
	ExtraBuilders []node.BuilderConfig
}

func (c *Config) Validate() error {
	if !c.Enabled {
		return nil
	}
	if c.ContractAddress == (common.Address{}) {
		return errors.New("registry: ContractAddress is required")
	}
	if c.PollInterval < 0 {
		return errors.New("registry: PollInterval must be positive")
	}
	return nil
}

// RPCURLs returns distinct, non-empty sources in priority order: the optional
// explicit RPCURL, followed by each validator's PrivateURL.
func (c *Config) RPCURLs(validators []node.ValidatorConfig) []string {
	var urls []string
	seen := make(map[string]bool)
	add := func(url string) {
		if url != "" && !seen[url] {
			urls = append(urls, url)
			seen[url] = true
		}
	}
	add(c.RPCURL)
	for _, validator := range validators {
		add(validator.PrivateURL)
	}
	return urls
}

func (c *Config) pollInterval() time.Duration {
	if c.PollInterval <= 0 {
		return DefaultPollInterval
	}
	return time.Duration(c.PollInterval)
}

// EffectivePollInterval is PollInterval with the default applied.
func (c *Config) EffectivePollInterval() time.Duration { return c.pollInterval() }
