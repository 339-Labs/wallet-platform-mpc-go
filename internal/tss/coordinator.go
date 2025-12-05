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

// 会话协调消息类型
const (
	MsgTypeKeygenInvite    = "keygen_invite"
	MsgTypeKeygenAccept    = "keygen_accept"
	MsgTypeSignInvite      = "sign_invite"
	MsgTypeSignAccept      = "sign_accept"
	MsgTypeReshareInvite   = "reshare_invite"
	MsgTypeReshareAccept   = "reshare_accept"
)

// SessionInvite 会话邀请
type SessionInvite struct {
	SessionID    string   `json:"session_id"`
	Type         string   `json:"type"` // keygen, sign, reshare
	WalletID     string   `json:"wallet_id"`
	Threshold    int      `json:"threshold"`
	TotalParts   int      `json:"total_parts"`
	PartyIDs     []string `json:"party_ids"`
	Message      string   `json:"message,omitempty"`      // 签名时的消息
	OldThreshold int      `json:"old_threshold,omitempty"` // reshare用
	OldPartyIDs  []string `json:"old_party_ids,omitempty"` // reshare用
	NewThreshold int      `json:"new_threshold,omitempty"` // reshare用
	NewPartyIDs  []string `json:"new_party_ids,omitempty"` // reshare用
	Initiator    string   `json:"initiator"`
	Timestamp    int64    `json:"timestamp"`
}

// SessionAccept 会话接受确认
type SessionAccept struct {
	SessionID string `json:"session_id"`
	NodeID    string `json:"node_id"`
	Accepted  bool   `json:"accepted"`
	Error     string `json:"error,omitempty"`
}

// Coordinator 会话协调器 - 负责协调多节点会话
type Coordinator struct {
	p2pHost      *p2p.P2PHost
	msgManager   *p2p.MessageManager
	keygenMgr    *KeygenManager
	signingMgr   *SigningManager
	resharingMgr *ResharingManager
	localNodeID  string
	
	// 等待确认的会话
	pendingSessions map[string]*PendingSession
	pendingMu       sync.RWMutex
	
	ctx    context.Context
	cancel context.CancelFunc
	log    *logrus.Entry
}

// PendingSession 待确认的会话
type PendingSession struct {
	Invite      *SessionInvite
	Accepted    map[string]bool
	AcceptChan  chan string
	StartTime   time.Time
}

// NewCoordinator 创建协调器
func NewCoordinator(
	ctx context.Context,
	p2pHost *p2p.P2PHost,
	msgManager *p2p.MessageManager,
	keygenMgr *KeygenManager,
	signingMgr *SigningManager,
	resharingMgr *ResharingManager,
	localNodeID string,
) *Coordinator {
	ctx, cancel := context.WithCancel(ctx)
	
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
	c.p2pHost.RegisterHandler(MsgTypeSignInvite, c.handleSignInvite)
	c.p2pHost.RegisterHandler(MsgTypeSignAccept, c.handleSignAccept)
	c.p2pHost.RegisterHandler(MsgTypeReshareInvite, c.handleReshareInvite)
	c.p2pHost.RegisterHandler(MsgTypeReshareAccept, c.handleReshareAccept)
	
	c.log.Info("Coordinator started")
	return nil
}

// Stop 停止协调器
func (c *Coordinator) Stop() error {
	c.cancel()
	return nil
}

// InitiateKeygen 发起keygen并等待所有节点就绪
func (c *Coordinator) InitiateKeygen(ctx context.Context, sessionID, walletID string, threshold, totalParts int, partyIDs []string) error {
	c.log.WithFields(logrus.Fields{
		"session_id": sessionID,
		"wallet_id":  walletID,
		"party_ids":  partyIDs,
	}).Info("Initiating keygen session")
	
	// 创建邀请
	invite := &SessionInvite{
		SessionID:  sessionID,
		Type:       "keygen",
		WalletID:   walletID,
		Threshold:  threshold,
		TotalParts: totalParts,
		PartyIDs:   partyIDs,
		Initiator:  c.localNodeID,
		Timestamp:  time.Now().UnixNano(),
	}
	
	// 创建待确认会话
	pending := &PendingSession{
		Invite:     invite,
		Accepted:   make(map[string]bool),
		AcceptChan: make(chan string, len(partyIDs)),
		StartTime:  time.Now(),
	}
	pending.Accepted[c.localNodeID] = true // 自己已接受
	
	c.pendingMu.Lock()
	c.pendingSessions[sessionID] = pending
	c.pendingMu.Unlock()
	
	// 广播邀请给其他节点
	if err := c.broadcastInvite(invite, MsgTypeKeygenInvite); err != nil {
		return fmt.Errorf("failed to broadcast keygen invite: %w", err)
	}
	
	// 等待所有节点接受
	if err := c.waitForAcceptance(ctx, sessionID, partyIDs); err != nil {
		return err
	}
	
	c.log.WithField("session_id", sessionID).Info("All nodes accepted keygen invite")
	return nil
}

// InitiateSigning 发起签名并等待所有节点就绪
func (c *Coordinator) InitiateSigning(ctx context.Context, sessionID, walletID, message string, partyIDs []string, threshold int) error {
	c.log.WithFields(logrus.Fields{
		"session_id": sessionID,
		"wallet_id":  walletID,
		"party_ids":  partyIDs,
	}).Info("Initiating signing session")
	
	invite := &SessionInvite{
		SessionID:  sessionID,
		Type:       "sign",
		WalletID:   walletID,
		Threshold:  threshold,
		PartyIDs:   partyIDs,
		Message:    message,
		Initiator:  c.localNodeID,
		Timestamp:  time.Now().UnixNano(),
	}
	
	pending := &PendingSession{
		Invite:     invite,
		Accepted:   make(map[string]bool),
		AcceptChan: make(chan string, len(partyIDs)),
		StartTime:  time.Now(),
	}
	pending.Accepted[c.localNodeID] = true
	
	c.pendingMu.Lock()
	c.pendingSessions[sessionID] = pending
	c.pendingMu.Unlock()
	
	if err := c.broadcastInvite(invite, MsgTypeSignInvite); err != nil {
		return fmt.Errorf("failed to broadcast sign invite: %w", err)
	}
	
	if err := c.waitForAcceptance(ctx, sessionID, partyIDs); err != nil {
		return err
	}
	
	c.log.WithField("session_id", sessionID).Info("All nodes accepted sign invite")
	return nil
}

// broadcastInvite 广播邀请
func (c *Coordinator) broadcastInvite(invite *SessionInvite, msgType string) error {
	data, err := json.Marshal(invite)
	if err != nil {
		return err
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

// waitForAcceptance 等待所有节点接受
func (c *Coordinator) waitForAcceptance(ctx context.Context, sessionID string, partyIDs []string) error {
	c.pendingMu.RLock()
	pending, ok := c.pendingSessions[sessionID]
	c.pendingMu.RUnlock()
	
	if !ok {
		return fmt.Errorf("session not found: %s", sessionID)
	}
	
	timeout := time.After(30 * time.Second)
	needed := len(partyIDs)
	
	for {
		c.pendingMu.RLock()
		accepted := len(pending.Accepted)
		c.pendingMu.RUnlock()
		
		if accepted >= needed {
			return nil
		}
		
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timeout:
			return fmt.Errorf("timeout waiting for nodes to accept (got %d/%d)", accepted, needed)
		case nodeID := <-pending.AcceptChan:
			c.log.WithFields(logrus.Fields{
				"session_id": sessionID,
				"node_id":    nodeID,
				"accepted":   accepted + 1,
				"needed":     needed,
			}).Debug("Node accepted invite")
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
		"initiator":  invite.Initiator,
	}).Info("Received keygen invite")
	
	// 检查自己是否在参与者列表中
	isParticipant := false
	for _, pid := range invite.PartyIDs {
		if pid == c.localNodeID {
			isParticipant = true
			break
		}
	}
	
	if !isParticipant {
		c.log.Debug("Not a participant in this keygen session")
		return nil
	}
	
	// 发送接受确认
	accept := &SessionAccept{
		SessionID: invite.SessionID,
		NodeID:    c.localNodeID,
		Accepted:  true,
	}
	
	if err := c.sendAccept(accept, MsgTypeKeygenAccept); err != nil {
		c.log.WithError(err).Error("Failed to send keygen accept")
		return err
	}
	
	// 加入keygen会话
	go func() {
		// 等待一小段时间确保所有节点都准备好
		time.Sleep(500 * time.Millisecond)
		
		req := &types.KeygenRequest{
			WalletID:   invite.WalletID,
			Threshold:  invite.Threshold,
			TotalParts: invite.TotalParts,
			PartyIDs:   invite.PartyIDs,
		}
		
		_, err := c.keygenMgr.JoinKeygen(c.ctx, invite.SessionID, req)
		if err != nil {
			c.log.WithError(err).Error("Failed to join keygen session")
		}
	}()
	
	return nil
}

// handleKeygenAccept 处理keygen接受确认
func (c *Coordinator) handleKeygenAccept(msg *types.P2PMessage) error {
	var accept SessionAccept
	if err := json.Unmarshal(msg.Data, &accept); err != nil {
		return err
	}
	
	c.log.WithFields(logrus.Fields{
		"session_id": accept.SessionID,
		"node_id":    accept.NodeID,
		"accepted":   accept.Accepted,
	}).Debug("Received keygen accept")
	
	c.pendingMu.Lock()
	if pending, ok := c.pendingSessions[accept.SessionID]; ok {
		if accept.Accepted {
			pending.Accepted[accept.NodeID] = true
			select {
			case pending.AcceptChan <- accept.NodeID:
			default:
			}
		}
	}
	c.pendingMu.Unlock()
	
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
		"initiator":  invite.Initiator,
	}).Info("Received sign invite")
	
	isParticipant := false
	for _, pid := range invite.PartyIDs {
		if pid == c.localNodeID {
			isParticipant = true
			break
		}
	}
	
	if !isParticipant {
		return nil
	}
	
	accept := &SessionAccept{
		SessionID: invite.SessionID,
		NodeID:    c.localNodeID,
		Accepted:  true,
	}
	
	if err := c.sendAccept(accept, MsgTypeSignAccept); err != nil {
		return err
	}
	
	go func() {
		time.Sleep(500 * time.Millisecond)
		
		req := &types.SignRequest{
			WalletID:  invite.WalletID,
			Message:   invite.Message,
			PartyIDs:  invite.PartyIDs,
			RequestID: invite.SessionID,
		}
		
		_, err := c.signingMgr.JoinSigning(c.ctx, invite.SessionID, req)
		if err != nil {
			c.log.WithError(err).Error("Failed to join signing session")
		}
	}()
	
	return nil
}

// handleSignAccept 处理签名接受确认
func (c *Coordinator) handleSignAccept(msg *types.P2PMessage) error {
	var accept SessionAccept
	if err := json.Unmarshal(msg.Data, &accept); err != nil {
		return err
	}
	
	c.pendingMu.Lock()
	if pending, ok := c.pendingSessions[accept.SessionID]; ok {
		if accept.Accepted {
			pending.Accepted[accept.NodeID] = true
			select {
			case pending.AcceptChan <- accept.NodeID:
			default:
			}
		}
	}
	c.pendingMu.Unlock()
	
	return nil
}

// handleReshareInvite 处理reshare邀请
func (c *Coordinator) handleReshareInvite(msg *types.P2PMessage) error {
	var invite SessionInvite
	if err := json.Unmarshal(msg.Data, &invite); err != nil {
		return err
	}
	
	c.log.WithFields(logrus.Fields{
		"session_id": invite.SessionID,
		"wallet_id":  invite.WalletID,
	}).Info("Received reshare invite")
	
	// 检查是否是旧成员或新成员
	isOldMember := contains(invite.OldPartyIDs, c.localNodeID)
	isNewMember := contains(invite.NewPartyIDs, c.localNodeID)
	
	if !isOldMember && !isNewMember {
		return nil
	}
	
	accept := &SessionAccept{
		SessionID: invite.SessionID,
		NodeID:    c.localNodeID,
		Accepted:  true,
	}
	
	if err := c.sendAccept(accept, MsgTypeReshareAccept); err != nil {
		return err
	}
	
	go func() {
		time.Sleep(500 * time.Millisecond)
		
		req := &ResharingRequest{
			WalletID:     invite.WalletID,
			OldThreshold: invite.OldThreshold,
			OldPartyIDs:  invite.OldPartyIDs,
			NewThreshold: invite.NewThreshold,
			NewPartyIDs:  invite.NewPartyIDs,
		}
		
		_, err := c.resharingMgr.JoinResharing(c.ctx, invite.SessionID, req)
		if err != nil {
			c.log.WithError(err).Error("Failed to join resharing session")
		}
	}()
	
	return nil
}

// handleReshareAccept 处理reshare接受确认
func (c *Coordinator) handleReshareAccept(msg *types.P2PMessage) error {
	var accept SessionAccept
	if err := json.Unmarshal(msg.Data, &accept); err != nil {
		return err
	}
	
	c.pendingMu.Lock()
	if pending, ok := c.pendingSessions[accept.SessionID]; ok {
		if accept.Accepted {
			pending.Accepted[accept.NodeID] = true
			select {
			case pending.AcceptChan <- accept.NodeID:
			default:
			}
		}
	}
	c.pendingMu.Unlock()
	
	return nil
}

// sendAccept 发送接受确认
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

// CleanupSession 清理会话
func (c *Coordinator) CleanupSession(sessionID string) {
	c.pendingMu.Lock()
	delete(c.pendingSessions, sessionID)
	c.pendingMu.Unlock()
}
