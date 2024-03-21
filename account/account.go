package account

import (
	"crypto/ecdsa"
	"errors"
	"math/big"

	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/accounts/keystore"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/bnb-chain/bsc-mev-sentry/log"
)

type Mode string

const (
	privateKeyMode Mode = "privateKey"
	keystoreMode   Mode = "keystore"
)

type Account interface {
	Address() common.Address
	SignTx(tx *types.Transaction, chainID *big.Int) (*types.Transaction, error)
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
	address  common.Address
	account  accounts.Account
}

func newKeystoreAccount(keystorePath, password, opAccount string) (*keystoreAccount, error) {
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

	return &keystoreAccount{ks, address, account}, nil
}

func (k *keystoreAccount) SignTx(tx *types.Transaction, chainID *big.Int) (*types.Transaction, error) {
	signedTx, err := k.keystore.SignTx(k.account, tx, chainID)
	if err != nil {
		log.Errorw("failed to sign tx", "err", err)
		return nil, err
	}

	return signedTx, nil
}

func (k *keystoreAccount) Address() common.Address {
	return k.address
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

func (p *privateKeyAccount) SignTx(tx *types.Transaction, chainID *big.Int) (*types.Transaction, error) {
	signedTx, err := types.SignTx(tx, types.LatestSignerForChainID(chainID), p.key)
	if err != nil {
		log.Errorw("failed to sign tx", "err", err)
		return nil, err
	}

	return signedTx, nil
}

func (p *privateKeyAccount) Address() common.Address {
	return p.address
}
