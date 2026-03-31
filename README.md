# PR Agent Go

`pr-agent-go` is a lightweight GitHub pull request automation service written in Go with the standard library.

`pr-agent-go` 是一个使用 Go 标准库实现的轻量级 GitHub Pull Request 自动化服务。

It receives GitHub webhooks, reviews pull requests with an LLM-compatible provider, posts structured summaries, tracks runtime metrics, and continues from explicit human instructions such as "merge this PR" or "update the branch and re-review".

它可以接收 GitHub webhook，使用兼容 LLM 的模型审查 PR，发布结构化摘要，记录运行指标，并根据人工输入继续执行诸如“直接合并”或“更新分支后重新审核”之类的动作。

This repository intentionally uses placeholder examples and generic hostnames in documentation. Deployment-specific domains, secrets, and local workstation paths are not included.

文档中故意使用占位示例和通用域名，不包含部署专用域名、真实密钥或本地工作站路径。

## Highlights | 亮点

- Webhook-driven PR review for `pull_request` events
- Structured review summaries with findings, strengths, and test suggestions
- Stage-by-stage timing output for CLI runs and background webhook processing
- Manual follow-up flow for merge, branch sync, re-review, and conflict handling
- Queue-based concurrent processing with runtime status endpoints
- CLI support for `review`, `check`, `add`, `status`, `stats`, `logs`, and `doctor`

- 基于 webhook 的 `pull_request` 事件审核
- 结构化审核摘要，包含问题、优点和测试建议
- CLI 与后台任务都会记录分阶段耗时
- 支持人工继续处理合并、同步分支、重新审核与冲突场景
- 内存队列并发处理，并提供运行状态接口
- CLI 提供 `review`、`check`、`add`、`status`、`stats`、`logs` 与 `doctor`

## Architecture | 架构

- `cmd/server`: CLI entrypoint and HTTP server
- `internal/orchestrator`: review, manual follow-up, and merge workflows
- `internal/github`: GitHub REST and GraphQL helpers
- `internal/review`: LLM interaction, normalization, and heuristic fallback
- `internal/conflict`: temporary-worktree conflict resolution
- `internal/processor`: in-memory queue and worker pool
- `internal/storage`: JSON-backed persistence

- `cmd/server`：CLI 入口与 HTTP 服务
- `internal/orchestrator`：审核、人工继续处理与合并流程
- `internal/github`：GitHub REST / GraphQL 封装
- `internal/review`：模型调用、结果规范化与启发式回退
- `internal/conflict`：临时工作目录中的冲突处理
- `internal/processor`：内存队列与 worker 池
- `internal/storage`：基于 JSON 的本地持久化

## Requirements | 运行要求

- Go `1.26+`
- A GitHub token with repository access
- Optional OpenAI-compatible API credentials for model-backed review
- `git` installed if you want automatic conflict handling

- Go `1.26+`
- 一个具备仓库访问权限的 GitHub token
- 可选的兼容 OpenAI 的模型 API 凭证
- 如果要自动处理冲突，需要安装 `git`

## Quick Start | 快速开始

```bash
git clone <your-fork-or-repo-url>
cd pr-agent-go
cp .env.example .env
go test ./...
go run ./cmd/server
```

Running the compiled binary without arguments is equivalent to:

```bash
./pr-agent-go serve
```

默认直接运行二进制时等同于执行 `./pr-agent-go serve`。

## Configuration | 配置

Minimum required variables:

- `GITHUB_TOKEN`
- `GITHUB_WEBHOOK_SECRET`

Common optional variables:

- `GITHUB_WEBHOOK_URL`
- `GITHUB_API_BASE_URL`
- `OPENAI_API_BASE_URL`
- `OPENAI_API_KEY`
- `OPENAI_MODEL`
- `WORKER_COUNT`
- `QUEUE_SIZE`
- `GIT_TEMP_DIR`
- `GIT_USER_NAME`
- `GIT_USER_EMAIL`

See [.env.example](.env.example) for the full template.

完整配置模板见 [.env.example](.env.example)。

## CLI

### Review a pull request | 审核一个 PR

```bash
./pr-agent-go review owner/repo 123
./pr-agent-go review https://github.com/owner/repo/pull/123
```

### Continue from a human decision | 人工继续处理一个 PR

```bash
./pr-agent-go check owner/repo 123 --note "merge this PR"
./pr-agent-go check https://github.com/owner/repo/pull/123 --note "update branch and re-review"
```

`check` 用于人工继续处理已经进入待处理状态的 PR，比如直接合并、更新分支、要求重新审核或补充评论。

### Add or update a repository webhook | 添加或更新仓库 webhook

```bash
./pr-agent-go add owner/repo
./pr-agent-go add https://github.com/owner/repo
```

This command uses `GITHUB_WEBHOOK_URL` and `GITHUB_WEBHOOK_SECRET` from the environment and ensures the repository has a webhook configured for `pull_request` events.

这个命令会读取环境中的 `GITHUB_WEBHOOK_URL` 与 `GITHUB_WEBHOOK_SECRET`，并确保目标仓库已配置 `pull_request` 事件 webhook。

### Backward compatibility | 向后兼容

The preferred command names are now:

- `review`
- `check`
- `add`
- `doctor`

Older aliases are still accepted for backward compatibility:

- `review-pr` -> `review`
- `intervene-pr` -> `check`
- `register-webhook` -> `add`

当前推荐使用的新命令为：

- `review`
- `check`
- `add`
- `doctor`

为了兼容旧用法，以下别名仍然可用：

- `review-pr` -> `review`
- `intervene-pr` -> `check`
- `register-webhook` -> `add`

### Inspect runtime state | 查看运行状态

```bash
./pr-agent-go status
./pr-agent-go stats
./pr-agent-go logs
./pr-agent-go doctor
```

All major CLI commands support `--json`.

主要 CLI 命令都支持 `--json`。

## Review Policy | 审核策略

The current trust decision is intentionally simple:

- automatic handling depends on `risk` and `confidence`
- draft PRs are never auto-merged
- branch protection and repository rules still take precedence
- findings are rendered for humans, but they do not block automatic handling by themselves
- if a provider does not return a usable confidence value, comments may show `confidence=unavailable`

当前信任判定规则刻意保持简单：

- 自动处理主要依据 `risk` 和 `confidence`
- Draft PR 不会自动合并
- GitHub 分支保护和仓库规则始终优先
- `findings` 主要用于给人看，不再单独阻断自动处理
- 如果模型供应商没有返回可用的 `confidence`，评论中会显示 `confidence=unavailable`

## GitHub Token Permissions | GitHub Token 权限

For a fine-grained personal access token, the current implementation works best with:

- `Pull requests: Write`
- `Contents: Write`
- `Webhooks: Write`
- `Metadata: Read`

You also need sufficient repository-level access to manage webhooks.

如果使用 fine-grained PAT，建议至少授予：

- `Pull requests: Write`
- `Contents: Write`
- `Webhooks: Write`
- `Metadata: Read`

另外，管理 webhook 还需要目标仓库本身具备足够权限。

## Development | 开发

```bash
go test ./...
go build ./...
./scripts/build_linux_x64.sh
```

## Deployment | 部署

This repository includes example scripts for packaging and deployment:

- [build_linux_x64.sh](scripts/build_linux_x64.sh)
- [deploy_remote.sh](scripts/deploy_remote.sh)

The deployment script in this repository is environment-specific. Treat it as an example and adapt it to your own infrastructure.

Example:

```bash
REMOTE_HOST=your-server.example ./scripts/deploy_remote.sh
```

仓库中的部署脚本是带环境假设的示例，请根据你自己的基础设施进行调整。

示例：

```bash
REMOTE_HOST=your-server.example ./scripts/deploy_remote.sh
```

## Security and Privacy | 安全与隐私

- Never commit real `.env` files or secrets
- Use placeholder webhook URLs and example repository names in documentation
- Review deployment scripts before reusing them elsewhere
- Rotate tokens and webhook secrets if they were ever exposed during testing

- 不要提交真实 `.env` 文件或密钥
- 文档里尽量使用占位 webhook 地址和示例仓库名
- 部署脚本复用前请先自行审查
- 如果测试中泄露过 token 或 webhook secret，请立即轮换

## Roadmap | 路线图

- Database-backed persistence
- Repository policy files
- Better webhook event coverage
- AstrBot or chat-ops reporting integrations
- Safer and richer automatic conflict resolution

- 数据库持久化
- 仓库级策略文件
- 更完整的 webhook 事件覆盖
- AstrBot / chat-ops 报告集成
- 更安全、更丰富的自动冲突处理

## Contributing | 贡献

Issues and pull requests are welcome. For larger workflow or API changes, a short proposal first will make review easier.

欢迎 issue 与 PR。对于较大的工作流或 API 改动，建议先开一个简短提案，便于后续评审。

## License | 许可证

This project is licensed under the MIT License. See [LICENSE](LICENSE) for details.

本项目采用 MIT License，详见 [LICENSE](LICENSE)。
