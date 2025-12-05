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
	MsgTypeSignReady     = "sign_ready"
	MsgTypeReshareInit   = "reshare_init"
	MsgTypeReshareJoin   = "reshare_join"
	MsgTypeReshareReady  = "reshare_ready"
)

// SessionCoordinator 会话协调器 - 负责在节点间同步会话
type SessionCoordinator struct {
	p2pHost      *p2p.P2PHost
	pubsub       *p2p.PubSubManager
	keygenMgr    *KeygenManager
	signingMgr   *SigningManager
	resharingMgr *ResharingManager
	localNodeID  string
	
	// 等待中的会话
	pendingSessions map[string]*PendingSession
	pendingMu       sync.RWMutex
	
	ctx    context.Context
	cancel context.CancelFunc
	log    *logrus.Entry
}

// PendingSession 待启动的会话
type PendingSession struct {
	SessionID   string
	Type        string // keygen, sign, reshare
	Request     interface{}
	ReadyNodes  map[string]bool
	RequiredNum int
	CreatedAt   time.Time
}

// KeygenInitMessage keygen初始化消息
type KeygenInitMessage struct {
	SessionID  string   `json:"session_id"`
	WalletID   string   `json:"wallet_id"`
	Threshold  int      `json:"threshold"`
	TotalParts int      `json:"total_parts"`
	PartyIDs   []string `json:"party_ids"`
	Initiator  string   `json:"initiator"`
}

// SignInitMessage 签名初始化消息
type SignInitMessage struct {
	SessionID  string   `json:"session_id"`
	WalletID   string   `json:"wallet_id"`
	Message    string   `json:"message"`
	PartyIDs   []string `json:"party_ids"`
	Initiator  string   `json:"initiator"`
}

// JoinMessage 加入会话消息
type JoinMessage struct {
	SessionID string `json:"session_id"`
	NodeID    string `json:"node_id"`
}

// NewSessionCoordinator 创建会话协调器
func NewSessionCoordinator(
	ctx context.Context,
	p2pHost *p2p.P2PHost,
	keygenMgr *KeygenManager,
	signingMgr *SigningManager,
	resharingMgr *ResharingManager,
	localNodeID string,
) *SessionCoordinator {
	ctx, cancel := context.WithCancel(ctx)
	
	sc := &SessionCoordinator{
		p2pHost:         p2pHost,
		pubsub:          p2pHost.GetPubSub(),
		keygenMgr:       keygenMgr,
		signingMgr:      signingMgr,
		resharingMgr:    resharingMgr,
		localNodeID:     localNodeID,
		pendingSessions: make(map[string]*PendingSession),
		ctx:             ctx,
		cancel:          cancel,
		log:             logrus.WithField("component", "session_coordinator"),
	}
	
	return sc
}

// Start 启动协调器
func (sc *SessionCoordinator) Start() error {
	// 注册消息处理器
	sc.p2pHost.RegisterHandler(MsgTypeKeygenInit, sc.handleKeygenInit)
	sc.p2pHost.RegisterHandler(MsgTypeKeygenJoin, sc.handleKeygenJoin)
	sc.p2pHost.RegisterHandler(MsgTypeKeygenReady, sc.handleKeygenReady)
	sc.p2pHost.RegisterHandler(MsgTypeSignInit, sc.handleSignInit)
	sc.p2pHost.RegisterHandler(MsgTypeSignJoin, sc.handleSignJoin)
	
	// 清理过期会话
	go sc.cleanupLoop()
	
	sc.log.Info("Session coordinator started")
	return nil
}

// Stop 停止协调器
func (sc *SessionCoordinator) Stop() error {
	sc.cancel()
	sc.log.Info("Session coordinator stopped")
	return nil
}

// InitiateKeygen 发起keygen会话
func (sc *SessionCoordinator) InitiateKeygen(ctx context.Context, req *types.KeygenRequest) (string, error) {
	sessionID := req.WalletID + "-" + fmt.Sprintf("%d", time.Now().UnixNano())
	
	sc.log.WithFields(logrus.Fields{
		"session_id": sessionID,
		"wallet_id":  req.WalletID,
		"parties":    req.PartyIDs,
	}).Info("Initiating keygen session")
	
	// 创建待处理会话
	pending := &PendingSession{
		SessionID:   sessionID,
		Type:        "keygen",
		Request:     req,
		ReadyNodes:  make(map[string]bool),
		RequiredNum: len(req.PartyIDs),
		CreatedAt:   time.Now(),
	}
	pending.ReadyNodes[sc.localNodeID] = true
	
	sc.pendingMu.Lock()
	sc.pendingSessions[sessionID] = pending
	sc.pendingMu.Unlock()
	
	// 广播初始化消息
	initMsg := &KeygenInitMessage{
		SessionID:  sessionID,
		WalletID:   req.WalletID,
		Threshold:  req.Threshold,
		TotalParts: req.TotalParts,
		PartyIDs:   req.PartyIDs,
		Initiator:  sc.localNodeID,
	}
	
	data, _ := json.Marshal(initMsg)
	p2pMsg := &types.P2PMessage{
		Type:      MsgTypeKeygenInit,
		SessionID: sessionID,
		From:      sc.localNodeID,
		Data:      data,
		Timestamp: time.Now().UnixNano(),
	}
	
	if err := sc.p2pHost.BroadcastMessage(p2pMsg); err != nil {
		return "", fmt.Errorf("failed to broadcast keygen init: %w", err)
	}
	
	// 等待所有节点就绪
	if err := sc.waitForReady(ctx, sessionID, req.PartyIDs); err != nil {
		return "", err
	}
	
	return sessionID, nil
}

// waitForReady 等待所有节点就绪
func (sc *SessionCoordinator) waitForReady(ctx context.Context, sessionID string, partyIDs []string) error {
	timeout := time.After(60 * time.Second)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timeout:
			sc.pendingMu.RLock()
			pending := sc.pendingSessions[sessionID]
			readyCount := 0
			if pending != nil {
				readyCount = len(pending.ReadyNodes)
			}
			sc.pendingMu.RUnlock()
			return fmt.Errorf("timeout waiting for nodes, ready: %d/%d", readyCount, len(partyIDs))
		case <-ticker.C:
			sc.pendingMu.RLock()
			pending := sc.pendingSessions[sessionID]
			if pending != nil && len(pending.ReadyNodes) >= len(partyIDs) {
				sc.pendingMu.RUnlock()
				sc.log.WithField("session_id", sessionID).Info("All nodes ready")
				return nil
			}
			sc.pendingMu.RUnlock()
		}
	}
}

// handleKeygenInit 处理keygen初始化消息
func (sc *SessionCoordinator) handleKeygenInit(msg *types.P2PMessage) error {
	var initMsg KeygenInitMessage
	if err := json.Unmarshal(msg.Data, &initMsg); err != nil {
		return err
	}
	
	sc.log.WithFields(logrus.Fields{
		"session_id": initMsg.SessionID,
		"wallet_id":  initMsg.WalletID,
		"initiator":  initMsg.Initiator,
	}).Info("Received keygen init")
	
	// 检查是否是参与方
	isParty := false
	for _, pid := range initMsg.PartyIDs {
		if pid == sc.localNodeID {
			isParty = true
			break
		}
	}
	
	if !isParty {
		sc.log.Debug("Not a party member, ignoring")
		return nil
	}
	
	// 如果是发起者，已经创建了pending session
	if initMsg.Initiator == sc.localNodeID {
		return nil
	}
	
	// 创建本地keygen会话
	req := &types.KeygenRequest{
		WalletID:   initMsg.WalletID,
		Threshold:  initMsg.Threshold,
		TotalParts: initMsg.TotalParts,
		PartyIDs:   initMsg.PartyIDs,
	}
	
	// 使用JoinKeygen而不是StartKeygen
	_, err := sc.keygenMgr.JoinKeygen(sc.ctx, initMsg.SessionID, req)
	if err != nil {
		sc.log.WithError(err).Error("Failed to join keygen")
		return err
	}
	
	// 发送加入确认
	joinMsg := &JoinMessage{
		SessionID: initMsg.SessionID,
		NodeID:    sc.localNodeID,
	}
	data, _ := json.Marshal(joinMsg)
	
	p2pMsg := &types.P2PMessage{
		Type:      MsgTypeKeygenJoin,
		SessionID: initMsg.SessionID,
		From:      sc.localNodeID,
		Data:      data,
		Timestamp: time.Now().UnixNano(),
	}
	
	return sc.p2pHost.BroadcastMessage(p2pMsg)
}

// handleKeygenJoin 处理keygen加入消息
func (sc *SessionCoordinator) handleKeygenJoin(msg *types.P2PMessage) error {
	var joinMsg JoinMessage
	if err := json.Unmarshal(msg.Data, &joinMsg); err != nil {
		return err
	}
	
	sc.log.WithFields(logrus.Fields{
		"session_id": joinMsg.SessionID,
		"node_id":    joinMsg.NodeID,
	}).Info("Node joined keygen session")
	
	sc.pendingMu.Lock()
	if pending, ok := sc.pendingSessions[joinMsg.SessionID]; ok {
		pending.ReadyNodes[joinMsg.NodeID] = true
	}
	sc.pendingMu.Unlock()
	
	return nil
}

// handleKeygenReady 处理keygen就绪消息
func (sc *SessionCoordinator) handleKeygenReady(msg *types.P2PMessage) error {
	// 可用于额外的同步
	return nil
}

// InitiateSign 发起签名会话
func (sc *SessionCoordinator) InitiateSign(ctx context.Context, req *types.SignRequest) (string, error) {
	sessionID := req.RequestID
	if sessionID == "" {
		sessionID = req.WalletID + "-sign-" + fmt.Sprintf("%d", time.Now().UnixNano())
	}
	
	sc.log.WithFields(logrus.Fields{
		"session_id": sessionID,
		"wallet_id":  req.WalletID,
		"parties":    req.PartyIDs,
	}).Info("Initiating sign session")
	
	// 创建待处理会话
	pending := &PendingSession{
		SessionID:   sessionID,
		Type:        "sign",
		Request:     req,
		ReadyNodes:  make(map[string]bool),
		RequiredNum: len(req.PartyIDs),
		CreatedAt:   time.Now(),
	}
	pending.ReadyNodes[sc.localNodeID] = true
	
	sc.pendingMu.Lock()
	sc.pendingSessions[sessionID] = pending
	sc.pendingMu.Unlock()
	
	// 广播初始化消息
	initMsg := &SignInitMessage{
		SessionID: sessionID,
		WalletID:  req.WalletID,
		Message:   req.Message,
		PartyIDs:  req.PartyIDs,
		Initiator: sc.localNodeID,
	}
	
	data, _ := json.Marshal(initMsg)
	p2pMsg := &types.P2PMessage{
		Type:      MsgTypeSignInit,
		SessionID: sessionID,
		From:      sc.localNodeID,
		Data:      data,
		Timestamp: time.Now().UnixNano(),
	}
	
	if err := sc.p2pHost.BroadcastMessage(p2pMsg); err != nil {
		return "", fmt.Errorf("failed to broadcast sign init: %w", err)
	}
	
	// 等待所有节点就绪
	if err := sc.waitForReady(ctx, sessionID, req.PartyIDs); err != nil {
		return "", err
	}
	
	return sessionID, nil
}

// handleSignInit 处理签名初始化消息
func (sc *SessionCoordinator) handleSignInit(msg *types.P2PMessage) error {
	var initMsg SignInitMessage
	if err := json.Unmarshal(msg.Data, &initMsg); err != nil {
		return err
	}
	
	sc.log.WithFields(logrus.Fields{
		"session_id": initMsg.SessionID,
		"wallet_id":  initMsg.WalletID,
		"initiator":  initMsg.Initiator,
	}).Info("Received sign init")
	
	// 检查是否是参与方
	isParty := false
	for _, pid := range initMsg.PartyIDs {
		if pid == sc.localNodeID {
			isParty = true
			break
		}
	}
	
	if !isParty {
		return nil
	}
	
	if initMsg.Initiator == sc.localNodeID {
		return nil
	}
	
	// 加入签名会话
	req := &types.SignRequest{
		WalletID:  initMsg.WalletID,
		Message:   initMsg.Message,
		PartyIDs:  initMsg.PartyIDs,
		RequestID: initMsg.SessionID,
	}
	
	_, err := sc.signingMgr.JoinSigning(sc.ctx, initMsg.SessionID, req)
	if err != nil {
		sc.log.WithError(err).Error("Failed to join signing")
		return err
	}
	
	// 发送加入确认
	joinMsg := &JoinMessage{
		SessionID: initMsg.SessionID,
		NodeID:    sc.localNodeID,
	}
	data, _ := json.Marshal(joinMsg)
	
	p2pMsg := &types.P2PMessage{
		Type:      MsgTypeSignJoin,
		SessionID: initMsg.SessionID,
		From:      sc.localNodeID,
		Data:      data,
		Timestamp: time.Now().UnixNano(),
	}
	
	return sc.p2pHost.BroadcastMessage(p2pMsg)
}

// handleSignJoin 处理签名加入消息
func (sc *SessionCoordinator) handleSignJoin(msg *types.P2PMessage) error {
	var joinMsg JoinMessage
	if err := json.Unmarshal(msg.Data, &joinMsg); err != nil {
		return err
	}
	
	sc.log.WithFields(logrus.Fields{
		"session_id": joinMsg.SessionID,
		"node_id":    joinMsg.NodeID,
	}).Info("Node joined sign session")
	
	sc.pendingMu.Lock()
	if pending, ok := sc.pendingSessions[joinMsg.SessionID]; ok {
		pending.ReadyNodes[joinMsg.NodeID] = true
	}
	sc.pendingMu.Unlock()
	
	return nil
}

// cleanupLoop 清理过期会话
func (sc *SessionCoordinator) cleanupLoop() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	
	for {
		select {
		case <-sc.ctx.Done():
			return
		case <-ticker.C:
			sc.pendingMu.Lock()
			for id, pending := range sc.pendingSessions {
				if time.Since(pending.CreatedAt) > 10*time.Minute {
					delete(sc.pendingSessions, id)
				}
			}
			sc.pendingMu.Unlock()
		}
	}
}

// GetPendingSession 获取待处理会话
func (sc *SessionCoordinator) GetPendingSession(sessionID string) *PendingSession {
	sc.pendingMu.RLock()
	defer sc.pendingMu.RUnlock()
	return sc.pendingSessions[sessionID]
}
