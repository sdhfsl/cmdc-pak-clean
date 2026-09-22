package main

import (
	"encoding/json"
	"testing"
)

// Fuzz message conversion: malformed client payloads must never panic.

func FuzzBuildWireMessages(f *testing.F) {
	seeds := []string{
		`[{"role":"user","content":"hi"}]`,
		`[{"role":"system","content":[{"type":"text","text":"s"}]}]`,
		`[{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"x","arguments":"{}"}}]}]`,
		`[{"role":"assistant","content":null,"tool_calls":[{"id":"c1","function":{"name":"x","arguments":[{"a":1}]}}]}]`,
		`[{"role":"tool","tool_call_id":"c1","content":"ok","is_error":true}]`,
		`[{"role":"tool","tool_call_id":"missing","content":[{"type":"text","text":"t"}]}]`,
		`[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgo="}}]}]`,
		`[{"role":"user","content":[{"type":"image_url"}]}]`,
		`[{"role":"assistant","reasoning_content":"think","content":"x"}]`,
		`[{"role":null,"content":123}]`,
		`[{"role":"user","content":[null,42,{"type":"text"}]}]`,
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		var msgs []any
		if err := json.Unmarshal(data, &msgs); err != nil {
			return
		}
		wire, system, err := buildWireMessages(msgs)
		if err != nil {
			return
		}
		if wire != nil || system != "" {
			_ = buildWireTools(msgs) // shared path, arbitrary input
		}
	})
}

func FuzzAnthropicMessagesToWire(f *testing.F) {
	seeds := []string{
		`[{"role":"user","content":"hi"}]`,
		`[{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"run","input":null}]}]`,
		`[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"r","is_error":true}]}]`,
		`[{"role":"user","content":[{"type":"tool_result","tool_use_id":"x","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"a"}}]}]}]`,
		`[{"role":"assistant","content":[{"type":"thinking","thinking":"t"}]}]`,
		`[{"role":"user","content":[{"type":"image","source":null}]}]`,
		`[{"role":null,"content":[{}]}]`,
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		var msgs []any
		if err := json.Unmarshal(data, &msgs); err != nil {
			return
		}
		_ = anthropicMessagesToWire(msgs)
		_ = anthropicSystemToString(msgs)
	})
}

func FuzzResponsesInputToWire(f *testing.F) {
	seeds := []string{
		`[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]`,
		`[{"type":"function_call","call_id":"c1","name":"run","arguments":"{}"}]`,
		`[{"type":"function_call_output","call_id":"c1","output":"ok","is_error":true}]`,
		`[{"type":"reasoning","summary":[{"type":"summary_text","text":"t"}]}]`,
		`[{"type":"message","role":"system","content":[{"text":"s"}]}]`,
		`[null,42,"x",{"type":"message"}]`,
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		var input any
		if err := json.Unmarshal(data, &input); err != nil {
			return
		}
		msgs, sys := responsesInputToWire(input)
		if msgs != nil || sys != "" {
			_, _ = responsesToolsToWire(msgs)
		}
	})
}

func FuzzNormalizeToolInput(f *testing.F) {
	for _, s := range []string{`null`, `{}`, `[]`, `[{}]`, `"{}"`, `"[1]"`, `123`, `"x"`, `[[{}]]`} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		var v any
		if err := json.Unmarshal(data, &v); err != nil {
			return
		}
		_ = normalizeToolInput(v)
	})
}
