# 配置参考

> 本文档是 `remote-workspace-mcp` 的详细参考。返回 [README](../README.md)。

## 完整 YAML 配置

```yaml
server:
  name: remote-workspace-mcpd
  listen: 0.0.0.0:8080
  mcp_path: /mcp
  state_dir: ./state
  trusted_proxies: []

auth:
  token_file: ./state/access-token

workspace:
  root: /data/projects/project-a

paths:
  readable:
    - /data/shared/docs
    - /data/shared/config
  writable:
    - /data/shared/generated

files:
  max_read_bytes: 1048576
  max_write_bytes: 10485760
  max_result_bytes: 1048576
  max_upload_bytes: 104857600
  max_download_bytes: 1073741824
  max_archive_entries: 10000
  max_extracted_bytes: 1073741824
  max_sync_manifest_bytes: 10485760
  create_parent_directories: true
  ignore_directories:
    - .git
    - node_modules
    - vendor
    - dist

bash:
  enabled: true
  timeout_seconds: 30
  max_output_bytes: 1048576
  max_concurrent: 4
  sandbox:
    mode: none
    require: false
    allow_network: false

logging:
  level: info
  file: ./logs/remote-workspace-mcpd.log
  max_size_mb: 20
  max_files: 5
  audit_enabled: true
```

YAML 使用严格字段检查。字段拼写错误、未知字段或多个 YAML 文档会导致配置加载失败。

## 配置项说明

### `server`

| 配置项 | 类型 | 默认值 | 说明 |
|---|---|---|---|
| `server.name` | string | `remote-workspace-mcpd` | MCP 初始化响应中的服务名称。 |
| `server.listen` | string | `0.0.0.0:8080` | HTTP 监听地址。`0.0.0.0` 表示监听所有网卡。 |
| `server.mcp_path` | string | `/mcp` | MCP Streamable HTTP 路径，必须以 `/` 开头。 |
| `server.state_dir` | string | `./state` | 服务状态目录配置。当前 Token 实际位置由 `auth.token_file` 决定；修改状态目录时建议同时修改 Token 路径。 |
| `server.trusted_proxies` | string[] | `[]` | 受信任代理的 IP 或 CIDR 列表。仅当请求来自这些来源时，审计才采信 `X-Forwarded-For` 中的客户端 IP；留空表示不信任任何代理，审计一律使用对端 IP，可防止调用方伪造来源 IP 污染审计归因。 |
| `server.tls.cert_file` | string | `""` | 用户自备的 TLS 证书文件路径。与 `server.tls.key_file` 必须同时配置；配置后服务仅以 TLS 监听，不再提供明文端口，且证书支持定时热加载（无需重启进程）。留空表示不启用 TLS。 |
| `server.tls.key_file` | string | `""` | 用户自备的 TLS 私钥文件路径，与 `server.tls.cert_file` 配对。mTLS（客户端证书校验）不在本次范围内。 |

生产环境可根据网络策略将 `listen` 改为具体内网 IP。只允许本机反向代理访问时可以使用 `127.0.0.1:8080`。

### `auth`

| 配置项 | 类型 | 默认值 | 说明 |
|---|---|---|---|
| `auth.token_file` | string | `./state/access-token` | 静态 Bearer Token 的持久化文件。不存在时自动生成。 |

Token 文件不能向 group 或 other 开放权限。所有 `/mcp`、`/files`、`/directories`、`/sync/*` 请求都必须携带：

```text
Authorization: Bearer <token>
```

`/healthz` 和 `/readyz` 不需要 Token。

### `workspace`

| 配置项 | 类型 | 默认值 | 说明 |
|---|---|---|---|
| `workspace.root` | string | 无，必填 | Agent 相对路径的起点，默认可读写。目录必须存在。 |

例如 `src/main.go` 会解析为 `<workspace.root>/src/main.go`。Workspace 不需要重复放入 `paths.readable` 或 `paths.writable`。

### `paths`

| 配置项 | 类型 | 默认值 | 说明 |
|---|---|---|---|
| `paths.readable` | string[] | `[]` | Workspace 之外允许读取的绝对目录。 |
| `paths.writable` | string[] | `[]` | Workspace 之外允许读写的绝对目录。 |

路径规则：

- 相对路径只允许位于 Workspace 内。
- 绝对路径必须位于 Workspace 或相应白名单内。
- `readable` 允许读取、搜索、列举和下载，不允许写入。
- `writable` 同时具备读取和写入权限。
- 服务启动时会规范化白名单路径，并拒绝不存在或无法解析的目录。
- 文件工具会解析符号链接，防止通过链接越过授权边界。
- Bash 只有启用 Bubblewrap 时才会按这些目录形成文件系统视图；`bash.sandbox.mode: none` 下，Shell 进程仍拥有服务运行用户本身的系统权限。

### `files`

| 配置项 | 类型 | 默认值 | 说明 |
|---|---|---|---|
| `files.max_read_bytes` | integer | `1048576` | MCP `read` 单次最大读取字节数。 |
| `files.max_write_bytes` | integer | `10485760` | `write`、`edit` 等文本写入工具的单次最大字节数。 |
| `files.max_result_bytes` | integer | `1048576` | 搜索、列表等工具的最大结果大小。 |
| `files.max_upload_bytes` | integer | `104857600` | 单文件、目录归档和同步差异包的最大上传字节数。 |
| `files.max_download_bytes` | integer | `1073741824` | 单文件下载或目录下载中普通文件的最大总字节数。 |
| `files.max_archive_entries` | integer | `10000` | 目录归档条目上限，也限制同步 manifest 文件数。 |
| `files.max_extracted_bytes` | integer | `1073741824` | 目录或同步归档解包后的最大总字节数，防止压缩炸弹。 |
| `files.max_sync_manifest_bytes` | integer | `10485760` | 同步计划请求和同步包内 manifest 的最大字节数。 |
| `files.create_parent_directories` | boolean | `true` | 写入前是否自动创建目标文件的父目录。 |
| `files.ignore_directories` | string[] | `.git`、`node_modules`、`vendor`、`dist` | 目录下载和同步扫描时跳过的目录名。 |

所有大小配置均以字节为单位。小于 1 的核心限制值会恢复为程序默认值。

### `bash`

| 配置项 | 类型 | 默认值 | 说明 |
|---|---|---|---|
| `bash.enabled` | boolean | `true` | 是否启用 Bash MCP 工具。 |
| `bash.timeout_seconds` | integer | `30` | 单次命令最大运行秒数。调用参数只能缩短，不能突破该上限。 |
| `bash.max_output_bytes` | integer | `1048576` | stdout 和 stderr 各自的最大保留字节数。 |
| `bash.max_concurrent` | integer | `4` | 同时执行的命令数量上限。 |
| `bash.sandbox.mode` | string | `none` | 文件系统隔离：`none`（直接执行）或 `bubblewrap`（Bubblewrap 沙箱）。 |
| `bash.sandbox.require` | boolean | `false` | `mode: bubblewrap` 时，若 `bwrap` 不可用是否拒绝启动/执行而非降级。 |
| `bash.sandbox.allow_network` | boolean | `false` | `mode: bubblewrap` 时是否保留网络；`false` 使用独立网络 namespace（仅 Linux 生效）。 |

#### 命令执行模型

`bash` 工具启用后，所有命令都统一经 `bash -c` 执行（不使用 `-l`，避免 login shell 重置 `PATH`/`HOME` 等环境），因此支持完整的 Shell 语法：管道、重定向、命令替换等。命令级白/黑名单已被移除，约束完全依赖 **Bubblewrap 沙箱**（`bash.sandbox`）与**运行用户权限**。

- `enabled: false`：完全关闭 `bash` 工具（返回 “bash is disabled”）。
- `bash.sandbox.mode: none`：直接以服务进程身份执行命令，无额外文件系统隔离；Shell 进程拥有服务运行用户本身的系统权限。
- `bash.sandbox.mode: bubblewrap`：通过 `bwrap` 构造受限文件系统视图（Workspace 可写、可读白名单只读、系统目录只读）与可选网络 namespace，详见 [Bubblewrap 沙箱原理](./sandbox.md)。

```yaml
bash:
  enabled: true
  sandbox:
    mode: bubblewrap
    require: true
    allow_network: false
```

本服务不做命令级白/黑名单，安全边界由沙箱与最小权限的运行用户共同构成；不要依赖命令名过滤作为安全控制。解释器、自定义脚本或其他工具仍可能实现被禁行为的等价效果，因此应配合非特权用户、cgroup、主机防火墙等补充控制。

### `logging`

| 配置项 | 类型 | 默认值 | 当前状态 |
|---|---|---|---|
| `logging.level` | string | `info` | `slog` 最低日志级别，支持 `debug`、`info`、`warn`、`error`。 |
| `logging.file` | string | `./logs/remote-workspace-mcpd.log` | `slog` 日志文件。日志只写入该文件，不写标准输出。 |
| `logging.max_size_mb` | integer | `20` | 单个日志文件触发轮转的大小，单位 MB。小于 1 时使用默认值 20。 |
| `logging.max_files` | integer | `5` | 保留的日志文件总数，包含活动文件。默认最多 5 个文件，即活动文件及历史文件 `.1` 到 `.4`。 |
| `logging.audit_enabled` | boolean | `true` | 控制是否将工具调用与传输接口的审计记录写入日志文件。启用后，每次受鉴权请求（成功或失败）都会产出 `audit=true` 的审计条目，含操作类型、目标、结果、耗时、客户端 IP 与失败原因。 |

`logging.level`、`logging.file`、`logging.max_size_mb` 和 `logging.max_files` 已接入运行时，`logging.audit_enabled` 也已生效。日志达到 `max_size_mb` 后轮转：现有历史文件依次后移，活动文件变为 `.1`；`max_files: 5` 时最多保留活动文件、`.1`、`.2`、`.3` 和 `.4`。
