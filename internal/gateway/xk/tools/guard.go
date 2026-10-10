// Package tools is the capability plane's B/C tiers, ported from xkeysmith's
// xp-tools (Rust): inject `xk__` tool schemas, execute the model's calls to
// them inside the gateway, and bridge MCP servers.
//
// 两条不变量:
//   - **只接管自己的工具**:一轮里若出现客户端工具调用,整轮原样交还客户端;
//   - **渐进式披露**:技能用 `unlocks:` 声明解锁哪些工具,被门控的工具默认不下发。
package tools

import "strings"

// Prefix 是网关注入的工具名前缀。客户端不会用这个前缀,因此不会撞名。
const Prefix = "xk__"

// SkillLoad 是网关自己提供的技能加载工具(B 档)。
const SkillLoad = "xk__skill_load"

// ApprovalMode 是审批策略。
type ApprovalMode string

const (
	ApprovalAllow ApprovalMode = "allow"
	ApprovalDeny  ApprovalMode = "deny"
)

// Guard 是审批闸门:拒绝也是一种**结果**——回填给模型一个明确的
// 「被拒绝」工具结果,它就能改策略,而不是卡在那里。
type Guard struct {
	Approval  ApprovalMode
	Allowlist []string
}

func (g Guard) Allows(tool string) bool {
	if g.Approval == ApprovalAllow {
		return true
	}
	for _, p := range g.Allowlist {
		if GlobMatch(p, tool) {
			return true
		}
	}
	return false
}

// DenialReason 写进工具结果,让模型知道下一步该怎么做。
func (g Guard) DenialReason(tool string) string {
	return "tool `" + tool + "` 未获授权:tools.approval = " + string(g.Approval) +
		",且它不在 allowlist 里。请改用已授权的工具,或让运维把它加进 tools.allowlist。"
}

// GlobMatch 是极简通配匹配:只支持 `*`,但支持多个。
// 刻意不引入 glob 库:要匹配的是工具名(字母数字下划线),30 行实现更可控。
func GlobMatch(pattern, text string) bool {
	if pattern == "*" {
		return true
	}
	if !strings.Contains(pattern, "*") {
		return pattern == text
	}
	parts := strings.Split(pattern, "*")
	cursor := 0
	for i, part := range parts {
		if part == "" {
			continue
		}
		if i == 0 {
			if !strings.HasPrefix(text[cursor:], part) {
				return false
			}
			cursor += len(part)
			continue
		}
		if i == len(parts)-1 && !strings.HasSuffix(pattern, "*") {
			return strings.HasSuffix(text[cursor:], part)
		}
		pos := strings.Index(text[cursor:], part)
		if pos < 0 {
			return false
		}
		cursor += pos + len(part)
	}
	return true
}

// ─────────────────────────── 工具定义与结果 ───────────────────────────

type ToolDef struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Schema      any    `json:"schema"`
}

type ToolCall struct {
	ID           string
	Name         string
	Arguments    any
	RawArguments string // 原始 JSON 字符串,回填时用它(避免二次序列化改变字节)
}

type ToolResult struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Content    string `json:"content"`
	IsError    bool   `json:"is_error"`
	Denied     bool   `json:"denied"`
	DurationMS uint64 `json:"duration_ms"`
	// Unlocked 是这次调用解锁的工具名模式(技能门控)。放在结果里而不是
	// 只写在平面内部:流式路径下解锁发生在响应返回之后,只有让观察者
	// 拿到它,才能把解锁状态持久化到会话里。
	Unlocked   []string `json:"unlocked"`
	UnlockedBy string   `json:"unlocked_by"`
}

// IsGatewayTool 判断一个调用是不是网关自己的工具。
func IsGatewayTool(name string) bool { return strings.HasPrefix(name, Prefix) }

// UnlockSet 是会话的解锁集。
type UnlockSet struct {
	Skills   []string
	Patterns []string
}
