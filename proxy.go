package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

var hopHeaders = map[string]bool{
	"Connection":          true,
	"Keep-Alive":          true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
	"Te":                  true,
	"Trailer":             true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
	"Host":                true,
}

type Proxy struct {
	cfg      *Config
	clients  map[string]*http.Client // per-alias upstream clients (proxy-aware)
	limiters map[string]*limiter
}

func NewProxy(cfg *Config) *Proxy {
	p := &Proxy{cfg: cfg}
	p.clients = make(map[string]*http.Client, len(cfg.Aliases))
	for name, a := range cfg.Aliases {
		proxy := a.Proxy
		if proxy == "" {
			proxy = cfg.Proxy
		}
		p.clients[name] = newHTTPClient(proxy)
	}
	p.limiters = make(map[string]*limiter, len(cfg.Aliases))
	for name, a := range cfg.Aliases {
		if a.MaxConcurrency > 0 {
			p.limiters[name] = newLimiter(a.MaxConcurrency, a.ConcurrencyMode)
		}
	}
	return p
}

// newHTTPClient builds an upstream HTTP client. proxy supports http/https
// (CONNECT) and socks5/socks5h (both handled natively by net/http).
func newHTTPClient(proxy string) *http.Client {
	tr := &http.Transport{
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		DisableCompression:    true,
		ResponseHeaderTimeout: 120 * time.Second,
	}
	if proxy != "" {
		if u, err := url.Parse(proxy); err == nil {
			tr.Proxy = http.ProxyURL(u)
		}
	}
	return &http.Client{Timeout: 0, Transport: tr} // streaming needs no overall timeout
}

// clientFor returns the upstream client for an alias (built in NewProxy).
func (p *Proxy) clientFor(name string) *http.Client {
	if c := p.clients[name]; c != nil {
		return c
	}
	return http.DefaultClient
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	sw := &statusWriter{ResponseWriter: w}
	start := time.Now()
	p.serve(sw, r)
	p.logRequest(r.Method, r.URL.Path, sw.status, time.Since(start).Round(time.Millisecond), sw.bytes, r.RemoteAddr)
}

// logRequest prints one fixed-width access-log line. Columns use constant
// widths so alignment never shifts as values grow; every value sits right
// after "key=" and the padding trails to the right. Over-long paths are
// clipped with "..." (remote is the last column and is not padded).
func (p *Proxy) logRequest(method, path string, status int, dur time.Duration, bytes int64, remote string) {
	log.Printf("请求 method=%-7s path=%-36s status=%-3d dur=%-9s bytes=%-9d remote=%s",
		method, clip(path, 36), status, dur.String(), bytes, remote)
}

// clip truncates s to at most n runes, appending "..." when it had to cut, so
// the fixed-width access-log columns never overflow. n < 3 falls back to 3.
func clip(s string, n int) string {
	if n < 3 {
		n = 3
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-3]) + "..."
}

// serve contains the routing logic; ServeHTTP wraps it with access logging.
func (p *Proxy) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/" {
		p.handleRoot(w)
		return
	}
	if r.URL.Path == "/healthz" || r.URL.Path == "/health" {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
		return
	}
	// Claude Code 每次会话启动会探测 /{alias}/api/hello（真实 Anthropic API 返回
	// 200 {"status":"ok"}）。这里同样应答，避免客户端因连通性探测 404 出现异常/卡住。
	if strings.HasSuffix(r.URL.Path, "/api/hello") {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
	// /{alias}/v1/{rest}
	if len(parts) < 3 || parts[1] != "v1" {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "unknown path", "path": r.URL.Path})
		return
	}
	aliasName := parts[0]
	rest := strings.Join(parts[2:], "/")
	alias := p.cfg.Aliases[aliasName]
	if alias == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": fmt.Sprintf("alias %q not found", aliasName)})
		return
	}
	if rest == "models" {
		p.proxyModels(w, r, aliasName, alias)
		return
	}
	if rest == "responses/compact" {
		p.handleCompact(w, r, aliasName, alias)
		return
	}
	clientProto := protoFromPath(rest)
	if clientProto == "" {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": fmt.Sprintf("unsupported endpoint %q", rest)})
		return
	}
	p.handle(w, r, aliasName, alias, clientProto)
}

// handleCompact handles POST /{alias}/v1/responses/compact, which Codex calls
// to compact a long conversation. Only an openai-responses upstream can honor
// it; for other upstreams we return a clear error instead of a 404 that Codex
// cannot act on.
func (p *Proxy) handleCompact(w http.ResponseWriter, r *http.Request, aliasName string, alias *Alias) {
	reqID := requestID()
	if alias.Protocol != ProtoOpenAIResponses {
		writeJSON(w, http.StatusNotImplemented, map[string]any{
			"error": "responses/compact requires an openai-responses upstream (this alias speaks " + alias.Protocol + ")",
		})
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "read body: " + err.Error()})
		return
	}
	if p.cfg.Debug {
		head := append([]string{r.Method + " " + r.URL.Path + " " + r.Proto}, debugHeaderLines(r.Header)...)
		p.debugStage(reqID, "triproxy 收到客户端请求：", "请求头：", "请求体：", head, body)
	}
	upReq, err := http.NewRequestWithContext(r.Context(), r.Method,
		strings.TrimRight(alias.Upstream, "/")+"/v1/responses/compact", bytes.NewReader(body))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	copyHeaders(upReq.Header, r.Header)
	upReq.Header.Del("Accept-Encoding")
	upReq.Header.Set("Content-Type", "application/json")
	upReq.Header.Set("Accept", "application/json")
	if upReq.Header.Get("User-Agent") == "" {
		upReq.Header.Set("User-Agent", "triproxy/1.0")
	}
	adaptUpstreamHeaders(upReq.Header, alias.Protocol)
	for k, v := range alias.Headers {
		upReq.Header.Set(k, v)
	}
	if p.cfg.Debug {
		head := append([]string{upReq.Method + " " + upReq.URL.String()}, debugHeaderLines(upReq.Header)...)
		p.debugStage(reqID, "triproxy 请求 LLM (alias="+aliasName+")：", "请求头：", "请求体：", head, body)
	}
	upResp, err := p.clientFor(aliasName).Do(upReq)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "upstream request failed: " + err.Error()})
		return
	}
	defer upResp.Body.Close()
	decompressBody(upResp)
	if p.cfg.Debug {
		p.debugStage(reqID, "triproxy 接收 LLM 响应：", "响应头：", "响应体：",
			append([]string{strconv.Itoa(upResp.StatusCode)}, debugHeaderLines(upResp.Header)...), nil)
	}
	copyRespHeaders(w, upResp)
	if p.cfg.Debug {
		p.debugStage(reqID, "triproxy 响应客户端：", "响应头：", "响应体：",
			append([]string{strconv.Itoa(upResp.StatusCode)}, debugHeaderLines(w.Header())...), nil)
	}
	w.WriteHeader(upResp.StatusCode)
	_, _ = io.Copy(w, upResp.Body)
}

func (p *Proxy) handleRoot(w http.ResponseWriter) {
	type a struct {
		Upstream string `json:"upstream"`
		Protocol string `json:"protocol"`
	}
	aliases := make(map[string]a, len(p.cfg.Aliases))
	for name, al := range p.cfg.Aliases {
		aliases[name] = a{Upstream: al.Upstream, Protocol: al.Protocol}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"service": "triproxy",
		"endpoints": []string{
			"/{alias}/v1/chat/completions",
			"/{alias}/v1/responses",
			"/{alias}/v1/messages",
			"/{alias}/v1/models",
		},
		"aliases": aliases,
	})
}

func protoFromPath(rest string) string {
	switch rest {
	case "chat/completions":
		return ProtoOpenAIChat
	case "responses":
		return ProtoOpenAIResponses
	case "messages":
		return ProtoAnthropicMessages
	default:
		return ""
	}
}

// reasoningBridgeFor resolves the effective reasoning-bridge flag for a
// request/response pair. The explicit `thinking` config gates the
// Responses↔Messages lossless envelope bridge; separately, a Chat upstream
// feeding a Messages client (Claude Code) must always surface reasoning_content
// as thinking blocks, because Chat providers require the thinking to be echoed
// back on the next turn and reject with 400 otherwise.
func reasoningBridgeFor(clientProto, upstreamProto string, cfg bool) bool {
	if cfg {
		return true
	}
	return upstreamProto == ProtoOpenAIChat && clientProto == ProtoAnthropicMessages
}

func (p *Proxy) handle(w http.ResponseWriter, r *http.Request, aliasName string, alias *Alias, clientProto string) {
	upstreamProto := alias.Protocol
	if upstreamProto == "" {
		upstreamProto = ProtoOpenAIChat
	}
	reqID := requestID()

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "read body: " + err.Error()})
		return
	}
	if p.cfg.Debug {
		head := append([]string{r.Method + " " + r.URL.Path + " " + r.Proto}, debugHeaderLines(r.Header)...)
		p.debugStage(reqID, "triproxy 收到客户端请求：", "请求头：", "请求体：", head, body)
	}
	// Codex signals streaming via the Accept header (Accept: text/event-stream)
	// even when its request body omits "stream": true. Falling back to the
	// header keeps Codex requests streaming even if the body field is absent.
	stream := bodyStreamFlag(body) || acceptStreaming(r.Header)

	if lim := p.limiters[aliasName]; lim != nil {
		if !lim.acquire(r.Context()) {
			if r.Context().Err() != nil {
				return // client disconnected while queued
			}
			writeClientError(w, clientProto, stream, http.StatusTooManyRequests,
				fmt.Sprintf("alias %q 并发超限 (max %d)", aliasName, alias.MaxConcurrency),
				"rate_limit_error")
			return
		}
		defer lim.release()
	}

	upstreamBody := body
	if clientProto != upstreamProto {
		opts := &ConvertOpts{ReasoningBridge: reasoningBridgeFor(clientProto, upstreamProto, alias.Thinking), ForceStream: stream}
		upstreamBody, err = convertRequestOpts(clientProto, upstreamProto, body, opts)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": fmt.Sprintf("request conversion failed: %v", err),
			})
			return
		}
	} else if stream {
		// Same-protocol passthrough: the upstream also needs stream:true in the
		// body even when the client only signaled streaming via Accept.
		upstreamBody = forceStreamInBody(body)
	}

	upReq, err := http.NewRequestWithContext(r.Context(), r.Method, alias.Endpoint(), bytes.NewReader(upstreamBody))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	copyHeaders(upReq.Header, r.Header)
	// 不把客户端的 Accept-Encoding 转发给上游（压缩是 代理↔上游 之间的事，
	// 转发会导致上游 gzip 而代理不负责解压，转换时拿到 gzip 字节而报错）。
	upReq.Header.Del("Accept-Encoding")
	upReq.Header.Set("Content-Type", "application/json")
	if stream {
		upReq.Header.Set("Accept", "text/event-stream")
	} else {
		upReq.Header.Set("Accept", "application/json")
	}
	// 客户端自带 User-Agent 就透传（保留真实客户端身份），否则用默认值。
	if upReq.Header.Get("User-Agent") == "" {
		upReq.Header.Set("User-Agent", "triproxy/1.0")
	}
	// 按上游协议调整头：剥离无关协议的专属头、补必填头、归一化认证。
	adaptUpstreamHeaders(upReq.Header, upstreamProto)
	// reasoning 桥接需要上游支持 Anthropic 扩展思考（thinking）。beta 头由用户
	// 自定义头最后覆盖，允许伪装成 Claude Code 等（它们自带 thinking 相关 beta）。
	if alias.Thinking && upstreamProto == ProtoAnthropicMessages {
		if upReq.Header.Get("anthropic-beta") == "" {
			upReq.Header.Set("anthropic-beta", "thinking-2024-12-04")
		}
	}
	// 用户自定义头最后应用，可覆盖上面的默认行为（如伪装成 Claude Code / Codex）。
	for k, v := range alias.Headers {
		upReq.Header.Set(k, v)
	}
	if p.cfg.Debug {
		head := append([]string{upReq.Method + " " + upReq.URL.String()}, debugHeaderLines(upReq.Header)...)
		p.debugStage(reqID, "triproxy 请求 LLM (alias="+aliasName+")：", "请求头：", "请求体：", head, upstreamBody)
	}

	upResp, err := p.clientFor(aliasName).Do(upReq)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "upstream request failed: " + err.Error()})
		return
	}
	defer upResp.Body.Close()
	decompressBody(upResp)

	// Upstream error responses keep their status code, but the body is
	// converted into the client's protocol so streaming clients (Codex /
	// Claude Code) don't hang on a provider-formatted error they can't parse.
	if upResp.StatusCode < 200 || upResp.StatusCode >= 300 {
		errBody, _ := readErrorBody(upResp)
		if p.cfg.Debug {
			head := append([]string{strconv.Itoa(upResp.StatusCode)}, debugHeaderLines(upResp.Header)...)
			p.debugStage(reqID, "triproxy 接收 LLM 响应：", "响应头：", "响应体：", head, errBody)
		}
		if clientProto == upstreamProto {
			p.debugStage(reqID, "triproxy 响应客户端：", "响应头：", "响应体：",
				append([]string{strconv.Itoa(upResp.StatusCode)}, debugHeaderLines(w.Header())...), errBody)
			relayErrorBytes(w, upResp, errBody)
		} else {
			msg, typ := extractErrorMessage(errBody, upResp.StatusCode)
			applyConvertedRespHeaders(w, upResp, clientProto)
			p.debugStage(reqID, "triproxy 响应客户端：", "响应头：", "响应体：",
				append([]string{strconv.Itoa(upResp.StatusCode)}, debugHeaderLines(w.Header())...), errBody)
			writeClientError(w, clientProto, stream, upResp.StatusCode, msg, typ)
		}
		return
	}

	// Same protocol: relay verbatim, streaming or not.
	if clientProto == upstreamProto {
		copyRespHeaders(w, upResp)
		w.WriteHeader(upResp.StatusCode)
		if p.cfg.Debug {
			p.debugStage(reqID, "triproxy 接收 LLM 响应：", "响应头：", "响应体：",
				append([]string{strconv.Itoa(upResp.StatusCode)}, debugHeaderLines(upResp.Header)...), nil)
			p.debugStage(reqID, "triproxy 响应客户端：", "响应头：", "响应体：",
				append([]string{strconv.Itoa(upResp.StatusCode)}, debugHeaderLines(w.Header())...), nil)
		}
		if strings.Contains(upResp.Header.Get("Content-Type"), "text/event-stream") {
			// 透传流式：不缓冲，逐行转发并 Flush。chat 流额外保证标准收尾
			// （见 relayChatStream）。debug 模式下边转发边解析出内容汇总，
			// 让日志也能看到上游模型实际返回的文本/工具调用。
			var mirror io.Writer
			var done chan struct{}
			if p.cfg.Debug {
				dbg := newStreamDebugger(reqID, true)
				pr, pw := io.Pipe()
				mirror = pw
				done = make(chan struct{})
				go func() {
					defer func() {
						_ = pr.Close()
						close(done)
					}()
					_ = parseUpstreamStream(upstreamProto, pr, func(ev streamEv) error {
						dbg.observe(ev)
						return nil
					})
					dbg.flush()
				}()
			}
			var rerr error
			if clientProto == ProtoOpenAIChat {
				rerr = relayChatStream(w, upResp.Body, mirror)
			} else if mirror != nil {
				_, rerr = copyStream(io.MultiWriter(w, mirror), upResp.Body)
			} else {
				_, rerr = copyStream(w, upResp.Body)
			}
			if mirror != nil {
				_ = mirror.(*io.PipeWriter).Close()
				<-done
			}
			if rerr != nil && r.Context().Err() == nil {
				log.Printf("streaming error (alias=%s): %v", clientProto, rerr)
			}
		} else {
			_, _ = copyStream(w, upResp.Body)
		}
		return
	}

	if stream {
		if p.cfg.Debug {
			head := append([]string{strconv.Itoa(upResp.StatusCode)}, debugHeaderLines(upResp.Header)...)
			p.debugStage(reqID, "triproxy 接收 LLM 响应：", "响应头：", "响应体：", head, nil)
		}
		p.streamConvert(w, r, upResp, clientProto, upstreamProto, modelFromBody(upstreamBody), alias.Thinking, aliasName, reqID)
		return
	}

	// Non-streaming cross-protocol.
	if strings.Contains(upResp.Header.Get("Content-Type"), "text/event-stream") {
		// Upstream streamed anyway; aggregate it into a complete response.
		resp, aerr := aggregateStreamToResp(upstreamProto, modelFromBody(upstreamBody), upResp.Body)
		if aerr != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": "aggregate stream: " + aerr.Error()})
			return
		}
		outBody, cerr := emitResponseTo(clientProto, resp, &ConvertOpts{ReasoningBridge: reasoningBridgeFor(clientProto, upstreamProto, alias.Thinking)})
		if cerr != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": cerr.Error()})
			return
		}
		applyConvertedRespHeaders(w, upResp, clientProto)
		w.Header().Set("Content-Type", "application/json")
		if p.cfg.Debug {
			p.debugStage(reqID, "triproxy 接收 LLM 响应：", "响应头：", "响应体：",
				append([]string{strconv.Itoa(upResp.StatusCode)}, debugHeaderLines(upResp.Header)...), nil)
			p.debugStage(reqID, "triproxy 响应客户端：", "响应头：", "响应体：",
				append([]string{strconv.Itoa(http.StatusOK)}, debugHeaderLines(w.Header())...), outBody)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(outBody)
		return
	}

	upstreamJSON, err := io.ReadAll(upResp.Body)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "read upstream: " + err.Error()})
		return
	}
	if p.cfg.Debug {
		head := append([]string{strconv.Itoa(upResp.StatusCode)}, debugHeaderLines(upResp.Header)...)
		p.debugStage(reqID, "triproxy 接收 LLM 响应：", "响应头：", "响应体：", head, upstreamJSON)
	}
	outBody, err := convertResponseOpts(upstreamProto, clientProto, upstreamJSON, &ConvertOpts{ReasoningBridge: reasoningBridgeFor(clientProto, upstreamProto, alias.Thinking)})
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "response conversion failed: " + err.Error()})
		return
	}
	applyConvertedRespHeaders(w, upResp, clientProto)
	w.Header().Set("Content-Type", "application/json")
	if p.cfg.Debug {
		p.debugStage(reqID, "triproxy 响应客户端：", "响应头：", "响应体：",
			append([]string{strconv.Itoa(http.StatusOK)}, debugHeaderLines(w.Header())...), outBody)
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(outBody)
}

func (p *Proxy) streamConvert(w http.ResponseWriter, r *http.Request, upResp *http.Response, clientProto, upstreamProto, model string, bridge bool, aliasName, reqID string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	applyConvertedRespHeaders(w, upResp, clientProto)
	if p.cfg.Debug {
		p.debugStage(reqID, "triproxy 响应客户端（流式）：", "响应头：", "响应体：",
			append([]string{strconv.Itoa(http.StatusOK)}, debugHeaderLines(w.Header())...), nil)
	}
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	enc := newStreamEncoder(clientProto, w, model, reasoningBridgeFor(clientProto, upstreamProto, bridge))
	// 流式响应体不能缓冲打印；改为在转发的同时记录事件，完成后汇总一行日志。
	var dbg *streamDebugger
	// 逐事件 Flush：Go 的 ResponseWriter 有 4KB 写缓冲，不主动 Flush 的话
	// SSE 事件会攒到 4KB 才发一批，客户端（Claude Code/Codex）界面会长时间
	// 看不到任何输出，看起来像"卡住"。逐事件推流与官方 Anthropic/OpenAI 一致。
	emit := func(ev streamEv) error {
		if err := enc.Write(ev); err != nil {
			return err
		}
		if flusher != nil {
			flusher.Flush()
		}
		return nil
	}
	if p.cfg.Debug {
		dbg = newStreamDebugger(reqID, true)
		wrapped := emit
		emit = func(ev streamEv) error {
			dbg.observe(ev)
			return wrapped(ev)
		}
	}
	var runErr error
	if strings.Contains(upResp.Header.Get("Content-Type"), "text/event-stream") {
		runErr = parseUpstreamStream(upstreamProto, upResp.Body, emit)
	} else {
		// Upstream ignored the stream flag; synthesize events from the body.
		raw, err := io.ReadAll(upResp.Body)
		if err != nil {
			runErr = err
		} else if resp, perr := parseUpstreamResponse(upstreamProto, raw); perr == nil {
			for _, ev := range respToStreamEvents(resp, model) {
				if werr := emit(ev); werr != nil {
					runErr = werr
					break
				}
				if flusher != nil {
					flusher.Flush()
				}
			}
		} else {
			runErr = fmt.Errorf("unparseable upstream response: %v", perr)
		}
	}
	if cerr := enc.Close(); runErr == nil && cerr != nil {
		runErr = cerr
	}
	if dbg != nil {
		dbg.flush()
	}
	if flusher != nil {
		flusher.Flush()
	}
	if runErr != nil && r.Context().Err() == nil {
		log.Printf("streaming error (alias=%s): %v", clientProto, runErr)
	}
}

// copyStream copies r to w and flushes the ResponseWriter after every chunk.
// io.Copy alone would let the response accumulate in Go's 4KB write buffer,
// so SSE data would only reach the client in 4KB bursts (or not at all until
// the stream ends), which reads as "stuck" in streaming clients.
func copyStream(w io.Writer, r io.Reader) (int64, error) {
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	var total int64
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return total, werr
			}
			total += int64(n)
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err == io.EOF {
			return total, nil
		}
		if err != nil {
			return total, err
		}
	}
}

// relayChatStream relays an OpenAI chat SSE stream to the client line by line,
// flushing after every line to keep streaming latency low.
//
// Strict OpenAI consumers (notably new-api's OpenAI channel, which re-emits
// the upstream chat stream as Anthropic for Claude Code) only finalize a
// stream when they see a usage-only trailing chunk (choices:[], usage:{...}),
// which providers send only when the request carries
// stream_options.include_usage. When the provider does not honor that option
// the terminal Anthropic message_delta/message_stop is never emitted and the
// client reports an interrupted conversation. To stay compatible regardless of
// the provider, when the upstream stream reaches [DONE] without having
// delivered a usage-only chunk, a synthesized one is injected first.
//
// mirror, when non-nil, receives the upstream's original lines (without the
// injection) for debug parsing.
func relayChatStream(w io.Writer, r io.Reader, mirror io.Writer) error {
	flusher, _ := w.(http.Flusher)
	br := bufio.NewReader(r)
	seenUsage := false
	for {
		line, err := br.ReadString('\n')
		if len(line) > 0 {
			trimmed := strings.TrimSpace(line)
			payload := ""
			if strings.HasPrefix(trimmed, "data:") {
				payload = strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
			}
			if payload == "[DONE]" && !seenUsage {
				if werr := writeSSE(w, "", map[string]any{
					"choices": []any{},
					"usage": map[string]any{
						"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0,
					},
				}); werr != nil {
					return werr
				}
				if flusher != nil {
					flusher.Flush()
				}
			} else if payload != "" && payload != "[DONE]" {
				var chunk struct {
					Choices []any           `json:"choices"`
					Usage   json.RawMessage `json:"usage"`
				}
				if json.Unmarshal([]byte(payload), &chunk) == nil &&
					len(chunk.Choices) == 0 && len(chunk.Usage) > 0 {
					seenUsage = true
				}
			}
			if _, werr := io.WriteString(w, line); werr != nil {
				return werr
			}
			if flusher != nil {
				flusher.Flush()
			}
			if mirror != nil {
				// 调试镜像只是"顺带"解析汇总，绝不能因为镜像管道出错/卡住
				// 而中断给客户端的转发。
				_, _ = io.WriteString(mirror, line)
			}
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

func (p *Proxy) proxyModels(w http.ResponseWriter, r *http.Request, aliasName string, alias *Alias) {
	reqID := requestID()
	if p.cfg.Debug {
		head := append([]string{r.Method + " " + r.URL.Path + " " + r.Proto}, debugHeaderLines(r.Header)...)
		p.debugStage(reqID, "triproxy 收到客户端请求：", "请求头：", "请求体：", head, nil)
	}
	upReq, err := http.NewRequestWithContext(r.Context(), r.Method, alias.ModelsEndpoint(), nil)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	copyHeaders(upReq.Header, r.Header)
	upReq.Header.Del("Accept-Encoding")
	upReq.Header.Set("Accept", "application/json")
	if upReq.Header.Get("User-Agent") == "" {
		upReq.Header.Set("User-Agent", "triproxy/1.0")
	}
	adaptUpstreamHeaders(upReq.Header, alias.Protocol)
	for k, v := range alias.Headers {
		upReq.Header.Set(k, v)
	}
	if p.cfg.Debug {
		head := append([]string{upReq.Method + " " + upReq.URL.String()}, debugHeaderLines(upReq.Header)...)
		p.debugStage(reqID, "triproxy 请求 LLM (alias="+aliasName+")：", "请求头：", "请求体：", head, nil)
	}
	upResp, err := p.clientFor(aliasName).Do(upReq)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "upstream request failed: " + err.Error()})
		return
	}
	defer upResp.Body.Close()
	decompressBody(upResp)
	modelsBody, err := io.ReadAll(upResp.Body)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "read upstream: " + err.Error()})
		return
	}
	if p.cfg.Debug {
		p.debugStage(reqID, "triproxy 接收 LLM 响应：", "响应头：", "响应体：",
			append([]string{strconv.Itoa(upResp.StatusCode)}, debugHeaderLines(upResp.Header)...), modelsBody)
	}
	copyRespHeaders(w, upResp)
	if p.cfg.Debug {
		p.debugStage(reqID, "triproxy 响应客户端：", "响应头：", "响应体：",
			append([]string{strconv.Itoa(upResp.StatusCode)}, debugHeaderLines(w.Header())...), modelsBody)
	}
	w.WriteHeader(upResp.StatusCode)
	_, _ = w.Write(modelsBody)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func copyHeaders(dst, src http.Header) {
	for k, vv := range src {
		if hopHeaders[k] {
			continue
		}
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

// adaptUpstreamHeaders adjusts outgoing headers to fit the upstream protocol:
// it strips headers belonging to a different protocol's SDK and normalizes the
// authentication header. This keeps a Codex request from leaking OpenAI
// specific headers into Anthropic (and vice versa) and ensures the upstream
// receives auth in the form it accepts.
func adaptUpstreamHeaders(h http.Header, upstreamProto string) {
	switch upstreamProto {
	case ProtoAnthropicMessages:
		// 去掉 OpenAI 客户端专属头（openai-beta / openai-organization / x-stainless-* 等）
		h.Del("Openai-Beta")
		h.Del("Openai-Organization")
		h.Del("Openai-Project")
		for k := range h {
			if strings.HasPrefix(strings.ToLower(k), "x-stainless-") {
				h.Del(k)
			}
		}
		// anthropic-version 是 Anthropic 必填头，客户端没带就补默认值
		if h.Get("Anthropic-Version") == "" {
			h.Set("Anthropic-Version", "2023-06-01")
		}
		// 认证：Anthropic 同时认 Authorization: Bearer 和 x-api-key。
		// Codex 客户端只带 Authorization，这里补 x-api-key 以兼容只看 x-api-key 的中转。
		if h.Get("X-Api-Key") == "" {
			if auth := h.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
				h.Set("X-Api-Key", strings.TrimPrefix(auth, "Bearer "))
			}
		}
	case ProtoOpenAIChat, ProtoOpenAIResponses:
		// 去掉 Anthropic 客户端专属头
		h.Del("Anthropic-Version")
		h.Del("Anthropic-Beta")
		h.Del("X-App")
		// 认证：OpenAI 只认 Authorization: Bearer。Claude Code 只带 x-api-key，
		// 这里转成 Authorization 并移除 x-api-key。
		if h.Get("Authorization") == "" {
			if key := h.Get("X-Api-Key"); key != "" {
				h.Set("Authorization", "Bearer "+key)
			}
		}
		h.Del("X-Api-Key")
	}
}

// genericRespHeaders are protocol-agnostic response headers worth forwarding
// for correlation and backoff (Retry-After drives client-side 429 backoff).
var genericRespHeaders = []string{
	"X-Request-Id", "Request-Id", "Openai-Organization", "Openai-Processing-Ms",
	"Anthropic-Organization-Id", "Retry-After",
}

// rlPairs maps the main rate-limit headers between OpenAI and Anthropic naming
// (best effort; the reset/timestamp fields are not 1:1 and pass through as-is).
var rlPairs = []struct{ openai, anthropic string }{
	{"X-Ratelimit-Limit-Requests", "Anthropic-Ratelimit-Requests-Limit"},
	{"X-Ratelimit-Remaining-Requests", "Anthropic-Ratelimit-Requests-Remaining"},
	{"X-Ratelimit-Limit-Tokens", "Anthropic-Ratelimit-Input-Tokens-Limit"},
	{"X-Ratelimit-Remaining-Tokens", "Anthropic-Ratelimit-Input-Tokens-Remaining"},
}

// copyRespHeaders is used when relaying an upstream response verbatim
// (same-protocol passthrough, error relay, /models): it copies the upstream
// Content-Type, generic headers and both rate-limit formats unchanged.
func copyRespHeaders(w http.ResponseWriter, upResp *http.Response) {
	if v := upResp.Header.Get("Content-Type"); v != "" {
		w.Header().Set("Content-Type", v)
	}
	for _, h := range genericRespHeaders {
		if v := upResp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	copyRateLimitHeaders(w.Header(), upResp.Header, "")
}

// applyConvertedRespHeaders copies correlation and rate-limit headers onto a
// cross-protocol converted response, translating rate-limit headers into the
// client protocol's naming. Content-Type is set by the caller.
func applyConvertedRespHeaders(w http.ResponseWriter, upResp *http.Response, clientProto string) {
	for _, h := range genericRespHeaders {
		if v := upResp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	copyRateLimitHeaders(w.Header(), upResp.Header, clientProto)
}

// copyRateLimitHeaders forwards upstream rate-limit headers, preferring the
// client protocol's format and falling back to the other format's value.
// clientProto "" means same-protocol: keep both formats unchanged.
func copyRateLimitHeaders(dst, src http.Header, clientProto string) {
	for _, h := range []string{
		"X-Ratelimit-Reset-Requests", "X-Ratelimit-Reset-Tokens",
		"Anthropic-Ratelimit-Reset", "Anthropic-Ratelimit-Created-At",
		"Anthropic-Ratelimit-Output-Tokens-Limit", "Anthropic-Ratelimit-Output-Tokens-Remaining",
	} {
		if v := src.Get(h); v != "" {
			dst.Set(h, v)
		}
	}
	for _, p := range rlPairs {
		openai, anthropic := src.Get(p.openai), src.Get(p.anthropic)
		switch clientProto {
		case ProtoAnthropicMessages:
			if v := firstNonEmpty(anthropic, openai); v != "" {
				dst.Set(p.anthropic, v)
			}
		case ProtoOpenAIChat, ProtoOpenAIResponses:
			if v := firstNonEmpty(openai, anthropic); v != "" {
				dst.Set(p.openai, v)
			}
		default: // "" same-protocol passthrough: keep both formats
			if openai != "" {
				dst.Set(p.openai, openai)
			}
			if anthropic != "" {
				dst.Set(p.anthropic, anthropic)
			}
		}
	}
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// decompressBody transparently decompresses a gzip upstream response. We never
// advertise Accept-Encoding, but some providers compress regardless; without
// this the converter would try to JSON-parse raw gzip bytes (the '\x1f' error).
func decompressBody(upResp *http.Response) {
	if !strings.EqualFold(upResp.Header.Get("Content-Encoding"), "gzip") {
		return
	}
	zr, err := gzip.NewReader(upResp.Body)
	if err != nil {
		return // 不是有效的 gzip，保持原样让后续流程报错更明确
	}
	upResp.Body = zr
	upResp.Header.Del("Content-Encoding")
	upResp.Header.Del("Content-Length")
}

// requestID returns a short random id that correlates the four debug stages of
// one proxied request.
func requestID() string {
	return genID("req")
}

// debugHeaderLines returns a sorted, secret-redacted list of header lines.
func debugHeaderLines(h http.Header) []string {
	lines := make([]string, 0, len(h))
	for k, vv := range h {
		for _, v := range vv {
			if isSecretHeader(k) {
				v = "[REDACTED]"
			}
			lines = append(lines, k+": "+v)
		}
	}
	sort.Strings(lines)
	return lines
}

// debugStage prints one stage of a proxied request — client request, upstream
// request, upstream response or client response — keyed by request id. head is
// the method/status line plus header lines; body may be nil for streaming
// responses, whose content is summarized separately.
func (p *Proxy) debugStage(reqID, title, headLabel, bodyLabel string, head []string, body []byte) {
	if !p.cfg.Debug {
		return
	}
	var sb strings.Builder
	sb.WriteString("[debug] ")
	sb.WriteString(reqID)
	sb.WriteString(" ")
	sb.WriteString(title)
	if len(head) > 0 {
		sb.WriteString("\n    ")
		sb.WriteString(headLabel)
		for _, l := range head {
			sb.WriteString("\n    ")
			sb.WriteString(l)
		}
	}
	if body != nil {
		sb.WriteString("\n    ")
		sb.WriteString(bodyLabel)
		sb.WriteString(" (")
		sb.WriteString(strconv.Itoa(len(body)))
		sb.WriteString(" bytes):\n")
		sb.WriteString(debugBody(body))
	}
	log.Print(sb.String())
}

// debugBodyLimit caps how much of a (formatted) request/response body is
// printed. Real Codex requests can exceed 100KB as the conversation grows.
const debugBodyLimit = 64 << 10

func debugBody(body []byte) string {
	if len(body) == 0 {
		return "(empty)"
	}
	// Pretty-print JSON bodies so tool schemas and message arrays are readable;
	// anything that is not valid JSON (SSE, plain text) is printed verbatim.
	out := body
	var buf bytes.Buffer
	if json.Indent(&buf, body, "", "  ") == nil {
		out = buf.Bytes()
	}
	if len(out) <= debugBodyLimit {
		return string(out)
	}
	return string(out[:debugBodyLimit]) +
		fmt.Sprintf("\n... (截断，原始 %d 字节，格式化后 %d 字节)", len(body), len(out))
}

// streamDebugger records canonical streaming events while they are forwarded,
// so debug mode can print what the model actually produced without buffering
// the response body (buffering would break streaming).
type toolCallLog struct {
	name string
	id   string
	args strings.Builder
}

type streamDebugger struct {
	reqID        string
	debug        bool
	text         strings.Builder
	reasoning    strings.Builder
	toolCalls    []*toolCallLog
	finishReason string
	usage        *Usage
	flushed      bool
}

func newStreamDebugger(reqID string, debug bool) *streamDebugger {
	return &streamDebugger{reqID: reqID, debug: debug}
}

func (d *streamDebugger) observe(ev streamEv) {
	switch ev.kind {
	case evText:
		d.text.WriteString(ev.text)
	case evReasoning:
		d.reasoning.WriteString(ev.reasoningText)
	case evToolBegin:
		d.toolCalls = append(d.toolCalls, &toolCallLog{name: ev.toolName, id: ev.toolID})
	case evToolArg:
		if len(d.toolCalls) > 0 {
			d.toolCalls[len(d.toolCalls)-1].args.WriteString(ev.toolArgs)
		}
	case evDone:
		d.finishReason = ev.finishReason
		d.usage = ev.usage
	case evError:
		d.finishReason = "error"
	}
}

// summary builds the one-line (plus content) stream summary.
func (d *streamDebugger) summary() string {
	var parts []string
	if d.finishReason != "" {
		parts = append(parts, "finish="+d.finishReason)
	}
	if d.usage != nil {
		parts = append(parts, fmt.Sprintf("usage={in:%d,out:%d,total:%d}",
			d.usage.InputTokens, d.usage.OutputTokens, total(d.usage)))
	}
	if len(d.toolCalls) > 0 {
		var tools []string
		for _, tc := range d.toolCalls {
			s := tc.name + "(" + tc.id + ")"
			if tc.args.Len() > 0 {
				s += " args=" + quoteTrunc(tc.args.String())
			}
			tools = append(tools, s)
		}
		parts = append(parts, "tools=["+strings.Join(tools, ", ")+"]")
	}
	head := strings.Join(parts, " ")
	if head != "" {
		head = " " + head
	}
	var content []string
	if d.reasoning.Len() > 0 {
		content = append(content, "推理: "+quoteTrunc(d.reasoning.String()))
	}
	if d.text.Len() > 0 {
		content = append(content, "文本: "+quoteTrunc(d.text.String()))
	}
	msg := "流式内容" + head
	if len(content) > 0 {
		msg += "\n    " + strings.Join(content, "\n    ")
	}
	return msg
}

// flush prints the accumulated stream summary once.
func (d *streamDebugger) flush() {
	if !d.debug || d.flushed {
		return
	}
	d.flushed = true
	log.Print("[debug] " + d.reqID + " " + d.summary())
}

// streamLogContentLimit caps each printed content line.
const streamLogContentLimit = 4000

func quoteTrunc(s string) string {
	if len(s) <= streamLogContentLimit {
		return strconv.Quote(s)
	}
	return strconv.Quote(s[:streamLogContentLimit]) + fmt.Sprintf(" ... (共 %d 字)", len(s))
}

func isSecretHeader(name string) bool {
	switch strings.ToLower(name) {
	case "authorization", "x-api-key", "proxy-authorization", "cookie":
		return true
	}
	return false
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func bodyStreamFlag(body []byte) bool {
	var m map[string]any
	if json.Unmarshal(body, &m) == nil {
		if b, ok := m["stream"].(bool); ok {
			return b
		}
	}
	return false
}

// acceptStreaming reports whether the client asked for an SSE stream via the
// Accept header. Codex's Responses client always sends text/event-stream on
// streaming requests.
func acceptStreaming(h http.Header) bool {
	return strings.Contains(h.Get("Accept"), "text/event-stream")
}

// forceStreamInBody rewrites the top-level "stream" field of a JSON body to
// true. The body is returned unchanged when it is not JSON or already has
// stream:true.
func forceStreamInBody(body []byte) []byte {
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		return body
	}
	if b, ok := m["stream"].(bool); ok && b {
		return body
	}
	m["stream"] = true
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

func modelFromBody(body []byte) string {
	var m map[string]any
	if json.Unmarshal(body, &m) == nil {
		if s, ok := m["model"].(string); ok {
			return s
		}
	}
	return ""
}

func parseUpstreamResponse(proto string, body []byte) (*Resp, error) {
	switch proto {
	case ProtoOpenAIResponses:
		return parseResponsesResp(body)
	case ProtoAnthropicMessages:
		return parseMessagesResp(body)
	default:
		return parseChatResp(body)
	}
}

func emitResponseTo(proto string, resp *Resp, opts *ConvertOpts) ([]byte, error) {
	switch proto {
	case ProtoOpenAIResponses:
		return emitResponsesResp(resp, opts)
	case ProtoAnthropicMessages:
		return emitMessagesResp(resp, opts)
	default:
		return emitChatResp(resp)
	}
}

// aggregateStreamToResp consumes an upstream SSE stream and folds it into a
// single canonical response.
func aggregateStreamToResp(proto, model string, r io.Reader) (*Resp, error) {
	resp := &Resp{Model: model, Object: "aggregated"}
	var text strings.Builder
	var toolCalls []ContentBlock
	var reasoning []ContentBlock
	var usage *Usage
	err := parseUpstreamStream(proto, r, func(ev streamEv) error {
		switch ev.kind {
		case evText:
			text.WriteString(ev.text)
		case evReasoning:
			blk := ContentBlock{Type: "reasoning", ReasoningSummary: ev.reasoningText}
			if ev.reasoningRedacted {
				blk.ThinkingIsRedacted = true
			}
			if n := len(reasoning); n > 0 && reasoning[n-1].Type == "reasoning" && !reasoning[n-1].ThinkingIsRedacted {
				reasoning[n-1].ReasoningSummary += ev.reasoningText
			} else {
				reasoning = append(reasoning, blk)
			}
		case evToolBegin:
			toolCalls = append(toolCalls, ContentBlock{
				Type: "tool_call", ToolCallID: ev.toolID, ToolName: ev.toolName,
			})
		case evToolArg:
			if len(toolCalls) > 0 {
				toolCalls[len(toolCalls)-1].ToolArgsText += ev.toolArgs
			}
		case evDone:
			resp.FinishReason = ev.finishReason
			if ev.usage != nil {
				usage = ev.usage
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	var blocks []ContentBlock
	blocks = append(blocks, reasoning...)
	if text.Len() > 0 {
		blocks = append(blocks, ContentBlock{Type: "text", Text: text.String()})
	}
	blocks = append(blocks, toolCalls...)
	resp.Messages = []Message{{Role: "assistant", Content: blocks}}
	resp.Usage = usage
	return resp, nil
}

// statusWriter records the HTTP status and bytes written so ServeHTTP can log
// them. It forwards Flush so SSE streaming keeps working through the wrapper.
type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// readErrorBody drains a bounded upstream error body (closing it after 30s if
// the upstream never finishes) so the error can be relayed, converted and
// logged.
func readErrorBody(upResp *http.Response) ([]byte, error) {
	stop := time.AfterFunc(30*time.Second, func() { upResp.Body.Close() })
	defer stop.Stop()
	return io.ReadAll(io.LimitReader(upResp.Body, 1<<20))
}

// relayErrorBytes forwards a same-protocol error verbatim from the already-read
// body.
func relayErrorBytes(w http.ResponseWriter, upResp *http.Response, body []byte) {
	copyRespHeaders(w, upResp)
	w.WriteHeader(upResp.StatusCode)
	_, _ = w.Write(body)
}

// writeClientError writes an error to the client in the format it can parse:
// streaming Responses/Messages clients get an SSE error event; everything else
// gets a JSON error object.
func writeClientError(w http.ResponseWriter, clientProto string, stream bool, status int, msg, typ string) {
	if stream && (clientProto == ProtoOpenAIResponses || clientProto == ProtoAnthropicMessages) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(status)
		if clientProto == ProtoOpenAIResponses {
			_ = writeSSE(w, "error", map[string]any{"type": "error", "code": typ, "message": msg})
		} else {
			_ = writeSSE(w, "error", map[string]any{"type": "error", "error": map[string]any{"type": typ, "message": msg}})
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	var payload any
	if clientProto == ProtoAnthropicMessages {
		payload = map[string]any{"type": "error", "error": map[string]any{"type": typ, "message": msg}}
	} else {
		payload = map[string]any{"error": map[string]any{
			"message": msg, "type": typ, "code": strconv.Itoa(status),
		}}
	}
	_ = json.NewEncoder(w).Encode(payload)
}

// limiter caps concurrent in-flight requests to one upstream alias using a
// buffered-channel semaphore. In "reject" mode an over-limit acquire fails
// immediately; in "queue" mode it waits for a slot (or the client context).
type limiter struct {
	ch     chan struct{}
	reject bool
}

func newLimiter(max int, mode string) *limiter {
	return &limiter{ch: make(chan struct{}, max), reject: mode == "reject"}
}

func (l *limiter) acquire(ctx context.Context) bool {
	if l.reject {
		select {
		case l.ch <- struct{}{}:
			return true
		default:
			return false
		}
	}
	select {
	case l.ch <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}

func (l *limiter) release() {
	<-l.ch
}

// extractErrorMessage pulls a readable message/type out of common provider
// error bodies: OpenAI {"error":{...}}, Anthropic {"type":"error","error":{...}},
// Anthropic SSE error events, or plain text.
func extractErrorMessage(body []byte, status int) (msg, typ string) {
	if len(body) > 0 {
		var m map[string]any
		if json.Unmarshal(body, &m) == nil {
			if e, ok := m["error"].(map[string]any); ok {
				msg, _ = e["message"].(string)
				typ, _ = e["type"].(string)
			}
			if msg == "" {
				msg, _ = m["message"].(string)
			}
			if typ == "" {
				typ, _ = m["type"].(string)
			}
		}
	}
	if msg == "" {
		// Anthropic streams errors as "event: error" / "data: {...}".
		for _, line := range strings.Split(string(body), "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			var e map[string]any
			if json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &e) == nil {
				if errObj, ok := e["error"].(map[string]any); ok {
					msg, _ = errObj["message"].(string)
					typ, _ = errObj["type"].(string)
					if msg != "" {
						break
					}
				}
			}
		}
	}
	if msg == "" {
		msg = strings.TrimSpace(string(body))
		if len(msg) > 500 {
			msg = msg[:500]
		}
	}
	if msg == "" {
		msg = fmt.Sprintf("upstream returned HTTP %d", status)
	}
	if typ == "" {
		switch {
		case status == http.StatusTooManyRequests:
			typ = "rate_limit_error"
		case status == http.StatusUnauthorized || status == http.StatusForbidden:
			typ = "authentication_error"
		case status >= 500:
			typ = "api_error"
		default:
			typ = "invalid_request_error"
		}
	}
	return msg, typ
}
