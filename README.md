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
RPCConcurrency = 100 # The maximum number of concurrent requests.
RPCTimeout = "10s" # The timeout for RPC requests.

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
