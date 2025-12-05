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
	MsgTypeKeygenRequest   = "keygen_request"
	MsgTypeKeygenReady     = "keygen_ready"
	MsgTypeSignRequest     = "sign_request"
	MsgTypeSignReady       = "sign_ready"
	MsgTypeReshareRequest  = "reshare_request"
	MsgTypeReshareReady    = "reshare_ready"
)

// Coordinator TSS协调器 - 负责多节点会话同步
type Coordinator struct {
	p2pHost     *p2p.P2PHost
	msgManager  *p2p.MessageManager
	localNodeID string
	
	// 待处理的请求
	pendingRequests map[string]*CoordinationRequest
	pendingMu       sync.RWMutex
	
	// 就绪状态追踪
	readyNodes map[string]map[string]bool // sessionID -> nodeID -> ready
	readyMu    sync.RWMutex
	
	// 回调
	onKeygenRequest  func(ctx context.Context, sessionID string, req *types.KeygenRequest) error
	onSignRequest    func(ctx context.Context, sessionID string, req *types.SignRequest) error
	onReshareRequest func(ctx context.Context, sessionID string, req *types.ResharingRequest) error
	
	ctx    context.Context
	cancel context.CancelFunc
	log    *logrus.Entry
}

// CoordinationRequest 协调请求
type CoordinationRequest struct {
	SessionID   string          `json:"session_id"`
	Type        string          `json:"type"`
	InitiatorID string          `json:"initiator_id"`
	PartyIDs    []string        `json:"party_ids"`
	Data        json.RawMessage `json:"data"`
	Timestamp   int64           `json:"timestamp"`
}

// CoordinationReady 就绪消息
type CoordinationReady struct {
	SessionID string `json:"session_id"`
	NodeID    string `json:"node_id"`
	Ready     bool   `json:"ready"`
}

// NewCoordinator 创建协调器
func NewCoordinator(ctx context.Context, p2pHost *p2p.P2PHost, msgManager *p2p.MessageManager, localNodeID string) *Coordinator {
	ctx, cancel := context.WithCancel(ctx)
	
	c := &Coordinator{
		p2pHost:         p2pHost,
		msgManager:      msgManager,
		localNodeID:     localNodeID,
		pendingRequests: make(map[string]*CoordinationRequest),
		readyNodes:      make(map[string]map[string]bool),
		ctx:             ctx,
		cancel:          cancel,
		log:             logrus.WithField("component", "coordinator"),
	}
	
	// 注册消息处理器
	p2pHost.RegisterHandler(MsgTypeKeygenRequest, c.handleKeygenRequest)
	p2pHost.RegisterHandler(MsgTypeKeygenReady, c.handleReadyMessage)
	p2pHost.RegisterHandler(MsgTypeSignRequest, c.handleSignRequest)
	p2pHost.RegisterHandler(MsgTypeSignReady, c.handleReadyMessage)
	p2pHost.RegisterHandler(MsgTypeReshareRequest, c.handleReshareRequest)
	p2pHost.RegisterHandler(MsgTypeReshareReady, c.handleReadyMessage)
	
	return c
}

// SetKeygenHandler 设置keygen请求处理器
func (c *Coordinator) SetKeygenHandler(handler func(ctx context.Context, sessionID string, req *types.KeygenRequest) error) {
	c.onKeygenRequest = handler
}

// SetSignHandler 设置sign请求处理器
func (c *Coordinator) SetSignHandler(handler func(ctx context.Context, sessionID string, req *types.SignRequest) error) {
	c.onSignRequest = handler
}

// SetReshareHandler 设置reshare请求处理器
func (c *Coordinator) SetReshareHandler(handler func(ctx context.Context, sessionID string, req *types.ResharingRequest) error) {
	c.onReshareRequest = handler
}

// InitiateKeygen 发起keygen协调
func (c *Coordinator) InitiateKeygen(ctx context.Context, req *types.KeygenRequest) (string, error) {
	sessionID := uuid.New().String()
	
	c.log.WithFields(logrus.Fields{
		"session_id": sessionID,
		"wallet_id":  req.WalletID,
		"parties":    req.PartyIDs,
	}).Info("Initiating keygen coordination")
	
	// 序列化请求
	data, err := json.Marshal(req)
	if err != nil {
		return "", fmt.Errorf("failed to marshal request: %w", err)
	}
	
	// 创建协调请求
	coordReq := &CoordinationRequest{
		SessionID:   sessionID,
		Type:        MsgTypeKeygenRequest,
		InitiatorID: c.localNodeID,
		PartyIDs:    req.PartyIDs,
		Data:        data,
		Timestamp:   time.Now().UnixNano(),
	}
	
	// 初始化就绪追踪
	c.readyMu.Lock()
	c.readyNodes[sessionID] = make(map[string]bool)
	for _, pid := range req.PartyIDs {
		c.readyNodes[sessionID][pid] = false
	}
	c.readyMu.Unlock()
	
	// 广播请求给所有参与方
	if err := c.broadcastRequest(coordReq); err != nil {
		return "", fmt.Errorf("failed to broadcast request: %w", err)
	}
	
	// 本地标记为就绪
	c.markReady(sessionID, c.localNodeID)
	
	// 广播本地就绪状态
	c.broadcastReady(sessionID, MsgTypeKeygenReady)
	
	// 等待所有节点就绪
	if err := c.waitAllReady(ctx, sessionID, req.PartyIDs, 30*time.Second); err != nil {
		return "", fmt.Errorf("failed to wait for all nodes: %w", err)
	}
	
	c.log.WithField("session_id", sessionID).Info("All nodes ready for keygen")
	
	return sessionID, nil
}

// InitiateSign 发起签名协调
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
	
	data, err := json.Marshal(req)
	if err != nil {
		return "", fmt.Errorf("failed to marshal request: %w", err)
	}
	
	coordReq := &CoordinationRequest{
		SessionID:   sessionID,
		Type:        MsgTypeSignRequest,
		InitiatorID: c.localNodeID,
		PartyIDs:    req.PartyIDs,
		Data:        data,
		Timestamp:   time.Now().UnixNano(),
	}
	
	c.readyMu.Lock()
	c.readyNodes[sessionID] = make(map[string]bool)
	for _, pid := range req.PartyIDs {
		c.readyNodes[sessionID][pid] = false
	}
	c.readyMu.Unlock()
	
	if err := c.broadcastRequest(coordReq); err != nil {
		return "", fmt.Errorf("failed to broadcast request: %w", err)
	}
	
	c.markReady(sessionID, c.localNodeID)
	c.broadcastReady(sessionID, MsgTypeSignReady)
	
	if err := c.waitAllReady(ctx, sessionID, req.PartyIDs, 15*time.Second); err != nil {
		return "", fmt.Errorf("failed to wait for all nodes: %w", err)
	}
	
	c.log.WithField("session_id", sessionID).Info("All nodes ready for signing")
	
	return sessionID, nil
}

// broadcastRequest 广播协调请求
func (c *Coordinator) broadcastRequest(req *CoordinationRequest) error {
	data, err := json.Marshal(req)
	if err != nil {
		return err
	}
	
	msg := &types.P2PMessage{
		Type:      req.Type,
		SessionID: req.SessionID,
		From:      c.localNodeID,
		Data:      data,
		Timestamp: time.Now().UnixNano(),
	}
	
	return c.p2pHost.BroadcastMessage(msg)
}

// broadcastReady 广播就绪状态
func (c *Coordinator) broadcastReady(sessionID, msgType string) error {
	ready := &CoordinationReady{
		SessionID: sessionID,
		NodeID:    c.localNodeID,
		Ready:     true,
	}
	
	data, err := json.Marshal(ready)
	if err != nil {
		return err
	}
	
	msg := &types.P2PMessage{
		Type:      msgType,
		SessionID: sessionID,
		From:      c.localNodeID,
		Data:      data,
		Timestamp: time.Now().UnixNano(),
	}
	
	return c.p2pHost.BroadcastMessage(msg)
}

// markReady 标记节点就绪
func (c *Coordinator) markReady(sessionID, nodeID string) {
	c.readyMu.Lock()
	defer c.readyMu.Unlock()
	
	if _, ok := c.readyNodes[sessionID]; ok {
		c.readyNodes[sessionID][nodeID] = true
		c.log.WithFields(logrus.Fields{
			"session_id": sessionID,
			"node_id":    nodeID,
		}).Debug("Node marked as ready")
	}
}

// waitAllReady 等待所有节点就绪
func (c *Coordinator) waitAllReady(ctx context.Context, sessionID string, partyIDs []string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if time.Now().After(deadline) {
				// 返回未就绪的节点列表
				c.readyMu.RLock()
				var notReady []string
				if nodes, ok := c.readyNodes[sessionID]; ok {
					for nodeID, ready := range nodes {
						if !ready {
							notReady = append(notReady, nodeID)
						}
					}
				}
				c.readyMu.RUnlock()
				return fmt.Errorf("timeout waiting for nodes: %v", notReady)
			}
			
			if c.allReady(sessionID, partyIDs) {
				return nil
			}
		}
	}
}

// allReady 检查是否所有节点都就绪
func (c *Coordinator) allReady(sessionID string, partyIDs []string) bool {
	c.readyMu.RLock()
	defer c.readyMu.RUnlock()
	
	nodes, ok := c.readyNodes[sessionID]
	if !ok {
		return false
	}
	
	for _, pid := range partyIDs {
		if !nodes[pid] {
			return false
		}
	}
	return true
}

// handleKeygenRequest 处理keygen请求
func (c *Coordinator) handleKeygenRequest(msg *types.P2PMessage) error {
	var coordReq CoordinationRequest
	if err := json.Unmarshal(msg.Data, &coordReq); err != nil {
		return fmt.Errorf("failed to unmarshal coordination request: %w", err)
	}
	
	c.log.WithFields(logrus.Fields{
		"session_id": coordReq.SessionID,
		"initiator":  coordReq.InitiatorID,
		"from":       msg.From,
	}).Info("Received keygen request")
	
	// 检查是否是参与方
	isParty := false
	for _, pid := range coordReq.PartyIDs {
		if pid == c.localNodeID {
			isParty = true
			break
		}
	}
	
	if !isParty {
		c.log.Debug("Not a party member, ignoring keygen request")
		return nil
	}
	
	// 初始化就绪追踪
	c.readyMu.Lock()
	if _, ok := c.readyNodes[coordReq.SessionID]; !ok {
		c.readyNodes[coordReq.SessionID] = make(map[string]bool)
		for _, pid := range coordReq.PartyIDs {
			c.readyNodes[coordReq.SessionID][pid] = false
		}
	}
	c.readyMu.Unlock()
	
	// 解析keygen请求
	var keygenReq types.KeygenRequest
	if err := json.Unmarshal(coordReq.Data, &keygenReq); err != nil {
		return fmt.Errorf("failed to unmarshal keygen request: %w", err)
	}
	
	// 调用处理器
	if c.onKeygenRequest != nil {
		go func() {
			if err := c.onKeygenRequest(c.ctx, coordReq.SessionID, &keygenReq); err != nil {
				c.log.WithError(err).Error("Failed to handle keygen request")
			}
		}()
	}
	
	// 标记本地就绪
	c.markReady(coordReq.SessionID, c.localNodeID)
	
	// 广播就绪状态
	return c.broadcastReady(coordReq.SessionID, MsgTypeKeygenReady)
}

// handleSignRequest 处理签名请求
func (c *Coordinator) handleSignRequest(msg *types.P2PMessage) error {
	var coordReq CoordinationRequest
	if err := json.Unmarshal(msg.Data, &coordReq); err != nil {
		return fmt.Errorf("failed to unmarshal coordination request: %w", err)
	}
	
	c.log.WithFields(logrus.Fields{
		"session_id": coordReq.SessionID,
		"initiator":  coordReq.InitiatorID,
	}).Info("Received sign request")
	
	isParty := false
	for _, pid := range coordReq.PartyIDs {
		if pid == c.localNodeID {
			isParty = true
			break
		}
	}
	
	if !isParty {
		return nil
	}
	
	c.readyMu.Lock()
	if _, ok := c.readyNodes[coordReq.SessionID]; !ok {
		c.readyNodes[coordReq.SessionID] = make(map[string]bool)
		for _, pid := range coordReq.PartyIDs {
			c.readyNodes[coordReq.SessionID][pid] = false
		}
	}
	c.readyMu.Unlock()
	
	var signReq types.SignRequest
	if err := json.Unmarshal(coordReq.Data, &signReq); err != nil {
		return fmt.Errorf("failed to unmarshal sign request: %w", err)
	}
	
	if c.onSignRequest != nil {
		go func() {
			if err := c.onSignRequest(c.ctx, coordReq.SessionID, &signReq); err != nil {
				c.log.WithError(err).Error("Failed to handle sign request")
			}
		}()
	}
	
	c.markReady(coordReq.SessionID, c.localNodeID)
	return c.broadcastReady(coordReq.SessionID, MsgTypeSignReady)
}

// handleReshareRequest 处理重分享请求
func (c *Coordinator) handleReshareRequest(msg *types.P2PMessage) error {
	var coordReq CoordinationRequest
	if err := json.Unmarshal(msg.Data, &coordReq); err != nil {
		return fmt.Errorf("failed to unmarshal coordination request: %w", err)
	}
	
	c.log.WithFields(logrus.Fields{
		"session_id": coordReq.SessionID,
		"initiator":  coordReq.InitiatorID,
	}).Info("Received reshare request")
	
	isParty := false
	for _, pid := range coordReq.PartyIDs {
		if pid == c.localNodeID {
			isParty = true
			break
		}
	}
	
	if !isParty {
		return nil
	}
	
	c.readyMu.Lock()
	if _, ok := c.readyNodes[coordReq.SessionID]; !ok {
		c.readyNodes[coordReq.SessionID] = make(map[string]bool)
		for _, pid := range coordReq.PartyIDs {
			c.readyNodes[coordReq.SessionID][pid] = false
		}
	}
	c.readyMu.Unlock()
	
	var reshareReq types.ResharingRequest
	if err := json.Unmarshal(coordReq.Data, &reshareReq); err != nil {
		return fmt.Errorf("failed to unmarshal reshare request: %w", err)
	}
	
	if c.onReshareRequest != nil {
		go func() {
			if err := c.onReshareRequest(c.ctx, coordReq.SessionID, &reshareReq); err != nil {
				c.log.WithError(err).Error("Failed to handle reshare request")
			}
		}()
	}
	
	c.markReady(coordReq.SessionID, c.localNodeID)
	return c.broadcastReady(coordReq.SessionID, MsgTypeReshareReady)
}

// handleReadyMessage 处理就绪消息
func (c *Coordinator) handleReadyMessage(msg *types.P2PMessage) error {
	var ready CoordinationReady
	if err := json.Unmarshal(msg.Data, &ready); err != nil {
		return fmt.Errorf("failed to unmarshal ready message: %w", err)
	}
	
	c.log.WithFields(logrus.Fields{
		"session_id": ready.SessionID,
		"node_id":    ready.NodeID,
		"ready":      ready.Ready,
	}).Debug("Received ready message")
	
	if ready.Ready {
		c.markReady(ready.SessionID, ready.NodeID)
	}
	
	return nil
}

// Cleanup 清理会话
func (c *Coordinator) Cleanup(sessionID string) {
	c.pendingMu.Lock()
	delete(c.pendingRequests, sessionID)
	c.pendingMu.Unlock()
	
	c.readyMu.Lock()
	delete(c.readyNodes, sessionID)
	c.readyMu.Unlock()
}

// Stop 停止协调器
func (c *Coordinator) Stop() {
	c.cancel()
}
