package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLoadRegistryRPCSources(t *testing.T) {
	const validators = `
[[Validators]]
PrivateURL = "http://validator-1:8545"
[[Validators]]
PrivateURL = "http://validator-2:8545"
[[Validators]]
PrivateURL = "http://validator-1:8545"
`
	const registry = `
[Registry]
Enabled = true
ContractAddress = "0x0000000000000000000000000000000000000001"
`
	for _, tc := range []struct {
		name string
		toml string
		want []string
	}{
		{"validators only", validators + registry, []string{"http://validator-1:8545", "http://validator-2:8545"}},
		{"explicit source first", validators + registry + `RPCURL = "http://registry-node:8545"`, []string{"http://registry-node:8545", "http://validator-1:8545", "http://validator-2:8545"}},
		{"duplicate explicit source", validators + registry + `RPCURL = "http://validator-2:8545"`, []string{"http://validator-2:8545", "http://validator-1:8545"}},
		{"explicit source only", registry + `RPCURL = "http://registry-node:8545"`, []string{"http://registry-node:8545"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			require.NoError(t, os.WriteFile(path, []byte(tc.toml), 0600))
			cfg := Load(path)
			require.Equal(t, tc.want, cfg.Registry.RPCURLs(cfg.Validators))
		})
	}
}

func TestLoadRegistryRequiresSourceOnlyWhenEnabled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(path, []byte(`[Registry]
Enabled = true
ContractAddress = "0x0000000000000000000000000000000000000001"
`), 0600))
	require.PanicsWithValue(t, "registry: configure at least one Validators.PrivateURL or Registry.RPCURL", func() { Load(path) })
	require.NoError(t, os.WriteFile(path, []byte("[Registry]\nEnabled = false\n"), 0600))
	require.NotPanics(t, func() { Load(path) })
}
