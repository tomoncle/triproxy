package main

import (
	"os"
	"path/filepath"
	"testing"
)

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadConfigObjectForm(t *testing.T) {
	p := writeTemp(t, `
listen: ":8899"
aliases:
  llm:
    upstream: "https://chat.llm.com"
    protocol: chat
  abc:
    upstream: "http://api.opencode.com"
    protocol: messages
`)
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != ":8899" {
		t.Fatalf("listen = %q", cfg.Listen)
	}
	if cfg.Aliases["llm"].Protocol != ProtoOpenAIChat {
		t.Fatalf("llm protocol = %q", cfg.Aliases["llm"].Protocol)
	}
	if cfg.Aliases["llm"].Endpoint() != "https://chat.llm.com/v1/chat/completions" {
		t.Fatalf("llm endpoint = %q", cfg.Aliases["llm"].Endpoint())
	}
	if cfg.Aliases["abc"].Endpoint() != "http://api.opencode.com/v1/messages" {
		t.Fatalf("abc endpoint = %q", cfg.Aliases["abc"].Endpoint())
	}
	if cfg.Aliases["abc"].ModelsEndpoint() != "http://api.opencode.com/v1/models" {
		t.Fatalf("abc models endpoint = %q", cfg.Aliases["abc"].ModelsEndpoint())
	}
}

func TestLoadConfigStringShorthand(t *testing.T) {
	p := writeTemp(t, `
aliases:
  claude: "https://api.anthropic.com/v1/messages"
  openai: "https://api.openai.com/v1/chat/completions"
  codex: "https://api.openai.com/v1/responses"
  plain: "https://example.com"
`)
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Aliases["claude"].Protocol != ProtoAnthropicMessages {
		t.Fatalf("claude protocol = %q", cfg.Aliases["claude"].Protocol)
	}
	if cfg.Aliases["openai"].Protocol != ProtoOpenAIChat {
		t.Fatalf("openai protocol = %q", cfg.Aliases["openai"].Protocol)
	}
	if cfg.Aliases["codex"].Protocol != ProtoOpenAIResponses {
		t.Fatalf("codex protocol = %q", cfg.Aliases["codex"].Protocol)
	}
	if cfg.Aliases["plain"].Protocol != ProtoOpenAIChat {
		t.Fatalf("plain protocol = %q (want default chat)", cfg.Aliases["plain"].Protocol)
	}
}

func TestLoadConfigDefaultsAndErrors(t *testing.T) {
	p := writeTemp(t, `aliases:
  x:
    upstream: "http://localhost:1"
`)
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != ":8866" {
		t.Fatalf("default listen = %q", cfg.Listen)
	}
	if cfg.Aliases["x"].Protocol != ProtoOpenAIChat {
		t.Fatalf("default protocol = %q", cfg.Aliases["x"].Protocol)
	}

	p2 := writeTemp(t, `aliases:
  bad:
    upstream: "http://x"
    protocol: websocket
`)
	if _, err := LoadConfig(p2); err == nil {
		t.Fatal("expected error for unsupported protocol")
	}

	p3 := writeTemp(t, `aliases:
  empty: {}
`)
	if _, err := LoadConfig(p3); err == nil {
		t.Fatal("expected error for empty alias")
	}
}

func TestLoadConfigJSON(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	if err := os.WriteFile(p, []byte(`{"listen":":9000","aliases":{"a":{"upstream":"https://u.example","protocol":"messages"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != ":9000" || cfg.Aliases["a"].Endpoint() != "https://u.example/v1/messages" {
		t.Fatalf("json config parse failed: %+v", cfg)
	}
}

func TestLoadConfigMaxConcurrency(t *testing.T) {
	p := writeTemp(t, `aliases:
  a:
    upstream: "http://x"
    protocol: chat
    max_concurrency: 4
  b:
    upstream: "http://y"
    protocol: messages
    max_concurrency: 2
    concurrency_mode: queue
`)
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Aliases["a"].MaxConcurrency != 4 {
		t.Fatalf("a.max_concurrency = %d", cfg.Aliases["a"].MaxConcurrency)
	}
	if cfg.Aliases["a"].ConcurrencyMode != "reject" {
		t.Fatalf("a.concurrency_mode default = %q (want reject)", cfg.Aliases["a"].ConcurrencyMode)
	}
	if cfg.Aliases["b"].ConcurrencyMode != "queue" {
		t.Fatalf("b.concurrency_mode = %q", cfg.Aliases["b"].ConcurrencyMode)
	}
}

func TestLoadConfigInvalidConcurrencyMode(t *testing.T) {
	p := writeTemp(t, `aliases:
  x:
    upstream: "http://z"
    max_concurrency: 1
    concurrency_mode: bogus
`)
	if _, err := LoadConfig(p); err == nil {
		t.Fatal("expected error for invalid concurrency_mode")
	}
}

func TestProtocolNormalization(t *testing.T) {
	p := writeTemp(t, `aliases:
  a: { upstream: "http://a", protocol: chat }
  b: { upstream: "http://b", protocol: openai-chat }
  c: { upstream: "http://c", protocol: responses }
  d: { upstream: "http://d", protocol: openai-responses }
  e: { upstream: "http://e", protocol: messages }
  f: { upstream: "http://f", protocol: anthropic-messages }
`)
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"a": ProtoOpenAIChat, "b": ProtoOpenAIChat,
		"c": ProtoOpenAIResponses, "d": ProtoOpenAIResponses,
		"e": ProtoAnthropicMessages, "f": ProtoAnthropicMessages,
	}
	for name, w := range want {
		if got := cfg.Aliases[name].Protocol; got != w {
			t.Fatalf("alias %q protocol = %q (want %q)", name, got, w)
		}
	}
}

func TestLoadConfigHeaders(t *testing.T) {
	p := writeTemp(t, `aliases:
  llm:
    upstream: "http://x"
    protocol: anthropic-messages
    headers:
      User-Agent: "claude-cli/2.1.221 (external, cli)"
      x-app: "cli"
      anthropic-beta: "claude-code-20250219"
`)
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	h := cfg.Aliases["llm"].Headers
	if h["User-Agent"] != "claude-cli/2.1.221 (external, cli)" {
		t.Fatalf("User-Agent header = %q", h["User-Agent"])
	}
	if h["x-app"] != "cli" || h["anthropic-beta"] != "claude-code-20250219" {
		t.Fatalf("headers = %v", h)
	}
}

func TestLoadConfigProxy(t *testing.T) {
	p := writeTemp(t, `proxy: "http://127.0.0.1:7890"
aliases:
  a:
    upstream: "http://x"
    protocol: openai-chat
    proxy: "socks5://127.0.0.1:1080"
  b:
    upstream: "http://y"
    protocol: openai-chat
`)
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Proxy != "http://127.0.0.1:7890" {
		t.Fatalf("global proxy = %q", cfg.Proxy)
	}
	if cfg.Aliases["a"].Proxy != "socks5://127.0.0.1:1080" {
		t.Fatalf("alias proxy = %q", cfg.Aliases["a"].Proxy)
	}
	if cfg.Aliases["b"].Proxy != "" {
		t.Fatalf("alias b proxy should be empty (uses global)")
	}

	// 非法 scheme 应报错
	for _, bad := range []string{`ftp://bad`, `tcp://host:1`, `not-a-url::`} {
		p2 := writeTemp(t, `aliases:
  x:
    upstream: "http://z"
    proxy: "`+bad+`"
`)
		if _, err := LoadConfig(p2); err == nil {
			t.Fatalf("expected error for proxy %q", bad)
		}
	}
}
