package tools

import (
	"encoding/json"
	"fmt"
)

// 两种 wire 的工具语义差异,集中在这里。
//
//	|            | OpenAI 兼容                                    | Anthropic Messages                          |
//	| 工具定义   | tools[].function.{name,description,parameters}  | tools[].{name,description,input_schema}     |
//	| 发起调用   | message.tool_calls[].function.arguments(字符串) | content[].{type:tool_use,input}(对象)        |
//	| 回填结果   | 每个结果一条 {role:"tool",tool_call_id,content}  | 所有结果放进**同一条** user 消息的 tool_result 块 |
//
// 最后一行是最容易写错的地方:Anthropic 要求一次 assistant 的多个 tool_use
// 必须由**一条** user 消息里的多个 tool_result 应答。拆成多条会被 400。

// Dialect 与 rewrite 的方言名一致。
type Dialect string

const (
	DialectChat      Dialect = "chat"
	DialectResponses Dialect = "responses"
	DialectAnthropic Dialect = "anthropic"
)

// ExtractToolCalls 从上游响应里取出模型发起的工具调用。
func ExtractToolCalls(d Dialect, body map[string]any) []ToolCall {
	switch d {
	case DialectAnthropic:
		var out []ToolCall
		content, _ := body["content"].([]any)
		for _, block := range content {
			m, ok := block.(map[string]any)
			if !ok || m["type"] != "tool_use" {
				continue
			}
			id, _ := m["id"].(string)
			name, _ := m["name"].(string)
			raw, _ := json.Marshal(m["input"])
			out = append(out, ToolCall{ID: id, Name: name, Arguments: m["input"], RawArguments: string(raw)})
		}
		return out
	case DialectResponses:
		var out []ToolCall
		output, _ := body["output"].([]any)
		for _, item := range output {
			m, ok := item.(map[string]any)
			if !ok || m["type"] != "function_call" {
				continue
			}
			callID, _ := m["call_id"].(string)
			if callID == "" {
				callID, _ = m["id"].(string)
			}
			name, _ := m["name"].(string)
			raw, _ := m["arguments"].(string)
			var args any
			if raw != "" {
				_ = json.Unmarshal([]byte(raw), &args)
			}
			out = append(out, ToolCall{ID: callID, Name: name, Arguments: args, RawArguments: raw})
		}
		return out
	default: // Chat
		var out []ToolCall
		choices, _ := body["choices"].([]any)
		if len(choices) == 0 {
			return nil
		}
		msg, _ := choices[0].(map[string]any)
		if msg == nil {
			return nil
		}
		inner, _ := msg["message"].(map[string]any)
		if inner == nil {
			return nil
		}
		calls, _ := inner["tool_calls"].([]any)
		for _, c := range calls {
			m, ok := c.(map[string]any)
			if !ok {
				continue
			}
			id, _ := m["id"].(string)
			fn, _ := m["function"].(map[string]any)
			if fn == nil {
				continue
			}
			name, _ := fn["name"].(string)
			raw, _ := fn["arguments"].(string)
			var args any
			if raw != "" {
				_ = json.Unmarshal([]byte(raw), &args)
			}
			out = append(out, ToolCall{ID: id, Name: name, Arguments: args, RawArguments: raw})
		}
		return out
	}
}

// GatewayOnlyResponse keeps non-call content and the gateway calls for an
// internal mixed turn. Deferred client calls have never been exposed or run.
func GatewayOnlyResponse(d Dialect, body map[string]any) map[string]any {
	encoded, _ := json.Marshal(body)
	var out map[string]any
	_ = json.Unmarshal(encoded, &out)
	filter := func(items []any, typ, nameField string) []any {
		var kept []any
		for _, item := range items {
			obj, ok := item.(map[string]any)
			if !ok {
				kept = append(kept, item)
				continue
			}
			if kind, _ := obj["type"].(string); kind == typ {
				name, _ := obj[nameField].(string)
				if nameField == "function" {
					fn, _ := obj["function"].(map[string]any)
					name, _ = fn["name"].(string)
				}
				if !IsGatewayTool(name) {
					continue
				}
			}
			kept = append(kept, item)
		}
		return kept
	}
	switch d {
	case DialectResponses:
		items, _ := out["output"].([]any)
		out["output"] = filter(items, "function_call", "name")
	case DialectAnthropic:
		items, _ := out["content"].([]any)
		out["content"] = filter(items, "tool_use", "name")
	default:
		choices, _ := out["choices"].([]any)
		if len(choices) > 0 {
			choice, _ := choices[0].(map[string]any)
			msg, _ := choice["message"].(map[string]any)
			calls, _ := msg["tool_calls"].([]any)
			msg["tool_calls"] = filter(calls, "function", "function")
		}
	}
	return out
}

// AssistantTurn 从上游响应里取出「assistant 那一轮」,追加回去。
//
// 原样追加而不是重建:上游可能带了我们没建模的字段(例如 reasoning_content),
// 丢掉它们会让模型在下一轮看到一段被削过的历史。
func AssistantTurn(d Dialect, body map[string]any) any {
	if d == DialectResponses {
		output, _ := body["output"].([]any)
		var items []any
		for _, item := range output {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			switch m["type"] {
			case "message":
				blocks, _ := m["content"].([]any)
				var parts []any
				for _, b := range blocks {
					bm, ok := b.(map[string]any)
					if ok && bm["type"] == "output_text" {
						parts = append(parts, bm)
					}
				}
				if len(parts) > 0 {
					items = append(items, map[string]any{"type": "message", "role": "assistant", "content": parts})
				}
			case "function_call":
				// 必须原样回填:无状态调用时服务端没有历史,少了它随后的
				// function_call_output 会对不上号。
				items = append(items, m)
			}
		}
		return items
	}
	if d == DialectAnthropic {
		content := body["content"]
		if content == nil {
			content = []any{}
		}
		return map[string]any{"role": "assistant", "content": content}
	}
	choices, _ := body["choices"].([]any)
	if len(choices) == 0 {
		return nil
	}
	msg, _ := choices[0].(map[string]any)
	inner, _ := msg["message"].(map[string]any)
	if inner == nil {
		return nil
	}
	toolCalls := inner["tool_calls"]
	if toolCalls == nil {
		toolCalls = []any{}
	}
	return map[string]any{"role": "assistant", "content": inner["content"], "tool_calls": toolCalls}
}

// AppendResults 把工具结果追加到请求里。
//
//	Chat:      messages.push(assistant) + 每个结果一条 {role:"tool"}
//	Anthropic: messages.push(assistant) + **一条** user 消息装全部 tool_result
//	Responses: input.extend(assistant items) + 每个结果一条 function_call_output
func AppendResults(d Dialect, body map[string]any, assistant any, results []ToolResult) error {
	if d == DialectResponses {
		items, _ := body["input"].([]any)
		switch a := assistant.(type) {
		case []any:
			items = append(items, a...)
		case nil:
		default:
			items = append(items, a)
		}
		for _, r := range results {
			items = append(items, map[string]any{
				"type": "function_call_output", "call_id": r.ID, "output": r.Content,
			})
		}
		body["input"] = items
		return nil
	}
	msgs, ok := body["messages"].([]any)
	if !ok {
		return fmt.Errorf("请求没有 messages 数组")
	}
	if assistant != nil {
		msgs = append(msgs, assistant)
	}
	if len(results) > 0 {
		if d == DialectAnthropic {
			// 一条 user 消息装全部 tool_result:拆成多条会被 400。
			blocks := make([]any, 0, len(results))
			for _, r := range results {
				blocks = append(blocks, map[string]any{
					"type": "tool_result", "tool_use_id": r.ID, "content": r.Content,
				})
			}
			msgs = append(msgs, map[string]any{"role": "user", "content": blocks})
		} else {
			for _, r := range results {
				msgs = append(msgs, map[string]any{
					"role": "tool", "tool_call_id": r.ID, "content": r.Content,
				})
			}
		}
	}
	body["messages"] = msgs
	return nil
}
