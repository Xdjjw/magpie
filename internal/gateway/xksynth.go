package gateway

// 协议修复:上游/探针回的是完整 JSON,但客户端要的是流。
// 按协议规定的流形状把它**翻译**成 SSE——期待 `choices[].delta`(或
// Responses 的 output_text.delta)的 SDK 完全无感。这是 xkeysmith 的
// 「协议修复而不是协议假设」在 fork 里的对应实现。

import (
	"encoding/json"
	"fmt"
	"strings"
)

// synthesizeSSE 把一份完整响应体翻译成 SSE 行。
//
// 形状按 body 判断:有 output[] 走 Responses 事件名,否则走 Chat 的
// choices[].delta;错误体直接发一条 data(与上游的真实行为一致)。
// restore 用于 L4 占位符还原(可为 nil)。
func synthesizeSSE(body []byte, restore func(string) string) []byte {
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		// 不是 JSON:原样作为一条 data 事件发出,不吞掉内容。
		return []byte(sseLine(string(body)))
	}
	if _, isErr := doc["error"]; isErr {
		return []byte(sseLine(string(body)))
	}

	var out strings.Builder
	if output, ok := doc["output"].([]any); ok {
		// Responses 形状:按 Responses 的事件名合成(choices[] 那套客户端解析不了)。
		out.WriteString(namedSSE("response.created", map[string]any{
			"type":     "response.created",
			"response": map[string]any{"id": "xk_synth", "status": "in_progress"},
		}))
		for i, item := range output {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			switch m["type"] {
			case "message":
				blocks, _ := m["content"].([]any)
				for ci, b := range blocks {
					bm, ok := b.(map[string]any)
					if !ok {
						continue
					}
					if text, ok := bm["text"].(string); ok {
						out.WriteString(namedSSE("response.output_text.delta", map[string]any{
							"type":          "response.output_text.delta",
							"item_id":       fmt.Sprintf("msg_%d", i),
							"output_index":  i,
							"content_index": ci,
							"delta":         restoreText(restore, text),
						}))
					}
				}
			case "function_call":
				out.WriteString(namedSSE("response.output_item.added", map[string]any{
					"type": "response.output_item.added", "output_index": i, "item": m,
				}))
			}
		}
		status := "completed"
		if s, ok := doc["status"].(string); ok && s != "" {
			status = s
		}
		out.WriteString(namedSSE("response.completed", map[string]any{
			"type": "response.completed",
			"response": map[string]any{
				"id": "xk_synth", "status": status, "usage": doc["usage"],
			},
		}))
		out.WriteString("data: [DONE]\n\n")
		return []byte(out.String())
	}

	// Chat 形状。
	choices, _ := doc["choices"].([]any)
	if len(choices) == 0 {
		return []byte(sseLine(string(body)))
	}
	first, _ := choices[0].(map[string]any)
	msg, _ := first["message"].(map[string]any)
	delta := map[string]any{"role": "assistant"}
	if msg != nil {
		if content, ok := msg["content"].(string); ok {
			delta["content"] = restoreText(restore, content)
		} else if msg["content"] != nil {
			delta["content"] = msg["content"]
		}
		if calls, ok := msg["tool_calls"]; ok {
			delta["tool_calls"] = calls
		}
		if refusal, ok := msg["refusal"]; ok && refusal != nil {
			delta["refusal"] = refusal
		}
	}
	base := map[string]any{"id": first["id"], "model": doc["model"], "object": "chat.completion.chunk"}
	head := cloneMap(base)
	head["choices"] = []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant"}, "finish_reason": nil}}
	out.WriteString(sseLine(xkJSON(head)))

	bodyChunk := cloneMap(base)
	bodyChunk["choices"] = []any{map[string]any{"index": 0, "delta": delta, "finish_reason": nil}}
	out.WriteString(sseLine(xkJSON(bodyChunk)))

	finish := "stop"
	if fr, ok := first["finish_reason"].(string); ok && fr != "" {
		finish = fr
	}
	tail := cloneMap(base)
	tail["choices"] = []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": finish}}
	if u, ok := doc["usage"]; ok {
		tail["usage"] = u
	}
	out.WriteString(sseLine(xkJSON(tail)))
	out.WriteString("data: [DONE]\n\n")
	return []byte(out.String())
}

func restoreText(restore func(string) string, text string) string {
	if restore == nil {
		return text
	}
	return restore(text)
}

// sseLine 把一段 JSON 包成一条 SSE data 事件。
func sseLine(payload string) string {
	return "data: " + payload + "\n\n"
}

// namedSSE 生成带 event: 名的事件(Responses 形状需要)。
func namedSSE(name string, payload map[string]any) string {
	return "event: " + name + "\ndata: " + xkJSON(payload) + "\n\n"
}

func cloneMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// jsonString 是稳定的 JSON 序列化(合成 SSE 用)。
func xkJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}
