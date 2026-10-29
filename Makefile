# ============================================================
# substore-ui-go — Makefile
# 标准构建接口，兼容所有平台（Linux/macOS/Windows-Git-Bash）
# ============================================================

VERSION     ?= v1.1.0
OUTPUT_DIR  ?= ./dist
LDFLAGS     := -s -w -X main.version=$(VERSION)

.PHONY: all precheck test build cross docker verify clean help

all: precheck test build verify

precheck:
	@echo "[INFO] 检查 Go 环境..."
	@command -v go >/dev/null 2>&1 || { echo "[ERR] 未找到 go，请安装 Go 1.22+"; exit 1; }
	@echo "[ OK ] Go 环境就绪"

test: precheck
	@echo "[INFO] 运行测试..."
	@go test -v -count=1 ./internal/...

build: precheck
	@echo "[INFO] 构建当前平台..."
	@mkdir -p $(OUTPUT_DIR)
	@OS=$$(go env GOOS); ARCH=$$(go env GOARCH); \
	if [ "$$OS" = "windows" ]; then EXT=".exe"; else EXT=""; fi; \
	go build -ldflags "$(LDFLAGS)" -o $(OUTPUT_DIR)/substore-ui-go-$$OS-$$ARCH$$EXT .
	@echo "[ OK ] 产物: $(OUTPUT_DIR)/substore-ui-go-$$OS-$$ARCH$$EXT"

cross: precheck
	@bash scripts/build.sh cross

docker:
	@echo "[INFO] Docker 构建..."
	@docker build -t "substore-ui-go:$(VERSION)" -t "substore-ui-go:latest" .

verify:
	@bash scripts/build.sh verify

clean:
	@echo "[INFO] 清理..."
	@rm -rf $(OUTPUT_DIR)
	@echo "[ OK ] 已清理"

help:
	@echo "用法: make [命令]"
	@echo ""
	@echo "命令:"
	@echo "  all        完整流程: precheck → test → build → verify"
	@echo "  precheck   前置检查"
	@echo "  test       运行测试"
	@echo "  build      构建当前平台二进制 → dist/"
	@echo "  cross      跨平台构建 (linux/windows/darwin)"
	@echo "  docker     Docker 镜像构建"
	@echo "  verify     验证构建产物"
	@echo "  clean      清理构建产物"
	@echo ""
	@echo "变量:"
	@echo "  VERSION=$(VERSION)  OUTPUT_DIR=$(OUTPUT_DIR)"
