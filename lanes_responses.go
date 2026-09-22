package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
)

func responsesInputToWire(input any) ([]any, string) {
	out := []any{}
	var sysParts []string
	items := toAnySlice(input)
	toolNameByID := map[string]string{}
	for _, raw := range items {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		switch typ, _ := m["type"].(string); typ {
		case "message", "":
			role, _ := m["role"].(string)
			if role == "" {
				role = "user"
			}
			if role == "system" {
				for _, p := range toAnySlice(m["content"]) {
					if pm, ok2 := p.(map[string]any); ok2 {
						if s, _ := pm["text"].(string); s != "" {
							sysParts = append(sysParts, s)
						}
					}
				}
				if s, ok := m["content"].(string); ok && s != "" {
					sysParts = append(sysParts, s)
				}
				continue
			}
			if role == "developer" {
				continue
			}
			blocks := []any{}
			if s, ok := m["content"].(string); ok {
				if s != "" {
					blocks = append(blocks, map[string]any{"type": "text", "text": s})
				}
			} else {
				for _, p := range toAnySlice(m["content"]) {
					pm, ok2 := p.(map[string]any)
					if !ok2 {
						continue
					}
					switch pt, _ := pm["type"].(string); pt {
					case "input_text", "output_text":
						if s, _ := pm["text"].(string); s != "" {
							blocks = append(blocks, map[string]any{"type": "text", "text": s})
						}
					case "input_image":
						uri, _ := pm["image_url"].(string)
						if strings.HasPrefix(uri, "data:") {
							blocks = append(blocks, map[string]any{"type": "image", "image": uri, "mimeType": mimeFromDataURI(uri)})
						} else if strings.HasPrefix(uri, "http://") || strings.HasPrefix(uri, "https://") {
							if dataURI, err := fetchImageDataURI(uri); err == nil {
								blocks = append(blocks, map[string]any{"type": "image", "image": dataURI, "mimeType": mimeFromDataURI(dataURI)})
							} else {
								logLine("image fetch failed (not forwarded to model): %v", err)
								blocks = append(blocks, map[string]any{"type": "text", "text": "（图片加载失败，已跳过）"})
							}
						}
					}
				}
			}
			if len(blocks) > 0 {
				out = append(out, map[string]any{"role": role, "content": blocks})
			}
		case "function_call":
			id, _ := m["call_id"].(string)
			if id == "" {
				id, _ = m["id"].(string)
			}
			name, _ := m["name"].(string)
			toolNameByID[id] = name
			var parsedInput any
			if args, _ := m["arguments"].(string); args != "" {
				if err := json.Unmarshal([]byte(args), &parsedInput); err != nil || parsedInput == nil {
					parsedInput = map[string]any{"raw": args}
				}
			}
			out = append(out, map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "tool-call", "toolCallId": id, "toolName": name, "input": parsedInput},
			}})
		case "function_call_output":
			cid, _ := m["call_id"].(string)
			tname := toolNameByID[cid]
			if tname == "" {
				tname = "unknown"
			}
			val := ""
			switch o := m["output"].(type) {
			case string:
				val = o
			default:
				if b, err := json.Marshal(o); err == nil {
					val = string(b)
				}
			}
			outType := "text"
			if e, _ := m["is_error"].(bool); e {
				outType = "error-text"
			}
			out = append(out, map[string]any{"role": "tool", "content": []any{
				map[string]any{"type": "tool-result", "toolCallId": cid, "toolName": tname,
					"output": map[string]any{"type": outType, "value": val}},
			}})
		case "reasoning":
			var parts []string
			for _, p := range toAnySlice(m["summary"]) {
				if pm, ok := p.(map[string]any); ok {
					if s, _ := pm["text"].(string); s != "" {
						parts = append(parts, s)
					}
				}
			}
			if s := strings.Join(parts, "\n"); s != "" {
				out = append(out, map[string]any{"role": "assistant", "content": []any{
					map[string]any{"type": "reasoning", "text": s},
				}})
			}
		}
	}
	return out, strings.Join(sysParts, "\n\n")
}

func responsesToolsToWire(tools []any) ([]any, map[string]bool) {
	out := []any{}
	names := map[string]bool{}
	for _, raw := range tools {
		t, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if typ, _ := t["type"].(string); typ != "" && typ != "function" {
			continue
		}
		name, _ := t["name"].(string)
		if name == "" {
			continue
		}
		names[name] = true
		wire := map[string]any{"name": name, "description": t["description"]}
		if p, ok := t["parameters"]; ok && p != nil {
			wire["input_schema"] = p
		} else if p, ok := t["input_schema"]; ok && p != nil {
			wire["input_schema"] = p
		} else {
			wire["input_schema"] = emptySchema()
		}
		out = append(out, wire)
	}
	return out, names
}

func mapToolForCodex(name string, declared map[string]bool) string {
	if name == "" || declared[name] {
		return name
	}
	lower := strings.ToLower(name)
	for d := range declared {
		if strings.ToLower(d) == lower {
			return d
		}
	}
	return name
}

// respDir resolves the project directory for a Responses request, mirroring
// ctxDir for chat: scan input messages for an embedded working directory.
func respDir(body map[string]any) string {
	msgs := []any{}
	for _, raw := range toAnySlice(body["input"]) {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if t, _ := m["type"].(string); t != "message" && t != "" {
			continue
		}
		if c, ok := m["content"].(string); ok && c != "" {
			msgs = append(msgs, map[string]any{"content": c})
			continue
		}
		parts := []any{}
		for _, p := range toAnySlice(m["content"]) {
			if pm, ok := p.(map[string]any); ok {
				if t, _ := pm["type"].(string); t == "input_text" || t == "output_text" {
					parts = append(parts, map[string]any{"type": "text", "text": pm["text"]})
				} else if t, _ := pm["type"].(string); t == "text" {
					parts = append(parts, pm)
				}
			}
		}
		msgs = append(msgs, map[string]any{"content": parts})
	}
	return ctxDir(msgs)
}

func writeResponsesEvent(w io.Writer, event string, payload any) {
	b, err := json.Marshal(payload)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
}

// streamResponsesSSE converts gateway NDJSON into Responses SSE with the
// correct output_index sequence (reasoning=0, message=1, tool=2, ...).
// declared maps tool names declared by the client for Codex name remapping.
// Headers must be written by the caller before invoking.
func streamResponsesSSE(ctx context.Context, w http.ResponseWriter, body io.Reader, model, respID string, declared map[string]bool) {
	flusher, _ := w.(http.Flusher)

	writeResponsesEvent(w, "response.created", map[string]any{
		"type": "response.created",
		"response": map[string]any{"id": respID, "object": "response",
			"status": "in_progress", "model": model},
	})
	flusher.Flush()

	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	msgID, thinkID := "", ""
	msgOpen, thinkOpen := false, false
	toolIdx := 0
	msgIdx, thinkIdx := -1, -1
	var inTok, outTok float64
	var thinkN, textN int
	var msgText strings.Builder

	openMsg := func() {
		if !msgOpen {
			msgID = "msg_" + randID(12)
			msgIdx = toolIdx
			toolIdx++
			writeResponsesEvent(w, "response.output_item.added", map[string]any{
				"type": "response.output_item.added", "output_index": msgIdx,
				"item": map[string]any{"type": "message", "id": msgID,
					"status": "in_progress", "role": "assistant", "content": []any{}},
			})
			writeResponsesEvent(w, "response.content_part.added", map[string]any{
				"type": "response.content_part.added", "item_id": msgID,
				"output_index": msgIdx, "content_index": 0,
				"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
			})
			msgOpen = true
			flusher.Flush()
		}
	}
	openThink := func() {
		if !thinkOpen {
			thinkID = "rs_" + randID(12)
			thinkIdx = toolIdx
			toolIdx++
			writeResponsesEvent(w, "response.output_item.added", map[string]any{
				"type": "response.output_item.added", "output_index": thinkIdx,
				"item": map[string]any{"type": "reasoning", "id": thinkID, "summary": []any{}},
			})
			thinkOpen = true
			flusher.Flush()
		}
	}
	closeThink := func() {
		if thinkOpen {
			writeResponsesEvent(w, "response.output_item.done", map[string]any{
				"type": "response.output_item.done", "output_index": thinkIdx,
				"item": map[string]any{"type": "reasoning", "id": thinkID, "summary": []any{}},
			})
			thinkOpen = false
			flusher.Flush()
		}
	}
	closeMsg := func() {
		if msgOpen {
			full := msgText.String()
			writeResponsesEvent(w, "response.output_text.done", map[string]any{
				"type": "response.output_text.done", "item_id": msgID,
				"output_index": msgIdx, "content_index": 0, "text": full,
			})
			writeResponsesEvent(w, "response.output_item.done", map[string]any{
				"type": "response.output_item.done", "output_index": msgIdx,
				"item": map[string]any{"type": "message", "id": msgID,
					"status": "completed", "role": "assistant",
					"content": []any{map[string]any{"type": "output_text", "text": full, "annotations": []any{}}}},
			})
			msgOpen = false
			flusher.Flush()
		}
	}
	finish := func() {
		closeThink()
		closeMsg()
		if scannerTruncated(scanner) {
			logLine("upstream stream truncated 8MB line (model=%s): usage may be partial", model)
		}
		writeResponsesEvent(w, "response.completed", map[string]any{
			"type": "response.completed",
			"response": map[string]any{"id": respID, "object": "response",
				"status": "completed", "model": model,
				"usage": map[string]any{"input_tokens": int(inTok),
					"output_tokens": int(outTok), "total_tokens": int(inTok + outTok)}},
		})
		flusher.Flush()
		if thinkN == 0 {
			logLine("responses stream done: NO thinking deltas text=%d (model=%s)", textN, model)
		} else {
			logLine("responses stream done: thinking=%d text=%d (model=%s)", thinkN, textN, model)
		}
	}
	fail := func(msg string) {
		closeThink()
		closeMsg()
		writeResponsesEvent(w, "response.failed", map[string]any{
			"type": "response.failed",
			"response": map[string]any{"id": respID, "object": "response",
				"status": "failed", "model": model,
				"error": map[string]any{"code": "upstream_error", "message": msg},
				"usage": map[string]any{"input_tokens": int(inTok),
					"output_tokens": int(outTok), "total_tokens": int(inTok + outTok)}},
		})
		flusher.Flush()
		logLine("responses stream failed (model=%s): %s", model, msg)
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
			openThink()
		case "reasoning-delta":
			if t := evText(ev); t != "" {
				openThink()
				thinkN++
				writeResponsesEvent(w, "response.reasoning_summary_text.delta", map[string]any{
					"type":    "response.reasoning_summary_text.delta",
					"item_id": thinkID, "output_index": thinkIdx, "delta": t,
				})
				flusher.Flush()
			}
		case "reasoning-end":
			closeThink()
		case "text-delta":
			if t := evText(ev); t != "" {
				openMsg()
				textN++
				msgText.WriteString(t)
				writeResponsesEvent(w, "response.output_text.delta", map[string]any{
					"type": "response.output_text.delta", "item_id": msgID,
					"output_index": msgIdx, "content_index": 0, "delta": t,
				})
				flusher.Flush()
			}
		case "tool-call":
			closeThink()
			closeMsg()
			gwName, _ := ev["toolName"].(string)
			fcName := mapToolForCodex(gwName, declared)
			fcID := "fc_" + randID(8)
			args := toolCallArgs(ev)
			fcIdx := toolIdx
			toolIdx++
			writeResponsesEvent(w, "response.output_item.added", map[string]any{
				"type": "response.output_item.added", "output_index": fcIdx,
				"item": map[string]any{"type": "function_call", "id": fcID,
					"status": "in_progress", "name": fcName,
					"arguments": "", "call_id": fcID},
			})
			writeResponsesEvent(w, "response.function_call_arguments.delta", map[string]any{
				"type": "response.function_call_arguments.delta", "item_id": fcID,
				"output_index": fcIdx, "delta": args,
			})
			writeResponsesEvent(w, "response.output_item.done", map[string]any{
				"type": "response.output_item.done", "output_index": fcIdx,
				"item": map[string]any{"type": "function_call", "id": fcID,
					"status": "completed", "name": fcName,
					"arguments": args, "call_id": fcID},
			})
			flusher.Flush()
		case "finish":
			if total, ok := ev["totalUsage"].(map[string]any); ok {
				inTok, _ = total["inputTokens"].(float64)
				outTok, _ = total["outputTokens"].(float64)
			}
		case "error":
			msg := eventErrorText(ev)
			if msg == "" {
				msg = "upstream error"
			}
			logLine("responses lane upstream error: %s", msg)
			fail(msg)
			return
		}
	}
	if err := scanner.Err(); err != nil {
		logLine("upstream stream broken (model=%s): %v", model, err)
	}
	finish()
}

// assembleResponsesJSON buffers NDJSON into a full Responses body.
func assembleResponsesJSON(ctx context.Context, w http.ResponseWriter, body io.Reader, model, respID string, declared map[string]bool) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	output := []any{}
	var thinkParts []string
	textBuf := &strings.Builder{}
	var inTok, outTok float64
	errMsg := ""

	flushText := func() {
		if textBuf.Len() > 0 {
			output = append(output, map[string]any{"type": "message", "id": "msg_" + randID(8),
				"status": "completed", "role": "assistant",
				"content": []any{map[string]any{"type": "output_text", "text": textBuf.String(), "annotations": []any{}}},
			})
			textBuf.Reset()
		}
	}
	flushThink := func() {
		if len(thinkParts) > 0 {
			sum := []any{}
			for _, p := range thinkParts {
				sum = append(sum, map[string]any{"type": "summary_text", "text": p})
			}
			output = append(output, map[string]any{"type": "reasoning", "id": "rs_" + randID(8), "summary": sum})
			thinkParts = nil
		}
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
			thinkParts = append(thinkParts, evText(ev))
		case "reasoning-end":
			flushThink()
		case "text-delta":
			textBuf.WriteString(evText(ev))
		case "tool-call":
			flushThink()
			flushText()
			gwName, _ := ev["toolName"].(string)
			fcID := "fc_" + randID(8)
			output = append(output, map[string]any{"type": "function_call", "id": fcID,
				"status": "completed", "name": mapToolForCodex(gwName, declared),
				"arguments": toolCallArgs(ev), "call_id": fcID})
		case "finish":
			if total, ok := ev["totalUsage"].(map[string]any); ok {
				inTok, _ = total["inputTokens"].(float64)
				outTok, _ = total["outputTokens"].(float64)
			}
		case "error":
			upErr := eventErrorText(ev)
			if upErr == "" {
				upErr = "upstream error"
			}
			logLine("responses lane upstream error: %s", upErr)
			errMsg = upErr
		}
	}
	flushThink()
	flushText()
	w.Header().Set("Content-Type", "application/json")
	if errMsg != "" {
		_ = json.NewEncoder(w).Encode(map[string]any{"id": respID, "object": "response",
			"status": "failed", "model": model, "output": output,
			"error": map[string]any{"code": "upstream_error", "message": errMsg},
			"usage": map[string]any{"input_tokens": int(inTok),
				"output_tokens": int(outTok), "total_tokens": int(inTok + outTok)}})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"id": respID, "object": "response",
		"status": "completed", "model": model, "output": output,
		"usage": map[string]any{"input_tokens": int(inTok),
			"output_tokens": int(outTok), "total_tokens": int(inTok + outTok)}})
}

var (
	responsesDeclaredMu sync.Mutex
	responsesDeclared   = map[string]map[string]bool{}
)

// streamResponsesSSEWithMap streams with tool names remapped for Codex.
func streamResponsesSSEWithMap(ctx context.Context, w http.ResponseWriter, body io.Reader, model, respID string) {
	responsesDeclaredMu.Lock()
	declared := responsesDeclared[respID]
	responsesDeclaredMu.Unlock()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	streamResponsesSSE(ctx, w, body, model, respID, declared)
}

// handleResponses implements OpenAI Responses API on the gateway lane.
func handleResponses(w http.ResponseWriter, r *http.Request) {
	auth, _, err := loadLocalCommandCodeAuth()
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"error":{"message":"no local auth"}}`))
		return
	}
	bodyMap, err := readClientBody(r)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid json"}}`))
		return
	}
	model, _ := bodyMap["model"].(string)
	model = resolveModel(model)
	if model == "" {
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"error":{"message":"missing model"}}`))
		return
	}
	system := ""
	if inst, ok := bodyMap["instructions"].(string); ok && inst != "" {
		system = inst
	}
	for _, it := range toAnySlice(bodyMap["input"]) {
		if mm, ok := it.(map[string]any); ok {
			if t, _ := mm["type"].(string); t == "message" {
				if rr, _ := mm["role"].(string); rr == "developer" {
					for _, p := range toAnySlice(mm["content"]) {
						if pm, ok2 := p.(map[string]any); ok2 {
							if s, _ := pm["text"].(string); s != "" {
								system += "\n\n" + s
							}
						}
					}
				}
			}
		}
	}
	wireMsgs, sysFromInput := responsesInputToWire(bodyMap["input"])
	if sysFromInput != "" {
		if system == "" {
			system = sysFromInput
		} else {
			system += "\n\n" + sysFromInput
		}
	}
	if len(wireMsgs) == 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"error":{"message":"no user/assistant messages in input"}}`))
		return
	}
	toolsRaw, _ := bodyMap["tools"].([]any)
	wireTools, declared := responsesToolsToWire(toolsRaw)
	maxTokens := 64000
	clientMaxTokens := false
	if v, ok := bodyMap["max_output_tokens"].(float64); ok {
		if v > 0 {
			maxTokens = int(v)
			clientMaxTokens = true
		} else {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"error":{"message":"max_output_tokens must be > 0"}}`))
			return
		}
	} else if v, ok := bodyMap["max_tokens"].(float64); ok {
		if v > 0 {
			maxTokens = int(v)
			clientMaxTokens = true
		} else {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"error":{"message":"max_tokens must be > 0"}}`))
			return
		}
	}
	effort := ""
	if rc, ok := bodyMap["reasoning"].(map[string]any); ok {
		if v, ok2 := rc["effort"].(string); ok2 {
			effort = v
		}
	}
	if effort == "" {
		if v, ok := bodyMap["reasoning_effort"].(string); ok {
			effort = v
		}
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

	extras := pickExtras(bodyMap, "top_p")
	upResp, err := forwardToGateway(r.Context(), auth, model, wireMsgs, system, wireTools, maxTokens, bodyMap["temperature"], effort, stableThreadID(conversationKey(bodyMap)), extras, respDir(bodyMap))
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(502)
		_, _ = w.Write([]byte(`{"error":{"message":"upstream error"}}`))
		logLine("responses lane -> 502 %v", err)
		return
	}
	defer upResp.Body.Close()
	if upResp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(upResp.Body, 1<<20))
		gatewayError(w, upResp.StatusCode, respBody, "responses", model)
		return
	}
	respID := "resp_" + randID(12)
	stream := true
	if v, ok := bodyMap["stream"].(bool); ok {
		stream = v
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
	responsesDeclaredMu.Lock()
	responsesDeclared[respID] = declared
	responsesDeclaredMu.Unlock()
	if stream {
		streamResponsesSSEWithMap(r.Context(), w, upBody, model, respID)
	} else {
		assembleResponsesJSON(r.Context(), w, upBody, model, respID, declared)
	}
	responsesDeclaredMu.Lock()
	delete(responsesDeclared, respID)
	responsesDeclaredMu.Unlock()
	logLine("POST /v1/responses model=%s stream=%v -> 200", model, stream)
}
