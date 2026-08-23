# remote-workspace-mcpd —— 构建与开发辅助
# 说明：本 Makefile 中的 MODULE 为占位符，发布前请替换为真实的 GitHub 仓库路径，
#       例如 github.com/<iamyounglee>/remote-workspace-mcp

MODULE        := github.com/iamyounglee/remote-workspace-mcp
BINARY       := remote-workspace-mcpd
PKG_VERSION  := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS      := -s -w -X $(MODULE)/internal/mcpserver.Version=$(PKG_VERSION)

# 关闭 CGO 以产出纯静态二进制（不依赖 libc/musl），便于跨平台分发与在
# Alpine 等仅含 musl 的镜像中直接运行。本项目未引入任何 C 依赖，
# 且 os/user、os/exec、net/http 在 CGO_ENABLED=0 下均有纯 Go 实现，可安全关闭。
CGO_ENABLED  ?= 0

GO           ?= go
GOFILES      := $(shell find . -type f -name '*.go' -not -path './vendor/*')

.PHONY: all
all: lint test build

.PHONY: build
build: ## 构建纯静态链接的二进制（CGO_ENABLED=0）
	CGO_ENABLED=$(CGO_ENABLED) $(GO) build -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/remote-workspace-mcpd

.PHONY: install
install: ## 安装到 GOPATH/bin（go install）
	CGO_ENABLED=$(CGO_ENABLED) $(GO) install -ldflags "$(LDFLAGS)" ./cmd/remote-workspace-mcpd

.PHONY: test
test: ## 运行测试（含竞态检测）
	$(GO) test -race -count=1 ./...

.PHONY: cover
cover: ## 生成并查看测试覆盖率
	$(GO) test -race -covermode=atomic -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out

.PHONY: lint
lint: ## 运行 golangci-lint（需先安装）
	golangci-lint run ./...

.PHONY: vet
vet: ## 运行 go vet
	$(GO) vet ./...

.PHONY: tidy
tidy: ## 整理依赖
	$(GO) mod tidy

.PHONY: run
run: build ## 构建并直接以默认配置启动
	./$(BINARY)

.PHONY: clean
clean: ## 清理构建产物
	rm -f $(BINARY) coverage.out
	rm -rf dist

.PHONY: help
help: ## 显示帮助
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'
