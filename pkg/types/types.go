package types

import (
	"math/big"
	"time"

	"github.com/bnb-chain/tss-lib/v2/ecdsa/keygen"
)

// PartyID 表示TSS参与方的唯一标识
type PartyID struct {
	ID      string   `json:"id"`
	Moniker string   `json:"moniker"`
	Key     *big.Int `json:"key"`
	Index   int      `json:"index"`
}

// KeygenRequest 密钥生成请求
type KeygenRequest struct {
	SessionID  string   `json:"session_id,omitempty"` // 会话ID（可选，由协调器设置）
	WalletID   string   `json:"wallet_id"`
	WalletName string   `json:"wallet_name,omitempty"` // 钱包名称
	Threshold  int      `json:"threshold"`             // 签名所需的最少节点数
	TotalParts int      `json:"total_parts"`           // 总参与方数量
	PartyIDs   []string `json:"party_ids"`             // 参与方ID列表
}

// KeygenResult 密钥生成结果
type KeygenResult struct {
	WalletID   string    `json:"wallet_id"`
	PublicKey  string    `json:"public_key"`
	Address    string    `json:"address"`
	Threshold  int       `json:"threshold"`
	TotalParts int       `json:"total_parts"`
	CreatedAt  time.Time `json:"created_at"`
}

// SignRequest 签名请求
type SignRequest struct {
	WalletID  string   `json:"wallet_id"`
	Message   string   `json:"message"`   // hex编码的消息哈希
	PartyIDs  []string `json:"party_ids"` // 参与签名的节点ID列表
	RequestID string   `json:"request_id"`
}

// SignResult 签名结果
type SignResult struct {
	RequestID string `json:"request_id"`
	Signature string `json:"signature"` // hex编码的签名
	R         string `json:"r"`
	S         string `json:"s"`
	V         int    `json:"v"`
}

// ResharingRequest 密钥重分享请求
type ResharingRequest struct {
	WalletID     string   `json:"wallet_id"`
	OldThreshold int      `json:"old_threshold"`
	OldPartyIDs  []string `json:"old_party_ids"`
	NewThreshold int      `json:"new_threshold"`
	NewPartyIDs  []string `json:"new_party_ids"`
}

// ResharingResult 重分享结果
type ResharingResult struct {
	WalletID     string    `json:"wallet_id"`
	NewThreshold int       `json:"new_threshold"`
	NewPartyIDs  []string  `json:"new_party_ids"`
	Address      string    `json:"address"`
	CompletedAt  time.Time `json:"completed_at"`
}

// WalletInfo 钱包信息
type WalletInfo struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Address    string    `json:"address"`
	PublicKey  string    `json:"public_key"`
	Threshold  int       `json:"threshold"`
	TotalParts int       `json:"total_parts"`
	PartyIDs   []string  `json:"party_ids"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// KeyShare 密钥分片（存储在本地）
type KeyShare struct {
	WalletID  string                     `json:"wallet_id"`
	PartyID   string                     `json:"party_id"`
	LocalData *keygen.LocalPartySaveData `json:"local_data"`
	CreatedAt time.Time                  `json:"created_at"`
}

// TransactionRequest 交易请求
type TransactionRequest struct {
	WalletID string   `json:"wallet_id"`
	To       string   `json:"to"`
	Value    string   `json:"value"` // wei单位
	Data     string   `json:"data"`  // hex编码
	Nonce    uint64   `json:"nonce"`
	GasPrice string   `json:"gas_price"`
	GasLimit uint64   `json:"gas_limit"`
	ChainID  int64    `json:"chain_id"`
	PartyIDs []string `json:"party_ids"`
}

// P2PMessage P2P消息结构
type P2PMessage struct {
	Type      string `json:"type"` // keygen, sign, broadcast
	SessionID string `json:"session_id"`
	From      string `json:"from"`
	To        string `json:"to"` // 空表示广播
	Round     int    `json:"round"`
	Data      []byte `json:"data"`
	Timestamp int64  `json:"timestamp"`
}

// NodeInfo 节点信息
type NodeInfo struct {
	ID        string    `json:"id"`
	Address   string    `json:"address"` // P2P地址
	PublicKey string    `json:"public_key"`
	Status    string    `json:"status"` // online, offline
	LastSeen  time.Time `json:"last_seen"`
}

// SessionState 会话状态
type SessionState struct {
	ID           string            `json:"id"`
	Type         string            `json:"type"` // keygen, sign
	WalletID     string            `json:"wallet_id"`
	Status       string            `json:"status"` // pending, running, completed, failed
	PartyIDs     []string          `json:"party_ids"`
	Threshold    int               `json:"threshold"`
	CurrentRound int               `json:"current_round"`
	Messages     map[string][]byte `json:"messages"`
	Error        string            `json:"error"`
	CreatedAt    time.Time         `json:"created_at"`
	UpdatedAt    time.Time         `json:"updated_at"`
}

// APIResponse 通用API响应
type APIResponse struct {
	Success bool        `json:"success"`
	Data    interface{} `json:"data,omitempty"`
	Error   string      `json:"error,omitempty"`
}
