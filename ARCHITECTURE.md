# FileDock 架构

FileDock 是单 Go module。HTTP 服务只使用标准库；第三方包目前仅允许 `internal/store/sqlite` 直接导入 `modernc.org/sqlite`，以及 `internal/accounts` 直接导入 `golang.org/x/crypto/argon2`。

| 包 | 职责 | 可依赖的模块内包 |
| --- | --- | --- |
| `cmd/filedockd` | 组装依赖，启动 HTTP 服务，执行本地维护子命令（如 `claim-legacy`），管理进程日志和关闭流程 | `internal/config`、`internal/httpapi`、已登记的业务包和存储适配器 |
| `internal/config` | 读取和校验进程配置 | 无 |
| `internal/httpapi` | 注册路由、解析 HTTP 请求与会话令牌、调用业务服务、转换 HTTP 响应 | 已登记的业务包（`internal/accounts`、`internal/files`） |
| `internal/accounts` | 账户注册、Argon2id 密码校验、Bearer 会话令牌签发、认证与撤销 | 无 |
| `internal/files` | 校验文件和目录名称、标识、搜索条件、共享参数、同步游标和分页，协调目录管理、文件移动、删除与后台清理、搜索、只读共享、增量同步、内容写入与下载 | 无 |
| `internal/store/sqlite` | 持久化账户、会话摘要、文件、目录元数据、共享授权、按所有者隔离的增量变更日志与磁盘清理任务，执行事务化迁移、树约束检查、搜索查询与旧文件认领 | `internal/accounts`、`internal/files` |
| `internal/store/localdisk` | 本地磁盘暂存、流式写入、计算 SHA-256 与打开内容流 | `internal/files` |
| `internal/archcheck` | 在 `go test ./...` 中检查生产代码的直接导入 | 规则实现位于测试文件 |

命令入口与架构测试包禁止被生产代码导入。HTTP 层禁止直接访问存储适配器、SQL、磁盘文件和进程配置。下载文件时，HTTP 层先认证会话并将用户 ID 传给 `internal/files`，从业务服务取得读取流后再写入响应。

## 后续业务和存储

- 按业务能力组织包。账户与会话放在 `internal/accounts`，文件、目录、只读共享与个人目录增量同步的权限判断、业务校验与状态变更放在 `internal/files`。客户端落盘冲突处理与断点续传在对应需求开始后确定落点。跨业务包依赖默认禁止（`internal/accounts` 与 `internal/files` 之间禁止互相导入），确需引入时写明具体允许的导入关系。
- 业务包禁止依赖 HTTP、SQL、具体磁盘实现和进程配置。需要元数据查询或文件内容读写时，在使用方定义小接口，由 `cmd/filedockd` 注入实现。
- SQLite 元数据适配器放在 `internal/store/sqlite`，本地磁盘内容适配器放在 `internal/store/localdisk`。适配器可导入实现接口需要的业务类型。业务包禁止反向依赖适配器。
- 新增包按完整包路径归类。未归类的包和未明确允许的模块内导入默认报错。

## 自动检查

架构测试通过 `go list` 读取当前构建配置下本模块生产包的直接导入。入口、配置与存储适配器可使用标准库。通用业务层可使用 `bytes`、`context`、`errors`、`fmt`、`io`、`path`、`slices`、`strconv`、`strings`、`sync` 和 `time`；在此基础上，仅 `internal/accounts` 可使用加密标准库 `crypto/rand`、`crypto/sha256` 和 `crypto/subtle`。HTTP 层在通用业务层标准库清单的基础上可使用 `encoding/json`、`mime` 和 `net/http`。第三方依赖默认禁止，仅允许 `internal/store/sqlite` 直接导入 `modernc.org/sqlite`、仅允许 `internal/accounts` 直接导入 `golang.org/x/crypto/argon2`，其余包均禁止直接导入第三方依赖。对允许包内具体 API 的使用仍需通过代码审查，例如 HTTP handler 禁止调用 `http.ServeFile`。

## 账户与文件操作示例

| 操作 | `internal/httpapi` | `internal/accounts` / `internal/files` | `internal/store/` |
| --- | --- | --- | --- |
| 注册 / 登录 / 退出 | 限制凭据请求大小，解析 JSON 或 Bearer 头，转换统一状态码 | `accounts` 校验用户名密码、计算 Argon2id 哈希与令牌 SHA-256 摘要、管理会话生命周期 | `sqlite` 保存用户盐值、哈希、Argon2id 参数及会话令牌摘要 |
| 上传 | 认证会话取得用户 ID，解析目标目录与 multipart 流 | `files` 检查目标归属、文件名和大小，协调内容写入与元数据登记 | `localdisk` 写入内容，`sqlite` 在登记事务中再次检查目标目录 |
| 列表 | 认证会话取得用户 ID，解析目录和分页参数，返回 JSON | `files` 校验目录 ID 和分页上限 | `sqlite` 在同一读取事务中校验目录归属并查询直接子项 |
| 目录和移动 | 认证会话，解析 JSON 与资源 ID，转换错误码 | `files` 校验名称和位置，向存储提交单个变更 | `sqlite` 在写入事务中检查归属、目录环路及删除前的子项，原子更新元数据 |
| 搜索 | 认证会话，解析 `q`、`min_size`、`max_size`、`parent_id` 与分页参数，返回 JSON | `files` 校验过滤条件、目录 ID 和分页上限 | `sqlite` 在同一读取事务中校验可选目录归属，用参数化 `instr` 字面区分大小写查询 |
| 删除文件 | 认证会话，解析文件 ID，返回 `204` 或映射错误码 | `files` 校验归属删除请求，并由后台清理循环分批执行磁盘删除 | `sqlite` 在同一写事务中删除元数据并登记 `blob_cleanups` 任务；`localdisk` 在事务外删除对象 |
| 下载 | 认证会话取得用户 ID，解析文件标识，设置响应头并输出内容流 | `files` 先按 `(owner_id, id)` 校验归属并查询元数据，再取得内容流 | `sqlite` 按 `id` 与 `owner_id` 查找元数据，`localdisk` 打开内容流 |
| 共享与共享访问 | 认证会话，解析共享创建/列表/撤销及以 `share_id` 为范围的浏览和下载请求 | `files` 校验资源类型、接收者、方向与分页，先校验授权再打开磁盘流 | `sqlite` 在同一事务中校验所有者/接收者、目标资源和递归子树关系并隐藏外部父目录；`localdisk` 打开内容流 |
| 增量同步 | 认证会话，解析 `after` 游标与 `limit`，返回 `items`、`next_cursor`、`has_more` | `files` 校验所有者、游标非负与分页上限 | `sqlite` 维护每用户独立递增 `seq`，在元数据写入同一事务中落库变更事件，用 `limit+1` 只读分页查询 |

所有操作均由 `cmd/filedockd` 完成服务和适配器的组装。

## 目录一致性

每个用户的逻辑根目录对应 `parent_id IS NULL`。SQLite 用索引保证同一用户同一父目录下的目录名唯一，根目录使用单独的唯一索引。写入事务先取得 SQLite 写锁，再检查目标父目录归属、移动的祖先链或待删目录的子项，避免并发操作破坏树结构。

上传先检查目标再读取文件流，磁盘写入期间不持有 SQLite 写事务。元数据登记再次检查父目录，目标已被删除时返回目录不存在并清理新对象。文件的重命名和移动只更新 `name` 和 `parent_id`。

## 文件删除与磁盘清理

`DELETE /api/v1/files/{id}` 在同一个 SQLite 写事务内校验归属、删除 `files` 元数据记录并向 `blob_cleanups` 登记该 `object_key` 的清理任务。事务提交后立即返回 `204`，文件随即从列表、搜索和下载中消失，对应目录变空后即可直接删除；事务失败时回滚，原元数据与磁盘对象保持完整。

`cmd/filedockd` 在服务启动后启动后台清理循环，在 SQLite 写事务之外分批调用 `BlobStore.Delete`。磁盘删除成功或对象已不存在时确认移除清理任务；磁盘删除暂时失败时递增重试次数并延后 `next_retry_at_ns`，不阻塞其他待清理任务，进程重启后重置重试时间继续处理。服务关闭时先停止 HTTP 服务与后台清理循环，再关闭 SQLite。

## 账号间只读共享

`shares` 表以 `(owner_id, recipient_id, resource_type, resource_id)` 唯一约束保存文件或目录的只读授权，重复创建返回已有授权；删除文件或目录时在同一写事务内一并清理对应共享记录（同时保留 `blob_cleanups` 机制）。

共享浏览与下载接口均以 `share_id` 为范围，在单个 SQLite 只读事务内核对当前会话接收者、共享授权、目标资源与当前目录子树关系（递归祖先检查）。共享根目录视为接收者的浏览起点，响应将共享根目录的直接子项 `parent_id` 归一化为 `null`，不暴露外部父目录或磁盘 `object_key`。撤销对后续请求立即生效，已打开的下载流允许继续完成。

## 个人目录增量同步

`sync_sequences` 与 `sync_changes` 按 `owner_id` 隔离维护独立递增的 `seq`（从 1 开始）。文件上传、改名/移动、删除和目录创建、改名/移动、删除均在修改元数据的同一个 SQLite 写事务内追加 `file.upsert`、`file.delete`、`folder.upsert` 或 `folder.delete` 事件；事件写入失败时回滚整个元数据事务。磁盘 I/O 保持在写事务之外。

首次迁移在同一事务内为已归属的现存目录（按深度父先于子）和文件生成初始事件，重复启动不重复生成；未认领旧文件仅在 `claim-legacy` 成功时写入目标用户的变更流。`GET /api/v1/sync/changes` 按 `seq > after` 升序查询 `limit+1` 条记录判断 `has_more`，空页保留请求游标，事件不暴露 `object_key` 或磁盘路径。
