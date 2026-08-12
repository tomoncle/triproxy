package main

import (
	"encoding/base64"
	"encoding/json"
	"strings"
)

// Reasoning bridge between OpenAI Responses reasoning items and Anthropic
// thinking blocks, modeled on the same technique used by cc-switch:
//
//   - Responses `reasoning` items have no direct Messages field. A reasoning
//     item that carries opaque `encrypted_content` is encoded into a versioned
//     base64 envelope and carried in a thinking block's `signature` (or in a
//     `redacted_thinking` block's `data` when there is no visible summary), so
//     the item survives a full round trip and can be restored exactly.
//   - Items with only a visible summary become plain `thinking` blocks.
//   - Native thinking blocks (whose signature is not one of our envelopes) are
//     not losslessly reversible to a reasoning item; they surface as a summary
//     reasoning item.
//
// The envelope prefix is ours, not cc-switch's, so the two never collide.
const reasoningItemPrefix = "triproxy-reasoning-v1:"

// reasoningBudgetForEffort maps a Responses reasoning.effort to an Anthropic
// extended-thinking budget. effort "xhigh" maps to adaptive thinking (budget 0).
func reasoningBudgetForEffort(effort string) (budget int, adaptive bool) {
	switch effort {
	case "low", "minimal":
		return 2048, false
	case "medium":
		return 8000, false
	case "high":
		return 32000, false
	case "xhigh":
		return 0, true
	default:
		return 0, false
	}
}

// effortFromThinking maps Anthropic thinking config back to a Responses effort.
func effortFromThinking(budget int, adaptive bool) string {
	if adaptive {
		return "xhigh"
	}
	switch {
	case budget <= 4096:
		return "low"
	case budget <= 16384:
		return "medium"
	default:
		return "high"
	}
}

// reasoningSummaryText extracts the visible summary from a Responses reasoning
// item (summary part types "summary_text" or "reasoning_text").
func reasoningSummaryText(raw json.RawMessage) string {
	var item struct {
		Summary []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"summary"`
	}
	if err := json.Unmarshal(raw, &item); err != nil {
		return ""
	}
	var parts []string
	for _, s := range item.Summary {
		if s.Type == "summary_text" || s.Type == "reasoning_text" {
			parts = append(parts, s.Text)
		}
	}
	return strings.Join(parts, "")
}

// reasoningItemHasEncryptedContent reports whether the item carries opaque
// encrypted reasoning that must round-trip losslessly.
func reasoningItemHasEncryptedContent(raw json.RawMessage) bool {
	var item struct {
		EncryptedContent string `json:"encrypted_content"`
	}
	if err := json.Unmarshal(raw, &item); err != nil {
		return false
	}
	return item.EncryptedContent != ""
}

func encodeReasoningItem(raw json.RawMessage) (string, bool) {
	var item struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &item); err != nil || item.Type != "reasoning" {
		return "", false
	}
	return reasoningItemPrefix + base64.RawURLEncoding.EncodeToString(raw), true
}

func decodeReasoningItem(text string) (json.RawMessage, bool) {
	if !strings.HasPrefix(text, reasoningItemPrefix) {
		return nil, false
	}
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(text, reasoningItemPrefix))
	if err != nil {
		return nil, false
	}
	var item struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(b, &item); err != nil || item.Type != "reasoning" {
		return nil, false
	}
	return b, true
}

// messagesThinkingBlock renders a canonical reasoning block as an Anthropic
// content block. The second return value is false when the block has nothing
// expressible (no summary, no envelope, not redacted).
func messagesThinkingBlock(b ContentBlock) (map[string]any, bool) {
	if b.ReasoningRaw != nil {
		if reasoningItemHasEncryptedContent(b.ReasoningRaw) {
			env, _ := encodeReasoningItem(b.ReasoningRaw)
			if b.ReasoningSummary != "" {
				return map[string]any{"type": "thinking", "thinking": b.ReasoningSummary, "signature": env}, true
			}
			return map[string]any{"type": "redacted_thinking", "data": env}, true
		}
		if b.ReasoningSummary != "" {
			return map[string]any{"type": "thinking", "thinking": b.ReasoningSummary}, true
		}
		return nil, false
	}
	if b.ThinkingIsRedacted {
		return map[string]any{"type": "redacted_thinking", "data": ""}, true
	}
	if b.ThinkingSignature != "" {
		return map[string]any{"type": "thinking", "thinking": b.ReasoningSummary, "signature": b.ThinkingSignature}, true
	}
	if b.ReasoningSummary != "" {
		return map[string]any{"type": "thinking", "thinking": b.ReasoningSummary}, true
	}
	return nil, false
}

// responsesReasoningItem renders a canonical reasoning block as a Responses
// reasoning output/input item. Items that carry the full raw item are restored
// exactly; native thinking becomes a summary item.
func responsesReasoningItem(b ContentBlock) (map[string]any, bool) {
	if b.ReasoningRaw != nil {
		var item map[string]any
		if json.Unmarshal(b.ReasoningRaw, &item) == nil {
			return item, true
		}
	}
	item := map[string]any{
		"type":    "reasoning",
		"id":      genID("rs"),
		"summary": []any{map[string]any{"type": "summary_text", "text": b.ReasoningSummary}},
	}
	if b.ThinkingIsRedacted && b.ReasoningSummary == "" {
		item["summary"] = []any{}
		item["encrypted_content"] = "opaque"
	}
	return item, true
}

// reasoningBlockFromItem builds a canonical reasoning block from a Responses
// reasoning item (raw bytes preserved for lossless round-trip).
func reasoningBlockFromItem(raw json.RawMessage) ContentBlock {
	return ContentBlock{
		Type:             "reasoning",
		ReasoningRaw:     raw,
		ReasoningSummary: reasoningSummaryText(raw),
	}
}
