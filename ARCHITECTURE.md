# FileDock 架构

FileDock 是单 Go module。HTTP 服务只使用标准库；第三方包目前仅允许 `internal/store/sqlite` 直接导入 `modernc.org/sqlite`，以及 `internal/accounts` 直接导入 `golang.org/x/crypto/argon2`。

| 包 | 职责 | 可依赖的模块内包 |
| --- | --- | --- |
| `cmd/filedockd` | 组装依赖，启动 HTTP 服务，执行本地维护子命令（如 `claim-legacy`），管理进程日志和关闭流程 | `internal/config`、`internal/httpapi`、已登记的业务包和存储适配器 |
| `internal/config` | 读取和校验进程配置 | 无 |
| `internal/httpapi` | 注册路由、解析 HTTP 请求与会话令牌、调用业务服务、转换 HTTP 响应 | 已登记的业务包（`internal/accounts`、`internal/files`） |
| `internal/accounts` | 账户注册、Argon2id 密码校验、Bearer 会话令牌签发、认证与撤销 | 无 |
| `internal/files` | 文件所有者权限判断、文件名和大小校验、协调内容写入与元数据登记及流式下载 | 无 |
| `internal/store/sqlite` | 通过 `database/sql` 持久化账户、会话摘要与文件元数据，执行事务化迁移与旧文件认领 | `internal/accounts`、`internal/files` |
| `internal/store/localdisk` | 本地磁盘暂存、流式写入、计算 SHA-256 与打开内容流 | `internal/files` |
| `internal/archcheck` | 在 `go test ./...` 中检查生产代码的直接导入 | 规则实现位于测试文件 |

命令入口与架构测试包禁止被生产代码导入。HTTP 层禁止直接访问存储适配器、SQL、磁盘文件和进程配置。下载文件时，HTTP 层先认证会话并将用户 ID 传给 `internal/files`，从业务服务取得读取流后再写入响应。

## 后续业务和存储

- 按业务能力组织包。账户与会话放在 `internal/accounts`，文件和目录的权限判断、业务校验与状态变更放在 `internal/files`。共享、同步与断点续传在对应需求开始后确定落点。跨业务包依赖默认禁止（`internal/accounts` 与 `internal/files` 之间禁止互相导入），确需引入时写明具体允许的导入关系。
- 业务包禁止依赖 HTTP、SQL、具体磁盘实现和进程配置。需要元数据查询或文件内容读写时，在使用方定义小接口，由 `cmd/filedockd` 注入实现。
- SQLite 元数据适配器放在 `internal/store/sqlite`，本地磁盘内容适配器放在 `internal/store/localdisk`。适配器可导入实现接口需要的业务类型。业务包禁止反向依赖适配器。
- 新增包按完整包路径归类。未归类的包和未明确允许的模块内导入默认报错。

## 自动检查

架构测试通过 `go list` 读取当前构建配置下本模块生产包的直接导入。入口、配置与存储适配器可使用标准库。通用业务层可使用 `bytes`、`context`、`errors`、`fmt`、`io`、`path`、`slices`、`strconv`、`strings`、`sync` 和 `time`；在此基础上，仅 `internal/accounts` 可使用加密标准库 `crypto/rand`、`crypto/sha256` 和 `crypto/subtle`。HTTP 层在通用业务层标准库清单的基础上可使用 `encoding/json`、`mime` 和 `net/http`。第三方依赖默认禁止，仅允许 `internal/store/sqlite` 直接导入 `modernc.org/sqlite`、仅允许 `internal/accounts` 直接导入 `golang.org/x/crypto/argon2`，其余包均禁止直接导入第三方依赖。对允许包内具体 API 的使用仍需通过代码审查，例如 HTTP handler 禁止调用 `http.ServeFile`。

## 账户与文件操作示例

| 操作 | `internal/httpapi` | `internal/accounts` / `internal/files` | `internal/store/` |
| --- | --- | --- | --- |
| 注册 / 登录 / 退出 | 限制凭据请求大小，解析 JSON 或 Bearer 头，转换统一状态码 | `accounts` 校验用户名密码、计算 Argon2id 哈希与令牌 SHA-256 摘要、管理会话生命周期 | `sqlite` 保存用户盐值、哈希、Argon2id 参数及会话令牌摘要 |
| 上传 | 认证会话取得用户 ID，流式解析 multipart 并提取原始文件名 | `files` 校验所有者 ID、文件名和大小，协调内容写入与元数据登记 | `localdisk` 将内容流写入磁盘，`sqlite` 保存带 `owner_id` 的元数据 |
| 列表 | 认证会话取得用户 ID，解析分页参数，返回 JSON | `files` 按所有者 ID 查询条目和有上限分页 | `sqlite` 按 `owner_id` 查询文件元数据 |
| 下载 | 认证会话取得用户 ID，解析文件标识，设置响应头并输出内容流 | `files` 先按 `(owner_id, id)` 校验归属并查询元数据，再取得内容流 | `sqlite` 按 `id` 与 `owner_id` 查找元数据，`localdisk` 打开内容流 |

所有操作均由 `cmd/filedockd` 完成服务和适配器的组装。
