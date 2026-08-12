# triproxy

A lightweight HTTP gateway written in Go that translates between **OpenAI Chat Completions**, **OpenAI Responses (used by Codex)** and **Anthropic Messages (used by Claude Code)**, and forwards requests to any upstream provider.

- Clients call with whichever protocol they know; the upstream answers in another protocol, and the proxy handles the bidirectional conversion (request + response, including SSE streaming).
- Each `alias` points to an upstream provider and declares which protocol that upstream speaks.
- API keys are passed through verbatim to the upstream.
- Same-protocol requests pass through with no conversion.

**Links:** [GitHub](https://github.com/tomoncle/triproxy) · [简体中文](#简体中文说明)

---

## Quick start

```bash
docker run -d --name triproxy \
  -p 8866:8866 \
  -v /path/to/config.yaml:/etc/triproxy/config.yaml \
  tomoncle/triproxy
```

The image bundles an example config at `/etc/triproxy/config.yaml`, so it starts out of the box — but you should mount your real `config.yaml` (see below). The container runs as a non-root user and has a built-in health check.

Verify it is up:

```bash
curl http://localhost:8866/healthz    # {"status":"ok"}
```

## Configuration

The container reads `/etc/triproxy/config.yaml` (YAML or JSON). Mount your own config to replace the example:

```bash
docker run -d --name triproxy \
  -p 8866:8866 \
  -v $(pwd)/config.yaml:/etc/triproxy/config.yaml \
  tomoncle/triproxy
```

Override the listen address without editing the config (arguments are appended to the entrypoint):

```bash
docker run -d -p 9000:9000 tomoncle/triproxy -listen :9000
```

### Minimal config

```yaml
listen: ":8866"

aliases:
  # example: DeepSeek / any OpenAI-compatible upstream
  llm:
    upstream: "https://api.deepseek.com"
    protocol: openai-chat

  # example: Claude-native upstream (Anthropic-compatible)
  anthropic:
    upstream: "https://api.anthropic.com"
    protocol: anthropic-messages
    headers:
      User-Agent: "claude-cli/2.1.221 (external, cli)"
      x-app: "cli"
```

### Alias field reference

| Field | Required | Default | Description |
|---|---|---|---|
| `upstream` | yes | - | Upstream base URL (path optional) |
| `protocol` | no | `openai-chat` | `openai-chat` / `openai-responses` / `anthropic-messages` (short names `chat` / `responses` / `messages` also accepted) |
| `path` | no | protocol default | Override the upstream endpoint path (e.g. `/v1/custom/completions`) |
| `max_concurrency` | no | `0` (unlimited) | Per-alias concurrency cap; 2~4 recommended |
| `concurrency_mode` | no | `reject` | `reject` = return 429 on over-limit; `queue` = wait for a free slot |
| `headers` | no | empty | Custom request headers, applied last (e.g. impersonate Claude Code / Codex) |
| `proxy` | no | empty | Per-alias upstream proxy: `http` / `https` / `socks5` / `socks5h` |
| `thinking` | no | `false` | Anthropic extended-thinking bridge (Responses reasoning ↔ Messages thinking, request + response + streaming) |

## Endpoints

Exposed as `http://host:8866/{alias}/v1/...`:

| Client protocol | Path | Typical client |
|---|---|---|
| Chat Completions | `/{alias}/v1/chat/completions` | OpenAI SDK, chat clients |
| Responses | `/{alias}/v1/responses` | Codex CLI |
| Messages | `/{alias}/v1/messages` | Claude Code |
| Models | `GET /{alias}/v1/models` | passthrough |
| Probe | `/{alias}/api/hello` | `200 {"status":"ok"}` (matches the official Anthropic API, so client startup probes don't 404) |

## Client examples

**Codex → an OpenAI-compatible upstream (protocol conversion):**

```bash
export OPENAI_BASE_URL="http://localhost:8866/llm/v1"
export OPENAI_API_KEY="your key (passed through to the upstream)"
codex
```

**Claude Code → an OpenAI-compatible upstream:**

```bash
export ANTHROPIC_BASE_URL="http://localhost:8866/llm"
export ANTHROPIC_API_KEY="your key"
claude
```

## docker-compose

```yaml
services:
  triproxy:
    image: tomoncle/triproxy:latest
    container_name: triproxy
    restart: unless-stopped
    ports:
      - "8866:8866"
    volumes:
      - ./config.yaml:/etc/triproxy/config.yaml
```

> The image has a built-in `HEALTHCHECK` (GET `/healthz` via `wget`), so no explicit healthcheck is needed.

## Tags & platforms

- `latest` — newest release
- `vX.Y.Z` — versioned releases (e.g. `v1.0.0`)

Multi-architecture: `linux/amd64`, `linux/arm64`, `linux/arm/v7`, `linux/386`, `linux/ppc64le`, `linux/s390x`, `linux/riscv64`.

---

## 简体中文说明

triproxy 是一个用 Go 编写的轻量 HTTP 网关，可在 **OpenAI Chat Completions**、**OpenAI Responses（Codex）**、**Anthropic Messages（Claude Code）** 三种协议之间双向转换，并转发到任意上游。

快速开始：

```bash
docker run -d --name triproxy \
  -p 8866:8866 \
  -v /path/to/config.yaml:/etc/triproxy/config.yaml \
  tomoncle/triproxy
```

- 镜像内置示例配置，开箱即用；正式部署请用 `-v` 挂载真实 `config.yaml` 覆盖；
- 容器内以非 root 用户运行，自带 `/healthz` 健康检查；
- 修改监听端口：`docker run ... tomoncle/triproxy -listen :9000`；
- 支持多架构：`linux/amd64`、`linux/arm64`、`linux/arm/v7`、`linux/386`、`linux/ppc64le`、`linux/s390x`、`linux/riscv64`；
- 完整文档见 [GitHub 仓库](https://github.com/tomoncle/triproxy)。
