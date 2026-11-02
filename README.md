# BSC-MEV-Sentry

BSC-MEV-Sentry serves as the proxy service for BSC MEV architecture, It has the following features:

1. Forward RPC requests: mev_sendBid, mev_params, mev_running, mev_bestBidGasFee to validators.
2. Forward RPC request: mev_reportIssue to builders.
3. Pay builders on behalf of validators for their bids.
4. Monitor validators' status and health.

See also: https://github.com/bnb-chain/BEPs/pull/322

For the details of mev_params, here are some notices:

1. The builder can call mev_params to obtain the delayLeftOver and bidSimulationLeftOver time settings, and then call
   the [BidBetterBefore](https://github.com/bnb-chain/bsc/blob/master/common/bidutil/bidutil.go) to calculate the
   deadline for sending the bid.
2. The builder can call mev_params to obtain the gasCeil of the validator, to generate a valid header in the block
   building settlement.
3. The builder can call mev_params to obtain the builderFeeCeil of the validator, to help to decide the builder fee.

# Usage

1. `make build`
2. `.build/sentry -config ./configs/config.toml`

❗❗❗This is an important security notice: Please do not configure any validator's private key here. 
Please create entirely new accounts as pay bid accounts.

config-example.toml:
```
[Service]
HTTPListenAddr = "localhost:8555" # The address to listen on for HTTP requests.
GRPCListenAddr = "" # Optional BidBlock gRPC endpoint. Empty = disabled.
RPCConcurrency = 100 # The maximum number of concurrent requests.
RPCTimeout = "10s" # Total RPC timeout. gRPC uses 10s when omitted.

[[Validators]] # A list of validators to forward requests to.
PrivateURL = "https://bsc-fuji" # The private rpc url of the validator, it can only been accessed in the local network.
PublicHostName = "bsc-fuji" # The domain name of the validator, if a request's HOST info is same with this, it will be forwarded to the validator.
PayAccountMode = "privateKey" # The unlock mode of the pay bid account.
PrivateKey = "59ba8068eb256d520...2bd306e1bd603fdb8c8da10e8" # The private key of the pay bid account.

[[Validators]]
PrivateURL = "https://bsc-mathwallet"
PublicHostName = "bsc-mathwallet"
PayAccountMode = "keystore"
KeystorePath = "./keystore" # The keystore file path of the pay bid account.
PasswordFilePath = "./password.txt" # The path of the pay bid account's password file.
PayAccountAddress = "0x12c86Bf9...845B98F23" # The address of the pay bid account.

[[Validators]]
PrivateURL = "https://bsc-trustwallet"
PublicHostName = "bsc-trustwallet"
PayAccountMode = "privateKey" # The unlock mode of the pay account.
PrivateKey = "59ba8068eb...d306e1bd603fdb8c8da10e8" # The private key of the pay account.

[[Builders]]
Address = "0x45EbEBe8...664D59c12" # The address of the builder.
URL = "http://bsc-builder-1" # The public URL of the builder.

[[Builders]]
Address = "0x980A75eC...fc9b863D5"
URL = "http://bsc-builder-2"

```

# On-chain builder registry (optional)

Instead of maintaining `[[Builders]]` by hand, the sentry can synchronize its allowlist from the
BuilderKeyRegistry contract published by the Good Will Alliance
(`good-will-alliance/contracts/builder-key-registry`). Enable it with a `[Registry]` section:

```toml
[Registry]
Enabled = true
ContractAddress = "0x..."            # registry proxy address on this network
PollInterval = "10m"
# RPCURL = "http://<trusted-node>:8545"  # optional: try this node before the validators

[[Registry.ExtraBuilders]]           # always accepted in addition to the registry set
Address = "0x..."
URL = "http://my-builder"
```

Behavior:

- No separate RPC URL is required. Each poll tries `Validators.PrivateURL` in configuration order,
  starting with the first validator. If `Registry.RPCURL` is set, it is tried first, followed by the
  validators. Duplicate URLs are tried only once. All sources must be trusted nodes on the same chain.
  A dial, read, or decode failure advances to the next source; the first successful snapshot wins.
  Each source gets up to 10 seconds for the complete attempt, so a poll can take up to 10 seconds
  per distinct source when nodes time out. Shutdown cancels the current attempt and stops retries.
- Both the finalized header and contract state are read from the same source.
- At the same finalized block hash, `defaultMaxKeys()` must return a nonzero value.
  This rejects the uninitialized implementation address (`not_a_registry`) without
  rejecting an initialized proxy whose builder list is empty. The check is not proof
  of contract identity; operators must still verify the deployed proxy address.
- The static `[[Builders]]` list is ignored. The HTTP and gRPC service ports open only after the
  first successful read; until then the sentry keeps retrying, and SIGINT/SIGTERM still shut it down.
- Effective allowlist = registry set ∪ `ExtraBuilders`.
- Reads take `getBuilders()` at the finalized block, so an applied change is never rolled back by a
  reorg. If the set changed it is swapped in atomically; requests never see a half-updated allowlist.
- Poll schedule: every `PollInterval` (default 10 minutes) while the registry is non-empty; every
  3 seconds while it is empty; after a failed read, including the first one, retry from 2 seconds
  with exponential backoff up to 1 minute. Neither shorter interval exceeds `PollInterval`.
- If all sources fail, the current allowlist and mode stay in place and a warning is logged.
  An address returning no data fails with `no_code_at_address`; an invalid or zero
  `defaultMaxKeys()` result fails with `not_a_registry`. Neither changes the current state
  or allows the service ports to open before a successful first read.
- An empty registry switches the sentry to allow-all mode: `mev_sendBid`, `mev_sendBidBlock` and
  `mev_hasBuilder` accept any builder address. Signature checks, rate limits and the builder fee
  ceiling still apply. Every empty read is logged at error level and counted under
  `result="empty"`. The last non-empty set is kept so `mev_reportIssue` can still reach builders.
  The next non-empty read switches back to allowlist mode.
- `mev_hasBuilder` answers from the sentry allowlist rather than from the validator node.
- A builder whose issue-reporting URL cannot be dialed is still allowlisted; the connection is retried
  on the next `mev_reportIssue`.

Observability:

- `mev_registryStatus` (read-only RPC) returns the source (`static` or `registry`), the mode
  (`allowlist` or `allow_all`), the block number,
  block hash, fetch time and fingerprint of the applied snapshot, the effective builder count, and the
  class of the last error (`eth_call_failed`, `resolve_block_failed`, `no_code_at_address`,
  `decode_failed`, `not_a_registry`, `timeout`, `fetch_failed`). The full error, which may contain the
  node URL, is only written to the sentry log. Two sentries reporting the same fingerprint hold the same set.
- Prometheus: `bsc_mev_sentry_registry_sync_total{result}`, `bsc_mev_sentry_registry_synced_block`,
  `bsc_mev_sentry_registry_builder_count`, `bsc_mev_sentry_registry_allow_all` (1 in allow-all mode;
  alert on it), `bsc_mev_sentry_registry_last_success_timestamp_seconds`.
