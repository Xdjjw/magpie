package rewrite

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Envelope 是待改写的请求:原始 JSON 是**唯一真源**,改写只能通过
// 受限的文本槽访问器进行(tool_use.input / tool_call arguments 等
// 结构化字段根本不在访问范围内)。这是「拦截在语义层」的落点。
type Envelope struct {
	Body    map[string]any
	Wire    Wire
	Dialect Dialect
}

type Wire string

const (
	WireOpenAi    Wire = "openai"
	WireAnthropic Wire = "anthropic"
)

type Dialect int

const (
	DialectMessages Dialect = iota
	DialectResponses
)

// DetectDialect 从报文形状判断方言(不是从路径猜):Responses 没有
// messages,它用 instructions + input。
func DetectDialect(body map[string]any) Dialect {
	if _, ok := body["messages"]; ok {
		return DialectMessages
	}
	if _, ok := body["input"]; ok {
		return DialectResponses
	}
	if _, ok := body["instructions"]; ok {
		return DialectResponses
	}
	return DialectMessages
}

func NewEnvelope(body map[string]any, wire Wire) *Envelope {
	return &Envelope{Body: body, Wire: wire, Dialect: DetectDialect(body)}
}

// ─────────────────────────── 文本槽 ───────────────────────────

type SlotKind int

const (
	SlotSystem SlotKind = iota
	SlotUserText
	SlotAssistantText
	SlotToolResult
	SlotUnknown
)

type Scope int

const (
	ScopeAll Scope = iota
	ScopeUserTurns
	ScopeUserAndSystem
)

func (s Scope) allows(k SlotKind) bool {
	switch s {
	case ScopeAll:
		return true
	case ScopeUserTurns:
		return k == SlotUserText
	default:
		return k == SlotUserText || k == SlotSystem
	}
}

// SlotCtx 是一个文本槽的定位信息(进审计与 LayerReport)。
type SlotCtx struct {
	Kind         SlotKind
	Role         string
	MessageIndex int
	Pointer      string
}

// slot 是槽的读写句柄。get/set 闭包指向 body 里的具体字符串节点,
// 因此改写只触达文本、绝不重建整份 JSON。
type slot struct {
	ctx SlotCtx
	get func() string
	set func(string)
}

// textSlots 按文档顺序枚举全部文本槽。
//
// 形状覆盖:
//   - messages 方言:system 轮 / 每条消息的 content(string 或 blocks);
//   - Anthropic 顶层 system(string 或 blocks);
//   - Responses 的顶层 instructions。
//
// 结构化字段(tool_use.input、tool_result 的 tool_use_id、函数参数)不在枚举范围内。
func (e *Envelope) textSlots() []*slot {
	var out []*slot
	if e.Dialect == DialectResponses {
		if _, ok := e.Body["instructions"]; ok {
			out = append(out, topLevelSlots(e.Body, "instructions", SlotCtx{Kind: SlotSystem, Pointer: "/instructions"})...)
		}
		if arr, ok := e.Body["input"].([]any); ok {
			for i, item := range arr {
				obj, ok := item.(map[string]any)
				if !ok {
					continue
				}
				role, _ := obj["role"].(string)
				kind := kindForRole(role)
				ptr := fmt.Sprintf("/input/%d/content", i)
				if _, ok := obj["content"]; ok {
					out = append(out, topLevelSlots(obj, "content", SlotCtx{Kind: kind, Role: role, MessageIndex: i + 1, Pointer: ptr})...)
				}
			}
		}
		return out
	}

	// messages 方言
	if e.Wire == WireAnthropic {
		if _, ok := e.Body["system"]; ok {
			out = append(out, topLevelSlots(e.Body, "system", SlotCtx{Kind: SlotSystem, Pointer: "/system"})...)
		}
	}
	msgs, _ := e.Body["messages"].([]any)
	for i, m := range msgs {
		obj, ok := m.(map[string]any)
		if !ok {
			continue
		}
		role, _ := obj["role"].(string)
		kind := kindForRole(role)
		if role == "system" {
			kind = SlotSystem
		}
		if _, ok := obj["content"]; !ok {
			continue
		}
		ptr := fmt.Sprintf("/messages/%d/content", i)
		out = append(out, topLevelSlots(obj, "content", SlotCtx{Kind: kind, Role: role, MessageIndex: i + 1, Pointer: ptr})...)
	}
	return out
}

func kindForRole(role string) SlotKind {
	switch role {
	case "system", "developer":
		return SlotSystem
	case "user":
		return SlotUserText
	case "assistant":
		return SlotAssistantText
	default:
		return SlotUnknown
	}
}

// topLevelSlots 处理顶层键的槽:字符串(可直接写回)或 blocks 数组。
func topLevelSlots(container map[string]any, key string, base SlotCtx) []*slot {
	if _, ok := container[key].(string); ok {
		return []*slot{mapSlot(container, key, base)}
	}
	return blocksOrStringSlots(container, key, base)
}

// blocksOrStringSlots 处理 content 的两种形态:纯字符串,或 blocks 数组。
// blocks 里只有 text 类型是文本槽;tool_use / tool_result 的结构字段
// 不暴露(tool_result 的文本正文除外——L3/L4 需要覆盖它)。
//
// 关键:槽必须持有**容器 + 键**而不是值的副本,否则 set 写不回原 JSON。
func blocksOrStringSlots(container map[string]any, key string, base SlotCtx) []*slot {
	switch t := container[key].(type) {
	case string:
		return []*slot{mapSlot(container, key, base)}
	case []any:
		var out []*slot
		for j, block := range t {
			obj, ok := block.(map[string]any)
			if !ok {
				continue
			}
			typ, _ := obj["type"].(string)
			switch typ {
			case "text", "input_text", "output_text":
				if _, ok := obj["text"]; !ok {
					continue
				}
				ctx := base
				ctx.Pointer = fmt.Sprintf("%s/%d/text", base.Pointer, j)
				out = append(out, mapSlot(obj, "text", ctx))
			case "tool_result":
				ctx := base
				ctx.Kind = SlotToolResult
				ctx.Pointer = fmt.Sprintf("%s/%d/content", base.Pointer, j)
				if inner, ok := obj["content"]; ok {
					out = append(out, contentSlots(inner, ctx)...)
				}
			}
		}
		return out
	}
	return nil
}

// contentSlots 处理「值本身就是 content」的情况(顶层 instructions/system、
// tool_result 的内层 content):字符串或 blocks 数组。
func contentSlots(v any, base SlotCtx) []*slot {
	switch t := v.(type) {
	case string:
		_ = t
		return nil // 顶层字符串槽由调用方直接 mapSlot 处理
	case []any:
		var out []*slot
		for j, block := range t {
			obj, ok := block.(map[string]any)
			if !ok {
				continue
			}
			typ, _ := obj["type"].(string)
			if typ == "text" || typ == "input_text" || typ == "output_text" {
				if _, ok := obj["text"]; !ok {
					continue
				}
				ctx := base
				ctx.Pointer = fmt.Sprintf("%s/%d/text", base.Pointer, j)
				out = append(out, mapSlot(obj, "text", ctx))
			}
		}
		return out
	}
	return nil
}

func mapSlot(obj map[string]any, key string, ctx SlotCtx) *slot {
	return &slot{
		ctx: ctx,
		get: func() string { s, _ := obj[key].(string); return s },
		set: func(s string) { obj[key] = s },
	}
}

// forEachTextMut 按作用域遍历并就地改写文本槽。
func (e *Envelope) forEachTextMut(scope Scope, f func(ctx SlotCtx, text *string)) {
	for _, s := range e.textSlots() {
		if !scope.allows(s.ctx.Kind) {
			continue
		}
		cur := s.get()
		f(s.ctx, &cur)
		if cur != s.get() {
			s.set(cur)
		}
	}
}

// forEachTextRef 只读遍历。
func (e *Envelope) forEachTextRef(scope Scope, f func(ctx SlotCtx, text string)) {
	for _, s := range e.textSlots() {
		if scope.allows(s.ctx.Kind) {
			f(s.ctx, s.get())
		}
	}
}

// CollectText 拼接全部文本槽(测试与审计预览用)。
func (e *Envelope) CollectText(scope Scope) string {
	var out []string
	e.forEachTextRef(scope, func(_ SlotCtx, text string) { out = append(out, text) })
	return joinStrings(out, "\n")
}

// ByteLen 是当前 body 序列化后的体积(用于 bytes_delta 报告)。
func (e *Envelope) ByteLen() int { return jsonLen(e.Body) }

// ─────────────────────────── system 注入 ───────────────────────────

// AppendSystemText 把文本追加到 system 槽位。
//
// Responses 没有 messages:system 等价物是顶层 instructions——不处理
// 这一支的话,技能注入与实验室指令会**静默失效**(这正是 Responses
// 方言上曾经发生过的事)。
func (e *Envelope) AppendSystemText(text string) bool {
	if strings.TrimSpace(text) == "" {
		return false
	}
	if e.Dialect == DialectResponses {
		switch existing := e.Body["instructions"].(type) {
		case string:
			if existing != "" {
				e.Body["instructions"] = existing + "\n\n" + text
			} else {
				e.Body["instructions"] = text
			}
			return true
		case []any:
			e.Body["instructions"] = append(existing, map[string]any{"type": "input_text", "text": text})
			return true
		case nil:
			e.Body["instructions"] = text
			return true
		default:
			return false
		}
	}
	if e.Wire == WireAnthropic {
		switch existing := e.Body["system"].(type) {
		case string:
			if existing != "" {
				e.Body["system"] = existing + "\n\n" + text
			} else {
				e.Body["system"] = text
			}
			return true
		case []any:
			e.Body["system"] = append(existing, map[string]any{"type": "text", "text": text})
			return true
		case nil:
			e.Body["system"] = text
			return true
		default:
			return false
		}
	}
	// OpenAI Chat:插一条 system 轮,放在开头连续 system 之后。
	msgs, _ := e.Body["messages"].([]any)
	insertAt := 0
	for insertAt < len(msgs) {
		m, ok := msgs[insertAt].(map[string]any)
		if !ok {
			break
		}
		if role, _ := m["role"].(string); role == "system" || role == "developer" {
			insertAt++
			continue
		}
		break
	}
	entry := map[string]any{"role": "system", "content": text}
	msgs = append(msgs[:insertAt], append([]any{entry}, msgs[insertAt:]...)...)
	e.Body["messages"] = msgs
	return true
}

// ─────────────────────────── 伪装轮次 ───────────────────────────

// PrimerTurn 是一条伪造的良性技术对话轮。
type PrimerTurn struct{ Role, Text string }

// PrependPrimerTurns 在「开头连续 system 之后」插入伪装轮次。
//
// 绝不插到 tool_use / tool_result 之间,否则 Anthropic 直接 400
// (工具配对不变量);首条同角色轮次会被合并而不是硬插。
func (e *Envelope) PrependPrimerTurns(turns []PrimerTurn) (int, error) {
	if len(turns) == 0 {
		return 0, nil
	}
	if e.Dialect == DialectResponses {
		// Responses 的 input 是条目列表:插 user/assistant 消息条目即可。
		arr, _ := e.Body["input"].([]any)
		var prefix []any
		for _, t := range turns {
			prefix = append(prefix, map[string]any{
				"type": "message", "role": t.Role,
				"content": []any{map[string]any{"type": "input_text", "text": t.Text}},
			})
		}
		e.Body["input"] = append(prefix, arr...)
		return len(turns), nil
	}
	msgs, _ := e.Body["messages"].([]any)
	if len(msgs) == 0 {
		return 0, nil
	}
	// 找到插入点:开头连续 system 之后。
	insertAt := 0
	for insertAt < len(msgs) {
		m, ok := msgs[insertAt].(map[string]any)
		if !ok {
			break
		}
		if role, _ := m["role"].(string); role == "system" || role == "developer" {
			insertAt++
			continue
		}
		break
	}
	// 工具配对保护:插入点若落在 tool 相关消息之间,直接拒绝。
	if insertAt > 0 {
		if m, ok := msgs[insertAt-1].(map[string]any); ok {
			if hasToolParts(m) {
				return 0, fmt.Errorf("primer turns would split a tool_use/tool_result pair")
			}
		}
	}
	if insertAt < len(msgs) {
		if m, ok := msgs[insertAt].(map[string]any); ok {
			if hasToolParts(m) {
				return 0, fmt.Errorf("primer turns would split a tool_use/tool_result pair")
			}
		}
	}

	var out []any
	out = append(out, msgs[:insertAt]...)
	inserted := 0
	for _, t := range turns {
		entry := map[string]any{"role": t.Role, "content": t.Text}
		// 与插入点前一条同角色 → 合并,保持交替不变量。
		if len(out) > 0 {
			if prev, ok := out[len(out)-1].(map[string]any); ok {
				if role, _ := prev["role"].(string); role == t.Role {
					if s, ok := prev["content"].(string); ok {
						prev["content"] = s + "\n\n" + t.Text
						inserted++
						continue
					}
				}
			}
		}
		out = append(out, entry)
		inserted++
	}
	out = append(out, msgs[insertAt:]...)
	e.Body["messages"] = out
	return inserted, nil
}

func hasToolParts(m map[string]any) bool {
	blocks, ok := m["content"].([]any)
	if !ok {
		return false
	}
	for _, b := range blocks {
		if obj, ok := b.(map[string]any); ok {
			if typ, _ := obj["type"].(string); typ == "tool_use" || typ == "tool_result" {
				return true
			}
		}
	}
	return false
}

func jsonLen(v any) int {
	b, err := json.Marshal(v)
	if err != nil {
		return 0
	}
	return len(b)
}

func joinStrings(ss []string, sep string) string { return strings.Join(ss, sep) }
