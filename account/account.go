package account

import (
	"crypto/ecdsa"
	"errors"
	"math/big"

	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/accounts/keystore"
	"github.com/ethereum/go-ethereum/cmd/utils"
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
	SetBalance(balance *big.Int)
	GetBalance() *big.Int
	SetNonce(nonce uint64)
	GetNonce() uint64
}

func New(config *Config) (Account, error) {
	switch config.Mode {
	case privateKeyMode:
		return newPrivateKeyAccount(config.PrivateKey)
	case keystoreMode:
		return newKeystoreAccount(config.KeystorePath, config.PasswordFilePath, config.Address)
	default:
		return nil, errors.New("invalid baseAccount mode")
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
	balance *big.Int
	nonce   uint64
	address common.Address
}

func (a *baseAccount) Address() common.Address {
	return a.address
}

func (a *baseAccount) SetBalance(balance *big.Int) {
	a.balance = balance
}

func (a *baseAccount) GetBalance() *big.Int {
	return a.balance
}

func (a *baseAccount) SetNonce(nonce uint64) {
	a.nonce = nonce
}

func (a *baseAccount) GetNonce() uint64 {
	return a.nonce
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
		log.Errorw("failed to create key store baseAccount", "err", err)
		return nil, err
	}

	passwords := utils.MakePasswordListFromPath(passwordFilePath)

	var unlock bool
	for _, password := range passwords {
		err = ks.Unlock(account, password)
		if err == nil {
			unlock = true
			break
		}
	}

	if !unlock {
		log.Errorw("failed to unlock baseAccount", "err", err)
		return nil, err
	}

	return &keystoreAccount{ks, account, &baseAccount{address: address}}, nil
}

func (k *keystoreAccount) SignTx(tx *types.Transaction, chainID *big.Int) (*types.Transaction, error) {
	signedTx, err := k.keystore.SignTx(k.account, tx, chainID)
	if err != nil {
		log.Errorw("failed to sign tx", "err", err)
		return nil, err
	}

	log.Infow("pay bid tx signed", "tx", signedTx.Hash().Hex())

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
	signedTx, err := types.SignTx(tx, types.NewLondonSigner(chainID), p.key)
	if err != nil {
		log.Errorw("failed to sign tx", "err", err)
		return nil, err
	}

	log.Infow("pay bid tx signed", "tx", signedTx.Hash().Hex())

	return signedTx, nil
}
