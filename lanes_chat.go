package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

func contentString(c any) string {
	switch v := c.(type) {
	case string:
		return v
	case []any:
		var sb strings.Builder
		for _, p := range v {
			if pm, ok := p.(map[string]any); ok {
				if t, _ := pm["type"].(string); t == "text" {
					if s, _ := pm["text"].(string); s != "" {
						sb.WriteString(s)
						sb.WriteString("\n")
					}
				}
			}
		}
		return strings.TrimRight(sb.String(), "\n")
	}
	return ""
}

func mimeFromDataURI(uri string) string {
	rest := strings.TrimPrefix(uri, "data:")
	if i := strings.Index(rest, ";"); i > 0 {
		return rest[:i]
	}
	if i := strings.Index(rest, ","); i > 0 {
		return rest[:i]
	}
	return ""
}

func isPublicImageURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("image fetch: unsupported scheme %q", u.Scheme)
	}
	if err := blockPrivateHost(strings.ToLower(u.Hostname())); err != nil {
		return err
	}
	return nil
}

func blockPrivateHost(h string) error {
	if h == "" || h == "localhost" || h == "127.0.0.1" || h == "::1" || h == "::" ||
		h == "0.0.0.0" || strings.HasSuffix(h, ".local") || strings.HasSuffix(h, ".localhost") ||
		strings.HasSuffix(h, ".internal") || strings.HasSuffix(h, ".lan") ||
		strings.HasPrefix(h, "10.") || strings.HasPrefix(h, "192.168.") ||
		strings.HasPrefix(h, "169.254.") || strings.HasPrefix(h, "100.64.") ||
		strings.HasPrefix(h, "192.0.2.") || strings.HasPrefix(h, "198.51.100.") ||
		strings.HasPrefix(h, "203.0.113.") {
		return fmt.Errorf("image fetch: private host blocked")
	}
	if strings.HasPrefix(h, "172.") {
		rest := strings.TrimPrefix(h, "172.")
		if i := strings.Index(rest, "."); i > 0 {
			if n, err := strconv.Atoi(rest[:i]); err == nil && n >= 16 && n <= 31 {
				return fmt.Errorf("image fetch: private host blocked")
			}
		}
	}
	if ip := net.ParseIP(h); ip != nil {
		if ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() ||
			ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() {
			return fmt.Errorf("image fetch: private IP blocked")
		}
	}
	return nil
}

func resolveAndCheckHost(host string, port string) error {
	if err := blockPrivateHost(strings.ToLower(host)); err != nil {
		return err
	}
	addrs, err := net.DefaultResolver.LookupIPAddr(context.Background(), host)
	if err != nil || len(addrs) == 0 {
		return fmt.Errorf("image fetch: DNS resolve failed for %q", host)
	}
	_ = port
	for _, a := range addrs {
		if err := blockPrivateHost(strings.ToLower(a.IP.String())); err != nil {
			return fmt.Errorf("image fetch: resolved private IP blocked")
		}
	}
	return nil
}

func imageFetchClient() *http.Client {
	return &http.Client{Timeout: 15 * time.Second, Transport: upstreamClient().Transport}
}

func fetchImageDataURI(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	if err := isPublicImageURL(rawURL); err != nil {
		return "", err
	}
	if err := resolveAndCheckHost(u.Hostname(), u.Port()); err != nil {
		return "", err
	}
	client := imageFetchClient()
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return fmt.Errorf("image fetch: too many redirects")
		}
		return isPublicImageURL(req.URL.String())
	}
	resp, err := client.Get(rawURL)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("image fetch status %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return "", err
	}
	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = http.DetectContentType(b)
	}
	return "data:" + ct + ";base64," + base64.StdEncoding.EncodeToString(b), nil
}

func openAIContentToWire(content any) []any {
	out := []any{}
	switch v := content.(type) {
	case string:
		if v != "" {
			out = append(out, map[string]any{"type": "text", "text": v})
		}
	case []any:
		for _, p := range v {
			pm, ok := p.(map[string]any)
			if !ok {
				continue
			}
			switch pt, _ := pm["type"].(string); pt {
			case "text":
				if s, _ := pm["text"].(string); s != "" {
					out = append(out, map[string]any{"type": "text", "text": s})
				}
			case "image_url":
				iu, _ := pm["image_url"].(map[string]any)
				uri, _ := iu["url"].(string)
				if uri == "" {
					continue
				}
				if strings.HasPrefix(uri, "data:") {
					out = append(out, map[string]any{"type": "image", "image": uri, "mimeType": mimeFromDataURI(uri)})
				} else if strings.HasPrefix(uri, "http://") || strings.HasPrefix(uri, "https://") {
					dataURI, err := fetchImageDataURI(uri)
					if err != nil {
						logLine("image fetch failed (not forwarded to model): %v", err)
						out = append(out, map[string]any{"type": "text", "text": "（图片加载失败，已跳过）"})
					} else {
						out = append(out, map[string]any{"type": "image", "image": dataURI, "mimeType": mimeFromDataURI(dataURI)})
					}
				}
			}
		}
	}
	return out
}

func buildWireMessages(msgs []any) ([]any, string, error) {
	var systemParts []string
	out := []any{}
	toolNameByID := map[string]string{}
	for _, raw := range msgs {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		role, _ := m["role"].(string)
		switch role {
		case "system", "developer":
			if s := contentString(m["content"]); s != "" {
				systemParts = append(systemParts, s)
			}
		case "user":
			blocks := openAIContentToWire(m["content"])
			if len(blocks) > 0 {
				out = append(out, map[string]any{"role": "user", "content": blocks})
			}
		case "assistant":
			blocks := openAIContentToWire(m["content"])
			// Preserve the assistant's prior thinking so multi-turn tool
			// loops keep the reasoning context (DeepSeek/Z Code style
			// reasoning_content, or a plain "reasoning" string).
			if r, _ := m["reasoning_content"].(string); r != "" {
				blocks = append([]any{map[string]any{"type": "reasoning", "text": r}}, blocks...)
			} else if r, _ := m["reasoning"].(string); r != "" {
				blocks = append([]any{map[string]any{"type": "reasoning", "text": r}}, blocks...)
			}
			if tcs, ok := m["tool_calls"].([]any); ok {
				for _, tcRaw := range tcs {
					tc, _ := tcRaw.(map[string]any)
					id, _ := tc["id"].(string)
					fn, _ := tc["function"].(map[string]any)
					tname, _ := fn["name"].(string)
					argsStr, _ := fn["arguments"].(string)
					var input any
					if err := json.Unmarshal([]byte(argsStr), &input); err != nil || input == nil {
						input = map[string]any{"raw": argsStr}
					}
					toolNameByID[id] = tname
					blocks = append(blocks, map[string]any{"type": "tool-call", "toolCallId": id, "toolName": tname, "input": input})
				}
			}
			if len(blocks) > 0 {
				out = append(out, map[string]any{"role": "assistant", "content": blocks})
			}
		case "tool":
			tcID, _ := m["tool_call_id"].(string)
			tname := toolNameByID[tcID]
			if tname == "" {
				tname = "unknown"
			}
			outType := "text"
			if e, _ := m["is_error"].(bool); e {
				outType = "error-text"
			}
			out = append(out, map[string]any{"role": "tool", "content": []any{
				map[string]any{"type": "tool-result", "toolCallId": tcID, "toolName": tname,
					"output": map[string]any{"type": outType, "value": contentString(m["content"])}},
			}})
		}
	}
	return out, strings.Join(systemParts, "\n\n"), nil
}

func buildWireTools(tools []any) []any {
	out := []any{}
	for _, raw := range tools {
		t, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		fn, _ := t["function"].(map[string]any)
		if fn == nil {
			continue
		}
		name, _ := fn["name"].(string)
		if strings.TrimSpace(name) == "" {
			continue
		}
		desc, _ := fn["description"].(string)
		wire := map[string]any{"name": name, "description": desc}
		if schema, ok := fn["parameters"]; ok && schema != nil {
			wire["input_schema"] = schema
		} else {
			wire["input_schema"] = emptySchema()
		}
		out = append(out, wire)
	}
	return out
}

// ---- OpenAI SSE translation ----

type sseMessage struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"`
	Created int64          `json:"created"`
	Model   string         `json:"model"`
	Choices []sseChoice    `json:"choices"`
	Usage   map[string]any `json:"usage,omitempty"`
}

type sseChoice struct {
	Index        int            `json:"index"`
	Delta        map[string]any `json:"delta,omitempty"`
	FinishReason any            `json:"finish_reason"`
}

func writeSSE(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", b)
	return err
}

func mapFinishReason(raw string) string {
	switch raw {
	case "tool-calls", "tool_calls":
		return "tool_calls"
	case "length":
		return "length"
	default:
		return "stop"
	}
}

func usageFromEvent(ev map[string]any) map[string]any {
	total, ok := ev["totalUsage"].(map[string]any)
	if !ok {
		return nil
	}
	inTok, _ := total["inputTokens"].(float64)
	outTok, _ := total["outputTokens"].(float64)
	return map[string]any{
		"prompt_tokens":     int(inTok),
		"completion_tokens": int(outTok),
		"total_tokens":      int(inTok + outTok),
	}
}

// streamChatSSE relays gateway NDJSON as OpenAI SSE chunks.
func streamChatSSE(ctx context.Context, w http.ResponseWriter, body io.Reader, chatID, model string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	created := time.Now().Unix()
	started := false
	toolIdx := 0

	emit := func(delta map[string]any, finish any, usage map[string]any) error {
		if !started {
			d := map[string]any{"role": "assistant"}
			for k, v := range delta {
				d[k] = v
			}
			delta = d
			started = true
		}
		chunk := sseMessage{ID: chatID, Object: "chat.completion.chunk", Created: created, Model: model,
			Choices: []sseChoice{{Index: 0, Delta: delta, FinishReason: finish}}, Usage: usage}
		return writeSSE(w, chunk)
	}
	done := func() {
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		flusher.Flush()
	}

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			logLine("client disconnected, aborting upstream stream (model=%s)", model)
			return
		default:
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" || !strings.HasPrefix(line, "{") {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		switch typ, _ := ev["type"].(string); typ {
		case "text-delta":
			if t := evText(ev); t != "" {
				if err := emit(map[string]any{"content": t}, nil, nil); err != nil {
					return
				}
				flusher.Flush()
			}
		case "reasoning-delta":
			if t := evText(ev); t != "" {
				if err := emit(map[string]any{"reasoning_content": t}, nil, nil); err != nil {
					return
				}
				flusher.Flush()
			}
		case "tool-call":
			tname, _ := ev["toolName"].(string)
			tcID, _ := ev["toolCallId"].(string)
			delta := map[string]any{"tool_calls": []any{map[string]any{
				"index": toolIdx, "id": tcID, "type": "function",
				"function": map[string]any{"name": tname, "arguments": toolCallArgs(ev)},
			}}}
			toolIdx++
			if err := emit(delta, nil, nil); err != nil {
				return
			}
			flusher.Flush()
		case "finish":
			rawFinish, _ := ev["rawFinishReason"].(string)
			_ = emit(map[string]any{}, mapFinishReason(rawFinish), usageFromEvent(ev))
			flusher.Flush()
			done()
			return
		case "error":
			msg := eventErrorText(ev)
			if msg == "" {
				msg = "upstream error"
			}
			logLine("upstream stream error: %s", msg)
			_ = emit(map[string]any{"content": ""}, "error", nil)
			flusher.Flush()
			done()
			return
		case "abort":
			_ = emit(map[string]any{}, "stop", nil)
			flusher.Flush()
			done()
			return
		}
	}
	if err := scanner.Err(); err != nil {
		logLine("upstream stream broken (model=%s): %v", model, err)
	}
	finish := any("stop")
	if scannerTruncated(scanner) {
		logLine("upstream stream truncated 8MB line (model=%s): reporting length", model)
		finish = "length"
	}
	_ = emit(map[string]any{}, finish, nil)
	flusher.Flush()
	done()
}

// assembleChatJSON buffers NDJSON into a full chat.completion body.
func assembleChatJSON(ctx context.Context, w http.ResponseWriter, body io.Reader, chatID, model string) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	var text, reasoning strings.Builder
	var toolCalls []any
	finishReason := ""
	var usage map[string]any

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return
		default:
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" || !strings.HasPrefix(line, "{") {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		switch typ, _ := ev["type"].(string); typ {
		case "text-delta":
			text.WriteString(evText(ev))
		case "reasoning-delta":
			reasoning.WriteString(evText(ev))
		case "tool-call":
			tname, _ := ev["toolName"].(string)
			tcID, _ := ev["toolCallId"].(string)
			toolCalls = append(toolCalls, map[string]any{
				"id": tcID, "type": "function",
				"function": map[string]any{"name": tname, "arguments": toolCallArgs(ev)},
			})
		case "finish":
			rawFinish, _ := ev["rawFinishReason"].(string)
			finishReason = mapFinishReason(rawFinish)
			usage = usageFromEvent(ev)
		case "error":
			if msg := eventErrorText(ev); msg != "" {
				logLine("upstream stream error: %s", msg)
			}
		}
	}
	message := map[string]any{"role": "assistant", "content": text.String()}
	if reasoning.Len() > 0 {
		message["reasoning_content"] = reasoning.String()
	}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
	}
	if finishReason == "" {
		finishReason = "stop"
	}
	if scannerTruncated(scanner) {
		logLine("upstream body truncated 8MB line (model=%s): reporting length", model)
		finishReason = "length"
	}
	resp := map[string]any{
		"id": chatID, "object": "chat.completion", "created": time.Now().Unix(), "model": model,
		"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finishReason}},
	}
	if usage != nil {
		resp["usage"] = usage
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// handleChat implements OpenAI /v1/chat/completions on the gateway lane.
func handleChat(w http.ResponseWriter, r *http.Request) {
	auth, authPath, err := loadLocalCommandCodeAuth()
	if err != nil {
		proxyError(w, 401, err.Error())
		logLine("%s %s -> 401 no local auth (%v) authPath=%s", r.Method, r.URL.Path, err, authPath)
		return
	}
	bodyMap, err := readClientBody(r)
	if err != nil {
		proxyError(w, 400, "invalid json")
		return
	}
	model, _ := bodyMap["model"].(string)
	model = resolveModel(model)
	if model == "" {
		proxyError(w, 400, "missing model")
		return
	}
	msgs, _ := bodyMap["messages"].([]any)
	wireMsgs, system, err := buildWireMessages(msgs)
	if err != nil {
		proxyError(w, 400, err.Error())
		return
	}
	if len(wireMsgs) == 0 {
		proxyError(w, 400, "no user/assistant/tool messages")
		return
	}
	toolsRaw, _ := bodyMap["tools"].([]any)
	wireTools := buildWireTools(toolsRaw)

	maxTokens := 64000
	clientMaxTokens := false
	if v, ok := bodyMap["max_tokens"].(float64); ok {
		if v > 0 {
			maxTokens = int(v)
			clientMaxTokens = true
		} else {
			proxyError(w, 400, "max_tokens must be > 0")
			return
		}
	} else if v, ok := bodyMap["max_completion_tokens"].(float64); ok {
		if v > 0 {
			maxTokens = int(v)
			clientMaxTokens = true
		} else {
			proxyError(w, 400, "max_completion_tokens must be > 0")
			return
		}
	}

	effort := ""
	if v, ok := bodyMap["reasoning_effort"].(string); ok {
		effort = v
	}
	// 思考与正文共用配额：开启思考时统一抬升至 128000（上游上限）。
	if raisedMax, raised := ensureThinkingBudget(maxTokens, clientMaxTokens, effort); raised {
		logLine("%s: max_tokens %d too small for thinking, raised to %d", model, maxTokens, raisedMax)
		maxTokens = raisedMax
	}
	// 上游参数校验上限：客户端超出安全上限的配额一律钳制。
	if capped := capMaxTokens(maxTokens); capped != maxTokens {
		logLine("%s: max_tokens %d exceeds upstream cap, clamped to %d", model, maxTokens, capped)
		maxTokens = capped
	}

	chatDir := ctxDir(msgs)
	extras := pickExtras(bodyMap, "top_p", "stop", "seed",
		"presence_penalty", "frequency_penalty", "parallel_tool_calls")
	upResp, err := forwardToGateway(r.Context(), auth, model, wireMsgs, system, wireTools, maxTokens, bodyMap["temperature"], effort, stableThreadID(conversationKey(bodyMap)), extras, chatDir)
	if err != nil {
		proxyError(w, 502, "upstream error: "+err.Error())
		logLine("%s %s -> 502 %v", r.Method, r.URL.Path, err)
		return
	}
	defer upResp.Body.Close()
	if upResp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(upResp.Body, 1<<20))
		gatewayError(w, upResp.StatusCode, respBody, "openai", model)
		return
	}
	upBody := newIdleTimeoutReader(upResp.Body, upstreamIdleTimeout())
	defer upBody.Close()
	// Tear the upstream stream down as soon as the client goes away (user
	// pressed stop) instead of waiting for the idle timeout to fire.
	stopUpstream := context.AfterFunc(r.Context(), func() {
		logLine("client gone, aborting upstream stream (model=%s)", model)
		upBody.Close()
	})
	defer stopUpstream()
	chatID := "chatcmpl-" + randID(12)
	stream := true
	if v, ok := bodyMap["stream"].(bool); ok {
		stream = v
	}
	if stream {
		streamChatSSE(r.Context(), w, upBody, chatID, model)
	} else {
		assembleChatJSON(r.Context(), w, upBody, chatID, model)
	}
	logLine("%s %s model=%s stream=%v -> 200", r.Method, r.URL.Path, model, stream)
}
