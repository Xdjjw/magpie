package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/gateway/xk/attach"
	"github.com/yetone/magpie/internal/gateway/xk/audit"
	"github.com/yetone/magpie/internal/gateway/xk/govern"
	"github.com/yetone/magpie/internal/gateway/xk/rewrite"
	"github.com/yetone/magpie/internal/provider"
)

func TestSemanticBudgetCoversNativeFallback(t *testing.T) {
	semanticTestEnv(t)
	calls := 0
	semanticProvider(t, provider.Chat, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"content":"I can't help with that."},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":3}}`)
	})
	p, _ := provider.Find("semantic-test")
	limits := govern.DefaultLimits()
	limits.MaxUpstreamCalls = 1
	r, state, cancel := xkRequest(httptest.NewRequest("POST", "/v1/chat/completions", nil), limits)
	defer cancel()
	s := New()
	body := []byte(`{"model":"m1","messages":[{"role":"user","content":"hi"}]}`)
	for _, semantic := range []bool{true, false} {
		p.Semantic = semantic
		var call Call
		code, _ := s.attempt(httptest.NewRecorder(), r, provider.Chat, *p, "m1", body, &call)
		if code != 503 {
			t.Fatalf("semantic=%v status=%d", semantic, code)
		}
	}
	if calls != 1 || state.budget.TokensUsed() != 13 {
		t.Fatalf("request budget reset across candidates: calls=%d tokens=%d", calls, state.budget.TokensUsed())
	}
}

func TestSemanticDeadlineCancelsNativeUpstream(t *testing.T) {
	semanticTestEnv(t)
	canceled := make(chan struct{})
	semanticProvider(t, provider.Chat, func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
			close(canceled)
		case <-time.After(5 * time.Second):
			w.WriteHeader(504)
		}
	})
	p, _ := provider.Find("semantic-test")
	limits := govern.DefaultLimits()
	limits.WallClockMS = 500
	r, _, cancel := xkRequest(httptest.NewRequest("POST", "/v1/chat/completions", nil), limits)
	defer cancel()
	var call Call
	New().attempt(httptest.NewRecorder(), r, provider.Chat, *p, "m1", []byte(`{"model":"m1","messages":[{"role":"user","content":"hi"}]}`), &call)
	if r.Context().Err() != context.DeadlineExceeded {
		t.Fatalf("deadline not shared with upstream: %v", r.Context().Err())
	}
	select {
	case <-canceled:
	case <-time.After(2 * time.Second):
		t.Fatal("native upstream remained active after semantic deadline")
	}
}

func TestSemanticNoNativeReplayAfterGatewayTools(t *testing.T) {
	semanticTestEnv(t)
	fake := &capFake{reply: func(call int, _ map[string]any) map[string]any {
		if call == 1 {
			return capToolCall("xk__skill_load", map[string]any{"name": "rev"})
		}
		return map[string]any{"error": map[string]any{"message": "max_tokens must be at least 1024"}}
	}}
	capSetup(t, fake)
	p, _ := provider.Find("xkeysmith")
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(fake.seen) > 0 {
			w.WriteHeader(400)
		}
		fake.ServeHTTP(w, r)
	}))
	t.Cleanup(up.Close)
	p.Chat, p.Responses = up.URL+"/v1", ""
	p.Fallback = []string{"spare/m1"}
	if err := provider.Save(*p); err != nil {
		t.Fatal(err)
	}
	spareCalls := 0
	spare := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		spareCalls++
		json.NewEncoder(w).Encode(chatAnswer("spare"))
	}))
	t.Cleanup(spare.Close)
	if err := provider.Save(provider.Provider{ID: "spare", Key: "fixture", Models: []string{"m1"}, Chat: spare.URL + "/v1"}); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	New().Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"xkeysmith/gpt-5","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != 400 || len(fake.seen) != 2 || spareCalls != 0 {
		t.Fatalf("request replayed after tools: status=%d calls=%d spare=%d body=%s", rec.Code, len(fake.seen), spareCalls, rec.Body.String())
	}
}

func TestSemanticAuditTracksEachLayerAndTool(t *testing.T) {
	semanticTestEnv(t)
	fake := &capFake{reply: func(call int, _ map[string]any) map[string]any {
		switch call {
		case 1:
			return chatAnswer("I can't help with that.")
		case 2:
			return capToolCall("xk__skill_load", map[string]any{"name": "rev"})
		default:
			return chatAnswer("done")
		}
	}}
	capSetup(t, fake)
	s := New()
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"xkeysmith/gpt-5","messages":[{"role":"user","content":"hi"}]}`)))
	trace := rec.Header().Get("x-xkeysmith-trace")
	lines := audit.NewReader(s.xkAuditDir()).ByTrace(trace)
	if rec.Code != 200 || trace == "" || len(lines) != 4 {
		t.Fatalf("trace=%q status=%d records=%d", trace, rec.Code, len(lines))
	}
	for i, line := range lines {
		if line.Trace.String() != trace || line.Seq != uint32(i) || line.Session != lines[0].Session {
			t.Fatalf("trace identity differs: %+v", line)
		}
	}
	if lines[0].ResponsePreview == nil || !strings.Contains(*lines[0].ResponsePreview, "I can't help") || lines[1].ResponsePreview == nil || !strings.Contains(*lines[1].ResponsePreview, "xk__skill_load") || lines[2].ToolCall == nil || lines[2].ToolCall.Tool != "xk__skill_load" || lines[3].RequestPreview == nil || !strings.Contains(*lines[3].RequestPreview, "逆向正文") {
		t.Fatalf("audit previews do not describe the actual layer and tool: %+v", lines)
	}
}

func TestSemanticStructuredRefusalSurvivesNativeDecoding(t *testing.T) {
	for _, tc := range []struct {
		proto string
		body  string
		want  string
	}{
		{proto: "chat", body: `{"choices":[{"message":{"content":"Here is the answer.","refusal":"policy decision"},"finish_reason":"stop"}]}`, want: "policy decision"},
		{proto: "chat", body: `data: {"choices":[{"delta":{"content":"Here is the answer.","refusal":"policy decision"},"finish_reason":"stop"}]}` + "\n\ndata: [DONE]\n\n", want: "policy decision"},
		{proto: "anthropic", body: `{"type":"message","content":[{"type":"refusal","refusal":"policy decision"}],"stop_reason":"refusal"}`, want: "policy decision"},
		{proto: "anthropic", body: "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"refusal\"}}\n\n", want: "refusal"},
		{proto: "responses", body: "event: response.refusal.delta\ndata: {\"type\":\"response.refusal.delta\",\"delta\":\"policy decision\"}\n\nevent: response.refusal.done\ndata: {\"type\":\"response.refusal.done\",\"refusal\":\"policy decision\"}\n\n", want: "policy decision"},
		{proto: "gemini", body: `{"promptFeedback":{"blockReason":"PROHIBITED_CONTENT","blockReasonMessage":"blocked"}}`, want: "blocked"},
		{proto: "gemini", body: "data: {\"candidates\":[{\"content\":{\"parts\":[]},\"finishReason\":\"SAFETY\",\"finishMessage\":\"blocked\"}]}\n\n", want: "blocked"},
		{proto: "gemini", body: `{"response":{"candidates":[{"content":{"parts":[]},"finishReason":"SAFETY","finishMessage":"blocked"}]}}`, want: "blocked"},
	} {
		obs := xkObservation(tc.proto, attach.ProbeResult{Status: 200, Body: []byte(tc.body)})
		if obs.RefusalField != tc.want {
			t.Fatalf("%s structured refusal disappeared: got=%q want=%q obs=%+v", tc.proto, obs.RefusalField, tc.want, obs)
		}
	}
}

func TestSemanticStreamErrorKeepsProtocolContentType(t *testing.T) {
	body := []byte("event: error\ndata: {\"type\":\"error\",\"error\":{\"message\":\"fixture upstream failed\"}}\n\n")
	rec := httptest.NewRecorder()
	xkWriteProbe(rec, nil, attach.ProbeResult{Status: 200, Body: body, Stream: true}, provider.Anthropic)
	if !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/event-stream") || rec.Body.String() != string(body) {
		t.Fatalf("SSE error rendered as JSON: %q %s", rec.Header().Get("Content-Type"), rec.Body.String())
	}
}

func TestSemanticL4RestoresAcrossProtocolEvents(t *testing.T) {
	pipeline, _ := rewrite.NewPipeline(rewrite.DefaultRewriteConfig(), []govern.LayerID{govern.L0, govern.L4})
	env, _ := xkEnvelope(provider.Chat, []byte(`{"messages":[{"role":"user","content":"connect to 10.20.30.40"}]}`))
	attempt, err := pipeline.Attempt(env, govern.L4)
	if err != nil || attempt.Vault.Len() != 1 {
		t.Fatalf("masking fixture: %v", err)
	}
	ph := attempt.Vault.Entries()[0].Placeholder
	cut := len(ph) / 2
	chunks := []any{
		map[string]any{"id": "c1", "model": "m1", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "connect " + ph[:cut]}}}},
		map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": ph[cut:] + " now"}, "finish_reason": "stop"}}},
	}
	var stream strings.Builder
	for _, chunk := range chunks {
		b, _ := json.Marshal(chunk)
		stream.WriteString("data: " + string(b) + "\n\n")
	}
	stream.WriteString("data: [DONE]\n\n")
	rec := httptest.NewRecorder()
	xkWriteStream(rec, &attach.LadderResult{Attempt: attempt, Probe: attach.ProbeResult{Status: 200, Stream: true, Body: []byte(stream.String())}}, provider.Chat)
	reply, err := xkReply(provider.Chat, attach.ProbeResult{Status: 200, Stream: true, Body: rec.Body.Bytes()})
	var text string
	for _, part := range reply.Parts {
		if part.Kind == Text {
			text += part.Text
		}
	}
	if err != nil || text != "connect 10.20.30.40 now" || strings.Contains(rec.Body.String(), "«TGT") {
		t.Fatalf("split placeholder corrupted protocol events: text=%q err=%v stream=%s", text, err, rec.Body.String())
	}
}
