package main

import (
	"fmt"
	"net/url"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Protocols we can speak on either side (client-facing or upstream). The
// canonical names are vendor-prefixed; the short aliases (chat/responses/
// messages) are accepted in config and normalized to these.
const (
	ProtoOpenAIChat        = "openai-chat"
	ProtoOpenAIResponses   = "openai-responses"
	ProtoAnthropicMessages = "anthropic-messages"
)

// DefaultMaxTokens is injected when a request is converted to the Anthropic
// Messages protocol, which requires max_tokens (OpenAI Chat/Responses do not).
const DefaultMaxTokens = 4096

// Config is the top-level service configuration.
type Config struct {
	Listen string `yaml:"listen" json:"listen"`
	// Debug logs every outgoing upstream request (URL + headers, secrets
	// redacted). Useful to verify client-impersonation headers reach upstream.
	Debug bool `yaml:"debug" json:"debug"`
	// Proxy is the default upstream proxy for all aliases (HTTP/HTTPS or
	// SOCKS5). Per-alias Proxy overrides it.
	Proxy   string            `yaml:"proxy" json:"proxy"`
	Aliases map[string]*Alias `yaml:"aliases" json:"aliases"`
}

// Alias describes one upstream provider reachable at /{name}/v1/...
//
// Upstream is the base URL (path optional). Protocol is the protocol the
// upstream speaks: "openai-chat" (OpenAI Chat Completions), "openai-responses"
// (OpenAI Responses), or "anthropic-messages" (Anthropic Messages). The short
// aliases chat/responses/messages are also accepted. When protocol is empty it
// defaults to openai-chat.
type Alias struct {
	Upstream string `yaml:"upstream" json:"upstream"`
	Protocol string `yaml:"protocol" json:"protocol"`
	// Path overrides the endpoint path sent to the upstream. Defaults to the
	// standard path for the configured protocol.
	Path string `yaml:"path" json:"path"`
	// MaxConcurrency caps how many requests to this alias may be in flight to
	// the upstream at once. 0 (default) means unlimited.
	MaxConcurrency int `yaml:"max_concurrency" json:"max_concurrency"`
	// ConcurrencyMode decides what happens when the limit is reached:
	//   "reject" (default) - respond 429 immediately, protecting the upstream;
	//   "queue"            - wait for a free slot (up to the client timeout).
	ConcurrencyMode string `yaml:"concurrency_mode" json:"concurrency_mode"`
	// Headers sets extra HTTP headers sent to this upstream (e.g. to present
	// as a specific client: User-Agent, x-app, anthropic-beta). Applied after
	// protocol adaptation, so they win. Empty by default (no impersonation).
	Headers map[string]string `yaml:"headers" json:"headers"`
	// Thinking enables the reasoning bridge for this alias: OpenAI Responses
	// reasoning items ↔ Anthropic thinking blocks in both directions (request +
	// response + streaming). Off by default: reasoning/thinking content is
	// dropped, which is safe for upstreams that do not support Anthropic
	// extended thinking. When enabled the proxy maps reasoning.effort to a
	// thinking budget, injects the anthropic-beta: thinking-2024-12-04 header
	// for Messages upstreams, and round-trips reasoning items losslessly through
	// thinking signatures.
	Thinking bool `yaml:"thinking" json:"thinking"`
	// Proxy overrides the global proxy for this alias. Supports http/https/
	// socks5/socks5h. Empty means "use the global proxy (or none)".
	Proxy string `yaml:"proxy" json:"proxy"`
}

// UnmarshalYAML accepts both the simple "llm -> https://chat.llm.com" string
// form and the explicit object form. For the string form the upstream protocol
// is inferred from the URL path when it is recognizable, otherwise "chat".
func (a *Alias) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		var s string
		if err := node.Decode(&s); err != nil {
			return err
		}
		a.Upstream = s
		a.Protocol = inferProtocolFromPath(s)
		return nil
	}
	type raw Alias
	var r raw
	if err := node.Decode(&r); err != nil {
		return err
	}
	*a = Alias(r)
	return nil
}

func inferProtocolFromPath(upstream string) string {
	p := strings.ToLower(upstream)
	switch {
	case strings.Contains(p, "/v1/messages") || strings.HasSuffix(p, "/messages"):
		return ProtoAnthropicMessages
	case strings.Contains(p, "/v1/responses") || strings.HasSuffix(p, "/responses"):
		return ProtoOpenAIResponses
	default:
		return ProtoOpenAIChat
	}
}

// normalizeProtocol maps the accepted spellings (short and vendor-prefixed) to
// the canonical names. Unknown values are returned unchanged so Validate can
// reject them with a clear message.
func normalizeProtocol(p string) string {
	switch strings.ToLower(strings.TrimSpace(p)) {
	case "openai-chat", "chat":
		return ProtoOpenAIChat
	case "openai-responses", "responses":
		return ProtoOpenAIResponses
	case "anthropic-messages", "messages":
		return ProtoAnthropicMessages
	default:
		return p
	}
}

// Endpoint returns the full upstream URL for chat-completion-like requests.
func (a *Alias) Endpoint() string {
	base := strings.TrimRight(a.Upstream, "/")
	switch a.Protocol {
	case ProtoOpenAIResponses:
		return base + defaultPath(a.Path, "/v1/responses")
	case ProtoAnthropicMessages:
		return base + defaultPath(a.Path, "/v1/messages")
	default:
		return base + defaultPath(a.Path, "/v1/chat/completions")
	}
}

// ModelsEndpoint returns the upstream URL for GET /v1/models passthrough.
func (a *Alias) ModelsEndpoint() string {
	return strings.TrimRight(a.Upstream, "/") + "/v1/models"
}

func defaultPath(p, def string) string {
	if p == "" {
		return def
	}
	if !strings.HasPrefix(p, "/") {
		return "/" + p
	}
	return p
}

// LoadConfig reads a YAML or JSON config file. JSON is valid YAML, so the same
// parser handles both.
func LoadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if cfg.Listen == "" {
		cfg.Listen = ":8866"
	}
	return &cfg, nil
}

func (c *Config) Validate() error {
	if len(c.Aliases) == 0 {
		return fmt.Errorf("config has no aliases")
	}
	if err := validateProxy("global", c.Proxy); err != nil {
		return err
	}
	for name, a := range c.Aliases {
		if a == nil {
			return fmt.Errorf("alias %q is empty", name)
		}
		if a.Upstream == "" {
			return fmt.Errorf("alias %q: upstream is required", name)
		}
		if a.Protocol == "" {
			a.Protocol = ProtoOpenAIChat
		} else {
			a.Protocol = normalizeProtocol(a.Protocol)
		}
		switch a.Protocol {
		case ProtoOpenAIChat, ProtoOpenAIResponses, ProtoAnthropicMessages:
			// ok
		default:
			return fmt.Errorf("alias %q: unsupported protocol %q (want openai-chat|openai-responses|anthropic-messages; short forms chat|responses|messages also accepted)", name, a.Protocol)
		}
		switch a.ConcurrencyMode {
		case "":
			if a.MaxConcurrency > 0 {
				a.ConcurrencyMode = "reject"
			}
		case "reject", "queue":
			// ok
		default:
			return fmt.Errorf("alias %q: unsupported concurrency_mode %q (want reject|queue)", name, a.ConcurrencyMode)
		}
		if err := validateProxy(name, a.Proxy); err != nil {
			return err
		}
	}
	return nil
}

// validateProxy checks that a proxy value is a supported URL scheme.
func validateProxy(name, raw string) error {
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%s proxy %q invalid: %w", name, raw, err)
	}
	switch u.Scheme {
	case "http", "https", "socks5", "socks5h":
		return nil
	default:
		return fmt.Errorf("%s proxy %q: unsupported scheme %q (want http|https|socks5|socks5h)", name, raw, u.Scheme)
	}
}
