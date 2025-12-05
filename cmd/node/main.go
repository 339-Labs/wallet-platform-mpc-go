package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/339-Labs/wallet-platform-mpc-go/internal/api"
	"github.com/339-Labs/wallet-platform-mpc-go/internal/config"
	"github.com/339-Labs/wallet-platform-mpc-go/internal/p2p"
	"github.com/339-Labs/wallet-platform-mpc-go/internal/storage"
	"github.com/339-Labs/wallet-platform-mpc-go/internal/tss"
	"github.com/339-Labs/wallet-platform-mpc-go/internal/wallet"
)

var (
	configFile = flag.String("config", "config.yaml", "Path to config file")
	nodeID     = flag.String("node-id", "", "Node ID (overrides config)")
	p2pPort    = flag.Int("p2p-port", 0, "P2P port (overrides config)")
	apiPort    = flag.Int("api-port", 0, "API port (overrides config)")
	dataDir    = flag.String("data-dir", "", "Data directory (overrides config)")
	logLevel   = flag.String("log-level", "", "Log level (overrides config)")
)

func main() {
	flag.Parse()

	// 设置日志
	log := logrus.New()
	log.SetFormatter(&logrus.JSONFormatter{
		TimestampFormat: time.RFC3339,
	})

	// 加载配置
	cfg := config.DefaultConfig()
	if _, err := os.Stat(*configFile); err == nil {
		loadedCfg, err := config.LoadConfig(*configFile)
		if err != nil {
			log.WithError(err).Fatal("Failed to load config")
		}
		cfg = loadedCfg
	}

	// 命令行参数覆盖配置
	if *nodeID != "" {
		cfg.Node.ID = *nodeID
	}
	if *p2pPort != 0 {
		cfg.P2P.Port = *p2pPort
	}
	if *apiPort != 0 {
		cfg.API.Port = *apiPort
	}
	if *dataDir != "" {
		cfg.Storage.DataDir = *dataDir
	}
	if *logLevel != "" {
		cfg.Log.Level = *logLevel
	}

	// 设置日志级别
	level, err := logrus.ParseLevel(cfg.Log.Level)
	if err != nil {
		level = logrus.InfoLevel
	}
	log.SetLevel(level)

	log.WithFields(logrus.Fields{
		"node_id":  cfg.Node.ID,
		"p2p_port": cfg.P2P.Port,
		"api_port": cfg.API.Port,
		"data_dir": cfg.Storage.DataDir,
	}).Info("Starting MPC Wallet Node")

	// 创建上下文
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 初始化存储
	store, err := storage.NewStorage(cfg.Storage.DataDir)
	if err != nil {
		log.WithError(err).Fatal("Failed to initialize storage")
	}
	defer store.Close()

	// 创建存储库
	walletRepo := storage.NewWalletRepository(store)
	sessionRepo := storage.NewSessionRepository(store)

	keyShareDir := filepath.Join(cfg.Storage.DataDir, "keyshares")
	keyShareRepo, err := storage.NewKeyShareRepository(store, keyShareDir, cfg.Node.ID)
	if err != nil {
		log.WithError(err).Fatal("Failed to initialize key share repository")
	}

	// 初始化P2P网络
	keyFile := filepath.Join(cfg.Storage.DataDir, cfg.Node.KeyFile)
	p2pHost, err := p2p.NewP2PHost(ctx, &cfg.P2P, cfg.Node.ID, keyFile)
	if err != nil {
		log.WithError(err).Fatal("Failed to initialize P2P host")
	}
	defer p2pHost.Stop()

	// 启动P2P服务
	if err := p2pHost.Start(); err != nil {
		log.WithError(err).Fatal("Failed to start P2P host")
	}

	// 创建消息管理器
	msgManager := p2p.NewMessageManager(p2pHost)
	if err := msgManager.Start(); err != nil {
		log.WithError(err).Fatal("Failed to start message manager")
	}
	defer msgManager.Stop()

	// 创建TSS管理器
	keygenTimeout := time.Duration(cfg.TSS.KeygenTimeout) * time.Second
	signTimeout := time.Duration(cfg.TSS.SignTimeout) * time.Second
	resharingTimeout := time.Duration(cfg.TSS.KeygenTimeout) * time.Second * 2 // 重分享需要更长时间

	keygenMgr := tss.NewKeygenManager(
		msgManager,
		keyShareRepo,
		sessionRepo,
		walletRepo,
		cfg.Node.ID,
		keygenTimeout,
	)

	signingMgr := tss.NewSigningManager(
		msgManager,
		keyShareRepo,
		sessionRepo,
		walletRepo,
		cfg.Node.ID,
		signTimeout,
	)

	resharingMgr := tss.NewResharingManager(
		msgManager,
		keyShareRepo,
		sessionRepo,
		walletRepo,
		cfg.Node.ID,
		resharingTimeout,
	)

	// 创建协调器（用于多节点协调）
	log.Info("Creating coordinator...")
	coordinator := tss.NewCoordinator(
		p2pHost,
		msgManager,
		keygenMgr,
		signingMgr,
		resharingMgr,
		cfg.Node.ID,
	)
	if err := coordinator.Start(); err != nil {
		log.WithError(err).Fatal("Failed to start coordinator")
	}
	log.Info("Coordinator created and started successfully")
	defer coordinator.Stop()

	// 创建钱包管理器
	walletMgr := wallet.NewManager(
		walletRepo,
		keyShareRepo,
		sessionRepo,
		keygenMgr,
		signingMgr,
		resharingMgr,
		coordinator,
		cfg.Node.ID,
	)

	// 创建API服务器
	apiServer := api.NewServer(&cfg.API, walletMgr, p2pHost)
	if err := apiServer.Start(); err != nil {
		log.WithError(err).Fatal("Failed to start API server")
	}
	defer apiServer.Stop()

	// 打印节点信息
	log.WithFields(logrus.Fields{
		"node_id": cfg.Node.ID,
		"peer_id": p2pHost.GetPeerID().String(),
		"addrs":   p2pHost.GetFullAddrs(),
		"api":     fmt.Sprintf("http://%s:%d", cfg.API.Host, cfg.API.Port),
	}).Info("MPC Wallet Node started")

	// 等待信号
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	<-sigChan
	log.Info("Shutting down...")
	cancel()
}
