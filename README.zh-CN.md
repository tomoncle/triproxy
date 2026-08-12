# triproxy

[English](README.md) | **简体中文**

一个用 Go 写的轻量 HTTP 网关：把 **OpenAI Chat Completions**、**OpenAI Responses（Codex 使用）**、**Anthropic Messages（Claude Code 使用）** 三种协议互相转换，并把请求转发到任意上游提供商。

核心能力：

- 客户端可以按自己熟悉的协议调用，上游用另一种协议应答，代理负责双向转换（请求体 + 响应体，含 SSE 流式）。
- 每个 `alias` 指向一个上游 provider，并声明上游说的是哪种协议。
- 客户端传来的 API Key（`Authorization` / `x-api-key` 等）**原样透传**给上游，不做任何处理（跨协议时只做格式归一，见"协议头适配"）。
- 客户端和上游协议相同时**透传**（不转换，保留所有字段）。

## 协议与端点

暴露地址格式：`http://localhost:8866/{alias}/v1/...`

| 客户端协议 | 路径 | 典型客户端 |
|---|---|---|
| Chat Completions | `/{alias}/v1/chat/completions` | OpenAI SDK、各种 Chat 客户端 |
| Responses | `/{alias}/v1/responses` | Codex CLI |
| Messages | `/{alias}/v1/messages` | Claude Code |
| Models | `GET /{alias}/v1/models` | 透传，不转换 |
| Claude Code 连通性探测 | `/{alias}/api/hello` | 返回 `200 {"status":"ok"}`（与官方 Anthropic API 一致，避免客户端探测 404） |

转换方向由"客户端调的路径 → 上游声明的协议"自动组合，例如：

```text
POST /llm/v1/responses        -> 上游 /v1/chat/completions   (openai-responses -> openai-chat)
POST /abc/v1/responses        -> 上游 /v1/messages           (openai-responses -> anthropic-messages)
POST /abc/v1/chat/completions -> 上游 /v1/messages           (openai-chat -> anthropic-messages)
POST /llm/v1/messages         -> 上游 /v1/chat/completions   (anthropic-messages -> openai-chat)
```

## 运行

```bash
go build -o triproxy .
./triproxy -config config.yaml          # 或 -config config.json（JSON 也行）
./triproxy -config config.yaml -listen :9000   # 覆盖监听地址
```

检查服务：`curl http://localhost:8866/healthz`

## 配置参考

配置文件为 YAML（JSON 也兼容，因为 JSON 是合法 YAML）。`listen` 缺省 `:8866`。

### 完整示例（含全部字段）

```yaml
listen: ":8866"
# proxy: "http://127.0.0.1:7890"    # 可选：全局默认上游代理（HTTP/SOCKS5）

aliases:
  # 例 1：Codex 客户端 -> OpenAI Chat Completions 上游，带并发限制
  #   POST /llm/v1/responses -> https://chat.llm.com/v1/chat/completions
  llm:
    upstream: "https://chat.llm.com"
    protocol: openai-chat
    max_concurrency: 4
    concurrency_mode: reject

  # 例 2：Codex 客户端 -> Anthropic Messages 上游，自定义头伪装成 Claude Code
  #   POST /abc/v1/responses -> https://api.opencode.com/v1/messages
  abc:
    upstream: "https://api.opencode.com"
    protocol: anthropic-messages
    headers:
      User-Agent: "claude-cli/2.1.221 (external, cli)"
      x-app: "cli"
      anthropic-beta: "claude-code-20250219"
    # proxy: "socks5://127.0.0.1:1080"   # 可选：只对本 alias 生效，覆盖全局

  # 例 3：任意客户端 -> OpenAI Responses 上游（Codex 原生协议）
  #   POST /co/v1/chat/completions -> https://api.responses.com/v1/responses
  co:
    upstream: "https://api.responses.com"
    protocol: openai-responses

  # 例 4：简写字符串形式（协议从路径推断，识别不出时默认 openai-chat）
  claude: "https://api.anthropic.com/v1/messages"

  # 例 5：自定义上游路径
  local:
    upstream: "http://127.0.0.1:1234"
    protocol: openai-chat
    path: "/v1/custom/completions"
```

### 字段说明

| 字段 | 必填 | 默认 | 说明 |
|---|---|---|---|
| `upstream` | 是 | - | 上游 base URL（可带路径） |
| `protocol` | 否 | `openai-chat` | 上游说的协议：`openai-chat` / `openai-responses` / `anthropic-messages`；兼容短名 `chat` / `responses` / `messages`（自动归一化，老配置不用改） |
| `path` | 否 | 按协议取标准路径 | 覆盖上游端点路径（如 `/v1/custom/completions`） |
| `max_concurrency` | 否 | `0`（不限制） | 同一 alias 同时打到上游的最大请求数；推荐 2~4 |
| `concurrency_mode` | 否 | `reject` | 超限策略：`reject` 立即返回 429（保护上游）；`queue` 排队等待 |
| `headers` | 否 | 空 | 自定义请求头 map，最后应用、优先级最高（可伪装成 Claude Code / Codex 等客户端） |
| `proxy` | 否 | 空 | 该 alias 的上游代理（覆盖全局 `proxy`），支持 `http`/`https`/`socks5`/`socks5h` |
| `thinking` | 否 | `false` | 开启 Anthropic extended thinking 桥接（Responses reasoning ↔ Messages thinking，请求+响应+流式）。要求上游支持 extended thinking；默认关，关闭时 reasoning/thinking 内容按旧行为丢弃 |

`protocol` 的规范名是 `openai-chat` / `openai-responses` / `anthropic-messages`；短名
`chat` / `responses` / `messages` 兼容并自动归一化。简写字符串形式下协议从 URL 路径推断。

### 排查 401 "unauthorized client detected"

这类报错是上游（多为第三方 router/中转）在做"客户端校验"——它要求请求看起来来自官方
Codex / Claude Code。解决：

1. 在对应 alias 配 `headers` 伪装（见"自定义头"小节），并确认用的是**最新二进制**（老版本
   不认 `headers` 字段）。
2. 开 `debug: true`，重启后看日志里 `[debug] alias=... 上游请求` 打印的**实际发给上游的头**
   （Authorization 会打码），确认 `User-Agent` / `x-app` / `anthropic-beta` 等伪装头确实到位。
3. 确认客户端实际走的是你配了 `headers` 的那个 alias 路径。

## 上游代理（HTTP / SOCKS5）

支持给上游走 HTTP/HTTPS 代理或 SOCKS5 代理（Go 标准库原生支持，无需额外依赖）：

- **全局默认**：顶层 `proxy`，对所有未单独配置的 alias 生效。
- **每 alias 覆盖**：alias 里的 `proxy` 覆盖全局。
- 格式：`http://host:port`、`https://host:port`、`socks5://host:port`（或 `socks5h://`，
  让代理端做 DNS 解析）；URL 可带账号密码（`http://user:pass@host:port`）。

```yaml
proxy: "http://127.0.0.1:7890"        # 全局默认
aliases:
  llm:
    upstream: "https://chat.llm.com"
    protocol: openai-chat             # 走全局代理
  abc:
    upstream: "https://api.opencode.com"
    protocol: anthropic-messages
    proxy: "socks5://127.0.0.1:1080"  # 覆盖：这个 alias 走 SOCKS5
```

## 并发限制（保护上游）

`max_concurrency` 限制同一 alias 同时打到上游的请求数，防止流量把上游打满：

- `reject`（默认）：超限的请求立即收到 **429**（错误消息按客户端协议格式化，Codex/Claude Code 能正常解析，客户端会自己退避重试）。
- `queue`：排队等待空闲槽位（客户端断开时自动取消，不会卡死）。
- 槽位覆盖整个请求生命周期（含流式，真正限制的是"同时打到上游的连接数"）。

```yaml
aliases:
  llm:
    upstream: "https://chat.llm.com"
    protocol: openai-chat
    max_concurrency: 4        # 0 = 不限制（默认）
    concurrency_mode: reject  # reject(默认) | queue
```

## 协议头适配（自动）与自定义头（可选）

### 请求头（按上游协议自动调整）

- 上游是 `anthropic-messages`：剥掉 OpenAI SDK 专属头（`openai-*`、`x-stainless-*`），
  补上必填的 `anthropic-version: 2023-06-01`，并把 `Authorization: Bearer` 镜像成 `x-api-key`。
- 上游是 `openai-chat` / `openai-responses`：剥掉 Anthropic 专属头（`anthropic-version`、
  `anthropic-beta`、`x-app`），并把 `x-api-key` 转成 `Authorization: Bearer`。
- 客户端自带的 `User-Agent` 会透传（不再被覆盖成 `triproxy/1.0`）。

### 响应头（自动适配）

- `Retry-After` 原样透传（429 退避需要）。
- rate-limit 头按客户端协议映射：Anthropic 上游的 `anthropic-ratelimit-*` → Codex 客户端的
  `x-ratelimit-*`，反之亦然；`request-id` 等保留用于和上游日志对账。

### 自定义头（可伪装成特定客户端）

某些中转只认官方 Claude Code / Codex 的 UA，可给 alias 配 `headers`，最后应用、优先级最高：

```yaml
# 上游是 Anthropic Messages -> 伪装成 Claude Code
aliases:
  sensenova:
    upstream: "https://...sensenova..."
    protocol: anthropic-messages
    headers:
      User-Agent: "claude-cli/2.1.221 (external, cli)"
      x-app: "cli"
      anthropic-beta: "claude-code-20250219"

# 上游是 OpenAI Responses -> 伪装成 Codex
aliases:
  codex_upstream:
    upstream: "https://...openai-responses..."
    protocol: openai-responses
    headers:
      User-Agent: "codex_cli_rs/0.139.0 (Linux 6.5.0; x86_64)"
```

> 注意：伪装成官方客户端可能违反上游服务条款，请自行评估；版本字符串也会过时，需自行维护。

## 内存占用说明

空载 RSS 约 6MB。内存会随"并发在途请求 × 请求时长"增长（非流式请求会缓冲完整上游响应，
流式请求持有连接直到结束），Go 的 GC 不会立刻把内存还给系统，所以高并发慢上游时 RSS 可能到
百 MB 量级——这是工作量导致的，不是泄漏。控制手段：

- 给慢/限流的上游配 `max_concurrency`（推荐 2~4，`reject` 模式），并发长请求数量封顶，内存自然下降。
- `triproxy.service` 已内置 `GOMEMLIMIT=256MiB` + `GOGC=50`，把堆封顶并主动归还内存；可按需调整。
- 如果上游响应就是慢（分钟级），调整 `ResponseHeaderTimeout`（默认 120s）避免误伤。

## 安装为 Linux 系统服务（systemd）

项目里附带了 `triproxy.service`。路径约定：**二进制 `/usr/local/bin/triproxy`，配置目录 `/etc/triproxy/`**。

```bash
# 1. 部署二进制与配置
sudo cp triproxy-linux-amd64 /usr/local/bin/triproxy
sudo chmod +x /usr/local/bin/triproxy
sudo mkdir -p /etc/triproxy
sudo cp config.yaml /etc/triproxy/config.yaml

# 2. 创建专用运行用户（避免以 root 运行）
sudo useradd -r -s /usr/sbin/nologin triproxy
sudo chown -R triproxy:triproxy /etc/triproxy

# 3. 安装并启用服务
sudo cp triproxy.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now triproxy

# 4. 检查状态与日志
systemctl status triproxy
journalctl -u triproxy -f
```

修改 `config.yaml` 后重启：`sudo systemctl restart triproxy`。

### 一键安装（install.sh）

从 GitHub Release 发布页下载 `install.sh`（或从仓库获取），默认**自动从 GitHub Release 下载**
与当前系统匹配的 Linux 二进制并部署为 systemd 服务（自动建用户、装配置、装服务、启动）：

```bash
# 下载最新版并安装（需要 root）
sudo ./install.sh

# 指定版本
sudo ./install.sh --version v1.2.0

# 使用本地放好的二进制（脚本同目录或 dist/ 下），不联网
sudo ./install.sh --local
```

支持的参数：

| 参数 | 说明 |
|---|---|
| `--version <tag>` | 下载指定版本（默认 `latest`，即最新 Release） |
| `--local` | 使用本地二进制（脚本同目录或 `dist/`），不下载 |
| `--yes` / `-y` | 跳过确认提示 |
| `--dry-run` | 只打印将要执行的动作，不改动系统 |
| `--uninstall` | 卸载（停服务 + 删除二进制/服务文件） |

支持的平台（Linux + systemd）：Debian/Ubuntu (apt)、RHEL/CentOS/Fedora/Rocky/AlmaLinux (yum/dnf)、
Arch/Manjaro (pacman)、openSUSE (zypper)、Alpine (apk，需装 bash)；
架构覆盖 amd64 / arm64 / 386 / arm / riscv64 / loong64 / ppc64le / s390x。

> 说明：下载模式需要仓库已在 GitHub 发布对应版本（打 tag 触发 CI 发布）；无网络/内网环境请用
> `--local` 离线安装，或从 Release 手动下载对应平台的二进制。

## 客户端接入示例

### Codex CLI -> 只支持 Chat 的上游（协议转换）

```bash
export OPENAI_BASE_URL="http://localhost:8866/llm/v1"
export OPENAI_API_KEY="你的 key（会透传给上游）"
codex
```

### Claude Code -> OpenAI Chat 上游

让 Claude Code 走代理（上游协议设为 `openai-chat`），它会用 `/v1/messages` 路径调用，代理转成 Chat 协议发给上游：

```bash
export ANTHROPIC_BASE_URL="http://localhost:8866/llm"
export ANTHROPIC_API_KEY="你的 key"
claude
```

## 转换细节与限制

实现的转换：

- 消息（system / user / assistant / tool）、文本内容、图片（base64 data URL）。
- **文档**：Responses `input_file` ↔ Anthropic `document`（base64 / url 两种 source）。
- 函数调用：`tool_calls` ↔ `tool_use` ↔ `function_call`，含 `tool_result` / `function_call_output` 的顺序保持。
- **工具结果媒体**：`function_call_output` / `tool_result` 的内容数组（text + image + file）结构化保留，
  不再压平成字符串。
- 工具声明与 `tool_choice` 的三种协议形态互转。
- **并行工具开关**：Responses `parallel_tool_calls` ↔ Anthropic `disable_parallel_tool_use`。
- 采样参数（`temperature`、`top_p`、`max_tokens`/`max_output_tokens`）互转。
- usage 计数互转（prompt/completion ↔ input/output）。
- **reasoning 桥接**（`thinking: true` 时）：`reasoning.effort` ↔ `thinking` 预算；
  reasoning item ↔ thinking 块（含加密内容的无损信封往返）；流式 `reasoning_summary_text.delta` ↔ `thinking_delta`；
  Chat 的 `reasoning_content`（DeepSeek/Qwen 约定）也会桥接为 reasoning item。
- **截断原因**：`max_tokens`/`refusal` 映射为 Responses `incomplete_details.reason`（`max_output_tokens`/`content_filter`）。
- **SSE 流式**（Chat 的 `data:` 块 / Responses 的 `response.*` 事件 / Anthropic 的 `content_block_*` 事件）双向转换。
- **Responses 事件完整性**：所有下发事件带递增 `sequence_number`；多工具并行调用各自独立完成。
- **`/responses/compact`**：上游是 Responses 时透传；其他上游返回明确 501（Codex 长会话压缩需要）。
- **上游错误转换**：非 2xx（如 429）保留状态码，但把错误 body 转成客户端协议的格式——
  流式 Codex/Claude 客户端收到 SSE `error` 事件，非流式收到 JSON 错误对象，避免
  Codex 因收到上游格式（如 Anthropic 的 `{"type":"error","error":{...}}`）解析不了而卡住。
- **请求日志**：每次请求打印 `method path status dur bytes remote`，如
  `请求 method=POST path=/llm/v1/responses status=429 dur=3ms bytes=84 remote=127.0.0.1:53102`。
- 上游错误读取带 30s 兜底超时 + `ResponseHeaderTimeout=120s`，防止上游挂起把代理拖死。
- 不把客户端的 `Accept-Encoding` 转发给上游；若上游仍返回 gzip 响应，会自动解压后再转换，
  避免转换器拿到 gzip 字节报 `invalid character '\x1f'`（gzip 魔数）之类的错误。

限制与说明：

- 上游为 Messages 协议时，`max_tokens` 是必填项；若客户端没传，会注入默认值 `4096`。
- 跨协议转换只携带双方都有的标准字段；provider 自定义参数仅在"同协议透传"时保留。
- 图片：支持 data URL（转成 Anthropic base64）；普通 http(s) 图片 URL 不会去抓取，转换到 Messages 时会被丢弃。
- Chat 协议没有标准文档/并行工具开关/推理输出字段：这些能力只在 Responses ↔ Messages 之间无损，
  Chat 侧按协议限制降级（文档丢弃、并行开关忽略、推理仅从 `reasoning_content` 尽力解析）。
- `thinking` 要求上游支持 Anthropic extended thinking；官方 Anthropic API 对 thinking 签名校验严格，
  若开启后收到 400，请关闭该开关（关闭后仍保留 thinking→reasoning 的摘要级转换）。
- reasoning 桥接的信封往返设计（reasoning item ↔ thinking 签名 / `redacted_thinking.data`）受
  [cc-switch](https://github.com/farion1231/cc-switch)（MIT License）启发，triproxy 为独立实现。
- `tool_choice` 的近似映射：`required → any`，`none → auto`（Anthropic 没有完全等价的枚举）。
- rate-limit 头映射为尽力而为：请求数准确，token 数两边口径不同（OpenAI 总量 vs Anthropic 分 input/output），
  `x-ratelimit-remaining-tokens` 近似映射到 `input-tokens-remaining`。
- 不支持流式的上游若收到 `stream: true`，代理会尝试把它整包响应转成流式事件发回给客户端。

## 测试

```bash
go test ./...   # 单元测试 + 用 mock 上游做的端到端集成测试
```

集成测试覆盖了：Responses→Chat、Responses→Messages、Chat→Messages 的非流式和流式场景、同协议透传、
鉴权透传、错误透传（含 429 转换）、请求/响应头适配与映射、并发限制（reject/queue）、未知 alias、`/v1/models`、
reasoning 桥接（参数/信封/流式双向）、文档、工具结果媒体、并行工具开关、`/responses/compact`。
