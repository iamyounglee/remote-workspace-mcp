# Bubblewrap 沙箱原理

> 本文档是 `remote-workspace-mcp` 的详细参考。返回 [README](../README.md)。

## Bubblewrap 实现原理

Bubblewrap 是 Linux 上基于 namespace 和 bind mount 的轻量沙箱工具。服务不会链接 Bubblewrap SDK，而是在执行 Bash 工具时查找 `bwrap` 可执行文件并组装命令参数。

当前实现大致等价于：

```bash
bwrap \
  --die-with-parent \
  --new-session \
  --proc /proc \
  --dev /dev \
  --tmpfs /tmp \
  --unshare-net \
  --ro-bind /usr /usr \
  --ro-bind /bin /bin \
  --ro-bind /lib /lib \
  --ro-bind /lib64 /lib64 \
  --bind <workspace> <workspace> \
  --ro-bind <readable-path> <readable-path> \
  --bind <writable-path> <writable-path> \
  --chdir <cwd> \
  <command> <args...>
```

实际参数会根据目录是否存在、白名单和 `allow_network` 动态生成。

Bubblewrap 不使用 `--clearenv`。服务通过 `os.Environ()` 将 MCP 进程启动时的完整环境显式传给普通命令和 Bubblewrap，沙箱内命令因此继承服务进程的 `PATH`，不会在代码中写死任何机器或用户目录。需要注意：继承只能保留服务进程已有的 PATH；如果服务由 systemd、Supervisor 等以精简环境启动，或启动时 PATH 本身不含命令目录，沙箱内仍无法通过命令名找到对应程序。此时应修正服务启动配置的环境，而不是在 Remote Workspace MCP 配置中硬编码机器路径。

### 隔离内容

- `--die-with-parent`：服务端父进程退出后，沙箱命令随之终止。
- `--new-session`：为命令创建新的会话，降低终端和信号相互影响。
- `--proc /proc`：在沙箱中挂载新的 `/proc`。
- `--dev /dev`：提供最小设备视图。
- `--tmpfs /tmp`：提供隔离的临时目录，命令结束后内容消失。
- `/usr`、`/bin`、`/lib`、`/lib64`：存在时以只读方式映射，提供程序和动态链接库。
- `allow_network: true`：沿用宿主网络 namespace，并在文件存在时只读映射 DNS、NSS、主机名和系统 CA 证书配置，使域名解析与 TLS 校验可用。
- Workspace：以可写方式映射。
- `paths.readable`：以只读方式映射。
- `paths.writable`：以可写方式映射。
- `--unshare-net`：当 `allow_network: false` 时创建独立网络 namespace，通常无法访问外部网络。
- `--chdir`：进入经路径解析器授权的工作目录后执行命令。

命令继承 MCP 服务进程启动时已有的完整环境。服务不会自动执行 `source ~/.bashrc`，也不会在代码或配置中硬编码 PATH。

### 启用方式

推荐严格配置：

```yaml
bash:
  enabled: true
  sandbox:
    mode: "bubblewrap"     # bubblewrap | none，默认 none
    require: true          # 沙箱不可用时拒绝执行，默认 false
    allow_network: false   # 是否允许联网，默认 false
```

启动前执行：

```bash
command -v bwrap
./remote-workspace-mcpd validate-config --config ./remote-workspace-mcp.yaml
```

`sandbox.require: true` 时，配置校验发现 `bwrap` 不可用会直接失败。`sandbox.require: false` 时，`bwrap` 不可用会降级为普通进程执行；生产环境不建议允许这种降级。

### Bubblewrap 不提供的能力

当前集成主要限制文件系统视图和网络 namespace，不应视为完整虚拟机或容器安全边界：

- 没有配置 seccomp 系统调用过滤。
- 没有配置 cgroup CPU、内存、磁盘或进程数量限制。
- 没有显式配置 PID、IPC、UTS 等全部 namespace 隔离选项。
- 不限制服务运行用户在已映射可写目录中的权限。
- `allow_network: true` 时不会提供域名、IP 或端口级网络 ACL。
- 内核、Bubblewrap 和宿主机安全策略仍是可信计算基础。
- 本服务不提供命令级白/黑名单，仅依赖沙箱与运行用户权限约束命令；解释器、自定义脚本或其他程序仍可能实现被禁行为的等价效果。

建议同时使用非特权系统用户运行服务，并用 systemd、cgroup、主机防火墙和文件权限补充资源及网络控制。
