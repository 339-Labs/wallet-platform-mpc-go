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
	MsgTypeKeygenInit    = "keygen_init"     // 发起keygen
	MsgTypeKeygenJoin    = "keygen_join"     // 加入keygen
	MsgTypeKeygenReady   = "keygen_ready"    // 准备就绪
	MsgTypeKeygenStart   = "keygen_start"    // 开始keygen
	MsgTypeSignInit      = "sign_init"       // 发起签名
	MsgTypeSignJoin      = "sign_join"       // 加入签名
	MsgTypeSignReady     = "sign_ready"      // 准备就绪
	MsgTypeSignStart     = "sign_start"      // 开始签名
	MsgTypeReshareInit   = "reshare_init"    // 发起重分享
	MsgTypeReshareJoin   = "reshare_join"    // 加入重分享
	MsgTypeReshareReady  = "reshare_ready"   // 准备就绪
	MsgTypeReshareStart  = "reshare_start"   // 开始重分享
)

// SessionInitMessage 会话初始化消息
type SessionInitMessage struct {
	SessionID   string   `json:"session_id"`
	SessionType string   `json:"session_type"` // keygen, sign, reshare
	WalletID    string   `json:"wallet_id"`
	Threshold   int      `json:"threshold"`
	TotalParts  int      `json:"total_parts"`
	PartyIDs    []string `json:"party_ids"`
	Initiator   string   `json:"initiator"`
	Message     string   `json:"message,omitempty"`      // 签名时的消息
	OldPartyIDs []string `json:"old_party_ids,omitempty"` // 重分享时的旧参与方
	NewPartyIDs []string `json:"new_party_ids,omitempty"` // 重分享时的新参与方
	Timestamp   int64    `json:"timestamp"`
}

// SessionReadyMessage 会话就绪消息
type SessionReadyMessage struct {
	SessionID string `json:"session_id"`
	PartyID   string `json:"party_id"`
	Ready     bool   `json:"ready"`
	Timestamp int64  `json:"timestamp"`
}

// Coordinator TSS会话协调器
type Coordinator struct {
	p2pHost     *p2p.P2PHost
	msgManager  *p2p.MessageManager
	localNodeID string
	
	// 待处理的会话
	pendingSessions map[string]*PendingSession
	sessionMu       sync.RWMutex
	
	// 会话处理器
	keygenHandler   func(ctx context.Context, sessionID string, msg *SessionInitMessage) error
	signHandler     func(ctx context.Context, sessionID string, msg *SessionInitMessage) error
	reshareHandler  func(ctx context.Context, sessionID string, msg *SessionInitMessage) error
	
	ctx    context.Context
	cancel context.CancelFunc
	log    *logrus.Entry
}

// PendingSession 待处理的会话
type PendingSession struct {
	InitMsg     *SessionInitMessage
	ReadyParties map[string]bool
	AllReady    chan struct{}
	StartTime   time.Time
}

// NewCoordinator 创建协调器
func NewCoordinator(ctx context.Context, p2pHost *p2p.P2PHost, msgManager *p2p.MessageManager, localNodeID string) *Coordinator {
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
	p2pHost.RegisterHandler(MsgTypeKeygenStart, c.handleKeygenStart)
	p2pHost.RegisterHandler(MsgTypeSignInit, c.handleSignInit)
	p2pHost.RegisterHandler(MsgTypeSignReady, c.handleSignReady)
	p2pHost.RegisterHandler(MsgTypeSignStart, c.handleSignStart)
	p2pHost.RegisterHandler(MsgTypeReshareInit, c.handleReshareInit)
	p2pHost.RegisterHandler(MsgTypeReshareReady, c.handleReshareReady)
	p2pHost.RegisterHandler(MsgTypeReshareStart, c.handleReshareStart)
	
	return c
}

// SetKeygenHandler 设置keygen处理器
func (c *Coordinator) SetKeygenHandler(handler func(ctx context.Context, sessionID string, msg *SessionInitMessage) error) {
	c.keygenHandler = handler
}

// SetSignHandler 设置sign处理器
func (c *Coordinator) SetSignHandler(handler func(ctx context.Context, sessionID string, msg *SessionInitMessage) error) {
	c.signHandler = handler
}

// SetReshareHandler 设置reshare处理器
func (c *Coordinator) SetReshareHandler(handler func(ctx context.Context, sessionID string, msg *SessionInitMessage) error) {
	c.reshareHandler = handler
}

// InitiateKeygen 发起keygen会话
func (c *Coordinator) InitiateKeygen(ctx context.Context, sessionID, walletID string, threshold, totalParts int, partyIDs []string) error {
	c.log.WithFields(logrus.Fields{
		"session_id": sessionID,
		"wallet_id":  walletID,
		"threshold":  threshold,
		"parties":    partyIDs,
	}).Info("Initiating keygen session")
	
	initMsg := &SessionInitMessage{
		SessionID:   sessionID,
		SessionType: "keygen",
		WalletID:    walletID,
		Threshold:   threshold,
		TotalParts:  totalParts,
		PartyIDs:    partyIDs,
		Initiator:   c.localNodeID,
		Timestamp:   time.Now().UnixNano(),
	}
	
	// 创建待处理会话
	pending := &PendingSession{
		InitMsg:      initMsg,
		ReadyParties: make(map[string]bool),
		AllReady:     make(chan struct{}),
		StartTime:    time.Now(),
	}
	
	c.sessionMu.Lock()
	c.pendingSessions[sessionID] = pending
	c.sessionMu.Unlock()
	
	// 广播初始化消息
	if err := c.broadcastSessionInit(MsgTypeKeygenInit, initMsg); err != nil {
		return fmt.Errorf("failed to broadcast keygen init: %w", err)
	}
	
	// 本节点也标记为ready
	c.markReady(sessionID, c.localNodeID)
	
	// 等待所有节点ready
	return c.waitForAllReady(ctx, sessionID, partyIDs, 60*time.Second)
}

// InitiateSign 发起签名会话
func (c *Coordinator) InitiateSign(ctx context.Context, sessionID, walletID, message string, threshold int, partyIDs []string) error {
	c.log.WithFields(logrus.Fields{
		"session_id": sessionID,
		"wallet_id":  walletID,
		"parties":    partyIDs,
	}).Info("Initiating sign session")
	
	initMsg := &SessionInitMessage{
		SessionID:   sessionID,
		SessionType: "sign",
		WalletID:    walletID,
		Threshold:   threshold,
		TotalParts:  len(partyIDs),
		PartyIDs:    partyIDs,
		Initiator:   c.localNodeID,
		Message:     message,
		Timestamp:   time.Now().UnixNano(),
	}
	
	pending := &PendingSession{
		InitMsg:      initMsg,
		ReadyParties: make(map[string]bool),
		AllReady:     make(chan struct{}),
		StartTime:    time.Now(),
	}
	
	c.sessionMu.Lock()
	c.pendingSessions[sessionID] = pending
	c.sessionMu.Unlock()
	
	if err := c.broadcastSessionInit(MsgTypeSignInit, initMsg); err != nil {
		return fmt.Errorf("failed to broadcast sign init: %w", err)
	}
	
	c.markReady(sessionID, c.localNodeID)
	
	return c.waitForAllReady(ctx, sessionID, partyIDs, 30*time.Second)
}

// broadcastSessionInit 广播会话初始化消息
func (c *Coordinator) broadcastSessionInit(msgType string, initMsg *SessionInitMessage) error {
	data, err := json.Marshal(initMsg)
	if err != nil {
		return err
	}
	
	msg := &types.P2PMessage{
		Type:      msgType,
		SessionID: initMsg.SessionID,
		From:      c.localNodeID,
		Data:      data,
		Timestamp: time.Now().UnixNano(),
	}
	
	return c.p2pHost.BroadcastMessage(msg)
}

// broadcastReady 广播ready消息
func (c *Coordinator) broadcastReady(sessionID, msgType string) error {
	readyMsg := &SessionReadyMessage{
		SessionID: sessionID,
		PartyID:   c.localNodeID,
		Ready:     true,
		Timestamp: time.Now().UnixNano(),
	}
	
	data, err := json.Marshal(readyMsg)
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

// markReady 标记节点ready
func (c *Coordinator) markReady(sessionID, partyID string) {
	c.sessionMu.Lock()
	defer c.sessionMu.Unlock()
	
	pending, ok := c.pendingSessions[sessionID]
	if !ok {
		return
	}
	
	pending.ReadyParties[partyID] = true
	c.log.WithFields(logrus.Fields{
		"session_id": sessionID,
		"party_id":   partyID,
		"ready_count": len(pending.ReadyParties),
		"total":      len(pending.InitMsg.PartyIDs),
	}).Debug("Party marked ready")
	
	// 检查是否所有节点都ready
	if len(pending.ReadyParties) >= len(pending.InitMsg.PartyIDs) {
		select {
		case <-pending.AllReady:
		default:
			close(pending.AllReady)
		}
	}
}

// waitForAllReady 等待所有节点ready
func (c *Coordinator) waitForAllReady(ctx context.Context, sessionID string, partyIDs []string, timeout time.Duration) error {
	c.sessionMu.RLock()
	pending, ok := c.pendingSessions[sessionID]
	c.sessionMu.RUnlock()
	
	if !ok {
		return fmt.Errorf("session not found: %s", sessionID)
	}
	
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	
	select {
	case <-pending.AllReady:
		c.log.WithField("session_id", sessionID).Info("All parties ready")
		return nil
	case <-timer.C:
		c.sessionMu.RLock()
		readyCount := len(pending.ReadyParties)
		c.sessionMu.RUnlock()
		return fmt.Errorf("timeout waiting for parties: got %d/%d ready", readyCount, len(partyIDs))
	case <-ctx.Done():
		return ctx.Err()
	}
}

// handleKeygenInit 处理keygen初始化消息
func (c *Coordinator) handleKeygenInit(msg *types.P2PMessage) error {
	var initMsg SessionInitMessage
	if err := json.Unmarshal(msg.Data, &initMsg); err != nil {
		return err
	}
	
	c.log.WithFields(logrus.Fields{
		"session_id": initMsg.SessionID,
		"initiator":  initMsg.Initiator,
		"parties":    initMsg.PartyIDs,
	}).Info("Received keygen init")
	
	// 检查自己是否在参与方列表中
	if !contains(initMsg.PartyIDs, c.localNodeID) {
		c.log.Debug("Not in party list, ignoring")
		return nil
	}
	
	// 如果是发起者，忽略（已经处理过了）
	if initMsg.Initiator == c.localNodeID {
		return nil
	}
	
	// 创建待处理会话
	pending := &PendingSession{
		InitMsg:      &initMsg,
		ReadyParties: make(map[string]bool),
		AllReady:     make(chan struct{}),
		StartTime:    time.Now(),
	}
	
	c.sessionMu.Lock()
	c.pendingSessions[initMsg.SessionID] = pending
	c.sessionMu.Unlock()
	
	// 调用keygen处理器
	if c.keygenHandler != nil {
		go func() {
			if err := c.keygenHandler(c.ctx, initMsg.SessionID, &initMsg); err != nil {
				c.log.WithError(err).Error("Keygen handler failed")
			}
		}()
	}
	
	// 广播ready
	return c.broadcastReady(initMsg.SessionID, MsgTypeKeygenReady)
}

// handleKeygenReady 处理keygen ready消息
func (c *Coordinator) handleKeygenReady(msg *types.P2PMessage) error {
	var readyMsg SessionReadyMessage
	if err := json.Unmarshal(msg.Data, &readyMsg); err != nil {
		return err
	}
	
	c.log.WithFields(logrus.Fields{
		"session_id": readyMsg.SessionID,
		"party_id":   readyMsg.PartyID,
	}).Debug("Received keygen ready")
	
	c.markReady(readyMsg.SessionID, readyMsg.PartyID)
	return nil
}

// handleKeygenStart 处理keygen开始消息
func (c *Coordinator) handleKeygenStart(msg *types.P2PMessage) error {
	c.log.WithField("session_id", msg.SessionID).Info("Received keygen start")
	return nil
}

// handleSignInit 处理sign初始化消息
func (c *Coordinator) handleSignInit(msg *types.P2PMessage) error {
	var initMsg SessionInitMessage
	if err := json.Unmarshal(msg.Data, &initMsg); err != nil {
		return err
	}
	
	c.log.WithFields(logrus.Fields{
		"session_id": initMsg.SessionID,
		"initiator":  initMsg.Initiator,
	}).Info("Received sign init")
	
	if !contains(initMsg.PartyIDs, c.localNodeID) {
		return nil
	}
	
	if initMsg.Initiator == c.localNodeID {
		return nil
	}
	
	pending := &PendingSession{
		InitMsg:      &initMsg,
		ReadyParties: make(map[string]bool),
		AllReady:     make(chan struct{}),
		StartTime:    time.Now(),
	}
	
	c.sessionMu.Lock()
	c.pendingSessions[initMsg.SessionID] = pending
	c.sessionMu.Unlock()
	
	if c.signHandler != nil {
		go func() {
			if err := c.signHandler(c.ctx, initMsg.SessionID, &initMsg); err != nil {
				c.log.WithError(err).Error("Sign handler failed")
			}
		}()
	}
	
	return c.broadcastReady(initMsg.SessionID, MsgTypeSignReady)
}

// handleSignReady 处理sign ready消息
func (c *Coordinator) handleSignReady(msg *types.P2PMessage) error {
	var readyMsg SessionReadyMessage
	if err := json.Unmarshal(msg.Data, &readyMsg); err != nil {
		return err
	}
	
	c.markReady(readyMsg.SessionID, readyMsg.PartyID)
	return nil
}

// handleSignStart 处理sign开始消息
func (c *Coordinator) handleSignStart(msg *types.P2PMessage) error {
	return nil
}

// handleReshareInit 处理reshare初始化消息
func (c *Coordinator) handleReshareInit(msg *types.P2PMessage) error {
	var initMsg SessionInitMessage
	if err := json.Unmarshal(msg.Data, &initMsg); err != nil {
		return err
	}
	
	c.log.WithFields(logrus.Fields{
		"session_id": initMsg.SessionID,
		"initiator":  initMsg.Initiator,
	}).Info("Received reshare init")
	
	// 检查是否在新旧参与方列表中
	inOld := contains(initMsg.OldPartyIDs, c.localNodeID)
	inNew := contains(initMsg.NewPartyIDs, c.localNodeID)
	
	if !inOld && !inNew {
		return nil
	}
	
	if initMsg.Initiator == c.localNodeID {
		return nil
	}
	
	pending := &PendingSession{
		InitMsg:      &initMsg,
		ReadyParties: make(map[string]bool),
		AllReady:     make(chan struct{}),
		StartTime:    time.Now(),
	}
	
	c.sessionMu.Lock()
	c.pendingSessions[initMsg.SessionID] = pending
	c.sessionMu.Unlock()
	
	if c.reshareHandler != nil {
		go func() {
			if err := c.reshareHandler(c.ctx, initMsg.SessionID, &initMsg); err != nil {
				c.log.WithError(err).Error("Reshare handler failed")
			}
		}()
	}
	
	return c.broadcastReady(initMsg.SessionID, MsgTypeReshareReady)
}

// handleReshareReady 处理reshare ready消息
func (c *Coordinator) handleReshareReady(msg *types.P2PMessage) error {
	var readyMsg SessionReadyMessage
	if err := json.Unmarshal(msg.Data, &readyMsg); err != nil {
		return err
	}
	
	c.markReady(readyMsg.SessionID, readyMsg.PartyID)
	return nil
}

// handleReshareStart 处理reshare开始消息
func (c *Coordinator) handleReshareStart(msg *types.P2PMessage) error {
	return nil
}

// CleanupSession 清理会话
func (c *Coordinator) CleanupSession(sessionID string) {
	c.sessionMu.Lock()
	defer c.sessionMu.Unlock()
	delete(c.pendingSessions, sessionID)
}

// Stop 停止协调器
func (c *Coordinator) Stop() {
	c.cancel()
}
