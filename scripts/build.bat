@echo off
setlocal enabledelayedexpansion

REM ============================================================
REM substore-ui-go — Windows Build Script
REM ============================================================

set VERSION=%1
if "%VERSION%"=="" set VERSION=v1.1.0
set OUTPUT_DIR=.\dist

echo [INFO] substore-ui-go Build - Version %VERSION%
echo.

REM Check Go
where go >nul 2>&1
if errorlevel 1 (
    echo [ERR] 未找到 go，请安装 Go 1.22+
    echo       Windows: https://go.dev/dl/go1.22.5.windows-amd64.msi
    exit /b 1
)

REM Check go.mod
if not exist go.mod (
    echo [ERR] go.mod 不存在
    exit /b 1
)

REM Run tests
echo [INFO] 运行测试...
go test -v -count=1 ./internal/...
if errorlevel 1 (
    echo [ERR] 测试失败
    exit /b 1
)
echo [ OK ] 测试通过
echo.

REM Build
echo [INFO] 构建 Windows 二进制...
if not exist %OUTPUT_DIR% mkdir %OUTPUT_DIR%

go build -ldflags "-s -w -X main.version=%VERSION%" -o %OUTPUT_DIR%\substore-ui-go-windows-amd64.exe .
if errorlevel 1 (
    echo [ERR] 构建失败
    exit /b 1
)

echo [ OK ] 产物: %OUTPUT_DIR%\substore-ui-go-windows-amd64.exe
echo.
echo 构建完成
endlocal
