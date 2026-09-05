# 安全边界与运维建议

> 本文档是 `remote-workspace-mcp` 的详细参考。返回 [README](../README.md)。

## 安全边界

- 静态 Token 在 HTTP 上传输时不加密，只应在可信内网中使用；生产环境应启用 `server.tls` 以 TLS 加密传输。
- Token 是服务级权限，不区分不同 Agent 或用户。
- Workspace 默认读写；额外绝对路径必须进入只读或可写白名单。
- 文件工具执行规范路径和符号链接检查。
- `bash.sandbox.mode: none` 时，Bash 可以访问服务运行用户有权限访问的其他路径。
- 本服务不做命令级白/黑名单，安全边界由 Bubblewrap 沙箱与最小权限运行用户构成；应配合非特权用户、cgroup、防火墙等补充控制。
- Bubblewrap 主要提供文件系统和可选网络隔离，不替代非特权用户、cgroup、防火墙、seccomp 或主机加固。
- `logging.audit_enabled` 开启后提供工具调用与传输审计（含客户端 IP 与失败原因）；鉴权失败等安全事件始终以 WARN 记录，不受该开关控制；审计输出到 `logging.file`。

## 运维建议

- 使用专用非特权系统用户运行 `remote-workspace-mcpd`。
- Token 文件、Workspace 和可写白名单按最小权限设置。
- 生产环境启用 Bubblewrap 沙箱（`bash.sandbox.mode: bubblewrap`、`bash.sandbox.require: true`），默认关闭网络（`bash.sandbox.allow_network: false`）。
- 需要目录隔离时设置 `bash.sandbox.mode: bubblewrap` 与 `bash.sandbox.require: true`。
- 默认关闭网络，仅为确需联网的命令开启。
- 用主机防火墙只允许可信 Agent 来源访问服务端口。
- Token 轮换后立即更新 Agent Secret。
- 使用 systemd 配置自动重启、文件描述符限制、cgroup 资源限制和日志收集。
- 启用 `server.tls.cert_file` / `key_file` 以 TLS 加密传输；证书支持定时热加载，替换证书文件后无需重启进程。
- 通过 `logging.audit_enabled: true` 开启工具调用与传输审计，并定期归档 `logging.file` 以满足追溯需求。

## 支持的运行平台

`remote-workspace-mcpd` 是纯 Go 标准库实现，无平台相关构建标签，可交叉编译到任意 Go 支持的操作系统与架构。Release 通过 GoReleaser 产出 `linux` / `darwin` / `windows` × `amd64` / `arm64`（Windows 无 arm64）。但各平台运行能力不同：

| 系统 (x64) | 服务可运行 | `bash` 工具 | 沙箱 | 说明 |
|---|---|---|---|---|
| Linux | ✅ 完整支持 | ✅ `bash -c` | ✅ Bubblewrap | 唯一官方目标；容器内需开启非特权 user namespace 才能运行 `bwrap` |
| macOS | ✅ 可运行 | ✅ 自带 `/bin/bash` | ❌ 仅 `none` | 适合本地开发 / 自测；CI 已覆盖 |
| Windows | 🟡 可运行（有前提） | 🟡 需 Git Bash / WSL 的 `bash` 在 PATH | ❌ 仅 `none` | 路径白名单需用盘符绝对路径；文件权限语义与 Linux 不同 |

要点：

- **沙箱（Bubblewrap）是 Linux 专属**，依赖内核 namespace / mount / 网络隔离能力；macOS 与 Windows 只能使用 `bash.sandbox.mode: none`，安全边界完全依赖运行用户权限与主机加固。
- **Windows 使用 `bash` 工具前**必须安装 Git Bash（或 WSL），并确保 `bash` 可执行文件在 `PATH` 中，否则服务可启动但命令执行会失败。
- **Agent / 客户端侧与操作系统无关**，只要能发起 HTTP 请求即可连接（任何 OS 或语言）。

