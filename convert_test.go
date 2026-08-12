package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// get digs a value out of a nested map using dot-separated keys and indexes
// like "choices.0.message.content".
func get(t *testing.T, v any, path string) any {
	t.Helper()
	cur := v
	for _, part := range strings.Split(path, ".") {
		if idx, ok := parseIndex(part); ok {
			arr, _ := cur.([]any)
			if idx >= len(arr) {
				t.Fatalf("path %s: index %d out of range (len %d)", path, idx, len(arr))
			}
			cur = arr[idx]
			continue
		}
		m, ok := cur.(map[string]any)
		if !ok {
			t.Fatalf("path %s: %v is not a map", path, cur)
		}
		cur, ok = m[part]
		if !ok {
			t.Fatalf("path %s: key %q missing", path, part)
		}
	}
	return cur
}

func parseIndex(s string) (int, bool) {
	var n int
	if s == "" {
		return 0, false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
	}
	return n, true
}

func asString(t *testing.T, v any) string {
	t.Helper()
	s, ok := v.(string)
	if !ok {
		t.Fatalf("expected string, got %T (%v)", v, v)
	}
	return s
}

func asNumber(t *testing.T, v any) float64 {
	t.Helper()
	f, ok := v.(float64)
	if !ok {
		t.Fatalf("expected number, got %T (%v)", v, v)
	}
	return f
}

func convertTo(t *testing.T, from, to, body string) map[string]any {
	t.Helper()
	out, err := convertRequest(from, to, []byte(body))
	if err != nil {
		t.Fatalf("convertRequest %s->%s: %v", from, to, err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("bad output json: %v\n%s", err, out)
	}
	return m
}

func convertRespTo(t *testing.T, from, to, body string) map[string]any {
	t.Helper()
	out, err := convertResponse(from, to, []byte(body))
	if err != nil {
		t.Fatalf("convertResponse %s->%s: %v", from, to, err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("bad output json: %v\n%s", err, out)
	}
	return m
}

func convertToOpts(t *testing.T, from, to, body string, opts *ConvertOpts) map[string]any {
	t.Helper()
	out, err := convertRequestOpts(from, to, []byte(body), opts)
	if err != nil {
		t.Fatalf("convertRequestOpts %s->%s: %v", from, to, err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("bad output json: %v\n%s", err, out)
	}
	return m
}

func convertRespToOpts(t *testing.T, from, to, body string, opts *ConvertOpts) map[string]any {
	t.Helper()
	out, err := convertResponseOpts(from, to, []byte(body), opts)
	if err != nil {
		t.Fatalf("convertResponseOpts %s->%s: %v", from, to, err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("bad output json: %v\n%s", err, out)
	}
	return m
}

func TestChatToMessagesReq(t *testing.T) {
	in := `{
		"model": "claude-sonnet",
		"messages": [
			{"role":"system","content":"You are helpful."},
			{"role":"user","content":"Hi"},
			{"role":"assistant","content":"Hello","tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"SF\"}"}}]},
			{"role":"tool","tool_call_id":"call_1","content":"{\"temp\":70}"}
		],
		"temperature": 0.5,
		"tools":[{"type":"function","function":{"name":"get_weather","description":"Get weather","parameters":{"type":"object"}}}],
		"tool_choice":"required",
		"stream": false
	}`
	m := convertTo(t, ProtoOpenAIChat, ProtoAnthropicMessages, in)
	if got := asString(t, get(t, m, "model")); got != "claude-sonnet" {
		t.Fatalf("model = %q", got)
	}
	if got := asString(t, get(t, m, "system")); got != "You are helpful." {
		t.Fatalf("system = %q", got)
	}
	if got := asNumber(t, get(t, m, "max_tokens")); got != DefaultMaxTokens {
		t.Fatalf("max_tokens = %v (want default %d)", got, DefaultMaxTokens)
	}
	if got := asString(t, get(t, m, "messages.0.role")); got != "user" {
		t.Fatalf("messages[0].role = %q", got)
	}
	if got := asString(t, get(t, m, "messages.0.content")); got != "Hi" {
		t.Fatalf("messages[0].content = %q", got)
	}
	if got := asString(t, get(t, m, "messages.1.content.0.text")); got != "Hello" {
		t.Fatalf("messages[1].content[0].text = %q", got)
	}
	if got := asString(t, get(t, m, "messages.1.content.1.type")); got != "tool_use" {
		t.Fatalf("messages[1].content[0].type = %q", got)
	}
	if got := asString(t, get(t, m, "messages.1.content.1.id")); got != "call_1" {
		t.Fatalf("tool_use id = %q", got)
	}
	if got := asString(t, get(t, m, "messages.1.content.1.input.city")); got != "SF" {
		t.Fatalf("tool_use input.city = %q", got)
	}
	if got := asString(t, get(t, m, "messages.2.content.0.type")); got != "tool_result" {
		t.Fatalf("messages[2].content[0].type = %q", got)
	}
	if got := asString(t, get(t, m, "messages.2.content.0.tool_use_id")); got != "call_1" {
		t.Fatalf("tool_result tool_use_id = %q", got)
	}
	if got := asString(t, get(t, m, "tools.0.name")); got != "get_weather" {
		t.Fatalf("tools[0].name = %q", got)
	}
	if got := asString(t, get(t, m, "tool_choice")); got != "any" {
		t.Fatalf("tool_choice = %q (want any)", got)
	}
}

func TestMessagesToChatReq(t *testing.T) {
	in := `{
		"model": "gpt-4o",
		"system": [{"type":"text","text":"Be brief."}],
		"messages": [
			{"role":"user","content":"What's the weather?"},
			{"role":"assistant","content":[{"type":"text","text":"Let me check."},{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"SF"}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"70F"}]}
		],
		"max_tokens": 100,
		"tools":[{"name":"get_weather","description":"Get","input_schema":{"type":"object"}}],
		"tool_choice":{"type":"tool","name":"get_weather"}
	}`
	m := convertTo(t, ProtoAnthropicMessages, ProtoOpenAIChat, in)
	if got := asString(t, get(t, m, "messages.0.role")); got != "system" {
		t.Fatalf("messages[0].role = %q", got)
	}
	if got := asString(t, get(t, m, "messages.0.content")); got != "Be brief." {
		t.Fatalf("system content = %q", got)
	}
	if got := asString(t, get(t, m, "messages.2.content")); got != "Let me check." {
		t.Fatalf("assistant content = %q", got)
	}
	if got := asString(t, get(t, m, "messages.2.tool_calls.0.id")); got != "toolu_1" {
		t.Fatalf("tool_calls[0].id = %q", got)
	}
	if got := asString(t, get(t, m, "messages.2.tool_calls.0.function.arguments")); got != `{"city":"SF"}` {
		t.Fatalf("arguments = %q", got)
	}
	if got := asString(t, get(t, m, "messages.3.role")); got != "tool" {
		t.Fatalf("messages[3].role = %q", got)
	}
	if got := asString(t, get(t, m, "messages.3.tool_call_id")); got != "toolu_1" {
		t.Fatalf("tool_call_id = %q", got)
	}
	if got := asNumber(t, get(t, m, "max_tokens")); got != 100 {
		t.Fatalf("max_tokens = %v", got)
	}
	if got := asString(t, get(t, m, "tool_choice.type")); got != "function" {
		t.Fatalf("tool_choice.type = %q", got)
	}
	if got := asString(t, get(t, m, "tool_choice.function.name")); got != "get_weather" {
		t.Fatalf("tool_choice.function.name = %q", got)
	}
}

func TestResponsesToChatReq(t *testing.T) {
	in := `{
		"model": "some-model",
		"instructions": "Be helpful",
		"input": [
			{"role":"user","content":"hi"},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"how are you"}]},
			{"type":"function_call","call_id":"call_9","name":"get_weather","arguments":"{\"city\":\"SF\"}"},
			{"type":"function_call_output","call_id":"call_9","output":"72F"}
		],
		"max_output_tokens": 256,
		"stream": false
	}`
	m := convertTo(t, ProtoOpenAIResponses, ProtoOpenAIChat, in)
	if got := asString(t, get(t, m, "messages.0.role")); got != "system" {
		t.Fatalf("messages[0].role = %q", got)
	}
	if got := asString(t, get(t, m, "messages.0.content")); got != "Be helpful" {
		t.Fatalf("system = %q", got)
	}
	if got := asString(t, get(t, m, "messages.1.content")); got != "hi" {
		t.Fatalf("messages[1] = %q", got)
	}
	if got := asString(t, get(t, m, "messages.2.content")); got != "how are you" {
		t.Fatalf("messages[2] = %q", got)
	}
	if got := asString(t, get(t, m, "messages.3.tool_calls.0.id")); got != "call_9" {
		t.Fatalf("tool call id = %q", got)
	}
	if got := asString(t, get(t, m, "messages.3.tool_calls.0.function.name")); got != "get_weather" {
		t.Fatalf("tool call name = %q", got)
	}
	if got := asString(t, get(t, m, "messages.4.role")); got != "tool" {
		t.Fatalf("messages[4].role = %q", got)
	}
	if got := asString(t, get(t, m, "messages.4.tool_call_id")); got != "call_9" {
		t.Fatalf("tool_call_id = %q", got)
	}
	if got := asString(t, get(t, m, "messages.4.content")); got != "72F" {
		t.Fatalf("tool output = %q", got)
	}
	if got := asNumber(t, get(t, m, "max_tokens")); got != 256 {
		t.Fatalf("max_tokens = %v", got)
	}
}

func TestChatToResponsesReq(t *testing.T) {
	in := `{
		"model":"m",
		"messages":[
			{"role":"system","content":"sys"},
			{"role":"user","content":"hi"},
			{"role":"assistant","content":"ok","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]},
			{"role":"tool","tool_call_id":"c1","content":"out"}
		],
		"max_tokens": 50
	}`
	m := convertTo(t, ProtoOpenAIChat, ProtoOpenAIResponses, in)
	if got := asString(t, get(t, m, "instructions")); got != "sys" {
		t.Fatalf("instructions = %q", got)
	}
	if got := asString(t, get(t, m, "input.0.type")); got != "message" {
		t.Fatalf("input[0].type = %q", got)
	}
	if got := asString(t, get(t, m, "input.0.content.0.type")); got != "input_text" {
		t.Fatalf("input[0].content[0].type = %q", got)
	}
	if got := asString(t, get(t, m, "input.0.content.0.text")); got != "hi" {
		t.Fatalf("input[0] text = %q", got)
	}
	if got := asString(t, get(t, m, "input.1.type")); got != "message" {
		t.Fatalf("input[1].type = %q", got)
	}
	if got := asString(t, get(t, m, "input.1.content.0.type")); got != "output_text" {
		t.Fatalf("input[1].content[0].type = %q", got)
	}
	if got := asString(t, get(t, m, "input.1.content.0.text")); got != "ok" {
		t.Fatalf("input[1] text = %q", got)
	}
	if got := asString(t, get(t, m, "input.2.type")); got != "function_call" {
		t.Fatalf("input[2].type = %q", got)
	}
	if got := asString(t, get(t, m, "input.2.call_id")); got != "c1" {
		t.Fatalf("call_id = %q", got)
	}
	if got := asString(t, get(t, m, "input.3.type")); got != "function_call_output" {
		t.Fatalf("input[3].type = %q", got)
	}
	if got := asNumber(t, get(t, m, "max_output_tokens")); got != 50 {
		t.Fatalf("max_output_tokens = %v", got)
	}
}

func TestResponsesToMessagesReq(t *testing.T) {
	in := `{
		"model":"m",
		"instructions":"sys",
		"input":[
			{"role":"user","content":"hi"},
			{"type":"function_call","call_id":"c1","name":"f","arguments":"{\"a\":1}"},
			{"type":"function_call_output","call_id":"c1","output":"2"}
		],
		"max_output_tokens": 100
	}`
	m := convertTo(t, ProtoOpenAIResponses, ProtoAnthropicMessages, in)
	if got := asString(t, get(t, m, "system")); got != "sys" {
		t.Fatalf("system = %q", got)
	}
	if got := asString(t, get(t, m, "messages.0.content")); got != "hi" {
		t.Fatalf("messages[0] = %q", got)
	}
	if got := asString(t, get(t, m, "messages.1.content.0.type")); got != "tool_use" {
		t.Fatalf("messages[1].content[0].type = %q", got)
	}
	if got := asNumber(t, get(t, m, "messages.1.content.0.input.a")); got != 1 {
		t.Fatalf("input.a = %v", got)
	}
	if got := asString(t, get(t, m, "messages.2.content.0.type")); got != "tool_result" {
		t.Fatalf("messages[2].content[0].type = %q", got)
	}
	if got := asString(t, get(t, m, "messages.2.content.0.content")); got != "2" {
		t.Fatalf("tool result = %q", got)
	}
	if got := asNumber(t, get(t, m, "max_tokens")); got != 100 {
		t.Fatalf("max_tokens = %v", got)
	}
}

func TestChatRespToResponsesResp(t *testing.T) {
	in := `{
		"id":"chatcmpl-x","object":"chat.completion","created":123,"model":"m",
		"choices":[{"index":0,"message":{"role":"assistant","content":"Hello world","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{\"x\":1}"}}]},"finish_reason":"tool_calls"}],
		"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}
	}`
	m := convertRespTo(t, ProtoOpenAIChat, ProtoOpenAIResponses, in)
	if got := asString(t, get(t, m, "object")); got != "response" {
		t.Fatalf("object = %q", got)
	}
	if got := asString(t, get(t, m, "status")); got != "completed" {
		t.Fatalf("status = %q", got)
	}
	if got := asString(t, get(t, m, "output.0.content.0.text")); got != "Hello world" {
		t.Fatalf("text = %q", got)
	}
	if got := asString(t, get(t, m, "output.1.type")); got != "function_call" {
		t.Fatalf("output[1].type = %q", got)
	}
	if got := asString(t, get(t, m, "output.1.arguments")); got != `{"x":1}` {
		t.Fatalf("arguments = %q", got)
	}
	if got := asNumber(t, get(t, m, "usage.input_tokens")); got != 10 {
		t.Fatalf("usage.input_tokens = %v", got)
	}
	if got := asNumber(t, get(t, m, "usage.output_tokens")); got != 5 {
		t.Fatalf("usage.output_tokens = %v", got)
	}
}

func TestMessagesRespToChatResp(t *testing.T) {
	in := `{
		"id":"msg_1","type":"message","role":"assistant","model":"m",
		"content":[{"type":"text","text":"Hi"},{"type":"tool_use","id":"tu1","name":"f","input":{"a":1}}],
		"stop_reason":"tool_use",
		"usage":{"input_tokens":10,"output_tokens":4}
	}`
	m := convertRespTo(t, ProtoAnthropicMessages, ProtoOpenAIChat, in)
	if got := asString(t, get(t, m, "object")); got != "chat.completion" {
		t.Fatalf("object = %q", got)
	}
	if got := asString(t, get(t, m, "choices.0.message.content")); got != "Hi" {
		t.Fatalf("content = %q", got)
	}
	if got := asString(t, get(t, m, "choices.0.message.tool_calls.0.id")); got != "tu1" {
		t.Fatalf("tool_calls[0].id = %q", got)
	}
	if got := asString(t, get(t, m, "choices.0.message.tool_calls.0.function.arguments")); got != `{"a":1}` {
		t.Fatalf("arguments = %q", got)
	}
	if got := asString(t, get(t, m, "choices.0.finish_reason")); got != "tool_calls" {
		t.Fatalf("finish_reason = %q", got)
	}
	if got := asNumber(t, get(t, m, "usage.prompt_tokens")); got != 10 {
		t.Fatalf("usage.prompt_tokens = %v", got)
	}
	if got := asNumber(t, get(t, m, "usage.completion_tokens")); got != 4 {
		t.Fatalf("usage.completion_tokens = %v", got)
	}
}

func TestResponsesRespToChatResp(t *testing.T) {
	in := `{
		"id":"resp_1","object":"response","created_at":123,"status":"completed","model":"m",
		"output":[
			{"type":"message","id":"msg_a","role":"assistant","content":[{"type":"output_text","text":"Hi","annotations":[]}]},
			{"type":"function_call","id":"fc_a","call_id":"c1","name":"f","arguments":"{}"}
		],
		"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}
	}`
	m := convertRespTo(t, ProtoOpenAIResponses, ProtoOpenAIChat, in)
	if got := asString(t, get(t, m, "choices.0.message.content")); got != "Hi" {
		t.Fatalf("content = %q", got)
	}
	if got := asString(t, get(t, m, "choices.0.message.tool_calls.0.id")); got != "c1" {
		t.Fatalf("tool_calls[0].id = %q", got)
	}
	if got := asString(t, get(t, m, "choices.0.finish_reason")); got != "stop" {
		t.Fatalf("finish_reason = %q", got)
	}
	if got := asNumber(t, get(t, m, "usage.prompt_tokens")); got != 10 {
		t.Fatalf("usage.prompt_tokens = %v", got)
	}
}

func TestResponsesRespToMessagesResp(t *testing.T) {
	in := `{
		"id":"resp_1","object":"response","created_at":123,"status":"completed","model":"m",
		"output":[
			{"type":"message","id":"msg_a","role":"assistant","content":[{"type":"output_text","text":"Hi","annotations":[]}]},
			{"type":"function_call","id":"fc_a","call_id":"c1","name":"f","arguments":"{\"b\":2}"}
		],
		"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}
	}`
	m := convertRespTo(t, ProtoOpenAIResponses, ProtoAnthropicMessages, in)
	if got := asString(t, get(t, m, "type")); got != "message" {
		t.Fatalf("type = %q", got)
	}
	if got := asString(t, get(t, m, "content.0.text")); got != "Hi" {
		t.Fatalf("content[0].text = %q", got)
	}
	if got := asString(t, get(t, m, "content.1.type")); got != "tool_use" {
		t.Fatalf("content[1].type = %q", got)
	}
	if got := asNumber(t, get(t, m, "content.1.input.b")); got != 2 {
		t.Fatalf("input.b = %v", got)
	}
	if got := asString(t, get(t, m, "stop_reason")); got != "end_turn" {
		t.Fatalf("stop_reason = %q", got)
	}
	if got := asNumber(t, get(t, m, "usage.input_tokens")); got != 10 {
		t.Fatalf("usage.input_tokens = %v", got)
	}
}

// TestAllProtocolCombos proves every client-protocol × upstream-protocol
// combination converts without error in both directions (request and
// response). The proxy also passes through when the two protocols match, so
// the matrix below covers all 9 possible routing paths.
func TestAllProtocolCombos(t *testing.T) {
	protos := []string{ProtoOpenAIChat, ProtoOpenAIResponses, ProtoAnthropicMessages}

	reqFor := map[string]string{
		ProtoOpenAIChat:        `{"model":"m","messages":[{"role":"system","content":"sys"},{"role":"user","content":"hi"}],"stream":false}`,
		ProtoOpenAIResponses:   `{"model":"m","instructions":"sys","input":[{"role":"user","content":"hi"}],"stream":false}`,
		ProtoAnthropicMessages: `{"model":"m","system":"sys","max_tokens":100,"messages":[{"role":"user","content":"hi"}],"stream":false}`,
	}
	respFor := map[string]string{
		ProtoOpenAIChat:        `{"id":"chatcmpl-1","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
		ProtoOpenAIResponses:   `{"id":"resp_1","object":"response","status":"completed","model":"m","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi","annotations":[]}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`,
		ProtoAnthropicMessages: `{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`,
	}

	for _, from := range protos {
		for _, to := range protos {
			if _, err := convertRequest(from, to, []byte(reqFor[from])); err != nil {
				t.Errorf("request %s->%s: %v", from, to, err)
			}
			if _, err := convertResponse(from, to, []byte(respFor[from])); err != nil {
				t.Errorf("response %s->%s: %v", from, to, err)
			}
		}
	}
}

// TestResponsesReqWithNonFunctionToolToChat reproduces the upstream error
// "tools[N].function.name invalid, should be set": a Responses request may
// carry non-function tools (e.g. {"type":"web_search"}) that have no name.
// When converted to Chat, those must not become functions with empty names.
func TestResponsesReqWithNonFunctionToolToChat(t *testing.T) {
	in := `{
		"model":"m",
		"tools":[
			{"type":"function","name":"get_weather","description":"d","parameters":{"type":"object"}},
			{"type":"web_search"}
		],
		"input":[{"role":"user","content":"hi"}]
	}`
	m := convertTo(t, ProtoOpenAIResponses, ProtoOpenAIChat, in)
	tools, ok := m["tools"].([]any)
	if !ok {
		t.Fatalf("tools missing: %v", m)
	}
	if len(tools) != 1 {
		t.Fatalf("expected 1 function tool after dropping web_search, got %d: %v", len(tools), tools)
	}
	if got := asString(t, get(t, m, "tools.0.function.name")); got != "get_weather" {
		t.Fatalf("tools[0].function.name = %q", got)
	}
}

// TestResponsesReqWithChatStyleTool: some clients send chat-style tools
// (function wrapper) even on the Responses endpoint; the name must still be
// picked up from function.name.
func TestResponsesReqWithChatStyleTool(t *testing.T) {
	in := `{
		"model":"m",
		"tools":[{"type":"function","function":{"name":"f","description":"d","parameters":{"type":"object"}}}],
		"input":[{"role":"user","content":"hi"}]
	}`
	m := convertTo(t, ProtoOpenAIResponses, ProtoOpenAIChat, in)
	if got := asString(t, get(t, m, "tools.0.function.name")); got != "f" {
		t.Fatalf("tools[0].function.name = %q", got)
	}
}

// TestResponsesFunctionCallOutputObject verifies that a structured (non-string)
// function_call_output is preserved as compact JSON instead of being dropped
// when converted to the Anthropic Messages protocol.
func TestResponsesFunctionCallOutputObject(t *testing.T) {
	in := `{
		"model":"m",
		"input":[
			{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"},
			{"type":"function_call_output","call_id":"c1","output":{"temp":20,"unit":"C"}}
		]
	}`
	m := convertTo(t, ProtoOpenAIResponses, ProtoAnthropicMessages, in)
	if got := asString(t, get(t, m, "messages.1.content.0.content")); got != `{"temp":20,"unit":"C"}` {
		t.Fatalf("tool result content = %q", got)
	}
}

// TestResponsesFunctionCallOutputArray: an array-typed function_call_output is
// preserved as a structured tool_result content array (parts), not flattened.
func TestResponsesFunctionCallOutputArray(t *testing.T) {
	in := `{
		"model":"m",
		"input":[
			{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"},
			{"type":"function_call_output","call_id":"c1","output":[{"type":"input_text","text":"result"}]}
		]
	}`
	m := convertTo(t, ProtoOpenAIResponses, ProtoAnthropicMessages, in)
	if got := asString(t, get(t, m, "messages.1.content.0.content.0.type")); got != "text" {
		t.Fatalf("content[0].type = %q (want text)", got)
	}
	if got := asString(t, get(t, m, "messages.1.content.0.content.0.text")); got != "result" {
		t.Fatalf("content[0].text = %q", got)
	}
}

// TestMessagesRespToResponsesIncomplete verifies a max_tokens stop is surfaced
// as status "incomplete" with incomplete_details in the Responses output.
func TestMessagesRespToResponsesIncomplete(t *testing.T) {
	in := `{
		"id":"msg_1","type":"message","role":"assistant","model":"m",
		"content":[{"type":"text","text":"partial"}],
		"stop_reason":"max_tokens",
		"usage":{"input_tokens":10,"output_tokens":4}
	}`
	m := convertRespTo(t, ProtoAnthropicMessages, ProtoOpenAIResponses, in)
	if got := asString(t, get(t, m, "status")); got != "incomplete" {
		t.Fatalf("status = %q (want incomplete)", got)
	}
	if got := asString(t, get(t, m, "incomplete_details.reason")); got != "max_output_tokens" {
		t.Fatalf("incomplete_details.reason = %q", got)
	}
}

// TestResponsesToMessagesReasoningParam verifies the reasoning.effort →
// Anthropic thinking budget bridge, and that it is disabled without the flag.
func TestResponsesToMessagesReasoningParam(t *testing.T) {
	in := `{
		"model":"m","reasoning":{"effort":"high"},
		"input":[{"role":"user","content":"hi"}]
	}`
	on := &ConvertOpts{ReasoningBridge: true}
	m := convertToOpts(t, ProtoOpenAIResponses, ProtoAnthropicMessages, in, on)
	if got := asNumber(t, get(t, m, "thinking.budget_tokens")); got != 32000 {
		t.Fatalf("thinking.budget_tokens = %v (want 32000)", got)
	}
	if got := asString(t, get(t, m, "thinking.type")); got != "enabled" {
		t.Fatalf("thinking.type = %q", got)
	}

	// Without the bridge the reasoning request must not leak into Messages.
	off := convertTo(t, ProtoOpenAIResponses, ProtoAnthropicMessages, in)
	if _, ok := off["thinking"]; ok {
		t.Fatalf("thinking must be absent without the thinking flag: %v", off)
	}
}

// TestMessagesToResponsesThinkingParam is the inverse bridge: Anthropic thinking
// → Responses reasoning effort.
func TestMessagesToResponsesThinkingParam(t *testing.T) {
	in := `{
		"model":"m","thinking":{"type":"enabled","budget_tokens":32000},
		"messages":[{"role":"user","content":"hi"}]
	}`
	m := convertToOpts(t, ProtoAnthropicMessages, ProtoOpenAIResponses, in, &ConvertOpts{ReasoningBridge: true})
	if got := asString(t, get(t, m, "reasoning.effort")); got != "high" {
		t.Fatalf("reasoning.effort = %q (want high)", got)
	}
}

// TestResponsesToMessagesReasoningInputItem verifies a replayed Responses
// reasoning item becomes an Anthropic thinking block, with the encrypted item
// carried losslessly in the signature envelope.
func TestResponsesToMessagesReasoningInputItem(t *testing.T) {
	in := `{
		"model":"m",
		"input":[
			{"role":"user","content":"hi"},
			{"id":"rs_1","type":"reasoning","summary":[{"type":"summary_text","text":"thinking hard"}],"encrypted_content":"opaque"}
		]
	}`
	m := convertToOpts(t, ProtoOpenAIResponses, ProtoAnthropicMessages, in, &ConvertOpts{ReasoningBridge: true})
	if got := asString(t, get(t, m, "messages.1.content.0.type")); got != "thinking" {
		t.Fatalf("messages[1].content[0].type = %q (want thinking)", got)
	}
	if got := asString(t, get(t, m, "messages.1.content.0.thinking")); got != "thinking hard" {
		t.Fatalf("thinking text = %q", got)
	}
	sig := asString(t, get(t, m, "messages.1.content.0.signature"))
	if !strings.HasPrefix(sig, "triproxy-reasoning-v1:") {
		t.Fatalf("signature envelope = %q (want triproxy-reasoning-v1: prefix)", sig)
	}
}

// TestMessagesRespToResponsesReasoning verifies an Anthropic thinking block in
// a response becomes a Responses reasoning item.
func TestMessagesRespToResponsesReasoning(t *testing.T) {
	in := `{
		"id":"msg_1","type":"message","role":"assistant","model":"m",
		"content":[
			{"type":"thinking","thinking":"let me think","signature":"abc"},
			{"type":"text","text":"hello"}
		],
		"stop_reason":"end_turn",
		"usage":{"input_tokens":1,"output_tokens":1}
	}`
	m := convertRespToOpts(t, ProtoAnthropicMessages, ProtoOpenAIResponses, in, &ConvertOpts{ReasoningBridge: true})
	if got := asString(t, get(t, m, "output.0.type")); got != "reasoning" {
		t.Fatalf("output[0].type = %q (want reasoning)", got)
	}
	if got := asString(t, get(t, m, "output.0.summary.0.text")); got != "let me think" {
		t.Fatalf("reasoning summary = %q", got)
	}
	if got := asString(t, get(t, m, "output.1.type")); got != "message" {
		t.Fatalf("output[1].type = %q (want message)", got)
	}
}

// TestResponsesRespToMessagesThinking is the inverse response bridge: a
// Responses reasoning item becomes an Anthropic thinking block.
func TestResponsesRespToMessagesThinking(t *testing.T) {
	in := `{
		"id":"resp_1","object":"response","status":"completed","model":"m",
		"output":[
			{"id":"rs_1","type":"reasoning","summary":[{"type":"summary_text","text":"planning"}],"encrypted_content":null},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done","annotations":[]}]}
		],
		"usage":{"input_tokens":1,"output_tokens":1}
	}`
	m := convertRespToOpts(t, ProtoOpenAIResponses, ProtoAnthropicMessages, in, &ConvertOpts{ReasoningBridge: true})
	if got := asString(t, get(t, m, "content.0.type")); got != "thinking" {
		t.Fatalf("content[0].type = %q (want thinking)", got)
	}
	if got := asString(t, get(t, m, "content.0.thinking")); got != "planning" {
		t.Fatalf("thinking text = %q", got)
	}
	if got := asString(t, get(t, m, "content.1.text")); got != "done" {
		t.Fatalf("content[1].text = %q", got)
	}
}

// TestChatRespReasoningContentToResponses bridges the Chat reasoning_content
// convention (DeepSeek/Qwen family) into a Responses reasoning item.
func TestChatRespReasoningContentToResponses(t *testing.T) {
	in := `{
		"id":"chatcmpl-1","object":"chat.completion","model":"m",
		"choices":[{"index":0,"message":{"role":"assistant","content":"hello","reasoning_content":"thinking"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
	}`
	m := convertRespToOpts(t, ProtoOpenAIChat, ProtoOpenAIResponses, in, &ConvertOpts{ReasoningBridge: true})
	if got := asString(t, get(t, m, "output.0.type")); got != "reasoning" {
		t.Fatalf("output[0].type = %q (want reasoning)", got)
	}
	if got := asString(t, get(t, m, "output.0.summary.0.text")); got != "thinking" {
		t.Fatalf("reasoning summary = %q", got)
	}
}

// TestResponsesToMessagesDocument verifies Responses input_file → Anthropic
// document conversion.
func TestResponsesToMessagesDocument(t *testing.T) {
	in := `{
		"model":"m",
		"input":[
			{"role":"user","content":[
				{"type":"input_text","text":"read this"},
				{"type":"input_file","file_url":"https://example.com/doc.pdf","filename":"doc.pdf"}
			]}
		]
	}`
	m := convertTo(t, ProtoOpenAIResponses, ProtoAnthropicMessages, in)
	if got := asString(t, get(t, m, "messages.0.content.1.type")); got != "document" {
		t.Fatalf("content[1].type = %q (want document)", got)
	}
	if got := asString(t, get(t, m, "messages.0.content.1.source.url")); got != "https://example.com/doc.pdf" {
		t.Fatalf("source.url = %q", got)
	}
	if got := asString(t, get(t, m, "messages.0.content.1.title")); got != "doc.pdf" {
		t.Fatalf("title = %q", got)
	}
}

// TestMessagesToResponsesDocument is the inverse: Anthropic document → Responses
// input_file.
func TestMessagesToResponsesDocument(t *testing.T) {
	in := `{
		"model":"m",
		"messages":[{"role":"user","content":[
			{"type":"text","text":"read"},
			{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"JVBERi0="},"title":"doc.pdf"}
		]}]
	}`
	m := convertTo(t, ProtoAnthropicMessages, ProtoOpenAIResponses, in)
	if got := asString(t, get(t, m, "input.0.content.1.type")); got != "input_file" {
		t.Fatalf("content[1].type = %q (want input_file)", got)
	}
	if got := asString(t, get(t, m, "input.0.content.1.file_data")); got != "data:application/pdf;base64,JVBERi0=" {
		t.Fatalf("file_data = %q", got)
	}
	if got := asString(t, get(t, m, "input.0.content.1.filename")); got != "doc.pdf" {
		t.Fatalf("filename = %q", got)
	}
}

// TestResponsesToMessagesToolResultMedia verifies a function_call_output whose
// output is an array of parts (text + image) becomes a tool_result content
// array instead of being flattened to a string.
func TestResponsesToMessagesToolResultMedia(t *testing.T) {
	in := `{
		"model":"m",
		"input":[
			{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"},
			{"type":"function_call_output","call_id":"c1","output":[
				{"type":"input_text","text":"result:"},
				{"type":"input_image","image_url":"data:image/png;base64,AAAA"}
			]}
		]
	}`
	m := convertTo(t, ProtoOpenAIResponses, ProtoAnthropicMessages, in)
	if got := asString(t, get(t, m, "messages.1.content.0.type")); got != "tool_result" {
		t.Fatalf("messages[1].content[0].type = %q", got)
	}
	if got := asString(t, get(t, m, "messages.1.content.0.content.0.type")); got != "text" {
		t.Fatalf("content[0].type = %q (want text)", got)
	}
	if got := asString(t, get(t, m, "messages.1.content.0.content.0.text")); got != "result:" {
		t.Fatalf("content[0].text = %q", got)
	}
	if got := asString(t, get(t, m, "messages.1.content.0.content.1.type")); got != "image" {
		t.Fatalf("content[1].type = %q (want image)", got)
	}
	if got := asString(t, get(t, m, "messages.1.content.0.content.1.source.data")); got != "AAAA" {
		t.Fatalf("image data = %q", got)
	}
}

// TestMessagesToResponsesToolResultMedia is the inverse: an Anthropic
// tool_result content array becomes a Responses output part array.
func TestMessagesToResponsesToolResultMedia(t *testing.T) {
	in := `{
		"model":"m",
		"messages":[
			{"role":"assistant","content":[{"type":"tool_use","id":"tu1","name":"f","input":{}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu1","content":[
				{"type":"text","text":"result:"},
				{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}
			]}]}
		]
	}`
	m := convertTo(t, ProtoAnthropicMessages, ProtoOpenAIResponses, in)
	if got := asString(t, get(t, m, "input.1.type")); got != "function_call_output" {
		t.Fatalf("input[1].type = %q", got)
	}
	if got := asString(t, get(t, m, "input.1.output.0.type")); got != "input_text" {
		t.Fatalf("output[0].type = %q (want input_text)", got)
	}
	if got := asString(t, get(t, m, "input.1.output.0.text")); got != "result:" {
		t.Fatalf("output[0].text = %q", got)
	}
	if got := asString(t, get(t, m, "input.1.output.1.type")); got != "input_image" {
		t.Fatalf("output[1].type = %q (want input_image)", got)
	}
	if got := asString(t, get(t, m, "input.1.output.1.image_url")); got != "data:image/png;base64,AAAA" {
		t.Fatalf("image_url = %q", got)
	}
}

// TestParallelToolCallsMapping verifies Responses parallel_tool_calls ↔
// Messages disable_parallel_tool_use (inverse flags).
func TestParallelToolCallsMapping(t *testing.T) {
	r2m := convertTo(t, ProtoOpenAIResponses, ProtoAnthropicMessages, `{
		"model":"m","parallel_tool_calls":false,"input":[{"role":"user","content":"hi"}]
	}`)
	if got := r2m["disable_parallel_tool_use"]; got != true {
		t.Fatalf("disable_parallel_tool_use = %v (want true)", got)
	}

	m2r := convertTo(t, ProtoAnthropicMessages, ProtoOpenAIResponses, `{
		"model":"m","disable_parallel_tool_use":true,"messages":[{"role":"user","content":"hi"}]
	}`)
	if got := m2r["parallel_tool_calls"]; got != false {
		t.Fatalf("parallel_tool_calls = %v (want false)", got)
	}
}

// TestResponsesEmptyToolSchemaNormalized reproduces the upstream 400
// "Invalid schema for function 'apply_patch': got 'type: null'": Codex's
// free-form tools carry "parameters": {}. Converted Chat/Messages requests must
// emit a schema with "type": "object" instead of passing the empty object
// through.
func TestResponsesEmptyToolSchemaNormalized(t *testing.T) {
	in := `{
		"model":"m",
		"tools":[
			{"type":"function","name":"apply_patch","description":"freeform","parameters":{}},
			{"type":"function","name":"normal","description":"d","parameters":{"type":"object","properties":{"x":{"type":"string"}}}}
		],
		"input":[{"role":"user","content":"hi"}]
	}`
	// → Chat
	chat := convertTo(t, ProtoOpenAIResponses, ProtoOpenAIChat, in)
	if got := asString(t, get(t, chat, "tools.0.function.parameters.type")); got != "object" {
		t.Fatalf("chat tools[0] parameters.type = %q (want object)", got)
	}
	if got := asString(t, get(t, chat, "tools.1.function.parameters.type")); got != "object" {
		t.Fatalf("chat tools[1] parameters.type = %q (want object, passthrough)", got)
	}
	// → Messages
	msg := convertTo(t, ProtoOpenAIResponses, ProtoAnthropicMessages, in)
	if got := asString(t, get(t, msg, "tools.0.input_schema.type")); got != "object" {
		t.Fatalf("messages tools[0] input_schema.type = %q (want object)", got)
	}
}

// TestChatRespObjectArguments: non-streaming Chat tool_calls whose arguments
// arrive as a JSON object must be preserved as compact JSON, not dropped.
func TestChatRespObjectArguments(t *testing.T) {
	in := `{
		"id":"chatcmpl-1","object":"chat.completion","model":"m",
		"choices":[{"index":0,"message":{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":{"cmd":"ls"}}}]},"finish_reason":"tool_calls"}],
		"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
	}`
	m := convertRespTo(t, ProtoOpenAIChat, ProtoOpenAIResponses, in)
	if got := asString(t, get(t, m, "output.1.type")); got != "function_call" {
		t.Fatalf("output[1].type = %q (want function_call)", got)
	}
	if got := asString(t, get(t, m, "output.1.arguments")); got != `{"cmd":"ls"}` {
		t.Fatalf("output[1].arguments = %q (want {\"cmd\":\"ls\"})", got)
	}
}

// TestMessagesThinkingToChatReasoningContent reproduces the upstream 400
// "The reasoning_content in the thinking mode must be passed back to the API":
// Claude Code echoes its received thinking blocks back, and the converted Chat
// request must carry them as reasoning_content alongside the tool calls.
func TestMessagesThinkingToChatReasoningContent(t *testing.T) {
	in := `{
		"model":"m",
		"messages":[
			{"role":"user","content":"北京天气"},
			{"role":"assistant","content":[
				{"type":"thinking","thinking":"let me think","signature":"sig1"},
				{"type":"tool_use","id":"tu1","name":"Bash","input":{"command":"curl wttr.in"}}
			]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu1","content":"ok"}]}
		]
	}`
	m := convertTo(t, ProtoAnthropicMessages, ProtoOpenAIChat, in)
	if got := asString(t, get(t, m, "messages.1.reasoning_content")); got != "let me think" {
		t.Fatalf("reasoning_content = %q (want 'let me think')", got)
	}
	if got := asString(t, get(t, m, "messages.1.tool_calls.0.function.name")); got != "Bash" {
		t.Fatalf("tool_calls[0].name = %q (want Bash)", got)
	}
}
