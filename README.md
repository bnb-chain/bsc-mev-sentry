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
RPCURL = "http://<validator-node>:8545"  # your own node; a third-party RPC could serve a forged set
PollInterval = "15s"

[[Registry.ExtraBuilders]]           # always accepted in addition to the registry set
Address = "0x..."
URL = "http://my-builder"
```

Behavior:

- Effective allowlist = (registry set, or the static `[[Builders]]` until the first successful read)
  ∪ `ExtraBuilders`.
- Every `PollInterval` the sentry reads `getBuilders()` at the finalized block, so an applied change
  is never rolled back by a reorg; a registry update becomes visible one poll after it is finalized.
  If the set changed it is swapped
  in atomically; requests never see a half-updated allowlist.
- A failed read never changes the allowlist: the previous set stays in place and a warning is logged.
  A successfully decoded registry is authoritative even when it is empty, so removing the last key
  takes effect (the allowlist then contains only `ExtraBuilders`); this is logged as a warning and
  counted under `result="empty"`. A misconfigured `ContractAddress` does not look like an empty
  registry: it fails with `no_code_at_address` and keeps the previous set.
- Before the first successful read, the static `[[Builders]]` list is combined with
  `ExtraBuilders`, so local additions are accepted from process start.
- A builder whose issue-reporting URL cannot be dialed is still allowlisted; the connection is retried
  on the next `mev_reportIssue`.

Observability:

- `mev_registryStatus` (read-only RPC) returns the source (`static` or `registry`), the block number,
  block hash, fetch time and fingerprint of the applied snapshot, the effective builder count, and the
  class of the last error (`eth_call_failed`, `resolve_block_failed`, `no_code_at_address`,
  `decode_failed`, `timeout`, `fetch_failed`). The full error, which may contain the
  node URL, is only written to the sentry log. Two sentries reporting the same fingerprint hold the same set.
- Prometheus: `bsc_mev_sentry_registry_sync_total{result}`, `bsc_mev_sentry_registry_synced_block`,
  `bsc_mev_sentry_registry_builder_count`, `bsc_mev_sentry_registry_last_success_timestamp_seconds`.
