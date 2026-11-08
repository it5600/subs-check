#!/bin/bash
# substore-ui-go FPK 构建脚本（Native + fnOS 统一网关模式）
#
# 关键点：必须使用官方 fnpack 打包。官方 fpk 是「双层嵌套」结构：
#   外层 tar.gz  内含  app.tgz（app/ 目录的压缩包）+ cmd/ + config/ + manifest + wizard/ + ICON*
# 而非官方 tar 直接平铺打包，会导致 fnOS 报「不是有效的 fpk 文件」。

set -e

APP_NAME="trim.substore-ui-go"
APP_DIR="$(cd "$(dirname "$0")" && pwd)"
BUILD_DIR="${APP_DIR}/dist"
STAGING_DIR="${BUILD_DIR}/staging"

echo "============================================"
echo "  Sub-Store 管理面板 FPK 构建 (Native)"
echo "  应用名: $APP_NAME"
echo "  构建目录: $BUILD_DIR"
echo "============================================"

# ---------- 定位 fnpack ----------
FNPACK=""
if command -v fnpack >/dev/null 2>&1; then
    FNPACK="$(command -v fnpack)"
elif [ -x "${APP_DIR}/../_cache/fnpack/fnpack.exe" ]; then
    FNPACK="${APP_DIR}/../_cache/fnpack/fnpack.exe"
elif [ -x "${APP_DIR}/scripts/fnpack" ]; then
    FNPACK="${APP_DIR}/scripts/fnpack"
else
    echo "错误：未找到 fnpack 工具" >&2
    echo "请从 https://static2.fnnas.com/fnpack/fnpack-1.2.1-linux-amd64 下载后放入 scripts/ 或加入 PATH" >&2
    exit 1
fi
echo "使用 fnpack: ${FNPACK}"

# ---------- 清理旧构建 ----------
rm -rf "$BUILD_DIR"
mkdir -p "$BUILD_DIR" "$STAGING_DIR"

# ---------- 清理 macOS 元数据 ----------
# fnpack 会把 staging 中的 .DS_Store 原样带入产物，因此必须在复制前清理。
find "$APP_DIR" -name '.DS_Store' -type f -delete 2>/dev/null || true

# ---------- 准备 staging 目录 ----------
# 目录结构必须与 fnpack 校验规则一致：
#   manifest / config/{privilege,resource} / cmd/ / wizard/ / app/ / ICON*.PNG
mkdir -p "${STAGING_DIR}"/{app/ui/images,app/native/www,cmd,config,wizard}

cp "$APP_DIR/manifest"        "$STAGING_DIR/"
cp "$APP_DIR/LICENSE"         "$STAGING_DIR/"
cp "$APP_DIR/ICON.PNG"        "$STAGING_DIR/"
cp "$APP_DIR/ICON_256.PNG"    "$STAGING_DIR/"
cp "$APP_DIR/config/privilege" "$STAGING_DIR/config/"
cp "$APP_DIR/config/resource"  "$STAGING_DIR/config/"
cp "$APP_DIR/app/ui/config"    "$STAGING_DIR/app/ui/"
cp "$APP_DIR/app/ui/images/"*  "$STAGING_DIR/app/ui/images/" 2>/dev/null || true

# Native 应用文件（合并二进制 + 前端静态资源）
echo "=== 复制 Native 应用文件 ==="
cp "$APP_DIR/app/native/substore-ui-go-linux" "$STAGING_DIR/app/native/"
cp "$APP_DIR/app/native/www/"*                "$STAGING_DIR/app/native/www/"

# 生命周期脚本（全部需要可执行权限）
for f in main install_init install_callback uninstall_init uninstall_callback \
         upgrade_init upgrade_callback config_init config_callback; do
    cp "$APP_DIR/cmd/$f" "$STAGING_DIR/cmd/"
    chmod +x "$STAGING_DIR/cmd/$f"
done

# 向导配置
for f in install uninstall upgrade config; do
    cp "$APP_DIR/wizard/$f" "$STAGING_DIR/wizard/" 2>/dev/null || true
done

echo "=== 验证 staging 目录 ==="
find "$STAGING_DIR" -type f | sort
echo "文件总数: $(find "$STAGING_DIR" -type f | wc -l)"

# ---------- 打包 ----------
echo "=== 打包 FPK（官方 fnpack，生成嵌套结构）==="
cd "$STAGING_DIR"
"$FNPACK" build

FPK_PATH="${STAGING_DIR}/${APP_NAME}.fpk"
if [ ! -f "${FPK_PATH}" ]; then
    echo "错误：未生成 fpk 文件" >&2
    exit 1
fi

# 校验嵌套结构：外层必须含 app.tgz
echo "=== 校验 FPK 结构 ==="
if ! tar -tzf "${FPK_PATH}" | grep -q "^app.tgz"; then
    echo "错误：FPK 缺少 app.tgz（嵌套结构不正确）" >&2
    exit 1
fi
tar -tzf "${FPK_PATH}" | head -20

cp "${FPK_PATH}" "${BUILD_DIR}/"

FPK_SIZE=$(du -h "${BUILD_DIR}/${APP_NAME}.fpk" | cut -f1)
echo "============================================"
echo "  构建完成！"
echo "  FPK: ${BUILD_DIR}/${APP_NAME}.fpk"
echo "  大小: $FPK_SIZE"
echo "============================================"
