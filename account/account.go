package account

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"math/big"

	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/accounts/keystore"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/bnb-chain/bsc-mev-sentry/log"
	"github.com/bnb-chain/bsc-mev-sentry/node"
	"github.com/bnb-chain/bsc-mev-sentry/syncutils"
)

type Mode string

const (
	privateKeyMode Mode = "privateKey"
	keystoreMode   Mode = "keystore"
)

type Account interface {
	PayBidTx(context.Context, common.Address, *big.Int) ([]byte, error)
}

func New(config *Config, fullNode node.FullNode) (Account, error) {
	switch config.Mode {
	case privateKeyMode:
		return newPrivateKeyAccount(config.PrivateKey, fullNode)
	case keystoreMode:
		return newKeystoreAccount(config.KeystorePath, config.Password, config.Address, fullNode)
	default:
		return nil, errors.New("invalid account mode")
	}
}

type Config struct {
	Mode Mode
	// PrivateKey private key of sentry wallet
	PrivateKey string
	// KeystorePath path of keystore
	KeystorePath string
	// Password keystore password
	Password string
	// Address public address of sentry wallet
	Address string
}

type keystoreAccount struct {
	keystore *keystore.KeyStore
	address  common.Address
	account  accounts.Account
	fullNode node.FullNode
}

func newKeystoreAccount(keystorePath, password, opAccount string, fullNode node.FullNode) (*keystoreAccount, error) {
	address := common.HexToAddress(opAccount)
	ks := keystore.NewKeyStore(keystorePath, keystore.StandardScryptN, keystore.StandardScryptP)
	account, err := ks.Find(accounts.Account{Address: address})
	if err != nil {
		log.Errorw("failed to create key store account", "err", err)
		return nil, err
	}

	err = ks.Unlock(account, password)
	if err != nil {
		log.Errorw("failed to unlock account", "err", err)
		return nil, err
	}

	return &keystoreAccount{ks, address, account, fullNode}, nil
}

func (k *keystoreAccount) PayBidTx(ctx context.Context, receiver common.Address, amount *big.Int) ([]byte, error) {
	// take pay bid tx as block tag
	if amount == nil {
		amount = big.NewInt(0)
	}

	balance, nonce, err := prev(ctx, k.fullNode, k.address)
	if err != nil {
		return nil, err
	}

	if balance.Cmp(amount) < 0 {
		log.Errorw("insufficient balance", "balance", balance, "amount", amount)
		return nil, errors.New("insufficient balance")
	}

	tx := types.NewTx(&types.LegacyTx{
		Nonce:    nonce,
		GasPrice: big.NewInt(0),
		Gas:      25000,
		To:       &receiver,
		Value:    amount,
	})

	signedTx, err := k.keystore.SignTx(k.account, tx, k.fullNode.ChainID())
	if err != nil {
		log.Errorw("failed to sign tx", "err", err)
		return nil, err
	}

	return signedTx.MarshalBinary()
}

type privateKeyAccount struct {
	key      *ecdsa.PrivateKey
	address  common.Address
	fullNode node.FullNode
}

func newPrivateKeyAccount(privateKey string, fullNode node.FullNode) (*privateKeyAccount, error) {
	key, err := crypto.HexToECDSA(privateKey)
	if err != nil {
		log.Errorw("failed to load private key", "err", err)
		return nil, err
	}

	pubKey := key.Public()
	pubKeyECDSA, ok := pubKey.(*ecdsa.PublicKey)
	if !ok {
		log.Errorw("public key is not *ecdsa.PublicKey", "err", err)
		return nil, err
	}

	addr := crypto.PubkeyToAddress(*pubKeyECDSA)

	return &privateKeyAccount{key, addr, fullNode}, nil
}

func (p *privateKeyAccount) PayBidTx(ctx context.Context, receiver common.Address, amount *big.Int) ([]byte, error) {
	// take pay bid tx as block tag
	if amount == nil {
		amount = big.NewInt(0)
	}

	balance, nonce, err := prev(ctx, p.fullNode, p.address)
	if err != nil {
		return nil, err
	}

	if balance.Cmp(amount) < 0 {
		log.Errorw("insufficient balance", "balance", balance, "amount", amount)
		return nil, errors.New("insufficient balance")
	}

	tx := types.NewTx(&types.LegacyTx{
		Nonce:    nonce,
		GasPrice: big.NewInt(0),
		Gas:      25000,
		To:       &receiver,
		Value:    amount,
	})

	signedTx, err := types.SignTx(tx, types.LatestSignerForChainID(p.fullNode.ChainID()), p.key)
	if err != nil {
		log.Errorw("failed to sign tx", "err", err)
		return nil, err
	}

	return signedTx.MarshalBinary()
}

func prev(ctx context.Context, fullNode node.FullNode, address common.Address) (balance *big.Int, nonce uint64, err error) {
	err = syncutils.BatchRun(
		func() error {
			var er error
			balance, er = fullNode.Balance(ctx, address)
			if er != nil {
				return er
			}

			return nil
		},
		func() error {
			var er error
			nonce, er = fullNode.PendingNonceAt(ctx, address)
			if er != nil {
				return er
			}

			return nil
		})

	if err != nil {
		log.Errorw("failed to query sentry balance or nonce", "err", err)
	}

	return
}
