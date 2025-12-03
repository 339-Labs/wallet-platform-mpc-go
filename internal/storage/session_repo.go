package storage

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/339-Labs/wallet-platform-mpc-go/pkg/types"

	"github.com/syndtr/goleveldb/leveldb"
)

// SessionRepository 会话存储库
type SessionRepository struct {
	storage *Storage
}

// NewSessionRepository 创建会话存储库
func NewSessionRepository(storage *Storage) *SessionRepository {
	return &SessionRepository{storage: storage}
}

// SaveSession 保存会话
func (r *SessionRepository) SaveSession(session *types.SessionState) error {
	session.UpdatedAt = time.Now()
	key := PrefixSession + session.ID
	return r.storage.PutJSON(key, session)
}

// GetSession 获取会话
func (r *SessionRepository) GetSession(sessionID string) (*types.SessionState, error) {
	key := PrefixSession + sessionID
	var session types.SessionState
	err := r.storage.GetJSON(key, &session)
	if err != nil {
		if err == leveldb.ErrNotFound {
			return nil, fmt.Errorf("session not found: %s", sessionID)
		}
		return nil, err
	}
	return &session, nil
}

// DeleteSession 删除会话
func (r *SessionRepository) DeleteSession(sessionID string) error {
	key := PrefixSession + sessionID
	return r.storage.Delete(key)
}

// UpdateSessionStatus 更新会话状态
func (r *SessionRepository) UpdateSessionStatus(sessionID, status string) error {
	session, err := r.GetSession(sessionID)
	if err != nil {
		return err
	}
	session.Status = status
	return r.SaveSession(session)
}

// UpdateSessionError 更新会话错误
func (r *SessionRepository) UpdateSessionError(sessionID, errMsg string) error {
	session, err := r.GetSession(sessionID)
	if err != nil {
		return err
	}
	session.Status = "failed"
	session.Error = errMsg
	return r.SaveSession(session)
}

// ListActiveSessions 列出所有活动会话
func (r *SessionRepository) ListActiveSessions() ([]*types.SessionState, error) {
	data, err := r.storage.GetAllByPrefix(PrefixSession)
	if err != nil {
		return nil, err
	}

	var sessions []*types.SessionState
	for _, v := range data {
		var session types.SessionState
		if err := json.Unmarshal(v, &session); err != nil {
			continue
		}
		if session.Status == "pending" || session.Status == "running" {
			sessions = append(sessions, &session)
		}
	}
	return sessions, nil
}

// ListSessionsByWallet 列出钱包的所有会话
func (r *SessionRepository) ListSessionsByWallet(walletID string) ([]*types.SessionState, error) {
	data, err := r.storage.GetAllByPrefix(PrefixSession)
	if err != nil {
		return nil, err
	}

	var sessions []*types.SessionState
	for _, v := range data {
		var session types.SessionState
		if err := json.Unmarshal(v, &session); err != nil {
			continue
		}
		if session.WalletID == walletID {
			sessions = append(sessions, &session)
		}
	}
	return sessions, nil
}

// CleanupExpiredSessions 清理过期会话
func (r *SessionRepository) CleanupExpiredSessions(maxAge time.Duration) error {
	data, err := r.storage.GetAllByPrefix(PrefixSession)
	if err != nil {
		return err
	}

	batch := r.storage.NewBatch()
	for key, v := range data {
		var session types.SessionState
		if err := json.Unmarshal(v, &session); err != nil {
			continue
		}
		// 清理已完成或失败且过期的会话
		if (session.Status == "completed" || session.Status == "failed") &&
			time.Since(session.UpdatedAt) > maxAge {
			batch.Delete(key)
		}
	}
	return batch.Write()
}
