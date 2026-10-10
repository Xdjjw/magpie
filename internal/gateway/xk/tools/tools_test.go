package tools

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func itoa(n int) string { return strconv.Itoa(n) }

// 与 xp-tools 的 guard.rs / exec.rs / lib.rs 测试对应。

// fakeSkills 是 SkillSource 的最小实现。
type fakeSkills struct {
	names   []string
	unlocks []string
	bodies  map[string]string
	perName map[string][]string
}

func (f *fakeSkills) Len() int                     { return len(f.names) }
func (f *fakeSkills) Names() []string              { return f.names }
func (f *fakeSkills) Unlocks() []string            { return f.unlocks }
func (f *fakeSkills) UnlocksFor(n string) []string { return f.perName[n] }
func (f *fakeSkills) Body(n string) (string, bool) {
	b, ok := f.bodies[n]
	return b, ok
}

// ─────────────────────────── guard ───────────────────────────

func TestDenyModeOnlyAllowsAllowlisted(t *testing.T) {
	g := Guard{Approval: ApprovalDeny, Allowlist: []string{"xk__skill_load"}}
	if !g.Allows("xk__skill_load") || g.Allows("xk__mcp__fs__write_file") {
		t.Fatal("deny 模式只应放行白名单")
	}
	if !strings.Contains(g.DenialReason("x"), "allowlist") {
		t.Fatal("拒绝理由必须指明出路")
	}
}

func TestAllowModePermitsEverything(t *testing.T) {
	g := Guard{Approval: ApprovalAllow}
	if !g.Allows("anything") {
		t.Fatal("allow 模式应全部放行")
	}
}

func TestWildcardsMatchToolNames(t *testing.T) {
	g := Guard{Approval: ApprovalDeny, Allowlist: []string{"xk__mcp__fs__*", "xk__skill_load"}}
	if !g.Allows("xk__mcp__fs__read_file") || g.Allows("xk__mcp__git__push") {
		t.Fatal("通配匹配不正确")
	}
}

func TestGlobMatcherEdges(t *testing.T) {
	cases := []struct {
		pattern, text string
		want          bool
	}{
		{"*", "x", true}, {"abc", "abc", true}, {"abc", "abcd", false},
		{"a*c", "abc", true}, {"a*", "abc", true}, {"*c", "abc", true},
		{"*d", "abc", false}, {"a*b*c", "axxbyyc", true}, {"a*b*c", "axxbyyd", false},
		{"xk__mcp__fs__*", "xk__mcp__fs__read", true},
	}
	for _, c := range cases {
		if got := GlobMatch(c.pattern, c.text); got != c.want {
			t.Fatalf("GlobMatch(%q,%q)=%v want %v", c.pattern, c.text, got, c.want)
		}
	}
}

func TestEmptyAllowlistDeniesEverythingInDenyMode(t *testing.T) {
	g := Guard{Approval: ApprovalDeny}
	if g.Allows("xk__skill_load") {
		t.Fatal("空白名单在 deny 模式下应全部拒绝")
	}
}

// ─────────────────────────── plane ───────────────────────────

func skillsFixture() *fakeSkills {
	return &fakeSkills{
		names:   []string{"rev", "plain"},
		unlocks: []string{"xk__mcp__ida__*"},
		perName: map[string][]string{"rev": {"xk__mcp__ida__*"}},
		bodies:  map[string]string{"rev": "逆向规程正文", "plain": "普通技能"},
	}
}

func TestPlaneInjectsSkillToolAndGates(t *testing.T) {
	cfg := DefaultToolsConfig()
	cfg.Enabled = true
	p := Build(cfg, skillsFixture(), nil)

	if !p.IsActive() {
		t.Fatal("启用后应 active")
	}
	if !p.IsGated("xk__mcp__ida__decompile") {
		t.Fatal("命中 unlocks 的工具应被门控")
	}
	if p.IsGated(SkillLoad) {
		t.Fatal("技能加载工具永远可用,否则没人能解锁")
	}

	// 未解锁:门控工具不下发
	visible := p.VisibleDefs(UnlockSet{})
	for _, d := range visible {
		if strings.Contains(d.Name, "ida") {
			t.Fatalf("未解锁的门控工具不得下发: %v", d.Name)
		}
	}
	if len(visible) != 1 || visible[0].Name != SkillLoad {
		t.Fatalf("visible=%v", visible)
	}

	// 解锁后下发
	unlocked := p.VisibleDefs(UnlockSet{Patterns: []string{"xk__mcp__ida__*"}})
	if len(unlocked) != 1 {
		t.Fatalf("解锁集只影响门控工具的存在,这里没有对应 MCP 工具: %v", unlocked)
	}
}

func TestPlaneDisabledIsInactive(t *testing.T) {
	p := Build(DefaultToolsConfig(), skillsFixture(), nil) // enabled=false
	if p.IsActive() {
		t.Fatal("默认关闭")
	}
	if len(p.AllDefs()) != 0 {
		t.Fatal("关闭时不该注入任何工具")
	}
}

func TestDispositionHandsBackMixedTurns(t *testing.T) {
	cfg := DefaultToolsConfig()
	cfg.Enabled = true
	p := Build(cfg, skillsFixture(), nil)

	if got := p.Disposition(nil); got != TurnNotOurs {
		t.Fatal("没有调用 → 不插手")
	}
	if got := p.Disposition([]ToolCall{{Name: "read_file"}}); got != TurnNotOurs {
		t.Fatal("纯客户端工具 → 不插手")
	}
	if got := p.Disposition([]ToolCall{{Name: SkillLoad}}); got != TurnAllOurs {
		t.Fatal("全是网关工具 → 网关执行")
	}
	// 混入客户端工具 → **整轮交还**(客户端 agent loop 期望完整 assistant 轮)
	if got := p.Disposition([]ToolCall{{Name: SkillLoad}, {Name: "read_file"}}); got != TurnHandBack {
		t.Fatal("混合轮必须整轮交还")
	}
}

func TestExecuteSkillLoadReturnsBodyAndUnlocks(t *testing.T) {
	cfg := DefaultToolsConfig()
	cfg.Enabled = true
	p := Build(cfg, skillsFixture(), nil)
	res := p.Execute(ToolCall{
		ID: "1", Name: SkillLoad, Arguments: map[string]any{"name": "rev"},
	})
	if res.IsError || res.Content != "逆向规程正文" {
		t.Fatalf("res=%+v", res)
	}
	if len(res.Unlocked) != 1 || res.Unlocked[0] != "xk__mcp__ida__*" || res.UnlockedBy != "rev" {
		t.Fatalf("解锁必须随结果返回(流式路径要靠它持久化): %+v", res)
	}
}

func TestExecuteUnknownSkillListsAvailable(t *testing.T) {
	cfg := DefaultToolsConfig()
	cfg.Enabled = true
	p := Build(cfg, skillsFixture(), nil)
	res := p.Execute(ToolCall{Name: SkillLoad, Arguments: map[string]any{"name": "nope"}})
	if !res.IsError || !strings.Contains(res.Content, "rev") {
		t.Fatalf("未知技能应把可用列表回给模型: %+v", res)
	}
}

func TestDeniedCallIsAResultNotAHang(t *testing.T) {
	cfg := DefaultToolsConfig()
	cfg.Enabled = true
	cfg.Approval = ApprovalDeny
	cfg.Allowlist = []string{} // 什么都不放行
	p := Build(cfg, skillsFixture(), nil)
	res := p.Execute(ToolCall{Name: SkillLoad, Arguments: map[string]any{"name": "rev"}})
	if !res.Denied || !res.IsError {
		t.Fatalf("被拒也是结果(denied=true): %+v", res)
	}
	if !strings.Contains(res.Content, "allowlist") {
		t.Fatal("拒绝理由要能让模型改策略")
	}
}

func TestUnknownToolIsReported(t *testing.T) {
	cfg := DefaultToolsConfig()
	cfg.Enabled = true
	cfg.Approval = ApprovalAllow
	p := Build(cfg, skillsFixture(), nil)
	res := p.Execute(ToolCall{Name: "xk__nope"})
	if !res.IsError || !strings.Contains(res.Content, "未知工具") {
		t.Fatalf("res=%+v", res)
	}
}

func TestTruncateLabelsTheCut(t *testing.T) {
	cfg := DefaultToolsConfig()
	cfg.MaxResultChars = 10
	p := Build(cfg, skillsFixture(), nil)
	out := p.Truncate(strings.Repeat("字", 50))
	if !strings.Contains(out, "truncated") || len([]rune(out)) > 60 {
		t.Fatalf("out=%q", out)
	}
}

// ─────────────────────────── MCP stdio 桥 ───────────────────────────

// mcpEcho 是一个最小的 MCP stdio 服务器(供端到端测试用)。
const mcpEcho = `#!/bin/sh
while IFS= read -r line; do
  case "$line" in
    *'"method":"initialize"'*)
      id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9]*\).*/\1/p')
      printf '{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2024-11-05","capabilities":{},"serverInfo":{"name":"echo","version":"1"}}}\n' "$id" ;;
    *'"method":"tools/list"'*)
      id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9]*\).*/\1/p')
      printf '{"jsonrpc":"2.0","id":%s,"result":{"tools":[{"name":"ping","description":"echo a message","inputSchema":{"type":"object"}}]}}\n' "$id" ;;
    *'"method":"tools/call"'*)
      id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9]*\).*/\1/p')
      printf '{"jsonrpc":"2.0","id":%s,"result":{"content":[{"type":"text","text":"pong"}],"isError":false}}\n' "$id" ;;
    *'"method":"notifications'*) : ;;
  esac
done
`

func writeEchoServer(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "echo-mcp.sh")
	if err := os.WriteFile(path, []byte(mcpEcho), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestMcpStdioBridgesTools(t *testing.T) {
	script := writeEchoServer(t)
	cfg := DefaultToolsConfig()
	cfg.Enabled = true
	server := McpServerCfg{
		Name: "echo", Enabled: true, Transport: "stdio", Command: script,
	}
	p := Build(cfg, skillsFixture(), []McpServerCfg{server})
	defer p.Close()

	status := p.McpStatus()
	if len(status) != 1 || status[0].Error != "" || status[0].Tools != 1 {
		t.Fatalf("status=%+v", status)
	}
	var found bool
	for _, d := range p.AllDefs() {
		if d.Name == "xk__mcp__echo__ping" {
			found = true
		}
	}
	if !found {
		t.Fatalf("MCP 工具必须以 xk__mcp__<server>__<tool> 命名注入: %v", p.AllDefs())
	}
	res := p.Execute(ToolCall{Name: "xk__mcp__echo__ping", Arguments: map[string]any{}})
	if res.IsError || res.Content != "pong" {
		t.Fatalf("res=%+v", res)
	}
}

func TestMcpAllowToolsWhitelist(t *testing.T) {
	script := writeEchoServer(t)
	cfg := DefaultToolsConfig()
	cfg.Enabled = true
	server := McpServerCfg{
		Name: "echo", Enabled: true, Transport: "stdio", Command: script,
		AllowTools: []string{"other"}, // 不含 ping
	}
	p := Build(cfg, skillsFixture(), []McpServerCfg{server})
	defer p.Close()
	for _, d := range p.AllDefs() {
		if d.Name == "xk__mcp__echo__ping" {
			t.Fatal("allow_tools 白名单外的工具不得暴露")
		}
	}
}

func TestMcpFailureIsVisibleNotFatal(t *testing.T) {
	cfg := DefaultToolsConfig()
	cfg.Enabled = true
	server := McpServerCfg{
		Name: "broken", Enabled: true, Transport: "stdio",
		Command: "/nonexistent/mcp-server",
	}
	p := Build(cfg, skillsFixture(), []McpServerCfg{server})
	status := p.McpStatus()
	if len(status) != 1 || status[0].Error == "" {
		t.Fatalf("连不上必须可见(不阻塞启动): %+v", status)
	}
	// 技能工具仍然可用
	if !p.IsActive() {
		t.Fatal("一个 MCP 挂掉不该让整个平面不可用")
	}
}

func TestMcpCwdIsChecked(t *testing.T) {
	cfg := DefaultToolsConfig()
	cfg.Enabled = true
	server := McpServerCfg{
		Name: "badcwd", Enabled: true, Transport: "stdio",
		Command: "/bin/sh", Cwd: "/nonexistent/dir",
	}
	p := Build(cfg, skillsFixture(), []McpServerCfg{server})
	status := p.McpStatus()
	if len(status) != 1 || !strings.Contains(status[0].Error, "不是存在的目录") {
		t.Fatalf("cwd 预检必须给出可排查的错误: %+v", status)
	}
}

func TestEnvExpand(t *testing.T) {
	os.Setenv("XK_TEST_DIR", "/tmp/lab")
	defer os.Unsetenv("XK_TEST_DIR")
	if got := EnvExpand("${XK_TEST_DIR}/mcp"); got != "/tmp/lab/mcp" {
		t.Fatalf("got %q", got)
	}
	if got := EnvExpand("$XK_TEST_DIR/run.py"); got != "/tmp/lab/run.py" {
		t.Fatalf("got %q", got)
	}
	// 未定义的原样保留(展开成空串会把失败点推得离原因很远)
	if got := EnvExpand("${XK_UNDEFINED_XYZ}/x"); got != "${XK_UNDEFINED_XYZ}/x" {
		t.Fatalf("got %q", got)
	}
}

func TestIsGatewayTool(t *testing.T) {
	if !IsGatewayTool("xk__skill_load") || IsGatewayTool("read_file") {
		t.Fatal("前缀判定")
	}
}

var _ = json.Marshal

// ─────────────────────────── MCP Streamable HTTP ───────────────────────────

func TestMcpHTTPJsonAndSession(t *testing.T) {
	var sawSession string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			ID     int    `json:"id"`
			Method string `json:"method"`
		}
		_ = json.Unmarshal(body, &req)
		if sid := r.Header.Get("Mcp-Session-Id"); sid != "" {
			sawSession = sid
		}
		w.Header().Set("content-type", "application/json")
		switch req.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "sess-1")
			io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-06-18"}}`)
		case "tools/list":
			io.WriteString(w, `{"jsonrpc":"2.0","id":`+itoa(req.ID)+`,"result":{"tools":[{"name":"http_ping","description":"d","inputSchema":{"type":"object"}}]}}`)
		case "tools/call":
			io.WriteString(w, `{"jsonrpc":"2.0","id":`+itoa(req.ID)+`,"result":{"content":[{"type":"text","text":"http-pong"}],"isError":false}}`)
		default:
			io.WriteString(w, `{"jsonrpc":"2.0","id":`+itoa(req.ID)+`,"result":{}}`)
		}
	}))
	defer srv.Close()

	cfg := DefaultToolsConfig()
	cfg.Enabled = true
	p := Build(cfg, skillsFixture(), []McpServerCfg{{
		Name: "web", Enabled: true, Transport: "http", URL: srv.URL,
	}})
	defer p.Close()

	status := p.McpStatus()
	if len(status) != 1 || status[0].Error != "" || status[0].Tools != 1 {
		t.Fatalf("status=%+v", status)
	}
	if sawSession != "sess-1" {
		t.Fatalf("会话 id 必须带回去: %q", sawSession)
	}
	res := p.Execute(ToolCall{Name: "xk__mcp__web__http_ping", Arguments: map[string]any{}})
	if res.IsError || res.Content != "http-pong" {
		t.Fatalf("res=%+v", res)
	}
}

func TestMcpHTTPSSEResponse(t *testing.T) {
	// 服务器用 SSE 回,且**先发一条通知**(id 不匹配),再发真正的应答。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			ID     int    `json:"id"`
			Method string `json:"method"`
		}
		_ = json.Unmarshal(body, &req)
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, "data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\"}\n\n")
		switch req.Method {
		case "initialize":
			io.WriteString(w, `data: {"jsonrpc":"2.0","id":1,"result":{}}`+"\n\n")
		case "tools/list":
			io.WriteString(w, `data: {"jsonrpc":"2.0","id":`+itoa(req.ID)+`,"result":{"tools":[{"name":"sse_ping","inputSchema":{}}]}}`+"\n\n")
		case "tools/call":
			io.WriteString(w, `data: {"jsonrpc":"2.0","id":`+itoa(req.ID)+`,"result":{"content":[{"type":"text","text":"sse-pong"}]}}`+"\n\n")
		default:
			io.WriteString(w, `data: {"jsonrpc":"2.0","id":`+itoa(req.ID)+`,"result":{}}`+"\n\n")
		}
	}))
	defer srv.Close()

	cfg := DefaultToolsConfig()
	cfg.Enabled = true
	p := Build(cfg, skillsFixture(), []McpServerCfg{{
		Name: "sse", Enabled: true, Transport: "http", URL: srv.URL,
	}})
	defer p.Close()
	res := p.Execute(ToolCall{Name: "xk__mcp__sse__sse_ping", Arguments: map[string]any{}})
	if res.IsError || res.Content != "sse-pong" {
		t.Fatalf("SSE 应答必须被读到 id 匹配那条: %+v", res)
	}
}
