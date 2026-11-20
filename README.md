# FileDock

《网络系统实践》课程的网络文件系统项目。当前（第三轮）支持账户注册、登录、退出、Bearer 会话认证，以及按文件所有者隔离的文件上传、分页列表、流式下载与第二轮旧文件显式认领。

> [!NOTE]
> 当前以本机 HTTP 为开发与验收环境；目录树、共享、同步与断点续传留待后续轮次。

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

## API 与双账号验收命令

下面命令在 macOS / Linux 终端（另开一个窗口）中可直接复制执行，验证公开健康检查、双账号注册/登录、按所有者隔离的文件上传/列表/下载、退出撤销令牌以及重启持久化。

### 1. 健康检查（公开接口）

```sh
curl -i http://127.0.0.1:8080/healthz
```

`GET /healthz` 返回 HTTP `200`、`Content-Type: application/json` 和 `{"status":"ok"}`。未带有效会话访问 `/api/v1/files` 会返回 HTTP `401`。

### 2. 注册并登录两个账号（Alice 与 Bob）

```sh
curl -fsS -X POST http://127.0.0.1:8080/api/v1/auth/register \
  -H "Content-Type: application/json" \
  -d '{"username":"alice","password":"alice-password"}'

curl -fsS -X POST http://127.0.0.1:8080/api/v1/auth/register \
  -H "Content-Type: application/json" \
  -d '{"username":"bob","password":"bob-password"}'

TOKEN_A=$(curl -fsS -X POST http://127.0.0.1:8080/api/v1/auth/login \
  -H "Content-Type: application/json" \
  -d '{"username":"alice","password":"alice-password"}' \
  | python3 -c 'import json,sys; print(json.load(sys.stdin)["token"])')

TOKEN_B=$(curl -fsS -X POST http://127.0.0.1:8080/api/v1/auth/login \
  -H "Content-Type: application/json" \
  -d '{"username":"bob","password":"bob-password"}' \
  | python3 -c 'import json,sys; print(json.load(sys.stdin)["token"])')
```

- `POST /api/v1/auth/register`：用户名唯一（冲突返回 `409`），密码使用 Argon2id 与独立随机盐计算哈希，持久化盐、哈希与 Argon2id 参数。
- `POST /api/v1/auth/login`：成功返回随机生成的 Bearer `token` 与 `expires_at`，SQLite 仅保存令牌的 SHA-256 摘要；用户名不存在或密码错误均返回统一 `401` 响应。

### 3. Alice 上传、列表与下载文件

```sh
printf 'hello filedock round 3\n' > /tmp/filedock-demo.txt
shasum -a 256 /tmp/filedock-demo.txt

UPLOAD_RESP=$(curl -fsS -H "Authorization: Bearer $TOKEN_A" \
  -F "file=@/tmp/filedock-demo.txt" \
  http://127.0.0.1:8080/api/v1/files)
echo "$UPLOAD_RESP"

FILE_ID=$(printf '%s' "$UPLOAD_RESP" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')

curl -fsS -H "Authorization: Bearer $TOKEN_A" http://127.0.0.1:8080/api/v1/files

curl -fsS -H "Authorization: Bearer $TOKEN_A" \
  -o /tmp/filedock-download.txt \
  "http://127.0.0.1:8080/api/v1/files/$FILE_ID/content"
shasum -a 256 /tmp/filedock-download.txt
```

- `POST /api/v1/files` 返回 HTTP `201` 和包含 `id`、`name`、`size`、`sha256`、`created_at` 的 JSON 元数据，文件所有者由当前会话决定。
- `GET /api/v1/files` 仅返回当前登录用户拥有的文件列表（空列表返回 `[]`，支持 `?limit=50&offset=0`，上限 `100`）。
- `GET /api/v1/files/{id}/content` 先校验文件归属，再打开磁盘流并以 `Content-Disposition: attachment; ...` 返回；使用 `curl -fsS -o` 保存响应正文后，SHA-256 与原文件一致。

### 4. 验证 Bob 隔离与退出会话

```sh
# Bob 的列表不包含 Alice 的文件（返回 []）
curl -fsS -H "Authorization: Bearer $TOKEN_B" http://127.0.0.1:8080/api/v1/files

# Bob 访问 Alice 的文件 ID 返回 404
curl -i -H "Authorization: Bearer $TOKEN_B" "http://127.0.0.1:8080/api/v1/files/$FILE_ID/content"

# Alice 退出登录，当前令牌立即失效
curl -fsS -X POST -H "Authorization: Bearer $TOKEN_A" http://127.0.0.1:8080/api/v1/auth/logout

# 退出后再次携带旧令牌访问返回 401
curl -i -H "Authorization: Bearer $TOKEN_A" http://127.0.0.1:8080/api/v1/files
```

## 旧数据迁移与显式认领

从第二轮升级时，服务启动会在事务内自动为 `files` 表补齐 `owner_id` 列（可重复执行），保留已有文件 `id`、`object_key`、`sha256`、`created_at` 和磁盘对象。旧记录的 `owner_id` 初始为空字符串，对所有普通用户不可见；服务启动和用户注册均不会隐式认领旧文件。

要将尚未归属的旧文件认领给某个已注册用户，可执行显式本地维护命令：

```sh
FILEDOCK_DATA_DIR=./data go run ./cmd/filedockd claim-legacy --owner alice
```

命令会输出本次认领的旧文件数量（例如 `claimed 1 legacy file(s) for owner alice`）；重复执行时已归属文件不会被再次修改，报告处理数量为 `0`。

## 测试与构建

```sh
gofmt -w cmd internal
go mod tidy
go test ./...
go vet ./...
go build ./...
```

`go test ./...` 会执行单元测试、SQLite 迁移与多租户隔离集成测试，以及 `internal/archcheck` 生产代码依赖检查。开发前阅读 [AGENTS.md](AGENTS.md) 和 [ARCHITECTURE.md](ARCHITECTURE.md)。
