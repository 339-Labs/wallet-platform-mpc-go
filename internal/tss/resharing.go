package tss

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/bnb-chain/tss-lib/v2/ecdsa/keygen"
	"github.com/bnb-chain/tss-lib/v2/ecdsa/resharing"
	"github.com/bnb-chain/tss-lib/v2/tss"
	"github.com/google/uuid"
	"github.com/sirupsen/logrus"

	"github.com/339-Labs/wallet-platform-mpc-go/internal/p2p"
	"github.com/339-Labs/wallet-platform-mpc-go/internal/storage"
	"github.com/339-Labs/wallet-platform-mpc-go/pkg/types"
)

// ResharingSession 密钥重分享会话
type ResharingSession struct {
	ID       string
	WalletID string

	// 旧配置
	OldThreshold int
	OldPartyIDs  []string

	// 新配置
	NewThreshold int
	NewPartyIDs  []string

	LocalNodeID string
	IsOldMember bool // 本节点是否是旧成员
	IsNewMember bool // 本节点是否是新成员

	// TSS相关
	oldPartyIDs   tss.SortedPartyIDs
	newPartyIDs   tss.SortedPartyIDs
	ourOldPartyID *tss.PartyID
	ourNewPartyID *tss.PartyID
	params        *tss.ReSharingParameters
	party         tss.Party
	oldKeyData    *keygen.LocalPartySaveData

	// 通道
	outChan chan tss.Message
	endChan chan *keygen.LocalPartySaveData
	errChan chan error

	// 消息处理
	msgHandler *p2p.SessionMessageHandler
	msgManager *p2p.MessageManager

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

// ResharingRequest 重分享请求
type ResharingRequest struct {
	WalletID     string   `json:"wallet_id"`
	OldThreshold int      `json:"old_threshold"`
	OldPartyIDs  []string `json:"old_party_ids"`
	NewThreshold int      `json:"new_threshold"`
	NewPartyIDs  []string `json:"new_party_ids"`
}

// ResharingManager 重分享管理器
type ResharingManager struct {
	msgManager   *p2p.MessageManager
	keyShareRepo *storage.KeyShareRepository
	sessionRepo  *storage.SessionRepository
	walletRepo   *storage.WalletRepository
	localNodeID  string

	sessions  map[string]*ResharingSession
	sessionMu sync.RWMutex

	timeout time.Duration
	log     *logrus.Entry
}

// NewResharingManager 创建重分享管理器
func NewResharingManager(
	msgManager *p2p.MessageManager,
	keyShareRepo *storage.KeyShareRepository,
	sessionRepo *storage.SessionRepository,
	walletRepo *storage.WalletRepository,
	localNodeID string,
	timeout time.Duration,
) *ResharingManager {
	return &ResharingManager{
		msgManager:   msgManager,
		keyShareRepo: keyShareRepo,
		sessionRepo:  sessionRepo,
		walletRepo:   walletRepo,
		localNodeID:  localNodeID,
		sessions:     make(map[string]*ResharingSession),
		timeout:      timeout,
		log:          logrus.WithField("component", "resharing_manager"),
	}
}

// StartResharing 开始密钥重分享
func (rm *ResharingManager) StartResharing(ctx context.Context, req *ResharingRequest) (*ResharingSession, error) {
	rm.log.WithFields(logrus.Fields{
		"wallet_id":     req.WalletID,
		"old_threshold": req.OldThreshold,
		"new_threshold": req.NewThreshold,
		"old_parties":   req.OldPartyIDs,
		"new_parties":   req.NewPartyIDs,
	}).Info("Starting resharing session")

	// 验证参数
	if err := rm.validateResharingRequest(req); err != nil {
		return nil, err
	}

	// 检查本节点角色
	isOldMember := contains(req.OldPartyIDs, rm.localNodeID)
	isNewMember := contains(req.NewPartyIDs, rm.localNodeID)

	if !isOldMember && !isNewMember {
		return nil, fmt.Errorf("local node is not part of resharing")
	}

	// 获取旧的密钥分片（如果是旧成员）
	var oldKeyData *keygen.LocalPartySaveData
	if isOldMember {
		var err error
		oldKeyData, err = rm.keyShareRepo.GetKeyShare(req.WalletID, rm.localNodeID)
		if err != nil {
			return nil, fmt.Errorf("failed to get old key share: %w", err)
		}
	}

	// 创建会话ID
	sessionID := uuid.New().String()

	// 创建会话
	session := &ResharingSession{
		ID:           sessionID,
		WalletID:     req.WalletID,
		OldThreshold: req.OldThreshold,
		OldPartyIDs:  req.OldPartyIDs,
		NewThreshold: req.NewThreshold,
		NewPartyIDs:  req.NewPartyIDs,
		LocalNodeID:  rm.localNodeID,
		IsOldMember:  isOldMember,
		IsNewMember:  isNewMember,
		oldKeyData:   oldKeyData,
		outChan:      make(chan tss.Message, 100),
		endChan:      make(chan *keygen.LocalPartySaveData, 1),
		errChan:      make(chan error, 1),
		status:       "pending",
		keyShareRepo: rm.keyShareRepo,
		sessionRepo:  rm.sessionRepo,
		walletRepo:   rm.walletRepo,
		msgManager:   rm.msgManager,
		log:          logrus.WithField("session_id", sessionID),
	}

	// 创建PartyIDs
	session.oldPartyIDs = CreatePartyIDs(req.OldPartyIDs)
	session.newPartyIDs = CreatePartyIDs(req.NewPartyIDs)

	if isOldMember {
		session.ourOldPartyID = GeneratePartyID(rm.localNodeID, GetPartyIndex(session.oldPartyIDs, rm.localNodeID))
	}
	if isNewMember {
		session.ourNewPartyID = GeneratePartyID(rm.localNodeID, GetPartyIndex(session.newPartyIDs, rm.localNodeID))
	}

	// 创建重分享参数
	session.params = rm.createResharingParams(session)
	if session.params == nil {
		return nil, fmt.Errorf("failed to create resharing params")
	}

	// 创建P2P消息处理器
	session.msgHandler = rm.msgManager.CreateSession(sessionID, "resharing")

	// 保存会话状态
	sessionState := &types.SessionState{
		ID:        sessionID,
		Type:      "resharing",
		WalletID:  req.WalletID,
		Status:    "pending",
		PartyIDs:  append(req.OldPartyIDs, req.NewPartyIDs...),
		Threshold: req.NewThreshold,
		CreatedAt: time.Now(),
	}
	if err := rm.sessionRepo.SaveSession(sessionState); err != nil {
		return nil, fmt.Errorf("failed to save session: %w", err)
	}

	// 注册会话
	rm.sessionMu.Lock()
	rm.sessions[sessionID] = session
	rm.sessionMu.Unlock()

	// 启动重分享
	go session.run(ctx, rm.timeout)

	return session, nil
}

// JoinResharing 加入重分享会话
func (rm *ResharingManager) JoinResharing(ctx context.Context, sessionID string, req *ResharingRequest) (*ResharingSession, error) {
	rm.log.WithFields(logrus.Fields{
		"session_id": sessionID,
		"wallet_id":  req.WalletID,
	}).Info("Joining resharing session")

	// 使用写锁检查并创建会话，避免竞态条件
	rm.sessionMu.Lock()
	if existingSession, exists := rm.sessions[sessionID]; exists {
		rm.sessionMu.Unlock()
		return existingSession, nil
	}

	// 检查本节点角色
	isOldMember := contains(req.OldPartyIDs, rm.localNodeID)
	isNewMember := contains(req.NewPartyIDs, rm.localNodeID)

	if !isOldMember && !isNewMember {
		rm.sessionMu.Unlock()
		return nil, fmt.Errorf("local node is not part of resharing")
	}

	// 获取旧的密钥分片（如果是旧成员）
	var oldKeyData *keygen.LocalPartySaveData
	if isOldMember {
		var err error
		oldKeyData, err = rm.keyShareRepo.GetKeyShare(req.WalletID, rm.localNodeID)
		if err != nil {
			rm.sessionMu.Unlock()
			return nil, fmt.Errorf("failed to get old key share: %w", err)
		}
	}

	// 创建会话
	session := &ResharingSession{
		ID:           sessionID,
		WalletID:     req.WalletID,
		OldThreshold: req.OldThreshold,
		OldPartyIDs:  req.OldPartyIDs,
		NewThreshold: req.NewThreshold,
		NewPartyIDs:  req.NewPartyIDs,
		LocalNodeID:  rm.localNodeID,
		IsOldMember:  isOldMember,
		IsNewMember:  isNewMember,
		oldKeyData:   oldKeyData,
		outChan:      make(chan tss.Message, 100),
		endChan:      make(chan *keygen.LocalPartySaveData, 1),
		errChan:      make(chan error, 1),
		status:       "pending",
		keyShareRepo: rm.keyShareRepo,
		sessionRepo:  rm.sessionRepo,
		walletRepo:   rm.walletRepo,
		msgManager:   rm.msgManager,
		log:          logrus.WithField("session_id", sessionID),
	}

	// 创建PartyIDs
	session.oldPartyIDs = CreatePartyIDs(req.OldPartyIDs)
	session.newPartyIDs = CreatePartyIDs(req.NewPartyIDs)

	if isOldMember {
		session.ourOldPartyID = GeneratePartyID(rm.localNodeID, GetPartyIndex(session.oldPartyIDs, rm.localNodeID))
	}
	if isNewMember {
		session.ourNewPartyID = GeneratePartyID(rm.localNodeID, GetPartyIndex(session.newPartyIDs, rm.localNodeID))
	}

	// 创建重分享参数
	session.params = rm.createResharingParams(session)
	if session.params == nil {
		rm.sessionMu.Unlock()
		return nil, fmt.Errorf("failed to create resharing params")
	}

	// 创建P2P消息处理器
	session.msgHandler = rm.msgManager.CreateSession(sessionID, "resharing")

	// 保存会话状态到存储库（与StartResharing保持一致）
	sessionState := &types.SessionState{
		ID:        sessionID,
		Type:      "resharing",
		WalletID:  req.WalletID,
		Status:    "pending",
		PartyIDs:  append(req.OldPartyIDs, req.NewPartyIDs...),
		Threshold: req.NewThreshold,
		CreatedAt: time.Now(),
	}
	if err := rm.sessionRepo.SaveSession(sessionState); err != nil {
		rm.sessionMu.Unlock()
		return nil, fmt.Errorf("failed to save session: %w", err)
	}

	// 注册会话
	rm.sessions[sessionID] = session
	rm.sessionMu.Unlock()

	// 启动重分享
	go session.run(ctx, rm.timeout)

	return session, nil
}

// validateResharingRequest 验证重分享请求
// threshold 表示签名所需的最少节点数
func (rm *ResharingManager) validateResharingRequest(req *ResharingRequest) error {
	if req.OldThreshold < 2 {
		return fmt.Errorf("old threshold must be at least 2")
	}
	if req.NewThreshold < 2 {
		return fmt.Errorf("new threshold must be at least 2")
	}
	if len(req.OldPartyIDs) < req.OldThreshold {
		return fmt.Errorf("old parties must be at least old_threshold (%d)", req.OldThreshold)
	}
	if len(req.NewPartyIDs) < req.NewThreshold {
		return fmt.Errorf("new parties must be at least new_threshold (%d)", req.NewThreshold)
	}
	return nil
}

// createResharingParams 创建重分享参数
// threshold 表示签名所需的最少节点数，TSS 库需要 threshold-1
func (rm *ResharingManager) createResharingParams(session *ResharingSession) *tss.ReSharingParameters {
	oldCtx := tss.NewPeerContext(session.oldPartyIDs)
	newCtx := tss.NewPeerContext(session.newPartyIDs)

	var ourPartyID *tss.PartyID
	if session.IsOldMember {
		ourPartyID = session.ourOldPartyID
	} else {
		ourPartyID = session.ourNewPartyID
	}

	// 用户的 threshold 表示签名所需节点数，TSS 库需要 threshold-1
	params := tss.NewReSharingParameters(
		tss.S256(),
		oldCtx,
		newCtx,
		ourPartyID,
		len(session.OldPartyIDs),
		session.OldThreshold-1,
		len(session.NewPartyIDs),
		session.NewThreshold-1,
	)

	return params
}

// GetSession 获取会话
func (rm *ResharingManager) GetSession(sessionID string) (*ResharingSession, bool) {
	rm.sessionMu.RLock()
	defer rm.sessionMu.RUnlock()
	session, ok := rm.sessions[sessionID]
	return session, ok
}

// CleanupSession 清理会话
func (rm *ResharingManager) CleanupSession(sessionID string) {
	rm.sessionMu.Lock()
	defer rm.sessionMu.Unlock()

	if session, ok := rm.sessions[sessionID]; ok {
		rm.msgManager.CloseSession(sessionID)
		close(session.outChan)
		delete(rm.sessions, sessionID)
	}
}

// run 运行重分享会话
func (s *ResharingSession) run(ctx context.Context, timeout time.Duration) {
	s.log.WithFields(logrus.Fields{
		"is_old_member": s.IsOldMember,
		"is_new_member": s.IsNewMember,
	}).Info("Starting resharing party")

	// 设置状态
	s.mu.Lock()
	s.status = "running"
	s.mu.Unlock()

	// 创建TSS重分享参与方
	if s.IsOldMember {
		// 旧成员 - 持有密钥分片
		s.party = resharing.NewLocalParty(s.params, *s.oldKeyData, s.outChan, s.endChan).(*resharing.LocalParty)
	} else {
		// 新成员 - 不持有密钥分片
		s.party = resharing.NewLocalParty(s.params, keygen.LocalPartySaveData{}, s.outChan, s.endChan).(*resharing.LocalParty)
	}

	// 启动TSS协议
	go func() {
		if err := s.party.Start(); err != nil {
			s.errChan <- err.Cause()
		}
	}()

	// 创建超时上下文
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// 主循环
	for {
		select {
		case <-ctx.Done():
			s.handleError(fmt.Errorf("resharing timeout"))
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
			// 重分享完成
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
func (s *ResharingSession) sendMessage(msg tss.Message) error {
	wrapper, err := SerializeMessage(msg)
	if err != nil {
		return err
	}

	data, err := json.Marshal(wrapper)
	if err != nil {
		return err
	}

	// 获取所有参与方（旧+新）
	allParties := make([]string, 0, len(s.OldPartyIDs)+len(s.NewPartyIDs))
	allParties = append(allParties, s.OldPartyIDs...)
	for _, p := range s.NewPartyIDs {
		if !contains(s.OldPartyIDs, p) {
			allParties = append(allParties, p)
		}
	}

	dest := msg.GetTo()

	if msg.IsBroadcast() {
		// 广播消息
		return s.msgManager.BroadcastToParties(s.ID, allParties, data, 0)
	} else if len(dest) > 0 {
		// 点对点消息
		return s.msgManager.SendToParty(s.ID, dest[0].Id, data, 0)
	}

	return nil
}

// handleMessage 处理收到的消息
func (s *ResharingSession) handleMessage(p2pMsg *types.P2PMessage) error {
	s.log.WithFields(logrus.Fields{
		"from":  p2pMsg.From,
		"round": p2pMsg.Round,
	}).Debug("Received resharing message")

	// 反序列化消息
	var wrapper MessageWrapper
	if err := json.Unmarshal(p2pMsg.Data, &wrapper); err != nil {
		return fmt.Errorf("failed to unmarshal message wrapper: %w", err)
	}

	// 合并所有PartyID用于解析
	allPartyIDs := make(tss.SortedPartyIDs, 0, len(s.oldPartyIDs)+len(s.newPartyIDs))
	allPartyIDs = append(allPartyIDs, s.oldPartyIDs...)
	for _, pid := range s.newPartyIDs {
		found := false
		for _, existing := range allPartyIDs {
			if existing.Id == pid.Id {
				found = true
				break
			}
		}
		if !found {
			allPartyIDs = append(allPartyIDs, pid)
		}
	}

	// 解析TSS消息
	tssMsg, err := DeserializeMessage(&wrapper, allPartyIDs)
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
func (s *ResharingSession) handleSuccess(result *keygen.LocalPartySaveData) {
	s.mu.Lock()
	s.status = "completed"
	s.result = result
	s.mu.Unlock()

	// 只有新成员需要保存新的密钥分片
	if s.IsNewMember {
		pubKey, address, err := RecoverPublicKey(result)
		if err != nil {
			s.log.WithError(err).Error("Failed to recover public key")
			return
		}

		s.log.WithFields(logrus.Fields{
			"address":    address,
			"public_key": PublicKeyToHex(pubKey),
		}).Info("Resharing completed successfully")

		// 保存新的密钥分片
		if err := s.keyShareRepo.SaveKeyShare(s.WalletID, s.LocalNodeID, result); err != nil {
			s.log.WithError(err).Error("Failed to save new key share")
		}

		// 更新钱包信息
		wallet, err := s.walletRepo.GetWallet(s.WalletID)
		if err == nil {
			wallet.Threshold = s.NewThreshold
			wallet.TotalParts = len(s.NewPartyIDs)
			wallet.PartyIDs = s.NewPartyIDs
			if err := s.walletRepo.SaveWallet(wallet); err != nil {
				s.log.WithError(err).Error("Failed to update wallet info")
			}
		}
	} else {
		s.log.Info("Resharing completed (old member - share invalidated)")
	}

	// 更新会话状态
	if err := s.sessionRepo.UpdateSessionStatus(s.ID, "completed"); err != nil {
		s.log.WithError(err).Error("Failed to update session status")
	}
}

// handleError 处理错误
func (s *ResharingSession) handleError(err error) {
	s.mu.Lock()
	s.status = "failed"
	s.error = err
	s.mu.Unlock()

	s.log.WithError(err).Error("Resharing failed")

	// 更新会话状态
	if updateErr := s.sessionRepo.UpdateSessionError(s.ID, err.Error()); updateErr != nil {
		s.log.WithError(updateErr).Error("Failed to update session error")
	}
}

// GetStatus 获取状态
func (s *ResharingSession) GetStatus() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.status
}

// GetResult 获取结果
func (s *ResharingSession) GetResult() (*keygen.LocalPartySaveData, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.status != "completed" {
		return nil, fmt.Errorf("resharing not completed, status: %s", s.status)
	}
	if s.error != nil {
		return nil, s.error
	}
	return s.result, nil
}

// WaitForCompletion 等待完成
func (s *ResharingSession) WaitForCompletion(timeout time.Duration) (*keygen.LocalPartySaveData, error) {
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

	return nil, fmt.Errorf("resharing timeout")
}

// contains 检查切片是否包含元素
func contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}
