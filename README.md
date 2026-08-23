# Remote Workspace MCP

[![CI](https://github.com/iamyounglee/remote-workspace-mcp/actions/workflows/ci.yml/badge.svg)](https://github.com/iamyounglee/remote-workspace-mcp/actions/workflows/ci.yml)
[![Latest Release](https://img.shields.io/github/v/release/iamyounglee/remote-workspace-mcp)](https://github.com/iamyounglee/remote-workspace-mcp/releases)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](./LICENSE)
[![Go](https://img.shields.io/badge/Go-1.26-blue.svg)](https://go.dev/)

`remote-workspace-mcpd` 是部署在远端机器上的 MCP 服务。它通过 Streamable HTTP 向编码 Agent 提供「像操作本地工作区一样」的远程文件读写、代码搜索、受控命令执行、文件传输与目录增量同步能力，并使用静态 Bearer Token 鉴权、Workspace / 路径白名单与可选 Bubblewrap 沙箱（Linux）约束访问范围。

## 目录

- [文档](#文档)
- [特性](#特性)
- [推荐使用场景](#推荐使用场景)
- [环境要求](#环境要求)
- [安装](#安装)
- [快速开始](#快速开始)
- [配置](#配置)
- [安全](#安全)
- [支持的运行平台](#支持的运行平台)
- [贡献与许可](#贡献与许可)

> 以 MIT 许可证开源，模块路径 `github.com/iamyounglee/remote-workspace-mcp`，守护进程（二进制）名为 `remote-workspace-mcpd`。欢迎参与贡献，见 [CONTRIBUTING.md](./CONTRIBUTING.md) 与 [CODE_OF_CONDUCT.md](./CODE_OF_CONDUCT.md)。

## 文档

- [配置参考](./docs/configuration.md) — 完整 YAML 示例与逐项说明（server / auth / workspace / paths / files / bash / logging）
- [MCP 工具与接口](./docs/api.md) — 工具参数、接口生命周期、文件/目录传输、增量同步、健康检查
- [Bubblewrap 沙箱原理](./docs/sandbox.md) — namespace 隔离、启用方式与能力边界
- [安全边界与运维建议](./docs/operations.md)

## 特性

- **MCP Streamable HTTP**：`POST /mcp`，无状态、无需会话。
- **静态 Bearer Token 鉴权**：首次启动自动生成、持久化、可热轮换。
- **Workspace 与路径白名单**：相对 / 绝对路径控制，独立只读 / 可写名单，解析符号链接防越权。
- **MCP 工具**：`read`、`write`、`edit`、`apply_patch`、`grep`、`glob`、`list`、`bash`。
- **文件 / 目录传输**：单文件 `PUT/GET /files`，目录 `tar` / `tar.gz` `PUT/GET /directories`。
- **增量同步**：基于 manifest 与 SHA-256 的差异同步 `POST /sync/plan`、`PUT /sync/apply`。
- **受控命令执行**：`bash` 工具可开关（`enabled`），命令统一经 `bash -c` 执行（支持完整 Shell 语法），并可选 Bubblewrap 沙箱（`bash.sandbox.mode: none|bubblewrap`）做文件系统与网络隔离。
- **资源限制与健康检查**：文件大小、超时、并发限制；`GET /healthz`、`GET /readyz`。

## 推荐使用场景

`remote-workspace-mcp` 把「文件读写 / 搜索 / 命令执行 / 传输 / 同步」收敛成一个受控的远端 MCP 服务，适合任何需要让编码 Agent 安全操作远端机器的场景。典型用法：

1. **接入 DeepSeek++ 浏览器插件做远端开发**：把服务部署在开发机或云主机上，在浏览器里的 DeepSeek++ 插件中配置 `streamableHttp` 类型的 MCP 指向该服务，即可让网页版对话模型直接读写远端仓库、运行构建与测试，把「网页模型」变成一台可操作真实远端工作区的开发代理。
2. **网络受限的内网机器开发**：内网机器无法直连公网 IDE 插件或云端 Agent 时，在内网机上部署本服务，Agent 通过内网可达的 HTTP 地址操作内网代码与命令，避免把整个内网暴露给外部，也无需在受限机上安装完整 IDE。
3. **CI / 构建农场中的一次性远程构建与调试**：将服务临时部署到构建节点，让 Agent 在失败现场直接登录查看日志、复现、打补丁并重新触发构建，缩短「本地改 → 提交 → 等 CI」的循环；任务结束即可销毁实例。
4. **多 Agent 协作的共享远端工作区**：多个编码 Agent（或同一 Agent 的多次会话）共享同一远端 Workspace，通过统一的路径白名单、Token 鉴权与增量同步保持一致，避免每个人都维护一份本地副本带来的状态漂移。
5. **临时云开发机 / 沙箱预览**：为每次需求或 PR 拉起一台临时云机器并部署本服务，Agent 在其中完成开发验证；配合 Bubblewrap 沙箱（`bash.sandbox.mode: bubblewrap`）隔离文件系统与网络，验证完成后整机回收，环境零残留。

> 所有场景都建议结合网络 ACL / 防火墙将服务端口限制在可信来源，并对生产环境启用 Bubblewrap 沙箱与最小权限运行用户。详见 [安全边界与运维建议](./docs/operations.md)。

## 环境要求

- Go 1.26+。
- 建议以普通非特权系统用户运行。
- `bash`：启用 `bash` 工具时需要（命令经 `bash -c` 执行，需系统存在 `bash`）。
- Bubblewrap（可选）：仅当 `bash.sandbox.mode: bubblewrap` 时需要。
- 暂不提供 Docker 部署文件；构建后二进制可直接运行，无需 Go 运行时。

## 安装

### 方式一：go install

通过 `go install` 安装最新版（需本地已安装 **Go 1.26+**）：

```bash
go install github.com/iamyounglee/remote-workspace-mcp/cmd/remote-workspace-mcpd@latest
```

### 方式二：从 Release 下载

从 [GitHub Releases](https://github.com/iamyounglee/remote-workspace-mcp/releases) 下载对应平台压缩包（含 `remote-workspace-mcpd` 二进制与 `config.example.yaml`，并提供 `checksums.txt` 校验），解压即可使用：

```bash
# 以 Linux x64 为例
curl -fsSL -O https://github.com/iamyounglee/remote-workspace-mcp/releases/latest/download/remote-workspace-mcpd_<VERSION>_linux_amd64.tar.gz
tar -xzf remote-workspace-mcpd_<VERSION>_linux_amd64.tar.gz
./remote-workspace-mcpd --help
```

### 方式三：源码构建

从源码构建（推荐用仓库 `Makefile`）：

```bash
make build   # CGO_ENABLED=0 go build -o remote-workspace-mcpd ./cmd/remote-workspace-mcpd
make test    # go test -race ./...
make lint    # golangci-lint run ./...
```

## 快速开始

```bash
# 复制示例配置并改名为运行时配置
cp config.example.yaml config.yaml
# 编辑配置，至少设置 workspace.root 指向你的项目目录
# 校验配置是否合法
./remote-workspace-mcpd validate-config --config ./config.yaml
# 启动服务，守护进程开始监听 MCP 端口
# 不带任何参数等价于 serve --config config.yaml，配置相对当前工作目录解析
./remote-workspace-mcpd serve        --config ./config.yaml
# 查看当前 Bearer Token，首次启动会自动生成并持久化
./remote-workspace-mcpd token show   --config ./config.yaml
# 轮转 Token，生成、持久化并立即激活新 Token，旧 Token 失效
./remote-workspace-mcpd token rotate --config ./config.yaml
```

在 Agent 侧配置 MCP（`token show` 获取的 Token 建议放入平台 Secret，不要提交代码）：

```json
{
  "mcpServers": {
    "remote-workspace-mcp": {
      "type": "streamableHttp",
      "url": "http://<remote-host>:<port>/mcp",
      "headers": { "Authorization": "Bearer <token-from-token-show>" }
    }
  }
}
```

## 配置

完整 YAML 示例与逐项说明见 [docs/configuration.md](./docs/configuration.md)，亦可参考仓库内的 `config.example.yaml`。安全相关配置建议见 [安全边界与运维建议](./docs/operations.md)。

## 安全

`remote-workspace-mcpd` 通过静态 Bearer Token 鉴权；Token 为服务级权限、以明文经 HTTP 传输，因此应结合网络 ACL / 防火墙将服务端口限制在可信来源内。

生产环境建议：

- Linux 平台启用 Bubblewrap 沙箱（`bash.sandbox.mode: bubblewrap`、`bash.sandbox.require: true`）约束命令执行的文件系统与网络视图；
- 默认关闭网络访问（`bash.sandbox.allow_network: false`）；`allow_network` 仅当 `mode: bubblewrap` 且在 Linux 上运行时才生效，非 Linux 或 `mode: none` 下无论该值如何命令都按宿主机网络执行，无法隔离网络；
- 使用专用非特权系统用户运行，并按最小权限配置 Workspace 与路径白名单。

更完整的安全边界与运维建议见 [安全边界与运维建议](./docs/operations.md)。

## 支持的运行平台

`remote-workspace-mcpd` 是纯 Go 实现，可交叉编译到任意 Go 支持的平台。**生产部署唯一完整支持 Linux x64**——Bubblewrap 沙箱仅 Linux 可用，提供文件系统与网络隔离。

macOS / Windows 可运行服务与 `bash` 工具，但只能使用 `bash.sandbox.mode: none`，**无沙箱隔离**：

- **macOS**：系统自带 `/bin/bash`，`bash` 工具开箱可用。
- **Windows**：需安装 **Git Bash** 或 WSL，并把 `bash`（如 `C:\Program Files\Git\bin\bash.exe`）加入 `PATH`；此外路径白名单需使用 Windows 盘符绝对路径、文件权限语义与 Linux 不同。

跨平台构建与测试见 CI（ubuntu / macOS / windows）。完整说明见 [安全边界与运维建议](./docs/operations.md#支持的运行平台)。

## 贡献与许可

- 贡献指南：[CONTRIBUTING.md](./CONTRIBUTING.md)
- 行为准则：[CODE_OF_CONDUCT.md](./CODE_OF_CONDUCT.md)
- 许可证：[MIT](./LICENSE)
