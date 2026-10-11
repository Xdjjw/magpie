package attach

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/yetone/magpie/internal/gateway/xk/govern"
	"github.com/yetone/magpie/internal/gateway/xk/tools"
)

// 工具轮执行循环(B 档的核心):模型发出 xk__ 调用 → 网关自己执行 →
// 结果回填 → 再调上游,直到模型给出最终答复或触到闸门。
//
// 两条硬规则:
//   - **只执行自己的工具**:混合轮先执行网关调用,再让模型请求客户端工具;
//   - **触到闸门 = 明确错误**,绝不把客户端执行不了的 xk__ 调用交还回去。

// ToolPlane 是执行循环需要的工具平面能力(便于测试替换)。
type ToolPlane interface {
	IsActive() bool
	Disposition(calls []tools.ToolCall) tools.TurnDisposition
	Execute(call tools.ToolCall) tools.ToolResult
	Truncate(content string) string
}

// ToolLoopConfig 是一次工具轮循环的输入。
type ToolLoopConfig struct {
	Plane   ToolPlane
	Dialect tools.Dialect
	// MaxRounds 之外还有 RoundBudget:按本次请求剩余的资源逐轮计费。
	MaxRounds int
	Budget    *govern.Budget
	Context   context.Context
	Tokens    func(map[string]any) uint64
	OnTool    func(round int, call tools.ToolCall, result tools.ToolResult)
}

// ToolLoopResult 是循环的结局。
type ToolLoopResult struct {
	// FinalBody 是最终答复(模型不再发 xk__ 调用)。
	FinalBody map[string]any
	// Rounds 是实际执行的工具轮数。
	Rounds int
	// Calls 是所有已执行的工具调用记录(进审计)。
	Calls []tools.ToolResult
	// Unlocked 累积的解锁模式(技能门控,写回会话)。
	Unlocked []string
	// StopReason 非空 = 触到闸门,调用方必须给明确错误。
	StopReason govern.StopReason
}

// RunToolLoop 执行工具轮。
//
// 语义:先发一次请求拿**响应**,从响应里看模型是否发起 xk__ 调用;
// 有则执行、回填、再发,直到模型给出最终答复或触到闸门。
// roundTrip 发一次上游并返回响应体(调用方负责方言与还原)。
func RunToolLoop(cfg *ToolLoopConfig, request map[string]any, roundTrip func(body map[string]any) (map[string]any, error)) (*ToolLoopResult, error) {
	res := &ToolLoopResult{}
	if cfg.Plane == nil || !cfg.Plane.IsActive() {
		resp, err := roundTrip(request)
		if err != nil {
			return nil, err
		}
		res.FinalBody = resp
		return res, nil
	}
	seen := map[string]tools.ToolResult{}
	body := request

	for round := 1; ; round++ {
		if cfg.Context != nil && cfg.Context.Err() != nil {
			return res, cfg.Context.Err()
		}
		if round > 1 && cfg.Budget != nil {
			if reason, ok := cfg.Budget.CheckResources(); !ok {
				res.StopReason = reason
				return res, nil
			}
		}
		if scoped, ok := cfg.Plane.(interface{ BeginRound() }); ok {
			scoped.BeginRound()
		}
		resp, err := roundTrip(body)
		if err != nil {
			return res, err
		}
		res.FinalBody = resp
		if round > 1 && cfg.Budget != nil {
			tokens := uint64(0)
			if cfg.Tokens != nil {
				tokens = cfg.Tokens(resp)
			}
			cfg.Budget.NoteToolRound(tokens)
		}

		calls := tools.ExtractToolCalls(cfg.Dialect, resp)
		switch cfg.Plane.Disposition(calls) {
		case tools.TurnNotOurs:
			// 模型没发起网关工具:这就是最终答复。
			return res, nil
		case tools.TurnHandBack:
			// No call in this response has reached the client. Keep only the
			// gateway calls in the internal turn, feed back their results and
			// let the model request client tools again with that context.
			resp = tools.GatewayOnlyResponse(cfg.Dialect, resp)
			calls = tools.ExtractToolCalls(cfg.Dialect, resp)
		}

		// 闸门 1:轮数上限。
		if round > cfg.MaxRounds {
			res.StopReason = govern.StopCallsExhausted
			return res, nil
		}
		// 闸门 2:逐轮计费(只看 token 与墙钟,不占阶梯调用预算)。
		if cfg.Budget != nil {
			if reason, ok := cfg.Budget.CheckResources(); !ok {
				res.StopReason = reason
				return res, nil
			}
		}

		assistant := tools.AssistantTurn(cfg.Dialect, resp)
		results := make([]tools.ToolResult, 0, len(calls))
		for _, call := range calls {
			if cfg.Context != nil && cfg.Context.Err() != nil {
				return res, cfg.Context.Err()
			}
			// 同一个调用只执行一次(模型可能重复发同一个 id)。
			if cached, ok := seen[call.ID]; ok {
				results = append(results, cached)
				continue
			}
			var out tools.ToolResult
			if scoped, ok := cfg.Plane.(interface {
				ExecuteContext(context.Context, tools.ToolCall) tools.ToolResult
			}); ok {
				ctx := cfg.Context
				if ctx == nil {
					ctx = context.Background()
				}
				out = scoped.ExecuteContext(ctx, call)
			} else {
				out = cfg.Plane.Execute(call)
			}
			out.Content = cfg.Plane.Truncate(out.Content)
			seen[call.ID] = out
			if cfg.OnTool != nil {
				cfg.OnTool(round, call, out)
			}
			results = append(results, out)
			res.Unlocked = appendUnique(res.Unlocked, out.Unlocked...)
		}
		res.Calls = append(res.Calls, results...)
		res.Rounds = round

		next := cloneBody(body)
		if err := tools.AppendResults(cfg.Dialect, next, assistant, results); err != nil {
			return res, fmt.Errorf("回填工具结果失败: %w", err)
		}
		body = next
	}
}

func cloneBody(body map[string]any) map[string]any {
	b, err := json.Marshal(body)
	if err != nil {
		return body
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		return body
	}
	return out
}

func appendUnique(dst []string, items ...string) []string {
	for _, item := range items {
		found := false
		for _, x := range dst {
			if x == item {
				found = true
				break
			}
		}
		if !found {
			dst = append(dst, item)
		}
	}
	return dst
}
