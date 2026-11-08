# 在另一台飞牛 NAS 上安装（FPK 1.4.9）

本项目把 `subs-check`（订阅节点测活）与 `Sub-Store`（订阅转换）合并为**单个 Go 二进制**，
通过 fnOS 统一网关（Unix Socket）接入，无需开放安全入口。

> 只需要**已构建好的 `.fpk`**，不需要 clone 源码、不需要 Go 环境、不需要自己打包。
> 想从源码自行构建的，见 [`fpk/README.md`](fpk/README.md)。

## 1. 下载并校验

```bash
FPK_URL="https://github.com/it5600/subs-check/releases/download/fpk-v1.4.9/trim.substore-ui-go-1.4.9.fpk"
curl -fL --retry 2 -o /vol1/trim.substore-ui-go-1.4.9.fpk "$FPK_URL"

stat -c %s /vol1/trim.substore-ui-go-1.4.9.fpk
md5sum    /vol1/trim.substore-ui-go-1.4.9.fpk
sha256sum /vol1/trim.substore-ui-go-1.4.9.fpk
```

| 项 | 值 |
|---|---|
| 文件名 | `trim.substore-ui-go-1.4.9.fpk` |
| 大小 | `66922590` 字节（约 64 MB） |
| MD5 | `75e6c3494330e910a864d914b1572a30` |
| SHA256 | `ca4259ffff6e2cc1a709d4a44bc0413ee8025504d52198fea88e42acc2fb43fa` |
| appname | `trim.substore-ui-go` |
| 版本 | `1.4.9` |
| 平台 / 最低系统 | `x86` / fnOS `1.1.3100` |

**校验不通过立即停止安装**，不要用改名包或旧版本凑数。

## 2. 安装（按可用通道三选一）

### A. trim-cli（已配置 OAuth profile）

```bash
trim-cli --profile <profile> app install-fpk /vol1/trim.substore-ui-go-1.4.9.fpk \
  --volume-id <volumeId> --dry-run --yes          # 先预检，不启动安装任务

trim-cli --profile <profile> app install-fpk /vol1/trim.substore-ui-go-1.4.9.fpk \
  --volume-id <volumeId> \
  --custom-parameters '[{"key":"wizard_port","value":"8300"}]' --yes
```

- 写操作必须显式 `--yes`；`wizard_port` 是向导必填项，不传可能被要求转 UI 决策。
- 包已在 NAS 上时可用 `--remote-path /vol1/.../xxx.fpk`。
- 上传返回 `20001` = 目标存储卷不可用，先核实 `--volume-id` 存在、已挂载且健康。

### B. NAS 本机 SSH

```bash
appcenter-cli install-fpk --help     # 先确认本机 CLI 的实际参数
appcenter-cli install-fpk /vol1/trim.substore-ui-go-1.4.9.fpk
```

### C. 应用中心 Web UI（最稳兜底）

「应用中心 → 手动安装 FPK」→ 上传 `.fpk` → 安装向导「服务端口」填 `8300` → 确认安装。

> 若提示 `already installed`，说明该机已有旧版，见第 5 节升级步骤，**不要重复安装**。

## 3. 安装后必做

| 项 | 说明 |
|---|---|
| 配置目录 | `/vol1/@appshare/trim.substore-ui-go/config/`，首次启动自动生成 `config.yaml` |
| 访问入口 | `http(s)://<NAS>:5666/app/trim-substore-ui-go/`（网关前缀用连字符） |
| **默认 API Key** | `hy584521` —— **公开默认值，安装完成后立即修改**（写 `settings.conf` 的 `API_KEY=`，或 `config.yaml` 的 `api-key`） |
| 运行身份 | `substoreui:substoreui`，不要 `chmod 777`，不要删除共享目录 |

## 4. 验证清单

```bash
# 1) 应用状态
appcenter-cli status trim.substore-ui-go          # 期望 version=1.4.9, status=running

# 2) 启动版本
grep '应用版本' /vol1/@appdata/trim.substore-ui-go/app.log | tail -1

# 3) 网关已注册（200/302 均可，404 = 前缀未注册）
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:5666/app/trim-substore-ui-go/

# 4) 内部 HTTP 探活（端口以 ss -lntp 实测为准，常见 8199 / 8198 / 8300）
PORT=$(ss -lntp | grep -oE ':81(98|99)|:8300' | head -1 | tr -d ':')
curl -s -H "X-API-Key: <你的API Key>" "http://127.0.0.1:${PORT}/api/health"
curl -s -H "X-API-Key: <你的API Key>" "http://127.0.0.1:${PORT}/api/status"
```

`/api/status` 必须包含 `eta` / `elapsed` / `collectorPassed` 字段——这是 **1.4.9 独有的版本指纹**，
可用来确认跑的不是旧二进制。另可比对
`md5sum /vol1/@appcenter/trim.substore-ui-go/native/substore-ui-go-linux`
与 FPK 内 `app.tgz` 解出的同名文件。

## 5. 升级（已装旧版）

fnOS 的 `install-fpk` **不会覆盖已存在的二进制**，只提示 `already installed`：

```bash
appcenter-cli stop trim.substore-ui-go
cp /vol1/@appcenter/trim.substore-ui-go/native/substore-ui-go-linux{,.bak}   # 先备份
cp -f <新二进制> /vol1/@appcenter/trim.substore-ui-go/native/substore-ui-go-linux
chown substoreui:substoreui /vol1/@appcenter/trim.substore-ui-go/native/substore-ui-go-linux
chmod 755 /vol1/@appcenter/trim.substore-ui-go/native/substore-ui-go-linux
appcenter-cli start trim.substore-ui-go
```

用 `md5sum` 验证内容，**不要用 mtime 判断新旧**（FPK 沿用 tar 内的时间戳）。
若卸载被拒，改走应用中心 Web UI。

## 6. 回滚与已知陷阱

- 全新安装失败：应用中心卸载，保留 `.fpk` 与日志，**不要删除共享目录**。
- 升级失败：`stop` → 用备份二进制覆盖回去 → `chown` → `start` → 复查状态。

| 陷阱 | 后果 |
|---|---|
| 网关前缀写成 `/app/trim.substore-ui-go`（含点） | fnOS 校验拒绝，入口 404 |
| 手动导出 `SUB_STORE_PATH` | 覆盖内嵌 `sub-store.bundle.js` 路径，启动报 `Cannot find module` |
| 用 `curl 8300` 判断安装成功 | 应用走 Unix Socket，8300 只是设置占位，会误判失败 |
| 把 `wizard/*` 写成 shell 脚本 | 向导解析失败，安装中断 |
| `data-share` 目录名不含 appname 前缀 | 配置文件落不到共享目录 |

## 7. 想自己构建？

见 [`fpk/README.md`](fpk/README.md)。需要 Go 交叉编译 + 官方 `fnpack`，
产物结构与校验方式都在里面。
