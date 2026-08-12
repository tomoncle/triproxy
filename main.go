package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func main() {
	configPath := flag.String("config", "config.yaml", "path to config file (YAML or JSON)")
	listen := flag.String("listen", "", "listen address override (defaults to config listen or :8866)")
	flag.Parse()

	cfg, err := LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	if *listen != "" {
		cfg.Listen = *listen
	}

	srv := &http.Server{Addr: cfg.Listen, Handler: NewProxy(cfg)}
	go func() {
		log.Printf("triproxy listening on %s", cfg.Listen)
		logChannels(cfg)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
	log.Println("shutdown complete")
}

// clientPaths maps a client protocol to the path suffix exposed under
// /{alias}/v1/.
var clientPaths = map[string]string{
	ProtoOpenAIChat:        "chat/completions",
	ProtoOpenAIResponses:   "responses",
	ProtoAnthropicMessages: "messages",
}

// logChannels prints the configured channels grouped by alias, one line per
// alias for the upstream plus one line per client protocol for the local URL.
// This keeps each line short enough to fit a terminal.
func logChannels(cfg *Config) {
	base := localBaseURL(cfg.Listen)
	names := make([]string, 0, len(cfg.Aliases))
	for name := range cfg.Aliases {
		names = append(names, name)
	}
	sort.Strings(names)
	log.Printf("渠道配置 (客户端协议 -> 本地地址 ; 上游协议 -> 上游地址):")
	for _, name := range names {
		alias := cfg.Aliases[name]
		log.Printf("  [%s] 上游 %s -> %s", name, alias.Protocol, alias.Endpoint())
		for _, cp := range []string{ProtoOpenAIChat, ProtoOpenAIResponses, ProtoAnthropicMessages} {
			local := fmt.Sprintf("%s/%s/v1/%s", base, name, clientPaths[cp])
			note := ""
			if cp == alias.Protocol {
				note = "  (透传)"
			}
			log.Printf("    %-18s -> %s%s", cp, local, note)
		}
	}
}

// localBaseURL derives a displayable base URL from the listen address. When
// the proxy listens on all interfaces (":8866" / "0.0.0.0:8866") we show
// 127.0.0.1 since there is no single public host.
func localBaseURL(listen string) string {
	host := "127.0.0.1"
	port := "8866"
	if h, p, err := net.SplitHostPort(listen); err == nil {
		if h != "" && h != "0.0.0.0" && h != "::" {
			if strings.Contains(h, ":") {
				host = "[" + h + "]"
			} else {
				host = h
			}
		}
		if p != "" {
			port = p
		}
	} else if _, perr := strconv.Atoi(listen); perr == nil {
		// listen may be a bare port like "8866"
		port = listen
	}
	return "http://" + host + ":" + port
}
