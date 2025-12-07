# MPC Wallet 流程图 (Mermaid)

本文档包含可渲染的 Mermaid 流程图。

> **注意**: 本项目使用 BNB Chain tss-lib，实现的是 **GG18 (Gennaro-Goldfeder 2018)** 协议，使用 Paillier 同态加密实现 MtA。

## 1. 整体系统架构

```mermaid
graph TB
    subgraph Client
        HTTP[HTTP Client]
    end
    
    subgraph API["API Layer"]
        Server[api/server.go<br/>Gin HTTP Server]
    end
    
    subgraph Wallet["Wallet Layer"]
        Manager[wallet/manager.go<br/>Wallet Manager]
    end
    
    subgraph TSS["TSS Layer"]
        Coord[Coordinator<br/>会话协调]
        Keygen[KeygenManager<br/>密钥生成]
        Signing[SigningManager<br/>签名]
        Reshare[ResharingManager<br/>密钥重分享]
        TSSLib[BNB tss-lib<br/>GG20 Protocol]
    end
    
    subgraph P2P["P2P Layer"]
        Host[P2PHost<br/>连接管理]
        PubSub[PubSubManager<br/>GossipSub]
        Discovery[Discovery<br/>DHT + mDNS]
        NodeMgr[NodeManager<br/>节点状态]
        MsgMgr[MessageManager<br/>消息路由]
    end
    
    subgraph Storage["Storage Layer"]
        WalletRepo[WalletRepository]
        KeyShareRepo[KeyShareRepository]
        SessionRepo[SessionRepository]
        LevelDB[(LevelDB)]
    end
    
    HTTP --> Server
    Server --> Manager
    Manager --> Coord
    Manager --> Keygen
    Manager --> Signing
    Manager --> Reshare
    
    Coord --> Keygen
    Coord --> Signing
    Keygen --> TSSLib
    Signing --> TSSLib
    Reshare --> TSSLib
    
    Keygen --> MsgMgr
    Signing --> MsgMgr
    Reshare --> MsgMgr
    
    MsgMgr --> Host
    Host --> PubSub
    Host --> Discovery
    Host --> NodeMgr
    
    Keygen --> KeyShareRepo
    Signing --> KeyShareRepo
    Manager --> WalletRepo
    Coord --> SessionRepo
    
    WalletRepo --> LevelDB
    KeyShareRepo --> LevelDB
    SessionRepo --> LevelDB
```

## 2. Keygen (密钥生成) 流程

```mermaid
sequenceDiagram
    participant Client
    participant API as API Server
    participant WM as Wallet Manager
    participant Coord as Coordinator
    participant KM as Keygen Manager
    participant P2P as P2P Host
    participant Node2 as Node-2
    participant Node3 as Node-3
    participant TSS as tss-lib
    
    Client->>API: POST /api/v1/wallets
    API->>WM: CreateWallet()
    WM->>Coord: InitiateKeygen()
    
    Note over Coord: 创建会话 UUID
    
    Coord->>KM: PrepareKeygen()
    Coord->>P2P: BroadcastMessage(keygen_init)
    P2P-->>Node2: keygen_init
    P2P-->>Node3: keygen_init
    
    Node2-->>P2P: keygen_ready
    Node3-->>P2P: keygen_ready
    
    Note over Coord: 检查所有节点就绪
    
    Coord->>P2P: BroadcastMessage(keygen_start)
    P2P-->>Node2: keygen_start
    P2P-->>Node3: keygen_start
    
    Coord->>KM: session.Start()
    
    rect rgb(200, 220, 255)
        Note over KM,TSS: TSS Protocol Execution
        KM->>TSS: LocalParty.Start()
        
        loop Round 1-3
            TSS->>P2P: outChan → 发送消息
            P2P-->>Node2: TSS消息
            P2P-->>Node3: TSS消息
            Node2-->>P2P: TSS响应
            Node3-->>P2P: TSS响应
            P2P->>TSS: msgChan ← 接收消息
        end
        
        TSS->>KM: endChan (LocalPartySaveData)
    end
    
    KM->>KM: 保存密钥分片到 LevelDB
    KM->>WM: 返回结果
    WM->>API: 钱包信息
    API->>Client: {address, public_key, ...}
```

## 3. Signing (签名) 流程

```mermaid
sequenceDiagram
    participant Client
    participant API as API Server
    participant WM as Wallet Manager
    participant Coord as Coordinator
    participant SM as Signing Manager
    participant P2P as P2P Host
    participant Node2 as Node-2
    participant TSS as tss-lib
    
    Client->>API: POST /api/v1/sign/message
    API->>WM: SignMessage()
    
    Note over WM: 计算消息哈希 (Keccak256)
    
    WM->>Coord: InitiateSign()
    Coord->>SM: PrepareSigning()
    
    Note over SM: 加载本地密钥分片
    
    Coord->>P2P: BroadcastMessage(sign_init)
    P2P-->>Node2: sign_init
    
    Node2-->>P2P: sign_ready
    
    Coord->>P2P: BroadcastMessage(sign_start)
    P2P-->>Node2: sign_start
    
    Coord->>SM: session.Start()
    
    rect rgb(255, 220, 200)
        Note over SM,TSS: GG18 Signing Protocol (Paillier MtA)
        SM->>TSS: LocalParty.Start(message, keyData)
        
        Note over TSS: Round 1: 生成随机数, Paillier加密
        TSS->>P2P: 广播承诺 + Paillier密文
        Node2-->>P2P: 广播承诺 + Paillier密文
        
        Note over TSS: Round 2-3: Paillier MtA + MtAwc
        TSS->>P2P: 点对点交换MtA数据
        Node2-->>P2P: 点对点交换MtA数据
        
        Note over TSS: Round 4-6: 计算δ, R, 签名分片
        TSS->>P2P: 广播签名分片
        Node2-->>P2P: 广播签名分片
        
        Note over TSS: 聚合签名并验证
        TSS->>SM: endChan (SignatureData)
    end
    
    SM->>WM: SignResult {r, s, v}
    WM->>API: 签名结果
    API->>Client: {signature: "0x..."}
```

## 4. Resharing (密钥重分享) 流程

```mermaid
sequenceDiagram
    participant API as API Server
    participant WM as Wallet Manager
    participant RM as Resharing Manager
    participant P2P as P2P Host
    participant Old1 as Node-1 (旧+新)
    participant Old2 as Node-2 (旧+新)
    participant Old3 as Node-3 (仅旧)
    participant New4 as Node-4 (仅新)
    participant New5 as Node-5 (仅新)
    
    API->>WM: ReshareWallet()
    WM->>RM: StartResharing()
    
    Note over RM: 检查节点角色
    
    RM->>P2P: 广播 reshare_init
    P2P-->>Old1: reshare_init
    P2P-->>Old2: reshare_init
    P2P-->>Old3: reshare_init
    P2P-->>New4: reshare_init
    P2P-->>New5: reshare_init
    
    rect rgb(220, 255, 220)
        Note over Old1,New5: TSS Resharing Protocol
        
        Note over Old1,Old3: 旧成员加载现有分片
        Note over New4,New5: 新成员无分片
        
        Note over Old1,Old3: Round 1: 生成新多项式
        Old1->>P2P: VSS 分发
        Old2->>P2P: VSS 分发
        Old3->>P2P: VSS 分发
        
        P2P-->>Old1: 分片
        P2P-->>Old2: 分片
        P2P-->>New4: 分片
        P2P-->>New5: 分片
        
        Note over Old1,New5: Round 2: 验证并聚合
    end
    
    Note over Old1,Old2: 保存新分片
    Note over New4,New5: 保存新分片
    Note over Old3: 分片作废
    
    RM->>WM: ResharingResult
    WM->>API: 重分享完成
```

## 5. P2P 网络拓扑

```mermaid
graph TB
    subgraph "P2P Network"
        N1[Node-1<br/>:4001]
        N2[Node-2<br/>:4002]
        N3[Node-3<br/>:4003]
        
        N1 <-->|libp2p| N2
        N2 <-->|libp2p| N3
        N1 <-->|libp2p| N3
    end
    
    subgraph "Discovery"
        DHT[(Kademlia DHT)]
        MDNS[mDNS<br/>局域网发现]
    end
    
    subgraph "PubSub Topics"
        T1[mpc/keygen]
        T2[mpc/sign]
        T3[mpc/broadcast]
        T4[mpc/heartbeat]
    end
    
    N1 --> DHT
    N2 --> DHT
    N3 --> DHT
    
    N1 --> MDNS
    N2 --> MDNS
    N3 --> MDNS
    
    N1 --> T1
    N1 --> T2
    N1 --> T3
    N1 --> T4
    
    N2 --> T1
    N2 --> T2
    N2 --> T3
    N2 --> T4
    
    N3 --> T1
    N3 --> T2
    N3 --> T3
    N3 --> T4
```

## 6. 消息处理流程

```mermaid
flowchart TB
    subgraph "消息接收"
        A[PubSub 接收消息] --> B{消息类型?}
    end
    
    subgraph "协调消息"
        B -->|keygen_init| C1[handleKeygenInit]
        B -->|keygen_ready| C2[handleKeygenReady]
        B -->|keygen_start| C3[handleKeygenStart]
        B -->|sign_init| C4[handleSignInit]
        B -->|sign_ready| C5[handleSignReady]
        B -->|sign_start| C6[handleSignStart]
    end
    
    subgraph "TSS 消息"
        B -->|keygen_round| D1[routeToSession]
        B -->|sign_round| D2[routeToSession]
        
        D1 --> E1{查找 Session}
        D2 --> E1
        
        E1 -->|找到| F1[Session.MsgChan]
        E1 -->|未找到| F2[丢弃消息]
        
        F1 --> G1[TSS Party.Update]
    end
    
    subgraph "心跳消息"
        B -->|ping| H1[handlePing]
        H1 --> H2[发送 pong]
    end
    
    C1 --> I1[PrepareKeygen]
    C3 --> I2[session.Start]
    C4 --> I3[PrepareSigning]
    C6 --> I4[session.Start]
```

## 7. TSS GG18 签名协议详细流程

```mermaid
flowchart TB
    subgraph "Round 1: 生成随机数和承诺"
        A1[生成随机数 k_i, γ_i] --> A2[计算 Γ_i = g^γ_i]
        A2 --> A3[计算承诺 C_i = H_Γ_i_]
        A3 --> A4[Paillier加密 c_i = Enc_k_i_]
        A4 --> A5[广播 C_i, c_i, Paillier证明]
    end
    
    subgraph "Round 2: 揭示承诺 + Paillier MtA"
        B1[验证承诺 H_Γ_j_ == C_j] --> B2[揭示 Γ_i = g^γ_i]
        B2 --> B3[执行 Paillier MtA]
        B3 --> B4[计算 D_ji = c_i^γ_j · Enc_-β_ji_]
        B4 --> B5[点对点发送 D_ji + Range Proof]
    end
    
    subgraph "Round 3: MtAwc"
        C1[验证 Range Proof] --> C2[解密得到 α_ij = k_iγ_j - β_ji]
        C2 --> C3[执行 MtAwc: k_i * w_j]
        C3 --> C4[w_j = x_j * Lagrange_j_]
    end
    
    subgraph "Round 4: 计算 δ"
        D1[δ_i = k_iγ_i + Σα_ij + Σβ_ji] --> D2[广播 δ_i]
    end
    
    subgraph "Round 5: 计算 R"
        E1[δ = Σδ_i mod q] --> E2[Γ = Πᵢ Γ_i]
        E2 --> E3[R = Γ^δ^-1 = g^1/Σk_i]
        E3 --> E4[r = R.x mod q]
    end
    
    subgraph "Round 6: 生成并聚合签名"
        F1[σ_i = k_iw_i + Σμ_ij + Σν_ji] --> F2[s_i = m·k_i + r·σ_i]
        F2 --> F3[广播 s_i]
        F3 --> F4[聚合 s = Σs_i mod q]
        F4 --> F5[验证签名有效性]
        F5 --> F6[输出签名 r, s, v]
    end
    
    A5 --> B1
    B5 --> C1
    C4 --> D1
    D2 --> E1
    E4 --> F1
```

## 8. 存储结构

```mermaid
erDiagram
    WALLET {
        string id PK "钱包ID"
        string name "钱包名称"
        string address "以太坊地址"
        string public_key "公钥"
        int threshold "签名阈值"
        int total_parts "总分片数"
        array party_ids "参与方列表"
        datetime created_at "创建时间"
        datetime updated_at "更新时间"
    }
    
    KEYSHARE {
        string wallet_id FK "钱包ID"
        string party_id PK "参与方ID"
        blob local_data "加密的密钥分片"
        datetime created_at "创建时间"
    }
    
    SESSION {
        string id PK "会话ID"
        string type "类型: keygen/sign/reshare"
        string wallet_id FK "钱包ID"
        string status "状态"
        array party_ids "参与方列表"
        int threshold "阈值"
        int current_round "当前轮次"
        string error "错误信息"
        datetime created_at "创建时间"
        datetime updated_at "更新时间"
    }
    
    WALLET ||--o{ KEYSHARE : has
    WALLET ||--o{ SESSION : has
```

---

## 使用说明

这些流程图使用 [Mermaid](https://mermaid.js.org/) 语法编写，可以在以下环境中渲染：

1. **GitHub/GitLab**: 直接在 Markdown 文件中显示
2. **VS Code**: 安装 Mermaid 预览插件
3. **在线编辑器**: [Mermaid Live Editor](https://mermaid.live/)
4. **文档工具**: Notion, Obsidian, Typora 等

如果您的环境不支持 Mermaid 渲染，请参考 `PROJECT_ARCHITECTURE.md` 中的 ASCII 流程图。
