# 在另一台飞牛 NAS 上安装（FPK 1.4.11）

本项目把 `subs-check`（订阅节点测活）与 `Sub-Store`（订阅转换）合并为**单个 Go 二进制**，
通过 fnOS 统一网关（Unix Socket）接入，无需开放安全入口。

> 只需要**已构建好的 `.fpk`**，不需要 clone 源码、不需要 Go 环境、不需要自己打包。
> 想从源码自行构建的，见 [`fpk/README.md`](fpk/README.md)。
> **新装一律用 1.4.11**；1.4.9 面板空白、1.4.10 已废弃。

## 1. 下载并校验

```bash
FPK_URL="https://github.com/it5600/subs-check/releases/download/fpk-v1.4.11/trim.substore-ui-go-1.4.11.fpk"
curl -fL --retry 2 -o /vol1/trim.substore-ui-go-1.4.11.fpk "$FPK_URL"

stat -c %s /vol1/trim.substore-ui-go-1.4.11.fpk
md5sum    /vol1/trim.substore-ui-go-1.4.11.fpk
sha256sum /vol1/trim.substore-ui-go-1.4.11.fpk
```

| 项 | 值 |
|---|---|
| 文件名 | `trim.substore-ui-go-1.4.11.fpk` |
| 大小 | `66922755` 字节（约 64 MB） |
| MD5 | `689a055811dfa1420276f5dfd4860178` |
| SHA256 | `335adf80592ac315f18e32fc02cc0f7022bb908bf6ea4641b87508af2970843d` |
| 内部二进制 MD5 | `e46a05176c4f8bbe1dce91a4667c9fbc` |
| appname | `trim.substore-ui-go` |
| 版本 | `1.4.11` |
| 平台 / 最低系统 | `x86` / fnOS `1.1.3100` |

**校验不通过立即停止安装**，不要用改名包或旧版本凑数。

## 2. 安装（按可用通道三选一）

### A. trim-cli（已配置 OAuth profile）

```bash
trim-cli --profile <profile> app install-fpk /vol1/trim.substore-ui-go-1.4.11.fpk \
  --volume-id <volumeId> --dry-run --yes          # 先预检，不启动安装任务

trim-cli --profile <profile> app install-fpk /vol1/trim.substore-ui-go-1.4.11.fpk \
  --volume-id <volumeId> \
  --custom-parameters '[{"key":"wizard_port","value":"8300"}]' --yes
```

- 写操作必须显式 `--yes`；`wizard_port` 是向导必填项，不传可能被要求转 UI 决策。
- 包已在 NAS 上时可用 `--remote-path /vol1/.../xxx.fpk`。
- 上传返回 `20001` = 目标存储卷不可用，先核实 `--volume-id` 存在、已挂载且健康。

### B. NAS 本机 SSH

```bash
appcenter-cli install-fpk --help     # 先确认本机 CLI 的实际参数
appcenter-cli install-fpk /vol1/trim.substore-ui-go-1.4.11.fpk
```

### C. 应用中心 Web UI（最稳兜底）

「应用中心 → 手动安装 FPK」→ 上传 `.fpk` → 安装向导「服务端口」填 `8300` → 确认安装。

> 若提示 `already installed`，说明该机已有旧版，见第 5 节升级步骤，**不要重复安装**。

## 3. 安装后必做

| 项 | 说明 |
|---|---|
| 配置目录 | `/vol1/@appshare/trim.substore-ui-go/config/`，首次启动自动生成 `config.yaml` |
| 访问入口 | `http(s)://<NAS>:5666/app/trim-substore-ui-go/`（网关前缀用连字符） |
| **默认 API Key** | `hy584521` —— **公开默认值，安装完成后立即修改**（写 `settings.conf` 的 `API_KEY=`，或 `config.yaml` 的 `api-key`）。1.4.11 起启动日志不再打印明文 Key |
| 运行身份 | `substoreui:substoreui`，不要 `chmod 777`，不要删除共享目录 |

## 4. 验证清单

```bash
# 1) 应用状态
appcenter-cli status trim.substore-ui-go          # 期望 version=1.4.11, status=running

# 2) 启动版本
grep '应用版本' /vol1/@appdata/trim.substore-ui-go/app.log | tail -1

# 3) 网关已注册（200/302 均可，404 = 前缀未注册）
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:5666/app/trim-substore-ui-go/

# 4) 内部 HTTP 探活（端口以 ss -lntp 实测为准，常见 8199 / 8198 / 8300）
PORT=$(ss -lntp | grep -oE ':81(98|99)|:8300' | head -1 | tr -d ':')
curl -s -H "X-API-Key: <你的API Key>" "http://127.0.0.1:${PORT}/api/health"
curl -s -H "X-API-Key: <你的API Key>" "http://127.0.0.1:${PORT}/api/status"
```

**1.4.11 的版本指纹**（确认跑的不是旧二进制）：

```bash
# 1) 不带凭据访问 dashboard，401 响应体必须同时含 code 与 error
curl -s -i "http://127.0.0.1:${PORT}/api/dashboard" | head -20
# 期望：HTTP/1.1 401 + {"code":401001,"message":"未授权","error":"无效的API密钥"}

# 2) go:embed 前端已是 1.4.11（改磁盘 www/ 无效，只有新二进制会返回这个标记）
curl -s "http://127.0.0.1:${PORT}/app.js" | grep -c "请求超时，请检查服务状态"   # 期望 1

# 3) 落盘二进制的 MD5
md5sum /vol1/@appcenter/trim.substore-ui-go/native/substore-ui-go-linux
# 期望 e46a05176c4f8bbe1dce91a4667c9fbc
```

`/api/status`（带 API Key）应含 `eta` / `elapsed` / `collectorPassed`（1.4.9 起提供，1.4.11 字段与类型不变）。

> 排障提醒：前端是 `go:embed` 编译进二进制的，改磁盘 `native/www/app.js` **不生效**；
> fnOS 托管的 nginx 配置会被自动重写，靠网关注入 `X-API-Key` 也**不生效**。
> 这两条路都不要试，直接换二进制。

## 5. 升级（已装 1.4.9 / 1.4.10）

fnOS 的 `install-fpk` **不会覆盖已存在的二进制**，只提示 `already installed`——
直接「手动安装 FPK」装完仍是旧二进制，1.4.9 的面板空白问题不会消失。
必须从 FPK 中解出新二进制强制覆盖：

```bash
PKG=/vol1/trim.substore-ui-go-1.4.11.fpk      # 已下载并校验
STAGE=/tmp/fpk1411
BIN=/vol1/@appcenter/trim.substore-ui-go/native/substore-ui-go-linux

# 外层 tar.gz 含 app.tgz；app.tgz 内路径是 native/substore-ui-go-linux（无 app/ 前缀）
rm -rf "$STAGE" && mkdir -p "$STAGE"
tar xzf "$PKG" -C "$STAGE" app.tgz
tar xzf "$STAGE/app.tgz" -C "$STAGE" native/substore-ui-go-linux
md5sum "$STAGE/native/substore-ui-go-linux"    # 期望 e46a05176c4f8bbe1dce91a4667c9fbc

appcenter-cli stop trim.substore-ui-go
cp -a "$BIN" "$BIN.bak-1.4.10"                 # 保留旧版回滚物
cp -f "$STAGE/native/substore-ui-go-linux" "$BIN"
chown substoreui:substoreui "$BIN"
chmod 755 "$BIN"
md5sum "$BIN"                                 # 必须与上面解出的 MD5 一致
appcenter-cli start trim.substore-ui-go
appcenter-cli status trim.substore-ui-go
```

用 `md5sum` 验证内容，**不要用 mtime 判断新旧**（FPK 沿用 tar 内的时间戳）。
升级后按第 4 节的三项指纹验证新二进制确实生效，浏览器硬刷新后应出现登录框。
若卸载被拒，改走应用中心 Web UI。

## 6. 回滚与已知陷阱

- 全新安装失败：应用中心卸载，保留 `.fpk` 与日志，**不要删除共享目录**。
- 升级失败：`stop` → 用 `.bak-1.4.10` 备份二进制覆盖回去 → `chown` → `start` → 复查状态。

| 陷阱 | 后果 |
|---|---|
| 网关前缀写成 `/app/trim.substore-ui-go`（含点） | fnOS 校验拒绝，入口 404 |
| 手动导出 `SUB_STORE_PATH` | 覆盖内嵌 `sub-store.bundle.js` 路径，启动报 `Cannot find module` |
| 用 `curl 8300` 判断安装成功 | 应用走 Unix Socket，8300 只是设置占位，会误判失败 |
| 把 `wizard/*` 写成 shell 脚本 | 向导解析失败，安装中断 |
| `data-share` 目录名不含 appname 前缀 | 配置文件落不到共享目录 |
| 以为改磁盘 `www/` 或 nginx 能修前端 | 前端是 go:embed，nginx 由 fnOS 托管自动重写，两条路都无效 |

## 7. 想自己构建？

见 [`fpk/README.md`](fpk/README.md)。需要 Go 交叉编译 + 官方 `fnpack`，
产物结构与校验方式都在里面。
