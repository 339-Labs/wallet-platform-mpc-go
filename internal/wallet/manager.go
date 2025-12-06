package wallet

import (
	"context"
	"encoding/hex"
	"fmt"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/google/uuid"
	"github.com/sirupsen/logrus"

	"github.com/339-Labs/wallet-platform-mpc-go/internal/storage"
	"github.com/339-Labs/wallet-platform-mpc-go/internal/tss"
	mpcTypes "github.com/339-Labs/wallet-platform-mpc-go/pkg/types"
)

// Manager 钱包管理器
type Manager struct {
	walletRepo   *storage.WalletRepository
	keyShareRepo *storage.KeyShareRepository
	sessionRepo  *storage.SessionRepository
	keygenMgr    *tss.KeygenManager
	signingMgr   *tss.SigningManager
	resharingMgr *tss.ResharingManager
	coordinator  *tss.Coordinator
	localNodeID  string

	log *logrus.Entry
}

// NewManager 创建钱包管理器
func NewManager(
	walletRepo *storage.WalletRepository,
	keyShareRepo *storage.KeyShareRepository,
	sessionRepo *storage.SessionRepository,
	keygenMgr *tss.KeygenManager,
	signingMgr *tss.SigningManager,
	resharingMgr *tss.ResharingManager,
	coordinator *tss.Coordinator,
	localNodeID string,
) *Manager {
	return &Manager{
		walletRepo:   walletRepo,
		keyShareRepo: keyShareRepo,
		sessionRepo:  sessionRepo,
		keygenMgr:    keygenMgr,
		signingMgr:   signingMgr,
		resharingMgr: resharingMgr,
		coordinator:  coordinator,
		localNodeID:  localNodeID,
		log:          logrus.WithField("component", "wallet_manager"),
	}
}

// CreateWallet 创建新钱包（发起DKG）
func (m *Manager) CreateWallet(ctx context.Context, name string, threshold, totalParts int, partyIDs []string) (*mpcTypes.WalletInfo, error) {
	m.log.WithFields(logrus.Fields{
		"name":            name,
		"threshold":       threshold,
		"total_parts":     totalParts,
		"party_ids":       partyIDs,
		"has_coordinator": m.coordinator != nil,
	}).Info("Creating new wallet")

	// 生成钱包ID
	walletID := uuid.New().String()

	// 创建密钥生成请求
	req := &mpcTypes.KeygenRequest{
		WalletID:   walletID,
		WalletName: name,
		Threshold:  threshold,
		TotalParts: totalParts,
		PartyIDs:   partyIDs,
	}

	var sessionID string
	var session *tss.KeygenSession
	var err error

	// 如果有 coordinator，则使用 coordinator 发起 keygen（多节点协调模式）
	if m.coordinator != nil {
		sessionID, err = m.coordinator.InitiateKeygen(ctx, req)
		if err != nil {
			return nil, fmt.Errorf("failed to initiate keygen: %w", err)
		}

		// 获取会话
		session, _ = m.keygenMgr.GetSession(sessionID)
		if session == nil {
			return nil, fmt.Errorf("keygen session not found after initiation")
		}
	} else {
		// 单节点模式（用于测试）
		session, err = m.keygenMgr.StartKeygen(ctx, req)
		if err != nil {
			return nil, fmt.Errorf("failed to start keygen: %w", err)
		}
	}

	// 等待完成
	result, err := session.WaitForCompletion(5 * time.Minute)
	if err != nil {
		return nil, fmt.Errorf("keygen failed: %w", err)
	}

	// 获取公钥和地址
	pubKey, address, err := tss.RecoverPublicKey(result)
	if err != nil {
		return nil, fmt.Errorf("failed to recover public key: %w", err)
	}

	// 创建钱包信息
	wallet := &mpcTypes.WalletInfo{
		ID:         walletID,
		Name:       name,
		Address:    address,
		PublicKey:  tss.PublicKeyToHex(pubKey),
		Threshold:  threshold,
		TotalParts: totalParts,
		PartyIDs:   partyIDs,
		CreatedAt:  time.Now(),
		UpdatedAt:  time.Now(),
	}

	// 保存钱包信息
	if err := m.walletRepo.SaveWallet(wallet); err != nil {
		return nil, fmt.Errorf("failed to save wallet: %w", err)
	}

	m.log.WithFields(logrus.Fields{
		"wallet_id": walletID,
		"address":   address,
	}).Info("Wallet created successfully")

	return wallet, nil
}

// GetWallet 获取钱包信息
func (m *Manager) GetWallet(walletID string) (*mpcTypes.WalletInfo, error) {
	return m.walletRepo.GetWallet(walletID)
}

// GetWalletByAddress 通过地址获取钱包
func (m *Manager) GetWalletByAddress(address string) (*mpcTypes.WalletInfo, error) {
	return m.walletRepo.GetWalletByAddress(address)
}

// ListWallets 列出所有钱包
func (m *Manager) ListWallets() ([]*mpcTypes.WalletInfo, error) {
	return m.walletRepo.ListWallets()
}

// DeleteWallet 删除钱包
func (m *Manager) DeleteWallet(walletID string) error {
	// 获取钱包信息
	wallet, err := m.walletRepo.GetWallet(walletID)
	if err != nil {
		return err
	}

	// 删除密钥分片
	if err := m.keyShareRepo.DeleteKeyShare(walletID, m.localNodeID); err != nil {
		m.log.WithError(err).Warn("Failed to delete key share")
	}

	// 删除钱包
	if err := m.walletRepo.DeleteWallet(walletID); err != nil {
		return err
	}

	m.log.WithFields(logrus.Fields{
		"wallet_id": walletID,
		"address":   wallet.Address,
	}).Info("Wallet deleted")

	return nil
}

// SignMessage 签名消息
func (m *Manager) SignMessage(ctx context.Context, walletID string, message []byte, signerIDs []string) (*mpcTypes.SignResult, error) {
	m.log.WithFields(logrus.Fields{
		"wallet_id":  walletID,
		"signer_ids": signerIDs,
	}).Info("Signing message")

	// 获取钱包信息
	wallet, err := m.walletRepo.GetWallet(walletID)
	if err != nil {
		return nil, fmt.Errorf("failed to get wallet: %w", err)
	}

	// 验证签名者数量 - threshold 表示签名所需的最少节点数
	if len(signerIDs) < wallet.Threshold {
		return nil, fmt.Errorf("not enough signers, need at least %d, got %d", wallet.Threshold, len(signerIDs))
	}

	// 计算消息哈希
	messageHash := crypto.Keccak256(message)

	// 创建签名请求
	req := &mpcTypes.SignRequest{
		WalletID:  walletID,
		Message:   hex.EncodeToString(messageHash),
		PartyIDs:  signerIDs,
		RequestID: uuid.New().String(),
	}

	var sessionID string
	var session *tss.SigningSession

	// 如果有 coordinator，则使用 coordinator 发起签名（多节点协调模式）
	if m.coordinator != nil {
		sessionID, err = m.coordinator.InitiateSign(ctx, req)
		if err != nil {
			return nil, fmt.Errorf("failed to initiate signing: %w", err)
		}

		// 获取会话
		session, _ = m.signingMgr.GetSession(sessionID)
		if session == nil {
			return nil, fmt.Errorf("signing session not found after initiation")
		}
	} else {
		// 单节点模式（用于测试）
		session, err = m.signingMgr.StartSigning(ctx, req)
		if err != nil {
			return nil, fmt.Errorf("failed to start signing: %w", err)
		}
	}

	// 等待完成（使用和创建钱包相同的超时时间）
	result, err := session.WaitForCompletion(5 * time.Minute)
	if err != nil {
		return nil, fmt.Errorf("signing failed: %w", err)
	}

	m.log.WithFields(logrus.Fields{
		"wallet_id": walletID,
		"signature": result.Signature,
	}).Info("Message signed successfully")

	return result, nil
}

// SignTransaction 签名交易
func (m *Manager) SignTransaction(ctx context.Context, req *mpcTypes.TransactionRequest) (*mpcTypes.SignResult, string, error) {
	m.log.WithFields(logrus.Fields{
		"wallet_id": req.WalletID,
		"to":        req.To,
		"value":     req.Value,
	}).Info("Signing transaction")

	// 获取钱包信息
	wallet, err := m.walletRepo.GetWallet(req.WalletID)
	if err != nil {
		return nil, "", fmt.Errorf("failed to get wallet: %w", err)
	}

	// 构建交易
	to := common.HexToAddress(req.To)
	value, ok := new(big.Int).SetString(req.Value, 10)
	if !ok {
		return nil, "", fmt.Errorf("invalid value")
	}

	gasPrice, ok := new(big.Int).SetString(req.GasPrice, 10)
	if !ok {
		return nil, "", fmt.Errorf("invalid gas price")
	}

	var data []byte
	if req.Data != "" {
		data, err = hex.DecodeString(req.Data)
		if err != nil {
			return nil, "", fmt.Errorf("invalid data: %w", err)
		}
	}

	// 创建交易对象
	tx := types.NewTransaction(
		req.Nonce,
		to,
		value,
		req.GasLimit,
		gasPrice,
		data,
	)

	// 获取交易哈希（用于签名）
	chainID := big.NewInt(req.ChainID)
	signer := types.NewEIP155Signer(chainID)
	txHash := signer.Hash(tx)

	// 创建签名请求
	signReq := &mpcTypes.SignRequest{
		WalletID:  req.WalletID,
		Message:   hex.EncodeToString(txHash[:]),
		PartyIDs:  req.PartyIDs,
		RequestID: uuid.New().String(),
	}

	var sessionID string
	var session *tss.SigningSession

	// 如果有 coordinator，则使用 coordinator 发起签名（多节点协调模式）
	if m.coordinator != nil {
		sessionID, err = m.coordinator.InitiateSign(ctx, signReq)
		if err != nil {
			return nil, "", fmt.Errorf("failed to initiate signing: %w", err)
		}

		// 获取会话
		session, _ = m.signingMgr.GetSession(sessionID)
		if session == nil {
			return nil, "", fmt.Errorf("signing session not found after initiation")
		}
	} else {
		// 单节点模式（用于测试）
		session, err = m.signingMgr.StartSigning(ctx, signReq)
		if err != nil {
			return nil, "", fmt.Errorf("failed to start signing: %w", err)
		}
	}

	// 等待完成（使用和创建钱包相同的超时时间）
	result, err := session.WaitForCompletion(5 * time.Minute)
	if err != nil {
		return nil, "", fmt.Errorf("signing failed: %w", err)
	}

	// 解析签名
	r, ok := new(big.Int).SetString(result.R, 16)
	if !ok {
		return nil, "", fmt.Errorf("invalid signature R")
	}
	s, ok := new(big.Int).SetString(result.S, 16)
	if !ok {
		return nil, "", fmt.Errorf("invalid signature S")
	}
	v := big.NewInt(int64(result.V))

	// 调整V值（EIP-155）
	v = v.Add(v, big.NewInt(int64(chainID.Uint64()*2+35)))

	// 创建签名交易
	signedTx, err := tx.WithSignature(signer, append(append(r.Bytes(), s.Bytes()...), byte(result.V)))
	if err != nil {
		return nil, "", fmt.Errorf("failed to create signed transaction: %w", err)
	}

	// 序列化签名交易
	rawTx, err := rlp.EncodeToBytes(signedTx)
	if err != nil {
		return nil, "", fmt.Errorf("failed to encode transaction: %w", err)
	}

	rawTxHex := hex.EncodeToString(rawTx)

	m.log.WithFields(logrus.Fields{
		"wallet_id": req.WalletID,
		"tx_hash":   signedTx.Hash().Hex(),
		"address":   wallet.Address,
	}).Info("Transaction signed successfully")

	return result, rawTxHex, nil
}

// GetAddress 获取钱包地址
func (m *Manager) GetAddress(walletID string) (string, error) {
	wallet, err := m.walletRepo.GetWallet(walletID)
	if err != nil {
		return "", err
	}
	return wallet.Address, nil
}

// ReshareWallet 重新分享钱包密钥
// 用于：1. 更换参与方  2. 修改阈值  3. 定期刷新密钥分片
func (m *Manager) ReshareWallet(ctx context.Context, req *mpcTypes.ResharingRequest) (*mpcTypes.ResharingResult, error) {
	m.log.WithFields(logrus.Fields{
		"wallet_id":     req.WalletID,
		"old_threshold": req.OldThreshold,
		"new_threshold": req.NewThreshold,
		"old_parties":   req.OldPartyIDs,
		"new_parties":   req.NewPartyIDs,
	}).Info("Resharing wallet")

	// 获取当前钱包信息
	wallet, err := m.walletRepo.GetWallet(req.WalletID)
	if err != nil {
		return nil, fmt.Errorf("failed to get wallet: %w", err)
	}

	// 验证旧配置与当前钱包匹配
	if req.OldThreshold != wallet.Threshold {
		return nil, fmt.Errorf("old threshold mismatch: expected %d, got %d", wallet.Threshold, req.OldThreshold)
	}

	// 创建内部请求
	internalReq := &tss.ResharingRequest{
		WalletID:     req.WalletID,
		OldThreshold: req.OldThreshold,
		OldPartyIDs:  req.OldPartyIDs,
		NewThreshold: req.NewThreshold,
		NewPartyIDs:  req.NewPartyIDs,
	}

	// 启动重分享
	session, err := m.resharingMgr.StartResharing(ctx, internalReq)
	if err != nil {
		return nil, fmt.Errorf("failed to start resharing: %w", err)
	}

	// 等待完成
	_, err = session.WaitForCompletion(10 * time.Minute)
	if err != nil {
		return nil, fmt.Errorf("resharing failed: %w", err)
	}

	m.log.WithFields(logrus.Fields{
		"wallet_id":     req.WalletID,
		"new_threshold": req.NewThreshold,
		"new_parties":   req.NewPartyIDs,
	}).Info("Wallet reshared successfully")

	return &mpcTypes.ResharingResult{
		WalletID:     req.WalletID,
		NewThreshold: req.NewThreshold,
		NewPartyIDs:  req.NewPartyIDs,
		Address:      wallet.Address,
		CompletedAt:  time.Now(),
	}, nil
}

// RefreshKeyShares 刷新密钥分片（不改变参与方和阈值）
func (m *Manager) RefreshKeyShares(ctx context.Context, walletID string) (*mpcTypes.ResharingResult, error) {
	wallet, err := m.walletRepo.GetWallet(walletID)
	if err != nil {
		return nil, fmt.Errorf("failed to get wallet: %w", err)
	}

	// 使用相同的参与方和阈值进行重分享（刷新分片）
	req := &mpcTypes.ResharingRequest{
		WalletID:     walletID,
		OldThreshold: wallet.Threshold,
		OldPartyIDs:  wallet.PartyIDs,
		NewThreshold: wallet.Threshold,
		NewPartyIDs:  wallet.PartyIDs,
	}

	return m.ReshareWallet(ctx, req)
}

// HasKeyShare 检查是否有密钥分片
func (m *Manager) HasKeyShare(walletID string) (bool, error) {
	return m.keyShareRepo.HasKeyShare(walletID, m.localNodeID)
}

// GetKeyShareInfo 获取密钥分片信息
func (m *Manager) GetKeyShareInfo(walletID string) (*mpcTypes.KeyShare, error) {
	return m.keyShareRepo.GetKeyShareMeta(walletID, m.localNodeID)
}
