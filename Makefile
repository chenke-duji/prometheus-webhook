# prometheus-webhook build helper
# 目标：本机(go build)、Windows/Linux 交叉编译、测试、vet
#
# 用法：
#   make build                # 本机平台编译 -> bin/webhookd
#   make build-windows        # 交叉编译 Windows amd64 -> bin/webhookd-windows-amd64.exe
#   make build-linux-amd64    # 交叉编译 Linux x86_64 -> bin/webhookd-linux-amd64
#   make build-linux-arm64    # 交叉编译 Linux aarch64 -> bin/webhookd-linux-arm64
#   make build-linux          # 交叉编译 amd64 + arm64
#   make build-all            # Windows + Linux
#   make release              # 输出到 release/ 目录
#   make test                 # 运行全部测试
#   make vet                  # 静态检查
#   make clean                # 清理 bin/ release/
#
# 运行 bin/webhookd -v 可查看构建版本与日期。

GO      ?= go
BIN_DIR := bin
RELEASE_DIR := release

# 构建信息注入（-X main.buildVersion / main.buildDate）
VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
BUILD_DATE?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS   := -ldflags "-X main.buildVersion=$(VERSION) -X main.buildDate=$(BUILD_DATE)"

.PHONY: build build-windows build-linux-amd64 build-linux-arm64 build-linux build-all release test vet lint tidy coverage clean

build:
	$(GO) build $(LDFLAGS) -o $(BIN_DIR)/webhookd ./cmd/webhookd

# Windows 交叉编译（纯 Go，无 cgo）
build-windows:
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 $(GO) build $(LDFLAGS) -o $(BIN_DIR)/webhookd-windows-amd64.exe ./cmd/webhookd

# Linux 交叉编译
build-linux-amd64:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build $(LDFLAGS) -o $(BIN_DIR)/webhookd-linux-amd64 ./cmd/webhookd

build-linux-arm64:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) build $(LDFLAGS) -o $(BIN_DIR)/webhookd-linux-arm64 ./cmd/webhookd

build-linux: build-linux-amd64 build-linux-arm64

build-all: build-windows build-linux

# 输出到 release/ 目录
release: build-all
	@mkdir -p $(RELEASE_DIR)
	cp $(BIN_DIR)/webhookd-windows-amd64.exe $(RELEASE_DIR)/
	cp $(BIN_DIR)/webhookd-linux-amd64 $(RELEASE_DIR)/
	cp $(BIN_DIR)/webhookd-linux-arm64 $(RELEASE_DIR)/
	cp config.example.yaml $(RELEASE_DIR)/
	@echo "Release artifacts in $(RELEASE_DIR)/"

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

lint:
	@command -v golangci-lint >/dev/null 2>&1 || { echo "Installing golangci-lint..."; go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest; }
	golangci-lint run ./...

tidy:
	$(GO) mod tidy

coverage:
	$(GO) test -race -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out

clean:
	rm -rf $(BIN_DIR) $(RELEASE_DIR)
