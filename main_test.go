package main

import (
	"bytes"
	"log"
	"strings"
	"testing"
)

func TestLocalBaseURL(t *testing.T) {
	cases := map[string]string{
		":8866":          "http://127.0.0.1:8866",
		"0.0.0.0:8866":   "http://127.0.0.1:8866",
		"127.0.0.1:9000": "http://127.0.0.1:9000",
		"localhost:9000": "http://localhost:9000",
		"8866":           "http://127.0.0.1:8866",
		"[::1]:7000":     "http://[::1]:7000",
	}
	for listen, want := range cases {
		if got := localBaseURL(listen); got != want {
			t.Errorf("localBaseURL(%q) = %q, want %q", listen, got, want)
		}
	}
}

// TestLogChannelsAligned verifies the per-client-protocol lines (one per alias)
// all have their " -> " separator at the same column, so the multi-line
// channel log stays aligned.
func TestLogChannelsAligned(t *testing.T) {
	cfg := &Config{Listen: ":8866", Aliases: map[string]*Alias{
		"llm": {Upstream: "http://u1", Protocol: ProtoOpenAIChat},
		"abc": {Upstream: "http://u2", Protocol: ProtoAnthropicMessages},
	}}
	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(old)
	logChannels(cfg)

	arrowPos := -1
	for _, ln := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		// 跳过表头和每个 alias 的上游行
		if strings.Contains(ln, "渠道配置") || strings.Contains(ln, "上游") {
			continue
		}
		a := strings.Index(ln, " -> ")
		if a < 0 {
			t.Fatalf("separator missing in line: %q", ln)
		}
		if arrowPos == -1 {
			arrowPos = a
			continue
		}
		if a != arrowPos {
			t.Fatalf("列未对齐: %q (arrow@%d want %d)", ln, a, arrowPos)
		}
	}
	if arrowPos < 0 {
		t.Fatal("no data rows captured")
	}
}
