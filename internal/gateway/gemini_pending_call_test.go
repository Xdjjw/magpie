package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/tidwall/gjson"
	"github.com/yetone/magpie/internal/provider"
)

// geminiPairingUpstream is a Gemini API that reads its histories as
// Google's does: every turn holding functionCall parts must be followed
// right away by a turn whose functionResponse parts answer each of those
// calls ("Please ensure that the number of function response parts is
// equal to the number of function call parts of the function call turn",
// ai.google.dev/gemini-api/docs/troubleshooting). One that isn't is turned
// away 400, the whole request with it. What it takes, it answers.
func geminiPairingUpstream(t *testing.T) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var bodies []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if !strings.HasSuffix(r.URL.Path, ":streamGenerateContent") {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		if geminiMixesResponseAndText(b) {
			// Vertex AI's answer to a user turn mixing a functionResponse
			// with text (#1533)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, vertexModelLast)
			return
		}
		if !geminiAnswersEveryCall(b) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"error":{"code":400,"message":"Please ensure that the number of function response parts is equal to the number of function call parts of the function call turn.","status":"INVALID_ARGUMENT"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, sse(
			`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"paired-"}]}}],"modelVersion":"gm"}`,
			`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"up"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":3,"totalTokenCount":10}}`,
		))
	}))
	t.Cleanup(up.Close)
	return up, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), bodies...)
	}
}

// geminiMixesResponseAndText reports whether a turn of body's contents
// holds a functionResponse beside text.
func geminiMixesResponseAndText(body []byte) bool {
	for _, c := range gjson.GetBytes(body, "contents").Array() {
		resp, text := false, false
		for _, p := range c.Get("parts").Array() {
			resp = resp || p.Get("functionResponse").Exists()
			text = text || p.Get("text").Exists()
		}
		if resp && text {
			return true
		}
	}
	return false
}

// geminiAnswersEveryCall reports whether body's contents hold, right after
// every turn that calls functions, a turn answering each of those calls.
func geminiAnswersEveryCall(body []byte) bool {
	var q struct {
		Contents []struct {
			Role  string `json:"role"`
			Parts []struct {
				FunctionCall *struct {
					Name string `json:"name"`
					ID   string `json:"id"`
				} `json:"functionCall"`
				FunctionResponse *struct {
					Name string `json:"name"`
					ID   string `json:"id"`
				} `json:"functionResponse"`
			} `json:"parts"`
		} `json:"contents"`
	}
	if json.Unmarshal(body, &q) != nil {
		return false
	}
	type call struct{ name, id string }
	var want []call
	for _, c := range q.Contents {
		if len(want) > 0 {
			// the turn right after the calls answers each of them
			got := map[string]bool{}
			for _, p := range c.Parts {
				if p.FunctionResponse == nil {
					continue
				}
				if p.FunctionResponse.ID != "" {
					got[p.FunctionResponse.ID] = true
				} else {
					got[p.FunctionResponse.Name] = true
				}
			}
			for _, w := range want {
				key := w.id
				if key == "" {
					key = w.name
				}
				if !got[key] {
					return false
				}
			}
			want = nil
		}
		for _, p := range c.Parts {
			if p.FunctionCall != nil {
				want = append(want, call{p.FunctionCall.Name, p.FunctionCall.ID})
			}
		}
	}
	return len(want) == 0
}

// A turn the agent stopped with its tool call still out (the user went on
// in the same conversation) is replayed by the client with the call and no
// result. Google turns a history whose call turn goes unanswered away with
// 400, every later turn with it, so the chat build answers such a call
// with a synthetic result (pairToolMessages, #1275). The Gemini build,
// which serves a Google sign-in, Vertex AI and a custom provider's Gemini
// API alike, sent the call as it was.
func TestGeminiInterruptedCallIsAnswered(t *testing.T) {
	up, bodies := geminiPairingUpstream(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	if err := provider.Save(provider.Provider{ID: "gem", Name: "Gem", Key: "AIza-test", Gemini: up.URL + "/v1beta", Models: []string{"gemini-2.5-pro"}}); err != nil {
		t.Fatal(err)
	}
	gw := httptest.NewServer(New().Handler())
	defer gw.Close()
	res, err := http.Post(gw.URL+"/v1/chat/completions", "application/json", strings.NewReader(
		`{"model":"gem/gemini-2.5-pro","stream":true,"messages":[
			{"role":"user","content":"look into it"},
			{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"Bash","arguments":"{\"command\":\"date\"}"}}]},
			{"role":"user","content":"continue"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 || !strings.Contains(string(b), "paired-") {
		t.Fatalf("status %d: %s", res.StatusCode, b)
	}
	bs := bodies()
	if len(bs) != 1 {
		t.Fatalf("upstream asked %d times", len(bs))
	}
	if !strings.Contains(gjson.Get(bs[0], "contents").Raw, "the turn was interrupted") {
		t.Fatalf("no synthetic result sent: %s", bs[0])
	}
}

// One call of a parallel turn answered, its mate left out by the stop: the
// turn that follows the calls answers both, the client's own beside the
// synthetic one.
func TestGeminiParallelCallsOneAnswered(t *testing.T) {
	up, bodies := geminiPairingUpstream(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	if err := provider.Save(provider.Provider{ID: "gem", Name: "Gem", Key: "AIza-test", Gemini: up.URL + "/v1beta", Models: []string{"gemini-2.5-pro"}}); err != nil {
		t.Fatal(err)
	}
	gw := httptest.NewServer(New().Handler())
	defer gw.Close()
	res, err := http.Post(gw.URL+"/v1/chat/completions", "application/json", strings.NewReader(
		`{"model":"gem/gemini-2.5-pro","stream":true,"messages":[
			{"role":"user","content":"look into it"},
			{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"A","arguments":"{}"}},{"id":"call_2","type":"function","function":{"name":"B","arguments":"{}"}}]},
			{"role":"tool","tool_call_id":"call_1","content":"1"},
			{"role":"user","content":"continue"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 || !strings.Contains(string(b), "paired-") {
		t.Fatalf("status %d: %s", res.StatusCode, b)
	}
	bs := bodies()
	if len(bs) != 1 {
		t.Fatalf("upstream asked %d times", len(bs))
	}
	if !strings.Contains(gjson.Get(bs[0], "contents").Raw, "the turn was interrupted") {
		t.Fatalf("no synthetic result sent: %s", bs[0])
	}
}

// The same interrupted turn from a Messages client (Claude Code's history
// holds the tool_use and no result).
func TestGeminiMessagesClientInterruptedCall(t *testing.T) {
	up, bodies := geminiPairingUpstream(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	if err := provider.Save(provider.Provider{ID: "gem", Name: "Gem", Key: "AIza-test", Gemini: up.URL + "/v1beta", Models: []string{"gemini-2.5-pro"}}); err != nil {
		t.Fatal(err)
	}
	gw := httptest.NewServer(New().Handler())
	defer gw.Close()
	res, err := http.Post(gw.URL+"/v1/messages", "application/json", strings.NewReader(
		`{"model":"gem/gemini-2.5-pro","max_tokens":64,"stream":true,"messages":[
			{"role":"user","content":"look into it"},
			{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"date"}}]},
			{"role":"user","content":"continue"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 || !strings.Contains(string(b), "paired-") {
		t.Fatalf("status %d: %s", res.StatusCode, b)
	}
	bs := bodies()
	if len(bs) != 1 {
		t.Fatalf("upstream asked %d times", len(bs))
	}
	if !strings.Contains(gjson.Get(bs[0], "contents").Raw, "the turn was interrupted") {
		t.Fatalf("no synthetic result sent: %s", bs[0])
	}
}

// A history whose every call is answered goes as it was.
func TestGeminiAnsweredCallsUnchanged(t *testing.T) {
	up, bodies := geminiPairingUpstream(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	if err := provider.Save(provider.Provider{ID: "gem", Name: "Gem", Key: "AIza-test", Gemini: up.URL + "/v1beta", Models: []string{"gemini-2.5-pro"}}); err != nil {
		t.Fatal(err)
	}
	gw := httptest.NewServer(New().Handler())
	defer gw.Close()
	res, err := http.Post(gw.URL+"/v1/chat/completions", "application/json", strings.NewReader(
		`{"model":"gem/gemini-2.5-pro","stream":true,"messages":[
			{"role":"user","content":"look into it"},
			{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"Bash","arguments":"{\"command\":\"date\"}"}}]},
			{"role":"tool","tool_call_id":"call_1","content":"Thu Oct 8"},
			{"role":"assistant","content":"done"},
			{"role":"user","content":"thanks"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 || !strings.Contains(string(b), "paired-") {
		t.Fatalf("status %d: %s", res.StatusCode, b)
	}
	bs := bodies()
	if len(bs) != 1 {
		t.Fatalf("upstream asked %d times", len(bs))
	}
	if strings.Contains(gjson.Get(bs[0], "contents").Raw, "the turn was interrupted") {
		t.Fatalf("synthetic result sent beside the client's own: %s", bs[0])
	}
}

// The synthetic answer goes in a turn of its own before what the user said
// (a turn mixing a functionResponse with text is turned away, #1533), in
// the turn of answers the client already sent, or, for a Claude model on
// Antigravity, in the one tool_result turn; a call with no id answered by
// name is left as it is.
func TestPairFunctionTurnsShapes(t *testing.T) {
	call := func(id, name string) map[string]any {
		fc := map[string]any{"name": name}
		if id != "" {
			fc["id"] = id
		}
		return map[string]any{"functionCall": fc}
	}
	resp := func(id, name string) map[string]any {
		fr := map[string]any{"name": name, "response": map[string]any{"output": "ok"}}
		if id != "" {
			fr["id"] = id
		}
		return map[string]any{"functionResponse": fr}
	}
	text := map[string]any{"text": "continue"}
	turn := func(role string, parts ...map[string]any) map[string]any {
		return map[string]any{"role": role, "parts": parts}
	}
	shape := func(cs []map[string]any) string {
		var out []string
		for _, c := range cs {
			s := c["role"].(string) + ":"
			for _, p := range c["parts"].([]map[string]any) {
				switch {
				case p["functionCall"] != nil:
					s += "C"
				case p["functionResponse"] != nil:
					s += "R"
				default:
					s += "T"
				}
			}
			out = append(out, s)
		}
		return strings.Join(out, " ")
	}
	for _, c := range []struct {
		name     string
		in       []map[string]any
		agClaude bool
		want     string
	}{
		{"before the user's words", []map[string]any{turn("user", text), turn("model", call("a", "A")), turn("user", text)}, false, "user:T model:C user:R user:T"},
		{"beside the client's answers", []map[string]any{turn("user", text), turn("model", call("a", "A"), call("b", "B")), turn("user", resp("a", "A")), turn("user", text)}, false, "user:T model:CC user:RR user:T"},
		{"one tool_result turn on Antigravity's Claude", []map[string]any{turn("user", text), turn("model", call("a", "A")), turn("user", text)}, true, "user:T model:C user:RT"},
		{"last", []map[string]any{turn("user", text), turn("model", call("a", "A"))}, false, "user:T model:C user:R"},
		{"no id, answered by name", []map[string]any{turn("user", text), turn("model", call("", "A")), turn("user", resp("", "A")), turn("user", text)}, false, "user:T model:C user:R user:T"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := shape(pairFunctionTurns(c.in, false, c.agClaude)); got != c.want {
				t.Fatalf("got %s, want %s", got, c.want)
			}
		})
	}
}
