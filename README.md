# Sub-Store UI (Go 合并版)

基于 [beck-8/subs-check](https://github.com/beck-8/subs-check) 深度学习成果，将 subs-check 核心与 Sub-Store 管理面板合并为单一 Go 二进制，部署于飞牛 OS (fnOS) 的一体化应用。

## 架构概览

```
substore-ui-go/
├── main.go                    # 入口：Gin 引擎 + subs-check 主循环
├── go.mod / go.sum            # 依赖锁定（含 mihomo/gin/fsnotify/cron）
├── Makefile                   # 构建入口
├── scripts/
│   ├── build.sh               # 统一构建脚本
│   └── build.bat              # Windows 构建脚本
├── internal/
│   ├── config/config.go       # 环境变量配置
│   ├── model/model.go         # 数据结构
│   ├── middleware/
│   │   ├── middleware.go      # CSRF / API Key 鉴权 / CORS / 日志
│   │   ├── ratelimit.go       # 令牌桶限流
│   │   ├── circuitbreaker.go  # 断路器
│   │   └── session.go         # Cookie Session
│   ├── service/
│   │   ├── parser.go          # YAML 解析、订阅节点提取
│   │   └── proxy.go           # HTTP 代理、节点缓存
│   ├── handler/handler.go     # Sub-Store API 端点
│   ├── server/server.go       # 旧 HTTP 服务器（保留兼容）
│   └── subscheck/             # ← 从 subs-check 合并的核心代码
│       ├── app/               # 应用初始化、路由、处理器
│       ├── check/             # 节点检测管线（alive/speed/media）
│       ├── config/            # 配置结构与默认值
│       ├── save/              # 结果保存（local/webdav/gist/s3/r2）
│       ├── export/            # 订阅导出转换
│       ├── proxy/             # 订阅拉取与去重
│       ├── utils/             # 通知/回调/路径工具
│       ├── assets/            # 内嵌资源（node二进制+sub-store bundle）
│       └── monitor/           # 内存监控
├── public/                    # 前端静态资源
├── Dockerfile                 # Docker 构建
├── BUILDING.md                # 构建指南
└── README.md
```

## 核心特性

| 特性 | 说明 |
|------|------|
| **单二进制部署** | subs-check + Sub-Store 面板合二为一，无需独立进程 |
| **Gin 统一路由** | 所有 API 路由在同一 Gin Engine 上注册 |
| **内嵌资源** | Node.js 二进制 + Sub-Store bundle 通过 go:embed 打包 |
| **断网独立运行** | 离线节点检测、订阅转换、结果导出 |
| **多平台检测** | OpenAI/YouTube/Netflix/Disney/Gemini/IPRisk 等 |
| **多存储后端** | Local/WebDAV/GitHub Gist/S3/Cloudflare R2 |
| **令牌桶限流** | O(1) 复杂度，每 IP 独立桶 |
| **断路器** | 代理请求链自动熔断/恢复 |
| **CSRF 防护** | Token 校验覆盖写操作 |

## 合并架构说明

合并前：subs-check (端口 8199) + Sub-Store (端口 8299) + 管理面板 (端口 8300) = 3 个进程
合并后：**单进程监听 8300 端口**，内部直接调用 subs-check 核心函数

- 检测触发：`POST /api/trigger-check` → `app.TriggerCheck()` → `check.Check()`
- 结果读取：`GET /api/results` → 直接读 `results.json`
- 节点列表：`GET /api/nodes` → 读检测结果或代理 `/sub/all.yaml`
- 配置管理：`GET/POST /api/config` → 直接读写 `config.yaml`

## 构建

```bash
# 完整流程
bash scripts/build.sh all

# 或 Make
make all

# 跨平台
make cross
```

产物位于 `./dist/`。

## FPK 打包（飞牛 OS）

```bash
cd trim.substore-ui-go
bash build.sh  # 输出 dist/trim.substore-ui-go.fpk
```

FPK 以 Native 模式运行，监听 TCP 8300 端口，通过 iframe 嵌入飞牛桌面。

## Docker 部署

```bash
docker-compose up -d --build
```

## 环境变量

| 变量 | 默认 | 说明 |
|------|------|------|
| `PORT` | 8300 | 监听端口 |
| `API_KEY` | hy584521 | 面板访问密钥 |
| `SUBS_CONFIG_PATH` | config/config.yaml | subs-check 配置路径 |
| `PUBLIC_DIR` | ./public | 前端静态目录 |
| `SOCKET_PATH` | - | Unix socket 路径（Gateway 模式） |
| `GATEWAY_PREFIX` | - | 网关前缀（Gateway 模式） |

## 技术栈

- **后端**: Go 1.26+，Gin v1.12，mihomo v1.19
- **前端**: 原生 HTML/CSS/JS，无构建步骤
- **测试**: Go 标准 testing，全部通过
- **Lint**: golangci-lint v2，0 issues

## 分支说明

- `master` → 原始 subs-check 代码
- `refactor/go-optimization` → 合并版：subs-check + Sub-Store 面板一体化
