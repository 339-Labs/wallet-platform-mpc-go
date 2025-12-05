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
	MsgTypeKeygenInit     = "keygen_init"
	MsgTypeKeygenJoin     = "keygen_join"
	MsgTypeKeygenReady    = "keygen_ready"
	MsgTypeSignInit       = "sign_init"
	MsgTypeSignJoin       = "sign_join"
	MsgTypeSignReady      = "sign_ready"
	MsgTypeResharingInit  = "resharing_init"
	MsgTypeResharingJoin  = "resharing_join"
)

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
	SessionID string   `json:"session_id"`
	WalletID  string   `json:"wallet_id"`
	Message   string   `json:"message"`
	PartyIDs  []string `json:"party_ids"`
	Initiator string   `json:"initiator"`
}

// JoinMessage 加入会话消息
type JoinMessage struct {
	SessionID string `json:"session_id"`
	NodeID    string `json:"node_id"`
}

// Coordinator TSS会话协调器
type Coordinator struct {
	p2pHost      *p2p.P2PHost
	msgManager   *p2p.MessageManager
	keygenMgr    *KeygenManager
	signingMgr   *SigningManager
	resharingMgr *ResharingManager
	localNodeID  string
	
	// 等待其他节点加入的会话
	pendingSessions map[string]*PendingSession
	sessionMu       sync.RWMutex
	
	ctx    context.Context
	cancel context.CancelFunc
	log    *logrus.Entry
}

// PendingSession 待处理的会话
type PendingSession struct {
	SessionID    string
	Type         string // keygen, sign, resharing
	RequiredNodes []string
	JoinedNodes  map[string]bool
	ReadyChan    chan bool
	Request      interface{}
	CreatedAt    time.Time
}

// NewCoordinator 创建协调器
func NewCoordinator(
	p2pHost *p2p.P2PHost,
	msgManager *p2p.MessageManager,
	keygenMgr *KeygenManager,
	signingMgr *SigningManager,
	resharingMgr *ResharingManager,
	localNodeID string,
) *Coordinator {
	ctx, cancel := context.WithCancel(context.Background())
	
	c := &Coordinator{
		p2pHost:         p2pHost,
		msgManager:      msgManager,
		keygenMgr:       keygenMgr,
		signingMgr:      signingMgr,
		resharingMgr:    resharingMgr,
		localNodeID:     localNodeID,
		pendingSessions: make(map[string]*PendingSession),
		ctx:             ctx,
		cancel:          cancel,
		log:             logrus.WithField("component", "coordinator"),
	}
	
	return c
}

// Start 启动协调器
func (c *Coordinator) Start() error {
	// 注册消息处理器
	c.p2pHost.RegisterHandler(MsgTypeKeygenInit, c.handleKeygenInit)
	c.p2pHost.RegisterHandler(MsgTypeKeygenJoin, c.handleKeygenJoin)
	c.p2pHost.RegisterHandler(MsgTypeKeygenReady, c.handleKeygenReady)
	c.p2pHost.RegisterHandler(MsgTypeSignInit, c.handleSignInit)
	c.p2pHost.RegisterHandler(MsgTypeSignJoin, c.handleSignJoin)
	c.p2pHost.RegisterHandler(MsgTypeSignReady, c.handleSignReady)
	
	// 启动清理协程
	go c.cleanupLoop()
	
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
func (c *Coordinator) InitiateKeygen(ctx context.Context, req *types.KeygenRequest) (*KeygenSession, error) {
	sessionID := req.WalletID + "-" + fmt.Sprintf("%d", time.Now().UnixNano())
	
	c.log.WithFields(logrus.Fields{
		"session_id": sessionID,
		"wallet_id":  req.WalletID,
		"parties":    req.PartyIDs,
	}).Info("Initiating keygen coordination")
	
	// 创建待处理会话
	pending := &PendingSession{
		SessionID:     sessionID,
		Type:          "keygen",
		RequiredNodes: req.PartyIDs,
		JoinedNodes:   make(map[string]bool),
		ReadyChan:     make(chan bool, 1),
		Request:       req,
		CreatedAt:     time.Now(),
	}
	pending.JoinedNodes[c.localNodeID] = true // 自己已加入
	
	c.sessionMu.Lock()
	c.pendingSessions[sessionID] = pending
	c.sessionMu.Unlock()
	
	// 广播keygen初始化消息
	initMsg := &KeygenInitMessage{
		SessionID:  sessionID,
		WalletID:   req.WalletID,
		Threshold:  req.Threshold,
		TotalParts: req.TotalParts,
		PartyIDs:   req.PartyIDs,
		Initiator:  c.localNodeID,
	}
	
	data, _ := json.Marshal(initMsg)
	p2pMsg := &types.P2PMessage{
		Type:      MsgTypeKeygenInit,
		SessionID: sessionID,
		From:      c.localNodeID,
		Data:      data,
		Timestamp: time.Now().UnixNano(),
	}
	
	if err := c.p2pHost.BroadcastMessage(p2pMsg); err != nil {
		c.log.WithError(err).Error("Failed to broadcast keygen init")
		return nil, err
	}
	
	// 等待所有节点加入（超时30秒）
	c.log.Info("Waiting for all nodes to join keygen session...")
	
	select {
	case <-pending.ReadyChan:
		c.log.Info("All nodes joined, starting keygen")
	case <-time.After(30 * time.Second):
		c.sessionMu.Lock()
		delete(c.pendingSessions, sessionID)
		c.sessionMu.Unlock()
		return nil, fmt.Errorf("timeout waiting for nodes to join keygen")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	
	// 所有节点就绪，开始keygen
	internalReq := &types.KeygenRequest{
		WalletID:   req.WalletID,
		Threshold:  req.Threshold,
		TotalParts: req.TotalParts,
		PartyIDs:   req.PartyIDs,
	}
	
	// 广播开始消息
	readyMsg := &types.P2PMessage{
		Type:      MsgTypeKeygenReady,
		SessionID: sessionID,
		From:      c.localNodeID,
		Data:      data,
		Timestamp: time.Now().UnixNano(),
	}
	c.p2pHost.BroadcastMessage(readyMsg)
	
	// 启动本地keygen
	return c.keygenMgr.StartKeygenWithSession(ctx, sessionID, internalReq)
}

// handleKeygenInit 处理keygen初始化消息
func (c *Coordinator) handleKeygenInit(msg *types.P2PMessage) error {
	var initMsg KeygenInitMessage
	if err := json.Unmarshal(msg.Data, &initMsg); err != nil {
		return err
	}
	
	c.log.WithFields(logrus.Fields{
		"session_id": initMsg.SessionID,
		"initiator":  initMsg.Initiator,
		"parties":    initMsg.PartyIDs,
	}).Info("Received keygen init")
	
	// 检查自己是否在参与方列表中
	isParticipant := false
	for _, pid := range initMsg.PartyIDs {
		if pid == c.localNodeID {
			isParticipant = true
			break
		}
	}
	
	if !isParticipant {
		c.log.Debug("Not a participant in this keygen session")
		return nil
	}
	
	// 创建待处理会话
	pending := &PendingSession{
		SessionID:     initMsg.SessionID,
		Type:          "keygen",
		RequiredNodes: initMsg.PartyIDs,
		JoinedNodes:   make(map[string]bool),
		ReadyChan:     make(chan bool, 1),
		Request: &types.KeygenRequest{
			WalletID:   initMsg.WalletID,
			Threshold:  initMsg.Threshold,
			TotalParts: initMsg.TotalParts,
			PartyIDs:   initMsg.PartyIDs,
		},
		CreatedAt: time.Now(),
	}
	pending.JoinedNodes[initMsg.Initiator] = true
	pending.JoinedNodes[c.localNodeID] = true
	
	c.sessionMu.Lock()
	c.pendingSessions[initMsg.SessionID] = pending
	c.sessionMu.Unlock()
	
	// 发送加入消息
	joinMsg := &JoinMessage{
		SessionID: initMsg.SessionID,
		NodeID:    c.localNodeID,
	}
	data, _ := json.Marshal(joinMsg)
	
	p2pMsg := &types.P2PMessage{
		Type:      MsgTypeKeygenJoin,
		SessionID: initMsg.SessionID,
		From:      c.localNodeID,
		Data:      data,
		Timestamp: time.Now().UnixNano(),
	}
	
	return c.p2pHost.BroadcastMessage(p2pMsg)
}

// handleKeygenJoin 处理keygen加入消息
func (c *Coordinator) handleKeygenJoin(msg *types.P2PMessage) error {
	var joinMsg JoinMessage
	if err := json.Unmarshal(msg.Data, &joinMsg); err != nil {
		return err
	}
	
	c.log.WithFields(logrus.Fields{
		"session_id": joinMsg.SessionID,
		"node_id":    joinMsg.NodeID,
	}).Info("Received keygen join")
	
	c.sessionMu.Lock()
	pending, exists := c.pendingSessions[joinMsg.SessionID]
	if !exists {
		c.sessionMu.Unlock()
		return nil
	}
	
	pending.JoinedNodes[joinMsg.NodeID] = true
	
	// 检查是否所有节点都已加入
	allJoined := true
	for _, nodeID := range pending.RequiredNodes {
		if !pending.JoinedNodes[nodeID] {
			allJoined = false
			break
		}
	}
	c.sessionMu.Unlock()
	
	if allJoined {
		c.log.WithField("session_id", joinMsg.SessionID).Info("All nodes joined keygen session")
		select {
		case pending.ReadyChan <- true:
		default:
		}
	}
	
	return nil
}

// handleKeygenReady 处理keygen就绪消息
func (c *Coordinator) handleKeygenReady(msg *types.P2PMessage) error {
	var initMsg KeygenInitMessage
	if err := json.Unmarshal(msg.Data, &initMsg); err != nil {
		return err
	}
	
	c.log.WithField("session_id", initMsg.SessionID).Info("Received keygen ready, starting local keygen")
	
	c.sessionMu.RLock()
	pending, exists := c.pendingSessions[initMsg.SessionID]
	c.sessionMu.RUnlock()
	
	if !exists {
		return nil
	}
	
	// 启动本地keygen
	req := pending.Request.(*types.KeygenRequest)
	go func() {
		_, err := c.keygenMgr.StartKeygenWithSession(context.Background(), initMsg.SessionID, req)
		if err != nil {
			c.log.WithError(err).Error("Failed to start keygen")
		}
	}()
	
	return nil
}

// InitiateSign 发起签名（协调所有节点）
func (c *Coordinator) InitiateSign(ctx context.Context, req *types.SignRequest) (*SigningSession, error) {
	sessionID := req.RequestID
	if sessionID == "" {
		sessionID = req.WalletID + "-sign-" + fmt.Sprintf("%d", time.Now().UnixNano())
	}
	
	c.log.WithFields(logrus.Fields{
		"session_id": sessionID,
		"wallet_id":  req.WalletID,
		"parties":    req.PartyIDs,
	}).Info("Initiating sign coordination")
	
	// 创建待处理会话
	pending := &PendingSession{
		SessionID:     sessionID,
		Type:          "sign",
		RequiredNodes: req.PartyIDs,
		JoinedNodes:   make(map[string]bool),
		ReadyChan:     make(chan bool, 1),
		Request:       req,
		CreatedAt:     time.Now(),
	}
	pending.JoinedNodes[c.localNodeID] = true
	
	c.sessionMu.Lock()
	c.pendingSessions[sessionID] = pending
	c.sessionMu.Unlock()
	
	// 广播签名初始化消息
	initMsg := &SignInitMessage{
		SessionID: sessionID,
		WalletID:  req.WalletID,
		Message:   req.Message,
		PartyIDs:  req.PartyIDs,
		Initiator: c.localNodeID,
	}
	
	data, _ := json.Marshal(initMsg)
	p2pMsg := &types.P2PMessage{
		Type:      MsgTypeSignInit,
		SessionID: sessionID,
		From:      c.localNodeID,
		Data:      data,
		Timestamp: time.Now().UnixNano(),
	}
	
	if err := c.p2pHost.BroadcastMessage(p2pMsg); err != nil {
		return nil, err
	}
	
	// 等待所有节点加入
	select {
	case <-pending.ReadyChan:
		c.log.Info("All nodes joined, starting signing")
	case <-time.After(30 * time.Second):
		c.sessionMu.Lock()
		delete(c.pendingSessions, sessionID)
		c.sessionMu.Unlock()
		return nil, fmt.Errorf("timeout waiting for nodes to join signing")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	
	// 广播开始消息
	readyMsg := &types.P2PMessage{
		Type:      MsgTypeSignReady,
		SessionID: sessionID,
		From:      c.localNodeID,
		Data:      data,
		Timestamp: time.Now().UnixNano(),
	}
	c.p2pHost.BroadcastMessage(readyMsg)
	
	// 启动本地签名
	req.RequestID = sessionID
	return c.signingMgr.StartSigningWithSession(ctx, sessionID, req)
}

// handleSignInit 处理签名初始化消息
func (c *Coordinator) handleSignInit(msg *types.P2PMessage) error {
	var initMsg SignInitMessage
	if err := json.Unmarshal(msg.Data, &initMsg); err != nil {
		return err
	}
	
	c.log.WithFields(logrus.Fields{
		"session_id": initMsg.SessionID,
		"initiator":  initMsg.Initiator,
	}).Info("Received sign init")
	
	// 检查自己是否在参与方列表中
	isParticipant := false
	for _, pid := range initMsg.PartyIDs {
		if pid == c.localNodeID {
			isParticipant = true
			break
		}
	}
	
	if !isParticipant {
		return nil
	}
	
	// 创建待处理会话
	pending := &PendingSession{
		SessionID:     initMsg.SessionID,
		Type:          "sign",
		RequiredNodes: initMsg.PartyIDs,
		JoinedNodes:   make(map[string]bool),
		ReadyChan:     make(chan bool, 1),
		Request: &types.SignRequest{
			WalletID:  initMsg.WalletID,
			Message:   initMsg.Message,
			PartyIDs:  initMsg.PartyIDs,
			RequestID: initMsg.SessionID,
		},
		CreatedAt: time.Now(),
	}
	pending.JoinedNodes[initMsg.Initiator] = true
	pending.JoinedNodes[c.localNodeID] = true
	
	c.sessionMu.Lock()
	c.pendingSessions[initMsg.SessionID] = pending
	c.sessionMu.Unlock()
	
	// 发送加入消息
	joinMsg := &JoinMessage{
		SessionID: initMsg.SessionID,
		NodeID:    c.localNodeID,
	}
	data, _ := json.Marshal(joinMsg)
	
	p2pMsg := &types.P2PMessage{
		Type:      MsgTypeSignJoin,
		SessionID: initMsg.SessionID,
		From:      c.localNodeID,
		Data:      data,
		Timestamp: time.Now().UnixNano(),
	}
	
	return c.p2pHost.BroadcastMessage(p2pMsg)
}

// handleSignJoin 处理签名加入消息
func (c *Coordinator) handleSignJoin(msg *types.P2PMessage) error {
	var joinMsg JoinMessage
	if err := json.Unmarshal(msg.Data, &joinMsg); err != nil {
		return err
	}
	
	c.log.WithFields(logrus.Fields{
		"session_id": joinMsg.SessionID,
		"node_id":    joinMsg.NodeID,
	}).Info("Received sign join")
	
	c.sessionMu.Lock()
	pending, exists := c.pendingSessions[joinMsg.SessionID]
	if !exists {
		c.sessionMu.Unlock()
		return nil
	}
	
	pending.JoinedNodes[joinMsg.NodeID] = true
	
	allJoined := true
	for _, nodeID := range pending.RequiredNodes {
		if !pending.JoinedNodes[nodeID] {
			allJoined = false
			break
		}
	}
	c.sessionMu.Unlock()
	
	if allJoined {
		select {
		case pending.ReadyChan <- true:
		default:
		}
	}
	
	return nil
}

// handleSignReady 处理签名就绪消息
func (c *Coordinator) handleSignReady(msg *types.P2PMessage) error {
	var initMsg SignInitMessage
	if err := json.Unmarshal(msg.Data, &initMsg); err != nil {
		return err
	}
	
	c.log.WithField("session_id", initMsg.SessionID).Info("Received sign ready, starting local signing")
	
	c.sessionMu.RLock()
	pending, exists := c.pendingSessions[initMsg.SessionID]
	c.sessionMu.RUnlock()
	
	if !exists {
		return nil
	}
	
	req := pending.Request.(*types.SignRequest)
	go func() {
		_, err := c.signingMgr.StartSigningWithSession(context.Background(), initMsg.SessionID, req)
		if err != nil {
			c.log.WithError(err).Error("Failed to start signing")
		}
	}()
	
	return nil
}

// cleanupLoop 清理过期会话
func (c *Coordinator) cleanupLoop() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-ticker.C:
			c.sessionMu.Lock()
			for id, pending := range c.pendingSessions {
				if time.Since(pending.CreatedAt) > 5*time.Minute {
					delete(c.pendingSessions, id)
				}
			}
			c.sessionMu.Unlock()
		}
	}
}
