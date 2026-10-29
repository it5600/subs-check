# 构建指南 — BUILDING

> 本文件面向需要独立构建、部署或二次开发的智能体/开发者。
> 目标：拉取分支后零沟通完成构建。

---

## 1. 环境要求

| 依赖 | 版本 | 说明 |
|------|------|------|
| **Go** | >= 1.22 | 唯一硬依赖，无 CGO 要求 |
| **Docker** | 任意（可选） | 容器化部署时需要 |
| **Bash** | 任意（可选） | 使用 build.sh 时需要 |
| **Make** | 任意（可选） | 使用 Makefile 时需要 |

### 检查环境

```bash
go version          # 确认 >= 1.22
go env GOOS GOARCH  # 确认目标平台
```

### 安装 Go

- **Windows**: https://go.dev/dl/go1.22.5.windows-amd64.msi
- **Linux**: https://go.dev/dl/go1.22.5.linux-amd64.tar.gz
- **macOS**: https://go.dev/dl/go1.22.5.darwin-amd64.pkg

---

## 2. 快速开始（3 步）

```bash
# 1. 拉取分支
git clone -b refactor/go-optimization https://github.com/it5600/subs-check.git
cd subs-check

# 2. 运行测试（验证环境）
make test

# 3. 构建
make build
```

产物位于 `./dist/` 目录。

---

## 3. 构建命令一览

### 使用 Make（推荐）

```bash
make all          # 完整流程: precheck → test → build → verify
make test         # 仅运行测试
make build        # 仅构建当前平台
make cross        # 跨平台构建 (linux/windows/darwin)
make docker       # Docker 镜像构建
make clean        # 清理产物
```

### 使用 build.sh

```bash
# Linux / macOS / Windows-Git-Bash
bash scripts/build.sh all          # 完整流程
bash scripts/build.sh all-cross    # 跨平台构建
bash scripts/build.sh test         # 仅测试
bash scripts/build.sh clean        # 清理
```

### 使用 build.bat（Windows）

```bat
scripts\build.bat                  # 完整流程
scripts\build.bat v2.0.0           # 指定版本号
```

### 手动构建

```bash
# Windows
go build -ldflags "-s -w -X main.version=v1.1.0" -o dist/substore-ui-go.exe .

# Linux
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags "-s -w" -o dist/substore-ui-go-linux .

# macOS
GOOS=darwin GOARCH=amd64 CGO_ENABLED=0 go build -ldflags "-s -w" -o dist/substore-ui-go-darwin .
```

---

## 4. 产物结构

```
dist/
├── substore-ui-go-windows-amd64.exe   # Windows 二进制
├── substore-ui-go-linux-amd64         # Linux x86_64
├── substore-ui-go-linux-arm64         # Linux ARM64
├── substore-ui-go-darwin-amd64        # macOS Intel
├── substore-ui-go-darwin-arm64        # macOS Apple Silicon
└── (tar.gz / zip 压缩包)
```

### 产物验证

```bash
# 检查二进制有效性
bash scripts/build.sh verify

# 或手动检查文件头
hexdump -C dist/substore-ui-go-linux-amd64 | head -1
# 预期: 7f 45 4c 46  (ELF header)
```

---

## 5. 目录结构约定

```
substore-ui-go/
├── main.go                    # 程序入口
├── go.mod / go.sum            # Go 模块定义与依赖锁定
├── Makefile                   # 构建入口（兼容 make）
├── scripts/
│   ├── build.sh               # 统一构建脚本（bash）
│   └── build.bat              # Windows 构建脚本
├── internal/
│   ├── config/                # 配置管理
│   ├── model/                 # 数据结构
│   ├── middleware/            # 中间件（CSRF/限流/断路器/Session）
│   ├── service/               # 业务逻辑（YAML解析/HTTP代理）
│   ├── handler/               # HTTP 处理器
│   └── server/                # HTTP 服务器
├── public/                    # 前端静态文件（无需构建）
├── Dockerfile                 # Docker 构建
├── docker-compose.yml         # Docker Compose 配置
├── .golangci.yml              # Lint 配置
├── .gitignore                 # Git 忽略规则
├── BUILDING.md                # 本文件
└── README.md                  # 项目说明
```

### 重要约定

1. **源码在 `internal/`**：所有 Go 源码必须放在 `internal/` 下，遵循 Go 最佳实践
2. **前端无需构建**：`public/` 下的文件直接由 Go 服务托管，无 npm/webpack
3. **产物在 `dist/`**：所有构建产物统一输出到 `dist/`，不污染源码目录
4. **依赖锁定**：`go.sum` 必须提交，确保依赖版本一致
5. **二进制不提交**：`.gitignore` 已排除所有平台二进制文件

---

## 6. Docker 构建

```bash
# 构建镜像
make docker

# 或手动
docker build -t substore-ui-go:v1.1.0 -t substore-ui-go:latest .

# 运行容器
docker run -d \
  --name substore-ui-go \
  -p 8300:8300 \
  -e API_KEY=your-key-here \
  substore-ui-go:latest
```

### Docker Compose

```bash
docker-compose up -d
```

---

## 7. 环境变量

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `PORT` | 8300 | HTTP 监听端口 |
| `API_KEY` | - | 面板访问密钥（**必填**） |
| `CONFIG_PATH` | config.yaml | 配置文件路径 |
| `SUB_CHECK_URL` | - | Sub-Check API 地址 |
| `SUB_STORE_URL` | - | Sub-Store API 地址 |
| `CORS_ORIGINS` | - | 允许的跨域来源（逗号分隔） |

### 示例

```bash
# Linux/macOS
export PORT=8300
export API_KEY=your-secret-key
./dist/substore-ui-go-linux-amd64

# Windows
set PORT=8300
set API_KEY=your-secret-key
dist\substore-ui-go-windows-amd64.exe

# Docker
docker run -d -p 8300:8300 -e API_KEY=your-key substore-ui-go:latest
```

---

## 8. 常见问题

### Q: `go build` 报错 "undefined: xxx"

**A**: 依赖未下载。执行：
```bash
go mod download
# 或
go mod tidy
```

### Q: 构建出的二进制运行时报 "permission denied"

**A**: Linux/macOS 需要执行权限：
```bash
chmod +x dist/substore-ui-go-linux-amd64
```

### Q: Windows 防火墙阻止访问 8300

**A**: 添加防火墙规则：
```powershell
New-NetFirewallRule -DisplayName "SubStore UI" -Direction Inbound -LocalPort 8300 -Protocol TCP -Action Allow
```

### Q: 跨平台构建时 CGO 报错

**A**: 本项目不使用 CGO，确保构建时设置：
```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build ...
```

### Q: 如何指定版本号？

**A**: 通过 ldflags 注入：
```bash
go build -ldflags "-s -w -X main.version=v2.0.0" -o substore-ui-go .
```

或使用构建脚本：
```bash
VERSION=v2.0.0 make build
# 或
bash scripts/build.sh build
# （在脚本开头修改 VERSION 变量）
```

### Q: 测试失败怎么办？

**A**: 查看详细输出：
```bash
go test -v ./internal/...
```

常见问题：
- **网络超时**：测试可能涉及外部 HTTP 请求，确保网络可达
- **端口冲突**：8300 端口被占用，修改 `PORT` 环境变量

### Q: 如何贡献代码？

**A**: 
1. 从 `refactor/go-optimization` 分支创建 feature 分支
2. 遵循现有代码风格（`gofmt` / `goimports`）
3. 提交前运行 `make all` 确保测试通过
4. 提交信息格式：`feat: xxx` / `fix: xxx` / `refactor: xxx`

---

## 9. 验证清单

构建完成后，执行以下检查：

```bash
# 1. 二进制可执行
./dist/substore-ui-go-linux-amd64 --help
# 预期：显示帮助信息或正常启动

# 2. HTTP 服务可访问
curl http://localhost:8300/
# 预期：返回 HTML 页面

# 3. API 健康检查
curl http://localhost:8300/api/health
# 预期：返回 {"status":"ok"}

# 4. 测试全通过
make test
# 预期：所有测试 PASS
```

---

## 10. 版本管理

### 当前分支

- `refactor/go-optimization` — Go 重构版主分支

### 版本号规则

- 语义化版本：`MAJOR.MINOR.PATCH`
- 默认版本：`v1.1.0`
- 构建时通过 `-ldflags "-X main.version=xxx"` 注入

### 提交信息规范

```
feat: 新功能
fix: Bug 修复
refactor: 代码重构
perf: 性能优化
docs: 文档更新
test: 测试相关
chore: 构建/工具变更
```

---

## 11. 联系方式

- **GitHub**: https://github.com/it5600/subs-check
- **分支**: `refactor/go-optimization`
- **问题反馈**: 创建 Issue
