package main

import (
	"encoding/json"
	"testing"
)

func wireFrom(t *testing.T, js string) []any {
	t.Helper()
	var msgs []any
	if err := json.Unmarshal([]byte(js), &msgs); err != nil {
		t.Fatal(err)
	}
	return repairToolPairing(msgs)
}

func TestRepairToolPairing(t *testing.T) {
	// orphan tool call gets a placeholder right after it
	got := wireFrom(t, `[
		{"role":"assistant","content":[{"type":"tool-call","toolCallId":"c1","toolName":"run","input":{}}]},
		{"role":"user","content":[{"type":"text","text":"next"}]}
	]`)
	if len(got) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(got))
	}
	mid := got[1].(map[string]any)
	if role, _ := mid["role"].(string); role != "tool" {
		t.Fatalf("placeholder not injected, got role %q", role)
	}
	blocks := mid["content"].([]any)
	b0 := blocks[0].(map[string]any)
	if id, _ := b0["toolCallId"].(string); id != "c1" {
		t.Fatalf("wrong call id %q", id)
	}

	// answered call is left untouched
	got = wireFrom(t, `[
		{"role":"assistant","content":[{"type":"tool-call","toolCallId":"c2","toolName":"run","input":{}}]},
		{"role":"tool","content":[{"type":"tool-result","toolCallId":"c2","toolName":"run","output":{"type":"text","value":"ok"}}]}
	]`)
	if len(got) != 2 {
		t.Fatalf("answered call altered: %d messages", len(got))
	}
	t.Log("orphan repaired, answered untouched")
}
