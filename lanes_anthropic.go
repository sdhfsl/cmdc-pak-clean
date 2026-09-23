package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

func anthropicSystemToString(v any) string {
	switch s := v.(type) {
	case string:
		return s
	case []any:
		var sb strings.Builder
		for _, p := range s {
			if pm, ok := p.(map[string]any); ok {
				if t, _ := pm["type"].(string); t == "text" {
					if x, _ := pm["text"].(string); x != "" {
						sb.WriteString(x)
						sb.WriteString("\n")
					}
				}
			}
		}
		return strings.TrimRight(sb.String(), "\n")
	}
	return ""
}

// anthropicMessagesToWire converts Anthropic messages to the gateway wire
// format. Blocks from the same message stay in one wire message so a tool
// call keeps its surrounding text; tool results live in their own role:"tool"
// message.
func anthropicMessagesToWire(msgs []any) []any {
	out := []any{}
	toolNameByID := map[string]string{}
	for _, raw := range msgs {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		role, _ := m["role"].(string)
		if role == "" {
			logLine("anthropic lane: dropping message with empty role")
			continue
		}
		var blocks []any
		flush := func() {
			if len(blocks) > 0 {
				out = append(out, map[string]any{"role": role, "content": blocks})
				blocks = nil
			}
		}
		switch c := m["content"].(type) {
		case string:
			if c != "" {
				blocks = append(blocks, map[string]any{"type": "text", "text": c})
			}
		case []any:
			for _, p := range c {
				pm, ok2 := p.(map[string]any)
				if !ok2 {
					continue
				}
				switch pt, _ := pm["type"].(string); pt {
				case "text":
					if s, _ := pm["text"].(string); s != "" {
						blocks = append(blocks, map[string]any{"type": "text", "text": s})
					}
				case "image":
					src, _ := pm["source"].(map[string]any)
					mt, _ := src["media_type"].(string)
					data, _ := src["data"].(string)
					if mt != "" && data != "" {
						blocks = append(blocks, map[string]any{"type": "image",
							"image": "data:" + mt + ";base64," + data, "mimeType": mt})
					}
				case "tool_use":
					id, _ := pm["id"].(string)
					name, _ := pm["name"].(string)
					toolNameByID[id] = name
					blocks = append(blocks, map[string]any{"type": "tool-call",
						"toolCallId": id, "toolName": name, "input": pm["input"]})
				case "tool_result":
					flush()
					tid, _ := pm["tool_use_id"].(string)
					tname := toolNameByID[tid]
					if tname == "" {
						tname = "unknown"
					}
					out = append(out, map[string]any{"role": "tool",
						"content": []any{anthropicToolResultBlock(tid, tname, pm)}})
				case "thinking":
					if s, _ := pm["thinking"].(string); s != "" {
						blocks = append(blocks, map[string]any{"type": "reasoning", "text": s})
					}
				}
			}
		}
		flush()
	}
	return out
}

// anthropicToolResultBlock converts an Anthropic tool_result block into the
// gateway wire tool-result shape. Only the text parts are forwarded (images
// dropped), using type "text" — or "error-text" when is_error is set. This
// matches the desktop harness (toWireToolOutput) exactly: the gateway's
// ModelMessage schema rejects richer output shapes ("content").
func anthropicToolResultBlock(toolCallID, toolName string, pm map[string]any) map[string]any {
	isErr, _ := pm["is_error"].(bool)
	text := anthropicToolResultText(pm)
	outType := "text"
	if isErr {
		outType = "error-text"
	}
	return map[string]any{"type": "tool-result",
		"toolCallId": toolCallID, "toolName": toolName,
		"output": map[string]any{"type": outType, "value": text}}
}

// anthropicToolResultText extracts the text value of a tool_result block's
// content field, mirroring official toWireToolOutput (text parts joined).
func anthropicToolResultText(pm map[string]any) string {
	switch cc := pm["content"].(type) {
	case string:
		return cc
	case []any:
		var sb strings.Builder
		for _, q := range cc {
			qm, ok := q.(map[string]any)
			if !ok {
				continue
			}
			if t, _ := qm["type"].(string); t == "text" {
				if s, _ := qm["text"].(string); s != "" {
					sb.WriteString(s)
					sb.WriteString("\n")
				}
			}
		}
		return strings.TrimRight(sb.String(), "\n")
	case map[string]any:
		if t, _ := cc["type"].(string); t == "text" {
			if s, _ := cc["text"].(string); s != "" {
				return s
			}
		}
	}
	return ""
}

func mapAnthropicStop(raw string) string {
	switch raw {
	case "tool-calls", "tool_calls":
		return "tool_use"
	case "length":
		return "max_tokens"
	default:
		return "end_turn"
	}
}

func writeAnthropicEvent(w io.Writer, event string, payload any) {
	b, err := json.Marshal(payload)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
}

// streamAnthropicSSE converts gateway NDJSON into Anthropic Messages SSE.
func streamAnthropicSSE(ctx context.Context, w http.ResponseWriter, body io.Reader, model string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	writeAnthropicEvent(w, "message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": "msg_" + randID(12), "type": "message", "role": "assistant", "model": model,
			"content": []any{}, "stop_reason": nil, "usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
		},
	})
	flusher.Flush()

	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	blockIndex := -1
	current := ""
	var outTok int
	var thinkN, textN int
	defer func() {
		if thinkN == 0 {
			logLine("anthropic stream done: NO thinking deltas text=%d", textN)
		} else {
			logLine("anthropic stream done: thinking=%d text=%d deltas", thinkN, textN)
		}
	}()

	startBlock := func(kind string) {
		blockIndex++
		if kind == "thinking" {
			current = "thinking"
			writeAnthropicEvent(w, "content_block_start", map[string]any{
				"type": "content_block_start", "index": blockIndex,
				"content_block": map[string]any{"type": "thinking", "thinking": ""},
			})
		} else {
			current = "text"
			writeAnthropicEvent(w, "content_block_start", map[string]any{
				"type": "content_block_start", "index": blockIndex,
				"content_block": map[string]any{"type": "text", "text": ""},
			})
		}
		flusher.Flush()
	}
	stopBlock := func() {
		if current != "" {
			writeAnthropicEvent(w, "content_block_stop", map[string]any{
				"type": "content_block_stop", "index": blockIndex,
			})
			flusher.Flush()
			current = ""
		}
	}
	finishUp := func(stop string) {
		stopBlock()
		writeAnthropicEvent(w, "message_delta", map[string]any{
			"type":  "message_delta",
			"delta": map[string]any{"stop_reason": stop},
			"usage": map[string]any{"output_tokens": outTok},
		})
		writeAnthropicEvent(w, "message_stop", map[string]any{"type": "message_stop"})
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
		case "reasoning-start":
			if current == "" {
				startBlock("thinking")
			}
		case "reasoning-delta":
			if t := evText(ev); t != "" {
				thinkN++
				if current != "thinking" {
					stopBlock()
					startBlock("thinking")
				}
				writeAnthropicEvent(w, "content_block_delta", map[string]any{
					"type": "content_block_delta", "index": blockIndex,
					"delta": map[string]any{"type": "thinking_delta", "thinking": t},
				})
				flusher.Flush()
			}
		case "reasoning-end":
			stopBlock()
		case "text-delta":
			if t := evText(ev); t != "" {
				textN++
				if current != "text" {
					stopBlock()
					startBlock("text")
				}
				writeAnthropicEvent(w, "content_block_delta", map[string]any{
					"type": "content_block_delta", "index": blockIndex,
					"delta": map[string]any{"type": "text_delta", "text": t},
				})
				flusher.Flush()
			}
		case "tool-call":
			stopBlock()
			tname, _ := ev["toolName"].(string)
			tcID, _ := ev["toolCallId"].(string)
			blockIndex++
			writeAnthropicEvent(w, "content_block_start", map[string]any{
				"type": "content_block_start", "index": blockIndex,
				"content_block": map[string]any{"type": "tool_use", "id": tcID, "name": tname, "input": map[string]any{}},
			})
			args := toolCallArgs(ev)
			if strings.TrimSpace(args) == "" {
				args = "{}"
			}
			writeAnthropicEvent(w, "content_block_delta", map[string]any{
				"type": "content_block_delta", "index": blockIndex,
				"delta": map[string]any{"type": "input_json_delta", "partial_json": args},
			})
			writeAnthropicEvent(w, "content_block_stop", map[string]any{
				"type": "content_block_stop", "index": blockIndex,
			})
			flusher.Flush()
		case "finish":
			if total, ok := ev["totalUsage"].(map[string]any); ok {
				if f, ok2 := total["outputTokens"].(float64); ok2 {
					outTok = int(f)
				}
			}
			rawFinish, _ := ev["rawFinishReason"].(string)
			finishUp(mapAnthropicStop(rawFinish))
			return
		case "error":
			msg := eventErrorText(ev)
			if msg == "" {
				msg = "upstream error"
			}
			logLine("anthropic lane upstream error: %s", msg)
			// Anthropic 没有 error 的 stop_reason：按异常收尾，已开的块由
			// finishUp 正常关闭，错误文本追加为最终文本块以便客户端可见。
			// If we are mid-thinking block, close it first: a text_delta must
			// never land inside a thinking block.
			if current != "text" {
				stopBlock()
				startBlock("text")
			}
			writeAnthropicEvent(w, "content_block_delta", map[string]any{
				"type": "content_block_delta", "index": blockIndex,
				"delta": map[string]any{"type": "text_delta", "text": "\n\n[upstream error: " + msg + "]"},
			})
			flusher.Flush()
			finishUp("end_turn")
			return
		}
	}
	if err := scanner.Err(); err != nil {
		logLine("upstream stream broken (model=%s): %v", model, err)
	}
	stop := "end_turn"
	if scannerTruncated(scanner) {
		logLine("upstream stream truncated 8MB line (model=%s): reporting max_tokens", model)
		stop = "max_tokens"
	}
	finishUp(stop)
}

// assembleAnthropicJSON buffers NDJSON into a full Anthropic message body.
func assembleAnthropicJSON(ctx context.Context, w http.ResponseWriter, body io.Reader, model string) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	content := []any{}
	thinkingBuf := &strings.Builder{}
	textBuf := &strings.Builder{}
	stopReason := "end_turn"
	var inTok, outTok float64

	flushThinking := func() {
		if thinkingBuf.Len() > 0 {
			content = append(content, map[string]any{"type": "thinking", "thinking": thinkingBuf.String()})
			thinkingBuf.Reset()
		}
	}
	flushText := func() {
		if textBuf.Len() > 0 {
			content = append(content, map[string]any{"type": "text", "text": textBuf.String()})
			textBuf.Reset()
		}
	}
	toolInput := func(args string) any {
		if strings.TrimSpace(args) == "" {
			return map[string]any{}
		}
		var v any
		if err := json.Unmarshal([]byte(args), &v); err != nil || v == nil {
			return map[string]any{}
		}
		return v
	}

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
		case "reasoning-delta":
			flushText()
			thinkingBuf.WriteString(evText(ev))
		case "reasoning-end":
			flushThinking()
		case "text-delta":
			flushThinking()
			textBuf.WriteString(evText(ev))
		case "tool-call":
			flushThinking()
			flushText()
			tname, _ := ev["toolName"].(string)
			tcID, _ := ev["toolCallId"].(string)
			content = append(content, map[string]any{"type": "tool_use", "id": tcID, "name": tname,
				"input": toolInput(toolCallArgs(ev))})
		case "finish":
			rawFinish, _ := ev["rawFinishReason"].(string)
			stopReason = mapAnthropicStop(rawFinish)
			if total, ok := ev["totalUsage"].(map[string]any); ok {
				inTok, _ = total["inputTokens"].(float64)
				outTok, _ = total["outputTokens"].(float64)
			}
		case "error":
			if msg := eventErrorText(ev); msg != "" {
				logLine("anthropic lane upstream error: %s", msg)
			}
		}
	}
	flushThinking()
	flushText()
	if scannerTruncated(scanner) {
		logLine("upstream body truncated 8MB line (model=%s): reporting max_tokens", model)
		stopReason = "max_tokens"
	}
	resp := map[string]any{
		"id": "msg_" + randID(12), "type": "message", "role": "assistant", "model": model,
		"content": content, "stop_reason": stopReason,
		"usage": map[string]any{"input_tokens": int(inTok), "output_tokens": int(outTok)},
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// handleMessages implements Anthropic Messages API on the gateway lane.
func handleMessages(w http.ResponseWriter, r *http.Request) {
	auth, _, err := loadLocalCommandCodeAuth()
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"no local auth"}}`))
		return
	}
	bodyMap, err := readClientBody(r)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"invalid json"}}`))
		return
	}
	model, _ := bodyMap["model"].(string)
	model = resolveModel(model)
	if model == "" {
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"missing model"}}`))
		return
	}
	msgs, _ := bodyMap["messages"].([]any)
	wireMsgs := anthropicMessagesToWire(msgs)
	system := anthropicSystemToString(bodyMap["system"])
	// fold any system/developer role inside messages into the system prompt
	sysFromMsgs := []string{}
	kept := make([]any, 0, len(wireMsgs))
	for _, wm := range wireMsgs {
		if m, ok := wm.(map[string]any); ok {
			if rr, _ := m["role"].(string); rr == "system" || rr == "developer" {
				for _, b := range toAnySlice(m["content"]) {
					if bm, ok2 := b.(map[string]any); ok2 {
						if s, _ := bm["text"].(string); s != "" {
							sysFromMsgs = append(sysFromMsgs, s)
						}
					}
				}
				continue
			}
		}
		kept = append(kept, wm)
	}
	wireMsgs = kept
	if len(sysFromMsgs) > 0 {
		extra := strings.Join(sysFromMsgs, "\n\n")
		if system == "" {
			system = extra
		} else {
			system += "\n\n" + extra
		}
	}
	if len(wireMsgs) == 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"no user/assistant messages"}}`))
		return
	}
	toolsRaw, _ := bodyMap["tools"].([]any)
	wireTools := []any{}
	for _, t := range toolsRaw {
		if tm, ok := t.(map[string]any); ok {
			name, _ := tm["name"].(string)
			if strings.TrimSpace(name) == "" {
				continue
			}
			desc, _ := tm["description"].(string)
			wire := map[string]any{"name": name, "description": desc}
			if schema, ok := tm["input_schema"]; ok && schema != nil {
				wire["input_schema"] = schema
			} else {
				wire["input_schema"] = emptySchema()
			}
			wireTools = append(wireTools, wire)
		}
	}
	maxTokens := 64000
	clientMaxTokens := false
	if v, ok := bodyMap["max_tokens"].(float64); ok {
		if v > 0 {
			maxTokens = int(v)
			clientMaxTokens = true
		} else {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"max_tokens must be > 0"}}`))
			return
		}
	}
	effort := ""
	if v, ok := bodyMap["reasoning_effort"].(string); ok {
		effort = v
	} else if v, ok := bodyMap["effort"].(string); ok {
		effort = v
	} else if oc, ok := bodyMap["output_config"].(map[string]any); ok {
		if v, ok2 := oc["effort"].(string); ok2 {
			effort = v
		}
	}
	// Anthropic-native thinking switch: honor an explicit disable.
	thinkingDisabled := false
	if th, ok := bodyMap["thinking"].(map[string]any); ok {
		if t, _ := th["type"].(string); t == "disabled" {
			thinkingDisabled = true
		}
	}
	// Anthropic 通道默认启用该模型支持的最大思考强度（思考内容 1:1 透传），
	// 目录里没有该模型时直传 "max" 给网关。三类例外：
	//   - 客户端显式关闭思考（reasoning_effort=none 或 thinking.disabled）：尊重其意图；
	//   - 小配额请求（标题/摘要等辅助调用）：保持思考关闭，避免为此类
	//     请求白跑一轮重思考，浪费时间和额度。
	switch {
	case thinkingDisabled:
		logLine("%s: thinking disabled via thinking.type=disabled", model)
	case effort == "":
		if clientMaxTokens && maxTokens <= auxTokenThreshold {
			logLine("%s: small max_tokens=%d, keeping thinking off (auxiliary request)", model, maxTokens)
		} else if top := maxEffortOf(model); top != "" {
			effort = top
		} else {
			effort = "max"
		}
	case strings.EqualFold(effort, "none"):
		logLine("%s: thinking explicitly disabled by client", model)
	default:
		// Explicit level from the client: keep it (forwardToGateway may
		// raise it to the model's top rung).
	}
	// 思考与正文共用 max_tokens 配额：开启思考时统一抬升至 128000（上游上限），
	// 避免思考吃光配额导致正文被截断（finishReason=length）。
	if raisedMax, raised := ensureThinkingBudget(maxTokens, clientMaxTokens, effort); raised {
		logLine("%s: max_tokens %d too small for thinking, raised to %d", model, maxTokens, raisedMax)
		maxTokens = raisedMax
	}
	// 上游参数校验上限：客户端超出安全上限的配额一律钳制。
	if capped := capMaxTokens(maxTokens); capped != maxTokens {
		logLine("%s: max_tokens %d exceeds upstream cap, clamped to %d", model, maxTokens, capped)
		maxTokens = capped
	}

	if p := projectDirFromMessages(msgs); p != "" {
		logLine("context dir from request: %s", p)
	}
	extras := pickExtras(bodyMap, "top_p", "top_k")
	if v, ok := bodyMap["stop_sequences"]; ok && v != nil {
		if extras == nil {
			extras = map[string]any{}
		}
		extras["stop"] = v
	}
	upResp, err := forwardToGateway(r.Context(), auth, model, wireMsgs, system, wireTools, maxTokens, bodyMap["temperature"], effort, stableThreadID(conversationKeyFor(bodyMap, toolsRaw)), extras, ctxDir(msgs))
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(502)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"api_error","message":"upstream error"}}`))
		logLine("anthropic lane -> 502 %v", err)
		return
	}
	defer upResp.Body.Close()
	if upResp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(upResp.Body, 1<<20))
		gatewayError(w, upResp.StatusCode, respBody, "anthropic", model)
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
	stream := true
	if v, ok := bodyMap["stream"].(bool); ok {
		stream = v
	}
	if stream {
		streamAnthropicSSE(r.Context(), w, upBody, model)
	} else {
		assembleAnthropicJSON(r.Context(), w, upBody, model)
	}
	logLine("POST /v1/messages model=%s stream=%v -> 200", model, stream)
}
