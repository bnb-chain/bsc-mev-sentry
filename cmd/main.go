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
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
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
