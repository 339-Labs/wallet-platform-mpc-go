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
	MsgTypeKeygenInit   = "keygen_init"
	MsgTypeKeygenReady  = "keygen_ready"
	MsgTypeKeygenStart  = "keygen_start"
	MsgTypeSignInit     = "sign_init"
	MsgTypeSignReady    = "sign_ready"
	MsgTypeSignStart    = "sign_start"
	MsgTypeReshareInit  = "reshare_init"
	MsgTypeReshareReady = "reshare_ready"
	MsgTypeReshareStart = "reshare_start"
)

// CoordinatorMessage 协调消息
type CoordinatorMessage struct {
	Type       string          `json:"type"`
	SessionID  string          `json:"session_id"`
	WalletID   string          `json:"wallet_id"`
	FromNodeID string          `json:"from_node_id"`
	Threshold  int             `json:"threshold"`
	PartyIDs   []string        `json:"party_ids"`
	Data       json.RawMessage `json:"data,omitempty"`
	Timestamp  int64           `json:"timestamp"`
}

// PendingSession 待处理的会话
type PendingSession struct {
	SessionID   string
	WalletID    string
	Type        string // keygen, sign, reshare
	Threshold   int
	PartyIDs    []string
	ReadyNodes  map[string]bool
	InitiatorID string
	CreatedAt   time.Time
	StartChan   chan struct{}
	mu          sync.Mutex
}

// Coordinator TSS会话协调器
type Coordinator struct {
	p2pHost     *p2p.P2PHost
	localNodeID string
	
	// 待处理的会话
	pendingSessions map[string]*PendingSession
	sessionMu       sync.RWMutex
	
	// 回调
	onKeygenStart  func(sessionID string, req *types.KeygenRequest) error
	onSignStart    func(sessionID string, req *types.SignRequest) error
	onReshareStart func(sessionID string, req *ResharingRequest) error
	
	ctx    context.Context
	cancel context.CancelFunc
	log    *logrus.Entry
}

// NewCoordinator 创建协调器
func NewCoordinator(ctx context.Context, p2pHost *p2p.P2PHost, localNodeID string) *Coordinator {
	ctx, cancel := context.WithCancel(ctx)
	
	c := &Coordinator{
		p2pHost:         p2pHost,
		localNodeID:     localNodeID,
		pendingSessions: make(map[string]*PendingSession),
		ctx:             ctx,
		cancel:          cancel,
		log:             logrus.WithField("component", "coordinator"),
	}
	
	// 注册消息处理器
	p2pHost.RegisterHandler(MsgTypeKeygenInit, c.handleKeygenInit)
	p2pHost.RegisterHandler(MsgTypeKeygenReady, c.handleKeygenReady)
	p2pHost.RegisterHandler(MsgTypeKeygenStart, c.handleKeygenStart)
	p2pHost.RegisterHandler(MsgTypeSignInit, c.handleSignInit)
	p2pHost.RegisterHandler(MsgTypeSignReady, c.handleSignReady)
	p2pHost.RegisterHandler(MsgTypeSignStart, c.handleSignStart)
	
	return c
}

// SetKeygenCallback 设置keygen回调
func (c *Coordinator) SetKeygenCallback(fn func(sessionID string, req *types.KeygenRequest) error) {
	c.onKeygenStart = fn
}

// SetSignCallback 设置sign回调
func (c *Coordinator) SetSignCallback(fn func(sessionID string, req *types.SignRequest) error) {
	c.onSignStart = fn
}

// InitiateKeygen 发起keygen协调
func (c *Coordinator) InitiateKeygen(ctx context.Context, req *types.KeygenRequest) (string, error) {
	sessionID := uuid.New().String()
	
	c.log.WithFields(logrus.Fields{
		"session_id": sessionID,
		"wallet_id":  req.WalletID,
		"parties":    req.PartyIDs,
		"threshold":  req.Threshold,
	}).Info("Initiating keygen coordination")
	
	// 检查本节点是否在参与方列表中
	if !contains(req.PartyIDs, c.localNodeID) {
		return "", fmt.Errorf("local node %s is not in party list", c.localNodeID)
	}
	
	// 创建待处理会话
	pending := &PendingSession{
		SessionID:   sessionID,
		WalletID:    req.WalletID,
		Type:        "keygen",
		Threshold:   req.Threshold,
		PartyIDs:    req.PartyIDs,
		ReadyNodes:  make(map[string]bool),
		InitiatorID: c.localNodeID,
		CreatedAt:   time.Now(),
		StartChan:   make(chan struct{}),
	}
	
	// 本节点标记为ready
	pending.ReadyNodes[c.localNodeID] = true
	
	c.sessionMu.Lock()
	c.pendingSessions[sessionID] = pending
	c.sessionMu.Unlock()
	
	// 广播keygen_init消息
	initMsg := &CoordinatorMessage{
		Type:       MsgTypeKeygenInit,
		SessionID:  sessionID,
		WalletID:   req.WalletID,
		FromNodeID: c.localNodeID,
		Threshold:  req.Threshold,
		PartyIDs:   req.PartyIDs,
		Timestamp:  time.Now().UnixNano(),
	}
	
	if err := c.broadcastCoordinatorMessage(initMsg); err != nil {
		return "", fmt.Errorf("failed to broadcast keygen init: %w", err)
	}
	
	// 等待所有节点ready
	if err := c.waitForAllReady(ctx, pending); err != nil {
		c.cleanupSession(sessionID)
		return "", err
	}
	
	// 广播keygen_start消息
	startMsg := &CoordinatorMessage{
		Type:       MsgTypeKeygenStart,
		SessionID:  sessionID,
		WalletID:   req.WalletID,
		FromNodeID: c.localNodeID,
		Threshold:  req.Threshold,
		PartyIDs:   req.PartyIDs,
		Timestamp:  time.Now().UnixNano(),
	}
	
	if err := c.broadcastCoordinatorMessage(startMsg); err != nil {
		return "", fmt.Errorf("failed to broadcast keygen start: %w", err)
	}
	
	return sessionID, nil
}

// InitiateSign 发起sign协调
func (c *Coordinator) InitiateSign(ctx context.Context, req *types.SignRequest) (string, error) {
	sessionID := req.RequestID
	if sessionID == "" {
		sessionID = uuid.New().String()
	}
	
	c.log.WithFields(logrus.Fields{
		"session_id": sessionID,
		"wallet_id":  req.WalletID,
		"parties":    req.PartyIDs,
	}).Info("Initiating sign coordination")
	
	// 检查本节点是否在参与方列表中
	if !contains(req.PartyIDs, c.localNodeID) {
		return "", fmt.Errorf("local node %s is not in party list", c.localNodeID)
	}
	
	// 创建待处理会话
	pending := &PendingSession{
		SessionID:   sessionID,
		WalletID:    req.WalletID,
		Type:        "sign",
		PartyIDs:    req.PartyIDs,
		ReadyNodes:  make(map[string]bool),
		InitiatorID: c.localNodeID,
		CreatedAt:   time.Now(),
		StartChan:   make(chan struct{}),
	}
	pending.ReadyNodes[c.localNodeID] = true
	
	c.sessionMu.Lock()
	c.pendingSessions[sessionID] = pending
	c.sessionMu.Unlock()
	
	// 序列化请求数据
	reqData, _ := json.Marshal(req)
	
	// 广播sign_init消息
	initMsg := &CoordinatorMessage{
		Type:       MsgTypeSignInit,
		SessionID:  sessionID,
		WalletID:   req.WalletID,
		FromNodeID: c.localNodeID,
		PartyIDs:   req.PartyIDs,
		Data:       reqData,
		Timestamp:  time.Now().UnixNano(),
	}
	
	if err := c.broadcastCoordinatorMessage(initMsg); err != nil {
		return "", fmt.Errorf("failed to broadcast sign init: %w", err)
	}
	
	// 等待所有节点ready
	if err := c.waitForAllReady(ctx, pending); err != nil {
		c.cleanupSession(sessionID)
		return "", err
	}
	
	// 广播sign_start消息
	startMsg := &CoordinatorMessage{
		Type:       MsgTypeSignStart,
		SessionID:  sessionID,
		WalletID:   req.WalletID,
		FromNodeID: c.localNodeID,
		PartyIDs:   req.PartyIDs,
		Data:       reqData,
		Timestamp:  time.Now().UnixNano(),
	}
	
	if err := c.broadcastCoordinatorMessage(startMsg); err != nil {
		return "", fmt.Errorf("failed to broadcast sign start: %w", err)
	}
	
	return sessionID, nil
}

// handleKeygenInit 处理keygen初始化消息
func (c *Coordinator) handleKeygenInit(msg *types.P2PMessage) error {
	var coordMsg CoordinatorMessage
	if err := json.Unmarshal(msg.Data, &coordMsg); err != nil {
		return err
	}
	
	c.log.WithFields(logrus.Fields{
		"session_id": coordMsg.SessionID,
		"from":       coordMsg.FromNodeID,
		"parties":    coordMsg.PartyIDs,
	}).Info("Received keygen init")
	
	// 检查本节点是否在参与方列表中
	if !contains(coordMsg.PartyIDs, c.localNodeID) {
		c.log.Debug("Not in party list, ignoring")
		return nil
	}
	
	// 创建待处理会话
	pending := &PendingSession{
		SessionID:   coordMsg.SessionID,
		WalletID:    coordMsg.WalletID,
		Type:        "keygen",
		Threshold:   coordMsg.Threshold,
		PartyIDs:    coordMsg.PartyIDs,
		ReadyNodes:  make(map[string]bool),
		InitiatorID: coordMsg.FromNodeID,
		CreatedAt:   time.Now(),
		StartChan:   make(chan struct{}),
	}
	
	c.sessionMu.Lock()
	if _, exists := c.pendingSessions[coordMsg.SessionID]; exists {
		c.sessionMu.Unlock()
		return nil // 已存在
	}
	c.pendingSessions[coordMsg.SessionID] = pending
	c.sessionMu.Unlock()
	
	// 发送ready消息
	readyMsg := &CoordinatorMessage{
		Type:       MsgTypeKeygenReady,
		SessionID:  coordMsg.SessionID,
		WalletID:   coordMsg.WalletID,
		FromNodeID: c.localNodeID,
		Timestamp:  time.Now().UnixNano(),
	}
	
	return c.broadcastCoordinatorMessage(readyMsg)
}

// handleKeygenReady 处理keygen ready消息
func (c *Coordinator) handleKeygenReady(msg *types.P2PMessage) error {
	var coordMsg CoordinatorMessage
	if err := json.Unmarshal(msg.Data, &coordMsg); err != nil {
		return err
	}
	
	c.log.WithFields(logrus.Fields{
		"session_id": coordMsg.SessionID,
		"from":       coordMsg.FromNodeID,
	}).Info("Received keygen ready")
	
	c.sessionMu.RLock()
	pending, exists := c.pendingSessions[coordMsg.SessionID]
	c.sessionMu.RUnlock()
	
	if !exists {
		return nil
	}
	
	pending.mu.Lock()
	pending.ReadyNodes[coordMsg.FromNodeID] = true
	allReady := len(pending.ReadyNodes) >= len(pending.PartyIDs)
	pending.mu.Unlock()
	
	if allReady {
		c.log.WithField("session_id", coordMsg.SessionID).Info("All parties ready for keygen")
		select {
		case pending.StartChan <- struct{}{}:
		default:
		}
	}
	
	return nil
}

// handleKeygenStart 处理keygen开始消息
func (c *Coordinator) handleKeygenStart(msg *types.P2PMessage) error {
	var coordMsg CoordinatorMessage
	if err := json.Unmarshal(msg.Data, &coordMsg); err != nil {
		return err
	}
	
	c.log.WithFields(logrus.Fields{
		"session_id": coordMsg.SessionID,
		"from":       coordMsg.FromNodeID,
	}).Info("Received keygen start")
	
	// 检查本节点是否在参与方列表中
	if !contains(coordMsg.PartyIDs, c.localNodeID) {
		return nil
	}
	
	// 调用keygen回调
	if c.onKeygenStart != nil {
		req := &types.KeygenRequest{
			WalletID:   coordMsg.WalletID,
			Threshold:  coordMsg.Threshold,
			TotalParts: len(coordMsg.PartyIDs),
			PartyIDs:   coordMsg.PartyIDs,
		}
		go func() {
			if err := c.onKeygenStart(coordMsg.SessionID, req); err != nil {
				c.log.WithError(err).Error("Keygen callback failed")
			}
		}()
	}
	
	return nil
}

// handleSignInit 处理sign初始化消息
func (c *Coordinator) handleSignInit(msg *types.P2PMessage) error {
	var coordMsg CoordinatorMessage
	if err := json.Unmarshal(msg.Data, &coordMsg); err != nil {
		return err
	}
	
	c.log.WithFields(logrus.Fields{
		"session_id": coordMsg.SessionID,
		"from":       coordMsg.FromNodeID,
	}).Info("Received sign init")
	
	if !contains(coordMsg.PartyIDs, c.localNodeID) {
		return nil
	}
	
	// 创建待处理会话
	pending := &PendingSession{
		SessionID:   coordMsg.SessionID,
		WalletID:    coordMsg.WalletID,
		Type:        "sign",
		PartyIDs:    coordMsg.PartyIDs,
		ReadyNodes:  make(map[string]bool),
		InitiatorID: coordMsg.FromNodeID,
		CreatedAt:   time.Now(),
		StartChan:   make(chan struct{}),
	}
	
	c.sessionMu.Lock()
	if _, exists := c.pendingSessions[coordMsg.SessionID]; exists {
		c.sessionMu.Unlock()
		return nil
	}
	c.pendingSessions[coordMsg.SessionID] = pending
	c.sessionMu.Unlock()
	
	// 发送ready消息
	readyMsg := &CoordinatorMessage{
		Type:       MsgTypeSignReady,
		SessionID:  coordMsg.SessionID,
		WalletID:   coordMsg.WalletID,
		FromNodeID: c.localNodeID,
		Timestamp:  time.Now().UnixNano(),
	}
	
	return c.broadcastCoordinatorMessage(readyMsg)
}

// handleSignReady 处理sign ready消息
func (c *Coordinator) handleSignReady(msg *types.P2PMessage) error {
	var coordMsg CoordinatorMessage
	if err := json.Unmarshal(msg.Data, &coordMsg); err != nil {
		return err
	}
	
	c.sessionMu.RLock()
	pending, exists := c.pendingSessions[coordMsg.SessionID]
	c.sessionMu.RUnlock()
	
	if !exists {
		return nil
	}
	
	pending.mu.Lock()
	pending.ReadyNodes[coordMsg.FromNodeID] = true
	allReady := len(pending.ReadyNodes) >= len(pending.PartyIDs)
	pending.mu.Unlock()
	
	if allReady {
		select {
		case pending.StartChan <- struct{}{}:
		default:
		}
	}
	
	return nil
}

// handleSignStart 处理sign开始消息
func (c *Coordinator) handleSignStart(msg *types.P2PMessage) error {
	var coordMsg CoordinatorMessage
	if err := json.Unmarshal(msg.Data, &coordMsg); err != nil {
		return err
	}
	
	if !contains(coordMsg.PartyIDs, c.localNodeID) {
		return nil
	}
	
	// 调用sign回调
	if c.onSignStart != nil {
		var req types.SignRequest
		if err := json.Unmarshal(coordMsg.Data, &req); err != nil {
			return err
		}
		go func() {
			if err := c.onSignStart(coordMsg.SessionID, &req); err != nil {
				c.log.WithError(err).Error("Sign callback failed")
			}
		}()
	}
	
	return nil
}

// waitForAllReady 等待所有节点ready
func (c *Coordinator) waitForAllReady(ctx context.Context, pending *PendingSession) error {
	timeout := time.After(30 * time.Second)
	
	for {
		pending.mu.Lock()
		allReady := len(pending.ReadyNodes) >= len(pending.PartyIDs)
		pending.mu.Unlock()
		
		if allReady {
			c.log.WithField("session_id", pending.SessionID).Info("All parties ready")
			return nil
		}
		
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timeout:
			pending.mu.Lock()
			ready := len(pending.ReadyNodes)
			total := len(pending.PartyIDs)
			pending.mu.Unlock()
			return fmt.Errorf("timeout waiting for parties: %d/%d ready", ready, total)
		case <-pending.StartChan:
			return nil
		case <-time.After(100 * time.Millisecond):
			// 继续检查
		}
	}
}

// broadcastCoordinatorMessage 广播协调消息
func (c *Coordinator) broadcastCoordinatorMessage(coordMsg *CoordinatorMessage) error {
	data, err := json.Marshal(coordMsg)
	if err != nil {
		return err
	}
	
	msg := &types.P2PMessage{
		Type:      coordMsg.Type,
		SessionID: coordMsg.SessionID,
		From:      c.localNodeID,
		Data:      data,
		Timestamp: time.Now().UnixNano(),
	}
	
	c.log.WithFields(logrus.Fields{
		"type":       coordMsg.Type,
		"session_id": coordMsg.SessionID,
	}).Debug("Broadcasting coordinator message")
	
	return c.p2pHost.BroadcastMessage(msg)
}

// cleanupSession 清理会话
func (c *Coordinator) cleanupSession(sessionID string) {
	c.sessionMu.Lock()
	delete(c.pendingSessions, sessionID)
	c.sessionMu.Unlock()
}

// Stop 停止协调器
func (c *Coordinator) Stop() {
	c.cancel()
}
