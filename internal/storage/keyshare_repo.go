package storage

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/339-Labs/wallet-platform-mpc-go/pkg/types"

	"github.com/bnb-chain/tss-lib/v2/ecdsa/keygen"
	"github.com/syndtr/goleveldb/leveldb"
)

// KeyShareRepository 密钥分片存储库
type KeyShareRepository struct {
	storage     *Storage
	keyShareDir string
	encryptKey  []byte
}

// NewKeyShareRepository 创建密钥分片存储库
func NewKeyShareRepository(storage *Storage, keyShareDir string, encryptionPassword string) (*KeyShareRepository, error) {
	// 确保目录存在
	if err := os.MkdirAll(keyShareDir, 0700); err != nil {
		return nil, fmt.Errorf("failed to create keyshare directory: %w", err)
	}

	// 从密码派生加密密钥
	hash := sha256.Sum256([]byte(encryptionPassword))

	return &KeyShareRepository{
		storage:     storage,
		keyShareDir: keyShareDir,
		encryptKey:  hash[:],
	}, nil
}

// SaveKeyShare 保存密钥分片（加密存储）
func (r *KeyShareRepository) SaveKeyShare(walletID, partyID string, localData *keygen.LocalPartySaveData) error {
	// 序列化数据
	data, err := json.Marshal(localData)
	if err != nil {
		return fmt.Errorf("failed to marshal key share: %w", err)
	}

	// 加密数据
	encryptedData, err := r.encrypt(data)
	if err != nil {
		return fmt.Errorf("failed to encrypt key share: %w", err)
	}

	// 保存到文件（双重备份）
	filename := fmt.Sprintf("%s_%s.keyshare", walletID, partyID)
	filePath := filepath.Join(r.keyShareDir, filename)
	if err := os.WriteFile(filePath, encryptedData, 0600); err != nil {
		return fmt.Errorf("failed to write key share file: %w", err)
	}

	// 保存元数据到LevelDB
	meta := &types.KeyShare{
		WalletID:  walletID,
		PartyID:   partyID,
		CreatedAt: time.Now(),
	}
	key := PrefixKeyShare + walletID + ":" + partyID
	return r.storage.PutJSON(key, meta)
}

// GetKeyShare 获取密钥分片
func (r *KeyShareRepository) GetKeyShare(walletID, partyID string) (*keygen.LocalPartySaveData, error) {
	// 从文件读取
	filename := fmt.Sprintf("%s_%s.keyshare", walletID, partyID)
	filePath := filepath.Join(r.keyShareDir, filename)

	encryptedData, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read key share file: %w", err)
	}

	// 解密数据
	data, err := r.decrypt(encryptedData)
	if err != nil {
		return nil, fmt.Errorf("failed to decrypt key share: %w", err)
	}

	// 反序列化
	var localData keygen.LocalPartySaveData
	if err := json.Unmarshal(data, &localData); err != nil {
		return nil, fmt.Errorf("failed to unmarshal key share: %w", err)
	}

	return &localData, nil
}

// DeleteKeyShare 删除密钥分片
func (r *KeyShareRepository) DeleteKeyShare(walletID, partyID string) error {
	// 删除文件
	filename := fmt.Sprintf("%s_%s.keyshare", walletID, partyID)
	filePath := filepath.Join(r.keyShareDir, filename)
	if err := os.Remove(filePath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to delete key share file: %w", err)
	}

	// 删除元数据
	key := PrefixKeyShare + walletID + ":" + partyID
	return r.storage.Delete(key)
}

// HasKeyShare 检查密钥分片是否存在
func (r *KeyShareRepository) HasKeyShare(walletID, partyID string) (bool, error) {
	key := PrefixKeyShare + walletID + ":" + partyID
	return r.storage.Has(key)
}

// GetKeyShareMeta 获取密钥分片元数据
func (r *KeyShareRepository) GetKeyShareMeta(walletID, partyID string) (*types.KeyShare, error) {
	key := PrefixKeyShare + walletID + ":" + partyID
	var meta types.KeyShare
	err := r.storage.GetJSON(key, &meta)
	if err != nil {
		if err == leveldb.ErrNotFound {
			return nil, fmt.Errorf("key share not found")
		}
		return nil, err
	}
	return &meta, nil
}

// ListKeySharesByWallet 列出钱包的所有密钥分片
func (r *KeyShareRepository) ListKeySharesByWallet(walletID string) ([]*types.KeyShare, error) {
	prefix := PrefixKeyShare + walletID + ":"
	data, err := r.storage.GetAllByPrefix(prefix)
	if err != nil {
		return nil, err
	}

	var shares []*types.KeyShare
	for _, v := range data {
		var share types.KeyShare
		if err := json.Unmarshal(v, &share); err != nil {
			continue
		}
		shares = append(shares, &share)
	}
	return shares, nil
}

// encrypt 加密数据
func (r *KeyShareRepository) encrypt(plaintext []byte) ([]byte, error) {
	block, err := aes.NewCipher(r.encryptKey)
	if err != nil {
		return nil, err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}

	return gcm.Seal(nonce, nonce, plaintext, nil), nil
}

// decrypt 解密数据
func (r *KeyShareRepository) decrypt(ciphertext []byte) ([]byte, error) {
	block, err := aes.NewCipher(r.encryptKey)
	if err != nil {
		return nil, err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	nonceSize := gcm.NonceSize()
	if len(ciphertext) < nonceSize {
		return nil, fmt.Errorf("ciphertext too short")
	}

	nonce, ciphertext := ciphertext[:nonceSize], ciphertext[nonceSize:]
	return gcm.Open(nil, nonce, ciphertext, nil)
}
