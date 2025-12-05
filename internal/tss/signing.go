package tss

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"sync"
	"time"

	"github.com/bnb-chain/tss-lib/v2/common"
	"github.com/bnb-chain/tss-lib/v2/ecdsa/keygen"
	"github.com/bnb-chain/tss-lib/v2/ecdsa/signing"
	"github.com/bnb-chain/tss-lib/v2/tss"
	"github.com/google/uuid"
	"github.com/sirupsen/logrus"

	"github.com/339-Labs/wallet-platform-mpc-go/internal/p2p"
	"github.com/339-Labs/wallet-platform-mpc-go/internal/storage"
	"github.com/339-Labs/wallet-platform-mpc-go/pkg/types"
)

// SigningSession 签名会话
type SigningSession struct {
	ID          string
	RequestID   string
	WalletID    string
	Message     *big.Int
	MessageHex  string
	SignerIDs   []string
	LocalNodeID string
	Threshold   int

	// TSS相关
	partyIDs   tss.SortedPartyIDs
	ourPartyID *tss.PartyID
	params     *tss.Parameters
	party      tss.Party
	keyData    *keygen.LocalPartySaveData

	// 通道
	outChan   chan tss.Message
	endChan   chan *common.SignatureData
	errChan   chan error
	startChan chan struct{} // 启动信号通道

	// 消息处理
	msgHandler *p2p.SessionMessageHandler
	msgManager *p2p.MessageManager

	// 上下文和超时
	ctx     context.Context
	timeout time.Duration

	// 状态
	status string
	result *SignatureData
	error  error
	mu     sync.RWMutex

	// 存储
	sessionRepo *storage.SessionRepository

	log *logrus.Entry
}

// SigningManager 签名管理器
type SigningManager struct {
	msgManager   *p2p.MessageManager
	keyShareRepo *storage.KeyShareRepository
	sessionRepo  *storage.SessionRepository
	walletRepo   *storage.WalletRepository
	localNodeID  string

	sessions  map[string]*SigningSession
	sessionMu sync.RWMutex

	timeout time.Duration
	log     *logrus.Entry
}

// NewSigningManager 创建签名管理器
func NewSigningManager(
	msgManager *p2p.MessageManager,
	keyShareRepo *storage.KeyShareRepository,
	sessionRepo *storage.SessionRepository,
	walletRepo *storage.WalletRepository,
	localNodeID string,
	timeout time.Duration,
) *SigningManager {
	return &SigningManager{
		msgManager:   msgManager,
		keyShareRepo: keyShareRepo,
		sessionRepo:  sessionRepo,
		walletRepo:   walletRepo,
		localNodeID:  localNodeID,
		sessions:     make(map[string]*SigningSession),
		timeout:      timeout,
		log:          logrus.WithField("component", "signing_manager"),
	}
}

// StartSigning 开始签名（兼容旧接口，内部调用 PrepareSigning + Start）
func (sm *SigningManager) StartSigning(ctx context.Context, req *types.SignRequest) (*SigningSession, error) {
	session, err := sm.PrepareSigning(ctx, req)
	if err != nil {
		return nil, err
	}
	session.Start()
	return session, nil
}

// PrepareSigning 准备签名会话（创建会话但不启动TSS协议）
func (sm *SigningManager) PrepareSigning(ctx context.Context, req *types.SignRequest) (*SigningSession, error) {
	sm.log.WithFields(logrus.Fields{
		"wallet_id":  req.WalletID,
		"request_id": req.RequestID,
		"party_ids":  req.PartyIDs,
	}).Info("Preparing signing session")

	// 创建会话ID
	sessionID := uuid.New().String()
	if req.RequestID != "" {
		sessionID = req.RequestID
	}

	// 检查会话是否已存在
	sm.sessionMu.Lock()
	if existingSession, exists := sm.sessions[sessionID]; exists {
		sm.sessionMu.Unlock()
		return existingSession, nil
	}
	sm.sessionMu.Unlock()

	// 获取钱包信息
	wallet, err := sm.walletRepo.GetWallet(req.WalletID)
	if err != nil {
		return nil, fmt.Errorf("failed to get wallet: %w", err)
	}

	// 验证签名者数量 - threshold 表示签名所需的最少节点数
	if len(req.PartyIDs) < wallet.Threshold {
		return nil, fmt.Errorf("not enough signers, need at least %d, got %d", wallet.Threshold, len(req.PartyIDs))
	}

	// 检查本节点是否在签名者列表中
	found := false
	for _, pid := range req.PartyIDs {
		if pid == sm.localNodeID {
			found = true
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("local node is not in signer list")
	}

	// 获取本地密钥分片
	keyData, err := sm.keyShareRepo.GetKeyShare(req.WalletID, sm.localNodeID)
	if err != nil {
		return nil, fmt.Errorf("failed to get key share: %w", err)
	}

	// 解析消息
	messageBytes, err := hex.DecodeString(req.Message)
	if err != nil {
		return nil, fmt.Errorf("invalid message hex: %w", err)
	}
	message := new(big.Int).SetBytes(messageBytes)

	// 创建会话
	session := &SigningSession{
		ID:          sessionID,
		RequestID:   req.RequestID,
		WalletID:    req.WalletID,
		Message:     message,
		MessageHex:  req.Message,
		SignerIDs:   req.PartyIDs,
		LocalNodeID: sm.localNodeID,
		Threshold:   wallet.Threshold,
		keyData:     keyData,
		outChan:     make(chan tss.Message, 100),
		endChan:     make(chan *common.SignatureData, 1),
		errChan:     make(chan error, 1),
		startChan:   make(chan struct{}, 1),
		ctx:         ctx,
		timeout:     sm.timeout,
		status:      "pending",
		sessionRepo: sm.sessionRepo,
		msgManager:  sm.msgManager,
		log:         logrus.WithField("session_id", sessionID),
	}

	// 创建签名PartyIDs
	session.partyIDs = CreatePartyIDs(req.PartyIDs)
	session.ourPartyID = GeneratePartyID(sm.localNodeID, GetPartyIndex(session.partyIDs, sm.localNodeID))

	// 创建参数
	session.params = CreateSigningParams(session.partyIDs, session.ourPartyID, wallet.Threshold)
	if session.params == nil {
		return nil, fmt.Errorf("failed to create signing params")
	}

	// 创建P2P消息处理器
	session.msgHandler = sm.msgManager.CreateSession(sessionID, "sign")

	// 保存会话状态
	sessionState := &types.SessionState{
		ID:        sessionID,
		Type:      "sign",
		WalletID:  req.WalletID,
		Status:    "pending",
		PartyIDs:  req.PartyIDs,
		Threshold: wallet.Threshold,
		CreatedAt: time.Now(),
	}
	if err := sm.sessionRepo.SaveSession(sessionState); err != nil {
		return nil, fmt.Errorf("failed to save session: %w", err)
	}

	// 注册会话
	sm.sessionMu.Lock()
	sm.sessions[sessionID] = session
	sm.sessionMu.Unlock()

	// 启动消息处理循环（但不启动TSS协议，等待Start信号）
	go session.run()

	sm.log.WithField("session_id", sessionID).Info("Signing session prepared, waiting for start signal")

	return session, nil
}

// JoinSigning 加入签名会话（兼容旧接口，内部调用 PrepareSigning）
func (sm *SigningManager) JoinSigning(ctx context.Context, sessionID string, req *types.SignRequest) (*SigningSession, error) {
	sm.log.WithFields(logrus.Fields{
		"session_id": sessionID,
		"wallet_id":  req.WalletID,
	}).Info("Joining signing session")

	// 确保使用正确的 session ID
	req.RequestID = sessionID
	return sm.PrepareSigning(ctx, req)
}

// GetSession 获取会话
func (sm *SigningManager) GetSession(sessionID string) (*SigningSession, bool) {
	sm.sessionMu.RLock()
	defer sm.sessionMu.RUnlock()
	session, ok := sm.sessions[sessionID]
	return session, ok
}

// CleanupSession 清理会话
func (sm *SigningManager) CleanupSession(sessionID string) {
	sm.sessionMu.Lock()
	defer sm.sessionMu.Unlock()

	if session, ok := sm.sessions[sessionID]; ok {
		sm.msgManager.CloseSession(sessionID)
		close(session.outChan)
		delete(sm.sessions, sessionID)
	}
}

// Start 启动TSS协议（在收到所有节点就绪确认后调用）
func (s *SigningSession) Start() {
	s.log.Info("Received start signal, launching TSS protocol")
	select {
	case s.startChan <- struct{}{}:
	default:
		// 已经启动过了
		s.log.Debug("Start signal already sent")
	}
}

// run 运行签名会话
func (s *SigningSession) run() {
	s.log.Info("Signing session started, waiting for start signal")

	// 创建超时上下文
	ctx, cancel := context.WithTimeout(s.ctx, s.timeout)
	defer cancel()

	// 等待启动信号或超时
	select {
	case <-s.startChan:
		s.log.Info("Start signal received, starting TSS protocol")
	case <-ctx.Done():
		s.handleError(fmt.Errorf("timeout waiting for start signal"))
		return
	case <-s.msgHandler.Done:
		s.handleError(fmt.Errorf("session closed before start"))
		return
	}

	// 设置状态
	s.mu.Lock()
	s.status = "running"
	s.mu.Unlock()

	// 创建TSS签名参与方
	s.party = signing.NewLocalParty(s.Message, s.params, *s.keyData, s.outChan, s.endChan).(*signing.LocalParty)

	// 启动TSS协议
	go func() {
		if err := s.party.Start(); err != nil {
			s.errChan <- err.Cause()
		}
	}()

	// 主循环
	for {
		select {
		case <-ctx.Done():
			s.handleError(fmt.Errorf("signing timeout"))
			return

		case msg := <-s.outChan:
			// 发送TSS消息
			if err := s.sendMessage(msg); err != nil {
				s.log.WithError(err).Error("Failed to send message")
			}

		case p2pMsg := <-s.msgHandler.MsgChan:
			// 处理收到的消息
			if err := s.handleMessage(p2pMsg); err != nil {
				s.log.WithError(err).Error("Failed to handle message")
			}

		case result := <-s.endChan:
			// 签名完成
			s.handleSuccess(result)
			return

		case err := <-s.errChan:
			s.handleError(err)
			return

		case <-s.msgHandler.Done:
			s.handleError(fmt.Errorf("session closed"))
			return
		}
	}
}

// sendMessage 发送TSS消息
func (s *SigningSession) sendMessage(msg tss.Message) error {
	wrapper, err := SerializeMessage(msg)
	if err != nil {
		return err
	}

	// 设置消息类型为签名
	data, err := json.Marshal(wrapper)
	if err != nil {
		return err
	}

	dest := msg.GetTo()

	if msg.IsBroadcast() {
		return s.msgManager.BroadcastToPartiesWithType(s.ID, s.SignerIDs, data, 0, p2p.MsgTypeSignRound)
	} else if len(dest) > 0 {
		return s.msgManager.SendToPartyWithType(s.ID, dest[0].Id, data, 0, p2p.MsgTypeSignRound)
	}

	return nil
}

// handleMessage 处理收到的消息
func (s *SigningSession) handleMessage(p2pMsg *types.P2PMessage) error {
	s.log.WithFields(logrus.Fields{
		"from":  p2pMsg.From,
		"round": p2pMsg.Round,
	}).Debug("Received signing message")

	// 反序列化消息
	var wrapper MessageWrapper
	if err := json.Unmarshal(p2pMsg.Data, &wrapper); err != nil {
		return fmt.Errorf("failed to unmarshal message wrapper: %w", err)
	}

	// 解析TSS消息
	tssMsg, err := DeserializeMessage(&wrapper, s.partyIDs)
	if err != nil {
		return fmt.Errorf("failed to deserialize tss message: %w", err)
	}

	// 更新TSS状态
	ok, tssErr := s.party.Update(tssMsg)
	if tssErr != nil {
		// TSS library may return error with nil cause for duplicate/already-processed messages
		// Only treat as real error if the cause is non-nil
		if tssErr.Cause() != nil {
			return fmt.Errorf("failed to update party: %w", tssErr.Cause())
		}
		// Log but don't fail for errors with nil cause
		s.log.WithField("tss_error", tssErr.Error()).Debug("TSS update returned error with nil cause (possibly duplicate message)")
	}
	if !ok {
		s.log.Debug("Message not yet ready to be processed")
	}

	return nil
}

// handleSuccess 处理成功
func (s *SigningSession) handleSuccess(result *common.SignatureData) {
	sigData := GetSignature(result)

	s.mu.Lock()
	s.status = "completed"
	s.result = sigData
	s.mu.Unlock()

	s.log.WithFields(logrus.Fields{
		"r":         sigData.R.Text(16),
		"s":         sigData.S.Text(16),
		"v":         sigData.V,
		"signature": SignatureToHex(sigData),
	}).Info("Signing completed successfully")

	// 更新会话状态
	if err := s.sessionRepo.UpdateSessionStatus(s.ID, "completed"); err != nil {
		s.log.WithError(err).Error("Failed to update session status")
	}
}

// handleError 处理错误
func (s *SigningSession) handleError(err error) {
	s.mu.Lock()
	s.status = "failed"
	s.error = err
	s.mu.Unlock()

	s.log.WithError(err).Error("Signing failed")

	// 更新会话状态
	if updateErr := s.sessionRepo.UpdateSessionError(s.ID, err.Error()); updateErr != nil {
		s.log.WithError(updateErr).Error("Failed to update session error")
	}
}

// GetStatus 获取状态
func (s *SigningSession) GetStatus() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.status
}

// GetResult 获取结果
func (s *SigningSession) GetResult() (*types.SignResult, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.status != "completed" {
		return nil, fmt.Errorf("signing not completed, status: %s", s.status)
	}
	if s.error != nil {
		return nil, s.error
	}

	return &types.SignResult{
		RequestID: s.RequestID,
		Signature: SignatureToHex(s.result),
		R:         s.result.R.Text(16),
		S:         s.result.S.Text(16),
		V:         s.result.V,
	}, nil
}

// WaitForCompletion 等待完成
func (s *SigningSession) WaitForCompletion(timeout time.Duration) (*types.SignResult, error) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		status := s.GetStatus()
		if status == "completed" {
			return s.GetResult()
		}
		if status == "failed" {
			s.mu.RLock()
			err := s.error
			s.mu.RUnlock()
			return nil, err
		}
		<-ticker.C
	}

	return nil, fmt.Errorf("signing timeout")
}
