package p2p

import (
	"context"
	"sync"
	"time"

	dht "github.com/libp2p/go-libp2p-kad-dht"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/p2p/discovery/mdns"
	drouting "github.com/libp2p/go-libp2p/p2p/discovery/routing"
	dutil "github.com/libp2p/go-libp2p/p2p/discovery/util"
	"github.com/sirupsen/logrus"
)

const (
	// 发现服务的命名空间
	DiscoveryNamespace = "mpc-wallet"
	// mDNS服务名称
	MDNSServiceTag = "_mpc-wallet._tcp"
	// 发现间隔
	DiscoveryInterval = time.Second * 30
)

// Discovery 节点发现服务
type Discovery struct {
	host   host.Host
	ctx    context.Context
	cancel context.CancelFunc

	// DHT发现
	dht         *dht.IpfsDHT
	routingDisc *drouting.RoutingDiscovery

	// mDNS发现
	mdnsService mdns.Service

	// 发现的节点
	foundPeers map[peer.ID]peer.AddrInfo
	peersMu    sync.RWMutex

	// 节点发现通知
	peerChan chan peer.AddrInfo

	// 配置
	enableDHT  bool
	enableMDNS bool

	log *logrus.Entry
}

// DiscoveryConfig 发现服务配置
type DiscoveryConfig struct {
	EnableDHT      bool
	EnableMDNS     bool
	BootstrapPeers []peer.AddrInfo
	Namespace      string
}

// NewDiscovery 创建发现服务
func NewDiscovery(ctx context.Context, h host.Host, cfg *DiscoveryConfig) (*Discovery, error) {
	ctx, cancel := context.WithCancel(ctx)

	d := &Discovery{
		host:       h,
		ctx:        ctx,
		cancel:     cancel,
		foundPeers: make(map[peer.ID]peer.AddrInfo),
		peerChan:   make(chan peer.AddrInfo, 100),
		enableDHT:  cfg.EnableDHT,
		enableMDNS: cfg.EnableMDNS,
		log:        logrus.WithField("component", "discovery"),
	}

	// 初始化DHT
	if cfg.EnableDHT {
		if err := d.initDHT(ctx, cfg.BootstrapPeers); err != nil {
			cancel()
			return nil, err
		}
	}

	// 初始化mDNS（本地网络发现）
	if cfg.EnableMDNS {
		if err := d.initMDNS(); err != nil {
			d.log.WithError(err).Warn("Failed to initialize mDNS, continuing without it")
		}
	}

	return d, nil
}

// initDHT 初始化Kademlia DHT
func (d *Discovery) initDHT(ctx context.Context, bootstrapPeers []peer.AddrInfo) error {
	d.log.Info("Initializing DHT...")

	// 创建DHT
	kademliaDHT, err := dht.New(ctx, d.host,
		dht.Mode(dht.ModeAutoServer),
		dht.ProtocolPrefix("/mpc-wallet"),
	)
	if err != nil {
		return err
	}
	d.dht = kademliaDHT

	// 启动DHT
	if err := kademliaDHT.Bootstrap(ctx); err != nil {
		return err
	}

	// 连接到引导节点
	var wg sync.WaitGroup
	for _, peerInfo := range bootstrapPeers {
		wg.Add(1)
		go func(pi peer.AddrInfo) {
			defer wg.Done()
			if err := d.host.Connect(ctx, pi); err != nil {
				d.log.WithError(err).WithField("peer", pi.ID).Warn("Failed to connect to bootstrap peer")
			} else {
				d.log.WithField("peer", pi.ID).Info("Connected to bootstrap peer")
			}
		}(peerInfo)
	}
	wg.Wait()

	// 创建路由发现
	d.routingDisc = drouting.NewRoutingDiscovery(kademliaDHT)

	d.log.Info("DHT initialized")
	return nil
}

// initMDNS 初始化mDNS发现（本地网络）
func (d *Discovery) initMDNS() error {
	d.log.Info("Initializing mDNS...")

	// 创建mDNS服务
	service := mdns.NewMdnsService(d.host, MDNSServiceTag, d)
	if err := service.Start(); err != nil {
		return err
	}
	d.mdnsService = service

	d.log.Info("mDNS initialized")
	return nil
}

// HandlePeerFound 实现 mdns.Notifee 接口 - mDNS发现节点回调
func (d *Discovery) HandlePeerFound(pi peer.AddrInfo) {
	if pi.ID == d.host.ID() {
		return // 忽略自己
	}

	d.log.WithFields(logrus.Fields{
		"peer":  pi.ID.String(),
		"addrs": pi.Addrs,
	}).Info("mDNS discovered peer")

	d.addPeer(pi)
}

// Start 启动发现服务
func (d *Discovery) Start() error {
	// 启动DHT发现循环
	if d.enableDHT && d.routingDisc != nil {
		go d.dhtDiscoveryLoop()
	}

	d.log.Info("Discovery service started")
	return nil
}

// Stop 停止发现服务
func (d *Discovery) Stop() error {
	d.cancel()

	if d.mdnsService != nil {
		if err := d.mdnsService.Close(); err != nil {
			d.log.WithError(err).Warn("Failed to close mDNS service")
		}
	}

	if d.dht != nil {
		if err := d.dht.Close(); err != nil {
			d.log.WithError(err).Warn("Failed to close DHT")
		}
	}

	close(d.peerChan)

	d.log.Info("Discovery service stopped")
	return nil
}

// dhtDiscoveryLoop DHT发现循环
func (d *Discovery) dhtDiscoveryLoop() {
	// 先广告自己
	dutil.Advertise(d.ctx, d.routingDisc, DiscoveryNamespace)
	d.log.Info("Advertising in DHT namespace: " + DiscoveryNamespace)

	ticker := time.NewTicker(DiscoveryInterval)
	defer ticker.Stop()

	// 立即执行一次发现
	d.discoverPeers()

	for {
		select {
		case <-d.ctx.Done():
			return
		case <-ticker.C:
			d.discoverPeers()
		}
	}
}

// discoverPeers 发现新节点
func (d *Discovery) discoverPeers() {
	d.log.Debug("Searching for peers...")

	peerChan, err := d.routingDisc.FindPeers(d.ctx, DiscoveryNamespace)
	if err != nil {
		d.log.WithError(err).Warn("Failed to find peers")
		return
	}

	for peer := range peerChan {
		if peer.ID == d.host.ID() {
			continue // 忽略自己
		}

		if len(peer.Addrs) == 0 {
			continue // 忽略没有地址的节点
		}

		d.addPeer(peer)
	}
}

// addPeer 添加发现的节点
func (d *Discovery) addPeer(pi peer.AddrInfo) {
	d.peersMu.Lock()
	_, exists := d.foundPeers[pi.ID]
	d.foundPeers[pi.ID] = pi
	d.peersMu.Unlock()

	if !exists {
		d.log.WithField("peer", pi.ID.String()).Info("New peer discovered")

		// 尝试连接
		go func() {
			if err := d.host.Connect(d.ctx, pi); err != nil {
				d.log.WithError(err).WithField("peer", pi.ID).Debug("Failed to connect to discovered peer")
			} else {
				d.log.WithField("peer", pi.ID).Info("Connected to discovered peer")
			}
		}()

		// 通知
		select {
		case d.peerChan <- pi:
		default:
		}
	}
}

// GetDiscoveredPeers 获取已发现的节点
func (d *Discovery) GetDiscoveredPeers() []peer.AddrInfo {
	d.peersMu.RLock()
	defer d.peersMu.RUnlock()

	peers := make([]peer.AddrInfo, 0, len(d.foundPeers))
	for _, pi := range d.foundPeers {
		peers = append(peers, pi)
	}
	return peers
}

// PeerChan 获取节点发现通知通道
func (d *Discovery) PeerChan() <-chan peer.AddrInfo {
	return d.peerChan
}

// GetDHT 获取DHT实例
func (d *Discovery) GetDHT() *dht.IpfsDHT {
	return d.dht
}

// FindPeer 通过ID查找节点
func (d *Discovery) FindPeer(peerID peer.ID) (peer.AddrInfo, error) {
	if d.dht == nil {
		d.peersMu.RLock()
		pi, ok := d.foundPeers[peerID]
		d.peersMu.RUnlock()
		if ok {
			return pi, nil
		}
		return peer.AddrInfo{}, nil
	}

	return d.dht.FindPeer(d.ctx, peerID)
}
