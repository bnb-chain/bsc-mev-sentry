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
HTTPListenAddr = "localhost:8555"
RPCConcurrency = 10
RPCTimeout = "10s"

[Account]
Mode = "keystore"
KeystorePath = "./keystore"
Password = "sentry"
Address = "0x837060bd423eFcDd5B7b6B92aB3CFc74B9CD0df4"

[[Validators]]
PrivateURL = "https://bsc-fuji"
PublicHostName = "bsc-fuji"

[[Validators]]
PrivateURL = "https://bsc-mathwallet"
PublicHostName = "bsc-mathwallet"

[[Validators]]
PrivateURL = "https://bsc-trustwallet"
PublicHostName = "bsc-trustwallet"

[[Builders]]
Address = "0x837060bd423eFcDd5B7b6B92aB3CFc74B9CD0df4"
URL = "http://x.x.x.x:8546"

[Chain]
URL = "http://x.x.x.x:8547"
```

- `Validators`: A list of validators to send bid for.
    - `PrivateURL`: The rpc url of the validator.
    - `PublicHostName`: The domain name of the validator.
