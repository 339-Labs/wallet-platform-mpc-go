# Wallet Platform MPC Go Wallet Makefile

.PHONY: all build clean test run-node1 run-node2 run-node3 run-cluster stop-cluster docker-build docker-up docker-down lint fmt help

# 变量
BINARY_NAME=wallet-platform-mpc-go
GO=go
GOFLAGS=-v
BUILD_DIR=./build

# 默认目标
all: build

# 构建
build:
	@echo "Building $(BINARY_NAME)..."
	@mkdir -p $(BUILD_DIR)
	$(GO) build $(GOFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME) ./cmd/node
	@echo "Build complete: $(BUILD_DIR)/$(BINARY_NAME)"

# 快速构建（不带详细输出）
build-quick:
	@$(GO) build -o $(BUILD_DIR)/$(BINARY_NAME) ./cmd/node

# 清理
clean:
	@echo "Cleaning..."
	@rm -rf $(BUILD_DIR)
	@rm -f wallet-platform-mpc-go
	@rm -rf data/
	@rm -f .cluster.pids
	@echo "Clean complete"

# 测试
test:
	@echo "Running tests..."
	$(GO) test -v ./...

# 运行单个节点
run-node1: build-quick
	./$(BUILD_DIR)/$(BINARY_NAME) -config configs/node1.yaml

run-node2: build-quick
	./$(BUILD_DIR)/$(BINARY_NAME) -config configs/node2.yaml

run-node3: build-quick
	./$(BUILD_DIR)/$(BINARY_NAME) -config configs/node3.yaml

# 运行集群
run-cluster: build-quick
	@chmod +x scripts/start_cluster.sh
	@./scripts/start_cluster.sh

# 停止集群
stop-cluster:
	@chmod +x scripts/stop_cluster.sh
	@./scripts/stop_cluster.sh

# Docker构建
docker-build:
	@echo "Building Docker image..."
	docker build -t mpc:latest .

# Docker启动集群
docker-up:
	@echo "Starting Docker cluster..."
	docker-compose up -d

# Docker停止集群
docker-down:
	@echo "Stopping Docker cluster..."
	docker-compose down

# Docker日志
docker-logs:
	docker-compose logs -f

# 代码格式化
fmt:
	@echo "Formatting code..."
	$(GO) fmt ./...

# 代码检查
lint:
	@echo "Linting code..."
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run ./...; \
	else \
		echo "golangci-lint not installed, skipping..."; \
	fi

# 依赖管理
deps:
	@echo "Downloading dependencies..."
	$(GO) mod download
	$(GO) mod tidy

# 生成mock
mocks:
	@echo "Generating mocks..."
	@if command -v mockgen >/dev/null 2>&1; then \
		go generate ./...; \
	else \
		echo "mockgen not installed"; \
	fi

# 安装开发工具
install-tools:
	@echo "Installing development tools..."
	go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest
	go install github.com/golang/mock/mockgen@latest

# 帮助
help:
	@echo "MPC Wallet Build Commands:"
	@echo ""
	@echo "  make build        - Build the binary"
	@echo "  make clean        - Clean build artifacts"
	@echo "  make test         - Run tests"
	@echo "  make fmt          - Format code"
	@echo "  make lint         - Lint code"
	@echo "  make deps         - Download dependencies"
	@echo ""
	@echo "Running Nodes:"
	@echo "  make run-node1    - Run node 1"
	@echo "  make run-node2    - Run node 2"
	@echo "  make run-node3    - Run node 3"
	@echo "  make run-cluster  - Run all 3 nodes"
	@echo "  make stop-cluster - Stop all nodes"
	@echo ""
	@echo "Docker Commands:"
	@echo "  make docker-build - Build Docker image"
	@echo "  make docker-up    - Start Docker cluster"
	@echo "  make docker-down  - Stop Docker cluster"
	@echo "  make docker-logs  - View Docker logs"
	@echo ""
