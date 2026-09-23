package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func newTestRequest(origin string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8787/api/config", nil)
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	return req
}

// Table-driven tests pinning the request-decision logic that the live
// verification covered but no automated test did.

func TestEnsureThinkingBudget(t *testing.T) {
	cases := []struct {
		name      string
		maxTokens int
		clientSet bool
		effort    string
		want      int
		wantRaise bool
	}{
		{"no effort keeps quota", 200, true, "", 200, false},
		{"none keeps quota", 200, true, "none", 200, false},
		{"NONE case-insensitive", 200, true, "NONE", 200, false},
		{"client unset -> default", 0, false, "max", thinkingTokenDefault, true},
		{"client unset default already", thinkingTokenDefault, false, "max", thinkingTokenDefault, false},
		{"small client quota -> floor", 200, true, "max", thinkingTokenFloor, true},
		{"exactly floor stays", thinkingTokenFloor, true, "max", thinkingTokenFloor, false},
		{"above floor stays", 200000, true, "high", 200000, false},
	}
	for _, c := range cases {
		got, raised := ensureThinkingBudget(c.maxTokens, c.clientSet, c.effort)
		if got != c.want || raised != c.wantRaise {
			t.Errorf("%s: got (%d,%v), want (%d,%v)", c.name, got, raised, c.want, c.wantRaise)
		}
	}
}

func TestCapMaxTokens(t *testing.T) {
	for _, c := range []struct{ in, want int }{
		{0, 0}, {100, 100}, {maxTokenCap, maxTokenCap},
		{maxTokenCap + 1, maxTokenCap}, {200000, maxTokenCap}, {384000, maxTokenCap},
	} {
		if got := capMaxTokens(c.in); got != c.want {
			t.Errorf("capMaxTokens(%d)=%d, want %d", c.in, got, c.want)
		}
	}
}

func TestEventErrorText(t *testing.T) {
	cases := []struct {
		name string
		ev   string
		want string
	}{
		{"string form", `{"type":"error","error":"premium_credits_exhausted"}`, "premium_credits_exhausted"},
		{"object form", `{"type":"error","error":{"message":"boom"}}`, "boom"},
		{"top-level message", `{"type":"error","message":"top"}`, "top"},
		{"string wins over empty object", `{"error":"s","message":"m"}`, "s"},
		{"nothing", `{"type":"error"}`, ""},
		{"empty string error", `{"error":""}`, ""},
		{"non-string non-object", `{"error":123}`, ""},
	}
	for _, c := range cases {
		var ev map[string]any
		if err := json.Unmarshal([]byte(c.ev), &ev); err != nil {
			t.Fatalf("%s: bad fixture: %v", c.name, err)
		}
		if got := eventErrorText(ev); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestScanToolChoice(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"auto string", `"auto"`, `{"type":"auto"}`},
		{"required string", `"required"`, `{"type":"any"}`},
		{"none string dropped", `"none"`, `null`},
		{"anthropic any", `{"type":"any"}`, `{"type":"any"}`},
		{"anthropic tool", `{"type":"tool","name":"x"}`, `{"type":"tool","name":"x"}`},
		{"tool without name -> any", `{"type":"tool"}`, `{"type":"any"}`},
		{"openai chat function", `{"type":"function","function":{"name":"x"}}`, `{"type":"tool","name":"x"}`},
		{"responses function", `{"type":"function","name":"x"}`, `{"type":"tool","name":"x"}`},
		{"unknown dropped", `{"type":"weird"}`, `null`},
		{"number dropped", `42`, `null`},
	}
	for _, c := range cases {
		var in any
		if err := json.Unmarshal([]byte(c.in), &in); err != nil {
			t.Fatalf("%s: bad fixture: %v", c.name, err)
		}
		got := scanToolChoice(in)
		var gotJSON, wantJSON string
		if got == nil {
			gotJSON = "null"
		} else {
			b, _ := json.Marshal(got)
			gotJSON = string(b)
		}
		want := map[string]any{}
		_ = json.Unmarshal([]byte(c.want), &want)
		if c.want == "null" {
			wantJSON = "null"
		} else {
			b, _ := json.Marshal(want)
			wantJSON = string(b)
		}
		if gotJSON != wantJSON {
			t.Errorf("%s: got %s, want %s", c.name, gotJSON, wantJSON)
		}
	}
}

func TestToolCallArgs(t *testing.T) {
	cases := []struct {
		name string
		ev   string
		want string
	}{
		{"object input", `{"input":{"a":1}}`, `{"a":1}`},
		{"null input", `{"input":null}`, `{}`},
		{"array wrapper", `{"input":[{"a":1}]}`, `{"a":1}`},
		{"json string", `{"input":"{\"a\":1}"}`, `{"a":1}`},
		{"missing both", `{"type":"tool-call"}`, `{}`},
		{"args fallback", `{"args":{"b":2}}`, `{"b":2}`},
		{"empty string input", `{"input":""}`, `{}`},
	}
	for _, c := range cases {
		var ev map[string]any
		if err := json.Unmarshal([]byte(c.ev), &ev); err != nil {
			t.Fatalf("%s: bad fixture: %v", c.name, err)
		}
		got := toolCallArgs(ev)
		var a, b any
		if json.Unmarshal([]byte(got), &a) != nil || json.Unmarshal([]byte(c.want), &b) != nil {
			t.Errorf("%s: non-JSON output %q", c.name, got)
			continue
		}
		ga, _ := json.Marshal(a)
		gb, _ := json.Marshal(b)
		if string(ga) != string(gb) {
			t.Errorf("%s: got %s, want %s", c.name, ga, gb)
		}
	}
}

func TestIsTrustedOrigin(t *testing.T) {
	cases := []struct {
		origin string
		want   bool
	}{
		{"", true},
		{"http://127.0.0.1:8787", true},
		{"http://localhost:8787", true},
		{"http://[::1]:8787", true},
		{"https://evil.example.com", false},
		{"null", false},
		{"file:///tmp/x.html", false},
		{"://bad", false},
	}
	for _, c := range cases {
		req := newTestRequest(c.origin)
		if got := isTrustedOrigin(req); got != c.want {
			t.Errorf("origin %q: got %v, want %v", c.origin, got, c.want)
		}
	}
}

func TestConversationKeyForToolSurface(t *testing.T) {
	body := map[string]any{"conversation_id": "sess-1"}
	a := []any{
		map[string]any{"name": "read", "description": "d"},
		map[string]any{"name": "edit", "description": "d"},
	}
	b := []any{
		map[string]any{"name": "read", "description": "d"},
		map[string]any{"name": "edit", "description": "d"},
		map[string]any{"name": "bash", "description": "d"},
	}
	reordered := []any{
		map[string]any{"name": "edit", "description": "d"},
		map[string]any{"name": "read", "description": "d"},
	}
	ka := conversationKeyFor(body, a)
	kb := conversationKeyFor(body, b)
	kr := conversationKeyFor(body, reordered)
	kn := conversationKeyFor(body, nil)
	if ka == "" {
		t.Fatal("base key must not be empty")
	}
	if kb == ka {
		t.Error("different tool surfaces must produce different keys")
	}
	if kr != ka {
		t.Error("tool order must not change the key")
	}
	if kn != conversationKey(body) {
		t.Error("no tools must degrade to the plain conversation key")
	}
	if conversationKeyFor(map[string]any{}, a) != "" {
		t.Error("no conversation id must still return empty")
	}
	t.Logf("keys: base=%q +tool=%q", kn, ka)
}
