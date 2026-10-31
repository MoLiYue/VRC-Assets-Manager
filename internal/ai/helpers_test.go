package ai

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// httptestLLM: an OpenAI-compatible service that answers every chat with what reply says of the request.
func httptestLLM(t *testing.T, reply func(body string) string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		out, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": reply(string(b))}, "finish_reason": "stop"}}})
		_, _ = w.Write(out)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func fmtJSON(v any) string { b, _ := json.Marshal(v); return string(b) }
