# PR Agent Go

`pr-agent-go` is a lightweight GitHub pull request automation service written in Go with the standard library.

It receives GitHub webhooks, reviews pull requests with an LLM-compatible provider, posts structured summaries, tracks runtime metrics, and can continue from explicit human instructions such as "merge this PR" or "update the branch and re-review".

This repository intentionally uses placeholder examples and generic hostnames in documentation. Deployment-specific domains, secrets, and local workstation paths are not included.

## Highlights

- Webhook-driven PR review for `pull_request` events
- Structured review summaries with findings, strengths, and test suggestions
- Stage-by-stage timing output for CLI runs and background webhook processing
- Manual intervention flow for trusted merges, branch updates, and re-review
- Optional conflict handling in a temporary Git workspace
- Queue-based concurrent processing with runtime status endpoints
- CLI support for reviewing PRs, intervening on PRs, checking service health, and registering repository webhooks

## Architecture

The service is organized into a few small modules:

- `cmd/server`
  CLI entrypoint and HTTP server
- `internal/orchestrator`
  Review and intervention workflows
- `internal/github`
  GitHub REST and GraphQL client helpers
- `internal/review`
  LLM interaction, normalization, and heuristic fallbacks
- `internal/conflict`
  Temporary-worktree conflict resolution flow
- `internal/processor`
  In-memory queue and worker pool
- `internal/storage`
  JSON-backed event and review persistence

## Features

### Review workflow

- Collects PR metadata, changed files, and commit status
- Calls an OpenAI-compatible `chat/completions` endpoint or falls back to local heuristics
- Publishes a managed PR summary comment
- Persists review results and timing breakdowns locally

### Intervention workflow

- Accepts human instructions from CLI
- Can merge, update a branch, request re-review, or leave a comment-only decision
- Automatically marks draft pull requests as ready for review before merging when needed
- Publishes a separate final status comment for accepted merges

### Conflict workflow

- Clones PR branches into a temporary directory
- Attempts a local merge against the base branch
- Uses the model to resolve small, text-only conflicts when confidence is high enough
- Falls back to a conflict summary and human follow-up when automatic resolution is unsafe

### Operations

- `GET /healthz`
- `GET /internal/status`
- `GET /internal/daily-summary`
- `POST /webhooks/github`

## Requirements

- Go `1.26+`
- A GitHub token with repository access
- Optional OpenAI-compatible API credentials for model-backed review
- `git` installed if you want automated conflict handling

## Quick Start

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

## Configuration

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

## CLI

### Review a pull request

```bash
./pr-agent-go review-pr owner/repo 123
./pr-agent-go review-pr https://github.com/owner/repo/pull/123
```

### Continue from a human decision

```bash
./pr-agent-go intervene-pr owner/repo 123 --note "merge this PR"
./pr-agent-go intervene-pr https://github.com/owner/repo/pull/123 --note "update branch and re-review"
```

### Register or update a webhook

```bash
./pr-agent-go register-webhook owner/repo
./pr-agent-go register-webhook https://github.com/owner/repo
```

This command uses `GITHUB_WEBHOOK_URL` and `GITHUB_WEBHOOK_SECRET` from the environment and ensures the repository has the webhook configured for `pull_request` events.

### Inspect runtime state

```bash
./pr-agent-go status
./pr-agent-go stats
./pr-agent-go logs
./pr-agent-go check
```

All major CLI commands support `--json`.

## Review Policy

The default policy is intentionally conservative but practical:

- Low-risk, clean PRs can be auto-merged
- Draft PRs are never auto-merged during review
- Human intervention can explicitly approve a merge
- Missing or low-quality model confidence is treated cautiously
- Empty or malformed findings are filtered before rendering comments

These defaults can be extended later with repository-specific policy files.

## GitHub Token Permissions

For a fine-grained personal access token, the current implementation works best with:

- `Pull requests: Write`
- `Contents: Write`
- `Webhooks: Write`
- `Metadata: Read`

You also need sufficient repository-level access to manage webhooks.

## Development

Run tests:

```bash
go test ./...
```

Build:

```bash
go build ./...
```

Create a Linux x86-64 release bundle:

```bash
./scripts/build_linux_x64.sh
```

## Deployment

This repository includes example scripts for packaging and deployment:

- [build_linux_x64.sh](scripts/build_linux_x64.sh)
- [deploy_cruty_cn.sh](scripts/deploy_cruty_cn.sh)

The deployment script in this repository is environment-specific. If you publish this project publicly, treat it as an example and create your own deployment workflow for your infrastructure.

## Security and Privacy

- Never commit real `.env` files or secrets
- Use placeholder webhook URLs and example repository names in documentation
- Review deployment scripts before reusing them in another environment
- Rotate tokens and webhook secrets if they were ever exposed during testing

## Roadmap

- Database-backed persistence
- Repository policy files
- Better webhook event coverage
- AstrBot or chat-ops reporting integrations
- Safer and richer automatic conflict resolution

## Contributing

Issues and pull requests are welcome. If you plan to contribute larger workflow or API changes, opening a short proposal first will make review easier.

## License

This project is licensed under the MIT License. See [LICENSE](LICENSE) for details.
