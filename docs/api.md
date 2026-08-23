# MCP 工具与接口

> 本文档是 `remote-workspace-mcp` 的详细参考。返回 [README](../README.md)。

## MCP 工具

| 工具 | 功能 |
|---|---|
| `read` | 按行读取文本文件并返回 SHA-256。 |
| `write` | 原子创建或替换文本文件，支持 `expected_sha256`。 |
| `edit` | 精确替换文本，支持替换一次或全部匹配。 |
| `apply_patch` | 对一个或多个文件执行结构化精确编辑。 |
| `grep` | 正则或固定字符串搜索，支持 glob、上下文和结果上限。 |
| `glob` | 查找匹配路径，支持 `**`。 |
| `list` | 列举目录，可递归并限制条目数。 |
| `bash` | 按 Bash 策略、超时、输出、并发和可选 Bubblewrap 隔离执行命令。 |

`apply_patch` 是结构化精确替换集合，不是完整 unified diff 引擎。批次中的编辑按数组顺序执行；指向同一规范化路径的多项编辑会在内存中累计，并对该文件只原子写入一次。

- `expected_sha256` 始终校验文件在批次规划开始时的原始内容。同一文件的后续编辑即使依赖前一项结果，也应继续使用原始哈希。
- 服务在写盘前重新读取并复核所有目标文件的 SHA-256；任一文件发生变化时，整个批次在写盘前失败。
- `dry_run: true` 走完整的解析、读取、哈希校验和顺序编辑规划，但不写盘；响应按文件返回编辑索引、替换次数、原始/结果哈希与字节数。
- 默认模式会先完成全部预校验，再逐文件提交。设置 `rollback_on_error: true` 后，如果中途写入失败，服务会按相反顺序恢复本批次已写入文件；回滚属于补偿操作，无法对进程崩溃或外部并发写入提供数据库级事务保证。
- 单批最多 1000 项编辑、100 个唯一文件，请求体最多 16 MiB，累计输入和结果分别最多 64 MiB。单文件仍受 `max_read_bytes` 和 `max_write_bytes` 限制。

## MCP 接口说明

### 连接与生命周期

MCP 端点默认为 `POST /mcp`，所有 MCP 请求都需要携带：

```http
Authorization: Bearer <token>
Content-Type: application/json
Accept: application/json, text/event-stream
```

调用流程：

1. 发送 `initialize` 完成协议握手（无需会话 ID）。
2. 后续 `tools/list`、`tools/call` 等请求只需携带 Bearer 令牌，无需 `Mcp-Session-Id`。
3. 初始化完成后发送 `notifications/initialized` 通知。
4. 使用 `tools/list` 获取当前工具 schema，使用 `tools/call` 调用工具。

> 说明：本服务为无状态 Streamable HTTP 实现，不维护会话，也不提供 server-to-client 的 SSE 事件流（`GET /mcp` 固定返回 405）。

服务端返回 JSON-RPC 2.0 响应。常见错误码包括：

| 错误码 | 含义 |
|---|---|
| `-32700` | 请求体不是单个合法 JSON 值 |
| `-32600` | JSON-RPC 请求结构无效 |
| `-32601` | 方法不存在 |
| `-32602` | 工具参数或调用失败 |

### MCP 工具

所有工具通过以下 JSON-RPC 方法调用：

```json
{
  "jsonrpc": "2.0",
  "id": 2,
  "method": "tools/call",
  "params": {
    "name": "read",
    "arguments": {}
  }
}
```

工具结果位于 `result.content[].text` 中，通常是 JSON 字符串。路径参数支持 Workspace 相对路径；绝对路径必须符合 Workspace 或路径白名单规则。

#### `read`

读取文本文件并返回带行号内容和 SHA-256。

`read` 按原始字节读取文件，但返回值需要作为 JSON 文本展示：实现会先把字节转换为 Go 字符串，JSON 编码时无效 UTF-8 字节会显示为 Unicode 替换字符。因此它适合查看 UTF-8 文本，不能作为任意二进制内容或非 UTF-8 字节的无损传输接口。需要保留原始字节时，应使用文件下载接口；传入二进制文件时建议按二进制流上传和下载，不要经由文本/JSON 工具参数中转。

| 参数 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `path` | string | 是 | 文件路径 |
| `offset` | integer | 否 | 起始行，默认从第 1 行开始 |
| `limit` | integer | 否 | 最大读取行数 |
| `expected_sha256` | string | 否 | 文件哈希不匹配时拒绝读取 |

#### `write`

原子创建或替换文本文件。目标路径必须具备写权限。

| 参数 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `path` | string | 是 | 目标文件路径 |
| `content` | string | 是 | 文件内容 |
| `overwrite` | boolean | 否 | 是否允许覆盖已有文件 |
| `expected_sha256` | string | 否 | 防止覆盖期间文件被并发修改 |

#### `edit`

对文件原始字节执行精确替换。`replace_all` 为 false 时要求匹配唯一。目标文件不要求整体为 UTF-8；但 JSON 中的 `old_text` 和 `new_text` 来自 Unicode 字符串，它们会以 UTF-8 字节参与匹配和替换，因此无法直接表达任意非 UTF-8 目标字节。

| 参数 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `path` | string | 是 | 目标文件路径 |
| `old_text` | string | 是 | 待替换的原文本 |
| `new_text` | string | 是 | 替换后的文本 |
| `replace_all` | boolean | 否 | 是否替换全部匹配，默认 false |
| `expected_sha256` | string | 否 | 防止并发修改 |

#### `apply_patch`

执行一组结构化的原始字节精确编辑，不是完整 unified diff 引擎，也不提供跨文件事务回滚。与 `edit` 相同，目标文件不要求整体为 UTF-8；但每项编辑的 JSON `old_text` 和 `new_text` 来自 Unicode 字符串，只能以 UTF-8 字节表达匹配和替换内容，无法直接表达任意非 UTF-8 目标字节。

| 参数 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `edits` | array | 是 | 编辑项集合，每项包含 `path`、`old_text`、`new_text`，可选 `replace_all` 和 `expected_sha256` |
| `dry_run` | boolean | 否 | 只校验，不写入文件 |
| `rollback_on_error` | boolean | 否 | 中途写入失败时按相反顺序回滚本批次已写入文件（补偿性，非事务保证） |

所有编辑目标会先校验路径和匹配条件；任一编辑失败时不会执行本次写入。

#### `grep`

在许可路径内搜索文本，支持正则、固定字符串、Glob 和上下文。

| 参数 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `path` | string | 否 | 搜索根路径，默认 Workspace |
| `pattern` | string | 是 | 正则或固定字符串 |
| `glob` | string | 否 | 文件匹配模式 |
| `fixed_string` | boolean | 否 | 按字面量搜索，默认 false |
| `case_sensitive` | boolean | 否 | 是否区分大小写 |
| `context_before` / `context_after` | integer | 否 | 匹配前后文行数 |
| `max_results` | integer | 否 | 最大匹配数 |

#### `glob`

查找许可路径内的文件和目录。

| 参数 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `path` | string | 否 | 搜索根路径 |
| `pattern` | string | 是 | 支持 `**` 的 Glob 模式 |
| `max_results` | integer | 否 | 最大结果数 |

#### `list`

列举目录内容。

| 参数 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `path` | string | 否 | 目录路径，默认 Workspace |
| `recursive` | boolean | 否 | 是否递归列举 |
| `max_entries` | integer | 否 | 最大条目数 |

#### `bash`

命令统一经 `bash -c` 执行（支持完整 Shell 语法、管道、重定向与命令替换），不区分命令级白/黑名单；文件系统与网络隔离由 `bash.sandbox`（`none` 或 `bubblewrap`）决定，详见 [配置参考](./configuration.md#bash) 与 [Bubblewrap 沙箱原理](./sandbox.md)。

| 参数 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `command` | string | 是 | 要执行的命令 |
| `cwd` | string | 否 | 工作目录 |
| `timeout_seconds` | integer | 否 | 单次超时时间 |
| `max_output_bytes` | integer | 否 | stdout/stderr 合并输出上限 |

命令还会受到 `bash.sandbox.mode`、`bash.sandbox.require`、`bash.sandbox.allow_network`、并发数（`bash.max_concurrent`）、超时、输出上限以及 Workspace / 路径白名单限制。

### 文件和目录传输

所有传输接口都需要 Bearer Token，并使用原始 HTTP body，不使用 JSON 或 Base64 包装大文件。所有传输接口复用 Bearer Token 和路径权限。

### 单文件

```text
PUT /files?path=<path>
GET /files?path=<path>
```

上传请求体就是原始文件内容。上传先写入目标目录中的临时文件，再原子重命名。下载以 `application/octet-stream` 返回。

```bash
curl -H "Authorization: Bearer <token>" --upload-file ./build/app \
  "http://<remote-host>:8080/files?path=bin/app"

curl -H "Authorization: Bearer <token>" -o ./app \
  "http://<remote-host>:8080/files?path=bin/app"
```

### 目录

```text
PUT /directories?path=<path>&format=tar.gz
GET /directories?path=<path>&format=tar.gz
```

`format` 支持：

- `tar.gz` 或 `tgz`：默认，使用 gzip 压缩。
- `tar`：不压缩。

目录上传要求目标目录不存在。服务先把归档保存到临时文件，再解包到目标目录旁的临时目录；校验全部通过后原子重命名。目录上传只对目标目录路径加写锁，无法覆盖其子文件的写锁分片，因此与并发的单文件写（针对同一目录下的文件）不存在互斥保证；若需强一致，应避免对正在上传的目录并发执行单文件写操作。

目录归档会拒绝绝对路径、`..`、Windows 绝对路径、符号链接、硬链接、设备文件和其他特殊条目，并限制条目数及展开总大小。

```bash
curl -H "Authorization: Bearer <token>" --upload-file ./project.tar.gz \
  "http://<remote-host>:8080/directories?path=project&format=tar.gz"

curl -H "Authorization: Bearer <token>" -o project.tar.gz \
  "http://<remote-host>:8080/directories?path=project&format=tar.gz"
```

## 增量同步

增量同步采用“manifest 对比 + 差异归档”，不是 rsync wire protocol，也不做块级差量。

### 生成同步计划

```text
POST /sync/plan?path=<remote-directory>
```

请求：

```json
{
  "files": [
    {
      "path": "cmd/main.go",
      "size": 3281,
      "sha256": "<64-character-sha256>"
    }
  ]
}
```

响应包含：

- `missing`：远端不存在。
- `changed`：大小或 SHA-256 不一致。
- `unchanged`：内容一致。
- `extra`：仅远端存在。

### 应用差异包

```text
PUT /sync/apply?path=<remote-directory>&format=tar.gz
```

归档根目录必须包含 `.remote-workspace-mcp-sync-manifest.json`，并且只携带本次 `missing`、`changed` 文件。归档文件必须与 manifest 的路径、大小和 SHA-256 一一对应。

更新已有文件时可以提供 `expected_sha256`。如果远端文件在计划后发生变化，服务返回 `409 Conflict`，避免覆盖并发修改。

同步按文件使用临时落盘和原子替换，但不提供多文件事务回滚。若 `/sync/apply` 中途失败，已写入的文件不会回滚，错误信息会标明“部分变更已写入”。调用方必须按完整 plan 重新执行 `/sync/plan` 与 `/sync/apply`，不能只重试失败的单文件。当前版本保留 `extra` 文件，不自动删除远端内容。

## 健康检查

```text
GET /healthz
GET /readyz
```

成功响应：

```json
{"status":"ok"}
```

或：

```json
{"status":"ready"}
```

这两个接口不要求 Token，只表示进程和路由已启动，不执行 Workspace、磁盘或下游依赖的深度检查。
