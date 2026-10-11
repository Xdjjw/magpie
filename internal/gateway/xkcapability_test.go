package gateway

// 能力平面(A 档技能注入 + B 档工具轮执行)的**真实网关**端到端验证。
// 这层最容易出错:配置装载、注入形状、工具轮执行都在接线上,单测覆盖不到。

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

// capFake 记录每次收到的请求体,并可按剧本回复。
type capFake struct {
	seen  []map[string]any
	reply func(call int, req map[string]any) map[string]any
}

func (f *capFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var doc map[string]any
	_ = json.Unmarshal(raw, &doc)
	f.seen = append(f.seen, doc)
	resp := f.reply(len(f.seen), doc)
	w.Header().Set("content-type", "application/json")
	out, _ := json.Marshal(resp)
	_, _ = w.Write(out)
}

// capSetup 起一个 xkeysmith provider 指向假上游,并准备技能目录。
func capSetup(t *testing.T, f *capFake) {
	t.Helper()
	cfgHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfgHome)
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	// 技能目录:一个带 unlocks 的技能。
	skillDir := filepath.Join(cfgHome, "skills")
	if err := os.MkdirAll(filepath.Join(skillDir, "rev"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "rev", "SKILL.md"),
		[]byte("---\nname: rev\ndescription: 逆向规程\nallowed-tools:\n  - xk__skill_load\nunlocks:\n  - xk__mcp__ida__*\n---\n逆向正文\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XKEYSMITH_SKILLS_DIR", skillDir)
	t.Setenv("XKEYSMITH_TOOLS", "1")

	up := httptest.NewServer(f)
	t.Cleanup(up.Close)
	if err := provider.Save(provider.Provider{
		ID: "xkeysmith", Name: "xkeysmith", Preset: "xkeysmith", Key: "k",
		Models: []string{"gpt-5"}, Chat: up.URL + "/v1", Responses: up.URL + "/v1",
	}); err != nil {
		t.Fatal(err)
	}
}

func capToolCall(name string, args map[string]any) map[string]any {
	raw, _ := json.Marshal(args)
	return map[string]any{
		"choices": []any{map[string]any{
			"finish_reason": "tool_calls",
			"message": map[string]any{
				"role": "assistant", "content": nil,
				"tool_calls": []any{map[string]any{
					"id": "c1", "type": "function",
					"function": map[string]any{"name": name, "arguments": string(raw)},
				}},
			},
		}},
	}
}

func chatAnswer(text string) map[string]any {
	return map[string]any{
		"choices": []any{map[string]any{
			"finish_reason": "stop",
			"message":       map[string]any{"role": "assistant", "content": text},
		}},
	}
}

// A 档:技能索引被注入到 L1 的 system 槽位,且 L0 保持 pristine。
func TestCapabilitySkillsInjectedAtL1(t *testing.T) {
	fake := &capFake{reply: func(int, map[string]any) map[string]any {
		return chatAnswer("Here you go:\n```\nok\n```")
	}}
	capSetup(t, fake)

	code, body := xkPost(t, "/v1/chat/completions",
		`{"model":"xkeysmith/gpt-5","messages":[{"role":"user","content":"hi"}]}`)
	if code != 200 {
		t.Fatalf("code=%d body=%s", code, body)
	}
	// 采纳层是 L0(L0 就通过)→ L1 未执行 → 不该有技能注入。
	if strings.Contains(joinBodies(fake.seen), "local-audit-lab/skills") {
		t.Fatal("L0 采纳时不该注入技能(L0 必须 pristine)")
	}

	// 让 L0 被拒 → 升到 L1,此时技能索引必须出现。
	fake2 := &capFake{reply: func(call int, _ map[string]any) map[string]any {
		if call == 1 {
			return chatAnswer("I can't help with that.")
		}
		return chatAnswer("ok ```x```")
	}}
	capSetup(t, fake2)
	code, body = xkPost(t, "/v1/chat/completions",
		`{"model":"xkeysmith/gpt-5","messages":[{"role":"user","content":"hi"}]}`)
	if code != 200 {
		t.Fatalf("code=%d body=%s", code, body)
	}
	joined := joinBodies(fake2.seen)
	if !strings.Contains(joined, "local-audit-lab/skills") || !strings.Contains(joined, "- rev:") {
		t.Fatalf("L1 必须注入技能索引: %s", joined)
	}
}

// B 档:模型发起的 xk__ 工具调用由网关执行,结果回填,再调上游。
func TestCapabilityToolRoundExecuted(t *testing.T) {
	fake := &capFake{reply: func(call int, req map[string]any) map[string]any {
		if call == 1 {
			// 模型先要求加载技能
			return capToolCall("xk__skill_load", map[string]any{"name": "rev"})
		}
		return chatAnswer("done")
	}}
	capSetup(t, fake)

	code, body := xkPost(t, "/v1/chat/completions",
		`{"model":"xkeysmith/gpt-5","messages":[{"role":"user","content":"hi"}]}`)
	if code != 200 {
		t.Fatalf("code=%d body=%s", code, body)
	}
	if len(fake.seen) < 2 {
		t.Fatalf("工具轮必须再调一次上游(回填后),实际 %d 次", len(fake.seen))
	}
	// 第二次请求里应带上工具结果(技能正文)
	second := mustJSON(fake.seen[1])
	if !strings.Contains(string(second), "逆向正文") {
		t.Fatalf("技能正文必须作为工具结果回填: %s", second)
	}
	if !strings.Contains(string(second), `"role":"tool"`) {
		t.Fatalf("回填必须用 role:tool: %s", second)
	}
	// 客户端只看到最终答复(工具轮不下发)
	if !strings.Contains(body, "done") {
		t.Fatalf("客户端应看到最终答复: %s", body)
	}
}

// B 档闸门:轮数触顶必须给明确错误,绝不把 xk__ 调用交还客户端。
func TestCapabilityToolRoundGateIsHonest(t *testing.T) {
	fake := &capFake{reply: func(call int, _ map[string]any) map[string]any {
		// 永远回工具调用 → 必然触顶
		return capToolCall("xk__skill_load", map[string]any{"name": "rev"})
	}}
	capSetup(t, fake)

	code, body := xkPost(t, "/v1/chat/completions",
		`{"model":"xkeysmith/gpt-5","messages":[{"role":"user","content":"hi"}]}`)
	if code != 503 {
		t.Fatalf("触顶必须给明确错误(不交还 xk__ 调用):code=%d body=%s", code, body)
	}
	if !strings.Contains(body, "exhausted") {
		t.Fatalf("body=%s", body)
	}
}

// 混合轮在网关处理完自身调用后才向客户端提供客户端工具。
func TestCapabilityMixedTurnDefersClientCalls(t *testing.T) {
	fake := &capFake{reply: func(call int, req map[string]any) map[string]any {
		if call > 1 {
			return capToolCall("read_file", map[string]any{})
		}
		// 一轮里既有网关工具又有客户端工具
		return map[string]any{
			"choices": []any{map[string]any{
				"finish_reason": "tool_calls",
				"message": map[string]any{
					"role": "assistant", "content": nil,
					"tool_calls": []any{
						map[string]any{"id": "c1", "type": "function",
							"function": map[string]any{"name": "xk__skill_load", "arguments": `{"name":"rev"}`}},
						map[string]any{"id": "c2", "type": "function",
							"function": map[string]any{"name": "read_file", "arguments": "{}"}},
					},
				},
			}},
		}
	}}
	capSetup(t, fake)

	code, body := xkPost(t, "/v1/chat/completions",
		`{"model":"xkeysmith/gpt-5","messages":[{"role":"user","content":"hi"}]}`)
	if code != 200 {
		t.Fatalf("code=%d body=%s", code, body)
	}
	if len(fake.seen) != 2 {
		t.Fatalf("gateway result must reach one continuation, calls=%d", len(fake.seen))
	}
	if !strings.Contains(body, "read_file") || strings.Contains(body, "xk__") {
		t.Fatalf("client received a gateway tool: %s", body)
	}
}

// 审批闸门:deny + 空白名单时,未授权调用作为「被拒绝」结果回填,不挂起。
func TestCapabilityDeniedCallIsAResult(t *testing.T) {
	t.Setenv("XKEYSMITH_APPROVAL", "deny")
	t.Setenv("XKEYSMITH_ALLOWLIST", "")
	fake := &capFake{reply: func(call int, req map[string]any) map[string]any {
		if call == 1 {
			return capToolCall("xk__skill_load", map[string]any{"name": "rev"})
		}
		return chatAnswer("ok")
	}}
	capSetup(t, fake)

	code, body := xkPost(t, "/v1/chat/completions",
		`{"model":"xkeysmith/gpt-5","messages":[{"role":"user","content":"hi"}]}`)
	if code != 200 {
		t.Fatalf("被拒也是结果,不该挂起:code=%d body=%s", code, body)
	}
	if len(fake.seen) < 2 {
		t.Fatal("拒绝结果也要回填给模型")
	}
	second := mustJSON(fake.seen[1])
	if !strings.Contains(string(second), "allowlist") {
		t.Fatalf("拒绝理由必须让模型能改策略: %s", second)
	}
}

func joinBodies(seen []map[string]any) string {
	var parts []string
	for _, s := range seen {
		parts = append(parts, string(mustJSON(s)))
	}
	return strings.Join(parts, "\n")
}
