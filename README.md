# triproxy

**English** | [简体中文](README.zh-CN.md)

A lightweight HTTP gateway written in Go that translates between **OpenAI Chat Completions**, **OpenAI Responses (used by Codex)**, and **Anthropic Messages (used by Claude Code)**, and forwards requests to any upstream provider.

Core features:

- Clients call with whichever protocol they know; the upstream answers in another protocol, and the proxy handles the bidirectional conversion (request body + response body, including SSE streaming).
- Each `alias` points to an upstream provider and declares which protocol that upstream speaks.
- The API key sent by the client (`Authorization` / `x-api-key`, etc.) is **passed through verbatim** to the upstream without modification (only normalized across protocols — see "Header adaptation").
- When the client and upstream speak the same protocol, the proxy **passes through** (no conversion, all fields preserved).

## Protocols & endpoints

Exposed address format: `http://localhost:8866/{alias}/v1/...`

| Client protocol | Path | Typical client |
|---|---|---|
| Chat Completions | `/{alias}/v1/chat/completions` | OpenAI SDK, chat clients |
| Responses | `/{alias}/v1/responses` | Codex CLI |
| Messages | `/{alias}/v1/messages` | Claude Code |
| Models | `GET /{alias}/v1/models` | Passthrough, no conversion |
| Claude Code connectivity probe | `/{alias}/api/hello` | Returns `200 {"status":"ok"}` (matches the official Anthropic API, so the client's startup probe doesn't 404) |

The conversion direction is derived automatically from "the path the client calls → the protocol the upstream declares":

```text
POST /llm/v1/responses        -> upstream /v1/chat/completions   (openai-responses -> openai-chat)
POST /abc/v1/responses        -> upstream /v1/messages           (openai-responses -> anthropic-messages)
POST /abc/v1/chat/completions -> upstream /v1/messages           (openai-chat -> anthropic-messages)
POST /llm/v1/messages         -> upstream /v1/chat/completions   (anthropic-messages -> openai-chat)
```

## Running

```bash
go build -o triproxy .
./triproxy -config config.yaml          # or -config config.json (JSON works too)
./triproxy -config config.yaml -listen :9000   # override the listen address
```

Health check: `curl http://localhost:8866/healthz`

## Configuration reference

The config file is YAML (JSON also works, since JSON is valid YAML). `listen` defaults to `:8866`.

### Full example (all fields)

```yaml
listen: ":8866"
# proxy: "http://127.0.0.1:7890"    # optional: global default upstream proxy (HTTP/SOCKS5)

aliases:
  # Example 1: Codex client -> OpenAI Chat Completions upstream, with concurrency limit
  #   POST /llm/v1/responses -> https://chat.llm.com/v1/chat/completions
  llm:
    upstream: "https://chat.llm.com"
    protocol: openai-chat
    max_concurrency: 4
    concurrency_mode: reject

  # Example 2: Codex client -> Anthropic Messages upstream, impersonating Claude Code
  #   POST /abc/v1/responses -> https://api.opencode.com/v1/messages
  abc:
    upstream: "https://api.opencode.com"
    protocol: anthropic-messages
    headers:
      User-Agent: "claude-cli/2.1.221 (external, cli)"
      x-app: "cli"
      anthropic-beta: "claude-code-20250219"
    # proxy: "socks5://127.0.0.1:1080"   # optional: per-alias, overrides the global proxy

  # Example 3: any client -> OpenAI Responses upstream (Codex-native protocol)
  #   POST /co/v1/chat/completions -> https://api.responses.com/v1/responses
  co:
    upstream: "https://api.responses.com"
    protocol: openai-responses

  # Example 4: shorthand string form (protocol inferred from the URL path; defaults to openai-chat)
  claude: "https://api.anthropic.com/v1/messages"

  # Example 5: custom upstream path
  local:
    upstream: "http://127.0.0.1:1234"
    protocol: openai-chat
    path: "/v1/custom/completions"
```

### Field reference

| Field | Required | Default | Description |
|---|---|---|---|
| `upstream` | yes | - | Upstream base URL (path optional) |
| `protocol` | no | `openai-chat` | Protocol the upstream speaks: `openai-chat` / `openai-responses` / `anthropic-messages`; short names `chat` / `responses` / `messages` are also accepted (auto-normalized, old configs keep working) |
| `path` | no | protocol standard path | Override the upstream endpoint path (e.g. `/v1/custom/completions`) |
| `max_concurrency` | no | `0` (unlimited) | Max concurrent in-flight requests to the upstream per alias; 2~4 recommended |
| `concurrency_mode` | no | `reject` | Over-limit policy: `reject` returns 429 immediately (protects the upstream); `queue` waits for a free slot |
| `headers` | no | empty | Custom request header map, applied last with highest priority (e.g. impersonate Claude Code / Codex) |
| `proxy` | no | empty | Per-alias upstream proxy (overrides global `proxy`), supports `http`/`https`/`socks5`/`socks5h` |
| `thinking` | no | `false` | Enable the Anthropic extended-thinking bridge (Responses reasoning ↔ Messages thinking, request + response + streaming). Requires an upstream with extended thinking support; off by default — when off, reasoning/thinking content is dropped as before |

The canonical `protocol` names are `openai-chat` / `openai-responses` / `anthropic-messages`; the
short names `chat` / `responses` / `messages` are compatible and auto-normalized. In the shorthand
string form, the protocol is inferred from the URL path.

### Troubleshooting 401 "unauthorized client detected"

This error comes from the upstream (usually a third-party router/gateway) doing "client validation" —
it requires the request to look like it comes from the official Codex / Claude Code. To fix:

1. Configure `headers` on the alias to impersonate (see "Custom headers"), and make sure you run the
   **latest binary** (older versions don't know the `headers` field).
2. Enable `debug: true` and check the `[debug] alias=... 上游请求` line in the log for the **actual
   headers sent upstream** (`Authorization` is redacted) to confirm `User-Agent` / `x-app` /
   `anthropic-beta` are really in place.
3. Confirm the client is actually hitting the alias you configured `headers` on.

## Upstream proxy (HTTP / SOCKS5)

Requests to the upstream can go through an HTTP/HTTPS proxy or a SOCKS5 proxy (natively supported by the Go standard library, no extra dependencies):

- **Global default**: top-level `proxy`, applies to every alias that doesn't set its own.
- **Per-alias override**: an alias's `proxy` overrides the global one.
- Format: `http://host:port`, `https://host:port`, `socks5://host:port` (or `socks5h://` to resolve
  DNS on the proxy side); the URL may include credentials (`http://user:pass@host:port`).

```yaml
proxy: "http://127.0.0.1:7890"        # global default
aliases:
  llm:
    upstream: "https://chat.llm.com"
    protocol: openai-chat             # uses the global proxy
  abc:
    upstream: "https://api.opencode.com"
    protocol: anthropic-messages
    proxy: "socks5://127.0.0.1:1080"  # override: this alias goes through SOCKS5
```

## Concurrency limiting (protect the upstream)

`max_concurrency` caps how many requests to one alias can hit the upstream at the same time, so traffic can't saturate the upstream:

- `reject` (default): over-limit requests immediately receive a **429** (the error body is formatted for the client's protocol, so Codex/Claude Code can parse it and back off/retry on their own).
- `queue`: wait for a free slot (auto-cancelled if the client disconnects; never hangs).
- The slot covers the whole request lifetime (including streaming — this really limits "concurrent connections to the upstream").

```yaml
aliases:
  llm:
    upstream: "https://chat.llm.com"
    protocol: openai-chat
    max_concurrency: 4        # 0 = unlimited (default)
    concurrency_mode: reject  # reject(default) | queue
```

## Protocol header adaptation (automatic) & custom headers (optional)

### Request headers (adjusted per upstream protocol)

- Upstream is `anthropic-messages`: strip OpenAI SDK-specific headers (`openai-*`, `x-stainless-*`),
  add the required `anthropic-version: 2023-06-01`, and mirror `Authorization: Bearer` into `x-api-key`.
- Upstream is `openai-chat` / `openai-responses`: strip Anthropic-specific headers (`anthropic-version`,
  `anthropic-beta`, `x-app`) and turn `x-api-key` into `Authorization: Bearer`.
- The client's own `User-Agent` is passed through (no longer overwritten with `triproxy/1.0`).

### Response headers (automatic)

- `Retry-After` is passed through as-is (needed for 429 backoff).
- Rate-limit headers are mapped per client protocol: `anthropic-ratelimit-*` from an Anthropic upstream →
  `x-ratelimit-*` for Codex clients, and vice versa; `request-id` etc. are kept for reconciling upstream logs.

### Custom headers (impersonating a specific client)

Some gateways only accept the official Claude Code / Codex UA. Configure `headers` on the alias — applied last, highest priority:

```yaml
# Upstream is Anthropic Messages -> impersonate Claude Code
aliases:
  sensenova:
    upstream: "https://...sensenova..."
    protocol: anthropic-messages
    headers:
      User-Agent: "claude-cli/2.1.221 (external, cli)"
      x-app: "cli"
      anthropic-beta: "claude-code-20250219"

# Upstream is OpenAI Responses -> impersonate Codex
aliases:
  codex_upstream:
    upstream: "https://...openai-responses..."
    protocol: openai-responses
    headers:
      User-Agent: "codex_cli_rs/0.139.0 (Linux 6.5.0; x86_64)"
```

> Note: impersonating an official client may violate the upstream's terms of service — evaluate on
> your own. Version strings also go stale and must be maintained by you.

## Memory footprint

Idle RSS is about 6MB. Memory grows with "concurrent in-flight requests × request duration"
(non-streaming requests buffer the full upstream response; streaming requests hold the connection until
it ends). Go's GC doesn't return memory to the OS immediately, so RSS can reach hundreds of MB under
high concurrency with a slow upstream — that's the cost of the workload, not a leak. Ways to control it:

- Give slow/rate-limited upstreams a `max_concurrency` (2~4 recommended, `reject` mode) to cap concurrent long requests; memory drops naturally.
- `triproxy.service` already sets `GOMEMLIMIT=256MiB` + `GOGC=50` to cap the heap and return memory proactively; tune as needed.
- If the upstream is simply slow (minutes), adjust `ResponseHeaderTimeout` (default 120s) to avoid spurious timeouts.

## Install as a Linux systemd service

The project ships `triproxy.service`. Path conventions: **binary `/usr/local/bin/triproxy`, config directory `/etc/triproxy/`**.

```bash
# 1. Deploy binary & config
sudo cp triproxy-linux-amd64 /usr/local/bin/triproxy
sudo chmod +x /usr/local/bin/triproxy
sudo mkdir -p /etc/triproxy
sudo cp config.yaml /etc/triproxy/config.yaml

# 2. Create a dedicated runtime user (avoid running as root)
sudo useradd -r -s /usr/sbin/nologin triproxy
sudo chown -R triproxy:triproxy /etc/triproxy

# 3. Install and enable the service
sudo cp triproxy.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now triproxy

# 4. Check status & logs
systemctl status triproxy
journalctl -u triproxy -f
```

Restart after editing `config.yaml`: `sudo systemctl restart triproxy`.

### One-click install (install.sh)

Grab `install.sh` from the GitHub Release page (or from this repo). By default it **automatically downloads
the Linux binary matching your system from GitHub Releases** and deploys it as a systemd service
(creates the user, installs config + service, and starts it):

```bash
# Download the latest release and install (needs root)
sudo ./install.sh

# Install a specific version
sudo ./install.sh --version v1.2.0

# Use a local binary (same directory as the script, or dist/), no network
sudo ./install.sh --local
```

Options:

| Option | Description |
|---|---|
| `--version <tag>` | Download a specific version (default `latest`, i.e. the newest release) |
| `--local` | Use a local binary (script directory or `dist/`), no download |
| `--yes` / `-y` | Skip confirmation prompts |
| `--dry-run` | Print the actions that would run, without changing the system |
| `--uninstall` | Uninstall (stop the service + remove binary/service files) |

Supported platforms (Linux + systemd): Debian/Ubuntu (apt), RHEL/CentOS/Fedora/Rocky/AlmaLinux (yum/dnf),
Arch/Manjaro (pacman), openSUSE (zypper), Alpine (apk, requires bash);
architectures: amd64 / arm64 / 386 / arm / riscv64 / loong64 / ppc64le / s390x.

> Note: download mode requires the repo to have a matching GitHub release (push a tag to trigger CI
> publishing). For offline/air-gapped environments use `--local`, or download the binary for your
> platform manually from the Release page.

## Client examples

### Codex CLI -> an upstream that only supports Chat (protocol conversion)

```bash
export OPENAI_BASE_URL="http://localhost:8866/llm/v1"
export OPENAI_API_KEY="your key (passed through to the upstream)"
codex
```

### Claude Code -> an OpenAI Chat upstream

Point Claude Code at the proxy (upstream protocol set to `openai-chat`); it calls `/v1/messages`, and the proxy converts to the Chat protocol before forwarding:

```bash
export ANTHROPIC_BASE_URL="http://localhost:8866/llm"
export ANTHROPIC_API_KEY="your key"
claude
```

## Conversion details & limitations

Implemented conversions:

- Messages (system / user / assistant / tool), text content, images (base64 data URL).
- **Documents**: Responses `input_file` ↔ Anthropic `document` (base64 / url sources).
- Function calls: `tool_calls` ↔ `tool_use` ↔ `function_call`, preserving the order of `tool_result` / `function_call_output`.
- **Tool result media**: `function_call_output` / `tool_result` content arrays (text + image + file) are preserved structurally instead of being flattened to strings.
- Tool definitions and `tool_choice` across all three protocol shapes.
- **Parallel tool switch**: Responses `parallel_tool_calls` ↔ Anthropic `disable_parallel_tool_use`.
- Sampling params (`temperature`, `top_p`, `max_tokens`/`max_output_tokens`) are converted.
- Usage counters are converted (prompt/completion ↔ input/output).
- **Reasoning bridge** (with `thinking: true`): `reasoning.effort` ↔ thinking budget; reasoning items ↔
  thinking blocks (lossless envelope round-trip including encrypted content); streaming
  `reasoning_summary_text.delta` ↔ `thinking_delta`; Chat's `reasoning_content` (DeepSeek/Qwen
  convention) is also bridged into reasoning items.
- **Truncation reasons**: `max_tokens`/`refusal` map to Responses `incomplete_details.reason` (`max_output_tokens`/`content_filter`).
- **SSE streaming**: bidirectional conversion between Chat `data:` blocks / Responses `response.*`
  events / Anthropic `content_block_*` events.
- **Responses event integrity**: every emitted event carries an increasing `sequence_number`; parallel tool calls each complete independently.
- **`/responses/compact`**: passed through when the upstream is Responses; otherwise a clear 501 (needed by Codex for long-session compaction).
- **Upstream error conversion**: non-2xx (e.g. 429) keeps the status code but converts the error body
  into the client's protocol — streaming Codex/Claude clients receive an SSE `error` event, non-streaming
  ones a JSON error object, so Codex doesn't hang on upstream-formatted errors it can't parse (e.g.
  Anthropic's `{"type":"error","error":{...}}`).
- **Request log**: each request prints `method path status dur bytes remote`, e.g.
  `请求 method=POST path=/llm/v1/responses status=429 dur=3ms bytes=84 remote=127.0.0.1:53102`.
- Upstream error reads have a 30s fallback timeout + `ResponseHeaderTimeout=120s` so a hung upstream can't stall the proxy.
- The client's `Accept-Encoding` is not forwarded to the upstream; if the upstream still returns gzip,
  it is decompressed before conversion (avoids the converter choking on gzip bytes such as the
  `invalid character '\x1f'` magic-number error).

Limitations & notes:

- For Messages upstreams, `max_tokens` is required; if the client doesn't send it, a default of `4096` is injected.
- Cross-protocol conversion carries only the standard fields both sides share; provider-specific
  parameters are preserved only on same-protocol pass-through.
- Images: data URLs are supported (converted to Anthropic base64); plain http(s) image URLs are not
  fetched and are dropped when converting to Messages.
- Chat has no standard document / parallel-tool / reasoning-output fields: these capabilities are
  lossless only between Responses ↔ Messages; on the Chat side they degrade per protocol limits
  (documents dropped, parallel switch ignored, reasoning parsed best-effort from `reasoning_content`).
- `thinking` requires the upstream to support Anthropic extended thinking; the official Anthropic API
  strictly validates thinking signatures. If you get 400 with it enabled, turn it off (the summary-level
  thinking→reasoning conversion still works when off).
- The reasoning bridge's envelope round-trip design (reasoning item ↔ thinking signature /
  `redacted_thinking.data`) is inspired by [cc-switch](https://github.com/farion1231/cc-switch)
  (MIT License); triproxy is an independent implementation.
- `tool_choice` is approximated: `required → any`, `none → auto` (Anthropic has no exact equivalent).
- Rate-limit header mapping is best-effort: request counts are accurate, but token accounting differs
  between the two sides (OpenAI totals vs Anthropic input/output split);
  `x-ratelimit-remaining-tokens` is approximately mapped to `input-tokens-remaining`.
- If an upstream doesn't support streaming and receives `stream: true`, the proxy tries to convert the
  whole response into streaming events back to the client.

## Testing

```bash
go test ./...   # unit tests + end-to-end integration tests with a mock upstream
```

The integration tests cover: Responses→Chat, Responses→Messages, Chat→Messages (streaming and
non-streaming), same-protocol pass-through, auth pass-through, error pass-through (including 429
conversion), request/response header adaptation and mapping, concurrency limits (reject/queue), unknown
alias, `/v1/models`, the reasoning bridge (params/envelope/streaming both directions), documents, tool
result media, the parallel-tool switch, and `/responses/compact`.
