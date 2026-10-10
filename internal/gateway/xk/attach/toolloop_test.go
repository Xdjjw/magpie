package attach

import (
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/gateway/xk/govern"
	"github.com/yetone/magpie/internal/gateway/xk/tools"
)

// 工具轮循环的验收:执行、回填、闸门、整轮交还。

type fakePlane struct {
	active   bool
	defs     []tools.ToolDef
	executed []string
	handBack bool
}

func (f *fakePlane) IsActive() bool { return f.active }
func (f *fakePlane) Disposition(calls []tools.ToolCall) tools.TurnDisposition {
	if f.handBack {
		return tools.TurnHandBack
	}
	gateway, other := 0, 0
	for _, c := range calls {
		if tools.IsGatewayTool(c.Name) {
			gateway++
		} else {
			other++
		}
	}
	switch {
	case gateway == 0:
		return tools.TurnNotOurs
	case other == 0:
		return tools.TurnAllOurs
	default:
		return tools.TurnHandBack
	}
}
func (f *fakePlane) Execute(call tools.ToolCall) tools.ToolResult {
	f.executed = append(f.executed, call.Name)
	return tools.ToolResult{ID: call.ID, Name: call.Name, Content: "pong:" + call.Name,
		Unlocked: []string{"xk__mcp__ida__*"}, UnlockedBy: "rev"}
}
func (f *fakePlane) Truncate(s string) string { return s }

// chatRequest 是**请求**形状(初始入参与回填后的下一轮都长这样)。
func chatRequest() map[string]any {
	return map[string]any{
		"model":    "gpt-5",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}
}

// chatResponseWithCall 是**响应**形状(模型发起了工具调用)。
func chatResponseWithCall(name, id string) map[string]any {
	return map[string]any{
		"model":    "gpt-5",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"choices": []any{map[string]any{
			"finish_reason": "tool_calls",
			"message": map[string]any{
				"role": "assistant", "content": nil,
				"tool_calls": []any{map[string]any{
					"id": id, "type": "function",
					"function": map[string]any{"name": name, "arguments": "{}"},
				}},
			},
		}},
	}
}

func chatFinal() map[string]any {
	return map[string]any{
		"choices": []any{map[string]any{
			"finish_reason": "stop",
			"message":       map[string]any{"role": "assistant", "content": "done"},
		}},
	}
}

func TestToolLoopExecutesAndFeedsBack(t *testing.T) {
	plane := &fakePlane{active: true}
	cfg := &ToolLoopConfig{Plane: plane, Dialect: tools.DialectChat, MaxRounds: 4,
		Budget: govern.NewBudget(govern.DefaultLimits())}
	var sent []map[string]any
	round := 0
	roundTrip := func(body map[string]any) (map[string]any, error) {
		sent = append(sent, cloneBody(body))
		round++
		if round == 1 {
			return chatResponseWithCall("xk__skill_load", "c1"), nil
		}
		return chatFinal(), nil
	}
	res, err := RunToolLoop(cfg, chatRequest(), roundTrip)
	if err != nil {
		t.Fatal(err)
	}
	if len(plane.executed) != 1 || plane.executed[0] != "xk__skill_load" {
		t.Fatalf("executed=%v", plane.executed)
	}
	if res.Rounds != 1 {
		t.Fatalf("rounds=%d", res.Rounds)
	}
	if len(res.Unlocked) != 1 || res.Unlocked[0] != "xk__mcp__ida__*" {
		t.Fatalf("解锁必须累积: %v", res.Unlocked)
	}
	// 回填:第二轮请求(下标 1)里应有 assistant 轮与 role:tool 结果
	msgs, _ := sent[1]["messages"].([]any)
	joined := ""
	for _, m := range msgs {
		b, _ := m.(map[string]any)
		if b["role"] == "tool" {
			joined += b["content"].(string)
		}
	}
	if !strings.Contains(joined, "pong:xk__skill_load") {
		t.Fatalf("工具结果未回填: %v", sent[0]["messages"])
	}
}

func TestToolLoopHandsBackMixedTurns(t *testing.T) {
	plane := &fakePlane{active: true}
	cfg := &ToolLoopConfig{Plane: plane, Dialect: tools.DialectChat, MaxRounds: 4}
	// 纯客户端工具轮:不归网关管,原样返回(网关不执行、也不改)。
	res, err := RunToolLoop(cfg, chatRequest(), func(b map[string]any) (map[string]any, error) {
		return chatResponseWithCall("read_file", "c1"), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(plane.executed) != 0 {
		t.Fatalf("客户端工具不该被网关执行: %+v", res)
	}

	// 混轮:同一轮里既有网关工具又有客户端工具 → **整轮交还**。
	plane2 := &fakePlane{active: true}
	cfg2 := &ToolLoopConfig{Plane: plane2, Dialect: tools.DialectChat, MaxRounds: 4}
	res2, err := RunToolLoop(cfg2, chatRequest(), func(b map[string]any) (map[string]any, error) {
		return map[string]any{
			"choices": []any{map[string]any{
				"finish_reason": "tool_calls",
				"message": map[string]any{
					"role": "assistant", "content": nil,
					"tool_calls": []any{
						map[string]any{"id": "c1", "type": "function",
							"function": map[string]any{"name": "xk__skill_load", "arguments": "{}"}},
						map[string]any{"id": "c2", "type": "function",
							"function": map[string]any{"name": "read_file", "arguments": "{}"}},
					},
				},
			}},
		}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res2.HandedBack || len(plane2.executed) != 0 {
		t.Fatalf("混合轮必须整轮交还(网关不执行任何调用): %+v", res2)
	}
}

func TestToolLoopStopsAtRoundGate(t *testing.T) {
	plane := &fakePlane{active: true}
	cfg := &ToolLoopConfig{Plane: plane, Dialect: tools.DialectChat, MaxRounds: 2,
		Budget: govern.NewBudget(govern.DefaultLimits())}
	n := 0
	res, err := RunToolLoop(cfg, chatRequest(),
		func(b map[string]any) (map[string]any, error) {
			// 永远回工具调用(每次换一个 id,避免去重挡住)
			n++
			return chatResponseWithCall("xk__skill_load", "c"+string(rune('0'+n))), nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if res.StopReason != govern.StopCallsExhausted {
		t.Fatalf("触到轮数闸门必须给明确错误: %+v", res)
	}
}

func TestToolLoopRespectsTokenGate(t *testing.T) {
	plane := &fakePlane{active: true}
	limits := govern.DefaultLimits()
	limits.TokenBudget = 10
	budget := govern.NewBudget(limits)
	budget.NoteToolRound(50) // 已超 token 预算
	cfg := &ToolLoopConfig{Plane: plane, Dialect: tools.DialectChat, MaxRounds: 4, Budget: budget}
	res, err := RunToolLoop(cfg, chatRequest(),
		func(b map[string]any) (map[string]any, error) {
			return chatResponseWithCall("xk__skill_load", "c1"), nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if res.StopReason != govern.StopTokenBudgetExhausted {
		t.Fatalf("token 闸门必须生效: %+v", res)
	}
}

func TestToolLoopAnthropicResultsInOneUserMessage(t *testing.T) {
	plane := &fakePlane{active: true}
	cfg := &ToolLoopConfig{Plane: plane, Dialect: tools.DialectAnthropic, MaxRounds: 4}
	request := map[string]any{
		"model":    "claude",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}
	// 响应:模型同一轮发起两个 tool_use(Anthropic 要求它们由**一条**
	// user 消息里的两个 tool_result 应答)。
	anthropicResp := map[string]any{
		"content": []any{
			map[string]any{"type": "tool_use", "id": "t1", "name": "xk__skill_load", "input": map[string]any{}},
			map[string]any{"type": "tool_use", "id": "t2", "name": "xk__skill_load", "input": map[string]any{}},
		},
	}
	var sent map[string]any
	n := 0
	res, err := RunToolLoop(cfg, request, func(b map[string]any) (map[string]any, error) {
		n++
		if n == 1 {
			return anthropicResp, nil
		}
		sent = cloneBody(b)
		return map[string]any{"content": []any{map[string]any{"type": "text", "text": "done"}}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Rounds != 1 || len(plane.executed) != 2 {
		t.Fatalf("rounds=%d executed=%v", res.Rounds, plane.executed)
	}
	msgs, _ := sent["messages"].([]any)
	// 最后一条必须是**一条** user 消息,装两个 tool_result
	last, _ := msgs[len(msgs)-1].(map[string]any)
	if last["role"] != "user" {
		t.Fatalf("Anthropic 的 tool_result 必须放进 user 消息: %v", last)
	}
	blocks, _ := last["content"].([]any)
	if len(blocks) != 2 {
		t.Fatalf("两个 tool_result 必须在同一条消息里(拆开会被 400): %v", blocks)
	}
}
