package storage

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/util"
)

// Storage LevelDB存储接口
type Storage struct {
	db     *leveldb.DB
	dbPath string
	mu     sync.RWMutex
}

// 存储键前缀
const (
	PrefixWallet   = "wallet:"
	PrefixKeyShare = "keyshare:"
	PrefixSession  = "session:"
	PrefixNode     = "node:"
	PrefixTx       = "tx:"
)

// NewStorage 创建新的存储实例
func NewStorage(dataDir string) (*Storage, error) {
	// 确保目录存在
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create data directory: %w", err)
	}

	dbPath := filepath.Join(dataDir, "mpc.db")
	db, err := leveldb.OpenFile(dbPath, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to open leveldb: %w", err)
	}

	return &Storage{
		db:     db,
		dbPath: dbPath,
	}, nil
}

// Close 关闭数据库
func (s *Storage) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.db.Close()
}

// Put 存储键值对
func (s *Storage) Put(key string, value []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.db.Put([]byte(key), value, nil)
}

// Get 获取值
func (s *Storage) Get(key string) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.db.Get([]byte(key), nil)
}

// Delete 删除键值对
func (s *Storage) Delete(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.db.Delete([]byte(key), nil)
}

// Has 检查键是否存在
func (s *Storage) Has(key string) (bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.db.Has([]byte(key), nil)
}

// PutJSON 存储JSON对象
func (s *Storage) PutJSON(key string, value interface{}) error {
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("failed to marshal json: %w", err)
	}
	return s.Put(key, data)
}

// GetJSON 获取JSON对象
func (s *Storage) GetJSON(key string, value interface{}) error {
	data, err := s.Get(key)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, value)
}

// ListByPrefix 列出所有匹配前缀的键
func (s *Storage) ListByPrefix(prefix string) ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var keys []string
	iter := s.db.NewIterator(util.BytesPrefix([]byte(prefix)), nil)
	defer iter.Release()

	for iter.Next() {
		keys = append(keys, string(iter.Key()))
	}

	return keys, iter.Error()
}

// GetAllByPrefix 获取所有匹配前缀的键值对
func (s *Storage) GetAllByPrefix(prefix string) (map[string][]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make(map[string][]byte)
	iter := s.db.NewIterator(util.BytesPrefix([]byte(prefix)), nil)
	defer iter.Release()

	for iter.Next() {
		key := string(iter.Key())
		value := make([]byte, len(iter.Value()))
		copy(value, iter.Value())
		result[key] = value
	}

	return result, iter.Error()
}

// Batch 批量操作
type Batch struct {
	batch *leveldb.Batch
	s     *Storage
}

// NewBatch 创建批量操作
func (s *Storage) NewBatch() *Batch {
	return &Batch{
		batch: new(leveldb.Batch),
		s:     s,
	}
}

// Put 批量添加
func (b *Batch) Put(key string, value []byte) {
	b.batch.Put([]byte(key), value)
}

// Delete 批量删除
func (b *Batch) Delete(key string) {
	b.batch.Delete([]byte(key))
}

// Write 执行批量操作
func (b *Batch) Write() error {
	b.s.mu.Lock()
	defer b.s.mu.Unlock()
	return b.s.db.Write(b.batch, nil)
}

// Reset 重置批量操作
func (b *Batch) Reset() {
	b.batch.Reset()
}
