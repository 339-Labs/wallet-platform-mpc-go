#!/bin/bash

# MPC Wallet Node 启动脚本

set -e

# 颜色定义
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

# 默认配置
CONFIG_FILE=""
NODE_ID=""
P2P_PORT=""
API_PORT=""
DATA_DIR=""
LOG_LEVEL="info"

# 帮助信息
show_help() {
    echo "Usage: $0 [options]"
    echo ""
    echo "Options:"
    echo "  -c, --config      Config file path"
    echo "  -n, --node-id     Node ID"
    echo "  -p, --p2p-port    P2P port"
    echo "  -a, --api-port    API port"
    echo "  -d, --data-dir    Data directory"
    echo "  -l, --log-level   Log level (debug, info, warn, error)"
    echo "  -h, --help        Show this help"
    echo ""
    echo "Example:"
    echo "  $0 -c configs/node1.yaml"
    echo "  $0 -n node-1 -p 4001 -a 8081 -d ./data/node1"
}

# 解析参数
while [[ $# -gt 0 ]]; do
    case $1 in
        -c|--config)
            CONFIG_FILE="$2"
            shift 2
            ;;
        -n|--node-id)
            NODE_ID="$2"
            shift 2
            ;;
        -p|--p2p-port)
            P2P_PORT="$2"
            shift 2
            ;;
        -a|--api-port)
            API_PORT="$2"
            shift 2
            ;;
        -d|--data-dir)
            DATA_DIR="$2"
            shift 2
            ;;
        -l|--log-level)
            LOG_LEVEL="$2"
            shift 2
            ;;
        -h|--help)
            show_help
            exit 0
            ;;
        *)
            echo -e "${RED}Unknown option: $1${NC}"
            show_help
            exit 1
            ;;
    esac
done

# 构建命令参数
CMD="./wallet-platform-mpc-go"
ARGS=""

if [ -n "$CONFIG_FILE" ]; then
    ARGS="$ARGS -config $CONFIG_FILE"
fi

if [ -n "$NODE_ID" ]; then
    ARGS="$ARGS -node-id $NODE_ID"
fi

if [ -n "$P2P_PORT" ]; then
    ARGS="$ARGS -p2p-port $P2P_PORT"
fi

if [ -n "$API_PORT" ]; then
    ARGS="$ARGS -api-port $API_PORT"
fi

if [ -n "$DATA_DIR" ]; then
    ARGS="$ARGS -data-dir $DATA_DIR"
fi

if [ -n "$LOG_LEVEL" ]; then
    ARGS="$ARGS -log-level $LOG_LEVEL"
fi

# 检查二进制文件
if [ ! -f "$CMD" ]; then
    echo -e "${YELLOW}Binary not found, building...${NC}"
    go build -o wallet-platform-mpc-go ./cmd/node
fi

# 启动节点
echo -e "${GREEN}Starting MPC Wallet Node...${NC}"
echo -e "Command: $CMD $ARGS"
echo ""

exec $CMD $ARGS
