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
	MsgTypeKeygenJoin    = "keygen_join"
	MsgTypeKeygenReady   = "keygen_ready"
	MsgTypeSignInit      = "sign_init"
	MsgTypeSignJoin      = "sign_join"
	MsgTypeReshareInit   = "reshare_init"
	MsgTypeReshareJoin   = "reshare_join"
	MsgTypeTSSRound      = "tss_round"
)

// KeygenInitMessage 密钥生成初始化消息
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
	PartyID   string `json:"party_id"`
}

// ReadyMessage 就绪消息
type ReadyMessage struct {
	SessionID string   `json:"session_id"`
	PartyID   string   `json:"party_id"`
	ReadyList []string `json:"ready_list"`
}

// Coordinator TSS会话协调器
type Coordinator struct {
	p2pHost      *p2p.P2PHost
	msgManager   *p2p.MessageManager
	keygenMgr    *KeygenManager
	signingMgr   *SigningManager
	resharingMgr *ResharingManager
	localNodeID  string
	
	// 待处理的会话
	pendingSessions map[string]*PendingSession
	sessionsMu      sync.RWMutex
	
	ctx    context.Context
	cancel context.CancelFunc
	log    *logrus.Entry
}

// PendingSession 待处理会话
type PendingSession struct {
	ID         string
	Type       string // keygen, sign, reshare
	InitMsg    interface{}
	ReadyNodes map[string]bool
	StartTime  time.Time
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
	c.p2pHost.RegisterHandler(MsgTypeTSSRound, c.handleTSSRound)
	
	// 订阅TSS会话主题
	if ps := c.p2pHost.GetPubSub(); ps != nil {
		ps.Subscribe("tss/coordinate")
	}
	
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
func (c *Coordinator) InitiateKeygen(ctx context.Context, req *types.KeygenRequest) (string, error) {
	sessionID := uuid.New().String()
	
	c.log.WithFields(logrus.Fields{
		"session_id": sessionID,
		"wallet_id":  req.WalletID,
		"threshold":  req.Threshold,
		"parties":    req.PartyIDs,
	}).Info("Initiating keygen")
	
	// 创建初始化消息
	initMsg := &KeygenInitMessage{
		SessionID:  sessionID,
		WalletID:   req.WalletID,
		Threshold:  req.Threshold,
		TotalParts: req.TotalParts,
		PartyIDs:   req.PartyIDs,
		Initiator:  c.localNodeID,
	}
	
	// 创建待处理会话
	pending := &PendingSession{
		ID:         sessionID,
		Type:       "keygen",
		InitMsg:    initMsg,
		ReadyNodes: make(map[string]bool),
		StartTime:  time.Now(),
	}
	pending.ReadyNodes[c.localNodeID] = true
	
	c.sessionsMu.Lock()
	c.pendingSessions[sessionID] = pending
	c.sessionsMu.Unlock()
	
	// 广播初始化消息
	data, _ := json.Marshal(initMsg)
	p2pMsg := &types.P2PMessage{
		Type:      MsgTypeKeygenInit,
		SessionID: sessionID,
		From:      c.localNodeID,
		Data:      data,
		Timestamp: time.Now().UnixNano(),
	}
	
	if err := c.p2pHost.BroadcastMessage(p2pMsg); err != nil {
		return "", fmt.Errorf("failed to broadcast keygen init: %w", err)
	}
	
	// 等待所有节点就绪
	if err := c.waitForReady(ctx, sessionID, req.PartyIDs); err != nil {
		return "", err
	}
	
	// 启动本地keygen
	_, err := c.keygenMgr.StartKeygen(ctx, req)
	if err != nil {
		return "", fmt.Errorf("failed to start keygen: %w", err)
	}
	
	return sessionID, nil
}

// InitiateSign 发起签名
func (c *Coordinator) InitiateSign(ctx context.Context, req *types.SignRequest) (string, error) {
	sessionID := req.RequestID
	if sessionID == "" {
		sessionID = uuid.New().String()
		req.RequestID = sessionID
	}
	
	c.log.WithFields(logrus.Fields{
		"session_id": sessionID,
		"wallet_id":  req.WalletID,
		"parties":    req.PartyIDs,
	}).Info("Initiating signing")
	
	// 创建初始化消息
	initMsg := &SignInitMessage{
		SessionID: sessionID,
		WalletID:  req.WalletID,
		Message:   req.Message,
		PartyIDs:  req.PartyIDs,
		Initiator: c.localNodeID,
	}
	
	// 创建待处理会话
	pending := &PendingSession{
		ID:         sessionID,
		Type:       "sign",
		InitMsg:    initMsg,
		ReadyNodes: make(map[string]bool),
		StartTime:  time.Now(),
	}
	pending.ReadyNodes[c.localNodeID] = true
	
	c.sessionsMu.Lock()
	c.pendingSessions[sessionID] = pending
	c.sessionsMu.Unlock()
	
	// 广播初始化消息
	data, _ := json.Marshal(initMsg)
	p2pMsg := &types.P2PMessage{
		Type:      MsgTypeSignInit,
		SessionID: sessionID,
		From:      c.localNodeID,
		Data:      data,
		Timestamp: time.Now().UnixNano(),
	}
	
	if err := c.p2pHost.BroadcastMessage(p2pMsg); err != nil {
		return "", fmt.Errorf("failed to broadcast sign init: %w", err)
	}
	
	// 等待所有节点就绪
	if err := c.waitForReady(ctx, sessionID, req.PartyIDs); err != nil {
		return "", err
	}
	
	// 启动本地签名
	_, err := c.signingMgr.StartSigning(ctx, req)
	if err != nil {
		return "", fmt.Errorf("failed to start signing: %w", err)
	}
	
	return sessionID, nil
}

// waitForReady 等待所有节点就绪
func (c *Coordinator) waitForReady(ctx context.Context, sessionID string, partyIDs []string) error {
	timeout := time.After(30 * time.Second)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timeout:
			return fmt.Errorf("timeout waiting for parties to be ready")
		case <-ticker.C:
			c.sessionsMu.RLock()
			pending, ok := c.pendingSessions[sessionID]
			c.sessionsMu.RUnlock()
			
			if !ok {
				return fmt.Errorf("session not found")
			}
			
			allReady := true
			for _, pid := range partyIDs {
				if !pending.ReadyNodes[pid] {
					allReady = false
					break
				}
			}
			
			if allReady {
				c.log.WithField("session_id", sessionID).Info("All parties ready")
				return nil
			}
		}
	}
}

// handleKeygenInit 处理keygen初始化消息
func (c *Coordinator) handleKeygenInit(msg *types.P2PMessage) error {
	var initMsg KeygenInitMessage
	if err := json.Unmarshal(msg.Data, &initMsg); err != nil {
		return err
	}
	
	c.log.WithFields(logrus.Fields{
		"session_id": initMsg.SessionID,
		"wallet_id":  initMsg.WalletID,
		"initiator":  initMsg.Initiator,
	}).Info("Received keygen init")
	
	// 检查是否是参与方
	isParty := false
	for _, pid := range initMsg.PartyIDs {
		if pid == c.localNodeID {
			isParty = true
			break
		}
	}
	
	if !isParty {
		c.log.Debug("Not a party member, ignoring")
		return nil
	}
	
	// 创建待处理会话
	pending := &PendingSession{
		ID:         initMsg.SessionID,
		Type:       "keygen",
		InitMsg:    &initMsg,
		ReadyNodes: make(map[string]bool),
		StartTime:  time.Now(),
	}
	
	c.sessionsMu.Lock()
	c.pendingSessions[initMsg.SessionID] = pending
	c.sessionsMu.Unlock()
	
	// 发送加入确认
	joinMsg := &JoinMessage{
		SessionID: initMsg.SessionID,
		PartyID:   c.localNodeID,
	}
	data, _ := json.Marshal(joinMsg)
	p2pMsg := &types.P2PMessage{
		Type:      MsgTypeKeygenJoin,
		SessionID: initMsg.SessionID,
		From:      c.localNodeID,
		Data:      data,
		Timestamp: time.Now().UnixNano(),
	}
	
	if err := c.p2pHost.BroadcastMessage(p2pMsg); err != nil {
		c.log.WithError(err).Error("Failed to send join message")
	}
	
	// 加入keygen会话
	req := &types.KeygenRequest{
		WalletID:   initMsg.WalletID,
		Threshold:  initMsg.Threshold,
		TotalParts: initMsg.TotalParts,
		PartyIDs:   initMsg.PartyIDs,
	}
	
	go func() {
		// 等待一小段时间让其他节点也准备好
		time.Sleep(500 * time.Millisecond)
		
		_, err := c.keygenMgr.JoinKeygen(c.ctx, initMsg.SessionID, req)
		if err != nil {
			c.log.WithError(err).Error("Failed to join keygen")
		}
	}()
	
	return nil
}

// handleKeygenJoin 处理keygen加入消息
func (c *Coordinator) handleKeygenJoin(msg *types.P2PMessage) error {
	var joinMsg JoinMessage
	if err := json.Unmarshal(msg.Data, &joinMsg); err != nil {
		return err
	}
	
	c.log.WithFields(logrus.Fields{
		"session_id": joinMsg.SessionID,
		"party_id":   joinMsg.PartyID,
	}).Debug("Received keygen join")
	
	c.sessionsMu.Lock()
	if pending, ok := c.pendingSessions[joinMsg.SessionID]; ok {
		pending.ReadyNodes[joinMsg.PartyID] = true
	}
	c.sessionsMu.Unlock()
	
	return nil
}

// handleKeygenReady 处理keygen就绪消息
func (c *Coordinator) handleKeygenReady(msg *types.P2PMessage) error {
	var readyMsg ReadyMessage
	if err := json.Unmarshal(msg.Data, &readyMsg); err != nil {
		return err
	}
	
	c.sessionsMu.Lock()
	if pending, ok := c.pendingSessions[readyMsg.SessionID]; ok {
		pending.ReadyNodes[readyMsg.PartyID] = true
	}
	c.sessionsMu.Unlock()
	
	return nil
}

// handleSignInit 处理签名初始化消息
func (c *Coordinator) handleSignInit(msg *types.P2PMessage) error {
	var initMsg SignInitMessage
	if err := json.Unmarshal(msg.Data, &initMsg); err != nil {
		return err
	}
	
	c.log.WithFields(logrus.Fields{
		"session_id": initMsg.SessionID,
		"wallet_id":  initMsg.WalletID,
		"initiator":  initMsg.Initiator,
	}).Info("Received sign init")
	
	// 检查是否是参与方
	isParty := false
	for _, pid := range initMsg.PartyIDs {
		if pid == c.localNodeID {
			isParty = true
			break
		}
	}
	
	if !isParty {
		return nil
	}
	
	// 发送加入确认
	joinMsg := &JoinMessage{
		SessionID: initMsg.SessionID,
		PartyID:   c.localNodeID,
	}
	data, _ := json.Marshal(joinMsg)
	p2pMsg := &types.P2PMessage{
		Type:      MsgTypeSignJoin,
		SessionID: initMsg.SessionID,
		From:      c.localNodeID,
		Data:      data,
		Timestamp: time.Now().UnixNano(),
	}
	
	c.p2pHost.BroadcastMessage(p2pMsg)
	
	// 加入签名会话
	req := &types.SignRequest{
		WalletID:  initMsg.WalletID,
		Message:   initMsg.Message,
		PartyIDs:  initMsg.PartyIDs,
		RequestID: initMsg.SessionID,
	}
	
	go func() {
		time.Sleep(500 * time.Millisecond)
		_, err := c.signingMgr.JoinSigning(c.ctx, initMsg.SessionID, req)
		if err != nil {
			c.log.WithError(err).Error("Failed to join signing")
		}
	}()
	
	return nil
}

// handleSignJoin 处理签名加入消息
func (c *Coordinator) handleSignJoin(msg *types.P2PMessage) error {
	var joinMsg JoinMessage
	if err := json.Unmarshal(msg.Data, &joinMsg); err != nil {
		return err
	}
	
	c.sessionsMu.Lock()
	if pending, ok := c.pendingSessions[joinMsg.SessionID]; ok {
		pending.ReadyNodes[joinMsg.PartyID] = true
	}
	c.sessionsMu.Unlock()
	
	return nil
}

// handleTSSRound 处理TSS轮次消息
func (c *Coordinator) handleTSSRound(msg *types.P2PMessage) error {
	// 路由到对应的会话
	return c.msgManager.SendToSession(msg.SessionID, msg)
}
