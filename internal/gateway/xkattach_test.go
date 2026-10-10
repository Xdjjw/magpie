package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/provider"
)

// 阶段③/④ 的**真实网关**端到端验证:请求经 New().Handler() 的真实 HTTP
// 路径,provider 是 xkeysmith preset,上游是假服务器(按层递进拒绝)。
// 这覆盖接线本身——单测只覆盖了 attach 包,接线没跑过就不算完成。

// xkFake 按调用序号决定回什么:前 refuseUntil 次回拒绝。
type xkFake struct {
	calls       int
	refuseUntil int
	seen        []string
	// maskEcho:把请求里的占位符原样回显(验证响应侧还原)。
	maskEcho bool
}

func (f *xkFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.calls++
	f.seen = append(f.seen, string(body))
	w.Header().Set("Content-Type", "application/json")
	if f.calls <= f.refuseUntil {
		io.WriteString(w, `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"I can't help with that."}}]}`)
		return
	}
	content := "Here you go:\n```\nok\n```"
	if f.maskEcho {
		if i := strings.Index(string(body), "«TGT-"); i >= 0 {
			if j := strings.Index(string(body)[i:], "»"); j > 0 {
				content = "connecting to " + string(body)[i:i+j+len("»")] + " now"
			}
		}
	}
	io.WriteString(w, `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":`+jsonString(content)+`}}]}`)
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// xkSetup 注册一个 xkeysmith preset provider,指向假上游。
func xkSetup(t *testing.T, f *xkFake) *httptest.Server {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("MAGPIE_DEBUG", "")
	up := httptest.NewServer(f)
	t.Cleanup(up.Close)
	// xkeysmith 的 provider 用 Chat base 指向假上游。
	p := provider.Provider{
		ID: "xkeysmith", Name: "xkeysmith", Preset: "xkeysmith",
		Key: "k", Models: []string{"gpt-5"},
		Chat: up.URL + "/v1", Responses: up.URL + "/v1",
	}
	if err := provider.Save(p); err != nil {
		t.Fatal(err)
	}
	return up
}

func xkPost(t *testing.T, path, body string) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", path, strings.NewReader(body))
	New().Handler().ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func TestXkeysmithLadderRunsThroughRealGateway(t *testing.T) {
	fake := &xkFake{refuseUntil: 2} // L0、L1 拒,L2 过
	xkSetup(t, fake)

	code, body := xkPost(t, "/v1/chat/completions",
		`{"model":"xkeysmith/gpt-5","messages":[{"role":"user","content":"explain the crack tool"}]}`)
	if code != 200 {
		t.Fatalf("code=%d body=%s", code, body)
	}
	if !strings.Contains(body, "ok") {
		t.Fatalf("body=%s", body)
	}
	if fake.calls != 3 {
		t.Fatalf("阶梯应在真实路径上升级:calls=%d want 3", fake.calls)
	}
	// 层是累积的:L2 请求里应保留 L1 注入
	if !strings.Contains(fake.seen[2], "local-audit-lab") {
		t.Fatalf("L2 请求缺少 L1 注入: %s", fake.seen[2])
	}
}

func TestXkeysmithExhaustedGivesHonestError(t *testing.T) {
	fake := &xkFake{refuseUntil: 99}
	xkSetup(t, fake)

	code, body := xkPost(t, "/v1/chat/completions",
		`{"model":"xkeysmith/gpt-5","messages":[{"role":"user","content":"do it"}]}`)
	if code != 503 {
		t.Fatalf("全层耗尽必须给明确错误(不伪造成功):code=%d body=%s", code, body)
	}
	if !strings.Contains(body, "exhausted") {
		t.Fatalf("body=%s", body)
	}
}

func TestXkeysmithL4RestoresResponse(t *testing.T) {
	// 前四层都被拒,L4 才被采纳——这正是 L4 存在的场景。
	fake := &xkFake{refuseUntil: 4, maskEcho: true}
	xkSetup(t, fake)

	code, body := xkPost(t, "/v1/chat/completions",
		`{"model":"xkeysmith/gpt-5","messages":[{"role":"user","content":"connect to 10.20.30.40"}]}`)
	if code != 200 {
		t.Fatalf("code=%d body=%s", code, body)
	}
	// 只有**被采纳的 L4** 该是占位符;L0–L3 本来就含真值。
	last := fake.seen[len(fake.seen)-1]
	if !strings.Contains(last, "«TGT-") || strings.Contains(last, "10.20.30.40") {
		t.Fatalf("L4 必须让上游看到占位符: %s", last)
	}
	// 客户端拿回的是真值
	if strings.Contains(body, "«TGT-") || !strings.Contains(body, "10.20.30.40") {
		t.Fatalf("响应侧未还原: %s", body)
	}
}

func TestXkeysmithWritesAuditTrail(t *testing.T) {
	fake := &xkFake{refuseUntil: 1}
	xkSetup(t, fake)

	// 审计目录落在 XDG_CONFIG_HOME 下的 magpie 配置目录里。
	code, body := xkPost(t, "/v1/chat/completions",
		`{"model":"xkeysmith/gpt-5","messages":[{"role":"user","content":"hello"}]}`)
	if code != 200 {
		t.Fatalf("code=%d body=%s", code, body)
	}

	// 只认语义通路自己的审计目录(magpie 另有自己的调用日志 JSONL)。
	var files []string
	dir := New().xkAuditDir()
	_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && strings.HasSuffix(path, ".jsonl") {
			files = append(files, path)
		}
		return nil
	})
	if len(files) == 0 {
		t.Fatalf("没有写出审计 JSONL(语义通路的审计落盘未生效); dir=%s", dir)
	}
	data, _ := os.ReadFile(files[0])
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) < 2 {
		t.Fatalf("审计应每条 attempt 一条记录,实际 %d 行: %s", len(lines), data)
	}
	var first map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("审计行不是合法 JSON: %v (%s)", err, lines[0])
	}
	for _, key := range []string{"trace", "session", "layer", "verdict", "outcome", "request_hash"} {
		if _, ok := first[key]; !ok {
			t.Fatalf("审计记录缺少字段 %q: %s", key, lines[0])
		}
	}
}

func TestXkeysmithUpstreamErrorPassesThrough(t *testing.T) {
	// 上游故障不是拒绝:不升级、不透传为 503,而是原样给客户端。
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(502)
		io.WriteString(w, `{"error":{"message":"bad gateway"}}`)
	}))
	t.Cleanup(up.Close)
	if err := provider.Save(provider.Provider{
		ID: "xkeysmith", Name: "xkeysmith", Preset: "xkeysmith", Key: "k",
		Models: []string{"gpt-5"}, Chat: up.URL + "/v1", Responses: up.URL + "/v1",
	}); err != nil {
		t.Fatal(err)
	}
	code, body := xkPost(t, "/v1/chat/completions",
		`{"model":"xkeysmith/gpt-5","messages":[{"role":"user","content":"hello"}]}`)
	if code != 502 {
		t.Fatalf("上游故障应原样透传(非 503):code=%d body=%s", code, body)
	}
	if !strings.Contains(body, "bad gateway") {
		t.Fatalf("body=%s", body)
	}
}

func TestXkeysmithTraceHeaders(t *testing.T) {
	fake := &xkFake{refuseUntil: 1} // L0 拒 → L1 采纳(升级过)
	xkSetup(t, fake)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(
		`{"model":"xkeysmith/gpt-5","messages":[{"role":"user","content":"hi"}]}`))
	New().Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	if got := rec.Header().Get("x-xkeysmith-layer"); got != "L1" {
		t.Fatalf("layer 头=%q want L1", got)
	}
	if got := rec.Header().Get("x-xkeysmith-escalated"); got != "1" {
		t.Fatalf("escalated 头=%q want 1", got)
	}
}

// 流式客户端 + 非流式探针:必须翻译成真 SSE,而不是把 JSON 塞给期待 SSE 的客户端。
// (上游静默忽略 stream 是常见现象,xkeysmith 为此专门做过协议修复。)
func TestXkeysmithStreamClientGetsRealSSE(t *testing.T) {
	fake := &xkFake{refuseUntil: 0} // 直接采纳 L0
	xkSetup(t, fake)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(
		`{"model":"xkeysmith/gpt-5","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	New().Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	ct := rec.Header().Get("content-type")
	body := rec.Body.String()
	if !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("流式客户端必须收到 SSE,实际 content-type=%q body=%s", ct, body)
	}
	if !strings.Contains(body, "data:") {
		t.Fatalf("SSE 正文缺少 data 行: %s", body)
	}
}

// 流式 + L4:合成的 SSE 里也必须把占位符还原成真值。
func TestXkeysmithStreamRestoresPlaceholders(t *testing.T) {
	fake := &xkFake{refuseUntil: 4, maskEcho: true} // 一路升到 L4
	xkSetup(t, fake)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(
		`{"model":"xkeysmith/gpt-5","stream":true,"messages":[{"role":"user","content":"connect to 10.20.30.40"}]}`))
	New().Handler().ServeHTTP(rec, req)
	body := rec.Body.String()
	if !strings.Contains(rec.Header().Get("content-type"), "text/event-stream") {
		t.Fatalf("必须是 SSE: %q", rec.Header().Get("content-type"))
	}
	if strings.Contains(body, "«TGT-") {
		t.Fatalf("SSE 里不得残留占位符: %s", body)
	}
	if !strings.Contains(body, "10.20.30.40") {
		t.Fatalf("SSE 里必须还原成真值: %s", body)
	}
}
