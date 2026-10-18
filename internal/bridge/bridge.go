// Package bridge relays MCP between a stdio harness and a running wakil-mcp
// daemon.
//
// Some harnesses can only launch a child process and speak MCP over that
// process's standard input and output. This package is that child: it reads
// newline-delimited JSON-RPC from stdin, sends each message to the daemon over
// Streamable HTTP, and writes the reply back to stdout.
//
// The relay is deliberately frame-level rather than a second MCP server. It
// does not re-publish tools under its own names, does not reimplement the
// handshake, and does not interpret capabilities. Whatever the harness says
// reaches the daemon unchanged, so a harness cannot tell the difference between
// talking to the bridge and talking to the daemon directly.
package bridge

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// Options configures a bridge run.
type Options struct {
	// URL is the daemon's Streamable HTTP endpoint.
	URL string
	// Token is the bearer token to present. Empty sends no Authorization
	// header, which is what a daemon with authentication switched off wants.
	Token string
	// ProtocolVersion pins the protocol revision sent on requests that do not
	// carry one. Empty lets the handshake decide.
	ProtocolVersion string
	// PollEvery is how often the daemon is asked whether its tool list
	// changed. A stdio harness cannot be pushed notifications the way an HTTP
	// client can, so the bridge polls and translates a change into
	// notifications/tools/list_changed.
	PollEvery time.Duration
	// Logger receives diagnostics. It must not write to stdout, which is the
	// protocol channel.
	Logger *slog.Logger
	// In is the harness's message stream. Defaults to stdin.
	In io.Reader
	// Out is the harness's response stream. Defaults to stdout.
	Out io.Writer
	// HTTPClient overrides the client used for daemon requests.
	HTTPClient *http.Client
}

// bridge carries the mutable state of one run.
type bridge struct {
	opts Options
	log  *slog.Logger
	out  *bufio.Writer

	client *http.Client

	// writeMu serialises writes to the harness, because replies and
	// list-changed notifications come from different goroutines.
	writeMu sync.Mutex

	// mu guards the fields below, which the poller and the relay both touch.
	mu              sync.Mutex
	protocolVersion string
	sessionID       string

	// toolsSeen is the tool list the harness was last given. A change is
	// reported relative to what the harness actually saw, so a change that
	// happens between the handshake and the first poll is not swallowed.
	toolsSeen string
	haveTools bool
}

// Run relays messages until the input stream ends or ctx is cancelled.
func Run(ctx context.Context, opts Options) error {
	if opts.URL == "" {
		return errors.New("a daemon URL is required")
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.In == nil {
		opts.In = os.Stdin
	}
	if opts.Out == nil {
		opts.Out = os.Stdout
	}
	if opts.PollEvery == 0 {
		opts.PollEvery = 15 * time.Second
	}

	client := opts.HTTPClient
	if client == nil {
		// No client timeout: a tool call can legitimately take a long time,
		// and the daemon enforces its own call timeout.
		client = &http.Client{}
	}

	b := &bridge{
		opts:            opts,
		log:             opts.Logger,
		out:             bufio.NewWriter(opts.Out),
		client:          client,
		protocolVersion: opts.ProtocolVersion,
	}

	// The poller has its own cancellation so that Run still returns promptly
	// when the harness closes stdin: waiting on the caller's context would
	// block until the harness was shut down instead.
	pollerCtx, stopPoller := context.WithCancel(ctx)
	defer stopPoller()

	pollerDone := make(chan struct{})
	go func() {
		defer close(pollerDone)
		b.pollLoop(pollerCtx)
	}()

	err := b.relay(ctx)

	// Stop the poller before returning, so nothing writes to the harness after
	// Run reports that it is finished.
	stopPoller()
	<-pollerDone

	return err
}

// relay reads messages from the harness and hands each to the daemon.
func (b *bridge) relay(ctx context.Context) error {
	scanner := bufio.NewScanner(b.opts.In)
	// Tool arguments can be large, so the default 64 KiB line limit is not
	// enough for a harness that sends a document.
	scanner.Buffer(make([]byte, 0, 64*1024), maxFrameBytes)

	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}

		// Copy: the scanner reuses its buffer.
		message := make([]byte, len(line))
		copy(message, line)

		if err := b.forward(ctx, message); err != nil {
			// A transport failure must be reported to the harness as a
			// JSON-RPC error rather than killing the process, otherwise the
			// harness sees a dead child and reports a crash instead of a
			// useful message.
			b.fail(message, err)
		}
	}

	if err := scanner.Err(); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, io.EOF) {
			return nil
		}
		return fmt.Errorf("reading from the harness: %w", err)
	}
	return nil
}

// forward sends one message to the daemon and writes back what it returns.
//
// Each message is a separate HTTP request. That is exactly how a session-less
// Streamable HTTP client behaves, which is why the relay can be this simple.
func (b *bridge) forward(ctx context.Context, message []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.opts.URL, bytes.NewReader(message))
	if err != nil {
		return err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if b.opts.Token != "" {
		req.Header.Set("Authorization", "Bearer "+b.opts.Token)
	}

	b.mu.Lock()
	version := b.protocolVersion
	session := b.sessionID
	b.mu.Unlock()

	// The daemon requires the version header on any request that carries the
	// version in _meta, and pins the session when it issues one.
	if v := metaProtocolVersion(message); v != "" {
		version = v
	}
	if version != "" {
		req.Header.Set("Mcp-Protocol-Version", version)
	}
	if session != "" {
		req.Header.Set("Mcp-Session-Id", session)
	}

	resp, err := b.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if id := resp.Header.Get("Mcp-Session-Id"); id != "" {
		b.mu.Lock()
		b.sessionID = id
		b.mu.Unlock()
	}

	if resp.StatusCode == http.StatusAccepted || resp.StatusCode == http.StatusNoContent {
		// A notification was accepted. There is nothing to send back.
		return nil
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("the daemon returned %s: %s", resp.Status, summarise(body))
	}

	return b.deliver(resp.Header.Get("Content-Type"), body)
}

// deliver writes the daemon's reply to the harness, in whichever framing the
// daemon chose.
func (b *bridge) deliver(contentType string, body []byte) error {
	if !strings.Contains(contentType, "text/event-stream") {
		return b.writeMessage(bytes.TrimSpace(body))
	}

	// An event stream carries the response, and may also carry notifications
	// and server-initiated requests emitted while handling the call.
	for _, payload := range sseData(body) {
		if err := b.writeMessage(payload); err != nil {
			return err
		}
	}
	return nil
}

// writeMessage appends one JSON-RPC message to the harness's stream.
func (b *bridge) writeMessage(payload []byte) error {
	if len(payload) == 0 {
		return nil
	}

	// Learn the negotiated revision from the handshake so later requests can
	// name it.
	if v := resultProtocolVersion(payload); v != "" {
		b.mu.Lock()
		b.protocolVersion = v
		b.mu.Unlock()
	}

	// Remember what the harness is being told, so the poller can report a
	// change against it.
	b.noteTools(payload)

	b.writeMu.Lock()
	defer b.writeMu.Unlock()

	if _, err := b.out.Write(payload); err != nil {
		return err
	}
	if err := b.out.WriteByte('\n'); err != nil {
		return err
	}
	return b.out.Flush()
}

// fail reports a transport failure back to the harness as a JSON-RPC error, so
// a daemon outage surfaces as a failed call rather than a vanished process.
func (b *bridge) fail(request []byte, cause error) {
	b.log.Error("relaying to the daemon failed", "error", cause)

	var envelope struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	if err := json.Unmarshal(request, &envelope); err != nil {
		return
	}
	// Notifications get no reply, by definition.
	if len(envelope.ID) == 0 || string(envelope.ID) == "null" {
		return
	}

	reply := map[string]any{
		"jsonrpc": "2.0",
		"id":      json.RawMessage(envelope.ID),
		"error": map[string]any{
			"code":    -32001,
			"message": "wakil-mcp is unreachable: " + cause.Error(),
		},
	}
	encoded, err := json.Marshal(reply)
	if err != nil {
		return
	}
	_ = b.writeMessage(encoded)
}

// pollLoop watches the daemon's tool list and tells the harness when it
// changes. A stdio harness cannot be pushed a notification over HTTP, so the
// bridge is where that translation has to happen.
func (b *bridge) pollLoop(ctx context.Context) {
	ticker := time.NewTicker(b.opts.PollEvery)
	defer ticker.Stop()

	// Wait for the first interval before the first poll: the handshake has
	// just returned, so there is nothing to compare against yet.
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			b.checkToolList(ctx)
		}
	}
}

// checkToolList asks the daemon for its tool list and emits a notification if
// it differs from the last one observed.
func (b *bridge) checkToolList(ctx context.Context) {
	callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	request := []byte(`{"jsonrpc":"2.0","id":"wakil-bridge-poll","method":"tools/list"}`)
	req, err := http.NewRequestWithContext(callCtx, http.MethodPost, b.opts.URL, bytes.NewReader(request))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if b.opts.Token != "" {
		req.Header.Set("Authorization", "Bearer "+b.opts.Token)
	}

	b.mu.Lock()
	version := b.protocolVersion
	session := b.sessionID
	b.mu.Unlock()
	if version != "" {
		req.Header.Set("Mcp-Protocol-Version", version)
	}
	if session != "" {
		req.Header.Set("Mcp-Session-Id", session)
	}

	resp, err := b.client.Do(req)
	if err != nil {
		b.log.Debug("tool list poll failed", "error", err)
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxFrameBytes))
	if err != nil {
		return
	}
	if resp.StatusCode/100 != 2 {
		b.log.Debug("tool list poll was rejected", "status", resp.Status)
		return
	}

	var payload []byte
	if strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		for _, chunk := range sseData(body) {
			if bytes.Contains(chunk, []byte(`"tools"`)) {
				payload = chunk
			}
		}
	} else {
		payload = bytes.TrimSpace(body)
	}
	if len(payload) == 0 {
		return
	}

	// Compare the tool definitions, not the raw bytes, so a daemon that
	// reorders its list without changing it does not wake the harness.
	snapshot, ok := canonicalTools(payload)
	if !ok {
		return
	}

	b.mu.Lock()
	previous, seen := b.toolsSeen, b.haveTools
	b.toolsSeen = snapshot
	b.haveTools = true
	b.mu.Unlock()

	if !seen || snapshot == previous {
		return
	}

	b.log.Debug("the daemon's tool list changed; notifying the harness")
	notification := []byte(`{"jsonrpc":"2.0","method":"notifications/tools/list_changed"}`)
	_ = b.writeMessage(notification)
}

// noteTools records a tool list the harness has just been shown.
func (b *bridge) noteTools(payload []byte) {
	snapshot, ok := canonicalTools(payload)
	if !ok {
		return
	}

	b.mu.Lock()
	b.toolsSeen = snapshot
	b.haveTools = true
	b.mu.Unlock()
}

// metaProtocolVersion extracts params._meta.protocolVersion from a message.
func metaProtocolVersion(message []byte) string {
	var envelope struct {
		Params struct {
			Meta struct {
				ProtocolVersion string `json:"protocolVersion"`
			} `json:"_meta"`
		} `json:"params"`
	}
	if err := json.Unmarshal(message, &envelope); err != nil {
		return ""
	}
	return envelope.Params.Meta.ProtocolVersion
}

// resultProtocolVersion extracts result.protocolVersion from a handshake reply.
func resultProtocolVersion(message []byte) string {
	var envelope struct {
		Result struct {
			ProtocolVersion string `json:"protocolVersion"`
		} `json:"result"`
	}
	if err := json.Unmarshal(message, &envelope); err != nil {
		return ""
	}
	return envelope.Result.ProtocolVersion
}

// canonicalTools renders a tools/list reply as a stable string, so two replies
// that describe the same tools compare equal. The boolean reports whether the
// payload was a tools/list reply at all, which distinguishes an empty list from
// an unrelated message.
func canonicalTools(payload []byte) (string, bool) {
	var envelope struct {
		Result struct {
			Tools json.RawMessage `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return "", false
	}
	if len(envelope.Result.Tools) == 0 {
		return "", false
	}
	var tools []map[string]any
	if err := json.Unmarshal(envelope.Result.Tools, &tools); err != nil {
		return "", false
	}

	// Only the fields that affect what the harness can call. Descriptions and
	// schemas are included because a changed schema changes the tool.
	keys := make([]string, 0, len(tools))
	for _, tool := range tools {
		name, _ := tool["name"].(string)
		parts := []any{tool["description"], tool["inputSchema"], tool["title"]}
		digest, err := json.Marshal(parts)
		if err != nil {
			continue
		}
		keys = append(keys, name+"\x00"+string(digest))
	}
	sort.Strings(keys)

	joined, err := json.Marshal(keys)
	if err != nil {
		return "", false
	}
	return string(joined), true
}

// sseData extracts the data payloads from a server-sent event stream.
func sseData(body []byte) [][]byte {
	var out [][]byte

	var data bytes.Buffer
	flush := func() {
		if data.Len() == 0 {
			return
		}
		payload := bytes.TrimSpace(data.Bytes())
		if len(payload) > 0 {
			copied := make([]byte, len(payload))
			copy(copied, payload)
			out = append(out, copied)
		}
		data.Reset()
	}

	for _, line := range bytes.Split(body, []byte("\n")) {
		line = bytes.TrimRight(line, "\r")
		if len(line) == 0 {
			// A blank line ends the event.
			flush()
			continue
		}
		if rest, ok := bytes.CutPrefix(line, []byte("data:")); ok {
			// Multiple data lines in one event are joined with newlines.
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.Write(bytes.TrimPrefix(rest, []byte(" ")))
		}
	}
	flush()

	return out
}

// summarise shortens a response body for an error message.
func summarise(body []byte) string {
	text := strings.TrimSpace(string(body))
	if text == "" {
		return "(no body)"
	}
	if len(text) > 500 {
		return text[:500] + "..."
	}
	return text
}

// maxFrameBytes bounds a single protocol frame in either direction. It matches
// the daemon's request body limit, so a call that the daemon would accept is
// not rejected here first.
const maxFrameBytes = 32 << 20
