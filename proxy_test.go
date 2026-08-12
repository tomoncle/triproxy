package main

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func startProxy(t *testing.T, upstream, proto string) *httptest.Server {
	t.Helper()
	cfg := &Config{Aliases: map[string]*Alias{
		"llm": {Upstream: upstream, Protocol: proto},
	}}
	return httptest.NewServer(NewProxy(cfg))
}

func doRequest(t *testing.T, method, url, auth, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// accessCols reports the starting column of each marker (e.g. "status=") in an
// access-log line, searching forward so earlier fields can't shadow later ones.
func accessCols(line string, cols []string) []int {
	pos := make([]int, len(cols))
	from := 0
	for i, col := range cols {
		idx := strings.Index(line[from:], col)
		if idx < 0 {
			pos[i] = -1
			break
		}
		pos[i] = from + idx
		from = pos[i] + len(col)
	}
	return pos
}

// TestLogRequestColumnAlignment: 访问日志用固定列宽，无论 path/dur/bytes 长短，
// 每行 status= dur= bytes= remote= 的起始列都必须完全一致（长 path 会被裁剪）。
func TestLogRequestColumnAlignment(t *testing.T) {
	p := NewProxy(&Config{})
	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(old)

	cases := []struct {
		method, path string
		status       int
		dur          time.Duration
		bytes        int64
		remote       string
	}{
		{"POST", "/tmp/v1/messages", 200, 3 * time.Millisecond, 84, "127.0.0.1:53102"},
		{"POST", "/openai/v1/chat/completions", 200, time.Minute + 3*time.Second + 763*time.Millisecond, 426429, "127.0.0.1:58024"},
		{"GET", "/tmp/v1/models", 200, 24 * time.Millisecond, 222, "127.0.0.1:57406"},
		{"DELETE", "/a/very/long/path/that/exceeds/the/fixed/column/width/and/must/be/clipped", 404, 41*time.Second + 249*time.Millisecond, 999999999, "127.0.0.1:99999"},
	}
	for _, c := range cases {
		p.logRequest(c.method, c.path, c.status, c.dur, c.bytes, c.remote)
	}

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != len(cases) {
		t.Fatalf("期望 %d 行日志，实际 %d 行:\n%s", len(cases), len(lines), buf.String())
	}
	cols := []string{"status=", "dur=", "bytes=", "remote="}
	want := accessCols(lines[0], cols)
	for _, line := range lines[1:] {
		got := accessCols(line, cols)
		for i := range cols {
			if got[i] != want[i] {
				t.Errorf("列 %q 起始列 = %d，期望 %d\n%s", cols[i], got[i], want[i], buf.String())
			}
		}
	}
}

func TestClip(t *testing.T) {
	if got := clip("abc", 5); got != "abc" {
		t.Fatalf("短字符串不应被裁剪: %q", got)
	}
	if got := clip("abcdef", 5); got != "ab..." {
		t.Fatalf("clip(\"abcdef\", 5) = %q, 期望 \"ab...\"", got)
	}
	if got := clip("你好世界", 4); got != "你好世界" {
		t.Fatalf("rune 数不足时不裁剪: %q", got)
	}
	if got := clip("你好世界啊", 4); got != "你..." {
		t.Fatalf("clip(\"你好世界啊\", 4) = %q, 期望 \"你...\"（含省略号共 4 个 rune）", got)
	}
	if got := clip("你好世界啊哦", 5); got != "你好..." {
		t.Fatalf("clip(\"你好世界啊哦\", 5) = %q, 期望 \"你好...\"", got)
	}
	if got := clip("x", 1); got != "x" {
		t.Fatalf("clip(\"x\", 1) = %q, 期望 \"x\"（n<3 回退到 3，不裁剪）", got)
	}
}

// Claude Code probes /{alias}/api/hello on session start (the real Anthropic
// API answers 200 {"status":"ok"}). The proxy must answer the same for both
// GET and HEAD so the client's connectivity check succeeds instead of 404ing.
func TestProxyAnthropicHello(t *testing.T) {
	proxy := startProxy(t, "http://127.0.0.1:1", "openai-chat")
	for _, method := range []string{"GET", "HEAD"} {
		resp := doRequest(t, method, proxy.URL+"/llm/api/hello", "", "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s /llm/api/hello: got status %d, want 200", method, resp.StatusCode)
		}
		if method == "GET" {
			var m map[string]any
			if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
				t.Fatalf("GET /llm/api/hello: invalid JSON body: %v", err)
			}
			if m["status"] != "ok" {
				t.Fatalf("GET /llm/api/hello: body status = %v, want ok", m["status"])
			}
		}
	}
}

// The user's first example: Codex (Responses) -> upstream Chat Completions.
func TestProxyResponsesToChatNonStream(t *testing.T) {
	var gotPath, gotAuth, gotXKey string
	var gotBody []byte
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotXKey = r.Header.Get("X-Api-Key")
		gotBody, _ = io.ReadAll(r.Body)
		var m map[string]any
		if err := json.Unmarshal(gotBody, &m); err != nil {
			t.Errorf("upstream got invalid JSON: %v", err)
		}
		if m["model"] != "gpt-test" {
			t.Errorf("upstream model = %v", m["model"])
		}
		if _, ok := m["messages"]; !ok {
			t.Errorf("upstream body missing messages: %s", gotBody)
		}
		if got := m["messages"].([]any)[0].(map[string]any)["role"]; got != "system" {
			t.Errorf("messages[0].role = %v", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"gpt-test",
			"choices":[{"index":0,"message":{"role":"assistant","content":"Hi from chat"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":3,"completion_tokens":3,"total_tokens":6}
		}`))
	}))
	defer up.Close()

	proxy := startProxy(t, up.URL, ProtoOpenAIChat)
	resp := doRequest(t, "POST", proxy.URL+"/llm/v1/responses", "Bearer sk-test", `{
		"model":"gpt-test","instructions":"be nice","stream":false,
		"input":[{"role":"user","content":"hi"}]
	}`)
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, b)
	}
	if gotPath != "/v1/chat/completions" {
		t.Fatalf("upstream path = %q", gotPath)
	}
	if gotAuth != "Bearer sk-test" {
		t.Fatalf("Authorization = %q (must pass through)", gotAuth)
	}
	if gotXKey != "" {
		t.Fatalf("unexpected X-Api-Key = %q", gotXKey)
	}

	out, _ := io.ReadAll(resp.Body)
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("client got non-JSON: %v\n%s", err, out)
	}
	if m["object"] != "response" {
		t.Fatalf("client object = %v (want response)", m["object"])
	}
	if m["status"] != "completed" {
		t.Fatalf("client status = %v", m["status"])
	}
	outMsg := m["output"].([]any)[0].(map[string]any)
	if outMsg["type"] != "message" {
		t.Fatalf("output[0].type = %v", outMsg["type"])
	}
	text := outMsg["content"].([]any)[0].(map[string]any)["text"]
	if text != "Hi from chat" {
		t.Fatalf("output text = %v", text)
	}
	if u := m["usage"].(map[string]any); u["input_tokens"].(float64) != 3 {
		t.Fatalf("usage = %v", u)
	}
}

// The user's second example: Codex (Responses) -> upstream Anthropic Messages.
func TestProxyResponsesToMessagesNonStream(t *testing.T) {
	var gotPath, gotAuth string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		var b []byte
		b, _ = io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		if m["max_tokens"] == nil {
			t.Errorf("messages upstream missing max_tokens")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"msg_1","type":"message","role":"assistant","model":"claude-test",
			"content":[{"type":"text","text":"Hi from claude"}],
			"stop_reason":"end_turn",
			"usage":{"input_tokens":4,"output_tokens":2}
		}`))
	}))
	defer up.Close()

	proxy := startProxy(t, up.URL, ProtoAnthropicMessages)
	resp := doRequest(t, "POST", proxy.URL+"/llm/v1/responses", "Bearer sk-abc", `{
		"model":"claude-test","stream":false,
		"input":[{"role":"user","content":"hi"}]
	}`)
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, b)
	}
	if gotPath != "/v1/messages" {
		t.Fatalf("upstream path = %q", gotPath)
	}
	if gotAuth != "Bearer sk-abc" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
	out, _ := io.ReadAll(resp.Body)
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("client got non-JSON: %v\n%s", err, out)
	}
	text := m["output"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"]
	if text != "Hi from claude" {
		t.Fatalf("output text = %v", text)
	}
}

func TestProxyChatToMessagesNonStream(t *testing.T) {
	var gotPath string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"msg_1","type":"message","role":"assistant","model":"m",
			"content":[{"type":"text","text":"bonjour"}],
			"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}
		}`))
	}))
	defer up.Close()

	proxy := startProxy(t, up.URL, ProtoAnthropicMessages)
	resp := doRequest(t, "POST", proxy.URL+"/llm/v1/chat/completions", "Bearer sk-x", `{
		"model":"m","messages":[{"role":"user","content":"hi"}],"stream":false
	}`)
	if gotPath != "/v1/messages" {
		t.Fatalf("upstream path = %q", gotPath)
	}
	out, _ := io.ReadAll(resp.Body)
	var m map[string]any
	_ = json.Unmarshal(out, &m)
	if m["object"] != "chat.completion" {
		t.Fatalf("client object = %v", m["object"])
	}
	if got := m["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["content"]; got != "bonjour" {
		t.Fatalf("content = %v", got)
	}
}

func TestProxyStreamingResponsesToChat(t *testing.T) {
	var gotBody []byte
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", `{"id":"x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", `{"id":"x","choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":null}]}`)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", `{"id":"x","choices":[{"index":0,"delta":{"content":" world"},"finish_reason":null}]}`)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", `{"id":"x","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)
		_, _ = fmt.Fprintf(w, "data: [DONE]\n\n")
	}))
	defer up.Close()

	proxy := startProxy(t, up.URL, ProtoOpenAIChat)
	resp := doRequest(t, "POST", proxy.URL+"/llm/v1/responses", "Bearer sk-s", `{
		"model":"m","stream":true,"input":[{"role":"user","content":"hi"}]
	}`)
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("content-type = %q", ct)
	}
	out, _ := io.ReadAll(resp.Body)
	for _, want := range []string{
		"event: response.created",
		"event: response.output_text.delta",
		`"delta":"Hello"`,
		`"delta":" world"`,
		"event: response.completed",
		`"status":"completed"`,
	} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("client stream missing %q\n%s", want, out)
		}
	}
	var upReq map[string]any
	_ = json.Unmarshal(gotBody, &upReq)
	if upReq["stream"] != true {
		t.Fatalf("upstream stream flag = %v", upReq["stream"])
	}
}

func TestProxyStreamingResponsesToMessages(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = fmt.Fprintf(w, "event: message_start\ndata: %s\n\n", `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"usage":{"input_tokens":1,"output_tokens":0}}}`)
		_, _ = fmt.Fprintf(w, "event: content_block_start\ndata: %s\n\n", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
		_, _ = fmt.Fprintf(w, "event: content_block_delta\ndata: %s\n\n", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Salut"}}`)
		_, _ = fmt.Fprintf(w, "event: content_block_stop\ndata: %s\n\n", `{"type":"content_block_stop","index":0}`)
		_, _ = fmt.Fprintf(w, "event: message_delta\ndata: %s\n\n", `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":2}}`)
		_, _ = fmt.Fprintf(w, "event: message_stop\ndata: %s\n\n", `{"type":"message_stop"}`)
	}))
	defer up.Close()

	proxy := startProxy(t, up.URL, ProtoAnthropicMessages)
	resp := doRequest(t, "POST", proxy.URL+"/llm/v1/responses", "Bearer sk-s", `{
		"model":"m","stream":true,"input":[{"role":"user","content":"hi"}]
	}`)
	out, _ := io.ReadAll(resp.Body)
	for _, want := range []string{
		"event: response.created",
		`"delta":"Salut"`,
		"event: response.completed",
	} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("client stream missing %q\n%s", want, out)
		}
	}
}

// TestProxyStreamingResponsesToMessagesMultiTool: Codex (Responses client)
// against a Messages upstream that streams two parallel tool_use blocks with
// interleaved input_json deltas. The proxy must keep each tool's arguments
// separate, emit one function_call per tool with its name, and surface both in
// response.completed.
func TestProxyStreamingResponsesToMessagesMultiTool(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = fmt.Fprintf(w, "event: message_start\ndata: %s\n\n", `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"usage":{"input_tokens":2,"output_tokens":0}}}`)
		_, _ = fmt.Fprintf(w, "event: content_block_start\ndata: %s\n\n", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"tu_a","name":"tool_a","input":{}}}`)
		_, _ = fmt.Fprintf(w, "event: content_block_start\ndata: %s\n\n", `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"tu_b","name":"tool_b","input":{}}}`)
		_, _ = fmt.Fprintf(w, "event: content_block_delta\ndata: %s\n\n", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"x\":"}}`)
		_, _ = fmt.Fprintf(w, "event: content_block_delta\ndata: %s\n\n", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"y\":"}}`)
		_, _ = fmt.Fprintf(w, "event: content_block_delta\ndata: %s\n\n", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"1}"}}`)
		_, _ = fmt.Fprintf(w, "event: content_block_delta\ndata: %s\n\n", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"2}"}}`)
		_, _ = fmt.Fprintf(w, "event: content_block_stop\ndata: %s\n\n", `{"type":"content_block_stop","index":0}`)
		_, _ = fmt.Fprintf(w, "event: content_block_stop\ndata: %s\n\n", `{"type":"content_block_stop","index":1}`)
		_, _ = fmt.Fprintf(w, "event: message_delta\ndata: %s\n\n", `{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":4}}`)
		_, _ = fmt.Fprintf(w, "event: message_stop\ndata: %s\n\n", `{"type":"message_stop"}`)
	}))
	defer up.Close()

	proxy := startProxy(t, up.URL, ProtoAnthropicMessages)
	resp := doRequest(t, "POST", proxy.URL+"/llm/v1/responses", "Bearer sk-s", `{
		"model":"m","stream":true,"input":[{"role":"user","content":"hi"}]
	}`)
	out, _ := io.ReadAll(resp.Body)
	for _, want := range []string{
		"event: response.created",
		"event: response.output_item.added",
		`"call_id":"tu_a"`,
		`"call_id":"tu_b"`,
		`"name":"tool_a"`,
		`"name":"tool_b"`,
		"event: response.function_call_arguments.done",
		`"arguments":"{\"x\":1}"`,
		`"arguments":"{\"y\":2}"`,
		"event: response.completed",
		`"status":"completed"`,
	} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("client stream missing %q\n%s", want, out)
		}
	}
}

// TestProxyResponsesToMessagesReasoningBridge exercises the full reasoning
// bridge: a Codex request with reasoning.effort is converted to an Anthropic
// thinking request (param + beta header), and the upstream's thinking stream is
// converted back into Responses reasoning events for Codex.
func TestProxyResponsesToMessagesReasoningBridge(t *testing.T) {
	var gotBody []byte
	var gotBeta string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBeta = r.Header.Get("Anthropic-Beta")
		gotBody, _ = io.ReadAll(r.Body)
		var m map[string]any
		if err := json.Unmarshal(gotBody, &m); err != nil {
			t.Errorf("upstream got invalid JSON: %v", err)
		}
		if _, ok := m["thinking"]; !ok {
			t.Errorf("upstream body missing thinking param: %s", gotBody)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = fmt.Fprintf(w, "event: message_start\ndata: %s\n\n", `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"usage":{"input_tokens":1,"output_tokens":0}}}`)
		_, _ = fmt.Fprintf(w, "event: content_block_start\ndata: %s\n\n", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`)
		_, _ = fmt.Fprintf(w, "event: content_block_delta\ndata: %s\n\n", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"Let me"}}`)
		_, _ = fmt.Fprintf(w, "event: content_block_delta\ndata: %s\n\n", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":" think"}}`)
		_, _ = fmt.Fprintf(w, "event: content_block_stop\ndata: %s\n\n", `{"type":"content_block_stop","index":0}`)
		_, _ = fmt.Fprintf(w, "event: content_block_start\ndata: %s\n\n", `{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`)
		_, _ = fmt.Fprintf(w, "event: content_block_delta\ndata: %s\n\n", `{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"hello"}}`)
		_, _ = fmt.Fprintf(w, "event: content_block_stop\ndata: %s\n\n", `{"type":"content_block_stop","index":1}`)
		_, _ = fmt.Fprintf(w, "event: message_delta\ndata: %s\n\n", `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":1}}`)
		_, _ = fmt.Fprintf(w, "event: message_stop\ndata: %s\n\n", `{"type":"message_stop"}`)
	}))
	defer up.Close()

	cfg := &Config{Aliases: map[string]*Alias{
		"abc": {Upstream: up.URL, Protocol: ProtoAnthropicMessages, Thinking: true},
	}}
	proxy := httptest.NewServer(NewProxy(cfg))
	defer proxy.Close()

	resp := doRequest(t, "POST", proxy.URL+"/abc/v1/responses", "Bearer sk", `{
		"model":"m","stream":true,"reasoning":{"effort":"high"},
		"input":[{"role":"user","content":"hi"}]
	}`)
	out, _ := io.ReadAll(resp.Body)
	if !strings.Contains(gotBeta, "thinking-2024-12-04") {
		t.Fatalf("anthropic-beta = %q (want thinking-2024-12-04)", gotBeta)
	}
	if !strings.Contains(string(gotBody), `"thinking"`) {
		t.Fatalf("upstream body missing thinking: %s", gotBody)
	}
	for _, want := range []string{
		"event: response.reasoning_summary_part.added",
		`"delta":"Let me"`,
		`"snapshot":"Let me think"`,
		"event: response.reasoning_summary_part.done",
		`"delta":"hello"`,
		"event: response.completed",
	} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("client stream missing %q\n%s", want, out)
		}
	}
}

// TestProxyCompactPassthrough: Codex's POST /responses/compact is relayed to an
// openai-responses upstream.
func TestProxyCompactPassthrough(t *testing.T) {
	var gotPath string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"cmp_1","object":"response.compact"}`))
	}))
	defer up.Close()

	proxy := startProxy(t, up.URL, ProtoOpenAIResponses)
	resp := doRequest(t, "POST", proxy.URL+"/llm/v1/responses/compact", "Bearer sk-c", `{"input":[{"role":"user","content":"long history"}]}`)
	out, _ := io.ReadAll(resp.Body)
	if gotPath != "/v1/responses/compact" {
		t.Fatalf("upstream path = %q", gotPath)
	}
	if !strings.Contains(string(out), `"object":"response.compact"`) {
		t.Fatalf("compact response not relayed: %s", out)
	}
}

// TestProxyCompactUnsupported: for non-Responses upstreams compact returns a
// clear 501 instead of a 404 Codex cannot act on.
func TestProxyCompactUnsupported(t *testing.T) {
	proxy := startProxy(t, "http://127.0.0.1:1", ProtoAnthropicMessages)
	resp := doRequest(t, "POST", proxy.URL+"/llm/v1/responses/compact", "Bearer sk-c", `{"input":[]}`)
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("status = %d (want 501)", resp.StatusCode)
	}
	out, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(out), "requires an openai-responses upstream") {
		t.Fatalf("compact error body = %s", out)
	}
}

// TestProxyStreamingDetectedFromAcceptHeader reproduces the Codex failure:
// Codex signals streaming via Accept: text/event-stream even when its request
// body omits "stream": true. The proxy must treat the request as streaming,
// force stream:true into the upstream body, and return SSE to the client.
func TestProxyStreamingDetectedFromAcceptHeader(t *testing.T) {
	var gotBody []byte
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		if !strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
			t.Errorf("upstream Accept = %q (want text/event-stream)", r.Header.Get("Accept"))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", `{"id":"x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", `{"id":"x","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}`)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", `{"id":"x","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)
		_, _ = fmt.Fprintf(w, "data: [DONE]\n\n")
	}))
	defer up.Close()

	proxy := startProxy(t, up.URL, ProtoOpenAIChat)
	req, err := http.NewRequest("POST", proxy.URL+"/llm/v1/responses",
		strings.NewReader(`{"model":"m","input":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer sk")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)

	var upReq map[string]any
	if err := json.Unmarshal(gotBody, &upReq); err != nil {
		t.Fatalf("upstream body invalid JSON: %v", err)
	}
	if upReq["stream"] != true {
		t.Fatalf("upstream stream = %v (want true), body=%s", upReq["stream"], gotBody)
	}
	if !strings.Contains(string(out), "event: response.created") ||
		!strings.Contains(string(out), `"delta":"hi"`) ||
		!strings.Contains(string(out), "event: response.completed") {
		t.Fatalf("client should get Responses SSE, got: %s", out)
	}
}

// TestProxyPassthroughForcesStreamOnAccept: for a same-protocol passthrough, a
// client that signals streaming via Accept but omits stream:true in the body
// must still cause stream:true to reach the upstream.
func TestProxyPassthroughForcesStreamOnAccept(t *testing.T) {
	var gotBody []byte
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = fmt.Fprintf(w, "event: response.created\ndata: %s\n\n", `{"type":"response.created","response":{"id":"r1","object":"response","status":"in_progress"}}`)
		_, _ = fmt.Fprintf(w, "event: response.completed\ndata: %s\n\n", `{"type":"response.completed","response":{"id":"r1","object":"response","status":"completed","output":[]}}`)
	}))
	defer up.Close()

	proxy := startProxy(t, up.URL, ProtoOpenAIResponses)
	req, err := http.NewRequest("POST", proxy.URL+"/llm/v1/responses",
		strings.NewReader(`{"model":"m","input":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer sk")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)

	var upReq map[string]any
	if err := json.Unmarshal(gotBody, &upReq); err != nil {
		t.Fatalf("upstream body invalid JSON: %v", err)
	}
	if upReq["stream"] != true {
		t.Fatalf("passthrough upstream stream = %v (want true), body=%s", upReq["stream"], gotBody)
	}
}

// TestDebugBodyFormatsJSON verifies debug bodies are pretty-printed when they
// are JSON and passed through verbatim otherwise.
func TestDebugBodyFormatsJSON(t *testing.T) {
	if got := debugBody(nil); got != "(empty)" {
		t.Fatalf("empty body = %q", got)
	}
	jsonBody := []byte(`{"a":{"b":1},"c":[1,2]}`)
	got := debugBody(jsonBody)
	if !strings.Contains(got, "\n") || !strings.Contains(got, `"a": {`) {
		t.Fatalf("JSON body not pretty-printed:\n%s", got)
	}
	if got := debugBody([]byte("plain text")); got != "plain text" {
		t.Fatalf("non-JSON body altered: %q", got)
	}
	// Long body is truncated with a size marker (formatting inflates it past
	// the 64KB debug limit).
	long := []byte(`{"x":"` + strings.Repeat("a", 70000) + `"}`)
	out := debugBody(long)
	if !strings.Contains(out, "截断") {
		t.Fatalf("long body not truncated:\n%s...", out)
	}
}

// TestStreamDebuggerAccumulates verifies streaming responses are summarized for
// debug output without buffering the wire stream.
func TestStreamDebuggerAccumulates(t *testing.T) {
	d := newStreamDebugger("tmp", true)
	d.observe(streamEv{kind: evReasoning, reasoningText: "let me"})
	d.observe(streamEv{kind: evReasoning, reasoningText: " think"})
	d.observe(streamEv{kind: evText, text: "hello"})
	d.observe(streamEv{kind: evText, text: " world"})
	d.observe(streamEv{kind: evToolBegin, toolID: "c1", toolName: "exec_command"})
	d.observe(streamEv{kind: evDone, finishReason: "stop", usage: &Usage{InputTokens: 1, OutputTokens: 2}})

	s := d.summary()
	for _, want := range []string{
		"finish=stop",
		"usage={in:1,out:2,total:3}",
		`exec_command(c1)`,
		`推理: "let me think"`,
		`文本: "hello world"`,
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("summary missing %q\n%s", want, s)
		}
	}
}

func TestProxySameProtocolPassthrough(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"raw"},"finish_reason":"stop"}],"custom_provider_field":"kept"}`))
	}))
	defer up.Close()

	proxy := startProxy(t, up.URL, ProtoOpenAIChat)
	resp := doRequest(t, "POST", proxy.URL+"/llm/v1/chat/completions", "Bearer sk-x", `{
		"model":"m","messages":[{"role":"user","content":"hi"}],"stream":false,"custom_client_field":1
	}`)
	out, _ := io.ReadAll(resp.Body)
	var m map[string]any
	_ = json.Unmarshal(out, &m)
	if m["custom_provider_field"] != "kept" {
		t.Fatalf("passthrough lost provider field: %v", m)
	}
	if m["object"] != "chat.completion" {
		t.Fatalf("object = %v", m["object"])
	}
}

func TestProxyModelsPassthrough(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("models path = %q", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer sk-m" {
			t.Errorf("auth = %q", r.Header.Get("Authorization"))
		}
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"m1"}]}`))
	}))
	defer up.Close()

	proxy := startProxy(t, up.URL, ProtoOpenAIChat)
	resp := doRequest(t, "GET", proxy.URL+"/llm/v1/models", "Bearer sk-m", "")
	out, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(out), `"id":"m1"`) {
		t.Fatalf("models response = %s", out)
	}
}

func TestProxyUnknownAliasAndUpstreamError(t *testing.T) {
	proxy := startProxy(t, "http://127.0.0.1:1", ProtoOpenAIChat)
	resp := doRequest(t, "POST", proxy.URL+"/nope/v1/responses", "", `{"model":"m","input":"hi"}`)
	if resp.StatusCode != 404 {
		t.Fatalf("unknown alias status = %d", resp.StatusCode)
	}

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"error":{"message":"bad key"}}`))
	}))
	defer up.Close()
	proxy2 := startProxy(t, up.URL, ProtoOpenAIChat)
	resp2 := doRequest(t, "POST", proxy2.URL+"/llm/v1/responses", "Bearer bad", `{"model":"m","input":"hi"}`)
	if resp2.StatusCode != 401 {
		t.Fatalf("upstream error status = %d", resp2.StatusCode)
	}
	out, _ := io.ReadAll(resp2.Body)
	if !strings.Contains(string(out), "bad key") {
		t.Fatalf("upstream error body not relayed: %s", out)
	}
}

// TestProxyUpstream429Repro: Codex (streaming Responses client) against a Chat
// upstream that returns 429. Reproduces the reported "Codex hangs on 429" bug.
func TestProxyUpstream429Repro(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limited","type":"rate_limit_error","code":"429"}}`))
	}))
	defer up.Close()

	proxy := startProxy(t, up.URL, ProtoOpenAIChat)
	resp := doRequest(t, "POST", proxy.URL+"/llm/v1/responses", "Bearer sk", `{
		"model":"m","stream":true,"input":[{"role":"user","content":"hi"}]
	}`)
	out, _ := io.ReadAll(resp.Body)
	t.Logf("status=%d content-type=%s body=%s", resp.StatusCode, resp.Header.Get("Content-Type"), out)
	if resp.StatusCode != 429 {
		t.Fatalf("expected 429 passthrough, got %d", resp.StatusCode)
	}
	// Streaming Codex client must get a Responses-format SSE error event.
	if !strings.Contains(string(out), "event: error") ||
		!strings.Contains(string(out), `"type":"error"`) ||
		!strings.Contains(string(out), `"code":"rate_limit_error"`) ||
		!strings.Contains(string(out), "rate limited") {
		t.Fatalf("error not converted to Responses SSE format: %s", out)
	}
}

// TestProxyUpstream429AnthropicStreaming: upstream is Messages and returns 429
// as an Anthropic SSE error event; the Codex (Responses) client must receive a
// Responses-format error, not an unparseable Anthropic stream.
func TestProxyUpstream429AnthropicStreaming(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(429)
		_, _ = fmt.Fprintf(w, "event: error\ndata: %s\n\n", `{"type":"error","error":{"type":"rate_limit_error","message":"You have been rate limited"}}`)
	}))
	defer up.Close()

	proxy := startProxy(t, up.URL, ProtoAnthropicMessages)
	resp := doRequest(t, "POST", proxy.URL+"/llm/v1/responses", "Bearer sk", `{
		"model":"m","stream":true,"input":[{"role":"user","content":"hi"}]
	}`)
	out, _ := io.ReadAll(resp.Body)
	t.Logf("status=%d content-type=%s body=%s", resp.StatusCode, resp.Header.Get("Content-Type"), out)
	if resp.StatusCode != 429 {
		t.Fatalf("expected 429, got %d", resp.StatusCode)
	}
	if !strings.Contains(string(out), "You have been rate limited") ||
		!strings.Contains(string(out), `"code":"rate_limit_error"`) {
		t.Fatalf("Anthropic error not converted to Responses format: %s", out)
	}
	if strings.Contains(string(out), `"error":{"type":"rate_limit_error"`) {
		t.Fatalf("Anthropic nested error leaked to Responses client: %s", out)
	}
}

// TestProxyUpstream429NonStreamResponses: a non-streaming Codex client gets a
// JSON error object in Responses/Chat style.
func TestProxyUpstream429NonStreamResponses(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"error":{"message":"slow down","type":"rate_limit_error","code":"429"}}`))
	}))
	defer up.Close()

	proxy := startProxy(t, up.URL, ProtoOpenAIChat)
	resp := doRequest(t, "POST", proxy.URL+"/llm/v1/responses", "Bearer sk", `{
		"model":"m","stream":false,"input":[{"role":"user","content":"hi"}]
	}`)
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 429 {
		t.Fatalf("expected 429, got %d", resp.StatusCode)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("non-stream 429 should be JSON, got: %s", out)
	}
	if got := m["error"].(map[string]any)["message"]; got != "slow down" {
		t.Fatalf("error message = %v", got)
	}
}

// TestProxyConcurrentRequests proves requests (any alias / protocol / stream)
// are handled concurrently, not serially. A mock upstream sleeps 200ms; eight
// parallel requests must finish in ~200ms, not ~1.6s.
func TestProxyConcurrentRequests(t *testing.T) {
	const (
		delay   = 200 * time.Millisecond
		workers = 8
	)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(delay)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-c","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`))
	}))
	defer up.Close()

	proxy := startProxy(t, up.URL, ProtoOpenAIChat)
	start := time.Now()
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Exercise the cross-protocol conversion path (responses -> chat).
			resp := doRequest(t, "POST", proxy.URL+"/llm/v1/responses", "Bearer sk", `{
				"model":"m","stream":false,"input":[{"role":"user","content":"hi"}]
			}`)
			defer resp.Body.Close()
			if resp.StatusCode != 200 {
				errs <- fmt.Errorf("status %d", resp.StatusCode)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	elapsed := time.Since(start)
	if elapsed > delay+150*time.Millisecond {
		t.Fatalf("requests look serial: %d concurrent requests took %v (delay %v)", workers, elapsed, delay)
	}
	t.Logf("%d 并发请求总耗时 %v（每个上游延迟 %v）=> 并发处理", workers, elapsed, delay)
}

// doReq performs a chat-completion-style request and returns its status code.
func doReq(proxyURL string, path string) (int, error) {
	req, err := http.NewRequest("POST", proxyURL+path, strings.NewReader(`{
		"model":"m","stream":false,"input":[{"role":"user","content":"hi"}]
	}`))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer sk")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}

// TestProxyMaxConcurrencyReject: with max_concurrency=2/reject, the third
// concurrent request is answered 429 immediately while the first two hold the
// upstream. Deterministic: the upstream blocks until released.
func TestProxyMaxConcurrencyReject(t *testing.T) {
	release := make(chan struct{})
	var received int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&received, 1)
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-c","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer up.Close()

	cfg := &Config{Aliases: map[string]*Alias{
		"llm": {Upstream: up.URL, Protocol: ProtoOpenAIChat, MaxConcurrency: 2, ConcurrencyMode: "reject"},
	}}
	proxy := httptest.NewServer(NewProxy(cfg))
	defer proxy.Close()

	var wg sync.WaitGroup
	statuses := make([]int, 3)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			statuses[i], _ = doReq(proxy.URL, "/llm/v1/responses")
		}(i)
	}
	deadline := time.Now().Add(3 * time.Second)
	for atomic.LoadInt32(&received) < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if atomic.LoadInt32(&received) != 2 {
		t.Fatalf("upstream received %d requests (want 2)", received)
	}

	statuses[2], _ = doReq(proxy.URL, "/llm/v1/responses")
	if statuses[2] != http.StatusTooManyRequests {
		t.Fatalf("over-limit request status = %d (want 429)", statuses[2])
	}

	close(release)
	wg.Wait()
	if statuses[0] != 200 || statuses[1] != 200 {
		t.Fatalf("in-flight requests failed: %v", statuses[:2])
	}
}

// TestProxyMaxConcurrencyQueue: with max_concurrency=1/queue, excess requests
// wait instead of being rejected, and all eventually complete.
func TestProxyMaxConcurrencyQueue(t *testing.T) {
	release := make(chan struct{})
	var received int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&received, 1)
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-c","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer up.Close()

	cfg := &Config{Aliases: map[string]*Alias{
		"llm": {Upstream: up.URL, Protocol: ProtoOpenAIChat, MaxConcurrency: 1, ConcurrencyMode: "queue"},
	}}
	proxy := httptest.NewServer(NewProxy(cfg))
	defer proxy.Close()

	var wg sync.WaitGroup
	statuses := make([]int, 3)
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			statuses[i], _ = doReq(proxy.URL, "/llm/v1/responses")
		}(i)
	}
	deadline := time.Now().Add(3 * time.Second)
	for atomic.LoadInt32(&received) < 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if atomic.LoadInt32(&received) != 1 {
		t.Fatalf("queue should let only 1 reach upstream, got %d", received)
	}
	time.Sleep(100 * time.Millisecond)
	if got := atomic.LoadInt32(&received); got != 1 {
		t.Fatalf("more than 1 request reached upstream while queued: %d", got)
	}

	close(release)
	wg.Wait()
	for i, s := range statuses {
		if s != 200 {
			t.Fatalf("queued request %d status = %d (want 200)", i, s)
		}
	}
}

// captureHeaders returns a helper that records the upstream request headers.
func captureHeaders(t *testing.T) (*httptest.Server, func() map[string]string) {
	t.Helper()
	var mu sync.Mutex
	got := map[string]string{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		for k, v := range r.Header {
			got[k] = v[0]
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	return up, func() map[string]string {
		mu.Lock()
		defer mu.Unlock()
		out := map[string]string{}
		for k, v := range got {
			out[k] = v
		}
		return out
	}
}

func postJSON(t *testing.T, url string, hdr map[string]string, body string) {
	t.Helper()
	req, err := http.NewRequest("POST", url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
}

// Codex client (Authorization Bearer + OpenAI SDK headers) -> Anthropic upstream:
// OpenAI-specific headers must be stripped, anthropic-version added, and auth
// mirrored to x-api-key.
func TestProxyHeadersAdaptationToAnthropic(t *testing.T) {
	up, headers := captureHeaders(t)
	defer up.Close()
	cfg := &Config{Aliases: map[string]*Alias{
		"llm": {Upstream: up.URL, Protocol: ProtoAnthropicMessages},
	}}
	proxy := httptest.NewServer(NewProxy(cfg))
	defer proxy.Close()

	postJSON(t, proxy.URL+"/llm/v1/responses", map[string]string{
		"Authorization":    "Bearer sk-abc",
		"Openai-Beta":      "responses",
		"X-Stainless-Lang": "rust",
	}, `{"model":"m","input":"hi"}`)

	got := headers()
	if got["Anthropic-Version"] != "2023-06-01" {
		t.Fatalf("anthropic-version = %q (want 2023-06-01)", got["Anthropic-Version"])
	}
	if got["X-Api-Key"] != "sk-abc" {
		t.Fatalf("x-api-key = %q (want sk-abc)", got["X-Api-Key"])
	}
	if got["Authorization"] != "Bearer sk-abc" {
		t.Fatalf("authorization = %q", got["Authorization"])
	}
	for _, bad := range []string{"Openai-Beta", "X-Stainless-Lang"} {
		if _, ok := got[bad]; ok {
			t.Fatalf("OpenAI header %q leaked to Anthropic: %v", bad, got)
		}
	}
}

// Claude Code client (x-api-key + anthropic-* headers) -> OpenAI upstream:
// Anthropic headers must be stripped and x-api-key mapped to Authorization.
func TestProxyHeadersAdaptationToOpenAI(t *testing.T) {
	up, headers := captureHeaders(t)
	defer up.Close()
	cfg := &Config{Aliases: map[string]*Alias{
		"llm": {Upstream: up.URL, Protocol: ProtoOpenAIChat},
	}}
	proxy := httptest.NewServer(NewProxy(cfg))
	defer proxy.Close()

	postJSON(t, proxy.URL+"/llm/v1/chat/completions", map[string]string{
		"X-Api-Key":         "sk-ant-xyz",
		"Anthropic-Version": "2023-06-01",
		"Anthropic-Beta":    "claude-code-20250219",
		"X-App":             "cli",
	}, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)

	got := headers()
	if got["Authorization"] != "Bearer sk-ant-xyz" {
		t.Fatalf("authorization = %q (want Bearer sk-ant-xyz)", got["Authorization"])
	}
	for _, bad := range []string{"Anthropic-Version", "Anthropic-Beta", "X-App", "X-Api-Key"} {
		if _, ok := got[bad]; ok {
			t.Fatalf("Anthropic header %q leaked to OpenAI: %v", bad, got)
		}
	}
}

// Per-alias custom headers are applied last and can impersonate a specific
// client (Claude Code / Codex) toward the upstream.
func TestProxyCustomHeaders(t *testing.T) {
	up, headers := captureHeaders(t)
	defer up.Close()
	cfg := &Config{Aliases: map[string]*Alias{
		"llm": {
			Upstream: up.URL, Protocol: ProtoAnthropicMessages,
			Headers: map[string]string{
				"User-Agent":     "claude-cli/2.1.221 (external, cli)",
				"X-App":          "cli",
				"Anthropic-Beta": "claude-code-20250219",
			},
		},
	}}
	proxy := httptest.NewServer(NewProxy(cfg))
	defer proxy.Close()

	postJSON(t, proxy.URL+"/llm/v1/messages", map[string]string{
		"Authorization": "Bearer sk-abc",
	}, `{"model":"m","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`)

	got := headers()
	if got["User-Agent"] != "claude-cli/2.1.221 (external, cli)" {
		t.Fatalf("user-agent = %q", got["User-Agent"])
	}
	if got["X-App"] != "cli" || got["Anthropic-Beta"] != "claude-code-20250219" {
		t.Fatalf("custom headers = %v", got)
	}
	// adaptation 仍然生效：必填头 + 认证
	if got["Anthropic-Version"] != "2023-06-01" || got["X-Api-Key"] != "sk-abc" {
		t.Fatalf("adaptation lost: %v", got)
	}
}

// User-Agent from the client is preserved (not overwritten); absent -> default.
func TestProxyUserAgentPassthrough(t *testing.T) {
	up, headers := captureHeaders(t)
	defer up.Close()
	cfg := &Config{Aliases: map[string]*Alias{
		"llm": {Upstream: up.URL, Protocol: ProtoOpenAIChat},
	}}
	proxy := httptest.NewServer(NewProxy(cfg))
	defer proxy.Close()

	postJSON(t, proxy.URL+"/llm/v1/chat/completions", map[string]string{
		"User-Agent": "my-custom-client/1.0",
	}, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if got := headers()["User-Agent"]; got != "my-custom-client/1.0" {
		t.Fatalf("client user-agent overwritten: %q", got)
	}

	// 显式传空 User-Agent（Go 客户端默认会带 Go-http-client/1.1，这里清掉）
	req, _ := http.NewRequest("POST", proxy.URL+"/llm/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header["User-Agent"] = []string{""}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if got := headers()["User-Agent"]; got != "triproxy/1.0" {
		t.Fatalf("default user-agent = %q (want triproxy/1.0)", got)
	}
}

func postJSONHdrs(t *testing.T, url string, hdr map[string]string, body string) http.Header {
	t.Helper()
	req, err := http.NewRequest("POST", url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.Header
}

// Retry-After must reach the client on 429 (both same-protocol relay and
// cross-protocol converted errors), so the client can back off correctly.
func TestProxyRetryAfterForwarded(t *testing.T) {
	mk := func(proto string, body string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Retry-After", "30")
			w.WriteHeader(429)
			_, _ = w.Write([]byte(body))
		}))
	}

	// 跨协议：chat 上游 429 + Retry-After -> responses(Codex) 客户端
	up := mk(ProtoOpenAIChat, `{"error":{"message":"slow down"}}`)
	defer up.Close()
	cfg := &Config{Aliases: map[string]*Alias{"llm": {Upstream: up.URL, Protocol: ProtoOpenAIChat}}}
	proxy := httptest.NewServer(NewProxy(cfg))
	h := postJSONHdrs(t, proxy.URL+"/llm/v1/responses", map[string]string{"Authorization": "Bearer sk"}, `{"model":"m","input":"hi"}`)
	proxy.Close()
	if got := h.Get("Retry-After"); got != "30" {
		t.Fatalf("cross-protocol Retry-After = %q (want 30)", got)
	}

	// 同协议：chat 上游 429 + Retry-After -> chat 客户端
	up2 := mk(ProtoOpenAIChat, `{"error":{"message":"slow down"}}`)
	defer up2.Close()
	cfg2 := &Config{Aliases: map[string]*Alias{"llm": {Upstream: up2.URL, Protocol: ProtoOpenAIChat}}}
	proxy2 := httptest.NewServer(NewProxy(cfg2))
	h2 := postJSONHdrs(t, proxy2.URL+"/llm/v1/chat/completions", map[string]string{"Authorization": "Bearer sk"}, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	proxy2.Close()
	if got := h2.Get("Retry-After"); got != "30" {
		t.Fatalf("same-protocol Retry-After = %q (want 30)", got)
	}
}

// Anthropic 上游的限流头 -> OpenAI 客户端：应映射成 x-ratelimit-* 格式。
func TestProxyRateLimitMapToOpenAI(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Anthropic-Ratelimit-Requests-Limit", "10")
		w.Header().Set("Anthropic-Ratelimit-Requests-Remaining", "5")
		w.Header().Set("Anthropic-Ratelimit-Reset", "1786000000")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer up.Close()
	cfg := &Config{Aliases: map[string]*Alias{"llm": {Upstream: up.URL, Protocol: ProtoAnthropicMessages}}}
	proxy := httptest.NewServer(NewProxy(cfg))
	defer proxy.Close()
	h := postJSONHdrs(t, proxy.URL+"/llm/v1/chat/completions", map[string]string{"Authorization": "Bearer sk"}, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if got := h.Get("X-Ratelimit-Remaining-Requests"); got != "5" {
		t.Fatalf("x-ratelimit-remaining-requests = %q (want 5)", got)
	}
	if got := h.Get("X-Ratelimit-Limit-Requests"); got != "10" {
		t.Fatalf("x-ratelimit-limit-requests = %q (want 10)", got)
	}
	if got := h.Get("Anthropic-Ratelimit-Reset"); got != "1786000000" {
		t.Fatalf("passthrough reset = %q", got)
	}
}

// OpenAI 上游的限流头 -> Anthropic 客户端：应映射成 anthropic-ratelimit-* 格式。
func TestProxyRateLimitMapToAnthropic(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Ratelimit-Remaining-Requests", "7")
		w.Header().Set("X-Ratelimit-Limit-Requests", "20")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`))
	}))
	defer up.Close()
	cfg := &Config{Aliases: map[string]*Alias{"llm": {Upstream: up.URL, Protocol: ProtoOpenAIChat}}}
	proxy := httptest.NewServer(NewProxy(cfg))
	defer proxy.Close()
	h := postJSONHdrs(t, proxy.URL+"/llm/v1/messages", map[string]string{"X-Api-Key": "sk-ant"}, `{"model":"m","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`)
	if got := h.Get("Anthropic-Ratelimit-Requests-Remaining"); got != "7" {
		t.Fatalf("anthropic-ratelimit-requests-remaining = %q (want 7)", got)
	}
	if got := h.Get("Anthropic-Ratelimit-Requests-Limit"); got != "20" {
		t.Fatalf("anthropic-ratelimit-requests-limit = %q (want 20)", got)
	}
}

// HTTP 代理：请求应经过配置的 HTTP 代理，而不是直连上游。
func TestProxyUpstreamHTTPProxy(t *testing.T) {
	var proxied atomic.Bool
	var direct atomic.Bool
	proxySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxied.Store(true)
		if r.Host == "" || r.URL.Host == "" {
			direct.Store(true)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-p","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"via http proxy"},"finish_reason":"stop"}]}`))
	}))
	defer proxySrv.Close()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream 不应被直连（应走 HTTP 代理）")
	}))
	defer up.Close()

	cfg := &Config{Aliases: map[string]*Alias{
		"llm": {Upstream: up.URL, Protocol: ProtoOpenAIChat, Proxy: proxySrv.URL},
	}}
	proxy := httptest.NewServer(NewProxy(cfg))
	defer proxy.Close()
	req, _ := http.NewRequest("POST", proxy.URL+"/llm/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !proxied.Load() {
		t.Fatal("请求没有经过 HTTP 代理")
	}
	if direct.Load() {
		t.Fatal("代理收到的请求不是绝对 URI 形式")
	}
	if !strings.Contains(string(out), "via http proxy") {
		t.Fatalf("响应不是来自代理: %s", out)
	}
}

// SOCKS5 代理：最小 SOCKS5 服务端把流量转发到真上游，验证请求走 SOCKS5。
func TestProxyUpstreamSOCKS5Proxy(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-s","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"via socks5"},"finish_reason":"stop"}]}`))
	}))
	defer up.Close()

	socksAddr, cleanup := startSocks5(t)
	defer cleanup()
	cfg := &Config{Aliases: map[string]*Alias{
		"llm": {Upstream: up.URL, Protocol: ProtoOpenAIChat, Proxy: "socks5://" + socksAddr},
	}}
	proxy := httptest.NewServer(NewProxy(cfg))
	defer proxy.Close()
	req, _ := http.NewRequest("POST", proxy.URL+"/llm/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(out), "via socks5") {
		t.Fatalf("经 SOCKS5 代理的请求失败: status=%d body=%s", resp.StatusCode, out)
	}
}

// startSocks5 起一个极简 SOCKS5 服务端（无认证，只支持 CONNECT），返回地址和清理函数。
func startSocks5(t *testing.T) (addr string, cleanup func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go handleSocks5(c)
		}
	}()
	return ln.Addr().String(), func() { ln.Close() }
}

func handleSocks5(conn net.Conn) {
	defer conn.Close()
	// 握手：VER(5) NMETHODS METHODS...
	hdr := make([]byte, 2)
	if _, err := io.ReadFull(conn, hdr); err != nil || hdr[0] != 5 {
		return
	}
	if _, err := io.CopyN(io.Discard, conn, int64(hdr[1])); err != nil {
		return
	}
	if _, err := conn.Write([]byte{5, 0}); err != nil { // 选择无认证
		return
	}
	// CONNECT 请求：VER CMD RSV ATYP...
	hdr = make([]byte, 4)
	if _, err := io.ReadFull(conn, hdr); err != nil || hdr[0] != 5 || hdr[1] != 1 {
		return
	}
	var host string
	switch hdr[3] {
	case 1: // IPv4
		b := make([]byte, 4)
		if _, err := io.ReadFull(conn, b); err != nil {
			return
		}
		host = net.IP(b).String()
	case 3: // 域名
		lb := make([]byte, 1)
		if _, err := io.ReadFull(conn, lb); err != nil {
			return
		}
		d := make([]byte, lb[0])
		if _, err := io.ReadFull(conn, d); err != nil {
			return
		}
		host = string(d)
	case 4: // IPv6
		b := make([]byte, 16)
		if _, err := io.ReadFull(conn, b); err != nil {
			return
		}
		host = net.IP(b).String()
	default:
		return
	}
	portB := make([]byte, 2)
	if _, err := io.ReadFull(conn, portB); err != nil {
		return
	}
	port := binary.BigEndian.Uint16(portB)

	up, err := net.Dial("tcp", net.JoinHostPort(host, strconv.Itoa(int(port))))
	if err != nil {
		return
	}
	defer up.Close()
	// 成功响应：VER REP RSV ATYP(1) BND.ADDR BND.PORT
	if _, err := conn.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
		return
	}
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(up, conn); done <- struct{}{} }()
	go func() { _, _ = io.Copy(conn, up); done <- struct{}{} }()
	<-done
}

// 复现线上 502 "invalid character '\x1f'"：上游返回 gzip 压缩的 JSON，
// 而代理没解压就去解析。修复后应正常转换，且 Accept-Encoding 不应转发给上游。
func TestProxyGzipUpstreamResponse(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if v := r.Header.Get("Accept-Encoding"); v != "" {
			t.Errorf("Accept-Encoding 不应转发给上游: %q", v)
		}
		body := []byte(`{"id":"resp_1","object":"response","status":"completed","model":"m","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi from gzip"}]}]}`)
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		_, _ = zw.Write(body)
		_ = zw.Close()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write(buf.Bytes())
	}))
	defer up.Close()

	cfg := &Config{Aliases: map[string]*Alias{
		"llm": {Upstream: up.URL, Protocol: ProtoOpenAIResponses},
	}}
	proxy := httptest.NewServer(NewProxy(cfg))
	defer proxy.Close()
	req, _ := http.NewRequest("POST", proxy.URL+"/llm/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept-Encoding", "gzip") // 模拟客户端带了 gzip
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(out), "hi from gzip") {
		t.Fatalf("gzip 响应未正确处理: status=%d body=%s", resp.StatusCode, out)
	}
}

// debug 模式下，上游请求日志要能看到伪装头，且 Authorization 等密钥必须打码。
func TestDebugLogRedactsSecrets(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_1","object":"response","status":"completed","model":"m","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}`))
	}))
	defer up.Close()
	cfg := &Config{
		Debug: true,
		Aliases: map[string]*Alias{
			"co": {
				Upstream: up.URL, Protocol: ProtoOpenAIResponses,
				Headers: map[string]string{
					"User-Agent": "codex_cli_rs/0.139.0 (Linux 6.5.0; x86_64)",
				},
			},
		},
	}
	proxy := httptest.NewServer(NewProxy(cfg))
	defer proxy.Close()

	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(old)

	req, _ := http.NewRequest("POST", proxy.URL+"/co/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer sk-super-secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	out := buf.String()
	if !strings.Contains(out, "triproxy 请求 LLM (alias=co)") {
		t.Fatalf("debug 日志缺失: %s", out)
	}
	if !strings.Contains(out, "User-Agent: codex_cli_rs/0.139.0 (Linux 6.5.0; x86_64)") {
		t.Fatalf("伪装 UA 未出现在 debug 日志: %s", out)
	}
	if !strings.Contains(out, "Authorization: [REDACTED]") {
		t.Fatalf("Authorization 未打码: %s", out)
	}
	if strings.Contains(out, "sk-super-secret") {
		t.Fatalf("密钥泄漏到日志: %s", out)
	}
}

// TestDebugLogPassthroughStream verifies the same-protocol passthrough path
// logs all four stages plus a stream content summary, so the model's actual
// output is visible even when the proxy does not convert.
func TestDebugLogPassthroughStream(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", `{"id":"x","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", `{"id":"x","choices":[{"index":0,"delta":{"content":"你好"},"finish_reason":null}]}`)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", `{"id":"x","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)
		_, _ = fmt.Fprintf(w, "data: [DONE]\n\n")
	}))
	defer up.Close()
	cfg := &Config{Debug: true, Aliases: map[string]*Alias{
		"tmp": {Upstream: up.URL, Protocol: ProtoOpenAIChat},
	}}
	proxy := httptest.NewServer(NewProxy(cfg))
	defer proxy.Close()

	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(old)

	req, _ := http.NewRequest("POST", proxy.URL+"/tmp/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	// 透传流式内容由后台 goroutine 解析后异步打印，轮询等待其落盘。
	out := ""
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		out = buf.String()
		if strings.Contains(out, "流式内容") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	for _, want := range []string{
		"triproxy 收到客户端请求：",
		"triproxy 请求 LLM",
		"triproxy 接收 LLM 响应：",
		"triproxy 响应客户端：",
		"流式内容",
		`文本: "你好"`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("debug 日志缺失 %q:\n%s", want, out)
		}
	}
}

// TestChatUpstreamAutoBridgesThinkingToMessages: a Chat upstream feeding Claude
// Code (Messages client) must surface reasoning_content as thinking blocks even
// without the thinking config, because Chat providers require the thinking to
// be echoed back on the next turn (else 400).
func TestChatUpstreamAutoBridgesThinkingToMessages(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", `{"id":"x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"thinking hard"},"finish_reason":null}]}`)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", `{"id":"x","choices":[{"index":0,"delta":{"content":"你好"},"finish_reason":null}]}`)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", `{"id":"x","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)
		_, _ = fmt.Fprintf(w, "data: [DONE]\n\n")
	}))
	defer up.Close()
	// 注意：Thinking 不配置（默认 false）
	cfg := &Config{Aliases: map[string]*Alias{
		"tmp": {Upstream: up.URL, Protocol: ProtoOpenAIChat},
	}}
	proxy := httptest.NewServer(NewProxy(cfg))
	defer proxy.Close()

	resp := doRequest(t, "POST", proxy.URL+"/tmp/v1/messages", "x-api-key sk", `{
		"model":"m","max_tokens":100,"stream":true,
		"messages":[{"role":"user","content":"北京天气"}]
	}`)
	out, _ := io.ReadAll(resp.Body)
	for _, want := range []string{
		`"type":"thinking"`,
		`"type":"thinking_delta"`,
		`"thinking":"thinking hard"`,
		`"type":"text_delta"`,
	} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("messages stream missing %q (thinking auto-bridge broken)\n%s", want, out)
		}
	}
}
