package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// SkillLoadExecutor 是 B 档:把技能从「已注入」变成「按需加载」,
// 并把技能声明的 unlocks 交给工具平面做门控。
type SkillLoadExecutor struct {
	Skills         SkillSource
	MaxResultChars int
}

func (e *SkillLoadExecutor) Defs() []ToolDef {
	if e.Skills.Len() == 0 {
		return nil
	}
	names := strings.Join(e.Skills.Names(), ", ")
	return []ToolDef{{
		Name: SkillLoad,
		Description: fmt.Sprintf(
			"加载一个本地技能的完整规程。可用技能:%s。需要按某个技能的流程做事时,先调用它拿到完整步骤。", names),
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name": map[string]any{"type": "string", "description": "技能名(见描述里的列表)"},
			},
			"required": []string{"name"},
		},
	}}
}

func (e *SkillLoadExecutor) Handles(name string) bool { return name == SkillLoad }

func (e *SkillLoadExecutor) Call(_ string, args any) (string, bool, error) {
	wanted := argString(args, "name")
	if wanted == "" {
		return "缺少 `name` 参数", true, nil
	}
	body, ok := e.Skills.Body(wanted)
	if !ok {
		// 把可用列表回给模型:这比一句「not found」有用得多。
		return fmt.Sprintf("技能 `%s` 不存在。可用:%s", wanted, strings.Join(e.Skills.Names(), ", ")), true, nil
	}
	if e.MaxResultChars > 0 {
		r := []rune(body)
		if len(r) > e.MaxResultChars {
			body = string(r[:e.MaxResultChars]) + "\n…[技能正文被截断]"
		}
	}
	return body, false, nil
}

func (e *SkillLoadExecutor) Close() {}

// UnlocksFrom 返回这次技能加载解锁了哪些模式(供会话持久化)。
func (e *SkillLoadExecutor) UnlocksFrom(call ToolCall) ([]string, string) {
	name := argString(call.Arguments, "name")
	if name == "" {
		return nil, ""
	}
	return e.Skills.UnlocksFor(name), name
}

// McpExecutor 是 C 档:把一次工具调用转发给 MCP 服务器。
type McpExecutor struct {
	Server         McpServerCfg
	Client         McpClient
	MaxResultChars int
	defs           []ToolDef
}

func (e *McpExecutor) Defs() []ToolDef {
	if e.defs != nil {
		return e.defs
	}
	tools, err := e.Client.ListTools()
	if err != nil {
		e.defs = []ToolDef{}
		return e.defs
	}
	for _, t := range tools {
		// allow_tools 白名单:非空 = 只暴露列出的工具。
		if len(e.Server.AllowTools) > 0 && !allowlisted(e.Server.AllowTools, t.Name) {
			continue
		}
		e.defs = append(e.defs, ToolDef{
			Name:        "xk__mcp__" + e.Server.Name + "__" + t.Name,
			Description: t.Description,
			Schema:      t.InputSchema,
		})
	}
	return e.defs
}

func allowlisted(patterns []string, tool string) bool {
	for _, p := range patterns {
		if GlobMatch(p, tool) {
			return true
		}
	}
	return false
}

func (e *McpExecutor) Handles(name string) bool {
	for _, def := range e.Defs() {
		if def.Name == name {
			return true
		}
	}
	return false
}

func (e *McpExecutor) Call(name string, args any) (string, bool, error) {
	return e.CallContext(context.Background(), name, args)
}

func (e *McpExecutor) CallContext(ctx context.Context, name string, args any) (string, bool, error) {
	if !e.Handles(name) {
		return "", true, fmt.Errorf("MCP tool %q is not registered or allowlisted", name)
	}
	tool := strings.TrimPrefix(name, "xk__mcp__"+e.Server.Name+"__")
	if len(e.Server.AllowTools) > 0 && !allowlisted(e.Server.AllowTools, tool) {
		return "", true, fmt.Errorf("MCP tool %q is not allowlisted", tool)
	}
	raw, err := json.Marshal(args)
	if err != nil {
		return "", true, fmt.Errorf("参数序列化失败: %w", err)
	}
	var text string
	var isError bool
	if client, ok := e.Client.(interface {
		CallToolContext(context.Context, string, json.RawMessage) (string, bool, error)
	}); ok {
		text, isError, err = client.CallToolContext(ctx, tool, raw)
	} else {
		text, isError, err = e.Client.CallTool(tool, raw)
	}
	if err != nil {
		return err.Error(), true, nil
	}
	if e.MaxResultChars > 0 {
		r := []rune(text)
		if len(r) > e.MaxResultChars {
			text = string(r[:e.MaxResultChars]) + "\n…[工具结果被截断]"
		}
	}
	return text, isError, nil
}

func (e *McpExecutor) Close() {
	if e.Client != nil {
		e.Client.Close()
	}
}

func argString(args any, key string) string {
	m, ok := args.(map[string]any)
	if !ok {
		return ""
	}
	s, _ := m[key].(string)
	return s
}
