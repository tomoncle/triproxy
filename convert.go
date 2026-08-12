package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// ---------------------------------------------------------------------------
// Request parsing: source protocol -> canonical Req
// ---------------------------------------------------------------------------

type chatMsg struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCallID string          `json:"tool_call_id"`
	ToolCalls  []struct {
		ID       string `json:"id"`
		Type     string `json:"type"`
		Function struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		} `json:"function"`
	} `json:"tool_calls"`
}

// ConvertOpts carries per-alias conversion behaviors. The zero value keeps the
// legacy behavior (reasoning/thinking content dropped), so existing callers and
// tests are unaffected.
type ConvertOpts struct {
	// ReasoningBridge converts OpenAI Responses reasoning items ↔ Anthropic
	// thinking blocks in both directions (request + response + streaming).
	ReasoningBridge bool
	// ForceStream forces "stream": true in the emitted request body even when
	// the client omitted it, so upstreams stream to the proxy.
	ForceStream bool
}

// streamFor resolves the "stream" field for an emitted request: ForceStream
// wins over the client body so upstreams stream when the proxy detected a
// streaming client (e.g. via the Accept header).
func streamFor(req *Req, opts *ConvertOpts) bool {
	if opts != nil && opts.ForceStream {
		return true
	}
	return req.Stream
}

func parseChatReq(raw json.RawMessage) (*Req, error) {
	var m struct {
		Model       string            `json:"model"`
		Messages    []chatMsg         `json:"messages"`
		MaxTokens   *int              `json:"max_tokens"`
		Temperature *float64          `json:"temperature"`
		TopP        *float64          `json:"top_p"`
		Stream      bool              `json:"stream"`
		Tools       []json.RawMessage `json:"tools"`
		ToolChoice  json.RawMessage   `json:"tool_choice"`
		// Some Chat providers accept reasoning_effort (DeepSeek/Qwen family
		// convention); parsed so it can be bridged to Messages thinking.
		ReasoningEffort string `json:"reasoning_effort"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("parse chat request: %w", err)
	}
	req := &Req{
		Model:           m.Model,
		MaxTokens:       m.MaxTokens,
		Temperature:     m.Temperature,
		TopP:            m.TopP,
		Stream:          m.Stream,
		ReasoningEffort: m.ReasoningEffort,
	}
	for _, c := range m.Messages {
		switch c.Role {
		case "system":
			blocks, err := contentBlocksFromJSON(c.Content)
			if err != nil {
				return nil, err
			}
			req.System = joinText(req.System, blocks)
		case "tool":
			blocks, err := contentBlocksFromJSON(c.Content)
			if err != nil {
				return nil, err
			}
			if len(blocks) == 0 {
				blocks = []ContentBlock{{Type: "tool_result", ToolUseID: c.ToolCallID, ToolResultText: ""}}
			}
			for _, b := range blocks {
				bb := b
				if bb.ToolUseID == "" {
					bb.ToolUseID = c.ToolCallID
				}
				req.Messages = append(req.Messages, Message{Role: "tool", Content: []ContentBlock{bb}})
			}
		case "user", "assistant":
			blocks, err := contentBlocksFromJSON(c.Content)
			if err != nil {
				return nil, err
			}
			for _, tc := range c.ToolCalls {
				blocks = append(blocks, ContentBlock{
					Type: "tool_call", ToolCallID: tc.ID, ToolName: tc.Function.Name,
					ToolArgsText: jsonCompactOrString(tc.Function.Arguments),
				})
			}
			req.Messages = append(req.Messages, Message{Role: c.Role, Content: blocks})
		}
	}
	var err error
	req.Tools, err = toolsFromChat(m.Tools)
	if err != nil {
		return nil, err
	}
	if len(m.ToolChoice) > 0 && string(m.ToolChoice) != "null" {
		req.ToolChoice, err = toolChoiceFrom(m.ToolChoice)
		if err != nil {
			return nil, err
		}
	}
	return req, nil
}

func toolsFromChat(raws []json.RawMessage) ([]Tool, error) {
	var out []Tool
	for _, r := range raws {
		tool, ok, err := parseTool(r)
		if err != nil {
			return nil, fmt.Errorf("tool: %w", err)
		}
		if ok {
			out = append(out, tool)
		}
	}
	return out, nil
}

func parseResponsesReq(raw json.RawMessage) (*Req, error) {
	var m struct {
		Model             string            `json:"model"`
		Input             json.RawMessage   `json:"input"`
		Instructions      string            `json:"instructions"`
		MaxOutputToks     *int              `json:"max_output_tokens"`
		Temperature       *float64          `json:"temperature"`
		TopP              *float64          `json:"top_p"`
		Stream            bool              `json:"stream"`
		Tools             []json.RawMessage `json:"tools"`
		ToolChoice        json.RawMessage   `json:"tool_choice"`
		MaxCompletionT    *int              `json:"max_completion_tokens"`
		Reasoning         json.RawMessage   `json:"reasoning"`
		ParallelToolCalls *bool             `json:"parallel_tool_calls"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("parse responses request: %w", err)
	}
	req := &Req{
		Model:             m.Model,
		System:            m.Instructions,
		Temperature:       m.Temperature,
		TopP:              m.TopP,
		Stream:            m.Stream,
		MaxTokens:         m.MaxOutputToks,
		ParallelToolCalls: m.ParallelToolCalls,
	}
	if len(m.Reasoning) > 0 {
		var r struct {
			Effort string `json:"effort"`
		}
		if json.Unmarshal(m.Reasoning, &r) == nil {
			req.ReasoningEffort = r.Effort
		}
	}
	if req.MaxTokens == nil {
		req.MaxTokens = m.MaxCompletionT
	}
	if len(m.Input) == 0 {
		req.Messages = nil
	} else if isJSONString(m.Input) {
		var s string
		_ = json.Unmarshal(m.Input, &s)
		if s != "" {
			req.Messages = []Message{{Role: "user", Content: []ContentBlock{{Type: "text", Text: s}}}}
		}
	} else {
		var items []json.RawMessage
		if err := json.Unmarshal(m.Input, &items); err != nil {
			return nil, fmt.Errorf("responses input must be a string or array: %w", err)
		}
		msgs, err := messagesFromResponsesInput(items)
		if err != nil {
			return nil, err
		}
		req.Messages = msgs
	}
	var err error
	req.Tools, err = toolsFromResponses(m.Tools)
	if err != nil {
		return nil, err
	}
	if len(m.ToolChoice) > 0 && string(m.ToolChoice) != "null" {
		req.ToolChoice, err = toolChoiceFrom(m.ToolChoice)
		if err != nil {
			return nil, err
		}
	}
	return req, nil
}

func messagesFromResponsesInput(items []json.RawMessage) ([]Message, error) {
	var out []Message
	// lastIdx tracks the position of the current assistant message carrying tool
	// call blocks so consecutive function_call items accumulate in order.
	for _, it := range items {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(it, &obj); err != nil {
			continue
		}
		var typ, role string
		_ = json.Unmarshal(obj["type"], &typ)
		_ = json.Unmarshal(obj["role"], &role)
		if typ == "" && obj["role"] != nil {
			typ = "message"
		}
		switch typ {
		case "message":
			if role == "" {
				role = "user"
			}
			blocks, err := contentBlocksFromJSON(obj["content"])
			if err != nil {
				return nil, err
			}
			out = append(out, Message{Role: role, Content: blocks})
		case "function_call":
			var callID, name string
			_ = json.Unmarshal(obj["call_id"], &callID)
			_ = json.Unmarshal(obj["name"], &name)
			args := jsonCompactOrString(obj["arguments"])
			blk := ContentBlock{Type: "tool_call", ToolCallID: callID, ToolName: name, ToolArgsText: args}
			if n := len(out); n > 0 && out[n-1].Role == "assistant" {
				out[n-1].Content = append(out[n-1].Content, blk)
			} else {
				out = append(out, Message{Role: "assistant", Content: []ContentBlock{blk}})
			}
		case "function_call_output":
			var callID string
			_ = json.Unmarshal(obj["call_id"], &callID)
			output := obj["output"]
			blk := ContentBlock{Type: "tool_result", ToolUseID: callID, ToolResultText: jsonCompactOrString(output)}
			if !isJSONString(output) && len(output) > 0 {
				var items []json.RawMessage
				if json.Unmarshal(output, &items) == nil {
					if parts, err := contentBlocksFromJSON(output); err == nil && len(parts) > 0 {
						blk.ToolResultBlocks = parts
					}
				}
			}
			out = append(out, Message{Role: "tool", Content: []ContentBlock{blk}})
		case "reasoning":
			// Preserve the full item so it can be round-tripped losslessly to
			// Messages thinking (bridge) or restored on a Responses target.
			blk := reasoningBlockFromItem(it)
			if n := len(out); n > 0 && out[n-1].Role == "assistant" {
				out[n-1].Content = append(out[n-1].Content, blk)
			} else {
				out = append(out, Message{Role: "assistant", Content: []ContentBlock{blk}})
			}
		default:
			// audio / other input parts: skip
		}
	}
	return out, nil
}

func toolsFromResponses(raws []json.RawMessage) ([]Tool, error) {
	var out []Tool
	for _, r := range raws {
		tool, ok, err := parseTool(r)
		if err != nil {
			return nil, fmt.Errorf("tool: %w", err)
		}
		if ok {
			out = append(out, tool)
		}
	}
	return out, nil
}

func parseMessagesReq(raw json.RawMessage) (*Req, error) {
	var m struct {
		Model    string `json:"model"`
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		System       json.RawMessage   `json:"system"`
		MaxTokens    *int              `json:"max_tokens"`
		Temperature  *float64          `json:"temperature"`
		TopP         *float64          `json:"top_p"`
		Stream       bool              `json:"stream"`
		Tools        []json.RawMessage `json:"tools"`
		ToolChoice   json.RawMessage   `json:"tool_choice"`
		StopSequence []string          `json:"stop_sequences"`
		Thinking     struct {
			Type   string `json:"type"`
			Budget int    `json:"budget_tokens"`
		} `json:"thinking"`
		DisableParallelToolUse *bool `json:"disable_parallel_tool_use"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("parse messages request: %w", err)
	}
	req := &Req{
		Model:       m.Model,
		MaxTokens:   m.MaxTokens,
		Temperature: m.Temperature,
		TopP:        m.TopP,
		Stream:      m.Stream,
	}
	if m.Thinking.Type == "enabled" || m.Thinking.Type == "adaptive" {
		enabled := true
		req.ThinkingEnabled = &enabled
		req.ThinkingBudget = m.Thinking.Budget
	}
	if m.DisableParallelToolUse != nil {
		v := !*m.DisableParallelToolUse
		req.ParallelToolCalls = &v
	}
	sys, err := systemFromMessages(m.System)
	if err != nil {
		return nil, err
	}
	req.System = sys
	for _, c := range m.Messages {
		blocks, err := contentBlocksFromJSON(c.Content)
		if err != nil {
			return nil, err
		}
		switch c.Role {
		case "assistant":
			req.Messages = append(req.Messages, Message{Role: "assistant", Content: blocks})
		case "user":
			// Split any tool_result blocks out into role "tool" messages so the
			// canonical form is consistent with Chat/Responses.
			req.Messages = append(req.Messages, splitUserToolResults(blocks)...)
		}
	}
	req.Tools, err = toolsFromMessages(m.Tools)
	if err != nil {
		return nil, err
	}
	if len(m.ToolChoice) > 0 && string(m.ToolChoice) != "null" {
		req.ToolChoice, err = toolChoiceFrom(m.ToolChoice)
		if err != nil {
			return nil, err
		}
	}
	return req, nil
}

func systemFromMessages(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, nil
	}
	var arr []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &arr); err != nil {
		return "", fmt.Errorf("system must be a string or array: %w", err)
	}
	var parts []string
	for _, b := range arr {
		parts = append(parts, b.Text)
	}
	return strings.Join(parts, "\n"), nil
}

func toolsFromMessages(raws []json.RawMessage) ([]Tool, error) {
	var out []Tool
	for _, r := range raws {
		tool, ok, err := parseTool(r)
		if err != nil {
			return nil, fmt.Errorf("tool: %w", err)
		}
		if ok {
			out = append(out, tool)
		}
	}
	return out, nil
}

// parseTool reads a tool declaration tolerating the three common shapes:
//   - Responses:  {"type":"function","name":"x","parameters":{...}}
//   - Chat:       {"type":"function","function":{"name":"x","parameters":{...}}}
//   - Messages:   {"name":"x","input_schema":{...}}
//
// Non-function tools (e.g. {"type":"web_search"}) carry no name and cannot be
// represented on protocols other than Responses; they are skipped (ok=false)
// rather than emitted as functions with an empty name, which upstreams reject
// with "tools[N].function.name invalid, should be set".
func parseTool(raw json.RawMessage) (Tool, bool, error) {
	var t struct {
		Type        string          `json:"type"`
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
		InputSchema json.RawMessage `json:"input_schema"`
		Function    struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Parameters  json.RawMessage `json:"parameters"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &t); err != nil {
		return Tool{}, false, err
	}
	name := t.Name
	if name == "" {
		name = t.Function.Name
	}
	desc := t.Description
	if desc == "" {
		desc = t.Function.Description
	}
	params := t.Parameters
	if len(params) == 0 {
		params = t.InputSchema
	}
	if len(params) == 0 {
		params = t.Function.Parameters
	}
	if name == "" {
		return Tool{}, false, nil
	}
	return Tool{Name: name, Description: desc, InputSchema: params}, true, nil
}

// splitUserToolResults walks the blocks of a user message and emits a sequence
// of messages so every tool_result becomes its own role "tool" message while
// preserving relative order.
func splitUserToolResults(blocks []ContentBlock) []Message {
	var out []Message
	var pending []ContentBlock
	flush := func() {
		if len(pending) > 0 {
			out = append(out, Message{Role: "user", Content: pending})
			pending = nil
		}
	}
	for _, b := range blocks {
		if b.Type == "tool_result" {
			flush()
			out = append(out, Message{Role: "tool", Content: []ContentBlock{b}})
		} else {
			pending = append(pending, b)
		}
	}
	flush()
	return out
}

// ---------------------------------------------------------------------------
// Request emission: canonical Req -> target protocol
// ---------------------------------------------------------------------------

func emitChatReq(req *Req, opts *ConvertOpts) ([]byte, error) {
	body := map[string]any{}
	if req.Model != "" {
		body["model"] = req.Model
	}
	msgs := make([]any, 0, len(req.Messages)+1)
	if req.System != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": req.System})
	}
	for _, msg := range req.Messages {
		switch msg.Role {
		case "tool":
			for _, b := range msg.Content {
				msgs = append(msgs, map[string]any{
					"role":         "tool",
					"tool_call_id": b.ToolUseID,
					"content":      b.ToolResultText,
				})
			}
		case "user":
			content, err := contentBlocksToChat(msg.Content)
			if err != nil {
				return nil, err
			}
			msgs = append(msgs, map[string]any{"role": "user", "content": content})
		case "assistant":
			entry := map[string]any{"role": "assistant"}
			content, err := contentBlocksToChat(textAndImageBlocks(msg.Content))
			if err != nil {
				return nil, err
			}
			entry["content"] = content
			var rc []string
			var tcs []any
			for _, b := range msg.Content {
				switch b.Type {
				case "reasoning":
					// Chat providers (DeepSeek/Qwen and Claude-translating
					// gateways) require the previous response's reasoning_content
					// to be echoed back on the next turn, or they reject with 400
					// "reasoning_content ... must be passed back to the API".
					rc = append(rc, b.ReasoningSummary)
				case "tool_call":
					tcs = append(tcs, map[string]any{
						"id":   b.ToolCallID,
						"type": "function",
						"function": map[string]any{
							"name":      b.ToolName,
							"arguments": b.ToolArgsText,
						},
					})
				}
			}
			if len(rc) > 0 {
				entry["reasoning_content"] = strings.Join(rc, "")
			}
			if len(tcs) > 0 {
				entry["tool_calls"] = tcs
			}
			msgs = append(msgs, entry)
		}
	}
	body["messages"] = msgs
	if req.MaxTokens != nil {
		body["max_tokens"] = *req.MaxTokens
	}
	if req.Temperature != nil {
		body["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		body["top_p"] = *req.TopP
	}
	body["stream"] = streamFor(req, opts)
	if len(req.Tools) > 0 {
		tools := make([]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			tools = append(tools, map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        t.Name,
					"description": t.Description,
					"parameters":  toolSchema(t.InputSchema),
				},
			})
		}
		body["tools"] = tools
	}
	if req.ToolChoice != nil {
		body["tool_choice"] = toolChoiceToChat(req.ToolChoice)
	}
	return json.Marshal(body)
}

func emitResponsesReq(req *Req, opts *ConvertOpts) ([]byte, error) {
	body := map[string]any{}
	if req.Model != "" {
		body["model"] = req.Model
	}
	input := make([]any, 0, len(req.Messages))
	for _, msg := range req.Messages {
		switch msg.Role {
		case "tool":
			for _, b := range msg.Content {
				input = append(input, map[string]any{
					"type":    "function_call_output",
					"call_id": b.ToolUseID,
					"output":  toolResultContentToResponses(b),
				})
			}
		case "assistant":
			var textParts []string
			for _, b := range msg.Content {
				switch b.Type {
				case "text":
					textParts = append(textParts, b.Text)
				case "tool_call":
					if len(textParts) > 0 {
						input = append(input, messageItem("assistant", outputTextPart(strings.Join(textParts, "\n"))))
						textParts = nil
					}
					input = append(input, map[string]any{
						"type":      "function_call",
						"call_id":   b.ToolCallID,
						"name":      b.ToolName,
						"arguments": b.ToolArgsText,
					})
				case "reasoning":
					if len(textParts) > 0 {
						input = append(input, messageItem("assistant", outputTextPart(strings.Join(textParts, "\n"))))
						textParts = nil
					}
					if opts != nil && opts.ReasoningBridge {
						if item, ok := responsesReasoningItem(b); ok {
							input = append(input, item)
						}
					}
				}
			}
			if len(textParts) > 0 {
				input = append(input, messageItem("assistant", outputTextPart(strings.Join(textParts, "\n"))))
			}
		case "user":
			content := make([]any, 0, len(msg.Content))
			for _, b := range msg.Content {
				switch b.Type {
				case "text":
					content = append(content, map[string]any{"type": "input_text", "text": b.Text})
				case "image":
					content = append(content, map[string]any{"type": "input_image", "image_url": b.ImageURL})
				case "file":
					content = append(content, fileBlockToResponses(b))
				}
			}
			input = append(input, messageItem("user", content))
		}
	}
	body["input"] = input
	if req.System != "" {
		body["instructions"] = req.System
	}
	if req.MaxTokens != nil {
		body["max_output_tokens"] = *req.MaxTokens
	}
	if req.Temperature != nil {
		body["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		body["top_p"] = *req.TopP
	}
	// Bridge the client's Anthropic thinking request into Responses reasoning
	// effort (only when the bridge is enabled).
	if opts != nil && opts.ReasoningBridge && req.ThinkingEnabled != nil && *req.ThinkingEnabled {
		body["reasoning"] = map[string]any{"effort": effortFromThinking(req.ThinkingBudget, req.ThinkingBudget == 0)}
	}
	body["stream"] = streamFor(req, opts)
	if len(req.Tools) > 0 {
		tools := make([]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			tools = append(tools, map[string]any{
				"type":        "function",
				"name":        t.Name,
				"description": t.Description,
				"parameters":  toolSchema(t.InputSchema),
			})
		}
		body["tools"] = tools
	}
	if req.ToolChoice != nil {
		body["tool_choice"] = toolChoiceToResponses(req.ToolChoice)
	}
	if req.ParallelToolCalls != nil {
		body["parallel_tool_calls"] = *req.ParallelToolCalls
	}
	return json.Marshal(body)
}

func emitMessagesReq(req *Req, opts *ConvertOpts) ([]byte, error) {
	body := map[string]any{}
	if req.Model != "" {
		body["model"] = req.Model
	}
	if req.System != "" {
		body["system"] = req.System
	}
	// Bridge the client's reasoning request into Anthropic extended thinking
	// (the only way Messages can express it). Requires the thinking beta header
	// to be attached by the proxy, and upstream support for extended thinking.
	if req.ThinkingEnabled != nil && *req.ThinkingEnabled {
		if req.ThinkingBudget > 0 {
			body["thinking"] = map[string]any{"type": "enabled", "budget_tokens": req.ThinkingBudget}
		} else {
			body["thinking"] = map[string]any{"type": "adaptive"}
		}
	} else if opts != nil && opts.ReasoningBridge && req.ReasoningEffort != "" {
		if budget, adaptive := reasoningBudgetForEffort(req.ReasoningEffort); adaptive {
			body["thinking"] = map[string]any{"type": "adaptive"}
		} else if budget > 0 {
			body["thinking"] = map[string]any{"type": "enabled", "budget_tokens": budget}
		}
	}
	msgs := make([]any, 0, len(req.Messages))
	for _, msg := range req.Messages {
		switch msg.Role {
		case "tool":
			for _, b := range msg.Content {
				tr := map[string]any{
					"type":        "tool_result",
					"tool_use_id": b.ToolUseID,
					"content":     toolResultContentToMessages(b),
				}
				if b.ToolResultIsError {
					tr["is_error"] = true
				}
				msgs = append(msgs, map[string]any{
					"role":    "user",
					"content": []any{tr},
				})
			}
		case "user":
			msgs = append(msgs, map[string]any{"role": "user", "content": messagesContent(msg.Content, opts)})
		case "assistant":
			msgs = append(msgs, map[string]any{"role": "assistant", "content": messagesContent(msg.Content, opts)})
		}
	}
	body["messages"] = mergeAdjacent(msgs)
	maxTokens := DefaultMaxTokens
	if req.MaxTokens != nil {
		maxTokens = *req.MaxTokens
	}
	body["max_tokens"] = maxTokens
	if req.Temperature != nil {
		body["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		body["top_p"] = *req.TopP
	}
	body["stream"] = streamFor(req, opts)
	if len(req.Tools) > 0 {
		tools := make([]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			tools = append(tools, map[string]any{
				"name":         t.Name,
				"description":  t.Description,
				"input_schema": toolSchema(t.InputSchema),
			})
		}
		body["tools"] = tools
	}
	if req.ToolChoice != nil {
		body["tool_choice"] = toolChoiceToMessages(req.ToolChoice)
	}
	if req.ParallelToolCalls != nil {
		body["disable_parallel_tool_use"] = !*req.ParallelToolCalls
	}
	return json.Marshal(body)
}

func textPart(text string) []any {
	return []any{map[string]any{"type": "input_text", "text": text}}
}

func outputTextPart(text string) []any {
	return []any{map[string]any{"type": "output_text", "text": text}}
}

func messageItem(role string, content any) map[string]any {
	return map[string]any{"type": "message", "role": role, "content": content}
}

// messagesContent renders blocks as an Anthropic content field: a plain string
// for a single text block, otherwise an array of part objects. Reasoning blocks
// become thinking blocks only when the reasoning bridge is enabled.
func messagesContent(blocks []ContentBlock, opts *ConvertOpts) any {
	if len(blocks) == 1 && blocks[0].Type == "text" {
		return blocks[0].Text
	}
	var out []any
	for _, b := range blocks {
		switch b.Type {
		case "text":
			out = append(out, map[string]any{"type": "text", "text": b.Text})
		case "reasoning":
			if opts != nil && opts.ReasoningBridge {
				if blk, ok := messagesThinkingBlock(b); ok {
					out = append(out, blk)
				}
			}
		case "tool_call":
			out = append(out, map[string]any{
				"type":  "tool_use",
				"id":    b.ToolCallID,
				"name":  b.ToolName,
				"input": argumentsObject(b.ToolArgsText),
			})
		case "image":
			media, data := splitDataURL(b.ImageURL)
			if data != "" {
				out = append(out, map[string]any{
					"type": "image",
					"source": map[string]any{
						"type":       "base64",
						"media_type": media,
						"data":       data,
					},
				})
			} else if b.ImageData != "" {
				out = append(out, map[string]any{
					"type": "image",
					"source": map[string]any{
						"type":       "base64",
						"media_type": b.ImageMediaType,
						"data":       b.ImageData,
					},
				})
			}
		case "file":
			out = append(out, fileBlockToMessages(b))
		}
	}
	if len(out) == 0 {
		return ""
	}
	return out
}

// fileBlockToMessages renders a canonical file block as an Anthropic document.
func fileBlockToMessages(b ContentBlock) map[string]any {
	source := map[string]any{}
	if b.FileURL != "" {
		source["type"] = "url"
		source["url"] = b.FileURL
	} else if b.FileData != "" {
		source["type"] = "base64"
		source["media_type"] = b.FileMediaType
		source["data"] = b.FileData
	}
	blk := map[string]any{"type": "document", "source": source}
	if b.FileFilename != "" {
		blk["title"] = b.FileFilename
	}
	return blk
}

// fileBlockToResponses renders a canonical file block as a Responses input_file
// content part.
func fileBlockToResponses(b ContentBlock) map[string]any {
	item := map[string]any{"type": "input_file"}
	if b.FileURL != "" {
		item["file_url"] = b.FileURL
	} else if b.FileData != "" {
		item["file_data"] = dataURL(b.FileMediaType, b.FileData)
	}
	if b.FileFilename != "" {
		item["filename"] = b.FileFilename
	}
	return item
}

// toolResultContentToMessages renders a tool_result's content field: a plain
// string for the legacy form, otherwise an array of text/image/document parts
// so media survives the round trip.
func toolResultContentToMessages(b ContentBlock) any {
	if len(b.ToolResultBlocks) == 0 {
		return b.ToolResultText
	}
	var out []any
	for _, part := range b.ToolResultBlocks {
		switch part.Type {
		case "text":
			out = append(out, map[string]any{"type": "text", "text": part.Text})
		case "image":
			media, data := splitDataURL(part.ImageURL)
			if data == "" {
				media, data = part.ImageMediaType, part.ImageData
			}
			out = append(out, map[string]any{"type": "image", "source": map[string]any{
				"type": "base64", "media_type": media, "data": data,
			}})
		case "file":
			out = append(out, fileBlockToMessages(part))
		}
	}
	if len(out) == 0 {
		return b.ToolResultText
	}
	return out
}

// toolResultContentToResponses renders a function_call_output's output field: a
// plain string for the legacy form, otherwise an array of input_text /
// input_image / input_file parts.
func toolResultContentToResponses(b ContentBlock) any {
	if len(b.ToolResultBlocks) == 0 {
		return b.ToolResultText
	}
	var out []any
	for _, part := range b.ToolResultBlocks {
		switch part.Type {
		case "text":
			out = append(out, map[string]any{"type": "input_text", "text": part.Text})
		case "image":
			url := part.ImageURL
			if url == "" {
				url = dataURL(part.ImageMediaType, part.ImageData)
			}
			out = append(out, map[string]any{"type": "input_image", "image_url": url})
		case "file":
			out = append(out, fileBlockToResponses(part))
		}
	}
	if len(out) == 0 {
		return b.ToolResultText
	}
	return out
}

// mergeAdjacent collapses consecutive messages with the same role, which the
// Anthropic API rejects.
func mergeAdjacent(msgs []any) []any {
	if len(msgs) < 2 {
		return msgs
	}
	out := make([]any, 0, len(msgs))
	out = append(out, msgs[0])
	for _, m := range msgs[1:] {
		last := out[len(out)-1].(map[string]any)
		cur := m.(map[string]any)
		if last["role"] == cur["role"] {
			last["content"] = appendContent(last["content"], cur["content"])
			continue
		}
		out = append(out, m)
	}
	return out
}

func appendContent(a, b any) any {
	as, aIsStr := a.(string)
	bs, bIsStr := b.(string)
	if aIsStr && bIsStr {
		return as + bs
	}
	norm := func(v any) []any {
		if s, ok := v.(string); ok {
			return []any{map[string]any{"type": "text", "text": s}}
		}
		return v.([]any)
	}
	return append(norm(a), norm(b)...)
}

// ---------------------------------------------------------------------------
// Response parsing: source protocol -> canonical Resp
// ---------------------------------------------------------------------------

func parseChatResp(raw json.RawMessage) (*Resp, error) {
	var m struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		Model   string `json:"model"`
		Choices []struct {
			Index   int `json:"index"`
			Message struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
				// reasoning_content is a non-standard Chat convention used by
				// DeepSeek/Qwen-family providers; bridged into reasoning items.
				ReasoningContent string `json:"reasoning_content"`
				ToolCalls        []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string          `json:"name"`
						Arguments json.RawMessage `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason json.RawMessage `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
			TotalTokens      int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("parse chat response: %w", err)
	}
	r := &Resp{ID: m.ID, Object: m.Object, Model: m.Model, CreatedAt: m.Created}
	if len(m.Choices) > 0 {
		ch := m.Choices[0]
		// reasoning_content precedes the answer in the DeepSeek/Qwen
		// convention; place it before the content blocks.
		var blocks []ContentBlock
		if ch.Message.ReasoningContent != "" {
			blocks = append(blocks, ContentBlock{
				Type: "reasoning", ReasoningSummary: ch.Message.ReasoningContent,
			})
		}
		parsed, err := contentBlocksFromJSON(ch.Message.Content)
		if err != nil {
			return nil, err
		}
		blocks = append(blocks, parsed...)
		for _, tc := range ch.Message.ToolCalls {
			blocks = append(blocks, ContentBlock{
				Type: "tool_call", ToolCallID: tc.ID, ToolName: tc.Function.Name,
				ToolArgsText: jsonCompactOrString(tc.Function.Arguments),
			})
		}
		r.Messages = []Message{{Role: "assistant", Content: blocks}}
		r.FinishReason = jsonStringOrEmpty(ch.FinishReason)
	}
	if m.Usage.TotalTokens > 0 || m.Usage.PromptTokens > 0 || m.Usage.CompletionTokens > 0 {
		raw, _ := json.Marshal(m.Usage)
		r.Usage = &Usage{
			InputTokens: m.Usage.PromptTokens, OutputTokens: m.Usage.CompletionTokens,
			TotalTokens: m.Usage.TotalTokens, Raw: raw,
		}
	}
	return r, nil
}

func parseResponsesResp(raw json.RawMessage) (*Resp, error) {
	var m struct {
		ID                string            `json:"id"`
		Object            string            `json:"object"`
		CreatedAt         int64             `json:"created_at"`
		Status            string            `json:"status"`
		Model             string            `json:"model"`
		Output            []json.RawMessage `json:"output"`
		Usage             json.RawMessage   `json:"usage"`
		IncompleteDetails struct {
			Reason string `json:"reason"`
		} `json:"incomplete_details"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("parse responses response: %w", err)
	}
	r := &Resp{ID: m.ID, Object: m.Object, Model: m.Model, CreatedAt: m.CreatedAt, Status: m.Status}
	var blocks []ContentBlock
	for _, o := range m.Output {
		var item struct {
			Type      string          `json:"type"`
			Role      string          `json:"role"`
			Content   json.RawMessage `json:"content"`
			CallID    string          `json:"call_id"`
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(o, &item); err != nil {
			continue
		}
		switch item.Type {
		case "message":
			b, err := contentBlocksFromJSON(item.Content)
			if err != nil {
				return nil, err
			}
			blocks = append(blocks, b...)
		case "function_call":
			blocks = append(blocks, ContentBlock{
				Type: "tool_call", ToolCallID: item.CallID, ToolName: item.Name,
				ToolArgsText: jsonCompactOrString(item.Arguments),
			})
		case "reasoning":
			blocks = append(blocks, reasoningBlockFromItem(o))
		}
	}
	r.Messages = []Message{{Role: "assistant", Content: blocks}}
	r.IncompleteReason = m.IncompleteDetails.Reason
	switch m.Status {
	case "completed":
		r.FinishReason = "stop"
	case "incomplete":
		r.FinishReason = "length"
	}
	if len(m.Usage) > 0 {
		r.Usage = usageFromAny(m.Usage)
	}
	return r, nil
}

func parseMessagesResp(raw json.RawMessage) (*Resp, error) {
	var m struct {
		ID         string          `json:"id"`
		Type       string          `json:"type"`
		Role       string          `json:"role"`
		Model      string          `json:"model"`
		Content    json.RawMessage `json:"content"`
		StopReason json.RawMessage `json:"stop_reason"`
		Usage      struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("parse messages response: %w", err)
	}
	blocks, err := contentBlocksFromJSON(m.Content)
	if err != nil {
		return nil, err
	}
	r := &Resp{ID: m.ID, Object: m.Type, Model: m.Model, Messages: []Message{{Role: "assistant", Content: blocks}}}
	stopReason := jsonStringOrEmpty(m.StopReason)
	r.FinishReason = finishReasonFromAnthropic(stopReason)
	switch stopReason {
	case "max_tokens":
		r.IncompleteReason = "max_output_tokens"
	case "refusal":
		r.IncompleteReason = "content_filter"
	}
	if m.Usage.InputTokens > 0 || m.Usage.OutputTokens > 0 {
		raw, _ := json.Marshal(m.Usage)
		r.Usage = &Usage{
			InputTokens: m.Usage.InputTokens, OutputTokens: m.Usage.OutputTokens,
			TotalTokens: m.Usage.InputTokens + m.Usage.OutputTokens, Raw: raw,
		}
	}
	return r, nil
}

// ---------------------------------------------------------------------------
// Response emission: canonical Resp -> target protocol
// ---------------------------------------------------------------------------

func emitChatResp(r *Resp) ([]byte, error) {
	msg := map[string]any{"role": "assistant"}
	var textParts []string
	var tcs []any
	for _, b := range firstMessage(r).Content {
		switch b.Type {
		case "text":
			textParts = append(textParts, b.Text)
		case "tool_call":
			tcs = append(tcs, map[string]any{
				"id":   b.ToolCallID,
				"type": "function",
				"function": map[string]any{
					"name":      b.ToolName,
					"arguments": b.ToolArgsText,
				},
			})
		}
	}
	msg["content"] = strings.Join(textParts, "\n")
	if len(tcs) > 0 {
		msg["tool_calls"] = tcs
	}
	body := map[string]any{
		"id":      ensurePrefix(r.ID, "chatcmpl-"),
		"object":  "chat.completion",
		"created": createdOrNow(r.CreatedAt),
		"model":   r.Model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       msg,
			"finish_reason": finishReasonToChat(r.FinishReason),
			"logprobs":      nil,
		}},
	}
	if u := usageToChat(r.Usage); u != nil {
		body["usage"] = u
	}
	return json.Marshal(body)
}

func emitResponsesResp(r *Resp, opts *ConvertOpts) ([]byte, error) {
	status := r.Status
	if status == "" {
		status = "completed"
	}
	// Anthropic max_tokens has no direct Responses counterpart; surface it as
	// an incomplete response so Codex does not treat a truncated answer as a
	// successful completion.
	if status == "completed" && r.FinishReason == "length" {
		status = "incomplete"
	}
	output := make([]any, 0, 4)
	var msgID string
	var content []any
	flushMsg := func() {
		if msgID == "" {
			return
		}
		output = append(output, map[string]any{
			"id":      msgID,
			"type":    "message",
			"status":  "completed",
			"role":    "assistant",
			"content": content,
		})
		msgID = ""
		content = nil
	}
	for _, b := range firstMessage(r).Content {
		switch b.Type {
		case "text":
			if msgID == "" {
				msgID = genID("msg")
			}
			content = append(content, map[string]any{
				"type": "output_text", "text": b.Text, "annotations": []any{},
			})
		case "tool_call":
			flushMsg()
			output = append(output, map[string]any{
				"id":        genID("fc"),
				"type":      "function_call",
				"status":    "completed",
				"call_id":   b.ToolCallID,
				"name":      b.ToolName,
				"arguments": b.ToolArgsText,
			})
		case "reasoning":
			flushMsg()
			if opts != nil && opts.ReasoningBridge {
				if item, ok := responsesReasoningItem(b); ok {
					output = append(output, item)
				}
			}
		}
	}
	flushMsg()
	body := map[string]any{
		"id":         ensurePrefix(r.ID, "resp_"),
		"object":     "response",
		"created_at": createdOrNow(r.CreatedAt),
		"status":     status,
		"model":      r.Model,
		"output":     output,
		"error":      nil,
	}
	if status == "incomplete" {
		reason := r.IncompleteReason
		if reason == "" {
			reason = "max_output_tokens"
		}
		body["incomplete_details"] = map[string]any{"reason": reason}
	}
	if u := usageToResponses(r.Usage); u != nil {
		body["usage"] = u
	}
	return json.Marshal(body)
}

func emitMessagesResp(r *Resp, opts *ConvertOpts) ([]byte, error) {
	content := make([]any, 0, 4)
	for _, b := range firstMessage(r).Content {
		switch b.Type {
		case "text":
			content = append(content, map[string]any{"type": "text", "text": b.Text})
		case "reasoning":
			if opts != nil && opts.ReasoningBridge {
				if blk, ok := messagesThinkingBlock(b); ok {
					content = append(content, blk)
				}
			}
		case "tool_call":
			content = append(content, map[string]any{
				"type":  "tool_use",
				"id":    b.ToolCallID,
				"name":  b.ToolName,
				"input": argumentsObject(b.ToolArgsText),
			})
		}
	}
	body := map[string]any{
		"id":            ensurePrefix(r.ID, "msg_"),
		"type":          "message",
		"role":          "assistant",
		"model":         r.Model,
		"content":       content,
		"stop_reason":   finishReasonToAnthropic(r.FinishReason),
		"stop_sequence": nil,
	}
	if u := usageToMessages(r.Usage); u != nil {
		body["usage"] = u
	}
	return json.Marshal(body)
}

// ---------------------------------------------------------------------------
// Dispatch
// ---------------------------------------------------------------------------

func convertRequestOpts(from, to string, body []byte, opts *ConvertOpts) ([]byte, error) {
	var req *Req
	var err error
	switch from {
	case ProtoOpenAIChat:
		req, err = parseChatReq(body)
	case ProtoOpenAIResponses:
		req, err = parseResponsesReq(body)
	case ProtoAnthropicMessages:
		req, err = parseMessagesReq(body)
	default:
		return nil, fmt.Errorf("unsupported client protocol %q", from)
	}
	if err != nil {
		return nil, err
	}
	switch to {
	case ProtoOpenAIChat:
		return emitChatReq(req, opts)
	case ProtoOpenAIResponses:
		return emitResponsesReq(req, opts)
	case ProtoAnthropicMessages:
		return emitMessagesReq(req, opts)
	default:
		return nil, fmt.Errorf("unsupported upstream protocol %q", to)
	}
}

// convertRequest keeps the legacy signature (no bridge) so existing callers and
// tests are unchanged.
func convertRequest(from, to string, body []byte) ([]byte, error) {
	return convertRequestOpts(from, to, body, nil)
}

func convertResponseOpts(from, to string, body []byte, opts *ConvertOpts) ([]byte, error) {
	var resp *Resp
	var err error
	switch from {
	case ProtoOpenAIChat:
		resp, err = parseChatResp(body)
	case ProtoOpenAIResponses:
		resp, err = parseResponsesResp(body)
	case ProtoAnthropicMessages:
		resp, err = parseMessagesResp(body)
	default:
		return nil, fmt.Errorf("unsupported upstream protocol %q", from)
	}
	if err != nil {
		return nil, err
	}
	switch to {
	case ProtoOpenAIChat:
		return emitChatResp(resp)
	case ProtoOpenAIResponses:
		return emitResponsesResp(resp, opts)
	case ProtoAnthropicMessages:
		return emitMessagesResp(resp, opts)
	default:
		return nil, fmt.Errorf("unsupported client protocol %q", to)
	}
}

// convertResponse keeps the legacy signature (no bridge).
func convertResponse(from, to string, body []byte) ([]byte, error) {
	return convertResponseOpts(from, to, body, nil)
}

// ---------------------------------------------------------------------------
// Small helpers
// ---------------------------------------------------------------------------

func firstMessage(r *Resp) Message {
	if len(r.Messages) > 0 {
		return r.Messages[0]
	}
	return Message{}
}

func textAndImageBlocks(blocks []ContentBlock) []ContentBlock {
	var out []ContentBlock
	for _, b := range blocks {
		if b.Type == "text" || b.Type == "image" {
			out = append(out, b)
		}
	}
	return out
}

func joinText(prev string, blocks []ContentBlock) string {
	var parts []string
	if prev != "" {
		parts = append(parts, prev)
	}
	for _, b := range blocks {
		if b.Type == "text" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// toolSchema returns a valid JSON Schema object for a tool's parameters.
// Empty or type-less schemas — e.g. Codex's free-form tools that send
// "parameters": {} — are normalized to {"type":"object"}, because Chat and
// Messages providers reject tool schemas whose type is null.
func toolSchema(raw json.RawMessage) any {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return map[string]any{"type": "object"}
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) == nil {
		if _, ok := m["type"]; !ok {
			m["type"] = "object"
			return m
		}
	}
	return raw
}

func argumentsObject(args string) any {
	if args == "" {
		return map[string]any{}
	}
	var v any
	if json.Unmarshal([]byte(args), &v) == nil {
		return v
	}
	// Arguments that are not valid JSON (e.g. plain text) get wrapped so the
	// output stays a valid JSON object.
	return map[string]any{"_": args}
}

func isJSONString(raw json.RawMessage) bool {
	var s string
	return json.Unmarshal(raw, &s) == nil
}

func jsonStringOrEmpty(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return ""
}

// jsonCompactOrString returns the plain string value of a JSON string, or the
// compact JSON text of an object/array value. Responses function_call arguments
// and function_call_output may carry either form; targets that need a string
// (Anthropic tool_result.content) must not silently drop the structured form.
func jsonCompactOrString(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err == nil {
		return buf.String()
	}
	return string(raw)
}

func toolChoiceFrom(raw json.RawMessage) (*ToolChoice, error) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return &ToolChoice{Mode: s}, nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("bad tool_choice: %w", err)
	}
	var typ string
	_ = json.Unmarshal(obj["type"], &typ)
	var name string
	if obj["name"] != nil {
		_ = json.Unmarshal(obj["name"], &name)
	}
	// Chat style: {"type":"function","function":{"name":...}}
	if obj["function"] != nil {
		var f struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(obj["function"], &f)
		if f.Name != "" {
			name = f.Name
		}
	}
	switch typ {
	case "function":
		return &ToolChoice{Mode: "function", Name: name}, nil
	case "tool":
		return &ToolChoice{Mode: "tool", Name: name}, nil
	default:
		return &ToolChoice{Mode: typ}, nil
	}
}

func toolChoiceToChat(tc *ToolChoice) any {
	switch tc.Mode {
	case "none":
		return "none"
	case "required", "any":
		return "required"
	case "function", "tool":
		return map[string]any{"type": "function", "function": map[string]any{"name": tc.Name}}
	default:
		return "auto"
	}
}

func toolChoiceToResponses(tc *ToolChoice) any {
	switch tc.Mode {
	case "none":
		return "none"
	case "required", "any":
		return "required"
	case "function", "tool":
		return map[string]any{"type": "function", "name": tc.Name}
	default:
		return "auto"
	}
}

func toolChoiceToMessages(tc *ToolChoice) any {
	switch tc.Mode {
	case "any", "required":
		return "any"
	case "tool", "function":
		return map[string]any{"type": "tool", "name": tc.Name}
	default: // auto, none
		return "auto"
	}
}

func usageToChat(u *Usage) any {
	if u == nil {
		return nil
	}
	return map[string]any{
		"prompt_tokens":     u.InputTokens,
		"completion_tokens": u.OutputTokens,
		"total_tokens":      total(u),
	}
}

func usageToResponses(u *Usage) any {
	if u == nil {
		return nil
	}
	return map[string]any{
		"input_tokens":  u.InputTokens,
		"output_tokens": u.OutputTokens,
		"total_tokens":  total(u),
	}
}

func usageToMessages(u *Usage) any {
	if u == nil {
		return nil
	}
	return map[string]any{
		"input_tokens":  u.InputTokens,
		"output_tokens": u.OutputTokens,
	}
}

func total(u *Usage) int64 {
	if u.TotalTokens > 0 {
		return u.TotalTokens
	}
	return u.InputTokens + u.OutputTokens
}

// usageFromAny parses usage in either Chat or Responses/Messages shape.
func usageFromAny(raw json.RawMessage) *Usage {
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil {
		return nil
	}
	u := &Usage{Raw: raw}
	num := func(key string) int64 {
		if obj[key] == nil {
			return 0
		}
		var n int64
		_ = json.Unmarshal(obj[key], &n)
		return n
	}
	u.InputTokens = num("input_tokens") + num("prompt_tokens")
	u.OutputTokens = num("output_tokens") + num("completion_tokens")
	u.TotalTokens = num("total_tokens")
	return u
}

func finishReasonToChat(fr string) any {
	if fr == "" {
		return nil
	}
	switch fr {
	case "length":
		return "length"
	case "tool_calls":
		return "tool_calls"
	case "content_filter":
		return "content_filter"
	default:
		return "stop"
	}
}

func finishReasonToAnthropic(fr string) any {
	if fr == "" {
		return "end_turn"
	}
	switch fr {
	case "length":
		return "max_tokens"
	case "tool_calls":
		return "tool_use"
	case "content_filter":
		return "refusal"
	default:
		return "end_turn"
	}
}

func finishReasonFromAnthropic(stopReason string) string {
	switch stopReason {
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	case "refusal":
		return "content_filter"
	default:
		return "stop"
	}
}

func createdOrNow(created int64) int64 {
	if created > 0 {
		return created
	}
	return timeNow()
}

func ensurePrefix(id, prefix string) string {
	if id == "" {
		return genID(strings.TrimSuffix(prefix, "_"))
	}
	if strings.HasPrefix(id, prefix) {
		return id
	}
	return prefix + id
}

func genID(prefix string) string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%s_%d", prefix, timeNow())
	}
	return prefix + "_" + hex.EncodeToString(b[:])
}

func splitDataURL(u string) (media, data string) {
	const p = "data:"
	if !strings.HasPrefix(u, p) {
		return "", ""
	}
	rest := strings.TrimPrefix(u, p)
	comma := strings.Index(rest, ",")
	if comma < 0 {
		return "", ""
	}
	meta := strings.SplitN(rest[:comma], ";", 2)
	media = meta[0]
	if len(meta) == 2 && meta[1] == "base64" {
		data = rest[comma+1:]
	}
	return media, data
}
