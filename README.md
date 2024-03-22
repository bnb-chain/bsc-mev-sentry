# BSC-MEV-Sentry

BSC-MEV-Sentry serves as the proxy service for BSC MEV architecture, It has the following features:

1. Forward RPC requests under mev namespace to validators/builders.
2. Pay builders for their bid.
3. Monitor validators' status and health.

See also: https://github.com/bnb-chain/BEPs/pull/322

# Usage

Sentry settings are configured in the `config.toml` file. The following is an example of a `config.toml` file:

```
[Service]
HTTPListenAddr = "localhost:8555" # The address to listen on for HTTP requests.
RPCConcurrency = 100 # The maximum number of concurrent requests.
RPCTimeout = "10s" # The timeout for RPC requests.

[[Validators]] # A list of validators to forward requests to.
PrivateURL = "https://bsc-fuji" # The private rpc url of the validator, it can only been accessed in the local network.
PublicHostName = "bsc-fuji" # The domain name of the validator, if a request's HOST info is same with this, it will be forwarded to the validator.
AccountMode = "privateKey" # The unlock mode of the pay bid account.
PrivateKey = "59ba8068eb256d520179e903f43dacf6d8d57d72bd306e1bd603fdb8c8da10e8" # The private key of the pay bid account.

[[Validators]]
PrivateURL = "https://bsc-mathwallet"
PublicHostName = "bsc-mathwallet"
AccountMode = "keystore"
KeystorePath = "./keystore" # The keystore file path of the pay bid account.
PasswordFilePath = "./password.txt" # The path of the pay bid account's password file.
Address = "0x12c86Bf9...845B98F23" # The address of the pay bid account.

[[Validators]]
PrivateURL = "https://bsc-trustwallet"
PublicHostName = "bsc-trustwallet"
AccountMode = "privateKey" # The unlock mode of the pay account.
PrivateKey = "59ba8068eb256d520179e903f43dacf6d8d57d72bd306e1bd603fdb8c8da10e8" # The private key of the pay account.

[[Builders]]
Address = "0x45EbEBe8...664D59c12" # The address of the builder.
URL = "http://bsc-builder-1" # The public URL of the builder.

[[Builders]]
Address = "0x980A75eC...fc9b863D5"
URL = "http://bsc-builder-2"

[ChainRPC]
URL = "https://bsc-dataseed1.bnbchain.org"
```
