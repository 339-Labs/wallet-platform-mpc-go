package p2p

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"
	"github.com/libp2p/go-libp2p/p2p/security/noise"
	libp2ptls "github.com/libp2p/go-libp2p/p2p/security/tls"
	"github.com/multiformats/go-multiaddr"
	"github.com/sirupsen/logrus"

	"github.com/339-Labs/wallet-platform-mpc-go/internal/config"
	"github.com/339-Labs/wallet-platform-mpc-go/pkg/types"
)

const (
	// 协议ID
	ProtocolTSS    = "/mpc-wallet/tss/1.0.0"
	ProtocolKeygen = "/mpc-wallet/keygen/1.0.0"
	ProtocolSign   = "/mpc-wallet/sign/1.0.0"
)

// P2PHost P2P网络主机 - 核心组件，整合Discovery、PubSub和Node管理
type P2PHost struct {
	host   host.Host
	ctx    context.Context
	cancel context.CancelFunc
	cfg    *config.P2PConfig
	nodeID string

	// 子组件
	discovery   *Discovery     // 节点发现
	pubsub      *PubSubManager // 发布订阅
	nodeManager *NodeManager   // 节点管理

	// 消息处理
	msgHandlers map[string]MessageHandler
	msgChan     chan *types.P2PMessage

	// 连接的对等节点 (保留用于快速查找)
	peers   map[peer.ID]*PeerInfo
	peersMu sync.RWMutex

	log *logrus.Entry
}

// PeerInfo 对等节点信息
type PeerInfo struct {
	ID       peer.ID
	NodeID   string
	Addrs    []multiaddr.Multiaddr
	Status   string
	LastSeen time.Time
}

// MessageHandler 消息处理函数
type MessageHandler func(msg *types.P2PMessage) error

// P2PHostConfig P2P主机配置
type P2PHostConfig struct {
	*config.P2PConfig
	NodeID     string
	KeyFile    string
	EnableDHT  bool
	EnableMDNS bool
}

// NewP2PHost 创建P2P主机
func NewP2PHost(ctx context.Context, cfg *config.P2PConfig, nodeID string, keyFile string) (*P2PHost, error) {
	log := logrus.WithField("component", "p2p")

	// 加载或生成密钥
	priv, err := loadOrGenerateKey(keyFile)
	if err != nil {
		return nil, fmt.Errorf("failed to load/generate key: %w", err)
	}

	// 创建监听地址
	listenAddr, err := multiaddr.NewMultiaddr(fmt.Sprintf("/ip4/%s/tcp/%d", cfg.ListenAddr, cfg.Port))
	if err != nil {
		return nil, fmt.Errorf("failed to create multiaddr: %w", err)
	}

	// 创建libp2p主机
	h, err := libp2p.New(
		libp2p.Identity(priv),
		libp2p.ListenAddrs(listenAddr),
		libp2p.Security(libp2ptls.ID, libp2ptls.New),
		libp2p.Security(noise.ID, noise.New),
		libp2p.EnableRelay(),
		libp2p.NATPortMap(),
		libp2p.EnableHolePunching(),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create libp2p host: %w", err)
	}

	ctx, cancel := context.WithCancel(ctx)

	p2pHost := &P2PHost{
		host:        h,
		ctx:         ctx,
		cancel:      cancel,
		cfg:         cfg,
		nodeID:      nodeID,
		msgHandlers: make(map[string]MessageHandler),
		msgChan:     make(chan *types.P2PMessage, 1000),
		peers:       make(map[peer.ID]*PeerInfo),
		log:         log,
	}

	// 设置流处理器
	h.SetStreamHandler(protocol.ID(ProtocolTSS), p2pHost.handleTSSStream)
	h.SetStreamHandler(protocol.ID(ProtocolKeygen), p2pHost.handleKeygenStream)
	h.SetStreamHandler(protocol.ID(ProtocolSign), p2pHost.handleSignStream)

	// 连接通知处理
	h.Network().Notify(&network.NotifyBundle{
		ConnectedF: func(n network.Network, conn network.Conn) {
			p2pHost.onPeerConnected(conn.RemotePeer())
		},
		DisconnectedF: func(n network.Network, conn network.Conn) {
			p2pHost.onPeerDisconnected(conn.RemotePeer())
		},
	})

	// 创建节点管理器
	p2pHost.nodeManager = NewNodeManager(ctx, h, nodeID)

	// 创建PubSub管理器
	pubsubMgr, err := NewPubSubManager(ctx, h, nodeID, DefaultPubSubConfig())
	if err != nil {
		h.Close()
		cancel()
		return nil, fmt.Errorf("failed to create pubsub manager: %w", err)
	}
	p2pHost.pubsub = pubsubMgr

	// 解析引导节点
	var bootstrapPeers []peer.AddrInfo
	for _, addrStr := range cfg.BootstrapPeers {
		addr, err := multiaddr.NewMultiaddr(addrStr)
		if err != nil {
			log.WithError(err).WithField("addr", addrStr).Warn("Invalid bootstrap peer address")
			continue
		}
		peerInfo, err := peer.AddrInfoFromP2pAddr(addr)
		if err != nil {
			log.WithError(err).WithField("addr", addrStr).Warn("Failed to parse bootstrap peer")
			continue
		}
		bootstrapPeers = append(bootstrapPeers, *peerInfo)
	}

	// 创建发现服务
	discoveryCfg := &DiscoveryConfig{
		EnableDHT:      true,
		EnableMDNS:     true,
		BootstrapPeers: bootstrapPeers,
		Namespace:      DiscoveryNamespace,
	}
	discovery, err := NewDiscovery(ctx, h, discoveryCfg)
	if err != nil {
		log.WithError(err).Warn("Failed to create discovery service, continuing without it")
	} else {
		p2pHost.discovery = discovery
	}

	log.WithFields(logrus.Fields{
		"peer_id": h.ID().String(),
		"addrs":   h.Addrs(),
		"node_id": nodeID,
	}).Info("P2P host created")

	return p2pHost, nil
}

// Start 启动P2P服务
func (p *P2PHost) Start() error {
	// 连接到引导节点
	for _, addr := range p.cfg.BootstrapPeers {
		if err := p.ConnectToPeer(addr); err != nil {
			p.log.WithError(err).WithField("addr", addr).Warn("Failed to connect to bootstrap peer")
		}
	}

	// 启动节点管理器
	if err := p.nodeManager.Start(); err != nil {
		return fmt.Errorf("failed to start node manager: %w", err)
	}

	// 启动PubSub
	if err := p.pubsub.Start(); err != nil {
		return fmt.Errorf("failed to start pubsub: %w", err)
	}

	// 启动发现服务
	if p.discovery != nil {
		if err := p.discovery.Start(); err != nil {
			p.log.WithError(err).Warn("Failed to start discovery service")
		}
	}

	// 启动消息处理
	go p.handleMessages()
	go p.handlePubSubMessages()

	// 启动发现的节点处理
	if p.discovery != nil {
		go p.handleDiscoveredPeers()
	}

	p.log.Info("P2P host started")
	return nil
}

// Stop 停止P2P服务
func (p *P2PHost) Stop() error {
	p.cancel()

	// 停止发现服务
	if p.discovery != nil {
		p.discovery.Stop()
	}

	// 停止PubSub
	if p.pubsub != nil {
		p.pubsub.Stop()
	}

	// 停止节点管理器
	if p.nodeManager != nil {
		p.nodeManager.Stop()
	}

	close(p.msgChan)
	return p.host.Close()
}

// ConnectToPeer 连接到对等节点
func (p *P2PHost) ConnectToPeer(addrStr string) error {
	addr, err := multiaddr.NewMultiaddr(addrStr)
	if err != nil {
		return fmt.Errorf("invalid multiaddr: %w", err)
	}

	peerInfo, err := peer.AddrInfoFromP2pAddr(addr)
	if err != nil {
		return fmt.Errorf("failed to get peer info: %w", err)
	}

	if err := p.host.Connect(p.ctx, *peerInfo); err != nil {
		return fmt.Errorf("failed to connect: %w", err)
	}

	p.log.WithField("peer", peerInfo.ID.String()).Info("Connected to peer")
	return nil
}

// SendMessage 发送点对点消息
func (p *P2PHost) SendMessage(peerID peer.ID, protocolID string, msg *types.P2PMessage) error {
	stream, err := p.host.NewStream(p.ctx, peerID, protocol.ID(protocolID))
	if err != nil {
		return fmt.Errorf("failed to create stream: %w", err)
	}
	defer stream.Close()

	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("failed to marshal message: %w", err)
	}

	// 写入消息长度和数据
	length := uint32(len(data))
	if err := writeUint32(stream, length); err != nil {
		return fmt.Errorf("failed to write length: %w", err)
	}
	if _, err := stream.Write(data); err != nil {
		return fmt.Errorf("failed to write data: %w", err)
	}

	return nil
}

// BroadcastMessage 广播消息
func (p *P2PHost) BroadcastMessage(msg *types.P2PMessage) error {
	return p.pubsub.Publish(TopicBroadcast, msg)
}

// PublishToTopic 发布消息到指定主题
func (p *P2PHost) PublishToTopic(topic string, msg *types.P2PMessage) error {
	return p.pubsub.Publish(topic, msg)
}

// RegisterHandler 注册消息处理器
// 只注册到msgHandlers，消息会通过handlePubSubMessages统一路由
// 不再同时注册到PubSub，避免消息被处理两次
func (p *P2PHost) RegisterHandler(msgType string, handler MessageHandler) {
	p.msgHandlers[msgType] = handler
}

// GetPeerID 获取本节点的Peer ID
func (p *P2PHost) GetPeerID() peer.ID {
	return p.host.ID()
}

// GetNodeID 获取节点ID
func (p *P2PHost) GetNodeID() string {
	return p.nodeID
}

// GetAddrs 获取监听地址
func (p *P2PHost) GetAddrs() []multiaddr.Multiaddr {
	return p.host.Addrs()
}

// GetFullAddrs 获取完整的P2P地址
func (p *P2PHost) GetFullAddrs() []string {
	hostAddr, _ := multiaddr.NewMultiaddr(fmt.Sprintf("/p2p/%s", p.host.ID().String()))
	var addrs []string
	for _, addr := range p.host.Addrs() {
		addrs = append(addrs, addr.Encapsulate(hostAddr).String())
	}
	return addrs
}

// GetConnectedPeers 获取已连接的节点
func (p *P2PHost) GetConnectedPeers() []*PeerInfo {
	p.peersMu.RLock()
	defer p.peersMu.RUnlock()

	var peers []*PeerInfo
	for _, info := range p.peers {
		peers = append(peers, info)
	}
	return peers
}

// GetPeerByNodeID 通过NodeID获取peer
func (p *P2PHost) GetPeerByNodeID(nodeID string) (peer.ID, bool) {
	p.peersMu.RLock()
	defer p.peersMu.RUnlock()

	for peerID, info := range p.peers {
		if info.NodeID == nodeID {
			return peerID, true
		}
	}
	return "", false
}

// MessageChan 获取消息通道
func (p *P2PHost) MessageChan() <-chan *types.P2PMessage {
	return p.msgChan
}

// GetHost 获取底层libp2p主机
func (p *P2PHost) GetHost() host.Host {
	return p.host
}

// GetDiscovery 获取发现服务
func (p *P2PHost) GetDiscovery() *Discovery {
	return p.discovery
}

// GetPubSub 获取PubSub管理器
func (p *P2PHost) GetPubSub() *PubSubManager {
	return p.pubsub
}

// GetNodeManager 获取节点管理器
func (p *P2PHost) GetNodeManager() *NodeManager {
	return p.nodeManager
}

// handleTSSStream 处理TSS协议流
func (p *P2PHost) handleTSSStream(stream network.Stream) {
	defer stream.Close()
	p.handleStream(stream, "tss")
}

// handleKeygenStream 处理密钥生成协议流
func (p *P2PHost) handleKeygenStream(stream network.Stream) {
	defer stream.Close()
	p.handleStream(stream, "keygen")
}

// handleSignStream 处理签名协议流
func (p *P2PHost) handleSignStream(stream network.Stream) {
	defer stream.Close()
	p.handleStream(stream, "sign")
}

// handleStream 通用流处理
func (p *P2PHost) handleStream(stream network.Stream, protocolType string) {
	// 读取消息长度
	length, err := readUint32(stream)
	if err != nil {
		p.log.WithError(err).Error("Failed to read message length")
		return
	}

	// 读取消息数据
	data := make([]byte, length)
	if _, err := io.ReadFull(stream, data); err != nil {
		p.log.WithError(err).Error("Failed to read message data")
		return
	}

	// 解析消息
	var msg types.P2PMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		p.log.WithError(err).Error("Failed to unmarshal message")
		return
	}

	// 发送到消息通道
	select {
	case p.msgChan <- &msg:
	case <-p.ctx.Done():
	}
}

// handleMessages 处理消息
func (p *P2PHost) handleMessages() {
	for msg := range p.msgChan {
		if handler, ok := p.msgHandlers[msg.Type]; ok {
			if err := handler(msg); err != nil {
				p.log.WithError(err).WithField("type", msg.Type).Error("Failed to handle message")
			}
		}
	}
}

// handlePubSubMessages 处理PubSub消息
func (p *P2PHost) handlePubSubMessages() {
	if p.pubsub == nil {
		return
	}

	for {
		select {
		case <-p.ctx.Done():
			return
		case msg := <-p.pubsub.IncomingChan():
			if msg == nil {
				continue
			}
			select {
			case p.msgChan <- msg:
			case <-p.ctx.Done():
				return
			}
		}
	}
}

// handleDiscoveredPeers 处理发现的节点
func (p *P2PHost) handleDiscoveredPeers() {
	if p.discovery == nil {
		return
	}

	for {
		select {
		case <-p.ctx.Done():
			return
		case pi := <-p.discovery.PeerChan():
			p.log.WithFields(logrus.Fields{
				"peer_id": pi.ID.String(),
				"addrs":   pi.Addrs,
			}).Debug("Processing discovered peer")
		}
	}
}

// onPeerConnected 节点连接处理
func (p *P2PHost) onPeerConnected(peerID peer.ID) {
	p.peersMu.Lock()
	defer p.peersMu.Unlock()

	p.peers[peerID] = &PeerInfo{
		ID:       peerID,
		Addrs:    p.host.Network().Peerstore().Addrs(peerID),
		Status:   "connected",
		LastSeen: time.Now(),
	}

	p.log.WithField("peer", peerID.String()).Info("Peer connected")
}

// onPeerDisconnected 节点断开处理
func (p *P2PHost) onPeerDisconnected(peerID peer.ID) {
	p.peersMu.Lock()
	defer p.peersMu.Unlock()

	delete(p.peers, peerID)

	p.log.WithField("peer", peerID.String()).Info("Peer disconnected")
}

// loadOrGenerateKey 加载或生成密钥
func loadOrGenerateKey(keyFile string) (crypto.PrivKey, error) {
	// 尝试加载已有密钥
	if _, err := os.Stat(keyFile); err == nil {
		data, err := os.ReadFile(keyFile)
		if err != nil {
			return nil, err
		}
		return crypto.UnmarshalPrivateKey(data)
	}

	// 生成新密钥
	priv, _, err := crypto.GenerateKeyPairWithReader(crypto.Ed25519, 2048, rand.Reader)
	if err != nil {
		return nil, err
	}

	// 保存密钥
	data, err := crypto.MarshalPrivateKey(priv)
	if err != nil {
		return nil, err
	}

	if err := os.WriteFile(keyFile, data, 0600); err != nil {
		return nil, err
	}

	return priv, nil
}

// writeUint32 写入uint32
func writeUint32(w io.Writer, n uint32) error {
	buf := make([]byte, 4)
	buf[0] = byte(n >> 24)
	buf[1] = byte(n >> 16)
	buf[2] = byte(n >> 8)
	buf[3] = byte(n)
	_, err := w.Write(buf)
	return err
}

// readUint32 读取uint32
func readUint32(r io.Reader) (uint32, error) {
	buf := make([]byte, 4)
	if _, err := io.ReadFull(r, buf); err != nil {
		return 0, err
	}
	return uint32(buf[0])<<24 | uint32(buf[1])<<16 | uint32(buf[2])<<8 | uint32(buf[3]), nil
}
