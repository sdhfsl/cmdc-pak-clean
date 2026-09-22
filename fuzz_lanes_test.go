package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

// Handler-level fuzzing: full request path (parse -> validate -> upstream ->
// SSE) against a mock upstream, so the fuzzer stays offline.

func mockUpstream(f *testing.F) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(
			"{\"type\":\"start\"}\n" +
				"{\"type\":\"reasoning-delta\",\"text\":\"t\"}\n" +
				"{\"type\":\"text-delta\",\"text\":\"ok\"}\n" +
				"{\"type\":\"tool-call\",\"toolName\":\"run\",\"toolCallId\":\"c1\",\"input\":{\"a\":1}}\n" +
				"{\"type\":\"finish\",\"rawFinishReason\":\"tool-calls\",\"totalUsage\":{\"inputTokens\":1,\"outputTokens\":2}}\n"))
	}))
	f.Cleanup(srv.Close)
	_ = os.Setenv("CMDC_PAK_GATEWAY_URL", srv.URL)
	f.Cleanup(func() {
		_ = os.Unsetenv("CMDC_PAK_GATEWAY_URL")
	})
	return srv
}

func fuzzSeeds(f *testing.F) {
	for _, s := range []string{
		`{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}]}`,
		`{"model":"deepseek-v4-flash","max_tokens":0,"messages":[]}`,
		`{}`,
		`[]`,
		`null`,
		`"str"`,
		`{"model":123,"messages":"x"}`,
		`{"model":"m","messages":[null,42]}`,
		`{"model":"m","stream":true,"messages":[{"role":"user","content":[{"type":"text","text":"x"}]}]}`,
	} {
		f.Add([]byte(s))
	}
}

func FuzzHandleChatLane(f *testing.F) {
	mockUpstream(f)
	fuzzSeeds(f)
	f.Fuzz(func(t *testing.T, body []byte) {
		if bytes.Contains(body, []byte("http")) {
			return // keep the fuzzer offline (image fetch)
		}
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handleChat(w, req)
		switch w.Code {
		case http.StatusOK, http.StatusBadRequest, http.StatusUnauthorized, http.StatusBadGateway:
		default:
			t.Errorf("unexpected status %d", w.Code)
		}
	})
}

func FuzzHandleMessagesLane(f *testing.F) {
	mockUpstream(f)
	for _, s := range []string{
		`{"model":"deepseek-v4-flash","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`,
		`{"model":"m","max_tokens":-1,"messages":[]}`,
		`{"model":"m","max_tokens":100,"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"x","content":"r"}]}]}`,
		`{"model":"m","max_tokens":100,"thinking":{"type":"enabled"},"messages":[{"role":"user","content":"x"}]}`,
		`{}`,
		`null`,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		if bytes.Contains(body, []byte("http")) {
			return
		}
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
		req.Header.Set("anthropic-version", "2023-06-01")
		w := httptest.NewRecorder()
		handleMessages(w, req)
		switch w.Code {
		case http.StatusOK, http.StatusBadRequest, http.StatusUnauthorized, http.StatusBadGateway:
		default:
			t.Errorf("unexpected status %d", w.Code)
		}
	})
}

func FuzzHandleResponsesLane(f *testing.F) {
	mockUpstream(f)
	for _, s := range []string{
		`{"model":"deepseek-v4-flash","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`,
		`{"model":"m","input":"just a string"}`,
		`{"model":"m","input":[]}`,
		`{"model":"m","stream":false,"input":[{"type":"function_call","call_id":"c","name":"t","arguments":"{}"}]}`,
		`{}`,
		`null`,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		if bytes.Contains(body, []byte("http")) {
			return
		}
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
		w := httptest.NewRecorder()
		handleResponses(w, req)
		switch w.Code {
		case http.StatusOK, http.StatusBadRequest, http.StatusUnauthorized, http.StatusBadGateway:
		default:
			t.Errorf("unexpected status %d", w.Code)
		}
	})
}
