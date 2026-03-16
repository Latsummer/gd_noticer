# 项目名称
APP_NAME := gd_notice
# 入口路径
CMD_DIR := ./cmd/gd_notice
# 输出目录
BUILD_DIR := ./build
# 输出二进制
BINARY := $(BUILD_DIR)/$(APP_NAME)
# 配置文件
CONFIG := config.yaml

# Docker 相关
DOCKER_IMAGE := $(APP_NAME)
DOCKER_TAG ?= latest
DOCKER_CONTAINER := $(APP_NAME)

# Go 相关
GOCMD := go
GOBUILD := $(GOCMD) build
GOVET := $(GOCMD) vet
GOTEST := $(GOCMD) test
GOFMT := gofmt
GOIMPORTS := goimports
GOMOD := $(GOCMD) mod

# 版本信息（可选，预留）
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
BUILD_TIME := $(shell date '+%Y-%m-%d %H:%M:%S')
LDFLAGS := -s -w

.PHONY: all build run clean fmt vet test tidy help docker-build docker-run docker-stop docker-clean

## all: 默认目标，格式化 + 静态检查 + 构建
all: fmt vet build

## build: 编译二进制文件
build:
	@mkdir -p $(BUILD_DIR)
	$(GOBUILD) -ldflags "$(LDFLAGS)" -o $(BINARY) $(CMD_DIR)
	@echo "构建完成: $(BINARY)"

## run: 编译并运行服务
run: build
	$(BINARY) -config $(CONFIG)

## clean: 清理构建产物
clean:
	@rm -rf $(BUILD_DIR)
	@echo "清理完成"

## fmt: 格式化代码
fmt:
	$(GOFMT) -w .
	@echo "格式化完成"

## vet: 静态分析检查
vet:
	$(GOVET) ./...
	@echo "静态检查通过"

## test: 运行单元测试
test:
	$(GOTEST) -v -race ./...

## tidy: 整理依赖
tidy:
	$(GOMOD) tidy
	@echo "依赖整理完成"

## help: 显示帮助信息
help:
	@echo "可用命令:"
	@echo ""
	@grep -E '^## ' Makefile | sed 's/## /  /' | column -t -s ':'
	@echo ""

## docker-build: 构建 Docker 镜像
docker-build:
	docker build -t $(DOCKER_IMAGE):$(DOCKER_TAG) .
	@echo "Docker 镜像构建完成: $(DOCKER_IMAGE):$(DOCKER_TAG)"

## docker-run: 运行 Docker 容器
docker-run:
	docker run -d \
		--name $(DOCKER_CONTAINER) \
		--restart=always \
		-v $(PWD)/data:/app/data \
		$(DOCKER_IMAGE):$(DOCKER_TAG)
	@echo "Docker 容器已启动: $(DOCKER_CONTAINER)"

## docker-stop: 停止并移除 Docker 容器
docker-stop:
	docker stop $(DOCKER_CONTAINER) && docker rm $(DOCKER_CONTAINER)
	@echo "Docker 容器已停止并移除"

## docker-clean: 移除 Docker 镜像
docker-clean: docker-stop
	docker rmi $(DOCKER_IMAGE):$(DOCKER_TAG)
	@echo "Docker 镜像已移除"
