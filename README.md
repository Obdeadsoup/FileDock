# FileDock

《网络系统实践》课程的网络文件系统项目。第一轮提供 HTTP 服务和自动化依赖检查。

## 启动服务

最低 Go 版本为 **1.27.0**。安装 Go 并确保 `go` 在 `PATH` 上，然后从项目根目录启动服务。

```sh
go run ./cmd/filedockd
```

默认监听 `127.0.0.1:8080`。设置 `FILEDOCK_ADDR` 可覆盖地址，格式为 `主机:端口`，例如 `127.0.0.1:9090` 或 `[::1]:9090`。端口 `0` 由系统分配，实际地址见启动日志。使用 `Ctrl+C` 停止服务，服务最多等待 5 秒完成正在处理的请求。

在 PowerShell 中可以这样覆盖监听地址。

```powershell
$env:FILEDOCK_ADDR = '127.0.0.1:9090'
go run ./cmd/filedockd
```

## 健康检查

另开一个终端请求健康检查。如果修改了监听地址，同时调整命令中的地址。

```powershell
curl.exe -i http://127.0.0.1:8080/healthz
```

macOS 或 Linux 上使用 `curl`。`GET /healthz` 应返回 HTTP 200、`Content-Type: application/json` 和 `{"status":"ok"}`。

## 测试与构建

```sh
go test ./...
go vet ./...
go build ./...
```

`go test ./...` 会执行 HTTP handler 测试和生产代码依赖检查。开发前阅读 [AGENTS.md](AGENTS.md) 和 [ARCHITECTURE.md](ARCHITECTURE.md)。
