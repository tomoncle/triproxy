package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func parseEvents(t *testing.T, proto, src string) []streamEv {
	t.Helper()
	var evs []streamEv
	if err := parseUpstreamStream(proto, strings.NewReader(src), func(ev streamEv) error {
		evs = append(evs, ev)
		return nil
	}); err != nil {
		t.Fatalf("parse %s stream: %v", proto, err)
	}
	return evs
}

func encodeStream(t *testing.T, proto string, evs []streamEv) string {
	t.Helper()
	return encodeStreamBridge(t, proto, evs, false)
}

// encodeStreamBridge is encodeStream with an explicit reasoning-bridge flag.
func encodeStreamBridge(t *testing.T, proto string, evs []streamEv, bridge bool) string {
	t.Helper()
	var buf bytes.Buffer
	enc := newStreamEncoder(proto, &buf, "test-model", bridge)
	for _, ev := range evs {
		if err := enc.Write(ev); err != nil {
			t.Fatalf("encode %s event: %v", proto, err)
		}
	}
	if err := enc.Close(); err != nil {
		t.Fatalf("close %s encoder: %v", proto, err)
	}
	return buf.String()
}

func TestParseChatStream(t *testing.T) {
	src := `data: {"id":"x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}

data: {"id":"x","choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":null}]}

data: {"id":"x","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"f","arguments":""}}]},"finish_reason":null}]}

data: {"id":"x","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"a\":1}"}}]},"finish_reason":null}]}

data: {"id":"x","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}

data: [DONE]
`
	evs := parseEvents(t, ProtoOpenAIChat, src)
	var texts, args []string
	var began, done bool
	var fr string
	for _, ev := range evs {
		switch ev.kind {
		case evText:
			texts = append(texts, ev.text)
		case evToolBegin:
			began = ev.toolID == "c1" && ev.toolName == "f"
		case evToolArg:
			args = append(args, ev.toolArgs)
		case evDone:
			done = true
			fr = ev.finishReason
		}
	}
	if len(texts) != 1 || texts[0] != "Hello" {
		t.Fatalf("texts = %v", texts)
	}
	if !began {
		t.Fatal("tool begin missing")
	}
	if len(args) != 1 || args[0] != `{"a":1}` {
		t.Fatalf("args = %v", args)
	}
	if !done || fr != "tool_calls" {
		t.Fatalf("done=%v fr=%q", done, fr)
	}
}

func TestChatStreamToMessagesStream(t *testing.T) {
	chatSrc := `data: {"id":"x","choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":null}]}

data: {"id":"x","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"f","arguments":""}}]},"finish_reason":null}]}

data: {"id":"x","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"a\":1}"}}]},"finish_reason":null}]}

data: {"id":"x","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}

data: [DONE]
`
	out := encodeStream(t, ProtoAnthropicMessages, parseEvents(t, ProtoOpenAIChat, chatSrc))
	for _, want := range []string{
		"event: message_start",
		`"type":"text_delta"`,
		`"text":"Hello"`,
		"event: content_block_start",
		`"type":"tool_use"`,
		`"type":"input_json_delta"`,
		`"partial_json":"{\"a\":1}"`,
		`"stop_reason":"tool_use"`,
		"event: message_stop",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("messages stream missing %q\n---\n%s", want, out)
		}
	}
}

func TestChatStreamToResponsesStream(t *testing.T) {
	chatSrc := `data: {"id":"x","choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":null}]}

data: {"id":"x","choices":[{"index":0,"delta":{"content":" world"},"finish_reason":null}]}

data: {"id":"x","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

data: [DONE]
`
	out := encodeStream(t, ProtoOpenAIResponses, parseEvents(t, ProtoOpenAIChat, chatSrc))
	for _, want := range []string{
		"event: response.created",
		"event: response.output_text.delta",
		`"delta":"Hello"`,
		`"delta":" world"`,
		`"snapshot":"Hello world"`,
		"event: response.output_text.done",
		"event: response.completed",
		`"status":"completed"`,
		`"type":"output_text"`,
		`"text":"Hello world"`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("responses stream missing %q\n---\n%s", want, out)
		}
	}
}

func TestResponsesStreamToChatStream(t *testing.T) {
	src := `event: response.created
data: {"type":"response.created","response":{"id":"resp_1","object":"response","model":"m","status":"in_progress"}}

event: response.output_text.delta
data: {"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":"Hello"}

event: response.output_text.delta
data: {"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":" world"}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","object":"response","status":"completed","model":"m","usage":{"input_tokens":1,"output_tokens":2}}}
`
	out := encodeStream(t, ProtoOpenAIChat, parseEvents(t, ProtoOpenAIResponses, src))
	if !strings.Contains(out, `"content":"Hello"`) {
		t.Fatalf("chat stream missing first delta\n%s", out)
	}
	if !strings.Contains(out, `"content":" world"`) {
		t.Fatalf("chat stream missing second delta\n%s", out)
	}
	if !strings.Contains(out, `"finish_reason":"stop"`) {
		t.Fatalf("chat stream missing finish_reason\n%s", out)
	}
	if !strings.Contains(out, "data: [DONE]") {
		t.Fatalf("chat stream missing [DONE]\n%s", out)
	}
}

func TestMessagesStreamToChatStream(t *testing.T) {
	src := `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"usage":{"input_tokens":1,"output_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":2}}

event: message_stop
data: {"type":"message_stop"}
`
	out := encodeStream(t, ProtoOpenAIChat, parseEvents(t, ProtoAnthropicMessages, src))
	if !strings.Contains(out, `"content":"Hello"`) {
		t.Fatalf("chat stream missing text\n%s", out)
	}
	if !strings.Contains(out, `"finish_reason":"stop"`) {
		t.Fatalf("chat stream missing finish_reason\n%s", out)
	}
	if !strings.Contains(out, "data: [DONE]") {
		t.Fatalf("chat stream missing [DONE]\n%s", out)
	}
}

func TestMessagesStreamToolUseToChatStream(t *testing.T) {
	src := `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[]}}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"city\":"}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"SF\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":5}}

event: message_stop
data: {"type":"message_stop"}
`
	out := encodeStream(t, ProtoOpenAIChat, parseEvents(t, ProtoAnthropicMessages, src))
	if !strings.Contains(out, `"id":"toolu_1"`) {
		t.Fatalf("chat stream missing tool id\n%s", out)
	}
	if !strings.Contains(out, `"name":"get_weather"`) {
		t.Fatalf("chat stream missing tool name\n%s", out)
	}
	if !strings.Contains(out, `"arguments":"{\"city\":"`) || !strings.Contains(out, `"arguments":"\"SF\"}"`) {
		t.Fatalf("chat stream missing tool argument deltas\n%s", out)
	}
	if !strings.Contains(out, `"finish_reason":"tool_calls"`) {
		t.Fatalf("chat stream missing finish_reason tool_calls\n%s", out)
	}
}

func TestStreamFallbackFromFullResponse(t *testing.T) {
	// A complete non-stream chat response synthesized into a Responses stream.
	full := `{"id":"chatcmpl-x","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"Hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`
	resp, err := parseChatResp([]byte(full))
	if err != nil {
		t.Fatal(err)
	}
	out := encodeStream(t, ProtoOpenAIResponses, respToStreamEvents(resp, "m"))
	if !strings.Contains(out, "event: response.created") ||
		!strings.Contains(out, `"delta":"Hi"`) ||
		!strings.Contains(out, "event: response.completed") {
		t.Fatalf("fallback stream wrong\n%s", out)
	}
}

// TestResponsesStreamMultiToolParallel drives the full pipeline for parallel
// Anthropic tool use: Messages SSE -> canonical events -> Responses SSE. It
// verifies the parser routes input_json_delta to the right tool by index, the
// encoder keeps each tool's arguments separate, function_call_arguments.done
// carries the tool name, every event has an increasing sequence_number, and
// response.completed carries both output items and usage.
func TestResponsesStreamMultiToolParallel(t *testing.T) {
	src := `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"usage":{"input_tokens":12,"output_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"tu_a","name":"tool_a","input":{}}}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"tu_b","name":"tool_b","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"x\":"}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"y\":"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"1}"}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"2}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":8}}

event: message_stop
data: {"type":"message_stop"}
`
	evs := parseEvents(t, ProtoAnthropicMessages, src)
	var argsA, argsB strings.Builder
	var usage *Usage
	for _, ev := range evs {
		if ev.kind == evToolArg {
			switch ev.toolID {
			case "tu_a":
				argsA.WriteString(ev.toolArgs)
			case "tu_b":
				argsB.WriteString(ev.toolArgs)
			}
		}
		if ev.kind == evDone {
			usage = ev.usage
		}
	}
	if argsA.String() != `{"x":1}` || argsB.String() != `{"y":2}` {
		t.Fatalf("parser routed args wrong: A=%q B=%q", argsA.String(), argsB.String())
	}
	if usage == nil || usage.InputTokens != 12 || usage.OutputTokens != 8 {
		t.Fatalf("usage = %+v (want input=12 output=8)", usage)
	}

	out := encodeStream(t, ProtoOpenAIResponses, evs)
	var seqs []int64
	doneByName := map[string]string{}
	completedOutput := map[string]string{} // call_id -> arguments
	status := ""
	err := readSSE(strings.NewReader(out), func(_ string, data []byte) error {
		var e struct {
			Type           string `json:"type"`
			SequenceNumber int64  `json:"sequence_number"`
			Name           string `json:"name"`
			Arguments      string `json:"arguments"`
			Item           struct {
				Type      string `json:"type"`
				CallID    string `json:"call_id"`
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"item"`
			Response struct {
				Status string `json:"status"`
				Output []struct {
					Type      string `json:"type"`
					CallID    string `json:"call_id"`
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"output"`
				Usage struct {
					InputTokens  int64 `json:"input_tokens"`
					OutputTokens int64 `json:"output_tokens"`
				} `json:"usage"`
			} `json:"response"`
		}
		if err := json.Unmarshal(data, &e); err != nil {
			return err
		}
		if e.Type != "" {
			seqs = append(seqs, e.SequenceNumber)
		}
		switch e.Type {
		case "response.function_call_arguments.done":
			if e.Name == "" {
				t.Errorf("function_call_arguments.done missing name")
			}
			doneByName[e.Name] = e.Arguments
		case "response.completed":
			status = e.Response.Status
			for _, o := range e.Response.Output {
				if o.Type == "function_call" {
					completedOutput[o.CallID] = o.Arguments
				}
			}
			if e.Response.Usage.InputTokens != 12 || e.Response.Usage.OutputTokens != 8 {
				t.Errorf("completed usage = %+v", e.Response.Usage)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < len(seqs); i++ {
		if seqs[i] <= seqs[i-1] {
			t.Fatalf("sequence_number not strictly increasing: %v", seqs)
		}
	}
	if doneByName["tool_a"] != `{"x":1}` || doneByName["tool_b"] != `{"y":2}` {
		t.Fatalf("doneByName = %v", doneByName)
	}
	if completedOutput["tu_a"] != `{"x":1}` || completedOutput["tu_b"] != `{"y":2}` {
		t.Fatalf("completedOutput = %v", completedOutput)
	}
	if status != "completed" {
		t.Fatalf("status = %q (want completed)", status)
	}
}

// TestResponsesStreamIncomplete verifies that a length-truncated upstream
// (Anthropic max_tokens) is surfaced as an incomplete response with
// incomplete_details instead of a plain completed one.
func TestResponsesStreamIncomplete(t *testing.T) {
	out := encodeStream(t, ProtoOpenAIResponses, []streamEv{{kind: evDone, finishReason: "length"}})
	if !strings.Contains(out, `"status":"incomplete"`) {
		t.Fatalf("expected incomplete status\n%s", out)
	}
	if !strings.Contains(out, `"incomplete_details"`) || !strings.Contains(out, `"max_output_tokens"`) {
		t.Fatalf("expected incomplete_details reason=max_output_tokens\n%s", out)
	}
}

// TestMessagesStreamThinkingToResponsesStream verifies Anthropic thinking
// deltas become Responses reasoning events (bridge on).
func TestMessagesStreamThinkingToResponsesStream(t *testing.T) {
	src := `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"usage":{"input_tokens":1,"output_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"Let me"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":" think"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"hello"}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":1}}

event: message_stop
data: {"type":"message_stop"}
`
	evs := parseEvents(t, ProtoAnthropicMessages, src)
	out := encodeStreamBridge(t, ProtoOpenAIResponses, evs, true)
	for _, want := range []string{
		"event: response.reasoning_summary_part.added",
		`"delta":"Let me"`,
		`"snapshot":"Let me think"`,
		"event: response.reasoning_summary_part.done",
		`"type":"reasoning"`,
		`"delta":"hello"`,
		"event: response.completed",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("responses stream missing %q\n%s", want, out)
		}
	}
	if !strings.Contains(out, `"text":"Let me think"`) {
		t.Fatalf("reasoning summary missing from output_item.done\n%s", out)
	}
}

// TestResponsesStreamReasoningToMessagesStream is the inverse streaming bridge:
// Responses reasoning events become Anthropic thinking blocks.
func TestResponsesStreamReasoningToMessagesStream(t *testing.T) {
	src := `event: response.created
data: {"type":"response.created","response":{"id":"resp_1","object":"response","model":"m","status":"in_progress"}}

event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"id":"rs_1","type":"reasoning","summary":[],"encrypted_content":null}}

event: response.reasoning_summary_part.added
data: {"type":"response.reasoning_summary_part.added","item_id":"rs_1","output_index":0,"content_index":0,"part":{"type":"summary_text","text":""}}

event: response.reasoning_summary_text.delta
data: {"type":"response.reasoning_summary_text.delta","item_id":"rs_1","output_index":0,"content_index":0,"delta":"Plan","snapshot":"Plan"}

event: response.reasoning_summary_text.delta
data: {"type":"response.reasoning_summary_text.delta","item_id":"rs_1","output_index":0,"content_index":0,"delta":"ning","snapshot":"Planning"}

event: response.reasoning_summary_part.done
data: {"type":"response.reasoning_summary_part.done","item_id":"rs_1","output_index":0,"content_index":0,"part":{"type":"summary_text","text":"Planning"}}

event: response.output_item.done
data: {"type":"response.output_item.done","output_index":0,"item":{"id":"rs_1","type":"reasoning","status":"completed","summary":[{"type":"summary_text","text":"Planning"}],"encrypted_content":null}}

event: response.output_item.added
data: {"type":"response.output_item.added","output_index":1,"item":{"id":"msg_1","type":"message","status":"in_progress","role":"assistant","content":[]}}

event: response.output_text.delta
data: {"type":"response.output_text.delta","item_id":"msg_1","output_index":1,"content_index":0,"delta":"hello","snapshot":"hello"}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","object":"response","status":"completed","model":"m","output":[{"id":"rs_1","type":"reasoning","summary":[{"type":"summary_text","text":"Planning"}],"encrypted_content":null},{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"hello","annotations":[]}]}],"usage":{"input_tokens":1,"output_tokens":2}}}
`
	evs := parseEvents(t, ProtoOpenAIResponses, src)
	out := encodeStreamBridge(t, ProtoAnthropicMessages, evs, true)
	for _, want := range []string{
		"event: content_block_start",
		`"type":"thinking"`,
		`"type":"thinking_delta"`,
		`"thinking":"Plan"`,
		`"thinking":"ning"`,
		"event: content_block_stop",
		`"type":"text_delta"`,
		`"text":"hello"`,
		"event: message_stop",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("messages stream missing %q\n%s", want, out)
		}
	}
}

// TestChatStreamReasoningContentToResponsesStream bridges the Chat
// reasoning_content convention into Responses reasoning events (bridge on).
func TestChatStreamReasoningContentToResponsesStream(t *testing.T) {
	src := `data: {"id":"x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"hmm"},"finish_reason":null}]}

data: {"id":"x","choices":[{"index":0,"delta":{"reasoning_content":" yes"},"finish_reason":null}]}

data: {"id":"x","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}

data: {"id":"x","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

data: [DONE]
`
	evs := parseEvents(t, ProtoOpenAIChat, src)
	out := encodeStreamBridge(t, ProtoOpenAIResponses, evs, true)
	for _, want := range []string{
		"event: response.reasoning_summary_part.added",
		`"snapshot":"hmm yes"`,
		`"delta":"hi"`,
		"event: response.completed",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("responses stream missing %q\n%s", want, out)
		}
	}
}

// TestParseChatStreamObjectArguments reproduces the tool-call retry loop:
// some gateways (especially Claude-translating ones) emit tool_calls arguments
// as a JSON object, not a string. They must be preserved as compact JSON so
// Codex receives non-empty arguments and can execute the tool.
func TestParseChatStreamObjectArguments(t *testing.T) {
	src := `data: {"id":"x","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}

data: {"id":"x","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"f","arguments":{"cmd":"ls -la"}}}]},"finish_reason":null}]}

data: {"id":"x","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}

data: [DONE]
`
	evs := parseEvents(t, ProtoOpenAIChat, src)
	var args []string
	for _, ev := range evs {
		if ev.kind == evToolArg {
			args = append(args, ev.toolArgs)
		}
	}
	if len(args) != 1 || args[0] != `{"cmd":"ls -la"}` {
		t.Fatalf("args = %v (want [{\"cmd\":\"ls -la\"}])", args)
	}
}

// TestChatStreamToolArgsToResponsesStream covers the encoder routing for a Chat
// upstream that streams id + name + arguments in a single first delta at index
// 0: the arguments must reach the Responses function_call, not be dropped
// because the arg delta carries only an index.
func TestChatStreamToolArgsToResponsesStream(t *testing.T) {
	src := `data: {"id":"x","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}

data: {"id":"x","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"exec_command","arguments":{"cmd":"ls"}}}]},"finish_reason":null}]}

data: {"id":"x","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}

data: [DONE]
`
	evs := parseEvents(t, ProtoOpenAIChat, src)
	out := encodeStream(t, ProtoOpenAIResponses, evs)
	if !strings.Contains(out, `"delta":"{\"cmd\":\"ls\"}"`) {
		t.Fatalf("function_call_arguments.delta missing object args\n%s", out)
	}
	if !strings.Contains(out, `"arguments":"{\"cmd\":\"ls\"}"`) {
		t.Fatalf("function_call_arguments.done missing object args\n%s", out)
	}
}

// TestResponsesStreamMultiToolToChatStream verifies argument routing from a
// Responses upstream with two parallel function calls: deltas carry only
// item_id, so each tool's arguments must reach the right Chat tool index.
func TestResponsesStreamMultiToolToChatStream(t *testing.T) {
	src := `event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"id":"fc_a","type":"function_call","call_id":"call_a","name":"tool_a","arguments":"","status":"in_progress"}}

event: response.output_item.added
data: {"type":"response.output_item.added","output_index":1,"item":{"id":"fc_b","type":"function_call","call_id":"call_b","name":"tool_b","arguments":"","status":"in_progress"}}

event: response.function_call_arguments.delta
data: {"type":"response.function_call_arguments.delta","item_id":"fc_a","output_index":0,"delta":"{\"x\":","snapshot":"{\"x\":"}

event: response.function_call_arguments.delta
data: {"type":"response.function_call_arguments.delta","item_id":"fc_b","output_index":1,"delta":"{\"y\":","snapshot":"{\"y\":"}

event: response.function_call_arguments.delta
data: {"type":"response.function_call_arguments.delta","item_id":"fc_a","output_index":0,"delta":"1}","snapshot":"{\"x\":1}"}

event: response.function_call_arguments.delta
data: {"type":"response.function_call_arguments.delta","item_id":"fc_b","output_index":1,"delta":"2}","snapshot":"{\"y\":2}"}

event: response.completed
data: {"type":"response.completed","response":{"id":"r1","object":"response","status":"completed","output":[]}}
`
	evs := parseEvents(t, ProtoOpenAIResponses, src)
	out := encodeStream(t, ProtoOpenAIChat, evs)
	for _, want := range []string{
		`"arguments":"{\"x\":"},"index":0`,
		`"arguments":"{\"y\":"},"index":1`,
		`"arguments":"1}"},"index":0`,
		`"arguments":"2}"},"index":1`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("chat stream missing %q\n%s", want, out)
		}
	}
}

// TestChatStreamMultiToolToMessagesStream verifies the messagesEncoder falls
// back to the upstream tool index when argument deltas carry no call id (the
// Chat upstream convention), so parallel tools keep their blocks separate.
func TestChatStreamMultiToolToMessagesStream(t *testing.T) {
	src := `data: {"id":"x","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}

data: {"id":"x","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"tool_a","arguments":""}}]},"finish_reason":null}]}

data: {"id":"x","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_b","type":"function","function":{"name":"tool_b","arguments":""}}]},"finish_reason":null}]}

data: {"id":"x","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"x\":"}}]},"finish_reason":null}]}

data: {"id":"x","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"function":{"arguments":"{\"y\":"}}]},"finish_reason":null}]}

data: {"id":"x","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1}"}}]},"finish_reason":null}]}

data: {"id":"x","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"function":{"arguments":"2}"}}]},"finish_reason":null}]}

data: {"id":"x","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}

data: [DONE]
`
	evs := parseEvents(t, ProtoOpenAIChat, src)
	out := encodeStream(t, ProtoAnthropicMessages, evs)
	var got []string
	if err := readSSE(strings.NewReader(out), func(_ string, data []byte) error {
		var e struct {
			Index int `json:"index"`
			Delta struct {
				Type        string `json:"type"`
				PartialJSON string `json:"partial_json"`
			} `json:"delta"`
		}
		if json.Unmarshal(data, &e) != nil {
			return nil
		}
		if e.Delta.Type == "input_json_delta" {
			got = append(got, fmt.Sprintf("%d|%s", e.Index, e.Delta.PartialJSON))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	want := []string{`0|{"x":`, `1|{"y":`, `0|1}`, `1|2}`}
	if len(got) != len(want) {
		t.Fatalf("input_json_delta events = %v (want %v)", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("input_json_delta[%d] = %q (want %q)", i, got[i], want[i])
		}
	}
}

// TestMessagesEncoderBlockLifecycle reproduces Claude Code's
// "Content block is not a input_json block": a response that streams reasoning
// + text + tool call + resumed text must close the text block before the tool
// block starts, and resume text in a fresh block — never a text_delta for a
// stale index while the tool block is current.
func TestMessagesEncoderBlockLifecycle(t *testing.T) {
	evs := []streamEv{
		{kind: evReasoning, reasoningText: "let me think"},
		{kind: evText, text: "我来查一下"},
		{kind: evToolBegin, toolID: "call_1", toolName: "WebSearch"},
		// Chat upstreams stream argument deltas with only an index (no call id);
		// the encoder must map upstream index 0 to its own tool block index.
		{kind: evToolArg, toolIdx: 0, toolArgs: `{"query":"上海天气"}`},
		{kind: evText, text: "正在查询"},
		{kind: evDone, finishReason: "tool_calls"},
	}
	out := encodeStream(t, ProtoAnthropicMessages, evs)

	// Parse the block sequence: start/stop by index, delta types by index.
	type blockEv struct {
		op     string // start | stop
		index  int
		kind   string
		dtype  string // delta type when op==delta
		dindex int
		dtext  string
	}
	var blocks []blockEv
	if err := readSSE(strings.NewReader(out), func(_ string, data []byte) error {
		var e struct {
			Type         string `json:"type"`
			Index        int    `json:"index"`
			ContentBlock struct {
				Type string `json:"type"`
			} `json:"content_block"`
			Delta struct {
				Type  string `json:"type"`
				Text  string `json:"text"`
				Think string `json:"thinking"`
			} `json:"delta"`
		}
		if json.Unmarshal(data, &e) != nil {
			return nil
		}
		switch e.Type {
		case "content_block_start":
			blocks = append(blocks, blockEv{op: "start", index: e.Index, kind: e.ContentBlock.Type})
		case "content_block_stop":
			blocks = append(blocks, blockEv{op: "stop", index: e.Index})
		case "content_block_delta":
			text := e.Delta.Text
			if text == "" {
				text = e.Delta.Think
			}
			blocks = append(blocks, blockEv{op: "delta", index: e.Index, dtype: e.Delta.Type, dtext: text})
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// Exact expected sequence: text closed before tool, tool closed before the
	// resumed text — never a text_delta targeting a stale index.
	var seq []string
	for _, b := range blocks {
		switch b.op {
		case "start":
			seq = append(seq, fmt.Sprintf("start:%d:%s", b.index, b.kind))
		case "stop":
			seq = append(seq, fmt.Sprintf("stop:%d", b.index))
		case "delta":
			seq = append(seq, fmt.Sprintf("delta:%d:%s", b.index, b.dtype))
		}
	}
	want := []string{
		"start:0:text",
		"delta:0:text_delta",
		"stop:0",
		"start:1:tool_use",
		"delta:1:input_json_delta",
		"stop:1",
		"start:2:text",
		"delta:2:text_delta",
		"stop:2",
	}
	if len(seq) != len(want) {
		t.Fatalf("block sequence = %v\nwant %v\n---\n%s", seq, want, out)
	}
	for i := range want {
		if seq[i] != want[i] {
			t.Fatalf("block[%d] = %q (want %q)\n---\n%s", i, seq[i], want[i], out)
		}
	}
}

// TestResponsesStreamObjectArguments: a Responses upstream that emits
// function_call_arguments.delta as a JSON object must have it preserved.
func TestResponsesStreamObjectArguments(t *testing.T) {
	src := `event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"id":"fc_a","type":"function_call","call_id":"call_a","name":"tool_a","arguments":"","status":"in_progress"}}

event: response.function_call_arguments.delta
data: {"type":"response.function_call_arguments.delta","item_id":"fc_a","output_index":0,"delta":{"cmd":"ls"},"snapshot":"{\"cmd\":\"ls\"}"}

event: response.completed
data: {"type":"response.completed","response":{"id":"r1","object":"response","status":"completed","output":[]}}
`
	evs := parseEvents(t, ProtoOpenAIResponses, src)
	out := encodeStream(t, ProtoOpenAIChat, evs)
	if !strings.Contains(out, `"arguments":"{\"cmd\":\"ls\"}"`) {
		t.Fatalf("object args not preserved to Chat\n%s", out)
	}
}
