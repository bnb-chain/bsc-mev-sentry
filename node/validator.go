package node

import (
	"context"
	"crypto/tls"
	"errors"
	"math/big"
	"net"
	"net/http"
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
	dialer = &net.Dialer{
		Timeout:   time.Second,
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
	BestBidGasFee(ctx context.Context, parentHash common.Hash) (*big.Int, error)
	MevParams(ctx context.Context) (*types.MevParams, error)
	BidFeeCeil() uint64
	GeneratePayBidTx(ctx context.Context, builder common.Address, builderFee *big.Int) (hexutil.Bytes, error)
}

type ValidatorConfig struct {
	PrivateURL     string
	PublicHostName string
	BidFeeCeil     uint64

	AccountMode account.Mode
	// PrivateKey private key of sentry wallet
	PrivateKey string
	// KeystorePath path of keystore
	KeystorePath string
	// PasswordFilePath stores keystore password
	PasswordFilePath string
	// AccountAddress public address of sentry wallet
	AccountAddress string
}

func NewValidator(config *ValidatorConfig) Validator {
	cli, err := ethclient.DialOptions(context.Background(), config.PrivateURL, rpc.WithHTTPClient(client))
	if err != nil {
		log.Errorw("failed to dial validator", "url", config.PrivateURL, "err", err)
		return nil
	}

	acc, err := account.New(&account.Config{
		Mode:             config.AccountMode,
		PrivateKey:       config.PrivateKey,
		KeystorePath:     config.KeystorePath,
		PasswordFilePath: config.PasswordFilePath,
		Address:          config.AccountAddress})
	if err != nil {
		log.Panicw("failed to create account", "err", err)
	}

	v := &validator{
		cfg:       config,
		client:    cli,
		scheduler: gocron.NewScheduler(time.UTC),
		account:   acc,
	}

	if _, err := v.scheduler.Every(30).Second().Do(func() {
		v.refresh()
	}); err != nil {
		log.Debugw("error while setting up scheduler", "err", err)
	}

	v.scheduler.StartAsync()

	return v
}

type validator struct {
	cfg     *ValidatorConfig
	client  *ethclient.Client
	account account.Account

	scheduler  *gocron.Scheduler
	mevRunning bool
}

func (n *validator) SendBid(ctx context.Context, args types.BidArgs) (common.Hash, error) {
	return n.client.SendBid(ctx, args)
}

func (n *validator) MevRunning() bool {
	return n.mevRunning
}

func (n *validator) refresh() {
	mevRunning, err := n.client.MevRunning(context.Background())
	if err != nil {
		metrics.ChainError.Inc()
		log.Errorw("failed to fetch mev running status", "err", err)
	}

	n.mevRunning = mevRunning

	balance, err := n.client.BalanceAt(context.Background(), n.account.Address(), nil)
	if err != nil {
		metrics.ChainError.Inc()
		log.Errorw("failed to fetch validator account balance", "err", err)
	}

	n.account.SetBalance(balance)

	nonce, err := n.client.PendingNonceAt(context.Background(), n.account.Address())
	if err != nil {
		metrics.ChainError.Inc()
		log.Errorw("failed to fetch validator account nonce", "err", err)
	}

	n.account.SetNonce(nonce)
}

func (n *validator) BestBidGasFee(ctx context.Context, parentHash common.Hash) (*big.Int, error) {
	return n.client.BestBidGasFee(ctx, parentHash)
}

func (n *validator) MevParams(ctx context.Context) (*types.MevParams, error) {
	return n.client.MevParams(ctx)
}

func (n *validator) BidFeeCeil() uint64 {
	return n.cfg.BidFeeCeil
}

func (n *validator) GeneratePayBidTx(_ context.Context, builder common.Address, builderFee *big.Int) (hexutil.Bytes, error) {
	// take pay bid tx as block tag
	var (
		amount  = big.NewInt(0)
		balance = n.account.GetBalance()
		nonce   = n.account.GetNonce()
	)

	if builderFee != nil {
		amount = builderFee
	}

	if balance.Cmp(amount) < 0 {
		metrics.AccountError.WithLabelValues(n.account.Address().String(), "insufficient_balance").Inc()
		log.Errorw("insufficient balance", "balance", balance.Uint64(), "builderFee", builderFee.Uint64())
		return nil, errors.New("insufficient balance")
	}

	tx := types.NewTx(&types.LegacyTx{
		Nonce:    nonce,
		GasPrice: big.NewInt(0),
		Gas:      25000,
		To:       &builder,
		Value:    amount,
	})

	signedTx, err := n.account.SignTx(tx, amount)
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
