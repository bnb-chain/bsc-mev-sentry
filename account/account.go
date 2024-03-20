package account

import (
	"crypto/ecdsa"
	"errors"
	"math/big"
	"os"
	"strings"

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
		return newKeystoreAccount(config.KeystorePath, config.PasswordFilePath, config.Address)
	default:
		return nil, errors.New("invalid pay account mode")
	}
}

type Config struct {
	Mode Mode
	// PrivateKey private key of sentry wallet
	PrivateKey string
	// KeystorePath path of keystore
	KeystorePath string
	// PasswordFilePath stores keystore password
	PasswordFilePath string
	// Address public address of sentry wallet
	Address string
}

type baseAccount struct {
	address common.Address
}

func (a *baseAccount) Address() common.Address {
	return a.address
}

type keystoreAccount struct {
	keystore *keystore.KeyStore
	account  accounts.Account
	*baseAccount
}

func newKeystoreAccount(keystorePath, passwordFilePath, opAccount string) (*keystoreAccount, error) {
	address := common.HexToAddress(opAccount)
	ks := keystore.NewKeyStore(keystorePath, keystore.StandardScryptN, keystore.StandardScryptP)
	account, err := ks.Find(accounts.Account{Address: address})
	if err != nil {
		log.Errorw("failed to create key store account", "err", err)
		return nil, err
	}

	password := MakePasswordFromPath(passwordFilePath)

	err = ks.Unlock(account, password)
	if err != nil {
		log.Errorw("failed to unlock account", "err", err)
		return nil, err
	}

	err = os.Remove(passwordFilePath)
	if err != nil {
		log.Errorw("failed to remove password file", "err", err)
	}

	return &keystoreAccount{ks, account, &baseAccount{address: address}}, nil
}

func (k *keystoreAccount) SignTx(tx *types.Transaction, chainID *big.Int) (*types.Transaction, error) {
	signedTx, err := k.keystore.SignTx(k.account, tx, chainID)
	if err != nil {
		log.Errorw("failed to sign tx", "err", err)
		return nil, err
	}

	return signedTx, nil
}

type privateKeyAccount struct {
	key *ecdsa.PrivateKey
	*baseAccount
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

	return &privateKeyAccount{key, &baseAccount{address: addr}}, nil
}

func (p *privateKeyAccount) SignTx(tx *types.Transaction, chainID *big.Int) (*types.Transaction, error) {
	signedTx, err := types.SignTx(tx, types.LatestSignerForChainID(chainID), p.key)
	if err != nil {
		log.Errorw("failed to sign tx", "err", err)
		return nil, err
	}

	return signedTx, nil
}

func MakePasswordFromPath(path string) string {
	if path == "" {
		return ""
	}
	text, err := os.ReadFile(path)
	if err != nil {
		log.Panicw("failed to read password file: %v", err)
	}
	lines := strings.Split(string(text), "\n")
	if len(lines) == 0 {
		return ""
	}

	return strings.TrimRight(lines[0], "\r")
}
