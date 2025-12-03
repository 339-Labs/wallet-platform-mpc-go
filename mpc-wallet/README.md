# MPC Wallet - 基于TSS的分布式托管钱包

一个基于门限签名方案（Threshold Signature Scheme, TSS）的多方计算（MPC）托管钱包后端实现。使用 BNB Chain 的 tss-lib 库实现分布式密钥生成和签名，集成 libp2p 进行节点间通信，使用 LevelDB 进行数据持久化。

## 🌟 特性

- **分布式密钥生成 (DKG)**: 多个节点协作生成密钥，无需任何单一方持有完整私钥
- **门限签名**: t-of-n 签名方案，需要至少 t 个节点协作才能签名
- **P2P 网络通信**: 基于 libp2p 的去中心化节点通信
- **安全存储**: 密钥分片使用 AES-GCM 加密存储
- **以太坊兼容**: 支持 EIP-155 交易签名
- **RESTful API**: 完整的 HTTP API 接口
- **Docker 支持**: 容器化部署

## 📁 项目结构

```
mpc-wallet/
├── cmd/
│   └── node/              # 节点主程序入口
│       └── main.go
├── internal/
│   ├── config/            # 配置管理
│   ├── storage/           # LevelDB 存储层
│   │   ├── leveldb.go     # 基础存储
│   │   ├── wallet_repo.go # 钱包存储库
│   │   ├── keyshare_repo.go # 密钥分片存储库
│   │   └── session_repo.go  # 会话存储库
│   ├── p2p/               # P2P 网络层
│   │   ├── host.go        # libp2p 主机
│   │   └── message.go     # 消息管理
│   ├── tss/               # TSS 协议实现
│   │   ├── party.go       # 参与方管理
│   │   ├── keygen.go      # 密钥生成
│   │   └── signing.go     # 签名
│   ├── wallet/            # 钱包管理
│   │   └── manager.go
│   └── api/               # HTTP API
│       └── server.go
├── pkg/
│   └── types/             # 公共类型定义
├── configs/               # 配置文件
├── scripts/               # 脚本
├── Dockerfile
├── docker-compose.yaml
├── Makefile
└── README.md
```

## 🚀 快速开始

### 前置要求

- Go 1.21+
- Docker & Docker Compose (可选)

### 本地构建

```bash
# 克隆项目
git clone <repo-url>
cd mpc-wallet

# 下载依赖
go mod download

# 构建
make build
```

### 运行单节点

```bash
# 运行节点1
make run-node1

# 在另一个终端运行节点2
make run-node2

# 在另一个终端运行节点3
make run-node3
```

### 运行集群

```bash
# 启动3节点集群
make run-cluster

# 停止集群
make stop-cluster
```

### Docker 部署

```bash
# 构建镜像
make docker-build

# 启动集群
make docker-up

# 查看日志
make docker-logs

# 停止集群
make docker-down
```

## 📡 API 接口

### 基础端点

| 方法 | 路径 | 描述 |
|------|------|------|
| GET | `/api/v1/health` | 健康检查 |
| GET | `/api/v1/node/info` | 获取节点信息 |
| GET | `/api/v1/node/peers` | 获取连接的节点列表 |

### 钱包管理

| 方法 | 路径 | 描述 |
|------|------|------|
| POST | `/api/v1/wallets` | 创建新钱包 (发起DKG) |
| GET | `/api/v1/wallets` | 列出所有钱包 |
| GET | `/api/v1/wallets/:id` | 获取钱包详情 |
| DELETE | `/api/v1/wallets/:id` | 删除钱包 |
| GET | `/api/v1/wallets/:id/address` | 获取钱包地址 |

### 签名

| 方法 | 路径 | 描述 |
|------|------|------|
| POST | `/api/v1/sign/message` | 签名消息 |
| POST | `/api/v1/sign/transaction` | 签名交易 |

## 📖 API 使用示例

### 创建钱包

```bash
curl -X POST http://localhost:8081/api/v1/wallets \
  -H "Content-Type: application/json" \
  -d '{
    "name": "My MPC Wallet",
    "threshold": 2,
    "total_parts": 3,
    "party_ids": ["node-1", "node-2", "node-3"]
  }'
```

响应:
```json
{
  "success": true,
  "data": {
    "id": "wallet-uuid",
    "name": "My MPC Wallet",
    "address": "0x...",
    "public_key": "0x...",
    "threshold": 2,
    "total_parts": 3,
    "party_ids": ["node-1", "node-2", "node-3"],
    "created_at": "2024-01-01T00:00:00Z"
  }
}
```

### 签名消息

```bash
curl -X POST http://localhost:8081/api/v1/sign/message \
  -H "Content-Type: application/json" \
  -d '{
    "wallet_id": "wallet-uuid",
    "message": "Hello, MPC!",
    "signer_ids": ["node-1", "node-2"]
  }'
```

响应:
```json
{
  "success": true,
  "data": {
    "request_id": "req-uuid",
    "signature": "0x...",
    "r": "...",
    "s": "...",
    "v": 27
  }
}
```

### 签名交易

```bash
curl -X POST http://localhost:8081/api/v1/sign/transaction \
  -H "Content-Type: application/json" \
  -d '{
    "wallet_id": "wallet-uuid",
    "to": "0x742d35Cc6634C0532925a3b844Bc9e7595f0bEb0",
    "value": "1000000000000000000",
    "data": "",
    "nonce": 0,
    "gas_price": "20000000000",
    "gas_limit": 21000,
    "chain_id": 1,
    "party_ids": ["node-1", "node-2"]
  }'
```

## ⚙️ 配置说明

配置文件格式 (YAML):

```yaml
# 节点配置
node:
  id: "node-1"              # 节点唯一标识
  name: "MPC Node 1"        # 节点名称
  key_file: "node1.key"     # P2P密钥文件

# P2P网络配置
p2p:
  listen_addr: "0.0.0.0"
  port: 4001
  bootstrap_peers: []       # 引导节点地址列表
  max_peers: 50
  enable_relay: true
  pubsub_topic: "mpc-wallet"

# HTTP API配置
api:
  host: "0.0.0.0"
  port: 8081
  enable_cors: true
  enable_auth: false
  auth_token: ""

# 存储配置
storage:
  data_dir: "./data/node1"
  keyshare_dir: "./data/node1/keyshares"

# TSS配置
tss:
  default_threshold: 2      # 默认签名阈值
  default_total_parts: 3    # 默认参与方数量
  keygen_timeout: 300       # 密钥生成超时(秒)
  sign_timeout: 60          # 签名超时(秒)

# 日志配置
log:
  level: "info"             # debug, info, warn, error
  format: "json"
  output: "stdout"
```

## 🔐 安全考虑

1. **密钥分片加密**: 所有密钥分片使用 AES-256-GCM 加密存储
2. **P2P 通信加密**: 使用 TLS 和 Noise 协议保护节点间通信
3. **无单点故障**: 任何单一节点被攻破都不会泄露完整私钥
4. **门限安全**: 需要至少 t 个节点协作才能进行签名

## 🏗️ 架构说明

### TSS 协议流程

#### 密钥生成 (DKG)

1. 各节点生成本地随机数并计算承诺
2. 广播承诺并验证
3. 执行 Feldman VSS 分发密钥分片
4. 验证接收到的分片
5. 计算公共公钥

#### 签名

1. 选择 t 个签名节点
2. 执行 MtA (Multiplicative-to-Additive) 协议
3. 计算部分签名
4. 聚合生成完整签名

### P2P 网络

- 基于 libp2p 的 GossipSub 协议进行消息广播
- 支持直接连接和中继连接
- 自动节点发现和连接管理

## 📊 性能指标

| 操作 | 预期时间 (3节点, 2阈值) |
|------|------------------------|
| 密钥生成 | 5-30 秒 |
| 签名 | 1-5 秒 |

## 🛠️ 开发

### 运行测试

```bash
make test
```

### 代码格式化

```bash
make fmt
```

### 代码检查

```bash
make lint
```

## 📝 TODO

- [ ] 支持更多曲线 (ed25519, secp256k1)
- [ ] 密钥刷新协议
- [ ] 节点动态加入/退出
- [ ] Web UI 管理界面
- [ ] Prometheus 监控指标
- [ ] 支持更多区块链

## 📄 许可证

MIT License

## 🙏 致谢

- [BNB Chain tss-lib](https://github.com/bnb-chain/tss-lib) - TSS 协议实现
- [libp2p](https://github.com/libp2p/go-libp2p) - P2P 网络库
- [LevelDB](https://github.com/syndtr/goleveldb) - 键值存储
- [Gin](https://github.com/gin-gonic/gin) - HTTP 框架
