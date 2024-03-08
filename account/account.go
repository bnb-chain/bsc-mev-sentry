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
)

type Mode string

const (
	privateKeyMode Mode = "privateKey"
	keystoreMode   Mode = "keystore"
)

type Account interface {
	PayBidTx(context.Context, node.FullNode, common.Address, *big.Int) ([]byte, error)
}

func New(config *Config) (Account, error) {
	switch config.Mode {
	case privateKeyMode:
		return newPrivateKeyAccount(config.PrivateKey)
	case keystoreMode:
		return newKeystoreAccount(config.KeystorePath, config.Password, config.Address)
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
	account  accounts.Account
}

func newKeystoreAccount(keystorePath, password, opAccount string) (*keystoreAccount, error) {
	ks := keystore.NewKeyStore(keystorePath, keystore.StandardScryptN, keystore.StandardScryptP)
	account, err := ks.Find(accounts.Account{Address: common.HexToAddress(opAccount)})
	if err != nil {
		log.Errorw("failed to create key store account", "err", err)
		return nil, err
	}

	err = ks.Unlock(account, password)
	if err != nil {
		log.Errorw("failed to unlock account", "err", err)
		return nil, err
	}

	return &keystoreAccount{ks, account}, nil
}

func (k *keystoreAccount) PayBidTx(ctx context.Context, fullNode node.FullNode, receiver common.Address, amount *big.Int) ([]byte, error) {
	nonce, err := fetchNonce(ctx, fullNode, k.account.Address)
	if err != nil {
		return nil, err
	}

	tx := types.NewTx(&types.LegacyTx{
		Nonce:    nonce,
		GasPrice: big.NewInt(0),
		Gas:      25000,
		To:       &receiver,
		Value:    amount,
	})

	signedTx, err := k.keystore.SignTx(k.account, tx, fullNode.ChainID())
	if err != nil {
		log.Errorw("failed to sign tx", "err", err)
		return nil, err
	}

	return signedTx.MarshalBinary()
}

type privateKeyAccount struct {
	key     *ecdsa.PrivateKey
	address common.Address
}

func newPrivateKeyAccount(privateKey string) (*privateKeyAccount, error) {
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

	return &privateKeyAccount{key, addr}, nil
}

func (p *privateKeyAccount) PayBidTx(ctx context.Context, fullNode node.FullNode, receiver common.Address, amount *big.Int) ([]byte, error) {
	nonce, err := fetchNonce(ctx, fullNode, p.address)
	if err != nil {
		return nil, err
	}

	tx := types.NewTx(&types.LegacyTx{
		Nonce:    nonce,
		GasPrice: big.NewInt(0),
		Gas:      25000,
		To:       &receiver,
		Value:    amount,
	})

	signedTx, err := types.SignTx(tx, types.LatestSignerForChainID(fullNode.ChainID()), p.key)
	if err != nil {
		log.Errorw("failed to sign tx", "err", err)
		return nil, err
	}

	return signedTx.MarshalBinary()
}

func fetchNonce(ctx context.Context, fullNode node.FullNode, address common.Address) (uint64, error) {
	nonce, err := fullNode.PendingNonceAt(ctx, address)
	if err != nil {
		log.Errorw("failed to get nonce", "err", err)
		return 0, err
	}

	return nonce, err
}
