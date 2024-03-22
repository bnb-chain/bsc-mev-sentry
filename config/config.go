package config

import (
	"bufio"
	"fmt"
	"os"
	"reflect"
	"unicode"

	"github.com/naoina/toml"

	"github.com/bnb-chain/bsc-mev-sentry/node"
	"github.com/bnb-chain/bsc-mev-sentry/service"
)

type Config struct {
	Service    service.Config
	Validators []node.ValidatorConfig
	Builders   []node.BuilderConfig
	ChainRPC   node.ChainRPCConfig

	Debug DebugConfig
	Log   LogConfig
}

func Load(file string) *Config {
	f, err := os.Open(file)
	if err != nil {
		panic(err)
	}

	defer f.Close()

	cfg := defaultConfig
	err = tomlSettings.NewDecoder(bufio.NewReader(f)).Decode(&cfg)
	// Add file name to errors that have a line number.
	if _, ok := err.(*toml.LineError); ok {
		panic(err)
	}
	return &cfg
}

// TomlSettings - These settings ensure that TOML keys use the same names as Go struct fields.
var tomlSettings = toml.Config{
	NormFieldName: func(rt reflect.Type, key string) string {
		return key
	},
	FieldToKey: func(rt reflect.Type, field string) string {
		return field
	},
	MissingField: func(rt reflect.Type, field string) error {
		link := ""
		if unicode.IsUpper(rune(rt.Name()[0])) && rt.PkgPath() != "main" {
			link = fmt.Sprintf(", see https://godoc.org/%s#%s for available fields", rt.PkgPath(), rt.Name())
		}
		_, _ = fmt.Fprintf(os.Stderr, "field '%s' is not defined in %s%s\n", field, rt.String(), link)
		return nil
	},
}

type DebugConfig struct {
	ListenAddr string
}

type LogConfig struct {
	RootDir string
	Level   string
}

var defaultConfig = Config{
	Debug: DebugConfig{
		ListenAddr: ":6060",
	},
	Log: LogConfig{
		RootDir: "./logs",
		Level:   "debug",
	},
}
