package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/yetone/magpie/internal/provider"
)

func semanticTestEnv(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("XKEYSMITH_SKILLS_DIR", t.TempDir())
	for _, name := range []string{"XKEYSMITH_TOOLS", "XKEYSMITH_APPROVAL", "XKEYSMITH_ALLOWLIST", "XKEYSMITH_MCP_SERVERS", "XKEYSMITH_BIN", "XKEYSMITH_SKILLS_MODE"} {
		t.Setenv(name, "")
	}
	if err := SetXkMaster(true); err != nil {
		t.Fatal(err)
	}
}

func semanticProvider(t *testing.T, proto provider.Protocol, handler http.HandlerFunc) {
	t.Helper()
	up := httptest.NewServer(handler)
	t.Cleanup(up.Close)
	p := provider.Provider{ID: "semantic-test", Name: "Semantic Test", Semantic: true, Key: "fixture", Models: []string{"m1"}}
	switch proto {
	case provider.Chat:
		p.Chat = up.URL + "/v1"
	case provider.Responses:
		p.Responses = up.URL + "/v1"
	case provider.Anthropic:
		p.Anthropic = up.URL
	case provider.Gemini:
		p.Gemini = up.URL + "/v1beta"
	}
	if err := provider.Save(p); err != nil {
		t.Fatal(err)
	}
}

func TestSemanticKeepsNativeModelMappingAndUsage(t *testing.T) {
	semanticTestEnv(t)
	var upstreamModel string
	semanticProvider(t, provider.Chat, func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		_ = json.NewDecoder(r.Body).Decode(&request)
		upstreamModel, _ = request["model"].(string)
		w.Header().Set("Content-Type", "application/json")
		if upstreamModel != "m1" {
			w.WriteHeader(400)
			io.WriteString(w, `{"error":{"message":"unknown model"}}`)
			return
		}
		io.WriteString(w, `{"id":"reply-1","model":"m1","choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":10,"completion_tokens":3}}`)
	})
	s := New()
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"semantic-test/m1","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != 200 || upstreamModel != "m1" {
		t.Fatalf("status=%d upstream_model=%q body=%s", rec.Code, upstreamModel, rec.Body.String())
	}
	u := s.Recent()[0].Usage
	if u.Input != 10 || u.Output != 3 || u.Served != "m1" || u.ResponseID != "reply-1" {
		t.Fatalf("semantic request lost native usage: %+v", u)
	}
	if rec.Header().Get(providerHeader) == "" || rec.Header().Get("x-xkeysmith-trace") == "" {
		t.Fatalf("missing routing or semantic trace headers: %v", rec.Header())
	}
}

func TestSemanticKeepsNativeProtocolTranslation(t *testing.T) {
	semanticTestEnv(t)
	var paths []string
	semanticProvider(t, provider.Chat, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, sse(`data: {"id":"c1","model":"m1","choices":[{"index":0,"delta":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":3}}`, `data: [DONE]`))
	})
	rec := httptest.NewRecorder()
	New().Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"semantic-test/m1","input":"hi","stream":false}`)))
	if rec.Code != 200 || len(paths) != 1 || paths[0] != "/v1/chat/completions" || !strings.Contains(rec.Body.String(), `"output_text"`) {
		t.Fatalf("status=%d paths=%v body=%s", rec.Code, paths, rec.Body.String())
	}
}

func TestSemanticPreservesToolCallsAndStreams(t *testing.T) {
	cases := []struct {
		name                 string
		proto                provider.Protocol
		path, request, reply string
	}{
		{"chat", provider.Chat, "/v1/chat/completions", `{"model":"semantic-test/m1","messages":[{"role":"user","content":"hi"}],"stream":true}`, `{"id":"c1","model":"m1","choices":[{"finish_reason":"tool_calls","message":{"role":"assistant","content":null,"tool_calls":[{"id":"t1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"example.txt\"}"}}]}}]}`},
		{"responses", provider.Responses, "/v1/responses", `{"model":"semantic-test/m1","input":"hi","stream":true}`, `{"id":"resp_1","object":"response","model":"m1","status":"completed","output":[{"type":"function_call","id":"fc_1","call_id":"t1","name":"read_file","arguments":"{\"path\":\"example.txt\"}"}]}`},
		{"anthropic", provider.Anthropic, "/v1/messages", `{"model":"semantic-test/m1","max_tokens":32,"messages":[{"role":"user","content":"hi"}],"stream":true}`, `{"id":"msg_1","type":"message","model":"m1","role":"assistant","stop_reason":"tool_use","content":[{"type":"tool_use","id":"t1","name":"read_file","input":{"path":"example.txt"}}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			semanticTestEnv(t)
			calls := 0
			semanticProvider(t, tc.proto, func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, tc.reply)
			})
			rec := httptest.NewRecorder()
			New().Handler().ServeHTTP(rec, httptest.NewRequest("POST", tc.path, strings.NewReader(tc.request)))
			if rec.Code != 200 || calls != 1 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/event-stream") {
				t.Fatalf("status=%d upstream_calls=%d type=%q body=%s", rec.Code, calls, rec.Header().Get("Content-Type"), rec.Body.String())
			}
			var col collector
			decode := decoder(tc.proto)
			if err := readSSE(strings.NewReader(rec.Body.String()), func(_, data string) error { return decode(data, col.add) }); err != nil {
				t.Fatal(err)
			}
			result := col.finish()
			if len(result.Parts) != 1 || result.Parts[0].Kind != ToolCall || result.Parts[0].Name != "read_file" || !strings.Contains(string(result.Parts[0].Args), "example.txt") {
				t.Fatalf("tool call failed to round trip through the native decoder: %+v", result)
			}
		})
	}
}

func TestSemanticAcceptsNativeSSE(t *testing.T) {
	semanticTestEnv(t)
	calls := 0
	semanticProvider(t, provider.Chat, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, sse(`data: {"id":"c1","model":"m1","choices":[{"index":0,"delta":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`, `data: [DONE]`))
	})
	rec := httptest.NewRecorder()
	New().Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"semantic-test/m1","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != 200 || calls != 1 || !strings.Contains(rec.Body.String(), `"content":"ok"`) {
		t.Fatalf("status=%d calls=%d body=%s", rec.Code, calls, rec.Body.String())
	}
}

// Gemini clients keep their model and stream choice in the URL. The semantic
// path must still use the provider's native Gemini transport (which Magpie
// asks as streamGenerateContent), then restore the client's requested JSON or
// SSE shape without losing refusal/usage/model metadata.
func TestSemanticGeminiGenerateContentAndStream(t *testing.T) {
	for _, tc := range []struct {
		name   string
		path   string
		text   string
		stream bool
	}{
		{name: "json", path: "/v1beta/models/semantic-test/m1:generateContent", text: "gemini answer"},
		{name: "sse", path: "/v1beta/models/semantic-test/m1:streamGenerateContent?alt=sse", text: "gemini stream answer", stream: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			semanticTestEnv(t)
			var calls int
			var paths []string
			semanticProvider(t, provider.Gemini, func(w http.ResponseWriter, r *http.Request) {
				calls++
				paths = append(paths, r.URL.RequestURI())
				w.Header().Set("Content-Type", "text/event-stream")
				if calls == 1 {
					io.WriteString(w, sse(`data: {"candidates":[{"content":{"role":"model","parts":[]},"finishReason":"SAFETY"}],"usageMetadata":{"promptTokenCount":11,"candidatesTokenCount":1,"totalTokenCount":12}}`))
					return
				}
				io.WriteString(w, sse(`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"`+tc.text+`"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":11,"candidatesTokenCount":4,"totalTokenCount":15},"modelVersion":"m1","responseId":"gemini-r1"}`))
			})
			rec := httptest.NewRecorder()
			body := `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`
			s := New()
			s.Handler().ServeHTTP(rec, httptest.NewRequest("POST", tc.path, strings.NewReader(body)))
			if rec.Code != 200 || calls != 2 {
				t.Fatalf("status=%d calls=%d body=%s", rec.Code, calls, rec.Body.String())
			}
			if len(paths) != 2 || paths[0] != "/v1beta/models/m1:streamGenerateContent?alt=sse" || paths[1] != paths[0] {
				t.Fatalf("native Gemini path=%v", paths)
			}
			if tc.stream {
				if !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/event-stream") || !strings.Contains(rec.Body.String(), tc.text) {
					t.Fatalf("stream response type/body=%q %s", rec.Header().Get("Content-Type"), rec.Body.String())
				}
			} else {
				if strings.HasPrefix(strings.TrimSpace(rec.Body.String()), "data:") || !strings.Contains(rec.Body.String(), tc.text) {
					t.Fatalf("JSON response=%s", rec.Body.String())
				}
			}
			u := s.Recent()
			// The refusal probe is a real upstream call too, so request
			// accounting includes both probes while the final served model and
			// response id come from the accepted native reply.
			if len(u) == 0 || u[len(u)-1].Usage.Input != 22 || u[len(u)-1].Usage.Output != 5 || u[len(u)-1].Usage.Served != "m1" || u[len(u)-1].Usage.ResponseID != "gemini-r1" {
				t.Fatalf("native Gemini usage/model mapping lost: %+v", u)
			}
		})
	}
}

func TestSemanticChineseSessionWithoutHeader(t *testing.T) {
	semanticTestEnv(t)
	semanticProvider(t, provider.Chat, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(chatAnswer("ok"))
	})
	body, _ := json.Marshal(map[string]any{"model": "semantic-test/m1", "messages": []any{map[string]any{"role": "user", "content": strings.Repeat("中", 200)}}})
	rec := httptest.NewRecorder()
	New().Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(string(body))))
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestSemanticMixedToolsStayInTheirOwner(t *testing.T) {
	semanticTestEnv(t)
	fake := &capFake{reply: func(call int, request map[string]any) map[string]any {
		if call == 1 {
			response := capToolCall("xk__skill_load", map[string]any{"name": "rev"})
			message := response["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
			message["tool_calls"] = append(message["tool_calls"].([]any), map[string]any{"id": "client-1", "type": "function", "function": map[string]any{"name": "read_file", "arguments": `{"path":"example.txt"}`}})
			return response
		}
		return capToolCall("read_file", map[string]any{"path": "example.txt"})
	}}
	capSetup(t, fake)
	if err := SetXkMaster(true); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	New().Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"xkeysmith/gpt-5","stream":true,"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"read_file","parameters":{"type":"object"}}}]}`)))
	if rec.Code != 200 || len(fake.seen) != 2 || strings.Contains(rec.Body.String(), "xk__skill_load") || !strings.Contains(rec.Body.String(), "read_file") || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/event-stream") {
		t.Fatalf("status=%d upstream_calls=%d type=%q body=%s", rec.Code, len(fake.seen), rec.Header().Get("Content-Type"), rec.Body.String())
	}
	second, _ := json.Marshal(fake.seen[1])
	if !strings.Contains(string(second), "逆向正文") || strings.Contains(string(second), "client-1") {
		t.Fatalf("gateway-only internal turn must be paired with its actual result: %s", second)
	}
}

func TestXkMasterConcurrentRefresh(t *testing.T) {
	semanticTestEnv(t)
	var workers sync.WaitGroup
	for i := 0; i < 8; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for j := 0; j < 500; j++ {
				_ = XkMasterOn()
			}
		}()
	}
	for i := 0; i < 20; i++ {
		if err := SetXkMaster(i%2 == 0); err != nil {
			t.Fatal(err)
		}
		_ = XkMasterOn()
	}
	workers.Wait()
}
