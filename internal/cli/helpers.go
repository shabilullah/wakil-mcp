package cli

import (
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/shabilullah/wakil-mcp/internal/aggregate"
	"github.com/shabilullah/wakil-mcp/internal/config"
	"github.com/shabilullah/wakil-mcp/internal/upstream"
)

// previewIndex renders the names the daemon will publish for one server.
//
// It runs the real aggregator rather than approximating it, so a preview cannot
// drift from what a harness actually sees.
func previewIndex(defaults config.ServerDefaults, key string, up *config.Upstream, tools []upstream.Tool) *aggregate.Index {
	builder := aggregate.NewBuilder(defaults)
	builder.AddServer(key, up, tools)
	return builder.Build()
}

// parseDuration accepts Go duration syntax and returns it as a config value.
func parseDuration(text string) (config.Duration, error) {
	parsed, err := time.ParseDuration(strings.TrimSpace(text))
	if err != nil {
		return 0, fmt.Errorf("expected a duration such as 30s, 5m or 1h: %w", err)
	}
	if parsed < 0 {
		return 0, fmt.Errorf("a duration cannot be negative: %s", text)
	}
	return config.Duration(parsed), nil
}

// sortedKeys returns a map's keys in sorted order.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// splitPair splits a "left:right" argument, which is how the allow flags take
// a server-and-tool pair without having to guess where a tool name ends.
func splitPair(value string) (left, right string, err error) {
	left, right, ok := strings.Cut(value, ":")
	if !ok || strings.TrimSpace(left) == "" || strings.TrimSpace(right) == "" {
		return "", "", fmt.Errorf("expected SERVER:PATTERN, got %q", value)
	}
	return strings.TrimSpace(left), strings.TrimSpace(right), nil
}

// isLoopbackBind reports whether an address only accepts local connections.
func isLoopbackBind(bind string) bool {
	host, _, err := net.SplitHostPort(bind)
	if err != nil {
		host = bind
	}
	host = strings.Trim(strings.TrimSpace(host), "[]")
	if host == "" || host == "0.0.0.0" || host == "::" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// plural renders a count with a noun, for messages that read as sentences.
func plural(count int, singular string) string {
	if count == 1 {
		return strconv.Itoa(count) + " " + singular
	}
	return strconv.Itoa(count) + " " + singular + "s"
}

// wrap joins text into lines of at most width characters.
func wrap(text string, width int) []string {
	words := strings.Fields(text)
	if len(words) == 0 {
		return nil
	}

	var lines []string
	current := words[0]
	for _, word := range words[1:] {
		if len(current)+1+len(word) > width {
			lines = append(lines, current)
			current = word
			continue
		}
		current += " " + word
	}
	return append(lines, current)
}
