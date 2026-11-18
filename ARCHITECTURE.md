# FileDock 架构

FileDock 是单 Go module。当前 HTTP 服务只使用标准库。

| 包 | 职责 | 可依赖的模块内包 |
| --- | --- | --- |
| `cmd/filedockd` | 组装依赖，启动 HTTP 服务，管理进程日志和关闭流程 | `internal/config`、`internal/httpapi`、已登记的业务包和存储适配器 |
| `internal/config` | 读取和校验进程配置 | 无 |
| `internal/httpapi` | 注册路由、解析 HTTP 请求、调用业务服务、转换 HTTP 响应 | 已登记的业务包 |
| `internal/archcheck` | 在 `go test ./...` 中检查生产代码的直接导入 | 规则实现位于测试文件 |

命令入口与架构测试包禁止被生产代码导入。HTTP 层禁止直接访问存储适配器、SQL、磁盘文件和进程配置。下载文件时，HTTP 层从业务服务取得读取流，再写入响应。

## 后续业务和存储

- 按业务能力组织包。文件和目录的权限判断、业务校验与状态变更放在 `internal/files`。账户、共享、同步与断点续传在对应需求开始后确定落点。跨业务包依赖默认禁止，确需引入时写明具体允许的导入关系。
- 业务包禁止依赖 HTTP、SQL、具体磁盘实现和进程配置。需要元数据查询或文件内容读写时，在使用方定义小接口，由 `cmd/filedockd` 注入实现。
- SQLite 元数据适配器可放在 `internal/store/sqlite`，本地磁盘内容适配器可放在 `internal/store/localdisk`。适配器可导入实现接口需要的业务类型。业务包禁止反向依赖适配器。
- 第一轮已为 `internal/files`、`internal/store/sqlite` 和 `internal/store/localdisk` 登记职责。新增包按完整包路径归类。未归类的包和未明确允许的模块内导入默认报错。

## 自动检查

架构测试通过 `go list` 读取当前构建配置下本模块生产包的直接导入。入口、配置与存储适配器可使用标准库。业务层可使用 `bytes`、`context`、`errors`、`fmt`、`io`、`path`、`slices`、`strconv`、`strings`、`sync` 和 `time`。HTTP 层在这份清单的基础上可使用 `encoding/json` 和 `net/http`。首轮禁止引入第三方包。对允许包内具体 API 的使用仍需通过代码审查，例如 HTTP handler 禁止调用 `http.ServeFile`。

## 文件操作示例

| 操作 | `internal/httpapi` | `internal/files` | `internal/store/` |
| --- | --- | --- | --- |
| 上传 | 解析 multipart 请求，传入目标目录标识和内容流，转换业务结果 | 检查目录写权限、文件名和大小，协调内容写入与元数据登记 | `localdisk` 将内容流写入磁盘，`sqlite` 保存元数据 |
| 列表 | 解析目录标识与分页参数，返回 JSON | 检查目录访问权限，查询条目和分页 | `sqlite` 查询目录元数据 |
| 下载 | 解析文件标识，设置响应头并输出内容流 | 检查读取权限，查询元数据并取得内容流 | `sqlite` 查找元数据，`localdisk` 打开内容流 |

三个操作都由 `cmd/filedockd` 完成服务和适配器的组装。
