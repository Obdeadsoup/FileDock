# FileDock

《网络系统实践》课程的网络文件系统项目。当前（第二轮）支持本机单用户下的文件上传、分页列表与流式下载，并将文件对象与 SQLite 元数据持久化到本地数据目录。

> [!NOTE]
> 当前版本**无账户与鉴权能力**，仅用于**本机开发与验收**；账户、目录管理、共享、同步与断点续传留待后续轮次。

## 启动服务

最低 Go 版本为 **1.27.0**。安装 Go 并确保 `go` 在 `PATH` 上，然后从项目根目录启动服务。

```sh
go run ./cmd/filedockd
```

常用环境变量：

- `FILEDOCK_ADDR`：监听地址，默认 `127.0.0.1:8080`（格式为 `主机:端口`，如 `127.0.0.1:9090` 或 `[::1]:9090`，端口 `0` 由系统分配）。
- `FILEDOCK_DATA_DIR`：本地数据目录，默认 `./data`（用于保存 `objects/`、`tmp/` 和 `filedock.db`，已通过 `.gitignore` 忽略，不提交到 Git）。

使用 `Ctrl+C` 停止服务，服务最多等待 5 秒完成正在处理的请求。

在 PowerShell 中覆盖监听地址与数据目录：

```powershell
$env:FILEDOCK_ADDR = '127.0.0.1:9090'
$env:FILEDOCK_DATA_DIR = './data'
go run ./cmd/filedockd
```

## API 与验收命令

下面命令在 macOS / Linux 终端（另开一个窗口）中可直接复制执行，验证健康检查、上传、列表、下载以及重启后按原 ID 下载与 SHA-256 校验。

### 1. 健康检查

```sh
curl -i http://127.0.0.1:8080/healthz
```

`GET /healthz` 返回 HTTP `200`、`Content-Type: application/json` 和 `{"status":"ok"}`。

### 2. 查询空列表

```sh
curl -i http://127.0.0.1:8080/api/v1/files
```

空列表返回 HTTP `200` 与 JSON 空数组 `[]`。列表默认按创建时间倒序、ID 升序稳定排序，支持 `?limit=50&offset=0`（`limit` 默认 `50`，上限 `100`）。

### 3. 上传文件

```sh
printf 'hello filedock\n' > /tmp/filedock-demo.txt
shasum -a 256 /tmp/filedock-demo.txt
curl -i -F "file=@/tmp/filedock-demo.txt" http://127.0.0.1:8080/api/v1/files
```

- `POST /api/v1/files` 返回 HTTP `201` 和包含 `id`、`name`、`size`、`sha256`、`created_at` 的 JSON 元数据。
- 同名文件再次上传会创建新记录并生成新的 `id`。
- 单文件上限默认 `64 MiB`，超限返回 `413`；非法请求返回 `400`；失败上传不会出现在列表里。

### 4. 查看列表并下载文件

将下面命令中的 `<FILE_ID>` 替换为上传返回的 `id`：

```sh
curl -s http://127.0.0.1:8080/api/v1/files
curl -i -o /tmp/filedock-download.txt http://127.0.0.1:8080/api/v1/files/<FILE_ID>/content
shasum -a 256 /tmp/filedock-download.txt
```

- `GET /api/v1/files/{id}/content` 以 `Content-Disposition: attachment; ...` 流式返回文件内容，下载后的 SHA-256 应与上传前一致。
- 未知 ID 返回 `404`。

### 5. 重启后再次下载验证

停止正在运行的 `filedockd`（`Ctrl+C`），使用相同的 `FILEDOCK_DATA_DIR` 重新启动：

```sh
FILEDOCK_DATA_DIR=./data go run ./cmd/filedockd
```

使用原 `<FILE_ID>` 再次下载并核对 SHA-256：

```sh
curl -fsS -o /tmp/filedock-restart.txt http://127.0.0.1:8080/api/v1/files/<FILE_ID>/content
shasum -a 256 /tmp/filedock-restart.txt
```

## 测试与构建

```sh
go test ./...
go vet ./...
go build ./...
```

`go test ./...` 会执行单元测试、集成测试和 `internal/archcheck` 生产代码依赖检查。开发前阅读 [AGENTS.md](AGENTS.md) 和 [ARCHITECTURE.md](ARCHITECTURE.md)。
