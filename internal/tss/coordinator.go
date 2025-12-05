package tss

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/sirupsen/logrus"

	"github.com/339-Labs/wallet-platform-mpc-go/internal/p2p"
	"github.com/339-Labs/wallet-platform-mpc-go/pkg/types"
)

// 协调消息类型
const (
	MsgTypeKeygenInit    = "keygen_init"
	MsgTypeKeygenReady   = "keygen_ready"
	MsgTypeSignInit      = "sign_init"
	MsgTypeSignReady     = "sign_ready"
	MsgTypeResharingInit = "resharing_init"
	MsgTypeResharingReady = "resharing_ready"
)

// Coordinator TSS会话协调器
// 负责在所有参与方之间同步启动keygen/sign/resharing会话
type Coordinator struct {
	p2pHost     *p2p.P2PHost
	msgManager  *p2p.MessageManager
	keygenMgr   *KeygenManager
	signingMgr  *SigningManager
	resharingMgr *ResharingManager
	localNodeID string
	
	// 等待中的会话
	pendingSessions map[string]*PendingSession
	pendingMu       sync.RWMutex
	
	ctx    context.Context
	cancel context.CancelFunc
	log    *logrus.Entry
}

// PendingSession 等待中的会话
type PendingSession struct {
	SessionID   string
	Type        string // keygen, sign, resharing
	Request     interface{}
	ReadyNodes  map[string]bool
	RequiredNodes []string
	CreatedAt   time.Time
	ResultChan  chan error
}

// InitMessage 初始化消息
type InitMessage struct {
	SessionID   string          `json:"session_id"`
	Type        string          `json:"type"`
	Request     json.RawMessage `json:"request"`
	InitiatorID string          `json:"initiator_id"`
	Timestamp   int64           `json:"timestamp"`
}

// ReadyMessage 就绪消息
type ReadyMessage struct {
	SessionID string `json:"session_id"`
	NodeID    string `json:"node_id"`
	Type      string `json:"type"`
}

// NewCoordinator 创建协调器
func NewCoordinator(
	ctx context.Context,
	p2pHost *p2p.P2PHost,
	msgManager *p2p.MessageManager,
	localNodeID string,
) *Coordinator {
	ctx, cancel := context.WithCancel(ctx)
	
	c := &Coordinator{
		p2pHost:         p2pHost,
		msgManager:      msgManager,
		localNodeID:     localNodeID,
		pendingSessions: make(map[string]*PendingSession),
		ctx:             ctx,
		cancel:          cancel,
		log:             logrus.WithField("component", "coordinator"),
	}
	
	// 注册消息处理器
	p2pHost.RegisterHandler(MsgTypeKeygenInit, c.handleKeygenInit)
	p2pHost.RegisterHandler(MsgTypeKeygenReady, c.handleKeygenReady)
	p2pHost.RegisterHandler(MsgTypeSignInit, c.handleSignInit)
	p2pHost.RegisterHandler(MsgTypeSignReady, c.handleSignReady)
	
	return c
}

// SetManagers 设置管理器（解决循环依赖）
func (c *Coordinator) SetManagers(keygenMgr *KeygenManager, signingMgr *SigningManager, resharingMgr *ResharingManager) {
	c.keygenMgr = keygenMgr
	c.signingMgr = signingMgr
	c.resharingMgr = resharingMgr
}

// Start 启动协调器
func (c *Coordinator) Start() error {
	c.log.Info("Coordinator started")
	return nil
}

// Stop 停止协调器
func (c *Coordinator) Stop() error {
	c.cancel()
	c.log.Info("Coordinator stopped")
	return nil
}

// InitiateKeygen 发起keygen（协调所有节点）
func (c *Coordinator) InitiateKeygen(ctx context.Context, req *types.KeygenRequest) (string, error) {
	sessionID := uuid.New().String()
	
	c.log.WithFields(logrus.Fields{
		"session_id": sessionID,
		"wallet_id":  req.WalletID,
		"parties":    req.PartyIDs,
	}).Info("Initiating coordinated keygen")
	
	// 创建等待会话
	pending := &PendingSession{
		SessionID:     sessionID,
		Type:          "keygen",
		Request:       req,
		ReadyNodes:    make(map[string]bool),
		RequiredNodes: req.PartyIDs,
		CreatedAt:     time.Now(),
		ResultChan:    make(chan error, 1),
	}
	
	c.pendingMu.Lock()
	c.pendingSessions[sessionID] = pending
	c.pendingMu.Unlock()
	
	// 序列化请求
	reqData, err := json.Marshal(req)
	if err != nil {
		return "", fmt.Errorf("failed to marshal request: %w", err)
	}
	
	// 广播初始化消息
	initMsg := &InitMessage{
		SessionID:   sessionID,
		Type:        "keygen",
		Request:     reqData,
		InitiatorID: c.localNodeID,
		Timestamp:   time.Now().UnixNano(),
	}
	
	initData, _ := json.Marshal(initMsg)
	p2pMsg := &types.P2PMessage{
		Type:      MsgTypeKeygenInit,
		SessionID: sessionID,
		From:      c.localNodeID,
		Data:      initData,
		Timestamp: time.Now().UnixNano(),
	}
	
	if err := c.p2pHost.BroadcastMessage(p2pMsg); err != nil {
		return "", fmt.Errorf("failed to broadcast init message: %w", err)
	}
	
	// 本地也标记为ready
	c.markNodeReady(sessionID, c.localNodeID)
	
	// 等待所有节点就绪或超时
	timeout := time.After(30 * time.Second)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-timeout:
			return "", fmt.Errorf("timeout waiting for all nodes to be ready")
		case <-ticker.C:
			if c.allNodesReady(sessionID) {
				c.log.WithField("session_id", sessionID).Info("All nodes ready, starting keygen")
				return sessionID, nil
			}
		}
	}
}

// handleKeygenInit 处理keygen初始化消息
func (c *Coordinator) handleKeygenInit(msg *types.P2PMessage) error {
	var initMsg InitMessage
	if err := json.Unmarshal(msg.Data, &initMsg); err != nil {
		return fmt.Errorf("failed to unmarshal init message: %w", err)
	}
	
	// 忽略自己发起的
	if initMsg.InitiatorID == c.localNodeID {
		return nil
	}
	
	c.log.WithFields(logrus.Fields{
		"session_id": initMsg.SessionID,
		"initiator":  initMsg.InitiatorID,
	}).Info("Received keygen init")
	
	// 解析请求
	var req types.KeygenRequest
	if err := json.Unmarshal(initMsg.Request, &req); err != nil {
		return fmt.Errorf("failed to unmarshal keygen request: %w", err)
	}
	
	// 检查自己是否在参与方列表中
	isParty := false
	for _, pid := range req.PartyIDs {
		if pid == c.localNodeID {
			isParty = true
			break
		}
	}
	
	if !isParty {
		c.log.Debug("Not a party in this keygen, ignoring")
		return nil
	}
	
	// 创建等待会话
	pending := &PendingSession{
		SessionID:     initMsg.SessionID,
		Type:          "keygen",
		Request:       &req,
		ReadyNodes:    make(map[string]bool),
		RequiredNodes: req.PartyIDs,
		CreatedAt:     time.Now(),
		ResultChan:    make(chan error, 1),
	}
	
	c.pendingMu.Lock()
	c.pendingSessions[initMsg.SessionID] = pending
	c.pendingMu.Unlock()
	
	// 加入keygen会话
	if c.keygenMgr != nil {
		go func() {
			internalReq := &KeygenRequest{
				WalletID:   req.WalletID,
				Threshold:  req.Threshold,
				TotalParts: req.TotalParts,
				PartyIDs:   req.PartyIDs,
			}
			_, err := c.keygenMgr.JoinKeygen(c.ctx, initMsg.SessionID, internalReq)
			if err != nil {
				c.log.WithError(err).Error("Failed to join keygen")
			}
		}()
	}
	
	// 发送ready消息
	c.sendReadyMessage(initMsg.SessionID, "keygen")
	
	return nil
}

// handleKeygenReady 处理keygen就绪消息
func (c *Coordinator) handleKeygenReady(msg *types.P2PMessage) error {
	var readyMsg ReadyMessage
	if err := json.Unmarshal(msg.Data, &readyMsg); err != nil {
		return fmt.Errorf("failed to unmarshal ready message: %w", err)
	}
	
	c.log.WithFields(logrus.Fields{
		"session_id": readyMsg.SessionID,
		"node_id":    readyMsg.NodeID,
	}).Debug("Received keygen ready")
	
	c.markNodeReady(readyMsg.SessionID, readyMsg.NodeID)
	
	return nil
}

// handleSignInit 处理sign初始化消息
func (c *Coordinator) handleSignInit(msg *types.P2PMessage) error {
	var initMsg InitMessage
	if err := json.Unmarshal(msg.Data, &initMsg); err != nil {
		return fmt.Errorf("failed to unmarshal init message: %w", err)
	}
	
	if initMsg.InitiatorID == c.localNodeID {
		return nil
	}
	
	c.log.WithFields(logrus.Fields{
		"session_id": initMsg.SessionID,
		"initiator":  initMsg.InitiatorID,
	}).Info("Received sign init")
	
	var req types.SignRequest
	if err := json.Unmarshal(initMsg.Request, &req); err != nil {
		return fmt.Errorf("failed to unmarshal sign request: %w", err)
	}
	
	// 检查自己是否在参与方列表中
	isParty := false
	for _, pid := range req.PartyIDs {
		if pid == c.localNodeID {
			isParty = true
			break
		}
	}
	
	if !isParty {
		return nil
	}
	
	// 加入签名会话
	if c.signingMgr != nil {
		go func() {
			_, err := c.signingMgr.JoinSigning(c.ctx, initMsg.SessionID, &req)
			if err != nil {
				c.log.WithError(err).Error("Failed to join signing")
			}
		}()
	}
	
	c.sendReadyMessage(initMsg.SessionID, "sign")
	
	return nil
}

// handleSignReady 处理sign就绪消息
func (c *Coordinator) handleSignReady(msg *types.P2PMessage) error {
	var readyMsg ReadyMessage
	if err := json.Unmarshal(msg.Data, &readyMsg); err != nil {
		return fmt.Errorf("failed to unmarshal ready message: %w", err)
	}
	
	c.markNodeReady(readyMsg.SessionID, readyMsg.NodeID)
	return nil
}

// InitiateSign 发起签名（协调所有节点）
func (c *Coordinator) InitiateSign(ctx context.Context, req *types.SignRequest) (string, error) {
	sessionID := req.RequestID
	if sessionID == "" {
		sessionID = uuid.New().String()
	}
	
	c.log.WithFields(logrus.Fields{
		"session_id": sessionID,
		"wallet_id":  req.WalletID,
		"parties":    req.PartyIDs,
	}).Info("Initiating coordinated signing")
	
	// 创建等待会话
	pending := &PendingSession{
		SessionID:     sessionID,
		Type:          "sign",
		Request:       req,
		ReadyNodes:    make(map[string]bool),
		RequiredNodes: req.PartyIDs,
		CreatedAt:     time.Now(),
		ResultChan:    make(chan error, 1),
	}
	
	c.pendingMu.Lock()
	c.pendingSessions[sessionID] = pending
	c.pendingMu.Unlock()
	
	// 序列化请求
	reqData, _ := json.Marshal(req)
	
	// 广播初始化消息
	initMsg := &InitMessage{
		SessionID:   sessionID,
		Type:        "sign",
		Request:     reqData,
		InitiatorID: c.localNodeID,
		Timestamp:   time.Now().UnixNano(),
	}
	
	initData, _ := json.Marshal(initMsg)
	p2pMsg := &types.P2PMessage{
		Type:      MsgTypeSignInit,
		SessionID: sessionID,
		From:      c.localNodeID,
		Data:      initData,
		Timestamp: time.Now().UnixNano(),
	}
	
	if err := c.p2pHost.BroadcastMessage(p2pMsg); err != nil {
		return "", fmt.Errorf("failed to broadcast init message: %w", err)
	}
	
	c.markNodeReady(sessionID, c.localNodeID)
	
	// 等待所有节点就绪
	timeout := time.After(15 * time.Second)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-timeout:
			return "", fmt.Errorf("timeout waiting for signing nodes to be ready")
		case <-ticker.C:
			if c.allNodesReady(sessionID) {
				c.log.WithField("session_id", sessionID).Info("All nodes ready, starting signing")
				return sessionID, nil
			}
		}
	}
}

// sendReadyMessage 发送就绪消息
func (c *Coordinator) sendReadyMessage(sessionID, msgType string) {
	readyMsg := &ReadyMessage{
		SessionID: sessionID,
		NodeID:    c.localNodeID,
		Type:      msgType,
	}
	
	data, _ := json.Marshal(readyMsg)
	
	var p2pMsgType string
	switch msgType {
	case "keygen":
		p2pMsgType = MsgTypeKeygenReady
	case "sign":
		p2pMsgType = MsgTypeSignReady
	default:
		p2pMsgType = MsgTypeKeygenReady
	}
	
	p2pMsg := &types.P2PMessage{
		Type:      p2pMsgType,
		SessionID: sessionID,
		From:      c.localNodeID,
		Data:      data,
		Timestamp: time.Now().UnixNano(),
	}
	
	if err := c.p2pHost.BroadcastMessage(p2pMsg); err != nil {
		c.log.WithError(err).Error("Failed to send ready message")
	}
}

// markNodeReady 标记节点就绪
func (c *Coordinator) markNodeReady(sessionID, nodeID string) {
	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()
	
	if pending, ok := c.pendingSessions[sessionID]; ok {
		pending.ReadyNodes[nodeID] = true
		c.log.WithFields(logrus.Fields{
			"session_id":  sessionID,
			"node_id":     nodeID,
			"ready_count": len(pending.ReadyNodes),
			"required":    len(pending.RequiredNodes),
		}).Debug("Node marked as ready")
	}
}

// allNodesReady 检查是否所有节点都就绪
func (c *Coordinator) allNodesReady(sessionID string) bool {
	c.pendingMu.RLock()
	defer c.pendingMu.RUnlock()
	
	pending, ok := c.pendingSessions[sessionID]
	if !ok {
		return false
	}
	
	for _, nodeID := range pending.RequiredNodes {
		if !pending.ReadyNodes[nodeID] {
			return false
		}
	}
	
	return true
}

// CleanupSession 清理会话
func (c *Coordinator) CleanupSession(sessionID string) {
	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()
	delete(c.pendingSessions, sessionID)
}
