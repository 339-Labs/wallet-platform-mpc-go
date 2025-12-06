#!/bin/bash

# MPC Wallet 集群停止脚本

# 颜色定义
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"
cd "$PROJECT_DIR"

if [ -f .cluster.pids ]; then
    PIDS=$(cat .cluster.pids)
    echo -e "${YELLOW}Stopping nodes with PIDs: $PIDS${NC}"
    kill $PIDS 2>/dev/null || true
    rm -f .cluster.pids
    echo -e "${GREEN}Cluster stopped${NC}"
else
    echo -e "${YELLOW}No running cluster found. Trying to kill by process name...${NC}"
    pkill -f "wallet-platform-mpc-go" 2>/dev/null || true
    echo -e "${GREEN}Done${NC}"
fi
