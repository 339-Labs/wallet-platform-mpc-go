package storage

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/339-Labs/wallet-platform-mpc-go/pkg/types"

	"github.com/syndtr/goleveldb/leveldb"
)

// WalletRepository 钱包存储库
type WalletRepository struct {
	storage *Storage
}

// NewWalletRepository 创建钱包存储库
func NewWalletRepository(storage *Storage) *WalletRepository {
	return &WalletRepository{storage: storage}
}

// SaveWallet 保存钱包信息
func (r *WalletRepository) SaveWallet(wallet *types.WalletInfo) error {
	wallet.UpdatedAt = time.Now()
	key := PrefixWallet + wallet.ID
	return r.storage.PutJSON(key, wallet)
}

// GetWallet 获取钱包信息
func (r *WalletRepository) GetWallet(walletID string) (*types.WalletInfo, error) {
	key := PrefixWallet + walletID
	var wallet types.WalletInfo
	err := r.storage.GetJSON(key, &wallet)
	if err != nil {
		if err == leveldb.ErrNotFound {
			return nil, fmt.Errorf("wallet not found: %s", walletID)
		}
		return nil, err
	}
	return &wallet, nil
}

// DeleteWallet 删除钱包
func (r *WalletRepository) DeleteWallet(walletID string) error {
	key := PrefixWallet + walletID
	return r.storage.Delete(key)
}

// ListWallets 列出所有钱包
func (r *WalletRepository) ListWallets() ([]*types.WalletInfo, error) {
	data, err := r.storage.GetAllByPrefix(PrefixWallet)
	if err != nil {
		return nil, err
	}

	var wallets []*types.WalletInfo
	for _, v := range data {
		var wallet types.WalletInfo
		if err := json.Unmarshal(v, &wallet); err != nil {
			continue
		}
		wallets = append(wallets, &wallet)
	}
	return wallets, nil
}

// WalletExists 检查钱包是否存在
func (r *WalletRepository) WalletExists(walletID string) (bool, error) {
	key := PrefixWallet + walletID
	return r.storage.Has(key)
}

// GetWalletByAddress 通过地址获取钱包
func (r *WalletRepository) GetWalletByAddress(address string) (*types.WalletInfo, error) {
	wallets, err := r.ListWallets()
	if err != nil {
		return nil, err
	}
	for _, w := range wallets {
		if w.Address == address {
			return w, nil
		}
	}
	return nil, fmt.Errorf("wallet not found for address: %s", address)
}
