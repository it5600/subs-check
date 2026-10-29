# Sub-Store UI (Go 重构版)

基于 [beck-8/subs-check](https://github.com/beck-8/subs-check) 深度学习成果，使用 Go 语言从零重构的 Sub-Store 管理面板。

## 架构概览

```
substore-ui-go/
├── main.go                    # 入口
├── go.mod / go.sum
├── internal/
│   ├── config/config.go       # 环境变量配置
│   ├── model/model.go         # 数据结构
│   ├── middleware/
│   │   ├── middleware.go      # CSRF / API Key 鉴权 / CORS / 日志
│   │   ├── ratelimit.go       # 令牌桶限流 O(1)
│   │   ├── circuitbreaker.go  # 断路器
│   │   └── session.go         # Cookie Session
│   ├── service/
│   │   ├── parser.go          # YAML 解析、订阅节点提取
│   │   └── proxy.go           # HTTP 代理、节点缓存、ETag
│   ├── handler/handler.go     # API 端点、静态文件服务
│   └── server/server.go       # HTTP 服务器、中间件链
└── public/
    ├── index.html             # SPA 暗色主题（内联 CSS，无 CDN 依赖）
    └── app.js                 # 前端逻辑（6 页面：仪表盘/节点/订阅源/SubStore/日志/设置）
```

## 核心特性

| 特性 | 说明 |
|------|------|
| 纯 Go 实现 | 无 Node.js 运行时依赖，单二进制部署 |
| 内联 CSS | 前端无 CDN 依赖，国内可直接访问 |
| 令牌桶限流 | O(1) 复杂度，每 IP 独立桶 |
| 断路器 | 代理请求链自动熔断/恢复 |
| 节点缓存 | TTL 30s，返回副本防数据竞争 |
| 流式代理 | HTTP 连接池复用，支持 ETag |
| CSRF 防护 | Token 校验覆盖 GET/POST/PATCH |
| 零分配快照 | Stats 并发安全，struct 替代 map |

## 构建

```bash
# Windows
go build -o substore-ui-go.exe

# Linux
GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o substore-ui-go-linux
```

## Docker 部署

```bash
docker build -t substore-ui-go .
docker run -d --name substore-ui-go -p 8300:8300 substore-ui-go
```

## 环境变量

| 变量 | 默认 | 说明 |
|------|------|------|
| `PORT` | 8300 | 监听端口 |
| `API_KEY` | hy584521 | 面板访问密钥 |
| `CONFIG_PATH` | config.yaml | 配置文件路径 |
| `SUB_CHECK_URL` | - | Sub-Check API 地址 |
| `SUB_STORE_URL` | - | Sub-Store API 地址 |
| `CORS_ORIGINS` | - | 允许的跨域来源 |

## 技术栈

- **后端**: Go 1.22+，仅依赖 `gopkg.in/yaml.v3`
- **前端**: 原生 HTML/CSS/JS，无构建步骤
- **测试**: Go 标准 `testing` 包，38 项测试全通过

## 分支说明

- `master` → 原始 subs-check 代码（gin 框架）
- `refactor/go-optimization` → Go 重构版，零外部框架依赖
