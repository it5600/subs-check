# 飞牛 FPK 打包骨架

本目录是 `trim.substore-ui-go`（subs-check + Sub-Store 合并版）的 **fnOS 原生应用打包骨架**。
源码在仓库根目录，**已构建好的安装包请走 Releases 下载**，见 [`../INSTALL.md`](../INSTALL.md)。

## 目录结构

```
fpk/
├── manifest                 应用元数据（appname / version / os_min_version / changelog）
├── build.sh                 构建脚本（调用官方 fnpack，生成双层嵌套 FPK）
├── cmd/                     生命周期脚本（main / install / upgrade / uninstall / config）
│   └── main                 启动入口：环境变量、Unix Socket、日志轮转
├── config/
│   ├── privilege            运行身份：对象格式 {"defaults":{"run-as":"package"}}
│   └── resource             data-share 声明（共享目录须含 appname 前缀）
├── wizard/                  安装/升级/配置/卸载向导，**必须是 JSON 数组**
├── app/ui/                  桌面入口配置与图标
├── app/native/www/          管理面板静态资源（与 internal/assets/www/ 同源）
├── scripts/verify_fpk.py    离线校验 FPK 包结构
├── scripts/make_icons.py    图标生成
└── ICON.PNG / ICON_256.PNG
```

> `app/native/substore-ui-go-linux`（Linux 二进制）、`dist/`、`.fpk` 均被 `.gitignore` 排除，
> 不入库。历史上曾误提交 111 MB 的 `*.newN` 二进制，导致 GitHub 拒收 push，请勿重蹈。

## 构建前置

| 依赖 | 说明 |
|---|---|
| Go（与 `go.mod` 一致） | 交叉编译 `CGO_ENABLED=0 GOOS=linux GOARCH=amd64` |
| fnpack 1.2.1 | 官方工具 `https://static2.fnnas.com/fnpack/fnpack-1.2.1-linux-amd64`，放到 `scripts/fnpack` 或加入 PATH；`build.sh` 会自动探测 |
| Python 3 | 运行 `scripts/verify_fpk.py` |

## 构建步骤（在仓库根目录执行）

```bash
# 1. 交叉编译 Linux 二进制，放到骨架的 app/native/
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -trimpath -ldflags "-s -w -X main.version=1.4.9" \
  -o fpk/app/native/substore-ui-go-linux .

# 2. 同步最新前端资源（前端源文件在 internal/assets/www/）
cp internal/assets/www/app.js     fpk/app/native/www/app.js
cp internal/assets/www/index.html fpk/app/native/www/index.html

# 3. 构建 FPK（产物 fpk/dist/trim.substore-ui-go.fpk）
cd fpk && bash build.sh

# 4. 离线校验结构
python scripts/verify_fpk.py dist/trim.substore-ui-go.fpk
```

打包产物结构：外层 `tar.gz` 含 `app.tgz` + `cmd/` + `config/` + `wizard/` + `manifest` + `ICON*`，
内层 `app.tgz` 为 `native/` + `ui/` 目录树。**不要**改成平铺结构，fnOS 会报「不是有效的 fpk 文件」。

## 不可违反的约定

- **网关前缀必须用连字符**：`/app/trim-substore-ui-go`。fnOS 校验规则要求前缀不含 `.`，
  而 appname 是 `trim.substore-ui-go`，点必须替换为连字符。
- **wizard 文件必须是 JSON 数组**，不是 shell 脚本。
- **privilege 必须是对象**：`{"defaults":{"run-as":"package"}}`，运行用户 `substoreui`。
- **data-share 目录名必须含 appname 前缀**：`trim.substore-ui-go/config`。
- **不要导出 `SUB_STORE_PATH`**：Go 二进制会自行解码内嵌的 `sub-store.bundle.js`；
  手动设置会覆盖正确路径，导致 `Cannot find module`。
- **不要占用 8199 / 8299**：那是旧 Docker 部署遗留；本应用内部使用 8198 / 8298 / 8300，
  且对外只走 Unix Socket 网关。

## 部署注意

fnOS 的 `install-fpk` **不会覆盖已存在的二进制**。升级已安装实例时必须：

```bash
appcenter-cli stop trim.substore-ui-go
cp -f <新二进制> /vol1/@appcenter/trim.substore-ui-go/native/substore-ui-go-linux
chown substoreui:substoreui /vol1/@appcenter/trim.substore-ui-go/native/substore-ui-go-linux
chmod 755 /vol1/@appcenter/trim.substore-ui-go/native/substore-ui-go-linux
appcenter-cli start trim.substore-ui-go
```

用 `md5sum` 验证内容，**不要看 mtime**（FPK 沿用 tar 内时间戳，mtime 不随版本变化）。
