# MPC Wallet Node Dockerfile
FROM golang:1.21-alpine AS builder

# 安装构建依赖
RUN apk add --no-cache gcc musl-dev linux-headers git

WORKDIR /app

# 复制go mod文件
COPY go.mod go.sum ./
RUN go mod download

# 复制源代码
COPY cmd/ ./cmd/
COPY internal/ ./internal/
COPY pkg/ ./pkg/

# 构建
RUN CGO_ENABLED=1 GOOS=linux go build -a -installsuffix cgo -o mpc-node ./cmd/node

# 运行镜像
FROM alpine:3.18

RUN apk add --no-cache ca-certificates tzdata

WORKDIR /app

# 从构建阶段复制二进制文件
COPY --from=builder /app/mpc-node .

# 创建数据目录
RUN mkdir -p /app/data /app/configs

# 复制配置文件
COPY configs /app/configs/

# 设置环境变量
ENV DATA_DIR=/app/data
ENV CONFIG_FILE=/app/configs/node1.yaml

# 暴露端口
EXPOSE 4001 8081

# 启动命令
ENTRYPOINT ["./mpc-node"]
CMD ["-config", "/app/configs/node1.yaml"]
