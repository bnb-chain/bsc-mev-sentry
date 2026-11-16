package main

import (
	"context"
	"flag"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/gin-gonic/contrib/gzip"
	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/rpc"

	"github.com/bnb-chain/bsc-mev-sentry/config"
	ginutils "github.com/bnb-chain/bsc-mev-sentry/gin"
	"github.com/bnb-chain/bsc-mev-sentry/log"
	"github.com/bnb-chain/bsc-mev-sentry/node"
	"github.com/bnb-chain/bsc-mev-sentry/registry"
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

	// Static allowlist from [[Builders]], used only when registry sync is disabled.
	builders := make(map[common.Address]node.Builder)
	if !cfg.Registry.Enabled {
		for _, b := range cfg.Builders {
			builders[b.Address] = node.NewBuilder(b)
		}
	} else if len(cfg.Builders) > 0 {
		log.Warnw("[[Builders]] is ignored while registry sync is enabled", "count", len(cfg.Builders))
	}
	builderSet := service.NewBuilderSet(builders)
	registryState := service.NewRegistryState(cfg.Registry.Enabled, cfg.Registry.ContractAddress, len(builders))

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	rootCtx, cancelRoot := context.WithCancel(context.Background())
	var registryWG sync.WaitGroup
	defer func() {
		cancelRoot()
		registryWG.Wait()
	}()
	if cfg.Registry.Enabled {
		rpcURLs := cfg.Registry.RPCURLs(cfg.Validators)
		reader := registry.NewFallbackReader(rpcURLs, cfg.Registry.ContractAddress)
		log.Infow("registry sync enabled",
			"contract", cfg.Registry.ContractAddress,
			"rpcSources", rpcURLs,
			"pollInterval", cfg.Registry.EffectivePollInterval().String(),
			"extra", len(cfg.Registry.ExtraBuilders))
		syncer := registry.NewSyncer(&cfg.Registry, reader, builderSet, registryState)
		registryWG.Add(1)
		go func() {
			defer registryWG.Done()
			defer reader.Close()
			syncer.Run(rootCtx)
		}()

		// Service ports stay closed until the allowlist comes from the registry.
		log.Infow("waiting for the first registry read before opening service ports")
		select {
		case <-syncer.Ready():
			log.Infow("first registry read succeeded; opening service ports")
		case sig := <-sigCh:
			log.Infow("received signal before the first registry read, shutting down", "signal", sig.String())
			return
		}
	}

	rpcServer := rpc.NewServer()
	sentryService := service.NewMevSentryWithSet(&cfg.Service, validators, builderSet, registryState)
	if err := rpcServer.RegisterName("mev", sentryService); err != nil {
		panic(err)
	}

	// Share one concurrency budget across HTTP and gRPC.
	concurrencySem := ginutils.NewConcurrencySem(cfg.Service.RPCConcurrency)

	var grpcService *service.GRPCService
	if cfg.Service.GRPCListenAddr != "" {
		var err error
		grpcService, err = service.StartGRPCServer(cfg.Service.GRPCListenAddr, sentryService, concurrencySem)
		if err != nil {
			panic(err)
		}
	}

	app := gin.New()
	app.Use(
		ginutils.ConcurrencyLimiterWith(concurrencySem),
		ginutils.PanicRecovery(),
		gzip.Gzip(gzip.DefaultCompression),
	)

	app.POST("/", gin.WrapH(rpcServer))

	httpServer := &http.Server{Addr: cfg.Service.HTTPListenAddr, Handler: app}
	httpErrCh := make(chan error, 1)
	go func() {
		httpErrCh <- httpServer.ListenAndServe()
	}()

	// Drain both listeners on a signal or HTTP failure.
	select {
	case sig := <-sigCh:
		log.Infow("received signal, shutting down", "signal", sig.String())
	case err := <-httpErrCh:
		log.Errorf("http server stopped, err:%v", err)
	}

	// Covers the default 10s RPC timeout within a typical 30s pod grace period.
	const drainTimeout = 15 * time.Second
	var wg sync.WaitGroup
	if grpcService != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			grpcService.Shutdown(drainTimeout)
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), drainTimeout)
		defer cancel()
		if err := httpServer.Shutdown(ctx); err != nil {
			log.Errorf("http server shutdown, err:%v", err)
		}
	}()
	wg.Wait()
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
