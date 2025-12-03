package p2p

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/multiformats/go-multiaddr"
	"github.com/sirupsen/logrus"
)

// 节点状态
const (
	NodeStatusOnline     = "online"
	NodeStatusOffline    = "offline"
	NodeStatusConnecting = "connecting"
)

// NodeManager 节点管理器
type NodeManager struct {
	host      host.Host
	ctx       context.Context
	cancel    context.CancelFunc
	localNode *NodeInfo

	// 已知节点
	nodes      map[string]*NodeInfo // nodeID -> NodeInfo
	peerToNode map[peer.ID]string   // peerID -> nodeID
	nodeMu     sync.RWMutex

	// 节点事件通知
	eventChan chan *NodeEvent

	// 心跳超时
	heartbeatTimeout time.Duration

	log *logrus.Entry
}

// NodeInfo 节点详细信息
type NodeInfo struct {
	NodeID       string                `json:"node_id"`
	PeerID       peer.ID               `json:"peer_id"`
	Addrs        []multiaddr.Multiaddr `json:"addrs"`
	Status       string                `json:"status"`
	Version      string                `json:"version"`
	Capabilities []string              `json:"capabilities"`
	LastSeen     time.Time             `json:"last_seen"`
	Latency      time.Duration         `json:"latency"`
	ConnectedAt  time.Time             `json:"connected_at"`
}

// NodeEvent 节点事件
type NodeEvent struct {
	Type   string    `json:"type"` // connected, disconnected, updated
	NodeID string    `json:"node_id"`
	PeerID peer.ID   `json:"peer_id"`
	Time   time.Time `json:"time"`
}

// NodeAnnouncement 节点公告消息
type NodeAnnouncement struct {
	NodeID       string   `json:"node_id"`
	Version      string   `json:"version"`
	Capabilities []string `json:"capabilities"`
	Timestamp    int64    `json:"timestamp"`
}

// NewNodeManager 创建节点管理器
func NewNodeManager(ctx context.Context, h host.Host, nodeID string) *NodeManager {
	ctx, cancel := context.WithCancel(ctx)

	nm := &NodeManager{
		host:             h,
		ctx:              ctx,
		cancel:           cancel,
		nodes:            make(map[string]*NodeInfo),
		peerToNode:       make(map[peer.ID]string),
		eventChan:        make(chan *NodeEvent, 100),
		heartbeatTimeout: time.Minute * 2,
		log:              logrus.WithField("component", "node_manager"),
	}

	// 初始化本地节点信息
	nm.localNode = &NodeInfo{
		NodeID:       nodeID,
		PeerID:       h.ID(),
		Addrs:        h.Addrs(),
		Status:       NodeStatusOnline,
		Version:      "1.0.0",
		Capabilities: []string{"keygen", "sign"},
		LastSeen:     time.Now(),
		ConnectedAt:  time.Now(),
	}

	// 注册网络事件通知
	h.Network().Notify(&network.NotifyBundle{
		ConnectedF:    nm.onPeerConnected,
		DisconnectedF: nm.onPeerDisconnected,
	})

	return nm
}

// Start 启动节点管理器
func (nm *NodeManager) Start() error {
	// 启动节点状态检查
	go nm.nodeStatusLoop()

	nm.log.Info("Node manager started")
	return nil
}

// Stop 停止节点管理器
func (nm *NodeManager) Stop() error {
	nm.cancel()
	close(nm.eventChan)
	nm.log.Info("Node manager stopped")
	return nil
}

// GetLocalNode 获取本地节点信息
func (nm *NodeManager) GetLocalNode() *NodeInfo {
	return nm.localNode
}

// GetNode 获取节点信息
func (nm *NodeManager) GetNode(nodeID string) (*NodeInfo, bool) {
	nm.nodeMu.RLock()
	defer nm.nodeMu.RUnlock()
	node, ok := nm.nodes[nodeID]
	return node, ok
}

// GetNodeByPeerID 通过PeerID获取节点
func (nm *NodeManager) GetNodeByPeerID(peerID peer.ID) (*NodeInfo, bool) {
	nm.nodeMu.RLock()
	defer nm.nodeMu.RUnlock()

	nodeID, ok := nm.peerToNode[peerID]
	if !ok {
		return nil, false
	}
	node, ok := nm.nodes[nodeID]
	return node, ok
}

// GetAllNodes 获取所有节点
func (nm *NodeManager) GetAllNodes() []*NodeInfo {
	nm.nodeMu.RLock()
	defer nm.nodeMu.RUnlock()

	nodes := make([]*NodeInfo, 0, len(nm.nodes))
	for _, node := range nm.nodes {
		nodes = append(nodes, node)
	}
	return nodes
}

// GetOnlineNodes 获取在线节点
func (nm *NodeManager) GetOnlineNodes() []*NodeInfo {
	nm.nodeMu.RLock()
	defer nm.nodeMu.RUnlock()

	var nodes []*NodeInfo
	for _, node := range nm.nodes {
		if node.Status == NodeStatusOnline {
			nodes = append(nodes, node)
		}
	}
	return nodes
}

// GetOnlineNodeIDs 获取在线节点ID列表
func (nm *NodeManager) GetOnlineNodeIDs() []string {
	nodes := nm.GetOnlineNodes()
	ids := make([]string, len(nodes))
	for i, node := range nodes {
		ids[i] = node.NodeID
	}
	return ids
}

// RegisterNode 注册节点
func (nm *NodeManager) RegisterNode(nodeID string, peerID peer.ID) {
	nm.nodeMu.Lock()
	defer nm.nodeMu.Unlock()

	if _, exists := nm.nodes[nodeID]; exists {
		// 更新现有节点
		nm.nodes[nodeID].PeerID = peerID
		nm.nodes[nodeID].LastSeen = time.Now()
		nm.nodes[nodeID].Status = NodeStatusOnline
	} else {
		// 添加新节点
		nm.nodes[nodeID] = &NodeInfo{
			NodeID:      nodeID,
			PeerID:      peerID,
			Status:      NodeStatusOnline,
			LastSeen:    time.Now(),
			ConnectedAt: time.Now(),
		}
	}
	nm.peerToNode[peerID] = nodeID

	nm.log.WithFields(logrus.Fields{
		"node_id": nodeID,
		"peer_id": peerID.String(),
	}).Info("Node registered")
}

// UpdateNodeFromAnnouncement 从公告更新节点信息
func (nm *NodeManager) UpdateNodeFromAnnouncement(peerID peer.ID, ann *NodeAnnouncement) {
	nm.nodeMu.Lock()
	defer nm.nodeMu.Unlock()

	node, exists := nm.nodes[ann.NodeID]
	if !exists {
		node = &NodeInfo{
			NodeID:      ann.NodeID,
			PeerID:      peerID,
			ConnectedAt: time.Now(),
		}
		nm.nodes[ann.NodeID] = node
	}

	node.PeerID = peerID
	node.Version = ann.Version
	node.Capabilities = ann.Capabilities
	node.Status = NodeStatusOnline
	node.LastSeen = time.Now()
	node.Addrs = nm.host.Network().Peerstore().Addrs(peerID)

	nm.peerToNode[peerID] = ann.NodeID

	nm.log.WithField("node_id", ann.NodeID).Debug("Node updated from announcement")
}

// onPeerConnected 节点连接事件
func (nm *NodeManager) onPeerConnected(net network.Network, conn network.Conn) {
	peerID := conn.RemotePeer()

	nm.log.WithField("peer_id", peerID.String()).Info("Peer connected")

	// 发送事件
	select {
	case nm.eventChan <- &NodeEvent{
		Type:   "connected",
		PeerID: peerID,
		Time:   time.Now(),
	}:
	default:
	}
}

// onPeerDisconnected 节点断开事件
func (nm *NodeManager) onPeerDisconnected(net network.Network, conn network.Conn) {
	peerID := conn.RemotePeer()

	nm.nodeMu.Lock()
	if nodeID, ok := nm.peerToNode[peerID]; ok {
		if node, exists := nm.nodes[nodeID]; exists {
			node.Status = NodeStatusOffline
		}
	}
	nm.nodeMu.Unlock()

	nm.log.WithField("peer_id", peerID.String()).Info("Peer disconnected")

	// 发送事件
	select {
	case nm.eventChan <- &NodeEvent{
		Type:   "disconnected",
		PeerID: peerID,
		Time:   time.Now(),
	}:
	default:
	}
}

// nodeStatusLoop 节点状态检查循环
func (nm *NodeManager) nodeStatusLoop() {
	ticker := time.NewTicker(time.Second * 30)
	defer ticker.Stop()

	for {
		select {
		case <-nm.ctx.Done():
			return
		case <-ticker.C:
			nm.checkNodeStatus()
		}
	}
}

// checkNodeStatus 检查节点状态
func (nm *NodeManager) checkNodeStatus() {
	nm.nodeMu.Lock()
	defer nm.nodeMu.Unlock()

	now := time.Now()
	for nodeID, node := range nm.nodes {
		if node.Status == NodeStatusOnline && now.Sub(node.LastSeen) > nm.heartbeatTimeout {
			node.Status = NodeStatusOffline
			nm.log.WithField("node_id", nodeID).Warn("Node marked as offline (heartbeat timeout)")
		}
	}
}

// UpdateHeartbeat 更新节点心跳
func (nm *NodeManager) UpdateHeartbeat(nodeID string) {
	nm.nodeMu.Lock()
	defer nm.nodeMu.Unlock()

	if node, exists := nm.nodes[nodeID]; exists {
		node.LastSeen = time.Now()
		if node.Status != NodeStatusOnline {
			node.Status = NodeStatusOnline
			nm.log.WithField("node_id", nodeID).Info("Node back online")
		}
	}
}

// EventChan 获取事件通道
func (nm *NodeManager) EventChan() <-chan *NodeEvent {
	return nm.eventChan
}

// CreateAnnouncement 创建节点公告
func (nm *NodeManager) CreateAnnouncement() *NodeAnnouncement {
	return &NodeAnnouncement{
		NodeID:       nm.localNode.NodeID,
		Version:      nm.localNode.Version,
		Capabilities: nm.localNode.Capabilities,
		Timestamp:    time.Now().UnixNano(),
	}
}

// AnnouncementToBytes 公告序列化
func (ann *NodeAnnouncement) ToBytes() ([]byte, error) {
	return json.Marshal(ann)
}

// AnnouncementFromBytes 公告反序列化
func AnnouncementFromBytes(data []byte) (*NodeAnnouncement, error) {
	var ann NodeAnnouncement
	err := json.Unmarshal(data, &ann)
	return &ann, err
}

// IsNodeOnline 检查节点是否在线
func (nm *NodeManager) IsNodeOnline(nodeID string) bool {
	nm.nodeMu.RLock()
	defer nm.nodeMu.RUnlock()

	if node, exists := nm.nodes[nodeID]; exists {
		return node.Status == NodeStatusOnline
	}
	return false
}

// GetNodeCount 获取节点数量
func (nm *NodeManager) GetNodeCount() (total, online int) {
	nm.nodeMu.RLock()
	defer nm.nodeMu.RUnlock()

	total = len(nm.nodes)
	for _, node := range nm.nodes {
		if node.Status == NodeStatusOnline {
			online++
		}
	}
	return
}
