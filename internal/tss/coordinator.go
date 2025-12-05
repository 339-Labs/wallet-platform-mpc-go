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
	MsgTypeKeygenInvite    = "keygen_invite"
	MsgTypeKeygenAccept    = "keygen_accept"
	MsgTypeKeygenReady     = "keygen_ready"
	MsgTypeKeygenStart     = "keygen_start"
	MsgTypeSignInvite      = "sign_invite"
	MsgTypeSignAccept      = "sign_accept"
	MsgTypeSignReady       = "sign_ready"
	MsgTypeSignStart       = "sign_start"
	MsgTypeResharingInvite = "resharing_invite"
	MsgTypeResharingAccept = "resharing_accept"
)

// SessionInvite 会话邀请
type SessionInvite struct {
	SessionID   string   `json:"session_id"`
	Type        string   `json:"type"` // keygen, sign, resharing
	WalletID    string   `json:"wallet_id"`
	Threshold   int      `json:"threshold"`
	TotalParts  int      `json:"total_parts"`
	PartyIDs    []string `json:"party_ids"`
	Message     string   `json:"message,omitempty"`     // 用于签名
	OldPartyIDs []string `json:"old_party_ids,omitempty"` // 用于resharing
	InitiatorID string   `json:"initiator_id"`
	Timestamp   int64    `json:"timestamp"`
}

// SessionAccept 会话接受响应
type SessionAccept struct {
	SessionID string `json:"session_id"`
	PartyID   string `json:"party_id"`
	Accepted  bool   `json:"accepted"`
	Error     string `json:"error,omitempty"`
}

// Coordinator TSS会话协调器
type Coordinator struct {
	p2pHost      *p2p.P2PHost
	msgManager   *p2p.MessageManager
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

// PendingSession 待处理的会话
type PendingSession struct {
	Invite      *SessionInvite
	Accepts     map[string]bool
	AcceptCount int
	ReadyChan   chan struct{}
	StartChan   chan struct{}
	CreatedAt   time.Time
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
	c.p2pHost.RegisterHandler(MsgTypeKeygenInvite, c.handleKeygenInvite)
	c.p2pHost.RegisterHandler(MsgTypeKeygenAccept, c.handleKeygenAccept)
	c.p2pHost.RegisterHandler(MsgTypeKeygenStart, c.handleKeygenStart)
	c.p2pHost.RegisterHandler(MsgTypeSignInvite, c.handleSignInvite)
	c.p2pHost.RegisterHandler(MsgTypeSignAccept, c.handleSignAccept)
	c.p2pHost.RegisterHandler(MsgTypeSignStart, c.handleSignStart)
	
	c.log.Info("Coordinator started")
	return nil
}

// Stop 停止协调器
func (c *Coordinator) Stop() error {
	c.cancel()
	c.log.Info("Coordinator stopped")
	return nil
}

// InitiateKeygen 发起密钥生成
func (c *Coordinator) InitiateKeygen(ctx context.Context, req *types.KeygenRequest) (*KeygenSession, error) {
	sessionID := uuid.New().String()
	
	c.log.WithFields(logrus.Fields{
		"session_id": sessionID,
		"wallet_id":  req.WalletID,
		"threshold":  req.Threshold,
		"parties":    req.PartyIDs,
	}).Info("Initiating keygen")
	
	// 创建邀请
	invite := &SessionInvite{
		SessionID:   sessionID,
		Type:        "keygen",
		WalletID:    req.WalletID,
		Threshold:   req.Threshold,
		TotalParts:  req.TotalParts,
		PartyIDs:    req.PartyIDs,
		InitiatorID: c.localNodeID,
		Timestamp:   time.Now().UnixNano(),
	}
	
	// 创建待处理会话
	pending := &PendingSession{
		Invite:    invite,
		Accepts:   make(map[string]bool),
		ReadyChan: make(chan struct{}),
		StartChan: make(chan struct{}),
		CreatedAt: time.Now(),
	}
	
	// 自己先接受
	pending.Accepts[c.localNodeID] = true
	pending.AcceptCount = 1
	
	c.pendingMu.Lock()
	c.pendingSessions[sessionID] = pending
	c.pendingMu.Unlock()
	
	// 广播邀请给其他参与方
	if err := c.broadcastInvite(invite); err != nil {
		return nil, fmt.Errorf("failed to broadcast invite: %w", err)
	}
	
	// 等待所有参与方接受
	waitCtx, waitCancel := context.WithTimeout(ctx, 30*time.Second)
	defer waitCancel()
	
	if err := c.waitForAccepts(waitCtx, sessionID, len(req.PartyIDs)); err != nil {
		c.cleanupPending(sessionID)
		return nil, fmt.Errorf("failed to get all accepts: %w", err)
	}
	
	c.log.WithField("session_id", sessionID).Info("All parties accepted, starting keygen")
	
	// 广播开始信号
	if err := c.broadcastStart(sessionID, "keygen"); err != nil {
		c.cleanupPending(sessionID)
		return nil, fmt.Errorf("failed to broadcast start: %w", err)
	}
	
	// 本地启动keygen
	session, err := c.keygenMgr.StartKeygen(ctx, &types.KeygenRequest{
		WalletID:   req.WalletID,
		Threshold:  req.Threshold,
		TotalParts: req.TotalParts,
		PartyIDs:   req.PartyIDs,
	})
	if err != nil {
		c.cleanupPending(sessionID)
		return nil, err
	}
	
	return session, nil
}

// InitiateSigning 发起签名
func (c *Coordinator) InitiateSigning(ctx context.Context, req *types.SignRequest) (*SigningSession, error) {
	sessionID := req.RequestID
	if sessionID == "" {
		sessionID = uuid.New().String()
	}
	
	c.log.WithFields(logrus.Fields{
		"session_id": sessionID,
		"wallet_id":  req.WalletID,
		"parties":    req.PartyIDs,
	}).Info("Initiating signing")
	
	// 创建邀请
	invite := &SessionInvite{
		SessionID:   sessionID,
		Type:        "sign",
		WalletID:    req.WalletID,
		PartyIDs:    req.PartyIDs,
		Message:     req.Message,
		InitiatorID: c.localNodeID,
		Timestamp:   time.Now().UnixNano(),
	}
	
	// 创建待处理会话
	pending := &PendingSession{
		Invite:    invite,
		Accepts:   make(map[string]bool),
		ReadyChan: make(chan struct{}),
		StartChan: make(chan struct{}),
		CreatedAt: time.Now(),
	}
	
	pending.Accepts[c.localNodeID] = true
	pending.AcceptCount = 1
	
	c.pendingMu.Lock()
	c.pendingSessions[sessionID] = pending
	c.pendingMu.Unlock()
	
	// 广播邀请
	if err := c.broadcastInvite(invite); err != nil {
		return nil, fmt.Errorf("failed to broadcast invite: %w", err)
	}
	
	// 等待接受
	waitCtx, waitCancel := context.WithTimeout(ctx, 30*time.Second)
	defer waitCancel()
	
	if err := c.waitForAccepts(waitCtx, sessionID, len(req.PartyIDs)); err != nil {
		c.cleanupPending(sessionID)
		return nil, fmt.Errorf("failed to get all accepts: %w", err)
	}
	
	// 广播开始
	if err := c.broadcastStart(sessionID, "sign"); err != nil {
		c.cleanupPending(sessionID)
		return nil, fmt.Errorf("failed to broadcast start: %w", err)
	}
	
	// 本地启动签名
	req.RequestID = sessionID
	session, err := c.signingMgr.StartSigning(ctx, req)
	if err != nil {
		c.cleanupPending(sessionID)
		return nil, err
	}
	
	return session, nil
}

// broadcastInvite 广播邀请
func (c *Coordinator) broadcastInvite(invite *SessionInvite) error {
	data, err := json.Marshal(invite)
	if err != nil {
		return err
	}
	
	msgType := MsgTypeKeygenInvite
	if invite.Type == "sign" {
		msgType = MsgTypeSignInvite
	} else if invite.Type == "resharing" {
		msgType = MsgTypeResharingInvite
	}
	
	msg := &types.P2PMessage{
		Type:      msgType,
		SessionID: invite.SessionID,
		From:      c.localNodeID,
		Data:      data,
		Timestamp: time.Now().UnixNano(),
	}
	
	return c.p2pHost.BroadcastMessage(msg)
}

// broadcastStart 广播开始信号
func (c *Coordinator) broadcastStart(sessionID, sessionType string) error {
	msgType := MsgTypeKeygenStart
	if sessionType == "sign" {
		msgType = MsgTypeSignStart
	}
	
	msg := &types.P2PMessage{
		Type:      msgType,
		SessionID: sessionID,
		From:      c.localNodeID,
		Timestamp: time.Now().UnixNano(),
	}
	
	return c.p2pHost.BroadcastMessage(msg)
}

// waitForAccepts 等待所有参与方接受
func (c *Coordinator) waitForAccepts(ctx context.Context, sessionID string, required int) error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			c.pendingMu.RLock()
			pending, ok := c.pendingSessions[sessionID]
			c.pendingMu.RUnlock()
			
			if !ok {
				return fmt.Errorf("session not found")
			}
			
			if pending.AcceptCount >= required {
				return nil
			}
		}
	}
}

// handleKeygenInvite 处理keygen邀请
func (c *Coordinator) handleKeygenInvite(msg *types.P2PMessage) error {
	var invite SessionInvite
	if err := json.Unmarshal(msg.Data, &invite); err != nil {
		return err
	}
	
	c.log.WithFields(logrus.Fields{
		"session_id": invite.SessionID,
		"wallet_id":  invite.WalletID,
		"from":       invite.InitiatorID,
	}).Info("Received keygen invite")
	
	// 检查自己是否在参与方列表中
	isParty := false
	for _, pid := range invite.PartyIDs {
		if pid == c.localNodeID {
			isParty = true
			break
		}
	}
	
	if !isParty {
		c.log.Debug("Not in party list, ignoring invite")
		return nil
	}
	
	// 创建待处理会话
	pending := &PendingSession{
		Invite:    &invite,
		Accepts:   make(map[string]bool),
		StartChan: make(chan struct{}),
		CreatedAt: time.Now(),
	}
	
	c.pendingMu.Lock()
	c.pendingSessions[invite.SessionID] = pending
	c.pendingMu.Unlock()
	
	// 发送接受响应
	accept := &SessionAccept{
		SessionID: invite.SessionID,
		PartyID:   c.localNodeID,
		Accepted:  true,
	}
	
	return c.sendAccept(accept, MsgTypeKeygenAccept)
}

// handleKeygenAccept 处理keygen接受
func (c *Coordinator) handleKeygenAccept(msg *types.P2PMessage) error {
	var accept SessionAccept
	if err := json.Unmarshal(msg.Data, &accept); err != nil {
		return err
	}
	
	c.log.WithFields(logrus.Fields{
		"session_id": accept.SessionID,
		"party_id":   accept.PartyID,
		"accepted":   accept.Accepted,
	}).Info("Received keygen accept")
	
	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()
	
	pending, ok := c.pendingSessions[accept.SessionID]
	if !ok {
		return nil
	}
	
	if accept.Accepted && !pending.Accepts[accept.PartyID] {
		pending.Accepts[accept.PartyID] = true
		pending.AcceptCount++
	}
	
	return nil
}

// handleKeygenStart 处理keygen开始
func (c *Coordinator) handleKeygenStart(msg *types.P2PMessage) error {
	sessionID := msg.SessionID
	
	c.log.WithField("session_id", sessionID).Info("Received keygen start signal")
	
	c.pendingMu.RLock()
	pending, ok := c.pendingSessions[sessionID]
	c.pendingMu.RUnlock()
	
	if !ok {
		return fmt.Errorf("pending session not found: %s", sessionID)
	}
	
	// 启动keygen
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		
		_, err := c.keygenMgr.JoinKeygen(ctx, sessionID, &types.KeygenRequest{
			WalletID:   pending.Invite.WalletID,
			Threshold:  pending.Invite.Threshold,
			TotalParts: pending.Invite.TotalParts,
			PartyIDs:   pending.Invite.PartyIDs,
		})
		if err != nil {
			c.log.WithError(err).Error("Failed to join keygen")
		}
		
		c.cleanupPending(sessionID)
	}()
	
	return nil
}

// handleSignInvite 处理签名邀请
func (c *Coordinator) handleSignInvite(msg *types.P2PMessage) error {
	var invite SessionInvite
	if err := json.Unmarshal(msg.Data, &invite); err != nil {
		return err
	}
	
	c.log.WithFields(logrus.Fields{
		"session_id": invite.SessionID,
		"wallet_id":  invite.WalletID,
		"from":       invite.InitiatorID,
	}).Info("Received sign invite")
	
	// 检查自己是否在参与方列表中
	isParty := false
	for _, pid := range invite.PartyIDs {
		if pid == c.localNodeID {
			isParty = true
			break
		}
	}
	
	if !isParty {
		return nil
	}
	
	// 创建待处理会话
	pending := &PendingSession{
		Invite:    &invite,
		Accepts:   make(map[string]bool),
		StartChan: make(chan struct{}),
		CreatedAt: time.Now(),
	}
	
	c.pendingMu.Lock()
	c.pendingSessions[invite.SessionID] = pending
	c.pendingMu.Unlock()
	
	// 发送接受响应
	accept := &SessionAccept{
		SessionID: invite.SessionID,
		PartyID:   c.localNodeID,
		Accepted:  true,
	}
	
	return c.sendAccept(accept, MsgTypeSignAccept)
}

// handleSignAccept 处理签名接受
func (c *Coordinator) handleSignAccept(msg *types.P2PMessage) error {
	var accept SessionAccept
	if err := json.Unmarshal(msg.Data, &accept); err != nil {
		return err
	}
	
	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()
	
	pending, ok := c.pendingSessions[accept.SessionID]
	if !ok {
		return nil
	}
	
	if accept.Accepted && !pending.Accepts[accept.PartyID] {
		pending.Accepts[accept.PartyID] = true
		pending.AcceptCount++
	}
	
	return nil
}

// handleSignStart 处理签名开始
func (c *Coordinator) handleSignStart(msg *types.P2PMessage) error {
	sessionID := msg.SessionID
	
	c.log.WithField("session_id", sessionID).Info("Received sign start signal")
	
	c.pendingMu.RLock()
	pending, ok := c.pendingSessions[sessionID]
	c.pendingMu.RUnlock()
	
	if !ok {
		return fmt.Errorf("pending session not found: %s", sessionID)
	}
	
	// 启动签名
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		
		_, err := c.signingMgr.JoinSigning(ctx, sessionID, &types.SignRequest{
			WalletID:  pending.Invite.WalletID,
			Message:   pending.Invite.Message,
			PartyIDs:  pending.Invite.PartyIDs,
			RequestID: sessionID,
		})
		if err != nil {
			c.log.WithError(err).Error("Failed to join signing")
		}
		
		c.cleanupPending(sessionID)
	}()
	
	return nil
}

// sendAccept 发送接受响应
func (c *Coordinator) sendAccept(accept *SessionAccept, msgType string) error {
	data, err := json.Marshal(accept)
	if err != nil {
		return err
	}
	
	msg := &types.P2PMessage{
		Type:      msgType,
		SessionID: accept.SessionID,
		From:      c.localNodeID,
		Data:      data,
		Timestamp: time.Now().UnixNano(),
	}
	
	return c.p2pHost.BroadcastMessage(msg)
}

// cleanupPending 清理待处理会话
func (c *Coordinator) cleanupPending(sessionID string) {
	c.pendingMu.Lock()
	delete(c.pendingSessions, sessionID)
	c.pendingMu.Unlock()
}
