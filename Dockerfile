# ====== 构建阶段 ======
FROM golang:1.23-alpine AS builder

# 安装构建依赖（如需 cgo 可添加 gcc musl-dev）
RUN apk add --no-cache ca-certificates tzdata

WORKDIR /src

# 先复制依赖文件，利用 Docker 缓存层加速构建
COPY go.mod go.sum ./
RUN go mod download

# 复制源码并编译
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /bin/gd_notice ./cmd/gd_notice/

# ====== 运行阶段 ======
FROM alpine:3.19

# 安装运行时必要依赖：CA 证书（HTTPS 请求）和时区数据
RUN apk add --no-cache ca-certificates tzdata

WORKDIR /app

# 从构建阶段复制编译好的二进制
COPY --from=builder /bin/gd_notice /app/gd_notice

# 复制默认配置文件（运行时可通过挂载覆盖）
COPY config.yaml /app/config.yaml

# 创建数据目录（用于持久化状态文件）
RUN mkdir -p /app/data

# 暴露 HTTP 健康检查端口（如配置中启用 http_server）
EXPOSE 18080

# 启动服务
ENTRYPOINT ["/app/gd_notice"]
CMD ["-config", "/app/config.yaml"]
