package main

import (
	"encoding/json"
	"fmt"
)

// ContentBlock is one unit of content inside a message. Only one family of
// fields is populated depending on Type.
type ContentBlock struct {
	Type string

	// text
	Text string

	// tool_call (assistant requesting a tool)
	ToolCallID   string
	ToolName     string
	ToolArgs     json.RawMessage // raw JSON object/array for Messages, string for Chat/Responses
	ToolArgsText string          // the string form (arguments as a JSON string)

	// tool_result (tool output sent back)
	ToolUseID      string
	ToolResult     json.RawMessage // string or array, kept raw
	ToolResultText string          // plain string form

	// image
	ImageURL       string
	ImageData      string
	ImageMediaType string

	// file / document (Responses input_file ↔ Anthropic document)
	FileURL       string
	FileData      string
	FileMediaType string
	FileFilename  string

	// tool_result structured content: when the source protocol carries a
	// content array (text + image + file) instead of a plain string, the parts
	// are kept so media survives the round trip.
	ToolResultBlocks  []ContentBlock
	ToolResultIsError bool

	// reasoning (Responses item) / thinking (Anthropic block)
	//
	// The canonical representation is a "reasoning" block. When it came from a
	// Responses reasoning item, ReasoningRaw carries the full item so it can be
	// restored losslessly (including encrypted_content); ReasoningSummary is the
	// visible summary. When it came from an Anthropic thinking block,
	// ThinkingSignature / ThinkingIsRedacted preserve the native form so it can
	// be emitted back.
	ReasoningSummary   string          // visible summary text
	ReasoningRaw       json.RawMessage // full Responses reasoning item
	ThinkingSignature  string          // Anthropic thinking signature (native or bridge envelope)
	ThinkingIsRedacted bool            // Anthropic redacted_thinking
}

// Message is a single conversational turn.
type Message struct {
	Role    string // "system" | "user" | "assistant" | "tool"
	Content []ContentBlock
}

// Tool describes a function the model may call.
type Tool struct {
	Name        string
	Description string
	InputSchema json.RawMessage
}

// ToolChoice is a normalized tool-selection constraint.
type ToolChoice struct {
	Mode string // "auto" | "none" | "required" | "any" | "tool" | "function"
	Name string // used when Mode is "tool" or "function"
}

// Req is the canonical representation of a request, independent of protocol.
// Cross-protocol conversion only carries the well-known mapped fields; when
// the client and upstream speak the same protocol the request is passed
// through verbatim, so provider-specific parameters are not lost there.
type Req struct {
	Model       string
	Messages    []Message
	System      string
	MaxTokens   *int
	Temperature *float64
	TopP        *float64
	Stream      bool
	Tools       []Tool
	ToolChoice  *ToolChoice
	// ReasoningEffort is the Responses reasoning.effort / Chat reasoning_effort
	// requested by the client.
	ReasoningEffort string
	// ThinkingEnabled / ThinkingBudget are the Anthropic thinking param
	// requested by the client (budget 0 means "adaptive").
	ThinkingEnabled *bool
	ThinkingBudget  int
	// ParallelToolCalls is the Responses parallel_tool_calls flag (nil when the
	// client did not specify it). Messages expresses the inverse as
	// disable_parallel_tool_use.
	ParallelToolCalls *bool
}

// Usage is the canonical token accounting.
type Usage struct {
	InputTokens  int64
	OutputTokens int64
	// TotalTokens may be 0 when the source protocol does not report it.
	TotalTokens int64
	// Raw preserves the original usage object so same-shape emission is exact.
	Raw json.RawMessage
}

// Resp is the canonical representation of a non-streaming response.
type Resp struct {
	ID           string
	Object       string
	Model        string
	Status       string // Responses API status ("completed", "incomplete", "failed")
	FinishReason string // normalized: "stop" | "length" | "tool_calls" | "content_filter" | ""
	// IncompleteReason is the Responses incomplete_details.reason
	// ("max_output_tokens" | "content_filter").
	IncompleteReason string
	Messages         []Message
	Usage            *Usage
	CreatedAt        int64
}

// ---- JSON helpers ----

// rawStringOrObject turns a content field (JSON string or array) into blocks.
func contentBlocksFromJSON(raw json.RawMessage) ([]ContentBlock, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return []ContentBlock{{Type: "text", Text: s}}, nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("content must be a string or array: %w", err)
	}
	blocks := make([]ContentBlock, 0, len(items))
	for _, it := range items {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(it, &obj); err != nil {
			// A bare string inside an array.
			var t string
			if json.Unmarshal(it, &t) == nil {
				blocks = append(blocks, ContentBlock{Type: "text", Text: t})
				continue
			}
			return nil, fmt.Errorf("bad content item %s", string(it))
		}
		var typ string
		_ = json.Unmarshal(obj["type"], &typ)
		if typ == "" {
			// Chat-style items sometimes omit "type".
			if obj["text"] != nil {
				typ = "text"
			} else if obj["image_url"] != nil {
				typ = "image_url"
			}
		}
		switch typ {
		case "text", "input_text", "output_text":
			var t string
			if err := json.Unmarshal(obj["text"], &t); err != nil {
				return nil, fmt.Errorf("text block: %w", err)
			}
			blocks = append(blocks, ContentBlock{Type: "text", Text: t})
		case "refusal":
			var t string
			_ = json.Unmarshal(obj["refusal"], &t)
			blocks = append(blocks, ContentBlock{Type: "text", Text: t})
		case "image_url", "input_image":
			blk, err := imageBlockFromChat(obj["image_url"])
			if err != nil {
				return nil, err
			}
			blocks = append(blocks, blk)
		case "image":
			blk, err := imageBlockFromAnthropic(obj)
			if err != nil {
				return nil, err
			}
			blocks = append(blocks, blk)
		case "document", "input_file":
			blk := fileBlockFromJSON(obj)
			if blk != nil {
				blocks = append(blocks, *blk)
			}
		case "tool_use":
			var id, name string
			_ = json.Unmarshal(obj["id"], &id)
			_ = json.Unmarshal(obj["name"], &name)
			input := obj["input"]
			if len(input) == 0 {
				input = json.RawMessage("{}")
			}
			b, _ := json.Marshal(input)
			blocks = append(blocks, ContentBlock{
				Type: "tool_call", ToolCallID: id, ToolName: name,
				ToolArgs: input, ToolArgsText: string(b),
			})
		case "tool_result":
			var id string
			_ = json.Unmarshal(obj["tool_use_id"], &id)
			content := obj["content"]
			if len(content) == 0 || string(content) == "null" {
				content = json.RawMessage(`""`)
			}
			blk := ContentBlock{
				Type: "tool_result", ToolUseID: id,
				ToolResult: content, ToolResultText: jsonStringValue(content),
			}
			_ = json.Unmarshal(obj["is_error"], &blk.ToolResultIsError)
			// Keep array content structured so tool-result media (images/files)
			// survives the round trip instead of being flattened to a string.
			if !isJSONString(content) && len(content) > 0 {
				var items []json.RawMessage
				if json.Unmarshal(content, &items) == nil {
					if parts, err := contentBlocksFromJSON(content); err == nil && len(parts) > 0 {
						blk.ToolResultBlocks = parts
					}
				}
			}
			blocks = append(blocks, blk)
		case "thinking":
			var t, sig string
			_ = json.Unmarshal(obj["thinking"], &t)
			_ = json.Unmarshal(obj["signature"], &sig)
			blk := ContentBlock{Type: "reasoning", ReasoningSummary: t, ThinkingSignature: sig}
			if raw, ok := decodeReasoningItem(sig); ok {
				blk.ReasoningRaw = raw
				blk.ReasoningSummary = reasoningSummaryText(raw)
			}
			blocks = append(blocks, blk)
		case "redacted_thinking":
			var data string
			_ = json.Unmarshal(obj["data"], &data)
			blk := ContentBlock{Type: "reasoning", ThinkingIsRedacted: true}
			if raw, ok := decodeReasoningItem(data); ok {
				blk.ReasoningRaw = raw
				blk.ReasoningSummary = reasoningSummaryText(raw)
			}
			blocks = append(blocks, blk)
		default:
			// Skip unknown block types (reasoning parts, audio, etc.) rather than fail.
			continue
		}
	}
	return blocks, nil
}

func imageBlockFromChat(raw json.RawMessage) (ContentBlock, error) {
	// Responses image_url may be a plain string (data URL / http URL) or the
	// Chat form {"url": ...}.
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if s == "" {
			return ContentBlock{}, fmt.Errorf("image_url missing url")
		}
		return ContentBlock{Type: "image", ImageURL: s}, nil
	}
	var u struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(raw, &u); err != nil || u.URL == "" {
		return ContentBlock{}, fmt.Errorf("image_url block missing url")
	}
	return ContentBlock{Type: "image", ImageURL: u.URL}, nil
}

func imageBlockFromAnthropic(obj map[string]json.RawMessage) (ContentBlock, error) {
	var src struct {
		Type      string `json:"type"`
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
	}
	if err := json.Unmarshal(obj["source"], &src); err != nil {
		return ContentBlock{}, fmt.Errorf("image source: %w", err)
	}
	return ContentBlock{
		Type: "image", ImageData: src.Data,
		ImageMediaType: src.MediaType,
		ImageURL:       dataURL(src.MediaType, src.Data),
	}, nil
}

// fileBlockFromJSON parses either an Anthropic document block or a Responses
// input_file part into a canonical file block. Returns nil when the object has
// no recognizable source.
func fileBlockFromJSON(obj map[string]json.RawMessage) *ContentBlock {
	blk := ContentBlock{Type: "file"}
	var filename string
	_ = json.Unmarshal(obj["title"], &filename)
	if filename == "" {
		_ = json.Unmarshal(obj["filename"], &filename)
	}
	blk.FileFilename = filename

	// Responses input_file: file_url / file_data (data URL).
	var fileURL, fileData string
	_ = json.Unmarshal(obj["file_url"], &fileURL)
	_ = json.Unmarshal(obj["file_data"], &fileData)
	if fileURL != "" {
		blk.FileURL = fileURL
		return &blk
	}
	if fileData != "" {
		media, data := splitDataURL(fileData)
		blk.FileData = data
		blk.FileMediaType = media
		return &blk
	}

	// Anthropic document: source {url | base64}.
	src, ok := obj["source"]
	if !ok || len(src) == 0 {
		return nil
	}
	var s struct {
		Type      string `json:"type"`
		URL       string `json:"url"`
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
	}
	if err := json.Unmarshal(src, &s); err != nil {
		return nil
	}
	switch s.Type {
	case "url":
		blk.FileURL = s.URL
	case "base64":
		blk.FileData = s.Data
		blk.FileMediaType = s.MediaType
	default:
		return nil
	}
	return &blk
}

func dataURL(mediaType, data string) string {
	if data == "" {
		return ""
	}
	return "data:" + mediaType + ";base64," + data
}

// jsonStringValue returns the plain string value of a JSON string, or the raw
// compact JSON for non-string values.
func jsonStringValue(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return string(raw)
}

// contentBlocksToChat renders blocks as a Chat message content field: a plain
// string when possible, otherwise an array of part objects.
func contentBlocksToChat(blocks []ContentBlock) (json.RawMessage, error) {
	if len(blocks) == 0 {
		return json.RawMessage(`""`), nil
	}
	if len(blocks) == 1 && blocks[0].Type == "text" {
		return json.Marshal(blocks[0].Text)
	}
	parts := make([]json.RawMessage, 0, len(blocks))
	for _, b := range blocks {
		switch b.Type {
		case "text":
			p, _ := json.Marshal(map[string]any{"type": "text", "text": b.Text})
			parts = append(parts, p)
		case "image":
			if b.ImageURL != "" {
				p, _ := json.Marshal(map[string]any{
					"type": "image_url", "image_url": map[string]any{"url": b.ImageURL},
				})
				parts = append(parts, p)
			}
		}
	}
	if len(parts) == 0 {
		return json.RawMessage(`""`), nil
	}
	return json.Marshal(parts)
}
