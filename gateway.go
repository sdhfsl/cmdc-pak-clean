package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// forwardToGateway builds the harness request envelope and posts it to the
// gateway. ctxDirs optionally overrides the project directory used for the
// server context. threadID optionally pins the upstream thread; when empty a
// fresh UUID is generated (no prefix-cache reuse across turns). extras carries
// protocol-neutral sampling params (top_p, stop, tool_choice, ...) that are
// merged verbatim; nil values are skipped.
func forwardToGateway(ctx context.Context, auth *commandCodeAuth, model string, wireMsgs []any, system string, wireTools []any, maxTokens int, temperature any, effort string, threadID string, extras map[string]any, ctxDirs ...string) (*http.Response, error) {
	params := map[string]any{
		"model":      model,
		"messages":   wireMsgs,
		"tools":      wireTools,
		"max_tokens": maxTokens,
		"stream":     true,
	}
	if system != "" {
		params["system"] = system
	}
	// Opt-in tool-usage nudge: append a short instruction so models lean
	// toward calling tools. Off unless CMDC_PAK_TOOL_NUDGE is set; skipped
	// when the request declares no tools.
	if toolNudgeEnabled() && len(wireTools) > 0 {
		const nudge = "Always prefer using the available tools to complete the user's task. Call a tool whenever it can make the result more accurate or verifiable."
		if system == "" {
			params["system"] = nudge
		} else {
			params["system"] = system + "\n\n" + nudge
		}
	}
	if t, ok := temperature.(float64); ok {
		params["temperature"] = t
	}
	for k, v := range extras {
		if v == nil {
			continue
		}
		params[k] = v
	}
	if effort != "" && strings.ToLower(effort) != "none" {
		final := ""
		if forceEffortEnabled() {
			final = maxEffortOf(model)
		}
		if final == "" {
			final = clampEffort(model, effort)
		}
		if final == "" {
			// Model not in local catalog (no effort metadata): pass the
			// client's requested level straight through.
			final = effort
		}
		if final != "" {
			params["reasoning_effort"] = final
			logLine("%s: effort=%s (client=%s, force=%v)", model, final, effort, forceEffortEnabled())
		} else {
			logLine("%s: ignoring effort=%s (model has no effort support)", model, effort)
		}
	} else {
		if strings.ToLower(effort) == "none" {
			logLine("%s: effort=none requested, thinking disabled", model)
		} else {
			logLine("%s: no effort requested (thinking off)", model)
		}
	}

	dir := workDir
	if len(ctxDirs) > 0 && ctxDirs[0] != "" {
		dir = ctxDirs[0]
	}
	slug := projectSlug
	if dir != workDir {
		slug = filepath.Base(dir)
	}

	envelope := map[string]any{
		"config":         buildServerContext(dir),
		"memory":         nil,
		"taste":          nil,
		"skills":         nil,
		"permissionMode": "standard",
		"threadId":       threadID,
		"mode":           "agent",
		"params":         params,
	}
	envBytes, _ := json.Marshal(envelope)
	upReq, err := http.NewRequestWithContext(ctx, http.MethodPost, gatewayBaseURL()+generatePath, bytes.NewReader(envBytes))
	if err != nil {
		return nil, err
	}
	upReq.Header.Set("Content-Type", "application/json")
	upReq.Header.Set("User-Agent", "cli")
	upReq.Header.Set("x-command-code-version", cliVersionGet())
	upReq.Header.Set("x-cli-environment", "production")
	upReq.Header.Set("x-project-slug", slug)
	upReq.Header.Set("x-taste-learning", "false")
	upReq.Header.Set("x-session-id", sessionID)
	upReq.Header.Set("Authorization", "Bearer "+auth.ApiKey)
	return upstreamClient().Do(upReq)
}

// threadRegistry maps a client conversation key to a stable upstream
// threadId so consecutive turns share prefix cache. Entries expire after
// 30 minutes of disuse; the map is bounded to avoid unbounded growth.
var threadRegistry = struct {
	sync.Mutex
	m map[string]threadEntry
}{m: make(map[string]threadEntry)}

type threadEntry struct {
	id  string
	exp time.Time
}

func stableThreadID(convKey string) string {
	if convKey == "" {
		return newUUID()
	}
	threadRegistry.Lock()
	defer threadRegistry.Unlock()
	now := time.Now()
	if e, ok := threadRegistry.m[convKey]; ok && now.Before(e.exp) {
		e.exp = now.Add(30 * time.Minute)
		threadRegistry.m[convKey] = e
		return e.id
	}
	if len(threadRegistry.m) >= 512 {
		for k, e := range threadRegistry.m {
			if now.After(e.exp) {
				delete(threadRegistry.m, k)
			}
		}
		if len(threadRegistry.m) >= 512 {
			threadRegistry.m = make(map[string]threadEntry)
		}
	}
	id := newUUID()
	threadRegistry.m[convKey] = threadEntry{id: id, exp: now.Add(30 * time.Minute)}
	return id
}

// toolNudgeEnabled reports whether the opt-in tool-usage nudge is on.
// Enabled by CMDC_PAK_TOOL_NUDGE=1|on|true.
func toolNudgeEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CMDC_PAK_TOOL_NUDGE"))) {
	case "1", "on", "true", "yes":
		return true
	}
	return false
}

// scanToolChoice normalizes a client tool_choice (OpenAI chat, OpenAI
// Responses, or Anthropic shape) into the object form the gateway accepts.
// Returns nil when the value has no supported upstream equivalent ("none").
func scanToolChoice(v any) any {
	switch t := v.(type) {
	case string:
		switch t {
		case "auto":
			return map[string]any{"type": "auto"}
		case "required", "any":
			return map[string]any{"type": "any"}
		}
		return nil // "none" and anything else: no upstream equivalent
	case map[string]any:
		switch typ, _ := t["type"].(string); typ {
		case "auto", "any":
			return map[string]any{"type": typ}
		case "tool":
			if name, _ := t["name"].(string); name != "" {
				return map[string]any{"type": "tool", "name": name}
			}
			return map[string]any{"type": "any"}
		case "function":
			// OpenAI chat: {"type":"function","function":{"name":"x"}}
			if fn, ok := t["function"].(map[string]any); ok {
				if name, _ := fn["name"].(string); name != "" {
					return map[string]any{"type": "tool", "name": name}
				}
			}
			// OpenAI Responses: {"type":"function","name":"x"}
			if name, _ := t["name"].(string); name != "" {
				return map[string]any{"type": "tool", "name": name}
			}
		}
		return nil
	}
	return nil
}

// pickExtras copies supported sampling params from the client body into the
// gateway extras map, converting shapes where the protocols differ.
func pickExtras(body map[string]any, keys ...string) map[string]any {
	out := map[string]any{}
	for _, k := range keys {
		if v, ok := body[k]; ok && v != nil {
			out[k] = v
		}
	}
	if tc, ok := body["tool_choice"]; ok && tc != nil {
		if converted := scanToolChoice(tc); converted != nil {
			out["tool_choice"] = converted
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// scannerTruncated reports whether a bufio.Scanner stopped because a single
// line exceeded its buffer (8MB NDJSON line). Callers use this to surface a
// length/max_tokens finish reason instead of a fake clean stop.
func scannerTruncated(s *bufio.Scanner) bool {
	return errors.Is(s.Err(), bufio.ErrTooLong)
}

// upstreamIdleTimeout is how long an upstream stream may go without sending
// any byte before the proxy gives up. Overridable via CMDC_PAK_IDLE_TIMEOUT
// (seconds). Without it a stalled upstream would block the request goroutine
// and leak the connection until the client walks away.
func upstreamIdleTimeout() time.Duration {
	if v := os.Getenv("CMDC_PAK_IDLE_TIMEOUT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
	}
	return 180 * time.Second
}

// idleTimeoutReader closes the underlying body when no data arrives for d.
// The pending Read returns an error, which unwinds the stream loop. Close is
// safe to call concurrently (handler defer + client-gone AfterFunc).
type idleTimeoutReader struct {
	rc     io.ReadCloser
	d      time.Duration
	timer  *time.Timer
	closed atomic.Bool
}

func newIdleTimeoutReader(rc io.ReadCloser, d time.Duration) *idleTimeoutReader {
	it := &idleTimeoutReader{rc: rc, d: d}
	it.timer = time.AfterFunc(d, func() {
		logLine("upstream idle > %s, closing stream", d)
		_ = rc.Close()
	})
	return it
}

func (it *idleTimeoutReader) Read(p []byte) (int, error) {
	n, err := it.rc.Read(p)
	if n > 0 && !it.closed.Load() {
		it.timer.Reset(it.d)
	}
	return n, err
}

func (it *idleTimeoutReader) Close() error {
	it.closed.Store(true)
	it.timer.Stop()
	return it.rc.Close()
}

// conversationKey derives a stable key from client-supplied conversation
// identifiers. Falls back to "" (caller generates a fresh UUID).
func conversationKey(body map[string]any) string {
	for _, k := range []string{"conversation_id", "conversation", "session_id", "thread_id"} {
		if s, _ := body[k].(string); s != "" {
			h := sha1.Sum([]byte(s))
			return "conv:" + hex.EncodeToString(h[:8])
		}
	}
	if prev, _ := body["previous_response_id"].(string); prev != "" {
		h := sha1.Sum([]byte(prev))
		return "prev:" + hex.EncodeToString(h[:8])
	}
	return ""
}

// readClientBody reads a bounded request body and returns it as a map.
func readClientBody(r *http.Request) (map[string]any, error) {
	b, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// gatewayError writes a gateway error response to the client in a JSON shape
// each protocol lane understands.
func gatewayError(w http.ResponseWriter, status int, body []byte, shape string, model string) {
	msg := upstreamMessage(body)
	if len(msg) > 300 {
		msg = msg[:300]
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	switch shape {
	case "anthropic":
		enc, _ := json.Marshal(map[string]any{"type": "error",
			"error": map[string]any{"type": "api_error", "message": msg}})
		_, _ = w.Write(enc)
	case "responses":
		enc, _ := json.Marshal(map[string]any{"error": map[string]any{"message": msg}})
		_, _ = w.Write(enc)
	default:
		enc, _ := json.Marshal(map[string]any{
			"error": map[string]any{"message": msg, "type": "proxy_error", "code": status}})
		_, _ = w.Write(enc)
	}
	logLine("gateway error model=%s -> %d %s", model, status, msg)
}
