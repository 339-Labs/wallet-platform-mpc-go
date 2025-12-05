package api

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"

	"github.com/339-Labs/wallet-platform-mpc-go/internal/config"
	"github.com/339-Labs/wallet-platform-mpc-go/internal/p2p"
	"github.com/339-Labs/wallet-platform-mpc-go/internal/wallet"
	"github.com/339-Labs/wallet-platform-mpc-go/pkg/types"
)

// Server HTTP API服务器
type Server struct {
	engine    *gin.Engine
	server    *http.Server
	cfg       *config.APIConfig
	walletMgr *wallet.Manager
	p2pHost   *p2p.P2PHost
	log       *logrus.Entry
}

// NewServer 创建API服务器
func NewServer(cfg *config.APIConfig, walletMgr *wallet.Manager, p2pHost *p2p.P2PHost) *Server {
	if cfg.EnableCORS {
		gin.SetMode(gin.ReleaseMode)
	}

	engine := gin.New()
	engine.Use(gin.Recovery())
	engine.Use(gin.Logger())

	s := &Server{
		engine:    engine,
		cfg:       cfg,
		walletMgr: walletMgr,
		p2pHost:   p2pHost,
		log:       logrus.WithField("component", "api_server"),
	}

	// 设置路由
	s.setupRoutes()

	return s
}

// setupRoutes 设置路由
func (s *Server) setupRoutes() {
	// CORS中间件
	if s.cfg.EnableCORS {
		s.engine.Use(corsMiddleware())
	}

	// 认证中间件
	if s.cfg.EnableAuth {
		s.engine.Use(authMiddleware(s.cfg.AuthToken))
	}

	// API路由组
	api := s.engine.Group("/api/v1")
	{
		// 健康检查
		api.GET("/health", s.healthCheck)

		// 节点信息
		api.GET("/node/info", s.getNodeInfo)
		api.GET("/node/peers", s.getPeers)

		// 钱包管理
		wallets := api.Group("/wallets")
		{
			wallets.POST("", s.createWallet)
			wallets.GET("", s.listWallets)
			wallets.GET("/:id", s.getWallet)
			wallets.DELETE("/:id", s.deleteWallet)
			wallets.GET("/:id/address", s.getWalletAddress)
		}

		// 签名
		sign := api.Group("/sign")
		{
			sign.POST("/message", s.signMessage)
			sign.POST("/transaction", s.signTransaction)
		}

		// 密钥重分享
		reshare := api.Group("/reshare")
		{
			reshare.POST("", s.reshareWallet)
			reshare.POST("/refresh", s.refreshKeyShares)
		}

		// 会话管理
		sessions := api.Group("/sessions")
		{
			sessions.GET("", s.listSessions)
			sessions.GET("/:id", s.getSession)
		}
	}
}

// Start 启动服务器
func (s *Server) Start() error {
	addr := fmt.Sprintf("%s:%d", s.cfg.Host, s.cfg.Port)

	s.server = &http.Server{
		Addr:    addr,
		Handler: s.engine,
	}

	s.log.WithField("addr", addr).Info("Starting API server")

	go func() {
		if err := s.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			s.log.WithError(err).Fatal("API server error")
		}
	}()

	return nil
}

// Stop 停止服务器
func (s *Server) Stop() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	return s.server.Shutdown(ctx)
}

// 响应函数
func successResponse(c *gin.Context, data interface{}) {
	c.JSON(http.StatusOK, types.APIResponse{
		Success: true,
		Data:    data,
	})
}

func errorResponse(c *gin.Context, status int, err string) {
	c.JSON(status, types.APIResponse{
		Success: false,
		Error:   err,
	})
}

// CORS中间件
func corsMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Access-Control-Allow-Origin", "*")
		c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		c.Header("Access-Control-Allow-Headers", "Content-Type, Authorization")

		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}

		c.Next()
	}
}

// 认证中间件
func authMiddleware(token string) gin.HandlerFunc {
	return func(c *gin.Context) {
		authHeader := c.GetHeader("Authorization")
		if authHeader != "Bearer "+token {
			errorResponse(c, http.StatusUnauthorized, "unauthorized")
			c.Abort()
			return
		}
		c.Next()
	}
}

// 健康检查
func (s *Server) healthCheck(c *gin.Context) {
	successResponse(c, gin.H{
		"status": "ok",
		"time":   time.Now().Unix(),
	})
}

// 获取节点信息
func (s *Server) getNodeInfo(c *gin.Context) {
	info := gin.H{
		"node_id": s.p2pHost.GetNodeID(),
		"peer_id": s.p2pHost.GetPeerID().String(),
		"addrs":   s.p2pHost.GetFullAddrs(),
	}
	successResponse(c, info)
}

// 获取连接的对等节点
func (s *Server) getPeers(c *gin.Context) {
	peers := s.p2pHost.GetConnectedPeers()
	successResponse(c, peers)
}

// CreateWalletRequest 创建钱包请求
type CreateWalletRequest struct {
	Name       string   `json:"name" binding:"required"`
	Threshold  int      `json:"threshold" binding:"required,min=2"` // 签名所需的最少节点数，至少为2
	TotalParts int      `json:"total_parts" binding:"required,min=2"`
	PartyIDs   []string `json:"party_ids" binding:"required"`
}

// 创建钱包
func (s *Server) createWallet(c *gin.Context) {
	var req CreateWalletRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		errorResponse(c, http.StatusBadRequest, err.Error())
		return
	}

	wallet, err := s.walletMgr.CreateWallet(
		c.Request.Context(),
		req.Name,
		req.Threshold,
		req.TotalParts,
		req.PartyIDs,
	)
	if err != nil {
		errorResponse(c, http.StatusInternalServerError, err.Error())
		return
	}

	successResponse(c, wallet)
}

// 列出所有钱包
func (s *Server) listWallets(c *gin.Context) {
	wallets, err := s.walletMgr.ListWallets()
	if err != nil {
		errorResponse(c, http.StatusInternalServerError, err.Error())
		return
	}

	successResponse(c, wallets)
}

// 获取钱包
func (s *Server) getWallet(c *gin.Context) {
	walletID := c.Param("id")

	wallet, err := s.walletMgr.GetWallet(walletID)
	if err != nil {
		errorResponse(c, http.StatusNotFound, err.Error())
		return
	}

	successResponse(c, wallet)
}

// 删除钱包
func (s *Server) deleteWallet(c *gin.Context) {
	walletID := c.Param("id")

	if err := s.walletMgr.DeleteWallet(walletID); err != nil {
		errorResponse(c, http.StatusInternalServerError, err.Error())
		return
	}

	successResponse(c, gin.H{"deleted": true})
}

// 获取钱包地址
func (s *Server) getWalletAddress(c *gin.Context) {
	walletID := c.Param("id")

	address, err := s.walletMgr.GetAddress(walletID)
	if err != nil {
		errorResponse(c, http.StatusNotFound, err.Error())
		return
	}

	successResponse(c, gin.H{"address": address})
}

// SignMessageRequest 签名消息请求
type SignMessageRequest struct {
	WalletID  string   `json:"wallet_id" binding:"required"`
	Message   string   `json:"message" binding:"required"`
	SignerIDs []string `json:"signer_ids" binding:"required"`
}

// 签名消息
func (s *Server) signMessage(c *gin.Context) {
	var req SignMessageRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		errorResponse(c, http.StatusBadRequest, err.Error())
		return
	}

	result, err := s.walletMgr.SignMessage(
		c.Request.Context(),
		req.WalletID,
		[]byte(req.Message),
		req.SignerIDs,
	)
	if err != nil {
		errorResponse(c, http.StatusInternalServerError, err.Error())
		return
	}

	successResponse(c, result)
}

// 签名交易
func (s *Server) signTransaction(c *gin.Context) {
	var req types.TransactionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		errorResponse(c, http.StatusBadRequest, err.Error())
		return
	}

	result, rawTx, err := s.walletMgr.SignTransaction(c.Request.Context(), &req)
	if err != nil {
		errorResponse(c, http.StatusInternalServerError, err.Error())
		return
	}

	successResponse(c, gin.H{
		"signature": result,
		"raw_tx":    rawTx,
	})
}

// 列出会话
func (s *Server) listSessions(c *gin.Context) {
	// TODO: 实现会话列表
	successResponse(c, []interface{}{})
}

// 获取会话
func (s *Server) getSession(c *gin.Context) {
	sessionID := c.Param("id")
	// TODO: 实现会话获取
	successResponse(c, gin.H{"id": sessionID})
}

// ReshareRequest 重分享请求
type ReshareRequest struct {
	WalletID     string   `json:"wallet_id" binding:"required"`
	OldThreshold int      `json:"old_threshold" binding:"required,min=1"`
	OldPartyIDs  []string `json:"old_party_ids" binding:"required"`
	NewThreshold int      `json:"new_threshold" binding:"required,min=1"`
	NewPartyIDs  []string `json:"new_party_ids" binding:"required"`
}

// 重分享钱包
func (s *Server) reshareWallet(c *gin.Context) {
	var req ReshareRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		errorResponse(c, http.StatusBadRequest, err.Error())
		return
	}

	result, err := s.walletMgr.ReshareWallet(
		c.Request.Context(),
		&types.ResharingRequest{
			WalletID:     req.WalletID,
			OldThreshold: req.OldThreshold,
			OldPartyIDs:  req.OldPartyIDs,
			NewThreshold: req.NewThreshold,
			NewPartyIDs:  req.NewPartyIDs,
		},
	)
	if err != nil {
		errorResponse(c, http.StatusInternalServerError, err.Error())
		return
	}

	successResponse(c, result)
}

// RefreshRequest 刷新密钥分片请求
type RefreshRequest struct {
	WalletID string `json:"wallet_id" binding:"required"`
}

// 刷新密钥分片
func (s *Server) refreshKeyShares(c *gin.Context) {
	var req RefreshRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		errorResponse(c, http.StatusBadRequest, err.Error())
		return
	}

	result, err := s.walletMgr.RefreshKeyShares(c.Request.Context(), req.WalletID)
	if err != nil {
		errorResponse(c, http.StatusInternalServerError, err.Error())
		return
	}

	successResponse(c, result)
}
