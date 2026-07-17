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

	// One semaphore across HTTP and gRPC so total in-flight requests respect a
	// single RPCConcurrency budget (separate limits would allow 2x).
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

	// Block until a shutdown signal or an HTTP listener failure, then drain
	// BOTH listeners: gRPC flips health to NOT_SERVING and drains streams,
	// http.Server.Shutdown stops accepting and waits for in-flight JSON-RPC.
	// A rolling restart therefore never kills a bid mid-forward on either path.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	select {
	case sig := <-sigCh:
		log.Infow("received signal, shutting down", "signal", sig.String())
	case err := <-httpErrCh:
		log.Errorf("http server stopped, err:%v", err)
	}

	// Drain window for in-flight requests on shutdown. Covers the default
	// RPCTimeout (10s) with margin while staying well inside the typical k8s
	// termination grace period (30s).
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
