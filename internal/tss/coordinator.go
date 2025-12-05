package tss

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/339-Labs/wallet-platform-mpc-go/internal/p2p"
	"github.com/339-Labs/wallet-platform-mpc-go/pkg/types"
)

// 协调消息类型
const (
	MsgTypeKeygenInit    = "keygen_init"
	MsgTypeKeygenJoin    = "keygen_join"
	MsgTypeKeygenReady   = "keygen_ready"
	MsgTypeSignInit      = "sign_init"
	MsgTypeSignJoin      = "sign_join"
	MsgTypeReshareInit   = "reshare_init"
	MsgTypeReshareJoin   = "reshare_join"
)

// SessionCoordinator 会话协调器 - 负责协调多节点TSS会话
type SessionCoordinator struct {
	p2pHost      *p2p.P2PHost
	msgManager   *p2p.MessageManager
	keygenMgr    *KeygenManager
	signingMgr   *SigningManager
	resharingMgr *ResharingManager
	localNodeID  string

	// 等待中的会话
	pendingSessions map[string]*PendingSession
	sessionMu       sync.RWMutex

	ctx    context.Context
	cancel context.CancelFunc
	log    *logrus.Entry
}

// PendingSession 等待中的会话
type PendingSession struct {
	ID           string
	Type         string // keygen, sign, reshare
	InitiatorID  string
	Request      interface{}
	ReadyNodes   map[string]bool
	RequiredNodes []string
	CreatedAt    time.Time
}

// SessionInitMessage 会话初始化消息
type SessionInitMessage struct {
	SessionID   string          `json:"session_id"`
	Type        string          `json:"type"`
	InitiatorID string          `json:"initiator_id"`
	Request     json.RawMessage `json:"request"`
}

// SessionJoinMessage 会话加入消息
type SessionJoinMessage struct {
	SessionID string `json:"session_id"`
	NodeID    string `json:"node_id"`
}

// NewSessionCoordinator 创建会话协调器
func NewSessionCoordinator(
	ctx context.Context,
	p2pHost *p2p.P2PHost,
	msgManager *p2p.MessageManager,
	localNodeID string,
) *SessionCoordinator {
	ctx, cancel := context.WithCancel(ctx)

	sc := &SessionCoordinator{
		p2pHost:         p2pHost,
		msgManager:      msgManager,
		localNodeID:     localNodeID,
		pendingSessions: make(map[string]*PendingSession),
		ctx:             ctx,
		cancel:          cancel,
		log:             logrus.WithField("component", "session_coordinator"),
	}

	// 注册消息处理器
	p2pHost.RegisterHandler(MsgTypeKeygenInit, sc.handleKeygenInit)
	p2pHost.RegisterHandler(MsgTypeKeygenJoin, sc.handleKeygenJoin)
	p2pHost.RegisterHandler(MsgTypeKeygenReady, sc.handleKeygenReady)
	p2pHost.RegisterHandler(MsgTypeSignInit, sc.handleSignInit)
	p2pHost.RegisterHandler(MsgTypeSignJoin, sc.handleSignJoin)

	return sc
}

// SetManagers 设置TSS管理器（避免循环依赖）
func (sc *SessionCoordinator) SetManagers(keygenMgr *KeygenManager, signingMgr *SigningManager, resharingMgr *ResharingManager) {
	sc.keygenMgr = keygenMgr
	sc.signingMgr = signingMgr
	sc.resharingMgr = resharingMgr
}

// Start 启动协调器
func (sc *SessionCoordinator) Start() error {
	sc.log.Info("Session coordinator started")
	return nil
}

// Stop 停止协调器
func (sc *SessionCoordinator) Stop() error {
	sc.cancel()
	sc.log.Info("Session coordinator stopped")
	return nil
}

// InitiateKeygen 发起密钥生成 - 协调所有节点
func (sc *SessionCoordinator) InitiateKeygen(ctx context.Context, req *types.KeygenRequest) (*KeygenSession, error) {
	sc.log.WithFields(logrus.Fields{
		"wallet_id":  req.WalletID,
		"party_ids":  req.PartyIDs,
		"threshold":  req.Threshold,
	}).Info("Initiating coordinated keygen")

	// 序列化请求
	reqData, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	// 生成会话ID
	sessionID := fmt.Sprintf("keygen-%s-%d", req.WalletID, time.Now().UnixNano())

	// 创建待处理会话
	pending := &PendingSession{
		ID:            sessionID,
		Type:          "keygen",
		InitiatorID:   sc.localNodeID,
		Request:       req,
		ReadyNodes:    make(map[string]bool),
		RequiredNodes: req.PartyIDs,
		CreatedAt:     time.Now(),
	}
	pending.ReadyNodes[sc.localNodeID] = true

	sc.sessionMu.Lock()
	sc.pendingSessions[sessionID] = pending
	sc.sessionMu.Unlock()

	// 广播初始化消息给其他节点
	initMsg := &SessionInitMessage{
		SessionID:   sessionID,
		Type:        "keygen",
		InitiatorID: sc.localNodeID,
		Request:     reqData,
	}

	initData, _ := json.Marshal(initMsg)
	p2pMsg := &types.P2PMessage{
		Type:      MsgTypeKeygenInit,
		SessionID: sessionID,
		From:      sc.localNodeID,
		Data:      initData,
		Timestamp: time.Now().UnixNano(),
	}

	if err := sc.p2pHost.BroadcastMessage(p2pMsg); err != nil {
		return nil, fmt.Errorf("failed to broadcast keygen init: %w", err)
	}

	sc.log.WithField("session_id", sessionID).Info("Broadcast keygen init message")

	// 等待所有节点准备好
	if err := sc.waitForNodes(ctx, sessionID, req.PartyIDs, 30*time.Second); err != nil {
		return nil, fmt.Errorf("failed to gather nodes: %w", err)
	}

	// 所有节点准备好，启动本地keygen
	sc.log.WithField("session_id", sessionID).Info("All nodes ready, starting keygen")
	
	// 使用会话ID作为请求的一部分
	internalReq := &types.KeygenRequest{
		WalletID:   req.WalletID,
		Threshold:  req.Threshold,
		TotalParts: req.TotalParts,
		PartyIDs:   req.PartyIDs,
	}

	return sc.keygenMgr.StartKeygenWithSession(ctx, sessionID, internalReq)
}

// waitForNodes 等待所有节点准备好
func (sc *SessionCoordinator) waitForNodes(ctx context.Context, sessionID string, requiredNodes []string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			sc.sessionMu.RLock()
			pending, ok := sc.pendingSessions[sessionID]
			sc.sessionMu.RUnlock()

			if !ok {
				return fmt.Errorf("session not found")
			}

			allReady := true
			for _, nodeID := range requiredNodes {
				if !pending.ReadyNodes[nodeID] {
					allReady = false
					break
				}
			}

			if allReady {
				sc.log.WithFields(logrus.Fields{
					"session_id": sessionID,
					"nodes":      requiredNodes,
				}).Info("All nodes are ready")
				return nil
			}
		}
	}

	return fmt.Errorf("timeout waiting for nodes")
}

// handleKeygenInit 处理keygen初始化消息
func (sc *SessionCoordinator) handleKeygenInit(msg *types.P2PMessage) error {
	sc.log.WithFields(logrus.Fields{
		"from":       msg.From,
		"session_id": msg.SessionID,
	}).Info("Received keygen init message")

	var initMsg SessionInitMessage
	if err := json.Unmarshal(msg.Data, &initMsg); err != nil {
		return fmt.Errorf("failed to unmarshal init message: %w", err)
	}

	// 解析请求
	var req types.KeygenRequest
	if err := json.Unmarshal(initMsg.Request, &req); err != nil {
		return fmt.Errorf("failed to unmarshal keygen request: %w", err)
	}

	// 检查本节点是否在参与方列表中
	isParticipant := false
	for _, pid := range req.PartyIDs {
		if pid == sc.localNodeID {
			isParticipant = true
			break
		}
	}

	if !isParticipant {
		sc.log.Debug("Not a participant in this keygen session")
		return nil
	}

	// 创建本地待处理会话
	pending := &PendingSession{
		ID:            initMsg.SessionID,
		Type:          "keygen",
		InitiatorID:   initMsg.InitiatorID,
		Request:       &req,
		ReadyNodes:    make(map[string]bool),
		RequiredNodes: req.PartyIDs,
		CreatedAt:     time.Now(),
	}

	sc.sessionMu.Lock()
	sc.pendingSessions[initMsg.SessionID] = pending
	sc.sessionMu.Unlock()

	// 发送加入确认
	joinMsg := &SessionJoinMessage{
		SessionID: initMsg.SessionID,
		NodeID:    sc.localNodeID,
	}
	joinData, _ := json.Marshal(joinMsg)

	p2pMsg := &types.P2PMessage{
		Type:      MsgTypeKeygenJoin,
		SessionID: initMsg.SessionID,
		From:      sc.localNodeID,
		Data:      joinData,
		Timestamp: time.Now().UnixNano(),
	}

	if err := sc.p2pHost.BroadcastMessage(p2pMsg); err != nil {
		return fmt.Errorf("failed to send join message: %w", err)
	}

	sc.log.WithField("session_id", initMsg.SessionID).Info("Sent keygen join message")

	// 启动本地keygen会话（等待ready信号）
	go sc.waitAndStartKeygen(initMsg.SessionID, &req)

	return nil
}

// handleKeygenJoin 处理keygen加入消息
func (sc *SessionCoordinator) handleKeygenJoin(msg *types.P2PMessage) error {
	sc.log.WithFields(logrus.Fields{
		"from":       msg.From,
		"session_id": msg.SessionID,
	}).Info("Received keygen join message")

	var joinMsg SessionJoinMessage
	if err := json.Unmarshal(msg.Data, &joinMsg); err != nil {
		return fmt.Errorf("failed to unmarshal join message: %w", err)
	}

	sc.sessionMu.Lock()
	if pending, ok := sc.pendingSessions[joinMsg.SessionID]; ok {
		pending.ReadyNodes[joinMsg.NodeID] = true
		sc.log.WithFields(logrus.Fields{
			"session_id": joinMsg.SessionID,
			"node_id":    joinMsg.NodeID,
			"ready_count": len(pending.ReadyNodes),
			"required":   len(pending.RequiredNodes),
		}).Info("Node joined session")
	}
	sc.sessionMu.Unlock()

	return nil
}

// handleKeygenReady 处理keygen准备完成消息
func (sc *SessionCoordinator) handleKeygenReady(msg *types.P2PMessage) error {
	sc.log.WithFields(logrus.Fields{
		"from":       msg.From,
		"session_id": msg.SessionID,
	}).Debug("Received keygen ready message")

	return nil
}

// waitAndStartKeygen 等待并启动keygen
func (sc *SessionCoordinator) waitAndStartKeygen(sessionID string, req *types.KeygenRequest) {
	// 等待一小段时间让其他节点也准备好
	time.Sleep(2 * time.Second)

	sc.log.WithField("session_id", sessionID).Info("Starting keygen as participant")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	internalReq := &types.KeygenRequest{
		WalletID:   req.WalletID,
		Threshold:  req.Threshold,
		TotalParts: req.TotalParts,
		PartyIDs:   req.PartyIDs,
	}

	session, err := sc.keygenMgr.StartKeygenWithSession(ctx, sessionID, internalReq)
	if err != nil {
		sc.log.WithError(err).Error("Failed to start keygen as participant")
		return
	}

	// 等待完成
	result, err := session.WaitForCompletion(5 * time.Minute)
	if err != nil {
		sc.log.WithError(err).Error("Keygen failed as participant")
		return
	}

	sc.log.WithFields(logrus.Fields{
		"session_id": sessionID,
		"wallet_id":  req.WalletID,
	}).Info("Keygen completed as participant")

	_ = result // 结果已保存
}

// handleSignInit 处理签名初始化消息
func (sc *SessionCoordinator) handleSignInit(msg *types.P2PMessage) error {
	sc.log.WithFields(logrus.Fields{
		"from":       msg.From,
		"session_id": msg.SessionID,
	}).Info("Received sign init message")

	var initMsg SessionInitMessage
	if err := json.Unmarshal(msg.Data, &initMsg); err != nil {
		return fmt.Errorf("failed to unmarshal init message: %w", err)
	}

	var req types.SignRequest
	if err := json.Unmarshal(initMsg.Request, &req); err != nil {
		return fmt.Errorf("failed to unmarshal sign request: %w", err)
	}

	// 检查本节点是否在签名者列表中
	isParticipant := false
	for _, pid := range req.PartyIDs {
		if pid == sc.localNodeID {
			isParticipant = true
			break
		}
	}

	if !isParticipant {
		return nil
	}

	// 启动本地签名会话
	go sc.startSigningAsParticipant(initMsg.SessionID, &req)

	return nil
}

// handleSignJoin 处理签名加入消息
func (sc *SessionCoordinator) handleSignJoin(msg *types.P2PMessage) error {
	return nil
}

// startSigningAsParticipant 作为参与者启动签名
func (sc *SessionCoordinator) startSigningAsParticipant(sessionID string, req *types.SignRequest) {
	time.Sleep(1 * time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	session, err := sc.signingMgr.JoinSigning(ctx, sessionID, req)
	if err != nil {
		sc.log.WithError(err).Error("Failed to join signing session")
		return
	}

	result, err := session.WaitForCompletion(2 * time.Minute)
	if err != nil {
		sc.log.WithError(err).Error("Signing failed as participant")
		return
	}

	sc.log.WithFields(logrus.Fields{
		"session_id": sessionID,
		"signature":  result.Signature,
	}).Info("Signing completed as participant")
}

// InitiateSigning 发起签名 - 协调所有节点
func (sc *SessionCoordinator) InitiateSigning(ctx context.Context, req *types.SignRequest) (*SigningSession, error) {
	sc.log.WithFields(logrus.Fields{
		"wallet_id": req.WalletID,
		"party_ids": req.PartyIDs,
	}).Info("Initiating coordinated signing")

	reqData, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	sessionID := req.RequestID
	if sessionID == "" {
		sessionID = fmt.Sprintf("sign-%s-%d", req.WalletID, time.Now().UnixNano())
	}

	// 广播初始化消息
	initMsg := &SessionInitMessage{
		SessionID:   sessionID,
		Type:        "sign",
		InitiatorID: sc.localNodeID,
		Request:     reqData,
	}

	initData, _ := json.Marshal(initMsg)
	p2pMsg := &types.P2PMessage{
		Type:      MsgTypeSignInit,
		SessionID: sessionID,
		From:      sc.localNodeID,
		Data:      initData,
		Timestamp: time.Now().UnixNano(),
	}

	if err := sc.p2pHost.BroadcastMessage(p2pMsg); err != nil {
		return nil, fmt.Errorf("failed to broadcast sign init: %w", err)
	}

	// 等待一下让其他节点准备
	time.Sleep(1 * time.Second)

	// 启动本地签名
	req.RequestID = sessionID
	return sc.signingMgr.StartSigning(ctx, req)
}
