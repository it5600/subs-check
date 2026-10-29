#!/usr/bin/env bash
# ============================================================
# substore-ui-go — Unified Build Script
# 统一构建入口：本地开发 / 跨平台编译 / Docker / 测试验证
# ============================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"
cd "$PROJECT_DIR"

# ---------- 颜色 ----------
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
NC='\033[0m'

log_info()  { echo -e "${CYAN}[INFO]${NC}  $*"; }
log_ok()    { echo -e "${GREEN}[ OK ]${NC}  $*"; }
log_warn()  { echo -e "${YELLOW}[WARN]${NC}  $*"; }
log_err()   { echo -e "${RED}[ERR ]${NC}  $*"; }

# ---------- 版本 ----------
VERSION="${VERSION:-v1.1.0}"
LDFLAGS="-s -w -X main.version=${VERSION}"
OUTPUT_DIR="${OUTPUT_DIR:-./dist}"

# ---------- 前置检查 ----------
precheck() {
    log_info "检查 Go 环境..."
    if ! command -v go &>/dev/null; then
        log_err "未找到 go，请安装 Go 1.22+"
        log_err "  Windows: https://go.dev/dl/go1.22.5.windows-amd64.msi"
        log_err "  Linux:   https://go.dev/dl/go1.22.5.linux-amd64.tar.gz"
        exit 1
    fi

    local go_version
    go_version=$(go version | awk '{print $3}' | sed 's/go//')
    local major minor
    major=$(echo "$go_version" | cut -d. -f1)
    minor=$(echo "$go_version" | cut -d. -f2)

    if [ "$major" -lt 1 ] || { [ "$major" -eq 1 ] && [ "$minor" -lt 22 ]; }; then
        log_err "Go 版本 ${go_version} < 1.22，请升级"
        exit 1
    fi
    log_ok "Go ${go_version}"

    # 检查 go.mod 是否完整
    if [ ! -f go.mod ]; then
        log_err "go.mod 不存在，请在项目根目录执行"
        exit 1
    fi

    # 检查依赖完整性
    if ! go mod verify &>/dev/null; then
        log_warn "依赖校验失败，执行 go mod tidy..."
        go mod tidy
    fi
}

# ---------- 测试 ----------
run_tests() {
    log_info "运行测试..."
    go test -v -count=1 ./internal/... 2>&1 | tee /tmp/substore-test.log
    local test_exit=${PIPESTATUS[0]}
    if [ $test_exit -eq 0 ]; then
        local pass_count
        pass_count=$(grep -c "^=== RUN" /tmp/substore-test.log || true)
        log_ok "测试通过 (${pass_count} 项)"
    else
        log_err "测试失败，详见 /tmp/substore-test.log"
        exit 1
    fi
}

# ---------- 本地构建 ----------
build_local() {
    log_info "构建当前平台二进制..."
    mkdir -p "$OUTPUT_DIR"

    local os arch ext
    os=$(go env GOOS)
    arch=$(go env GOARCH)
    ext=""
    if [ "$os" = "windows" ]; then ext=".exe"; fi

    go build -ldflags "$LDFLAGS" -o "${OUTPUT_DIR}/substore-ui-go-${os}-${arch}${ext}" .
    log_ok "产物: ${OUTPUT_DIR}/substore-ui-go-${os}-${arch}${ext}"
}

# ---------- 跨平台构建 ----------
build_cross() {
    log_info "跨平台构建 (linux/windows/darwin)..."
    mkdir -p "$OUTPUT_DIR"

    local targets=(
        "linux   amd64 .tar.gz"
        "linux   arm64 .tar.gz"
        "windows amd64 .zip"
        "darwin  amd64 .tar.gz"
        "darwin  arm64 .tar.gz"
    )

    for target in "${targets[@]}"; do
        read -r os arch ext <<< "$target"
        log_info "  → ${os}/${arch}"

        local output="${OUTPUT_DIR}/substore-ui-go-${os}-${arch}"
        local archive="${output}${ext}"

        if [ "$os" = "windows" ]; then
            output="${output}.exe"
        fi

        GOOS=$os GOARCH=$arch CGO_ENABLED=0 \
            go build -ldflags "$LDFLAGS" -o "$output" .

        # 打包
        if [ "$ext" = ".tar.gz" ]; then
            tar czf "$archive" -C "$OUTPUT_DIR" "$(basename "$output")"
        elif [ "$ext" = ".zip" ]; then
            zip -q "$archive" "$output"
        fi

        log_ok "  产物: $archive"
    done

    log_ok "跨平台构建完成，产物位于 ${OUTPUT_DIR}/"
}

# ---------- Docker ----------
build_docker() {
    log_info "Docker 构建..."
    docker build -t "substore-ui-go:${VERSION}" -t "substore-ui-go:latest" .
    log_ok "镜像: substore-ui-go:${VERSION}"
}

# ---------- 验证 ----------
verify_build() {
    log_info "验证构建产物..."
    local binary="${OUTPUT_DIR}/substore-ui-go-$(go env GOOS)-$(go env GOARCH)"
    if [ "$(go env GOOS)" = "windows" ]; then
        binary="${binary}.exe"
    fi

    if [ ! -f "$binary" ]; then
        log_warn "产物不存在，跳过验证"
        return 0
    fi

    # 检查二进制有效性
    if [ "$(go env GOOS)" = "windows" ]; then
        # Windows 上检查 PE 头
        if ! head -c 2 "$binary" | grep -q "MZ"; then
            log_err "二进制文件头无效"
            exit 1
        fi
    else
        # Linux/macOS 上检查 ELF/Mach-O 头
        if ! head -c 4 "$binary" | grep -qE '\x7fELF|\xca\xfe\xba\xbe'; then
            log_err "二进制文件头无效"
            exit 1
        fi
    fi

    # 检查文件大小
    local size
    size=$(stat -c%s "$binary" 2>/dev/null || stat -f%z "$binary")
    if [ "$size" -lt 1000000 ]; then
        log_warn "二进制文件偏小 (${size} bytes)，可能构建不完整"
    else
        log_ok "二进制文件大小: ${size} bytes"
    fi

    log_ok "构建验证通过"
}

# ---------- 清理 ----------
clean() {
    log_info "清理构建产物..."
    rm -rf "$OUTPUT_DIR"
    rm -f /tmp/substore-test.log
    log_ok "已清理"
}

# ---------- 用法 ----------
usage() {
    cat << 'EOF'
用法: scripts/build.sh [命令]

命令:
  precheck     前置检查（Go 版本、依赖完整性）
  test         运行全部测试
  build        构建当前平台二进制 → dist/
  cross        跨平台构建 (linux/windows/darwin) → dist/
  docker       Docker 镜像构建
  verify       验证构建产物
  clean        清理构建产物
  all          完整流程: precheck → test → build → verify
  all-cross    完整流程: precheck → test → cross → verify

环境变量:
  VERSION      版本号（默认 v1.1.0）
  OUTPUT_DIR   产物目录（默认 ./dist）

示例:
  ./scripts/build.sh all          # 标准构建
  ./scripts/build.sh all-cross    # 跨平台构建
  ./scripts/build.sh test         # 仅测试
  ./scripts/build.sh clean        # 清理
EOF
}

# ---------- 主逻辑 ----------
CMD="${1:-help}"

case "$CMD" in
    precheck)   precheck ;;
    test)       precheck && run_tests ;;
    build)      precheck && build_local ;;
    cross)      precheck && build_cross ;;
    docker)     build_docker ;;
    verify)     verify_build ;;
    clean)      clean ;;
    all)        precheck && run_tests && build_local && verify_build ;;
    all-cross)  precheck && run_tests && build_cross ;;
    help|--help|-h) usage ;;
    *)          log_err "未知命令: $CMD"; echo; usage; exit 1 ;;
esac
