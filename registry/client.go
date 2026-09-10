package registry

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
)

// Only the read method the sentry needs. Hand-written instead of abigen output so
// the sentry does not depend on generated code for a single view call.
const builderRegistryABI = `[{
  "type": "function", "name": "getBuilders", "stateMutability": "view", "inputs": [],
  "outputs": [{"name": "", "type": "tuple[]", "components": [
    {"name": "addr", "type": "address"},
    {"name": "url",  "type": "string"},
    {"name": "name", "type": "string"}
  ]}]
}]`

var registryABI = func() abi.ABI {
	parsed, err := abi.JSON(strings.NewReader(builderRegistryABI))
	if err != nil {
		panic("registry: invalid embedded ABI: " + err.Error())
	}
	return parsed
}()

// Reader fetches the current builder set. Client implements it against a node;
// tests substitute fakes.
type Reader interface {
	Fetch(ctx context.Context) (*Snapshot, error)
}

// ChainBackend is the subset of ethclient.Client the reader uses.
type ChainBackend interface {
	HeaderByNumber(ctx context.Context, number *big.Int) (*headerLite, error)
	CallContractAtHash(ctx context.Context, msg ethereum.CallMsg, blockHash common.Hash) ([]byte, error)
}

// headerLite is what we need from a block header.
type headerLite struct {
	Number *big.Int
	Hash   common.Hash
}

// Client reads BuilderKeyRegistry.getBuilders() at a block tag.
type Client struct {
	backend  ChainBackend
	contract common.Address
	blockTag rpc.BlockNumber
	timeout  time.Duration
}

// Dial connects to rpcURL and returns a Client for the registry at contract.
func Dial(ctx context.Context, rpcURL string, contract common.Address, blockTag string) (*Client, error) {
	tag, err := ParseBlockTag(blockTag)
	if err != nil {
		return nil, err
	}
	ec, err := ethclient.DialContext(ctx, rpcURL)
	if err != nil {
		return nil, fmt.Errorf("registry: dial %s: %w", rpcURL, err)
	}
	return NewClient(&ethBackend{ec}, contract, tag), nil
}

// NewClient wraps an existing backend.
func NewClient(backend ChainBackend, contract common.Address, blockTag rpc.BlockNumber) *Client {
	return &Client{backend: backend, contract: contract, blockTag: blockTag, timeout: DefaultFetchTimeout}
}

// Fetch resolves the block tag to a concrete block, then calls getBuilders()
// pinned to that block's hash so the number, hash and set are consistent.
func (c *Client) Fetch(ctx context.Context) (*Snapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	header, err := c.backend.HeaderByNumber(ctx, big.NewInt(c.blockTag.Int64()))
	if err != nil {
		return nil, fmt.Errorf("registry: resolve block tag: %w", err)
	}
	data, err := registryABI.Pack("getBuilders")
	if err != nil {
		return nil, err
	}
	out, err := c.backend.CallContractAtHash(ctx, ethereum.CallMsg{To: &c.contract, Data: data}, header.Hash)
	if err != nil {
		return nil, fmt.Errorf("registry: eth_call getBuilders: %w", err)
	}
	if len(out) == 0 {
		// An address with no code returns empty output rather than an error.
		return nil, errors.New("registry: empty eth_call result; is ContractAddress correct on this network?")
	}
	var builders []Builder
	if err := registryABI.UnpackIntoInterface(&builders, "getBuilders", out); err != nil {
		return nil, fmt.Errorf("registry: decode getBuilders: %w", err)
	}
	return &Snapshot{
		Builders:    builders,
		BlockNumber: header.Number.Uint64(),
		BlockHash:   header.Hash,
		FetchedAt:   time.Now(),
		Fingerprint: Fingerprint(builders),
	}, nil
}

// ethBackend adapts *ethclient.Client to ChainBackend.
type ethBackend struct{ ec *ethclient.Client }

func (b *ethBackend) HeaderByNumber(ctx context.Context, number *big.Int) (*headerLite, error) {
	h, err := b.ec.HeaderByNumber(ctx, number)
	if err != nil {
		return nil, err
	}
	return &headerLite{Number: h.Number, Hash: h.Hash()}, nil
}

func (b *ethBackend) CallContractAtHash(ctx context.Context, msg ethereum.CallMsg, blockHash common.Hash) ([]byte, error) {
	return b.ec.CallContractAtHash(ctx, msg, blockHash)
}
