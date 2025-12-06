package tss

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/bnb-chain/tss-lib/v2/ecdsa/keygen"
	"github.com/bnb-chain/tss-lib/v2/tss"
	"github.com/google/uuid"
	"github.com/sirupsen/logrus"

	"github.com/339-Labs/wallet-platform-mpc-go/internal/p2p"
	"github.com/339-Labs/wallet-platform-mpc-go/internal/storage"
	"github.com/339-Labs/wallet-platform-mpc-go/pkg/types"
)

// KeygenSession 密钥生成会话
type KeygenSession struct {
	ID          string
	WalletID    string
	WalletName  string
	Threshold   int
	TotalParts  int
	NodeIDs     []string
	LocalNodeID string

	// TSS相关
	partyIDs   tss.SortedPartyIDs
	ourPartyID *tss.PartyID
	params     *tss.Parameters
	party      tss.Party

	// 通道
	outChan   chan tss.Message
	endChan   chan *keygen.LocalPartySaveData
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
	result *keygen.LocalPartySaveData
	error  error
	mu     sync.RWMutex

	// 存储
	keyShareRepo *storage.KeyShareRepository
	sessionRepo  *storage.SessionRepository
	walletRepo   *storage.WalletRepository

	log *logrus.Entry
}

// KeygenManager 密钥生成管理器
type KeygenManager struct {
	msgManager   *p2p.MessageManager
	keyShareRepo *storage.KeyShareRepository
	sessionRepo  *storage.SessionRepository
	walletRepo   *storage.WalletRepository
	localNodeID  string

	sessions  map[string]*KeygenSession
	sessionMu sync.RWMutex

	timeout time.Duration
	log     *logrus.Entry
}

// NewKeygenManager 创建密钥生成管理器
func NewKeygenManager(
	msgManager *p2p.MessageManager,
	keyShareRepo *storage.KeyShareRepository,
	sessionRepo *storage.SessionRepository,
	walletRepo *storage.WalletRepository,
	localNodeID string,
	timeout time.Duration,
) *KeygenManager {
	return &KeygenManager{
		msgManager:   msgManager,
		keyShareRepo: keyShareRepo,
		sessionRepo:  sessionRepo,
		walletRepo:   walletRepo,
		localNodeID:  localNodeID,
		sessions:     make(map[string]*KeygenSession),
		timeout:      timeout,
		log:          logrus.WithField("component", "keygen_manager"),
	}
}

// StartKeygen 开始密钥生成（兼容旧接口，内部调用 PrepareKeygen + Start）
func (km *KeygenManager) StartKeygen(ctx context.Context, req *types.KeygenRequest) (*KeygenSession, error) {
	session, err := km.PrepareKeygen(ctx, req)
	if err != nil {
		return nil, err
	}
	session.Start()
	return session, nil
}

// PrepareKeygen 准备密钥生成会话（创建会话但不启动TSS协议）
func (km *KeygenManager) PrepareKeygen(ctx context.Context, req *types.KeygenRequest) (*KeygenSession, error) {
	km.log.WithFields(logrus.Fields{
		"session_id":  req.SessionID,
		"wallet_id":   req.WalletID,
		"threshold":   req.Threshold,
		"total_parts": req.TotalParts,
		"party_ids":   req.PartyIDs,
	}).Info("Preparing keygen session")

	// 使用请求中的会话ID，如果没有则创建新的
	sessionID := req.SessionID
	if sessionID == "" {
		sessionID = uuid.New().String()
	}

	// 检查会话是否已存在
	km.sessionMu.Lock()
	if existingSession, exists := km.sessions[sessionID]; exists {
		km.sessionMu.Unlock()
		return existingSession, nil
	}
	km.sessionMu.Unlock()

	// 验证参数
	// threshold 表示签名所需的最少节点数（至少为2才有门限签名的意义）
	if req.Threshold < 2 {
		return nil, fmt.Errorf("threshold must be at least 2")
	}
	// 总节点数必须 >= threshold（签名所需节点数）
	if req.TotalParts < req.Threshold {
		return nil, fmt.Errorf("total parts must be at least threshold (%d)", req.Threshold)
	}
	if len(req.PartyIDs) != req.TotalParts {
		return nil, fmt.Errorf("party IDs count must match total parts")
	}

	// 检查本节点是否在参与方列表中
	found := false
	for _, pid := range req.PartyIDs {
		if pid == km.localNodeID {
			found = true
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("local node is not in party list")
	}

	// 创建会话
	session := &KeygenSession{
		ID:           sessionID,
		WalletID:     req.WalletID,
		WalletName:   req.WalletName,
		Threshold:    req.Threshold,
		TotalParts:   req.TotalParts,
		NodeIDs:      req.PartyIDs,
		LocalNodeID:  km.localNodeID,
		outChan:      make(chan tss.Message, 100),
		endChan:      make(chan *keygen.LocalPartySaveData, 1),
		errChan:      make(chan error, 1),
		startChan:    make(chan struct{}, 1),
		ctx:          ctx,
		timeout:      km.timeout,
		status:       "pending",
		keyShareRepo: km.keyShareRepo,
		sessionRepo:  km.sessionRepo,
		walletRepo:   km.walletRepo,
		msgManager:   km.msgManager,
		log:          logrus.WithField("session_id", sessionID),
	}

	// 创建PartyIDs
	session.partyIDs = CreatePartyIDs(req.PartyIDs)
	session.ourPartyID = GeneratePartyID(km.localNodeID, GetPartyIndex(session.partyIDs, km.localNodeID))

	// 创建参数
	session.params = CreateKeygenParams(session.partyIDs, session.ourPartyID, req.Threshold, req.TotalParts)
	if session.params == nil {
		return nil, fmt.Errorf("failed to create keygen params")
	}

	// 创建P2P消息处理器
	session.msgHandler = km.msgManager.CreateSession(sessionID, "keygen")

	// 保存会话状态
	sessionState := &types.SessionState{
		ID:        sessionID,
		Type:      "keygen",
		WalletID:  req.WalletID,
		Status:    "pending",
		PartyIDs:  req.PartyIDs,
		Threshold: req.Threshold,
		CreatedAt: time.Now(),
	}
	if err := km.sessionRepo.SaveSession(sessionState); err != nil {
		return nil, fmt.Errorf("failed to save session: %w", err)
	}

	// 注册会话
	km.sessionMu.Lock()
	km.sessions[sessionID] = session
	km.sessionMu.Unlock()

	// 启动消息处理循环（但不启动TSS协议，等待Start信号）
	go session.run()

	km.log.WithField("session_id", sessionID).Info("Keygen session prepared, waiting for start signal")

	return session, nil
}

// JoinKeygen 加入密钥生成会话（兼容旧接口，内部调用 PrepareKeygen）
func (km *KeygenManager) JoinKeygen(ctx context.Context, sessionID string, req *types.KeygenRequest) (*KeygenSession, error) {
	km.log.WithFields(logrus.Fields{
		"session_id": sessionID,
		"wallet_id":  req.WalletID,
	}).Info("Joining keygen session")

	// 确保使用正确的 session ID
	req.SessionID = sessionID
	return km.PrepareKeygen(ctx, req)
}

// GetSession 获取会话
func (km *KeygenManager) GetSession(sessionID string) (*KeygenSession, bool) {
	km.sessionMu.RLock()
	defer km.sessionMu.RUnlock()
	session, ok := km.sessions[sessionID]
	return session, ok
}

// CleanupSession 清理会话
func (km *KeygenManager) CleanupSession(sessionID string) {
	km.sessionMu.Lock()
	defer km.sessionMu.Unlock()

	if session, ok := km.sessions[sessionID]; ok {
		km.msgManager.CloseSession(sessionID)
		close(session.outChan)
		delete(km.sessions, sessionID)
	}
}

// Start 启动TSS协议（在收到所有节点就绪确认后调用）
func (s *KeygenSession) Start() {
	s.log.Info("Received start signal, launching TSS protocol")
	select {
	case s.startChan <- struct{}{}:
	default:
		// 已经启动过了
		s.log.Debug("Start signal already sent")
	}
}

// run 运行密钥生成会话
func (s *KeygenSession) run() {
	s.log.Info("Keygen session started, waiting for start signal")

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

	// 创建TSS参与方
	s.party = keygen.NewLocalParty(s.params, s.outChan, s.endChan).(*keygen.LocalParty)

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
			s.handleError(fmt.Errorf("keygen timeout"))
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
			// 密钥生成完成
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
func (s *KeygenSession) sendMessage(msg tss.Message) error {
	wrapper, err := SerializeMessage(msg)
	if err != nil {
		return err
	}

	data, err := json.Marshal(wrapper)
	if err != nil {
		return err
	}

	dest := msg.GetTo()

	if msg.IsBroadcast() {
		// 广播消息
		return s.msgManager.BroadcastToParties(s.ID, s.NodeIDs, data, 0)
	} else if len(dest) > 0 {
		// 点对点消息
		return s.msgManager.SendToParty(s.ID, dest[0].Id, data, 0)
	}

	return nil
}

// handleMessage 处理收到的消息
func (s *KeygenSession) handleMessage(p2pMsg *types.P2PMessage) error {
	s.log.WithFields(logrus.Fields{
		"from":  p2pMsg.From,
		"round": p2pMsg.Round,
	}).Debug("Received keygen message")

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
func (s *KeygenSession) handleSuccess(result *keygen.LocalPartySaveData) {
	s.mu.Lock()
	s.status = "completed"
	s.result = result
	s.mu.Unlock()

	// 获取公钥和地址
	pubKey, address, err := RecoverPublicKey(result)
	if err != nil {
		s.log.WithError(err).Error("Failed to recover public key")
		return
	}

	s.log.WithFields(logrus.Fields{
		"address":    address,
		"public_key": PublicKeyToHex(pubKey),
	}).Info("Keygen completed successfully")

	// 保存密钥分片
	if err := s.keyShareRepo.SaveKeyShare(s.WalletID, s.LocalNodeID, result); err != nil {
		s.log.WithError(err).Error("Failed to save key share")
	}

	// 保存钱包信息（所有参与节点都保存）
	if s.walletRepo != nil {
		walletName := s.WalletName
		if walletName == "" {
			walletName = s.WalletID // 如果没有名称，使用ID作为名称
		}
		wallet := &types.WalletInfo{
			ID:         s.WalletID,
			Name:       walletName,
			Address:    address,
			PublicKey:  PublicKeyToHex(pubKey),
			Threshold:  s.Threshold,
			TotalParts: s.TotalParts,
			PartyIDs:   s.NodeIDs,
			CreatedAt:  time.Now(),
			UpdatedAt:  time.Now(),
		}
		if err := s.walletRepo.SaveWallet(wallet); err != nil {
			s.log.WithError(err).Error("Failed to save wallet info")
		} else {
			s.log.WithField("wallet_id", s.WalletID).Info("Wallet info saved")
		}
	}

	// 更新会话状态
	if err := s.sessionRepo.UpdateSessionStatus(s.ID, "completed"); err != nil {
		s.log.WithError(err).Error("Failed to update session status")
	}
}

// handleError 处理错误
func (s *KeygenSession) handleError(err error) {
	s.mu.Lock()
	s.status = "failed"
	s.error = err
	s.mu.Unlock()

	s.log.WithError(err).Error("Keygen failed")

	// 更新会话状态
	if updateErr := s.sessionRepo.UpdateSessionError(s.ID, err.Error()); updateErr != nil {
		s.log.WithError(updateErr).Error("Failed to update session error")
	}
}

// GetStatus 获取状态
func (s *KeygenSession) GetStatus() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.status
}

// GetResult 获取结果
func (s *KeygenSession) GetResult() (*keygen.LocalPartySaveData, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.status != "completed" {
		return nil, fmt.Errorf("keygen not completed, status: %s", s.status)
	}
	if s.error != nil {
		return nil, s.error
	}
	return s.result, nil
}

// WaitForCompletion 等待完成
func (s *KeygenSession) WaitForCompletion(timeout time.Duration) (*keygen.LocalPartySaveData, error) {
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

	return nil, fmt.Errorf("keygen timeout")
}
