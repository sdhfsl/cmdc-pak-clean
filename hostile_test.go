package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Hostile-upstream tests: the gateway is not trusted to behave. Every case
// must leave the proxy alive, bounded and with a sane status code.
func TestHostileUpstreamBehaviors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		var env struct {
			Params struct {
				Model string `json:"model"`
			} `json:"params"`
		}
		_ = json.Unmarshal(body, &env)
		m := env.Params.Model
		switch {
		case strings.HasPrefix(m, "h-close-mid"):
			hj, ok := w.(http.Hijacker)
			if !ok {
				http.Error(w, "no hijack", 500)
				return
			}
			conn, buf, err := hj.Hijack()
			if err != nil {
				return
			}
			defer conn.Close()
			_, _ = buf.WriteString("HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\n\r\n")
			_, _ = buf.WriteString("{\"type\":\"text-delta\",\"text\":\"partial\"}\n")
			_ = buf.Flush()
			time.Sleep(50 * time.Millisecond)
			// close without terminating the stream
		case strings.HasPrefix(m, "h-garbage"):
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte("not json at all\n<<<>>>\n{\"broken\":\n{\"type\":\"text-delta\",\"text\":\"ok\"}\n{\"type\":\"finish\",\"rawFinishReason\":\"stop\",\"totalUsage\":{\"inputTokens\":1,\"outputTokens\":1}}\n"))
		case strings.HasPrefix(m, "h-oversize"):
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte("{\"type\":\"text-delta\",\"text\":\""))
			chunk := strings.Repeat("A", 1<<20)
			for i := 0; i < 9; i++ { // > 8MB single line
				_, _ = w.Write([]byte(chunk))
			}
			_, _ = w.Write([]byte("\"}\n"))
			_, _ = w.Write([]byte("{\"type\":\"finish\",\"rawFinishReason\":\"stop\",\"totalUsage\":{\"inputTokens\":1,\"outputTokens\":1}}\n"))
		case strings.HasPrefix(m, "h-empty"):
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(200)
		case strings.HasPrefix(m, "h-html500"):
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(500)
			_, _ = w.Write([]byte("<html><body><h1>502 Bad Gateway</h1></body></html>"))
		case strings.HasPrefix(m, "h-slow"):
			time.Sleep(2 * time.Second)
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte("{\"type\":\"text-delta\",\"text\":\"late\"}\n{\"type\":\"finish\",\"rawFinishReason\":\"stop\",\"totalUsage\":{\"inputTokens\":1,\"outputTokens\":1}}\n"))
		case strings.HasPrefix(m, "h-badutf8"):
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte("{\"type\":\"text-delta\",\"text\":\"\xff\xfe\xfd\"}\n{\"type\":\"finish\",\"rawFinishReason\":\"stop\",\"totalUsage\":{\"inputTokens\":1,\"outputTokens\":1}}\n"))
		default:
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte("{\"type\":\"text-delta\",\"text\":\"ok\"}\n{\"type\":\"finish\",\"rawFinishReason\":\"stop\",\"totalUsage\":{\"inputTokens\":1,\"outputTokens\":1}}\n"))
		}
	}))
	defer srv.Close()
	t.Setenv("CMDC_PAK_GATEWAY_URL", srv.URL)

	cases := []struct{ model, note string }{
		{"h-close-mid", "upstream dies mid-stream"},
		{"h-garbage", "non-JSON lines interleaved"},
		{"h-oversize", ">8MB single NDJSON line"},
		{"h-empty", "200 with empty body"},
		{"h-html500", "500 with HTML body"},
		{"h-slow", "slow but valid"},
		{"h-badutf8", "invalid UTF-8 in payload"},
	}
	for _, c := range cases {
		t.Run(c.model, func(t *testing.T) {
			payload := fmt.Sprintf(`{"model":"%s","messages":[{"role":"user","content":"hi"}]}`, c.model)
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(payload))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()

			done := make(chan struct{})
			go func() {
				handleChat(w, req)
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(30 * time.Second):
				t.Fatalf("%s: handler hung (%s)", c.model, c.note)
			}
			switch w.Code {
			case http.StatusOK, http.StatusBadRequest, http.StatusUnauthorized, http.StatusBadGateway, 500:
			default:
				t.Errorf("%s: unexpected status %d (%s)", c.model, w.Code, c.note)
			}
			t.Logf("%s: status=%d bytes=%d (%s)", c.model, w.Code, w.Body.Len(), c.note)
		})
	}
}
