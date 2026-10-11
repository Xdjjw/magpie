package gateway

// 另两条协议路径的语义通路覆盖:Anthropic `/v1/messages` 与
// Responses `/v1/responses`(Codex 默认走后者)。

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/provider"
)

// protoFake 按协议回复:记下请求,回拒绝或通过。
type protoFake struct {
	path        string
	calls       int
	refuseUntil int
	seen        []string
}

func (f *protoFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	f.calls++
	f.seen = append(f.seen, string(raw))
	w.Header().Set("content-type", "application/json")
	if f.calls <= f.refuseUntil {
		switch f.path {
		case "anthropic":
			_, _ = w.Write([]byte(`{"type":"message","stop_reason":"refusal","content":[{"type":"text","text":"I can't help with that."}]}`))
		default:
			_, _ = w.Write([]byte(`{"id":"resp_fixture","object":"response","model":"m","status":"completed","output":[{"id":"msg_fixture","type":"message","role":"assistant","content":[{"type":"output_text","text":"I can't help with that."}]}]}`))
		}
		return
	}
	switch f.path {
	case "anthropic":
		_, _ = w.Write([]byte(`{"type":"message","stop_reason":"end_turn","content":[{"type":"text","text":"ok from anthropic"}]}`))
	default:
		_, _ = w.Write([]byte(`{"id":"resp_fixture","object":"response","model":"m","status":"completed","output":[{"id":"msg_fixture","type":"message","role":"assistant","content":[{"type":"output_text","text":"ok from responses"}]}]}`))
	}
}

func protoSetup(t *testing.T, path string, f *protoFake) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	up := httptest.NewServer(f)
	t.Cleanup(up.Close)
	p := provider.Provider{
		ID: "xkeysmith", Name: "xkeysmith", Preset: "xkeysmith", Key: "k",
		Models: []string{"m"},
	}
	if path == "anthropic" {
		p.Anthropic = up.URL
	} else {
		p.Chat = up.URL + "/v1"
		p.Responses = up.URL + "/v1"
	}
	if err := provider.Save(p); err != nil {
		t.Fatal(err)
	}
}

func TestXkeysmithAnthropicPath(t *testing.T) {
	fake := &protoFake{path: "anthropic", refuseUntil: 1} // L0 拒 → L1 采纳
	protoSetup(t, "anthropic", fake)

	code, body := xkPost(t, "/v1/messages",
		`{"model":"xkeysmith/m","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	if code != 200 {
		t.Fatalf("code=%d body=%s", code, body)
	}
	if !strings.Contains(body, "ok from anthropic") {
		t.Fatalf("body=%s", body)
	}
	if fake.calls != 2 {
		t.Fatalf("应在 Anthropic 路径上升级:calls=%d", fake.calls)
	}
	// 层累积:L1 请求里应有注入
	if !strings.Contains(fake.seen[1], "local-audit-lab") {
		t.Fatalf("L1 注入缺失: %s", fake.seen[1])
	}
	// Anthropic 形状的 system 是顶层字段,不是 messages 轮
	var doc map[string]any
	_ = json.Unmarshal([]byte(fake.seen[1]), &doc)
	if _, ok := doc["system"]; !ok {
		if _, hasMsgs := doc["messages"]; hasMsgs {
			// 允许实现走 messages,但顶层 system 是 Anthropic 的正确位置
			t.Logf("note: system 未写在顶层,doc=%v", doc)
		}
	}
}

func TestXkeysmithResponsesPath(t *testing.T) {
	fake := &protoFake{path: "responses", refuseUntil: 1}
	protoSetup(t, "responses", fake)

	code, body := xkPost(t, "/v1/responses",
		`{"model":"xkeysmith/m","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)
	if code != 200 {
		t.Fatalf("code=%d body=%s", code, body)
	}
	if !strings.Contains(body, "ok from responses") {
		t.Fatalf("body=%s", body)
	}
	if fake.calls != 2 {
		t.Fatalf("应在 Responses 路径上升级:calls=%d", fake.calls)
	}
	// Responses 的 system 等价物是顶层 instructions——不处理这条会静默失效。
	if !strings.Contains(fake.seen[1], "instructions") {
		t.Fatalf("Responses 的注入必须落到 instructions: %s", fake.seen[1])
	}
}
