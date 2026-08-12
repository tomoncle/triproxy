package main

import (
	"bufio"
	"encoding/json"
	"io"
	"sort"
	"strings"
)

// streamEvKind enumerates the canonical streaming events that all three
// protocols are mapped onto and from.
type streamEvKind int

const (
	evCreated streamEvKind = iota
	evText
	evToolBegin
	evToolArg
	evReasoning
	evDone
	evError
)

// streamEv is one canonical streaming event.
type streamEv struct {
	kind streamEvKind

	text               string
	toolIdx            int    // upstream tool index, when known
	toolID             string // call id
	toolName           string
	toolArgs           string // delta for evToolArg
	reasoningText      string // delta for evReasoning
	reasoningRedacted  bool   // redacted_thinking (no visible summary)
	reasoningSignature string
	finishReason       string
	usage              *Usage
	errMsg             string
	model              string
}

// streamEncoder writes canonical events as one protocol's SSE stream.
type streamEncoder interface {
	Write(ev streamEv) error
	// Close writes any terminal events still pending (e.g. [DONE]).
	Close() error
}

// ---------------------------------------------------------------------------
// SSE framing
// ---------------------------------------------------------------------------

// readSSE parses a server-sent-events stream and invokes handle for each
// event with its (possibly empty) event name and joined data payload.
func readSSE(r io.Reader, handle func(event string, data []byte) error) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	var name string
	var data []byte
	dispatch := func() error {
		if len(data) > 0 {
			if err := handle(name, data); err != nil {
				return err
			}
		}
		name = ""
		data = nil
		return nil
	}
	for sc.Scan() {
		line := strings.TrimSuffix(sc.Text(), "\r")
		switch {
		case line == "":
			if err := dispatch(); err != nil {
				return err
			}
		case strings.HasPrefix(line, ":"):
			// comment
		case strings.HasPrefix(line, "event:"):
			name = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if len(data) > 0 {
				data = append(data, '\n')
			}
			data = append(data, payload...)
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return dispatch()
}

func writeSSE(w io.Writer, event string, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	var sb strings.Builder
	if event != "" {
		sb.WriteString("event: ")
		sb.WriteString(event)
		sb.WriteByte('\n')
	}
	sb.WriteString("data: ")
	sb.Write(b)
	sb.WriteString("\n\n")
	_, err = io.WriteString(w, sb.String())
	return err
}

// ---------------------------------------------------------------------------
// Upstream stream parsers (upstream protocol -> canonical events)
// ---------------------------------------------------------------------------

func parseChatStream(r io.Reader, emit func(streamEv) error) error {
	doneSent := false
	curToolIdx := 0
	err := readSSE(r, func(_ string, data []byte) error {
		line := strings.TrimSpace(string(data))
		if line == "[DONE]" {
			return nil
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Role    string `json:"role"`
					Content string `json:"content"`
					// reasoning_content is a Chat convention used by
					// DeepSeek/Qwen-family providers; bridged into reasoning.
					ReasoningContent string `json:"reasoning_content"`
					ToolCalls        []struct {
						Index    *int   `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name string `json:"name"`
							// Some gateways (esp. Claude-translating ones)
							// emit arguments as a JSON object, not a string.
							Arguments json.RawMessage `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason json.RawMessage `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(line), &chunk); err != nil {
			return nil // ignore non-JSON lines
		}
		for _, c := range chunk.Choices {
			if c.Delta.ReasoningContent != "" {
				if err := emit(streamEv{kind: evReasoning, reasoningText: c.Delta.ReasoningContent}); err != nil {
					return err
				}
			}
			if c.Delta.Content != "" {
				if err := emit(streamEv{kind: evText, text: c.Delta.Content}); err != nil {
					return err
				}
			}
			for _, tc := range c.Delta.ToolCalls {
				idx := curToolIdx
				if tc.Index != nil {
					idx = *tc.Index
					curToolIdx = idx
				}
				if tc.ID != "" && tc.Function.Name != "" {
					if err := emit(streamEv{
						kind: evToolBegin, toolIdx: idx, toolID: tc.ID, toolName: tc.Function.Name,
					}); err != nil {
						return err
					}
				}
				if args := jsonCompactOrString(tc.Function.Arguments); args != "" {
					if err := emit(streamEv{kind: evToolArg, toolIdx: idx, toolArgs: args}); err != nil {
						return err
					}
				}
			}
			if fr := jsonStringOrEmpty(c.FinishReason); fr != "" {
				doneSent = true
				if err := emit(streamEv{kind: evDone, finishReason: fr}); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if !doneSent {
		return emit(streamEv{kind: evDone, finishReason: "stop"})
	}
	return nil
}

func parseResponsesStream(r io.Reader, emit func(streamEv) error) error {
	// Responses function_call_arguments.delta carries item_id, not call_id;
	// map item_id -> call_id from the function_call output_item.added events so
	// argument deltas route to the right tool in downstream encoders.
	toolIDByItemID := map[string]string{}
	return readSSE(r, func(_ string, data []byte) error {
		var e struct {
			Type string `json:"type"`
			Item struct {
				ID        string `json:"id"`
				Type      string `json:"type"`
				CallID    string `json:"call_id"`
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"item"`
			Delta     json.RawMessage `json:"delta"`
			Arguments string          `json:"arguments"`
			ItemID    string          `json:"item_id"`
			Response  struct {
				ID     string          `json:"id"`
				Model  string          `json:"model"`
				Status string          `json:"status"`
				Usage  json.RawMessage `json:"usage"`
			} `json:"response"`
			Error json.RawMessage `json:"error"`
		}
		if err := json.Unmarshal(data, &e); err != nil {
			return nil
		}
		switch e.Type {
		case "response.created":
			return emit(streamEv{kind: evCreated, model: e.Response.Model})
		case "response.output_text.delta":
			return emit(streamEv{kind: evText, text: jsonCompactOrString(e.Delta)})
		case "response.output_item.added":
			if e.Item.Type == "function_call" {
				toolIDByItemID[e.Item.ID] = e.Item.CallID
				return emit(streamEv{kind: evToolBegin, toolID: e.Item.CallID, toolName: e.Item.Name})
			}
			if e.Item.Type == "reasoning" {
				return emit(streamEv{kind: evReasoning})
			}
		case "response.function_call_arguments.delta":
			return emit(streamEv{kind: evToolArg, toolID: toolIDByItemID[e.ItemID], toolArgs: jsonCompactOrString(e.Delta)})
		case "response.reasoning_summary_text.delta":
			return emit(streamEv{kind: evReasoning, reasoningText: jsonCompactOrString(e.Delta)})
		case "response.completed":
			fr := "stop"
			switch e.Response.Status {
			case "incomplete":
				fr = "length"
			case "failed":
				fr = ""
			}
			return emit(streamEv{kind: evDone, finishReason: fr, usage: usageFromAny(e.Response.Usage)})
		case "response.failed", "error":
			return emit(streamEv{kind: evError, errMsg: string(e.Error)})
		}
		return nil
	})
}

func parseMessagesStream(r io.Reader, emit func(streamEv) error) error {
	// Anthropic content blocks are addressed by index, and tool_use blocks do
	// not repeat their id on input_json_delta. Map each block index to the tool
	// id announced on content_block_start so parallel tool calls keep their
	// arguments separate.
	toolIDByIndex := map[int]string{}
	doneSent := false
	var usage Usage
	err := readSSE(r, func(_ string, data []byte) error {
		var e struct {
			Type         string `json:"type"`
			Index        *int   `json:"index"`
			ContentBlock struct {
				Type      string `json:"type"`
				ID        string `json:"id"`
				Name      string `json:"name"`
				Signature string `json:"signature"`
			} `json:"content_block"`
			Delta struct {
				Type        string          `json:"type"`
				Text        string          `json:"text"`
				PartialJSON json.RawMessage `json:"partial_json"`
				StopReason  string          `json:"stop_reason"`
				Thinking    string          `json:"thinking"`
			} `json:"delta"`
			Message struct {
				Usage struct {
					InputTokens  int64 `json:"input_tokens"`
					OutputTokens int64 `json:"output_tokens"`
				} `json:"usage"`
			} `json:"message"`
			Usage struct {
				OutputTokens int64 `json:"output_tokens"`
			} `json:"usage"`
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(data, &e); err != nil {
			return nil
		}
		switch e.Type {
		case "message_start":
			usage.InputTokens = e.Message.Usage.InputTokens
		case "content_block_start":
			switch e.ContentBlock.Type {
			case "tool_use":
				idx := derefInt(e.Index)
				toolIDByIndex[idx] = e.ContentBlock.ID
				return emit(streamEv{
					kind: evToolBegin, toolIdx: idx, toolID: e.ContentBlock.ID, toolName: e.ContentBlock.Name,
				})
			case "thinking":
				return emit(streamEv{kind: evReasoning, reasoningSignature: e.ContentBlock.Signature})
			case "redacted_thinking":
				return emit(streamEv{kind: evReasoning, reasoningRedacted: true, reasoningSignature: e.ContentBlock.Signature})
			}
		case "content_block_delta":
			switch e.Delta.Type {
			case "text_delta":
				return emit(streamEv{kind: evText, text: e.Delta.Text})
			case "input_json_delta":
				idx := derefInt(e.Index)
				return emit(streamEv{kind: evToolArg, toolIdx: idx, toolID: toolIDByIndex[idx], toolArgs: jsonCompactOrString(e.Delta.PartialJSON)})
			case "thinking_delta":
				return emit(streamEv{kind: evReasoning, reasoningText: e.Delta.Thinking})
			}
		case "message_delta":
			if e.Usage.OutputTokens > 0 {
				usage.OutputTokens = e.Usage.OutputTokens
			}
			if e.Delta.StopReason != "" {
				doneSent = true
				return emit(streamEv{
					kind: evDone, finishReason: finishReasonFromAnthropic(e.Delta.StopReason),
					usage: usageCopy(&usage),
				})
			}
		case "error":
			return emit(streamEv{kind: evError, errMsg: e.Error.Message})
		}
		return nil
	})
	if err != nil {
		return err
	}
	if !doneSent {
		return emit(streamEv{kind: evDone, finishReason: "stop", usage: usageCopy(&usage)})
	}
	return nil
}

func derefInt(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

// usageCopy returns a private copy of u, or nil when every counter is zero so
// downstream encoders do not emit an all-zero usage object. The copy matters
// because the caller reuses its accumulator across events.
func usageCopy(u *Usage) *Usage {
	if u == nil || (u.InputTokens == 0 && u.OutputTokens == 0 && u.TotalTokens == 0) {
		return nil
	}
	v := *u
	return &v
}

// ---------------------------------------------------------------------------
// Downstream stream encoders (canonical events -> client protocol)
// ---------------------------------------------------------------------------

type chatEncoder struct {
	w           io.Writer
	id          string
	created     int64
	model       string
	started     bool
	done        bool
	toolIdx     int
	toolIdxByID map[string]int
}

func newChatEncoder(w io.Writer, model string) *chatEncoder {
	return &chatEncoder{
		w: w, id: genID("chatcmpl"), created: timeNow(), model: model,
		toolIdxByID: map[string]int{},
	}
}

func (e *chatEncoder) Write(ev streamEv) error {
	if e.done {
		return nil
	}
	switch ev.kind {
	case evText:
		if !e.started {
			if err := e.chunk(map[string]any{"role": "assistant"}); err != nil {
				return err
			}
			e.started = true
		}
		return e.chunk(map[string]any{"content": ev.text})
	case evToolBegin:
		if !e.started {
			if err := e.chunk(map[string]any{"role": "assistant"}); err != nil {
				return err
			}
			e.started = true
		}
		idx := e.toolIdx
		e.toolIdx++
		if ev.toolID != "" {
			e.toolIdxByID[ev.toolID] = idx
		}
		return e.chunk(map[string]any{"tool_calls": []any{
			map[string]any{"index": idx, "id": ev.toolID, "type": "function",
				"function": map[string]any{"name": ev.toolName, "arguments": ""}},
		}})
	case evToolArg:
		idx := ev.toolIdx
		if ev.toolID != "" {
			if i, ok := e.toolIdxByID[ev.toolID]; ok {
				idx = i
			}
		}
		return e.chunk(map[string]any{"tool_calls": []any{
			map[string]any{"index": idx, "function": map[string]any{"arguments": ev.toolArgs}},
		}})
	case evDone:
		if !e.started {
			if err := e.chunk(map[string]any{"role": "assistant"}); err != nil {
				return err
			}
			e.started = true
		}
		e.done = true
		if err := e.finishChunk(finishReasonToChat(ev.finishReason)); err != nil {
			return err
		}
		if ev.usage != nil {
			// OpenAI streams usage in a trailing choices:[] chunk when the
			// client requests stream_options.include_usage. Strict OpenAI
			// consumers (e.g. new-api's OpenAI channel, which re-emits the
			// stream as Anthropic for Claude Code) rely on this chunk to emit
			// the terminal message_delta/message_stop; without it the client
			// never sees the message finalized and reports an interrupted
			// conversation.
			return e.usageChunk(ev.usage)
		}
		return nil
	}
	return nil
}

func (e *chatEncoder) chunk(delta map[string]any) error {
	payload := map[string]any{
		"id":      e.id,
		"object":  "chat.completion.chunk",
		"created": e.created,
		"model":   e.model,
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": nil}},
	}
	return writeSSE(e.w, "", payload)
}

// finishChunk emits the terminal chunk with finish_reason on the choice
// (choices[0].finish_reason) and an empty delta, matching the OpenAI / DeepSeek
// wire format. Consumers read finish_reason from the choice, not from delta.
func (e *chatEncoder) finishChunk(reason any) error {
	payload := map[string]any{
		"id":      e.id,
		"object":  "chat.completion.chunk",
		"created": e.created,
		"model":   e.model,
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": reason}},
	}
	return writeSSE(e.w, "", payload)
}

// usageChunk emits the usage-only trailing chunk OpenAI sends with
// stream_options.include_usage, so strict consumers can finalize the stream.
func (e *chatEncoder) usageChunk(u *Usage) error {
	total := u.TotalTokens
	if total == 0 && (u.InputTokens > 0 || u.OutputTokens > 0) {
		// Anthropic sources do not report a combined total; OpenAI defines
		// total_tokens = prompt_tokens + completion_tokens, so compute it.
		total = u.InputTokens + u.OutputTokens
	}
	payload := map[string]any{
		"id":      e.id,
		"object":  "chat.completion.chunk",
		"created": e.created,
		"model":   e.model,
		"choices": []any{},
		"usage": map[string]any{
			"prompt_tokens":     u.InputTokens,
			"completion_tokens": u.OutputTokens,
			"total_tokens":      total,
		},
	}
	return writeSSE(e.w, "", payload)
}

func (e *chatEncoder) Close() error {
	_, err := io.WriteString(e.w, "data: [DONE]\n\n")
	return err
}

// respTool tracks one in-progress Responses function_call item. Multiple tools
// may be open at once (parallel tool use), each addressed by call id and/or
// the upstream content-block index.
type respTool struct {
	itemID      string
	callID      string
	name        string
	outputIndex int
	argsSnap    string
	done        bool
}

// respItem is a completed output item awaiting emission in response.completed.
// Items are indexed at creation time and sorted before emission so out-of-order
// completion (parallel tools) still produces index-ordered output.
type respItem struct {
	index int
	raw   map[string]any
}

type responsesEncoder struct {
	w           io.Writer
	id          string
	created     int64
	model       string
	bridge      bool
	seq         int64
	outputIndex int
	// active text item
	textItemID   string
	textSnapshot string
	textIndex    int
	textOpen     bool
	// active reasoning item
	reasoningItemID   string
	reasoningText     string
	reasoningIndex    int
	reasoningRedacted bool
	reasoningOpen     bool
	// tool items
	toolsByID    map[string]*respTool
	toolsByIndex map[int]*respTool
	toolOrder    []*respTool
	// completed items, emitted sorted by index
	items        []*respItem
	usage        *Usage
	finishReason string
	done         bool
	createdSent  bool
}

func newResponsesEncoder(w io.Writer, model string, bridge bool) *responsesEncoder {
	return &responsesEncoder{
		w: w, id: genID("resp"), created: timeNow(), model: model,
		bridge:       bridge,
		toolsByID:    map[string]*respTool{},
		toolsByIndex: map[int]*respTool{},
	}
}

// nextSeq hands out the per-response event sequence number, starting at 0.
func (e *responsesEncoder) nextSeq() int64 {
	n := e.seq
	e.seq++
	return n
}

func (e *responsesEncoder) toolFor(ev streamEv) *respTool {
	if ev.toolID != "" {
		if st := e.toolsByID[ev.toolID]; st != nil {
			return st
		}
	}
	return e.toolsByIndex[ev.toolIdx]
}

func (e *responsesEncoder) Write(ev streamEv) error {
	if e.done {
		return nil
	}
	// Always emit response.created before the first content event, even if the
	// upstream started directly with a tool call (no text).
	if ev.kind != evCreated && !e.createdSent {
		switch ev.kind {
		case evText, evToolBegin, evToolArg, evReasoning, evDone:
			if err := e.emitCreated(e.model); err != nil {
				return err
			}
		}
	}
	switch ev.kind {
	case evCreated:
		return e.emitCreated(ev.model)
	case evReasoning:
		if !e.bridge {
			return nil
		}
		if !e.reasoningOpen {
			if err := e.finishTextItem(); err != nil {
				return err
			}
			e.reasoningRedacted = ev.reasoningRedacted
		}
		if err := e.ensureReasoningItem(); err != nil {
			return err
		}
		if !e.reasoningRedacted && ev.reasoningText != "" {
			e.reasoningText += ev.reasoningText
			return writeSSE(e.w, "response.reasoning_summary_text.delta", map[string]any{
				"type": "response.reasoning_summary_text.delta", "item_id": e.reasoningItemID,
				"output_index": e.reasoningIndex, "content_index": 0,
				"delta": ev.reasoningText, "snapshot": e.reasoningText,
				"sequence_number": e.nextSeq(),
			})
		}
		return nil
	case evText:
		if err := e.finishReasoningItem(); err != nil {
			return err
		}
		if err := e.ensureTextItem(); err != nil {
			return err
		}
		snap := e.textSnapshot + ev.text
		e.textSnapshot = snap
		return writeSSE(e.w, "response.output_text.delta", map[string]any{
			"type": "response.output_text.delta", "item_id": e.textItemID,
			"output_index": e.textIndex, "content_index": 0,
			"delta": ev.text, "snapshot": snap, "sequence_number": e.nextSeq(),
		})
	case evToolBegin:
		if err := e.finishReasoningItem(); err != nil {
			return err
		}
		if err := e.finishTextItem(); err != nil {
			return err
		}
		if st := e.toolFor(ev); st != nil {
			// Duplicate begin (some upstreams re-announce): fill any gaps.
			if st.name == "" {
				st.name = ev.toolName
			}
			if st.callID == "" {
				st.callID = ev.toolID
			}
			return nil
		}
		st := &respTool{
			itemID:      genID("fc"),
			callID:      ev.toolID,
			name:        ev.toolName,
			outputIndex: e.outputIndex,
		}
		e.outputIndex++
		e.toolOrder = append(e.toolOrder, st)
		if ev.toolID != "" {
			e.toolsByID[ev.toolID] = st
		}
		// Always index by the upstream tool index too: Chat upstreams stream
		// argument deltas with only an index (no call id), including index 0.
		e.toolsByIndex[ev.toolIdx] = st
		return writeSSE(e.w, "response.output_item.added", map[string]any{
			"type": "response.output_item.added", "output_index": st.outputIndex,
			"item": map[string]any{
				"id": st.itemID, "type": "function_call", "status": "in_progress",
				"call_id": ev.toolID, "name": ev.toolName, "arguments": "",
			},
			"sequence_number": e.nextSeq(),
		})
	case evToolArg:
		st := e.toolFor(ev)
		if st == nil {
			// Argument for an unknown tool: ignore rather than corrupt state.
			return nil
		}
		st.argsSnap += ev.toolArgs
		return writeSSE(e.w, "response.function_call_arguments.delta", map[string]any{
			"type": "response.function_call_arguments.delta", "item_id": st.itemID,
			"output_index": st.outputIndex, "delta": ev.toolArgs,
			"sequence_number": e.nextSeq(),
		})
	case evDone:
		if err := e.finishReasoningItem(); err != nil {
			return err
		}
		if err := e.finishTextItem(); err != nil {
			return err
		}
		for _, st := range e.toolOrder {
			if err := e.finishToolItem(st); err != nil {
				return err
			}
		}
		e.finishReason = ev.finishReason
		if ev.usage != nil {
			e.usage = ev.usage
		}
		e.done = true
		return e.emitCompleted()
	case evError:
		e.done = true
		return writeSSE(e.w, "error", map[string]any{
			"type": "error", "code": "server_error", "message": ev.errMsg,
			"sequence_number": e.nextSeq(),
		})
	}
	return nil
}

func (e *responsesEncoder) ensureTextItem() error {
	if e.textOpen {
		return nil
	}
	if !e.createdSent {
		if err := e.emitCreated(e.model); err != nil {
			return err
		}
	}
	e.textItemID = genID("msg")
	e.textSnapshot = ""
	e.textIndex = e.outputIndex
	e.outputIndex++
	e.textOpen = true
	if err := writeSSE(e.w, "response.output_item.added", map[string]any{
		"type": "response.output_item.added", "output_index": e.textIndex,
		"item": map[string]any{
			"id": e.textItemID, "type": "message", "status": "in_progress",
			"role": "assistant", "content": []any{},
		},
		"sequence_number": e.nextSeq(),
	}); err != nil {
		return err
	}
	return writeSSE(e.w, "response.content_part.added", map[string]any{
		"type": "response.content_part.added", "item_id": e.textItemID,
		"output_index": e.textIndex, "content_index": 0,
		"part":            map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
		"sequence_number": e.nextSeq(),
	})
}

func (e *responsesEncoder) finishTextItem() error {
	if !e.textOpen {
		return nil
	}
	if err := writeSSE(e.w, "response.output_text.done", map[string]any{
		"type": "response.output_text.done", "item_id": e.textItemID,
		"output_index": e.textIndex, "content_index": 0, "text": e.textSnapshot,
		"sequence_number": e.nextSeq(),
	}); err != nil {
		return err
	}
	if err := writeSSE(e.w, "response.content_part.done", map[string]any{
		"type": "response.content_part.done", "item_id": e.textItemID,
		"output_index": e.textIndex, "content_index": 0,
		"part":            map[string]any{"type": "output_text", "text": e.textSnapshot, "annotations": []any{}},
		"sequence_number": e.nextSeq(),
	}); err != nil {
		return err
	}
	item := map[string]any{
		"id": e.textItemID, "type": "message", "status": "completed", "role": "assistant",
		"content": []any{map[string]any{"type": "output_text", "text": e.textSnapshot, "annotations": []any{}}},
	}
	if err := writeSSE(e.w, "response.output_item.done", map[string]any{
		"type": "response.output_item.done", "output_index": e.textIndex, "item": item,
		"sequence_number": e.nextSeq(),
	}); err != nil {
		return err
	}
	e.items = append(e.items, &respItem{index: e.textIndex, raw: item})
	e.textOpen = false
	e.textItemID = ""
	return nil
}

func (e *responsesEncoder) ensureReasoningItem() error {
	if e.reasoningOpen {
		return nil
	}
	if !e.createdSent {
		if err := e.emitCreated(e.model); err != nil {
			return err
		}
	}
	e.reasoningItemID = genID("rs")
	e.reasoningText = ""
	e.reasoningIndex = e.outputIndex
	e.outputIndex++
	e.reasoningOpen = true
	item := map[string]any{
		"id": e.reasoningItemID, "type": "reasoning", "status": "in_progress",
		"summary": []any{}, "encrypted_content": nil,
	}
	if e.reasoningRedacted {
		item["encrypted_content"] = "opaque"
	}
	if err := writeSSE(e.w, "response.output_item.added", map[string]any{
		"type": "response.output_item.added", "output_index": e.reasoningIndex,
		"item": item, "sequence_number": e.nextSeq(),
	}); err != nil {
		return err
	}
	return writeSSE(e.w, "response.reasoning_summary_part.added", map[string]any{
		"type": "response.reasoning_summary_part.added", "item_id": e.reasoningItemID,
		"output_index": e.reasoningIndex, "content_index": 0,
		"part":            map[string]any{"type": "summary_text", "text": ""},
		"sequence_number": e.nextSeq(),
	})
}

func (e *responsesEncoder) finishReasoningItem() error {
	if !e.reasoningOpen {
		return nil
	}
	part := map[string]any{"type": "summary_text", "text": e.reasoningText}
	if err := writeSSE(e.w, "response.reasoning_summary_part.done", map[string]any{
		"type": "response.reasoning_summary_part.done", "item_id": e.reasoningItemID,
		"output_index": e.reasoningIndex, "content_index": 0, "part": part,
		"sequence_number": e.nextSeq(),
	}); err != nil {
		return err
	}
	item := map[string]any{
		"id": e.reasoningItemID, "type": "reasoning", "status": "completed",
		"summary": []any{part}, "encrypted_content": nil,
	}
	if e.reasoningRedacted && e.reasoningText == "" {
		item["summary"] = []any{}
		item["encrypted_content"] = "opaque"
	}
	if err := writeSSE(e.w, "response.output_item.done", map[string]any{
		"type": "response.output_item.done", "output_index": e.reasoningIndex, "item": item,
		"sequence_number": e.nextSeq(),
	}); err != nil {
		return err
	}
	e.items = append(e.items, &respItem{index: e.reasoningIndex, raw: item})
	e.reasoningOpen = false
	e.reasoningItemID = ""
	e.reasoningRedacted = false
	return nil
}

func (e *responsesEncoder) finishToolItem(st *respTool) error {
	if st == nil || st.done {
		return nil
	}
	if err := writeSSE(e.w, "response.function_call_arguments.done", map[string]any{
		"type": "response.function_call_arguments.done", "item_id": st.itemID,
		"output_index": st.outputIndex, "name": st.name, "arguments": st.argsSnap,
		"sequence_number": e.nextSeq(),
	}); err != nil {
		return err
	}
	item := map[string]any{
		"id": st.itemID, "type": "function_call", "status": "completed",
		"call_id": st.callID, "name": st.name, "arguments": st.argsSnap,
	}
	if err := writeSSE(e.w, "response.output_item.done", map[string]any{
		"type": "response.output_item.done", "output_index": st.outputIndex, "item": item,
		"sequence_number": e.nextSeq(),
	}); err != nil {
		return err
	}
	e.items = append(e.items, &respItem{index: st.outputIndex, raw: item})
	st.done = true
	return nil
}

func (e *responsesEncoder) emitCreated(model string) error {
	if e.createdSent {
		return nil
	}
	if model == "" {
		model = e.model
	}
	e.createdSent = true
	resp := map[string]any{
		"id": e.id, "object": "response", "created_at": e.created,
		"status": "in_progress", "model": model, "output": []any{}, "error": nil,
	}
	if err := writeSSE(e.w, "response.created", map[string]any{"type": "response.created", "response": resp, "sequence_number": e.nextSeq()}); err != nil {
		return err
	}
	return writeSSE(e.w, "response.in_progress", map[string]any{"type": "response.in_progress", "response": resp, "sequence_number": e.nextSeq()})
}

func (e *responsesEncoder) emitCompleted() error {
	status := "completed"
	var incomplete any
	switch {
	case e.finishReason == "length":
		status = "incomplete"
		incomplete = map[string]any{"reason": "max_output_tokens"}
	case e.finishReason == "" || e.finishReason == "error":
		status = "failed"
	}
	sort.SliceStable(e.items, func(i, j int) bool { return e.items[i].index < e.items[j].index })
	output := make([]any, 0, len(e.items))
	for _, it := range e.items {
		output = append(output, it.raw)
	}
	resp := map[string]any{
		"id": e.id, "object": "response", "created_at": e.created,
		"status": status, "model": e.model, "output": output, "error": nil,
	}
	if incomplete != nil {
		resp["incomplete_details"] = incomplete
	}
	if u := usageToResponses(e.usage); u != nil {
		resp["usage"] = u
	}
	return writeSSE(e.w, "response.completed", map[string]any{"type": "response.completed", "response": resp, "sequence_number": e.nextSeq()})
}

func (e *responsesEncoder) Close() error {
	if e.done {
		return nil
	}
	if !e.createdSent {
		if err := e.emitCreated(e.model); err != nil {
			return err
		}
	}
	if err := e.finishReasoningItem(); err != nil {
		return err
	}
	if err := e.finishTextItem(); err != nil {
		return err
	}
	for _, st := range e.toolOrder {
		if err := e.finishToolItem(st); err != nil {
			return err
		}
	}
	e.done = true
	return e.emitCompleted()
}

type messagesEncoder struct {
	w            io.Writer
	id           string
	model        string
	bridge       bool
	blockCounter int
	// text / thinking blocks are strictly sequential: each must be stopped
	// before the next different kind starts, or Claude Code's parser errors
	// ("Content block is not a input_json block"). Tool_use blocks may be
	// parallel (consecutive starts, deltas routed by index), matching Anthropic.
	textIndex     int
	textOpen      bool
	thinkingIndex int
	thinkingOpen  bool
	toolIndexByID map[string]int
	// upstream tool index -> this encoder's block index. The upstream's tool
	// index (e.g. Chat's tool_calls[].index) does not match the block index
	// once thinking/text blocks precede the tools; argument deltas must be
	// routed to the actual tool_use block, or Claude Code errors with
	// "Content block is not a input_json block".
	toolIdxMap   map[int]int
	toolOpen     []int
	inputTokens  int64
	outputTokens int64
	finishReason string
	started      bool
	done         bool
}

func newMessagesEncoder(w io.Writer, model string, bridge bool) *messagesEncoder {
	return &messagesEncoder{
		w: w, id: genID("msg"), model: model, bridge: bridge,
		toolIndexByID: map[string]int{},
		toolIdxMap:    map[int]int{},
	}
}

func (e *messagesEncoder) Write(ev streamEv) error {
	if e.done {
		return nil
	}
	if !e.started {
		if err := e.start(); err != nil {
			return err
		}
	}
	switch ev.kind {
	case evReasoning:
		if !e.bridge {
			return nil
		}
		if !e.thinkingOpen {
			if err := e.closeText(); err != nil {
				return err
			}
			if err := e.closeTools(); err != nil {
				return err
			}
			e.thinkingIndex = e.blockCounter
			e.blockCounter++
			e.thinkingOpen = true
			block := map[string]any{"type": "thinking", "thinking": "", "signature": ""}
			if ev.reasoningRedacted {
				block = map[string]any{"type": "redacted_thinking", "data": ""}
			}
			if err := writeSSE(e.w, "content_block_start", map[string]any{
				"type": "content_block_start", "index": e.thinkingIndex,
				"content_block": block,
			}); err != nil {
				return err
			}
		}
		if !ev.reasoningRedacted && ev.reasoningText != "" {
			return writeSSE(e.w, "content_block_delta", map[string]any{
				"type": "content_block_delta", "index": e.thinkingIndex,
				"delta": map[string]any{"type": "thinking_delta", "thinking": ev.reasoningText},
			})
		}
		return nil
	case evText:
		if !e.textOpen {
			if err := e.closeThinking(); err != nil {
				return err
			}
			if err := e.closeTools(); err != nil {
				return err
			}
			e.textIndex = e.blockCounter
			e.blockCounter++
			e.textOpen = true
			if err := writeSSE(e.w, "content_block_start", map[string]any{
				"type": "content_block_start", "index": e.textIndex,
				"content_block": map[string]any{"type": "text", "text": ""},
			}); err != nil {
				return err
			}
		}
		return writeSSE(e.w, "content_block_delta", map[string]any{
			"type": "content_block_delta", "index": e.textIndex,
			"delta": map[string]any{"type": "text_delta", "text": ev.text},
		})
	case evToolBegin:
		if err := e.closeThinking(); err != nil {
			return err
		}
		if err := e.closeText(); err != nil {
			return err
		}
		idx := e.blockCounter
		e.blockCounter++
		if err := writeSSE(e.w, "content_block_start", map[string]any{
			"type": "content_block_start", "index": idx,
			"content_block": map[string]any{"type": "tool_use", "id": ev.toolID, "name": ev.toolName, "input": map[string]any{}},
		}); err != nil {
			return err
		}
		e.toolOpen = append(e.toolOpen, idx)
		if ev.toolID != "" {
			e.toolIndexByID[ev.toolID] = idx
		}
		e.toolIdxMap[ev.toolIdx] = idx
		return nil
	case evToolArg:
		idx := ev.toolIdx
		if ev.toolID != "" {
			if i, ok := e.toolIndexByID[ev.toolID]; ok {
				idx = i
			}
		} else if i, ok := e.toolIdxMap[ev.toolIdx]; ok {
			idx = i
		}
		return writeSSE(e.w, "content_block_delta", map[string]any{
			"type": "content_block_delta", "index": idx,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": ev.toolArgs},
		})
	case evDone:
		if ev.usage != nil {
			e.inputTokens = ev.usage.InputTokens
			e.outputTokens = ev.usage.OutputTokens
		}
		e.finishReason = ev.finishReason
		if err := e.finish(); err != nil {
			return err
		}
		e.done = true
	case evError:
		e.done = true
	}
	return nil
}

func (e *messagesEncoder) closeThinking() error {
	if !e.thinkingOpen {
		return nil
	}
	if err := writeSSE(e.w, "content_block_stop", map[string]any{"type": "content_block_stop", "index": e.thinkingIndex}); err != nil {
		return err
	}
	e.thinkingOpen = false
	return nil
}

func (e *messagesEncoder) closeText() error {
	if !e.textOpen {
		return nil
	}
	if err := writeSSE(e.w, "content_block_stop", map[string]any{"type": "content_block_stop", "index": e.textIndex}); err != nil {
		return err
	}
	e.textOpen = false
	return nil
}

func (e *messagesEncoder) closeTools() error {
	for _, idx := range e.toolOpen {
		if err := writeSSE(e.w, "content_block_stop", map[string]any{"type": "content_block_stop", "index": idx}); err != nil {
			return err
		}
	}
	e.toolOpen = nil
	return nil
}

func (e *messagesEncoder) start() error {
	e.started = true
	usage := map[string]any{"input_tokens": 0, "output_tokens": 0}
	if e.inputTokens > 0 {
		usage["input_tokens"] = e.inputTokens
	}
	return writeSSE(e.w, "message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": e.id, "type": "message", "role": "assistant", "model": e.model,
			"content": []any{}, "stop_reason": nil, "stop_sequence": nil, "usage": usage,
		},
	})
}

func (e *messagesEncoder) finish() error {
	if err := e.closeThinking(); err != nil {
		return err
	}
	if err := e.closeText(); err != nil {
		return err
	}
	if err := e.closeTools(); err != nil {
		return err
	}
	if err := writeSSE(e.w, "message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": finishReasonToAnthropic(e.finishReason), "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": e.outputTokens},
	}); err != nil {
		return err
	}
	return writeSSE(e.w, "message_stop", map[string]any{"type": "message_stop"})
}

func (e *messagesEncoder) Close() error {
	if !e.done {
		if !e.started {
			if err := e.start(); err != nil {
				return err
			}
		}
		if err := e.finish(); err != nil {
			return err
		}
		e.done = true
	}
	return nil
}

func newStreamEncoder(proto string, w io.Writer, model string, bridge bool) streamEncoder {
	switch proto {
	case ProtoOpenAIResponses:
		return newResponsesEncoder(w, model, bridge)
	case ProtoAnthropicMessages:
		return newMessagesEncoder(w, model, bridge)
	default:
		return newChatEncoder(w, model)
	}
}

// parseUpstreamStream dispatches to the right parser.
func parseUpstreamStream(proto string, r io.Reader, emit func(streamEv) error) error {
	switch proto {
	case ProtoOpenAIResponses:
		return parseResponsesStream(r, emit)
	case ProtoAnthropicMessages:
		return parseMessagesStream(r, emit)
	default:
		return parseChatStream(r, emit)
	}
}

// respToStreamEvents turns a complete non-streaming response into canonical
// streaming events (used when an upstream ignores the stream flag).
func respToStreamEvents(resp *Resp, model string) []streamEv {
	evs := []streamEv{{kind: evCreated, model: model}}
	for _, b := range firstMessage(resp).Content {
		switch b.Type {
		case "text":
			if b.Text != "" {
				evs = append(evs, streamEv{kind: evText, text: b.Text})
			}
		case "reasoning":
			evs = append(evs, streamEv{kind: evReasoning, reasoningText: b.ReasoningSummary, reasoningRedacted: b.ThinkingIsRedacted})
		case "tool_call":
			evs = append(evs, streamEv{kind: evToolBegin, toolID: b.ToolCallID, toolName: b.ToolName})
			evs = append(evs, streamEv{kind: evToolArg, toolID: b.ToolCallID, toolArgs: b.ToolArgsText})
		}
	}
	evs = append(evs, streamEv{kind: evDone, finishReason: resp.FinishReason, usage: resp.Usage})
	return evs
}
