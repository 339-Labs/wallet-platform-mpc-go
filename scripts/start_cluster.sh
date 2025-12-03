#!/bin/bash

# MPC Wallet 集群启动脚本
# 启动3个节点的本地测试集群

set -e

# 颜色定义
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m'

# 项目根目录
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"
cd "$PROJECT_DIR"

echo -e "${BLUE}======================================${NC}"
echo -e "${BLUE}  MPC Wallet Cluster Startup Script  ${NC}"
echo -e "${BLUE}======================================${NC}"
echo ""

# 创建数据目录
echo -e "${YELLOW}Creating data directories...${NC}"
mkdir -p data/node1 data/node2 data/node3

# 构建项目
echo -e "${YELLOW}Building project...${NC}"
go build -o wallet-platform-mpc-go ./cmd/node
echo -e "${GREEN}Build successful!${NC}"
echo ""

# 启动节点1
echo -e "${GREEN}Starting Node 1...${NC}"
./wallet-platform-mpc-go -config configs/node1.yaml &
NODE1_PID=$!
echo "Node 1 PID: $NODE1_PID"
sleep 2

# 获取节点1的P2P地址
echo -e "${YELLOW}Waiting for Node 1 to start...${NC}"
sleep 3

# 启动节点2
echo -e "${GREEN}Starting Node 2...${NC}"
./wallet-platform-mpc-go -config configs/node2.yaml &
NODE2_PID=$!
echo "Node 2 PID: $NODE2_PID"
sleep 2

# 启动节点3
echo -e "${GREEN}Starting Node 3...${NC}"
./wallet-platform-mpc-go -config configs/node3.yaml &
NODE3_PID=$!
echo "Node 3 PID: $NODE3_PID"
sleep 2

echo ""
echo -e "${BLUE}======================================${NC}"
echo -e "${GREEN}Cluster started successfully!${NC}"
echo ""
echo "Node 1: http://localhost:8081 (P2P: 4001)"
echo "Node 2: http://localhost:8082 (P2P: 4002)"
echo "Node 3: http://localhost:8083 (P2P: 4003)"
echo ""
echo "PIDs: $NODE1_PID, $NODE2_PID, $NODE3_PID"
echo ""
echo -e "${YELLOW}Press Ctrl+C to stop all nodes${NC}"
echo -e "${BLUE}======================================${NC}"

# 保存PID到文件
echo "$NODE1_PID $NODE2_PID $NODE3_PID" > .cluster.pids

# 等待中断信号
trap "echo 'Stopping cluster...'; kill $NODE1_PID $NODE2_PID $NODE3_PID 2>/dev/null; rm -f .cluster.pids; exit 0" SIGINT SIGTERM

# 保持脚本运行
wait
