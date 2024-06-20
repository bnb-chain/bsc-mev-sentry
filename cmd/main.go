package main

import (
	"flag"
	"net/http"
	_ "net/http/pprof"

	"github.com/cockroachdb/errors"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/gin-gonic/contrib/gzip"
	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/bnb-chain/bsc-mev-sentry/config"
	ginutils "github.com/bnb-chain/bsc-mev-sentry/gin"
	"github.com/bnb-chain/bsc-mev-sentry/log"
	"github.com/bnb-chain/bsc-mev-sentry/node"
	"github.com/bnb-chain/bsc-mev-sentry/service"
)

const serviceName = "bsc-mev-sentry"

var configPath = flag.String("config", "./configs/config.toml", "mev-sentry config file path")

func init() {
	gin.SetMode(gin.ReleaseMode)
}

func main() {
	defer log.Stop()

	flag.Parse()

	cfg := config.Load(*configPath)
	initLogger(&cfg.Log)

	openPrometheusAndPprof(cfg.Debug.ListenAddr)

	log.Infow("bsc mev-sentry start", "configPath", *configPath,
		"validator_count", len(cfg.Validators), "builder_count", len(cfg.Builders))

	validators := make(map[string]node.Validator)
	for _, v := range cfg.Validators {
		validator := node.NewValidator(v)
		if validator != nil {
			validators[v.PublicHostName] = validator
		}
	}

	builders := make(map[common.Address]node.Builder)
	for _, b := range cfg.Builders {
		builder := node.NewBuilder(b)
		if builder != nil {
			builders[b.Address] = builder
		}
	}

	rpcServer := rpc.NewServer()
	sentryService := service.NewMevSentry(&cfg.Service, validators, builders)
	if err := rpcServer.RegisterName("mev", sentryService); err != nil {
		panic(err)
	}

	app := gin.New()
	app.Use(
		ginutils.ConcurrencyLimiter(cfg.Service.RPCConcurrency),
		ginutils.PanicRecovery(),
		gzip.Gzip(gzip.DefaultCompression),
	)

	app.POST("/", gin.WrapH(rpcServer))

	if err := app.Run(cfg.Service.HTTPListenAddr); err != nil {
		log.Errorf("fail to run rpc server, err:%v", err)
	}
}

func initLogger(cfg *config.LogConfig) {
	lvl, _ := log.ParseLevel(cfg.Level)
	log.Init(lvl, log.StandardizePath(cfg.RootDir, serviceName))
}

func openPrometheusAndPprof(addr string) {
	http.Handle("/debug/metrics/prometheus", promhttp.Handler())
	log.Infof("prometheus and pprof listen on: %v", addr)
	go func() {
		if err := http.ListenAndServe(addr, nil); err != http.ErrServerClosed {
			log.Errorf("failed to serving prometheus and pprof, err:%v", errors.WithStack(err))
		}
	}()
}
