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

// SessionCoordinator 会话协调器 - 确保所有节点在开始TSS协议前准备就绪
type SessionCoordinator struct {
	p2pHost     *p2p.P2PHost
	msgManager  *p2p.MessageManager
	localNodeID string
	
	// 待处理的会话请求
	pendingSessions map[string]*PendingSession
	sessionMu       sync.RWMutex
	
	// 就绪通知
	readyChan       map[string]chan string // sessionID -> ready nodeIDs
	readyMu         sync.RWMutex
	
	log *logrus.Entry
}

// PendingSession 待处理会话
type PendingSession struct {
	ID         string
	Type       string // keygen, sign, resharing
	WalletID   string
	PartyIDs   []string
	Threshold  int
	TotalParts int
	ReadyNodes map[string]bool
	CreatedAt  time.Time
	Data       []byte // 额外数据（如签名消息）
}

// SessionReadyMessage 会话就绪消息
type SessionReadyMessage struct {
	SessionID string   `json:"session_id"`
	Type      string   `json:"type"`
	NodeID    string   `json:"node_id"`
	WalletID  string   `json:"wallet_id"`
	PartyIDs  []string `json:"party_ids"`
	Threshold int      `json:"threshold"`
	Data      []byte   `json:"data,omitempty"`
}

// SessionStartMessage 会话开始消息
type SessionStartMessage struct {
	SessionID string `json:"session_id"`
	Type      string `json:"type"`
}

const (
	MsgTypeSessionReady = "session_ready"
	MsgTypeSessionStart = "session_start"
	MsgTypeSessionAck   = "session_ack"
)

// NewSessionCoordinator 创建会话协调器
func NewSessionCoordinator(p2pHost *p2p.P2PHost, msgManager *p2p.MessageManager, localNodeID string) *SessionCoordinator {
	sc := &SessionCoordinator{
		p2pHost:         p2pHost,
		msgManager:      msgManager,
		localNodeID:     localNodeID,
		pendingSessions: make(map[string]*PendingSession),
		readyChan:       make(map[string]chan string),
		log:             logrus.WithField("component", "session_coordinator"),
	}
	
	// 注册消息处理器
	p2pHost.RegisterHandler(MsgTypeSessionReady, sc.handleSessionReady)
	p2pHost.RegisterHandler(MsgTypeSessionStart, sc.handleSessionStart)
	p2pHost.RegisterHandler(MsgTypeSessionAck, sc.handleSessionAck)
	
	return sc
}

// InitiateKeygenSession 发起keygen会话协调
func (sc *SessionCoordinator) InitiateKeygenSession(ctx context.Context, walletID string, threshold, totalParts int, partyIDs []string) (string, error) {
	sessionID := uuid.New().String()
	
	sc.log.WithFields(logrus.Fields{
		"session_id": sessionID,
		"wallet_id":  walletID,
		"party_ids":  partyIDs,
	}).Info("Initiating keygen session coordination")
	
	// 创建待处理会话
	pending := &PendingSession{
		ID:         sessionID,
		Type:       "keygen",
		WalletID:   walletID,
		PartyIDs:   partyIDs,
		Threshold:  threshold,
		TotalParts: totalParts,
		ReadyNodes: make(map[string]bool),
		CreatedAt:  time.Now(),
	}
	pending.ReadyNodes[sc.localNodeID] = true
	
	sc.sessionMu.Lock()
	sc.pendingSessions[sessionID] = pending
	sc.sessionMu.Unlock()
	
	// 创建就绪通道
	sc.readyMu.Lock()
	sc.readyChan[sessionID] = make(chan string, len(partyIDs))
	sc.readyMu.Unlock()
	
	// 订阅会话主题
	if err := sc.p2pHost.GetPubSub().SubscribeSession(sessionID); err != nil {
		return "", fmt.Errorf("failed to subscribe session topic: %w", err)
	}
	
	// 广播会话就绪消息
	readyMsg := &SessionReadyMessage{
		SessionID: sessionID,
		Type:      "keygen",
		NodeID:    sc.localNodeID,
		WalletID:  walletID,
		PartyIDs:  partyIDs,
		Threshold: threshold,
	}
	
	if err := sc.broadcastSessionReady(readyMsg); err != nil {
		return "", fmt.Errorf("failed to broadcast session ready: %w", err)
	}
	
	// 等待所有节点就绪
	if err := sc.waitForAllReady(ctx, sessionID, partyIDs); err != nil {
		return "", fmt.Errorf("failed waiting for nodes: %w", err)
	}
	
	// 广播开始消息
	if err := sc.broadcastSessionStart(sessionID, "keygen"); err != nil {
		return "", fmt.Errorf("failed to broadcast session start: %w", err)
	}
	
	sc.log.WithField("session_id", sessionID).Info("All nodes ready, session starting")
	
	return sessionID, nil
}

// InitiateSignSession 发起签名会话协调
func (sc *SessionCoordinator) InitiateSignSession(ctx context.Context, walletID string, messageHash []byte, partyIDs []string, threshold int) (string, error) {
	sessionID := uuid.New().String()
	
	sc.log.WithFields(logrus.Fields{
		"session_id": sessionID,
		"wallet_id":  walletID,
		"party_ids":  partyIDs,
	}).Info("Initiating sign session coordination")
	
	// 创建待处理会话
	pending := &PendingSession{
		ID:         sessionID,
		Type:       "sign",
		WalletID:   walletID,
		PartyIDs:   partyIDs,
		Threshold:  threshold,
		ReadyNodes: make(map[string]bool),
		CreatedAt:  time.Now(),
		Data:       messageHash,
	}
	pending.ReadyNodes[sc.localNodeID] = true
	
	sc.sessionMu.Lock()
	sc.pendingSessions[sessionID] = pending
	sc.sessionMu.Unlock()
	
	// 创建就绪通道
	sc.readyMu.Lock()
	sc.readyChan[sessionID] = make(chan string, len(partyIDs))
	sc.readyMu.Unlock()
	
	// 订阅会话主题
	if err := sc.p2pHost.GetPubSub().SubscribeSession(sessionID); err != nil {
		return "", fmt.Errorf("failed to subscribe session topic: %w", err)
	}
	
	// 广播会话就绪消息
	readyMsg := &SessionReadyMessage{
		SessionID: sessionID,
		Type:      "sign",
		NodeID:    sc.localNodeID,
		WalletID:  walletID,
		PartyIDs:  partyIDs,
		Threshold: threshold,
		Data:      messageHash,
	}
	
	if err := sc.broadcastSessionReady(readyMsg); err != nil {
		return "", fmt.Errorf("failed to broadcast session ready: %w", err)
	}
	
	// 等待所有节点就绪
	if err := sc.waitForAllReady(ctx, sessionID, partyIDs); err != nil {
		return "", fmt.Errorf("failed waiting for nodes: %w", err)
	}
	
	// 广播开始消息
	if err := sc.broadcastSessionStart(sessionID, "sign"); err != nil {
		return "", fmt.Errorf("failed to broadcast session start: %w", err)
	}
	
	return sessionID, nil
}

// broadcastSessionReady 广播会话就绪消息
func (sc *SessionCoordinator) broadcastSessionReady(msg *SessionReadyMessage) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	
	p2pMsg := &types.P2PMessage{
		Type:      MsgTypeSessionReady,
		SessionID: msg.SessionID,
		From:      sc.localNodeID,
		Data:      data,
		Timestamp: time.Now().UnixNano(),
	}
	
	return sc.p2pHost.BroadcastMessage(p2pMsg)
}

// broadcastSessionStart 广播会话开始消息
func (sc *SessionCoordinator) broadcastSessionStart(sessionID, sessionType string) error {
	msg := &SessionStartMessage{
		SessionID: sessionID,
		Type:      sessionType,
	}
	
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	
	p2pMsg := &types.P2PMessage{
		Type:      MsgTypeSessionStart,
		SessionID: sessionID,
		From:      sc.localNodeID,
		Data:      data,
		Timestamp: time.Now().UnixNano(),
	}
	
	return sc.p2pHost.BroadcastMessage(p2pMsg)
}

// waitForAllReady 等待所有节点就绪
func (sc *SessionCoordinator) waitForAllReady(ctx context.Context, sessionID string, partyIDs []string) error {
	sc.readyMu.RLock()
	readyChan := sc.readyChan[sessionID]
	sc.readyMu.RUnlock()
	
	if readyChan == nil {
		return fmt.Errorf("ready channel not found for session %s", sessionID)
	}
	
	readyCount := 1 // 自己已经就绪
	needed := len(partyIDs)
	
	timeout := time.After(60 * time.Second)
	
	for readyCount < needed {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timeout:
			return fmt.Errorf("timeout waiting for nodes, got %d/%d ready", readyCount, needed)
		case nodeID := <-readyChan:
			sc.log.WithFields(logrus.Fields{
				"session_id": sessionID,
				"node_id":    nodeID,
				"ready":      readyCount + 1,
				"needed":     needed,
			}).Debug("Node ready")
			readyCount++
		}
	}
	
	return nil
}

// handleSessionReady 处理会话就绪消息
func (sc *SessionCoordinator) handleSessionReady(msg *types.P2PMessage) error {
	var readyMsg SessionReadyMessage
	if err := json.Unmarshal(msg.Data, &readyMsg); err != nil {
		return err
	}
	
	sc.log.WithFields(logrus.Fields{
		"session_id": readyMsg.SessionID,
		"from":       readyMsg.NodeID,
		"type":       readyMsg.Type,
	}).Info("Received session ready message")
	
	// 检查是否是发给自己的（在party列表中）
	isParticipant := false
	for _, pid := range readyMsg.PartyIDs {
		if pid == sc.localNodeID {
			isParticipant = true
			break
		}
	}
	
	if !isParticipant {
		return nil // 不是参与方，忽略
	}
	
	sc.sessionMu.Lock()
	pending, exists := sc.pendingSessions[readyMsg.SessionID]
	if !exists {
		// 这是新会话，需要加入
		pending = &PendingSession{
			ID:         readyMsg.SessionID,
			Type:       readyMsg.Type,
			WalletID:   readyMsg.WalletID,
			PartyIDs:   readyMsg.PartyIDs,
			Threshold:  readyMsg.Threshold,
			ReadyNodes: make(map[string]bool),
			CreatedAt:  time.Now(),
			Data:       readyMsg.Data,
		}
		sc.pendingSessions[readyMsg.SessionID] = pending
		
		// 创建就绪通道
		sc.readyMu.Lock()
		if sc.readyChan[readyMsg.SessionID] == nil {
			sc.readyChan[readyMsg.SessionID] = make(chan string, len(readyMsg.PartyIDs))
		}
		sc.readyMu.Unlock()
		
		// 订阅会话主题
		if err := sc.p2pHost.GetPubSub().SubscribeSession(readyMsg.SessionID); err != nil {
			sc.sessionMu.Unlock()
			return err
		}
		
		// 回复自己也准备好了
		myReadyMsg := &SessionReadyMessage{
			SessionID: readyMsg.SessionID,
			Type:      readyMsg.Type,
			NodeID:    sc.localNodeID,
			WalletID:  readyMsg.WalletID,
			PartyIDs:  readyMsg.PartyIDs,
			Threshold: readyMsg.Threshold,
		}
		sc.sessionMu.Unlock()
		
		return sc.broadcastSessionReady(myReadyMsg)
	}
	sc.sessionMu.Unlock()
	
	// 标记节点就绪
	pending.ReadyNodes[readyMsg.NodeID] = true
	
	// 通知等待的协程
	sc.readyMu.RLock()
	if ch, ok := sc.readyChan[readyMsg.SessionID]; ok {
		select {
		case ch <- readyMsg.NodeID:
		default:
		}
	}
	sc.readyMu.RUnlock()
	
	return nil
}

// handleSessionStart 处理会话开始消息
func (sc *SessionCoordinator) handleSessionStart(msg *types.P2PMessage) error {
	var startMsg SessionStartMessage
	if err := json.Unmarshal(msg.Data, &startMsg); err != nil {
		return err
	}
	
	sc.log.WithFields(logrus.Fields{
		"session_id": startMsg.SessionID,
		"type":       startMsg.Type,
	}).Info("Received session start message")
	
	// 会话开始逻辑由各自的Manager处理
	return nil
}

// handleSessionAck 处理会话确认消息
func (sc *SessionCoordinator) handleSessionAck(msg *types.P2PMessage) error {
	// ACK处理
	return nil
}

// GetPendingSession 获取待处理会话
func (sc *SessionCoordinator) GetPendingSession(sessionID string) (*PendingSession, bool) {
	sc.sessionMu.RLock()
	defer sc.sessionMu.RUnlock()
	session, ok := sc.pendingSessions[sessionID]
	return session, ok
}

// CleanupSession 清理会话
func (sc *SessionCoordinator) CleanupSession(sessionID string) {
	sc.sessionMu.Lock()
	delete(sc.pendingSessions, sessionID)
	sc.sessionMu.Unlock()
	
	sc.readyMu.Lock()
	if ch, ok := sc.readyChan[sessionID]; ok {
		close(ch)
		delete(sc.readyChan, sessionID)
	}
	sc.readyMu.Unlock()
	
	// 取消订阅会话主题
	sc.p2pHost.GetPubSub().UnsubscribeSession(sessionID)
}
