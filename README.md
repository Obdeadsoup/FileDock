# FileDock

《网络系统实践》课程的网络文件系统项目。当前支持账户与 Bearer 会话、目录管理、文件上传、分页列表、搜索过滤、流式下载、重命名、移动、删除及后台磁盘清理、指定账号只读共享、个人目录增量同步接口、Go 命令行客户端单向拉取同步（`cmd/filedock`），以及旧文件显式认领。

> [!NOTE]
> 当前以本机 HTTP 为开发与验收环境。双向编辑冲突、收到的共享同步、后台轮询、断点续传与 GUI 留待后续轮次。

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

- `POST /api/v1/files` 返回 HTTP `201` 和包含 `id`、`name`、`parent_id`、`size`、`sha256`、`created_at` 的 JSON 元数据，文件所有者由当前会话决定。
- `GET /api/v1/files` 返回当前用户指定目录中的直接文件。省略 `parent_id` 时返回根目录文件。
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

## 目录管理与文件移动

所有接口沿用 `Authorization: Bearer <token>`。每个账号拥有独立的逻辑根目录，由 JSON 中的 `parent_id: null` 表示。查询时省略 `parent_id` 即选择根目录，指定目录时使用非空目录 ID。

| 接口 | 输入及成功结果 |
| --- | --- |
| `POST /api/v1/folders` | JSON `{"name":"资料","parent_id":null}`。可省略 `parent_id`。返回 `201` 和目录元数据 `id`、`name`、`parent_id`、`created_at`、`updated_at`。 |
| `GET /api/v1/folders` | 可选查询参数 `parent_id`、`limit`、`offset`。返回 `200` 和直接子目录数组。 |
| `PATCH /api/v1/folders/{id}` | JSON 中提供 `name`、`parent_id` 或两者。返回 `200` 和更新后的目录元数据。 |
| `DELETE /api/v1/folders/{id}` | 删除空目录，返回 `204`。有直接文件或子目录时返回 `409`。 |
| `POST /api/v1/files` | 原有 multipart `file` 字段，可通过查询参数 `parent_id` 指定上传目录。 |
| `GET /api/v1/files` | 可选查询参数 `parent_id`、`limit`、`offset`。返回 `200` 和直接文件数组。 |
| `GET /api/v1/files/search` | 可选查询参数 `q`（区分大小写字面子串，支持中文，`%`/`_` 按普通字符匹配）、`min_size`、`max_size`、`parent_id`、`limit`、`offset`。默认搜索当前用户所有目录，指定 `parent_id` 时仅搜索该目录的直接文件。返回 `200` 和文件数组。 |
| `PATCH /api/v1/files/{id}` | JSON 中提供 `name`、`parent_id` 或两者。返回 `200` 和更新后的文件元数据。原 ID 和下载 URL 继续有效；改名和移动不增加 `content_revision`。 |
| `PUT /api/v1/files/{id}/content` | 以流式请求体按原文件 ID 替换文件内容，必须携带从该文件 `GET`/`HEAD` 读取到的具体强 `ETag` 作为 `If-Match`（缺省 `428`、过期 `412`、无权/不存在 `404`、超限 `413`）。成功返回 `200`、新 `ETag` 与递增后的 `content_revision`。 |
| `DELETE /api/v1/files/{id}` | 删除当前用户拥有的文件元数据并登记后台磁盘清理，返回 `204`；随后文件从列表/搜索消失且下载地址返回 `404`。 |
| `POST /api/v1/shares` | JSON `{"resource_type":"file"\|"folder","resource_id":"...","recipient_username":"bob"}`。创建只读共享返回 `201`，重复创建返回已有授权（`200`）。 |
| `GET /api/v1/shares` | 必填查询参数 `direction=sent\|received`，可选 `limit`、`offset`。返回 `200` 和共享授权数组。 |
| `DELETE /api/v1/shares/{share_id}` | 仅共享所有者可撤销，返回 `204`。撤销对后续请求立即生效。 |
| `GET /api/v1/shares/{share_id}/folders` | 接收者浏览共享目录的直接子目录，支持可选 `parent_id`（须在共享子树内）与 `limit`、`offset`。 |
| `GET /api/v1/shares/{share_id}/files` | 接收者浏览共享目录的直接文件（支持 `parent_id`、`limit`、`offset`）或查看单文件共享条目。 |
| `GET /api/v1/shares/{share_id}/files/{file_id}/content` | 接收者按 `share_id` 流式下载单独共享的文件或共享目录子树内的文件（不暴露外部父目录和 `object_key`）。 |
| `GET /api/v1/sync/changes` | 可选查询参数 `after`（排他游标，默认 `0`）、`limit`（默认 `50`，最大 `100`）。按当前用户独立递增 `seq` 升序返回 `items`、`next_cursor`、`has_more`。 |
| `GET /api/v1/auth/me` | 需有效 Bearer 令牌。返回当前会话用户 `id`、`username`、`created_at`，供同步客户端校验本地状态绑定。 |

两个列表都按创建时间降序、ID 升序排列。默认 `limit=50`、`offset=0`，每页最多 100 条，空结果为 `[]`。

文件和目录的名称都必须是单个名称，长度最多 255 个 UTF-8 字节。禁止空名称、全空白、`.`、`..`、斜杠、反斜杠以及 ASCII 控制字符。保留输入名称的大小写和空格。同一用户在同一父目录下的目录名须唯一，根目录也适用，比较时区分大小写。同名文件可以并存。文件和目录均按 ID 操作。

PATCH 省略 `parent_id` 时保留位置，显式传 `null` 时移回根目录。名称与位置同时提交时原子更新。源或目标不存在或属于其他用户时返回 `404`。目录重名、移入自身或后代目录均返回 `409`。

以下命令使用 Alice 的有效会话和上传示例中的 `FILE_ID`，将文件移动到两级目录并重命名。若已执行退出示例，先重新登录取得 `TOKEN_A`。

```sh
BASE=http://127.0.0.1:8080/api/v1
ROOT_ID=$(curl -fsS -H "Authorization: Bearer $TOKEN_A" -H "Content-Type: application/json" \
  -d '{"name":"course"}' "$BASE/folders" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')
CHILD_ID=$(curl -fsS -H "Authorization: Bearer $TOKEN_A" -H "Content-Type: application/json" \
  -d "{\"name\":\"lab\",\"parent_id\":\"$ROOT_ID\"}" "$BASE/folders" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')
curl -fsS -X PATCH -H "Authorization: Bearer $TOKEN_A" -H "Content-Type: application/json" \
  -d "{\"name\":\"lab-notes.txt\",\"parent_id\":\"$CHILD_ID\"}" "$BASE/files/$FILE_ID"
curl -fsS -H "Authorization: Bearer $TOKEN_A" "$BASE/files?parent_id=$CHILD_ID"
curl -fsS -H "Authorization: Bearer $TOKEN_A" -o /tmp/filedock-moved.txt "$BASE/files/$FILE_ID/content"
shasum -a 256 /tmp/filedock-demo.txt /tmp/filedock-moved.txt
```

## 文件搜索与删除演示

以下命令演示创建目录、上传带中文和 `%`/`_` 的文件、按子串与大小搜索、删除文件以及删除变空的目录：

```sh
BASE=http://127.0.0.1:8080/api/v1
DIR_ID=$(curl -fsS -H "Authorization: Bearer $TOKEN_A" -H "Content-Type: application/json" \
  -d '{"name":"实验目录"}' "$BASE/folders" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')

printf 'round 5 search & delete payload\n' > /tmp/filedock-search.txt
SEARCH_FILE_ID=$(curl -fsS -H "Authorization: Bearer $TOKEN_A" \
  -F "file=@/tmp/filedock-search.txt;filename=实验报告_100%.txt" \
  "$BASE/files?parent_id=$DIR_ID" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')

# 按中文与字面量 %/_ 及大小范围搜索（默认覆盖当前用户所有目录，也可附加 parent_id）
curl -fsS -G -H "Authorization: Bearer $TOKEN_A" \
  --data-urlencode "q=实验报告_100%" \
  --data-urlencode "min_size=10" \
  --data-urlencode "max_size=100" \
  --data-urlencode "parent_id=$DIR_ID" \
  "$BASE/files/search"

# 删除文件（返回 204）后，下载地址返回 404，空目录即可删除（返回 204）
curl -i -X DELETE -H "Authorization: Bearer $TOKEN_A" "$BASE/files/$SEARCH_FILE_ID"
curl -i -H "Authorization: Bearer $TOKEN_A" "$BASE/files/$SEARCH_FILE_ID/content"
curl -i -X DELETE -H "Authorization: Bearer $TOKEN_A" "$BASE/folders/$DIR_ID"
```

## 指定账号只读共享演示

以下命令演示 Alice 将目录共享给 Bob，Bob 列出收到的共享、浏览共享目录并下载文件，以及 Alice 撤销共享后 Bob 再次访问返回 `404`：

```sh
BASE=http://127.0.0.1:8080/api/v1
SHARE_DIR_ID=$(curl -fsS -H "Authorization: Bearer $TOKEN_A" -H "Content-Type: application/json" \
  -d '{"name":"shared-docs"}' "$BASE/folders" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')
printf 'shared content for bob\n' > /tmp/filedock-shared.txt
SHARED_FILE_ID=$(curl -fsS -H "Authorization: Bearer $TOKEN_A" \
  -F "file=@/tmp/filedock-shared.txt;filename=guide.txt" \
  "$BASE/files?parent_id=$SHARE_DIR_ID" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')

# Alice 创建目录共享给 Bob
SHARE_ID=$(curl -fsS -H "Authorization: Bearer $TOKEN_A" -H "Content-Type: application/json" \
  -d "{\"resource_type\":\"folder\",\"resource_id\":\"$SHARE_DIR_ID\",\"recipient_username\":\"bob\"}" \
  "$BASE/shares" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')

# Bob 查看收到的共享、浏览目录并下载文件
curl -fsS -H "Authorization: Bearer $TOKEN_B" "$BASE/shares?direction=received"
curl -fsS -H "Authorization: Bearer $TOKEN_B" "$BASE/shares/$SHARE_ID/files"
curl -fsS -H "Authorization: Bearer $TOKEN_B" -o /tmp/filedock-bob-dl.txt \
  "$BASE/shares/$SHARE_ID/files/$SHARED_FILE_ID/content"
shasum -a 256 /tmp/filedock-shared.txt /tmp/filedock-bob-dl.txt

# Alice 撤销共享（204），随后 Bob 再次访问返回 404
curl -i -X DELETE -H "Authorization: Bearer $TOKEN_A" "$BASE/shares/$SHARE_ID"
curl -i -H "Authorization: Bearer $TOKEN_B" "$BASE/shares/$SHARE_ID/files/$SHARED_FILE_ID/content"
```

## 个人目录增量同步演示

以下命令演示记录旧游标后执行目录创建、上传、移动与删除，再从旧游标按页拉取增量事件（支持相同游标重读）：

```sh
BASE=http://127.0.0.1:8080/api/v1

# 1. 记录当前游标
CURSOR=$(curl -fsS -H "Authorization: Bearer $TOKEN_A" "$BASE/sync/changes?after=0&limit=100" \
  | python3 -c 'import json,sys; print(json.load(sys.stdin)["next_cursor"])')

# 2. 创建目录、上传文件、移动文件回根目录并删除原目录与文件
SYNC_DIR=$(curl -fsS -H "Authorization: Bearer $TOKEN_A" -H "Content-Type: application/json" \
  -d '{"name":"sync-box"}' "$BASE/folders" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')
printf 'incremental sync payload\n' > /tmp/filedock-sync.txt
SYNC_FILE=$(curl -fsS -H "Authorization: Bearer $TOKEN_A" \
  -F "file=@/tmp/filedock-sync.txt;filename=sync.txt" \
  "$BASE/files?parent_id=$SYNC_DIR" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')
curl -fsS -X PATCH -H "Authorization: Bearer $TOKEN_A" -H "Content-Type: application/json" \
  -d '{"name":"sync-moved.txt","parent_id":null}' "$BASE/files/$SYNC_FILE" >/dev/null
curl -fsS -X DELETE -H "Authorization: Bearer $TOKEN_A" "$BASE/folders/$SYNC_DIR"
curl -fsS -X DELETE -H "Authorization: Bearer $TOKEN_A" "$BASE/files/$SYNC_FILE"

# 3. 从旧游标分批拉取变更流（limit=2）
PAGE1=$(curl -fsS -H "Authorization: Bearer $TOKEN_A" "$BASE/sync/changes?after=$CURSOR&limit=2")
echo "$PAGE1"
NEXT=$(printf '%s' "$PAGE1" | python3 -c 'import json,sys; print(json.load(sys.stdin)["next_cursor"])')
curl -fsS -H "Authorization: Bearer $TOKEN_A" "$BASE/sync/changes?after=$NEXT&limit=50"
```

## 命令行客户端单向拉取同步（`filedock sync pull`）

`cmd/filedock` 提供单向拉取同步命令，每次运行将当前账号的服务端个人目录增量镜像到本地目录后退出：

```sh
FILEDOCK_TOKEN="$TOKEN_A" go run ./cmd/filedock sync pull --server http://127.0.0.1:8080 --dir /tmp/mirror-a
```

### 首次运行与本地状态规则

- **首次接管空目录**：目标目录不存在时自动创建；若目录非空且尚未初始化 `.filedock/state.json`，客户端直接报错退出，不接管已有文件。首次拉取在修改镜像目录前即先持久化与 `server_url`、`user_id` 绑定的初始状态（`cursor = 0`），因此即使首次发布文件后中途崩溃，重跑也可安全恢复收敛。
- **状态绑定与安全**：客户端在镜像目录内通过 `os.Root` 受限根目录操作所有文件，将 `server_url`、`user_id`（来自 `GET /api/v1/auth/me`）、`cursor` 与按资源 ID 索引的清单原子写入 `.filedock/state.json`；**Bearer 令牌仅从环境变量 `FILEDOCK_TOKEN` 读取，不写入状态文件或日志**。服务端地址或用户不匹配时明确报错。
- **本地命名映射规则**：
  - 同一父目录下名称唯一（按大小写折叠比较）且不与 `.filedock` 冲突的条目，保留原始名称。
  - 若同一父目录下存在同名文件、仅大小写不同的条目（如 `Case.txt` 与 `case.txt`）或根目录下的 `.filedock`，每个冲突条目都会按其稳定资源 ID 的 SHA-256 前 8 位十六进制追加确定性后缀：目录为 `<name>__<tag>`，文件插入在最后扩展名前（如 `dup.txt` -> `dup__<tag>.txt`，无扩展名或以点开头无扩展名则追加到末尾）。若追加后仍与同级字面名称冲突，则升级为 16 位后缀。
- **保护与恢复**：仅管理清单已知或本次明确创建的条目，保留陌生文件；若发现目标路径被陌生文件占用或已托管文件被手动修改，报告冲突并停止。下载先写入 `.filedock/tmp/` 并校验 SHA-256 后发布，本地全部应用完成后再原子提交 `.filedock/state.json`；中途失败可安全重跑收敛。

### 双目录追赶验收演示

启动服务端后，在另一终端运行以下命令，演示同一账号的两个独立镜像目录从不同游标追赶并收敛到一致的目录与文件哈希：

```sh
BASE=http://127.0.0.1:8080/api/v1
rm -rf /tmp/mirror-a /tmp/mirror-b

# 1. 创建目录与文件（含同名文件），先同步到 mirror-a
DIR_ID=$(curl -fsS -H "Authorization: Bearer $TOKEN_A" -H "Content-Type: application/json" \
  -d '{"name":"project"}' "$BASE/folders" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')
printf 'v1 content\n' > /tmp/p1.txt
printf 'v2 content\n' > /tmp/p2.txt
F1=$(curl -fsS -H "Authorization: Bearer $TOKEN_A" -F "file=@/tmp/p1.txt;filename=readme.txt" "$BASE/files?parent_id=$DIR_ID" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')
F2=$(curl -fsS -H "Authorization: Bearer $TOKEN_A" -F "file=@/tmp/p2.txt;filename=readme.txt" "$BASE/files?parent_id=$DIR_ID" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')

FILEDOCK_TOKEN="$TOKEN_A" go run ./cmd/filedock sync pull --server http://127.0.0.1:8080 --dir /tmp/mirror-a

# 2. 在服务端移动、改名并删除其中一个文件，然后让 mirror-a 增量追赶、mirror-b 从游标 0 同步
curl -fsS -X PATCH -H "Authorization: Bearer $TOKEN_A" -H "Content-Type: application/json" \
  -d '{"name":"README-final.txt","parent_id":null}' "$BASE/files/$F1" >/dev/null
curl -fsS -X DELETE -H "Authorization: Bearer $TOKEN_A" "$BASE/files/$F2"

FILEDOCK_TOKEN="$TOKEN_A" go run ./cmd/filedock sync pull --server http://127.0.0.1:8080 --dir /tmp/mirror-a
FILEDOCK_TOKEN="$TOKEN_A" go run ./cmd/filedock sync pull --server http://127.0.0.1:8080 --dir /tmp/mirror-b

# 3. 核对两个镜像目录的文件列表与 SHA-256 完全一致
(cd /tmp/mirror-a && find . -path './.filedock' -prune -o -type f -print | sort | xargs shasum -a 256)
(cd /tmp/mirror-b && find . -path './.filedock' -prune -o -type f -print | sort | xargs shasum -a 256)
```

## 旧数据迁移与显式认领

从第二轮升级时，服务启动会在事务内自动为 `files` 表补齐 `owner_id` 列（可重复执行），保留已有文件 `id`、`object_key`、`sha256`、`created_at` 和磁盘对象。旧记录的 `owner_id` 初始为空字符串，对所有普通用户不可见；服务启动和用户注册均不会隐式认领旧文件。

目录迁移在同一个事务中创建 `folders` 表和索引，为 `files` 新增可空的 `parent_id` 列。迁移可重复执行，已归属的旧文件保留在其所有者的逻辑根目录。

要将尚未归属的旧文件认领给某个已注册用户，可执行显式本地维护命令：

```sh
FILEDOCK_DATA_DIR=./data go run ./cmd/filedockd claim-legacy --owner alice
```

命令会输出本次认领的旧文件数量（例如 `claimed 1 legacy file(s) for owner alice`），文件随后出现在 Alice 的根目录。重复执行时已归属文件不会被再次修改，报告处理数量为 `0`。

## 测试与构建

```sh
gofmt -w cmd internal
go mod tidy
go test ./...
go vet ./...
go build ./...
```

`go test ./...` 会执行单元测试、SQLite 迁移与多租户隔离集成测试，以及 `internal/archcheck` 生产代码依赖检查。开发前阅读 [AGENTS.md](AGENTS.md) 和 [ARCHITECTURE.md](ARCHITECTURE.md)。
