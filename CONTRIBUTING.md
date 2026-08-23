# 贡献指南

感谢你考虑为 `remote-workspace-mcpd` 做出贡献！

## 开发环境

- **Go**：`go 1.26` 或更高版本（见 `go.mod`）。
- **构建：**

  ```bash
  make build          # 输出 ./remote-workspace-mcpd
  ```

- **测试：**

  ```bash
  make test           # go test -race ./...
  make cover          # 生成覆盖率报告
  ```

- **静态检查：**

  ```bash
  make lint           # golangci-lint run
  go vet ./...
  ```

  提交前请保证 `make lint` 与 `make test` 均通过。CI 会自动运行同样的检查。

## 工作流

1. Fork 本仓库并基于 `main` 创建特性分支：`git checkout -b feat/your-change`。
2. 保持提交小而聚焦，信息遵循 Conventional Commits（如 `feat:`、`fix:`、`docs:`、`refactor:`）。
3. 新增功能请附带测试；修复 Bug 请补充回归测试。
4. 更新相关文档（`README.md`、`config.example.yaml` 等）。
5. 提交 Pull Request 到 `main`，并填写 PR 模板。

## 代码风格

- 遵循 `gofmt` / `goimports`，以及本仓库 `.editorconfig`。
- 公共 API 与复杂逻辑请补充包注释与行内注释（本项目以中文注释为主，保持一致即可）。
- 错误需包装上下文（`fmt.Errorf("...: %w", err)`），避免吞掉错误。

## 安全相关改动

涉及鉴权、路径白名单、沙箱或命令执行的行为变更属于**高危改动**。请在 PR 中说明安全影响，并同步更新 `config.example.yaml` 的注释与本文档。切勿通过公开 Issue 讨论未修复的安全漏洞。

## 版本与发布

- 版本号由 CI/Release 流程通过 `-ldflags` 注入，请勿在代码中硬编码版本。
- 发布以 `v*` tag 触发，详见 `.github/workflows/release.yml` 与 `.goreleaser.yaml`。
- 提交信息将用于自动生成 Release Notes（基于 Conventional Commits）。
