package node

import (
	"context"
	"crypto/tls"
	"errors"
	"math/big"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/go-co-op/gocron"

	"github.com/bnb-chain/bsc-mev-sentry/account"
	"github.com/bnb-chain/bsc-mev-sentry/log"
	"github.com/bnb-chain/bsc-mev-sentry/metrics"
)

var (
	PayBidTxGasUsed = uint64(25000)

	dialer = &net.Dialer{
		Timeout:   5 * time.Second,
		KeepAlive: 60 * time.Second,
	}

	transport = &http.Transport{
		DialContext:         dialer.DialContext,
		MaxIdleConnsPerHost: 50,
		MaxConnsPerHost:     50,
		IdleConnTimeout:     90 * time.Second,
		TLSClientConfig:     &tls.Config{InsecureSkipVerify: true},
	}

	client = &http.Client{
		Timeout:   5 * time.Second,
		Transport: transport,
	}
)

type Validator interface {
	SendBid(context.Context, types.BidArgs) (common.Hash, error)
	MevRunning() bool
	HasBuilder(ctx context.Context, builder common.Address) (bool, error)
	BestBidGasFee(ctx context.Context, parentHash common.Hash) (*big.Int, error)
	MevParams(ctx context.Context) (*types.MevParams, error)
	BuilderFeeCeil() *big.Int
	GeneratePayBidTx(ctx context.Context, builder common.Address, builderFee *big.Int) (hexutil.Bytes, error)
}

type ValidatorConfig struct {
	PrivateURL     string
	PublicHostName string

	PayAccountMode account.Mode
	// PrivateKey private key of sentry wallet
	PrivateKey string
	// KeystorePath path of keystore
	KeystorePath string
	// PasswordFilePath stores keystore password
	PasswordFilePath string
	// PayAccountAddress public address of sentry wallet
	PayAccountAddress string
}

func NewValidator(config ValidatorConfig) Validator {
	cli, err := ethclient.DialOptions(context.Background(), config.PrivateURL, rpc.WithHTTPClient(client))
	if err != nil {
		log.Errorw("failed to dial validator", "url", config.PrivateURL, "err", err)
		return nil
	}

	acc, err := account.New(&account.Config{
		Mode:             config.PayAccountMode,
		PrivateKey:       config.PrivateKey,
		KeystorePath:     config.KeystorePath,
		PasswordFilePath: config.PasswordFilePath,
		Address:          config.PayAccountAddress})
	if err != nil {
		log.Panicw("failed to create payAccount", "err", err)
	}

	v := &validator{
		cfg:        config,
		client:     cli,
		scheduler:  gocron.NewScheduler(time.UTC),
		payAccount: acc,
	}

	if _, err := v.scheduler.Every(500).Milliseconds().Do(func() {
		v.refresh()
	}); err != nil {
		log.Debugw("error while setting up scheduler", "err", err)
	}

	v.scheduler.StartAsync()

	return v
}

type validator struct {
	cfg        ValidatorConfig
	client     *ethclient.Client
	payAccount account.Account

	scheduler         *gocron.Scheduler
	chainID           atomic.Pointer[big.Int]
	mevRunning        uint32
	mevParams         atomic.Pointer[types.MevParams]
	payAccountBalance atomic.Pointer[big.Int]
	payAccountNonce   uint64
}

func (n *validator) SendBid(ctx context.Context, args types.BidArgs) (common.Hash, error) {
	hash, err := n.client.SendBid(ctx, args)
	if err != nil {
		metrics.ChainError.Inc()
		log.Errorw("failed to send bid", "err", err)

		if strings.Contains(err.Error(), "timeout") {
			err = errors.New("timeout when send bid to validator")
		}
	}

	return hash, err
}

func (n *validator) MevRunning() bool {
	return atomic.LoadUint32(&n.mevRunning) == 1
}

func (n *validator) HasBuilder(ctx context.Context, builder common.Address) (bool, error) {
	has, err := n.client.HasBuilder(ctx, builder)
	if err != nil {
		metrics.ChainError.Inc()
		log.Errorw("failed to check if has builder", "err", err)

		if strings.Contains(err.Error(), "timeout") {
			err = errors.New("timeout when check if has builder")
		}
	}

	return has, err
}

func (n *validator) refresh() {
	chainID, err := n.client.ChainID(context.Background())
	if err != nil {
		metrics.ChainError.Inc()
		log.Errorw("failed to fetch chainID", "url", n.cfg.PrivateURL, "err", err)
	}

	if chainID != nil {
		n.chainID.Store(chainID)
	}

	mevRunning, err := n.client.MevRunning(context.Background())
	if err != nil {
		metrics.ChainError.Inc()
		log.Errorw("failed to fetch mev running status", "url", n.cfg.PrivateURL, "err", err)
	}

	if mevRunning {
		atomic.StoreUint32(&n.mevRunning, 1)
	} else {
		atomic.StoreUint32(&n.mevRunning, 0)
	}

	balance, err := n.client.BalanceAt(context.Background(), n.payAccount.Address(), nil)
	if err != nil {
		metrics.ChainError.Inc()
		log.Errorw("failed to fetch validator payAccount balance", "err", err)
	}

	if balance != nil {
		n.payAccountBalance.Store(balance)
	}

	nonce, err := n.client.NonceAt(context.Background(), n.payAccount.Address(), nil)
	if err != nil {
		metrics.ChainError.Inc()
		log.Errorw("failed to fetch validator payAccount nonce", "err", err)
	}

	log.Infow("refresh payAccount nonce", "address", n.payAccount.Address(), "nonce", nonce)

	atomic.StoreUint64(&n.payAccountNonce, nonce)

	params, err := n.client.MevParams(context.Background())
	if err != nil {
		metrics.ChainError.Inc()
		log.Errorw("failed to fetch validator mev params", "err", err)
	}

	if params != nil {
		n.mevParams.Store(params)
	}
}

func (n *validator) BestBidGasFee(ctx context.Context, parentHash common.Hash) (*big.Int, error) {
	return n.client.BestBidGasFee(ctx, parentHash)
}

func (n *validator) MevParams(_ context.Context) (*types.MevParams, error) {
	return n.mevParams.Load(), nil
}

func (n *validator) BuilderFeeCeil() *big.Int {
	params := n.mevParams.Load()
	if params != nil {
		return params.BuilderFeeCeil
	}

	log.Errorw("mev params is nil, return 0 for BuilderFeeCeil", "validator", n.cfg.PublicHostName)

	return big.NewInt(0)
}

func (n *validator) GeneratePayBidTx(_ context.Context, builder common.Address, builderFee *big.Int) (hexutil.Bytes, error) {
	// take pay bid tx as block tag
	var amount = big.NewInt(0)

	if builderFee != nil {
		amount = builderFee
	}

	if n.payAccountBalance.Load().Cmp(amount) < 0 {
		metrics.AccountError.WithLabelValues(n.payAccount.Address().String(), "insufficient_balance").Inc()
		log.Errorw("insufficient balance", "balance", n.payAccountBalance.Load().String(),
			"builderFee", builderFee.String())
		return nil, errors.New("insufficient balance")
	}

	tx := types.NewTx(&types.LegacyTx{
		Nonce:    atomic.LoadUint64(&n.payAccountNonce),
		GasPrice: big.NewInt(0),
		Gas:      PayBidTxGasUsed,
		To:       &builder,
		Value:    amount,
	})

	signedTx, err := n.payAccount.SignTx(tx, n.chainID.Load())
	if err != nil {
		log.Errorw("failed to sign pay bid tx", "err", err)
		return nil, err
	}

	payBidTx, err := signedTx.MarshalBinary()
	if err != nil {
		log.Errorw("failed to marshal pay bid tx", "err", err)
		return nil, err
	}

	return payBidTx, nil
}
