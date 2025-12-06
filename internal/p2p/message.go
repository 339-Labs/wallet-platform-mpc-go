package p2p

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/339-Labs/wallet-platform-mpc-go/pkg/types"
)

// TSS消息主题
const (
	TopicTSSKeygen    = "tss/keygen"
	TopicTSSSign      = "tss/sign"
	TopicTSSResharing = "tss/resharing"
)

// 消息类型常量
const (
	MsgTypeKeygen      = "keygen"
	MsgTypeKeygenRound = "keygen_round"
	MsgTypeSign        = "sign"
	MsgTypeSignRound   = "sign_round"
	MsgTypeNodeInfo    = "node_info"
	MsgTypePing        = "ping"
	MsgTypePong        = "pong"
)

// MessageManager 消息管理器
type MessageManager struct {
	host   *P2PHost
	ctx    context.Context
	cancel context.CancelFunc

	// 会话消息通道
	sessions  map[string]*SessionMessageHandler
	sessionMu sync.RWMutex

	log *logrus.Entry
}

// SessionMessageHandler 会话消息处理器
type SessionMessageHandler struct {
	SessionID string
	Type      string
	MsgChan   chan *types.P2PMessage
	Done      chan struct{}
}

// NewMessageManager 创建消息管理器
func NewMessageManager(host *P2PHost) *MessageManager {
	ctx, cancel := context.WithCancel(context.Background())

	mm := &MessageManager{
		host:     host,
		ctx:      ctx,
		cancel:   cancel,
		sessions: make(map[string]*SessionMessageHandler),
		log:      logrus.WithField("component", "message_manager"),
	}

	// 注册消息处理器
	host.RegisterHandler(MsgTypeKeygen, mm.handleKeygenMessage)
	host.RegisterHandler(MsgTypeKeygenRound, mm.handleKeygenMessage)
	host.RegisterHandler(MsgTypeSign, mm.handleSignMessage)
	host.RegisterHandler(MsgTypeSignRound, mm.handleSignMessage)
	host.RegisterHandler(MsgTypeNodeInfo, mm.handleNodeInfoMessage)
	host.RegisterHandler(MsgTypePing, mm.handlePingMessage)

	return mm
}

// Start 启动消息管理器
func (mm *MessageManager) Start() error {
	go mm.messageLoop()
	mm.log.Info("Message manager started")
	return nil
}

// Stop 停止消息管理器
func (mm *MessageManager) Stop() error {
	mm.cancel()

	mm.sessionMu.Lock()
	for _, handler := range mm.sessions {
		close(handler.Done)
	}
	mm.sessionMu.Unlock()

	return nil
}

// CreateSession 创建会话消息处理器
func (mm *MessageManager) CreateSession(sessionID, sessionType string) *SessionMessageHandler {
	mm.sessionMu.Lock()
	defer mm.sessionMu.Unlock()

	handler := &SessionMessageHandler{
		SessionID: sessionID,
		Type:      sessionType,
		MsgChan:   make(chan *types.P2PMessage, 100),
		Done:      make(chan struct{}),
	}

	mm.sessions[sessionID] = handler
	mm.log.WithField("session_id", sessionID).Info("Session created")

	return handler
}

// CloseSession 关闭会话
func (mm *MessageManager) CloseSession(sessionID string) {
	mm.sessionMu.Lock()
	defer mm.sessionMu.Unlock()

	if handler, ok := mm.sessions[sessionID]; ok {
		close(handler.Done)
		delete(mm.sessions, sessionID)
		mm.log.WithField("session_id", sessionID).Info("Session closed")
	}
}

// GetSession 获取会话处理器
func (mm *MessageManager) GetSession(sessionID string) (*SessionMessageHandler, bool) {
	mm.sessionMu.RLock()
	defer mm.sessionMu.RUnlock()

	handler, ok := mm.sessions[sessionID]
	return handler, ok
}

// SendToSession 发送消息到会话
func (mm *MessageManager) SendToSession(sessionID string, msg *types.P2PMessage) bool {
	mm.sessionMu.RLock()
	handler, ok := mm.sessions[sessionID]
	mm.sessionMu.RUnlock()

	if !ok {
		return false
	}

	select {
	case handler.MsgChan <- msg:
		return true
	case <-handler.Done:
		return false
	default:
		mm.log.WithField("session_id", sessionID).Warn("Session message channel full")
		return false
	}
}

// BroadcastToParties 广播消息给指定的参与方
func (mm *MessageManager) BroadcastToParties(sessionID string, partyIDs []string, data []byte, round int) error {
	return mm.BroadcastToPartiesWithType(sessionID, partyIDs, data, round, MsgTypeKeygenRound)
}

// BroadcastToPartiesWithType 广播消息给指定的参与方（指定消息类型）
func (mm *MessageManager) BroadcastToPartiesWithType(sessionID string, partyIDs []string, data []byte, round int, msgType string) error {
	msg := &types.P2PMessage{
		Type:      msgType,
		SessionID: sessionID,
		From:      mm.host.GetNodeID(),
		Round:     round,
		Data:      data,
		Timestamp: time.Now().UnixNano(),
	}

	return mm.host.BroadcastMessage(msg)
}

// SendToParty 发送消息给指定的参与方
func (mm *MessageManager) SendToParty(sessionID, targetPartyID string, data []byte, round int) error {
	return mm.SendToPartyWithType(sessionID, targetPartyID, data, round, MsgTypeKeygenRound)
}

// SendToPartyWithType 发送消息给指定的参与方（指定消息类型）
func (mm *MessageManager) SendToPartyWithType(sessionID, targetPartyID string, data []byte, round int, msgType string) error {
	msg := &types.P2PMessage{
		Type:      msgType,
		SessionID: sessionID,
		From:      mm.host.GetNodeID(),
		To:        targetPartyID,
		Round:     round,
		Data:      data,
		Timestamp: time.Now().UnixNano(),
	}

	return mm.host.BroadcastMessage(msg)
}

// messageLoop 消息循环
func (mm *MessageManager) messageLoop() {
	for {
		select {
		case <-mm.ctx.Done():
			return
		}
	}
}

// handleKeygenMessage 处理密钥生成消息
func (mm *MessageManager) handleKeygenMessage(msg *types.P2PMessage) error {
	return mm.routeToSession(msg)
}

// handleSignMessage 处理签名消息
func (mm *MessageManager) handleSignMessage(msg *types.P2PMessage) error {
	return mm.routeToSession(msg)
}

// handleNodeInfoMessage 处理节点信息消息
func (mm *MessageManager) handleNodeInfoMessage(msg *types.P2PMessage) error {
	var nodeInfo types.NodeInfo
	if err := json.Unmarshal(msg.Data, &nodeInfo); err != nil {
		return err
	}

	mm.log.WithFields(logrus.Fields{
		"node_id": nodeInfo.ID,
		"address": nodeInfo.Address,
	}).Debug("Received node info")

	return nil
}

// handlePingMessage 处理ping消息
func (mm *MessageManager) handlePingMessage(msg *types.P2PMessage) error {
	// 响应pong
	pongMsg := &types.P2PMessage{
		Type:      MsgTypePong,
		SessionID: msg.SessionID,
		From:      mm.host.GetNodeID(),
		To:        msg.From,
		Timestamp: time.Now().UnixNano(),
	}

	return mm.host.BroadcastMessage(pongMsg)
}

// routeToSession 路由消息到会话
func (mm *MessageManager) routeToSession(msg *types.P2PMessage) error {
	if msg.SessionID == "" {
		mm.log.Warn("Received message without session ID")
		return nil
	}

	if !mm.SendToSession(msg.SessionID, msg) {
		mm.log.WithField("session_id", msg.SessionID).Debug("Session not found or closed")
	}

	return nil
}

// WaitForMessages 等待会话消息
func (h *SessionMessageHandler) WaitForMessages(timeout time.Duration) []*types.P2PMessage {
	var messages []*types.P2PMessage
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	for {
		select {
		case msg := <-h.MsgChan:
			messages = append(messages, msg)
		case <-timer.C:
			return messages
		case <-h.Done:
			return messages
		}
	}
}

// ReceiveMessage 接收单个消息
func (h *SessionMessageHandler) ReceiveMessage(timeout time.Duration) (*types.P2PMessage, bool) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case msg := <-h.MsgChan:
		return msg, true
	case <-timer.C:
		return nil, false
	case <-h.Done:
		return nil, false
	}
}
