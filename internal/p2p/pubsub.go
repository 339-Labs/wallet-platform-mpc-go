package p2p

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/sirupsen/logrus"

	"github.com/339-Labs/wallet-platform-mpc-go/pkg/types"
)

// Topic名称
const (
	TopicKeygen    = "mpc/keygen"
	TopicSign      = "mpc/sign"
	TopicBroadcast = "mpc/broadcast"
	TopicHeartbeat = "mpc/heartbeat"
)

// PubSubManager PubSub消息管理器
type PubSubManager struct {
	host   host.Host
	ps     *pubsub.PubSub
	ctx    context.Context
	cancel context.CancelFunc
	nodeID string

	// 订阅的主题
	topics   map[string]*pubsub.Topic
	subs     map[string]*pubsub.Subscription
	topicsMu sync.RWMutex

	// 消息处理器
	handlers  map[string][]MessageHandler
	handlerMu sync.RWMutex

	// 消息通道
	incomingChan chan *types.P2PMessage

	log *logrus.Entry
}

// PubSubConfig PubSub配置
type PubSubConfig struct {
	// GossipSub参数
	HeartbeatInterval time.Duration
	HistoryLength     int
	HistoryGossip     int

	// 验证配置
	ValidateQueueSize int
	OutboundQueueSize int
}

// DefaultPubSubConfig 默认配置
func DefaultPubSubConfig() *PubSubConfig {
	return &PubSubConfig{
		HeartbeatInterval: time.Second * 1,
		HistoryLength:     5,
		HistoryGossip:     3,
		ValidateQueueSize: 128,
		OutboundQueueSize: 128,
	}
}

// NewPubSubManager 创建PubSub管理器
func NewPubSubManager(ctx context.Context, h host.Host, nodeID string, cfg *PubSubConfig) (*PubSubManager, error) {
	if cfg == nil {
		cfg = DefaultPubSubConfig()
	}

	ctx, cancel := context.WithCancel(ctx)

	// 创建GossipSub
	ps, err := pubsub.NewGossipSub(ctx, h,
		pubsub.WithMessageSignaturePolicy(pubsub.StrictSign),
		pubsub.WithPeerOutboundQueueSize(cfg.OutboundQueueSize),
		pubsub.WithValidateQueueSize(cfg.ValidateQueueSize),
	)
	if err != nil {
		cancel()
		return nil, err
	}

	psm := &PubSubManager{
		host:         h,
		ps:           ps,
		ctx:          ctx,
		cancel:       cancel,
		nodeID:       nodeID,
		topics:       make(map[string]*pubsub.Topic),
		subs:         make(map[string]*pubsub.Subscription),
		handlers:     make(map[string][]MessageHandler),
		incomingChan: make(chan *types.P2PMessage, 1000),
		log:          logrus.WithField("component", "pubsub"),
	}

	return psm, nil
}

// Start 启动PubSub服务
func (psm *PubSubManager) Start() error {
	// 订阅默认主题
	defaultTopics := []string{TopicKeygen, TopicSign, TopicBroadcast, TopicHeartbeat}
	for _, topic := range defaultTopics {
		if err := psm.Subscribe(topic); err != nil {
			return err
		}
	}

	// 启动心跳
	go psm.heartbeatLoop()

	psm.log.Info("PubSub manager started")
	return nil
}

// Stop 停止PubSub服务
func (psm *PubSubManager) Stop() error {
	psm.cancel()

	psm.topicsMu.Lock()
	for name, sub := range psm.subs {
		sub.Cancel()
		psm.log.WithField("topic", name).Debug("Unsubscribed from topic")
	}
	for name, topic := range psm.topics {
		topic.Close()
		psm.log.WithField("topic", name).Debug("Closed topic")
	}
	psm.topicsMu.Unlock()

	close(psm.incomingChan)

	psm.log.Info("PubSub manager stopped")
	return nil
}

// Subscribe 订阅主题
func (psm *PubSubManager) Subscribe(topicName string) error {
	psm.topicsMu.Lock()
	defer psm.topicsMu.Unlock()

	// 检查是否已订阅
	if _, exists := psm.subs[topicName]; exists {
		return nil
	}

	// 加入主题
	topic, err := psm.ps.Join(topicName)
	if err != nil {
		return err
	}
	psm.topics[topicName] = topic

	// 订阅主题
	sub, err := topic.Subscribe()
	if err != nil {
		return err
	}
	psm.subs[topicName] = sub

	// 启动消息接收循环
	go psm.readLoop(topicName, sub)

	psm.log.WithField("topic", topicName).Info("Subscribed to topic")
	return nil
}

// Unsubscribe 取消订阅
func (psm *PubSubManager) Unsubscribe(topicName string) error {
	psm.topicsMu.Lock()
	defer psm.topicsMu.Unlock()

	if sub, exists := psm.subs[topicName]; exists {
		sub.Cancel()
		delete(psm.subs, topicName)
	}

	if topic, exists := psm.topics[topicName]; exists {
		topic.Close()
		delete(psm.topics, topicName)
	}

	psm.log.WithField("topic", topicName).Info("Unsubscribed from topic")
	return nil
}

// Publish 发布消息到主题
func (psm *PubSubManager) Publish(topicName string, msg *types.P2PMessage) error {
	psm.topicsMu.RLock()
	topic, exists := psm.topics[topicName]
	psm.topicsMu.RUnlock()

	if !exists {
		// 尝试订阅主题
		if err := psm.Subscribe(topicName); err != nil {
			return err
		}
		psm.topicsMu.RLock()
		topic = psm.topics[topicName]
		psm.topicsMu.RUnlock()
	}

	// 设置发送者信息
	msg.From = psm.nodeID
	msg.Timestamp = time.Now().UnixNano()

	// 序列化消息
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}

	return topic.Publish(psm.ctx, data)
}

// PublishToSession 发布消息到会话（使用会话特定的主题）
func (psm *PubSubManager) PublishToSession(sessionID string, msg *types.P2PMessage) error {
	topicName := "mpc/session/" + sessionID
	return psm.Publish(topicName, msg)
}

// SubscribeSession 订阅会话主题
func (psm *PubSubManager) SubscribeSession(sessionID string) error {
	topicName := "mpc/session/" + sessionID
	return psm.Subscribe(topicName)
}

// UnsubscribeSession 取消订阅会话主题
func (psm *PubSubManager) UnsubscribeSession(sessionID string) error {
	topicName := "mpc/session/" + sessionID
	return psm.Unsubscribe(topicName)
}

// RegisterHandler 注册消息处理器
func (psm *PubSubManager) RegisterHandler(msgType string, handler MessageHandler) {
	psm.handlerMu.Lock()
	defer psm.handlerMu.Unlock()
	psm.handlers[msgType] = append(psm.handlers[msgType], handler)
}

// readLoop 消息读取循环
func (psm *PubSubManager) readLoop(topicName string, sub *pubsub.Subscription) {
	for {
		msg, err := sub.Next(psm.ctx)
		if err != nil {
			if psm.ctx.Err() != nil {
				return // 上下文已取消
			}
			psm.log.WithError(err).WithField("topic", topicName).Warn("Error reading from subscription")
			continue
		}

		// 忽略自己发送的消息
		if msg.ReceivedFrom == psm.host.ID() {
			continue
		}

		// 解析消息
		var p2pMsg types.P2PMessage
		if err := json.Unmarshal(msg.Data, &p2pMsg); err != nil {
			psm.log.WithError(err).Warn("Failed to unmarshal message")
			continue
		}

		// 检查消息是否是发给自己的
		if p2pMsg.To != "" && p2pMsg.To != psm.nodeID {
			continue
		}

		psm.log.WithFields(logrus.Fields{
			"topic": topicName,
			"from":  p2pMsg.From,
			"type":  p2pMsg.Type,
		}).Debug("Received message")

		// 调用处理器
		psm.handleMessage(&p2pMsg)

		// 发送到通道
		select {
		case psm.incomingChan <- &p2pMsg:
		case <-psm.ctx.Done():
			return
		default:
			psm.log.Warn("Incoming message channel full, dropping message")
		}
	}
}

// handleMessage 处理消息
func (psm *PubSubManager) handleMessage(msg *types.P2PMessage) {
	psm.handlerMu.RLock()
	handlers := psm.handlers[msg.Type]
	psm.handlerMu.RUnlock()

	for _, handler := range handlers {
		if err := handler(msg); err != nil {
			psm.log.WithError(err).WithField("type", msg.Type).Warn("Handler error")
		}
	}
}

// heartbeatLoop 心跳循环
func (psm *PubSubManager) heartbeatLoop() {
	ticker := time.NewTicker(time.Second * 30)
	defer ticker.Stop()

	for {
		select {
		case <-psm.ctx.Done():
			return
		case <-ticker.C:
			msg := &types.P2PMessage{
				Type:      MsgTypePing,
				From:      psm.nodeID,
				Timestamp: time.Now().UnixNano(),
			}
			if err := psm.Publish(TopicHeartbeat, msg); err != nil {
				psm.log.WithError(err).Debug("Failed to send heartbeat")
			}
		}
	}
}

// IncomingChan 获取传入消息通道
func (psm *PubSubManager) IncomingChan() <-chan *types.P2PMessage {
	return psm.incomingChan
}

// GetTopicPeers 获取主题的订阅者
func (psm *PubSubManager) GetTopicPeers(topicName string) []peer.ID {
	psm.topicsMu.RLock()
	topic, exists := psm.topics[topicName]
	psm.topicsMu.RUnlock()

	if !exists {
		return nil
	}

	return topic.ListPeers()
}

// GetAllPeers 获取所有已知的PubSub节点
func (psm *PubSubManager) GetAllPeers() []peer.ID {
	return psm.ps.ListPeers(TopicBroadcast)
}
