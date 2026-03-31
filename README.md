# PR Agent Go V1

这是一个使用 Go 标准库实现的轻量 V1 PR Agent。

它当前支持：

- 接收 GitHub Pull Request webhook
- 拉取 PR 和变更文件信息
- 生成结构化审核摘要
- 记录每个处理阶段的耗时，并在 CLI / webhook 日志中输出汇总
- 回写或更新固定评论
- 对可信 PR 自动合并，对可信但落后基线的 PR 自动更新分支
- 对可信但发生冲突的 PR，在临时工作目录尝试解决冲突并继续合并
- 对其他 PR 自动请求人工介入，并支持通过 CLI 输入文字意见继续处理
- 把事件日志和审核记录写入本地 JSON
- 用 CLI 直观查看 GitHub、模型、队列和 PR 处理状态
- 用 worker 池并发处理 webhook 任务

## 启动

```bash
cd /Users/tianyue/Documents/Playground/pr-agent-go
cp .env.example .env
GOCACHE=$(pwd)/.gocache go test ./...
GOCACHE=$(pwd)/.gocache go run ./cmd/server
```

默认直接运行二进制会启动服务，等价于：

```bash
./pr-agent-go serve
```

## 必填环境变量

- `GITHUB_WEBHOOK_SECRET`
- `GITHUB_TOKEN`

可选：

- `GITHUB_WEBHOOK_URL`
- `WORKER_COUNT`
- `QUEUE_SIZE`
- `OPENAI_API_KEY`
- `OPENAI_MODEL`
- `OPENAI_API_BASE_URL`
- `GIT_TEMP_DIR`
- `GIT_USER_NAME`
- `GIT_USER_EMAIL`

未配置 LLM 时，会退化到本地启发式摘要。

如果后续要用 CLI 自动注册或更新仓库 webhook，建议提前配置：

- `GITHUB_WEBHOOK_URL`

## 接口

- `GET /healthz`
- `GET /internal/daily-summary`
- `GET /internal/status`
- `POST /webhooks/github`

## CLI

可以直接在服务器上执行二进制查看运行情况：

```bash
./pr-agent-go status
./pr-agent-go stats
./pr-agent-go check
./pr-agent-go logs
./pr-agent-go review-pr owner/repo 123
./pr-agent-go review-pr https://github.com/owner/repo/pull/123
./pr-agent-go intervene-pr owner/repo 123 --note "请直接合并"
./pr-agent-go intervene-pr https://github.com/owner/repo/pull/123 --note "请直接合并"
```

可用命令：

- `serve` 启动 webhook 服务
- `status` 查看 GitHub 状态、模型状态、队列情况、今日处理摘要
- `stats` 查看聚合统计和最近 PR 处理记录
- `check` 只执行外部连通性检查
- `logs` 查看最近事件日志和最近审核结果
- `review-pr owner/repo 123` 或完整 GitHub PR 链接，手动触发指定 PR 的审核、自动动作和评论回写，并实时打印阶段进度与耗时汇总
- `intervene-pr owner/repo 123` 或完整 GitHub PR 链接，针对需要人工介入的 PR 输入文字意见，由 Agent 解释并继续执行

如果想拿结构化结果：

```bash
./pr-agent-go status --json
./pr-agent-go stats --json
./pr-agent-go check --json
./pr-agent-go logs --json
./pr-agent-go review-pr owner/repo 123 --json
./pr-agent-go review-pr https://github.com/owner/repo/pull/123 --json
./pr-agent-go intervene-pr owner/repo 123 --note "同步分支后重新审核" --json
./pr-agent-go intervene-pr https://github.com/owner/repo/pull/123 --note "同步分支后重新审核" --json
```

## 本地验证

当前核心链路已经有自动化测试覆盖：

```bash
cd /Users/tianyue/Documents/Playground/pr-agent-go
GOCACHE=$(pwd)/.gocache go test ./...
GOCACHE=$(pwd)/.gocache go build ./...
```

测试覆盖了这些关键点：

- GitHub webhook 签名校验
- PR webhook 进入编排流程
- 拉取 PR、文件和 CI 状态
- 生成审核摘要
- 创建 PR 评论
- 审核记录写入本地 JSON
- Ping webhook 快速响应
- `json` / `form` 两种 GitHub webhook payload 格式

示例 webhook payload 放在：

- `testdata/github_pr_opened.json`

## Linux x86-64 发布构建

生成可直接放到服务器运行的 Linux x86-64 可执行文件：

```bash
cd /Users/tianyue/Documents/Playground/pr-agent-go
./scripts/build_linux_x64.sh
```

构建完成后会产出：

- `dist/linux-x64/pr-agent-go`
- `dist/linux-x64/.env.example`
- `dist/linux-x64/README.md`
- `dist/linux-x64/SHA256SUMS`
- `dist/linux-x64/pr-agent-go-linux-x64.tar.gz`

服务器上最简运行方式：

```bash
tar -xzf pr-agent-go-linux-x64.tar.gz
cp .env.example .env
# 编辑 .env，至少填 GITHUB_WEBHOOK_SECRET 和 GITHUB_TOKEN
./pr-agent-go
```

如果要直接部署到 `cruty.cn`：

```bash
cd /Users/tianyue/Documents/Playground/pr-agent-go
./scripts/deploy_cruty_cn.sh
```

这个脚本会：

- 重新构建 Linux x86-64 产物
- 上传到 `/www/wwwroot/go-server`
- 安装 `systemd` 服务 `pr-agent-go`
- 在替换版本前先切换服务状态
- 确保默认端口为 `9000`

## 并发处理

服务端会把 webhook 放进内存队列，再由 worker 池并发处理。

- `WORKER_COUNT` 控制并发 worker 数量
- `QUEUE_SIZE` 控制内存队列容量

`/internal/status` 和 CLI `status` 都会展示当前：

- 队列积压数量
- 正在处理的任务数
- 已完成数量
- 失败数量
- 最近失败原因

## 说明

当前自动动作规则比较保守：

- `low risk + high confidence + no findings + CI success + ready_for_manual_approval` 的 PR 会被视为可信
- 可信且 GitHub 判定可合并时，系统会自动 squash merge
- 可信但 `mergeable_state=behind` 时，系统会请求 GitHub 更新分支
- 可信但有真实冲突时，系统会把分支拉到 `GIT_TEMP_DIR` 下的临时工作目录，尝试做一次本地 merge
- 如果本地 merge 无冲突，或模型能高置信度解决少量文本冲突，系统会把结果推回 PR 分支并尝试继续 merge
- 如果冲突文件过多、过大、非文本，或模型置信度不够，会生成冲突处理建议并请求人工复核
- 其他情况会自动留言提示使用 `intervene-pr` 继续处理

后续可以继续升级为：

1. PostgreSQL 持久化
2. 仓库策略文件
3. AstrBot 日报
4. 人工最终审批流
