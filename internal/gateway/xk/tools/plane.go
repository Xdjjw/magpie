package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
)

// McpServerCfg 与 xp-core 对齐,支持 stdio 与 Streamable HTTP 传输。
type McpServerCfg struct {
	Name      string
	Enabled   bool
	Transport string // stdio | http
	Command   string
	Args      []string
	Cwd       string
	// http 传输:端点 URL 与额外请求头(例如 Authorization)。
	URL        string
	Headers    map[string]string
	AllowTools []string // 非空 = 只暴露列出的工具
	Env        map[string]string
}

// ToolsConfig 与 xp-core ToolsCfg 对齐。
type ToolsConfig struct {
	Enabled         bool
	InjectSkillTool bool
	Approval        ApprovalMode
	Allowlist       []string
	MaxToolRounds   int
	MaxResultChars  int
	CallTimeoutMS   uint64
}

func DefaultToolsConfig() ToolsConfig {
	return ToolsConfig{
		Enabled:         false,
		InjectSkillTool: true,
		Approval:        ApprovalAllow,
		MaxToolRounds:   4,
		MaxResultChars:  8000,
		CallTimeoutMS:   30000,
	}
}

// McpStatus 是一个 MCP 服务器的连接结果。失败**不阻塞启动**,但必须可见。
type McpStatus struct {
	Name      string `json:"name"`
	Transport string `json:"transport"`
	Tools     int    `json:"tools"`
	Error     string `json:"error,omitempty"`
}

// Executor 是一个工具执行器(技能加载 / 一个 MCP 服务器)。
type Executor interface {
	Defs() []ToolDef
	Handles(name string) bool
	Call(name string, args any) (content string, isError bool, err error)
	// Close 释放资源(MCP 子进程等)。
	Close()
}

// Plane 是工具平面。
type Plane struct {
	cfg       ToolsConfig
	guard     Guard
	executors []Executor
	defs      []ToolDef
	// gating 是所有技能 unlocks 的并集 —— 命中这些模式的工具默认不下发。
	gating    []string
	mcpStatus []McpStatus
	mu        sync.Mutex
}

// Build 构建工具平面。**不会失败**:某个 MCP 服务器连不上只记进 McpStatus,
// 因为一个本地 dev server 挂掉不该让整个代理起不来。
func Build(cfg ToolsConfig, skills SkillSource, servers []McpServerCfg) *Plane {
	p := &Plane{cfg: cfg, guard: Guard{Approval: cfg.Approval, Allowlist: cfg.Allowlist}}

	if cfg.Enabled && cfg.InjectSkillTool && skills.Len() > 0 {
		exec := &SkillLoadExecutor{Skills: skills, MaxResultChars: cfg.MaxResultChars}
		p.executors = append(p.executors, exec)
		p.defs = append(p.defs, exec.Defs()...)
	}

	if cfg.Enabled {
		for _, server := range servers {
			if !server.Enabled {
				continue
			}
			client, err := ConnectMcp(server, cfg.CallTimeoutMS)
			if err != nil {
				// 失败可见但不阻塞:如实记进状态。
				p.mcpStatus = append(p.mcpStatus, McpStatus{
					Name: server.Name, Transport: server.Transport, Tools: 0, Error: err.Error(),
				})
				continue
			}
			exec := &McpExecutor{Server: server, Client: client, MaxResultChars: cfg.MaxResultChars}
			p.executors = append(p.executors, exec)
			p.defs = append(p.defs, exec.Defs()...)
			p.mcpStatus = append(p.mcpStatus, McpStatus{
				Name: server.Name, Transport: server.Transport, Tools: len(exec.Defs()),
			})
		}
	}

	// 门控模式 = 所有技能 unlocks 的并集。没有技能声明 unlocks 就全量注入。
	for _, pat := range skills.Unlocks() {
		if !containsStr(p.gating, pat) {
			p.gating = append(p.gating, pat)
		}
	}
	return p
}

func (p *Plane) Close() {
	for _, e := range p.executors {
		e.Close()
	}
}

func (p *Plane) Config() *ToolsConfig     { return &p.cfg }
func (p *Plane) AllDefs() []ToolDef       { return p.defs }
func (p *Plane) GatingPatterns() []string { return p.gating }
func (p *Plane) McpStatus() []McpStatus   { return p.mcpStatus }

func (p *Plane) IsActive() bool { return p.cfg.Enabled && len(p.defs) > 0 }

// IsGated:这个工具是否需要先解锁。技能加载工具永远可用,
// 否则就没人能解锁任何东西了。
func (p *Plane) IsGated(tool string) bool {
	if tool == SkillLoad {
		return false
	}
	for _, pat := range p.gating {
		if GlobMatch(pat, tool) {
			return true
		}
	}
	return false
}

// VisibleDefs 是当前解锁状态下应下发的工具定义。
func (p *Plane) VisibleDefs(unlock UnlockSet) []ToolDef {
	var out []ToolDef
	for _, d := range p.defs {
		if !p.IsGated(d.Name) {
			out = append(out, d)
			continue
		}
		for _, pat := range unlock.Patterns {
			if GlobMatch(pat, d.Name) {
				out = append(out, d)
				break
			}
		}
	}
	return out
}

// TurnDisposition 是一轮工具调用的归属判定。
type TurnDisposition int

const (
	// TurnNotOurs 本轮没有网关工具 → 不插手。
	TurnNotOurs TurnDisposition = iota
	// TurnAllOurs 全是 xk__ 工具 → 网关逐个执行。
	TurnAllOurs
	// TurnHandBack is a mixed turn. The loop defers client calls before
	// exposing a response, executes gateway calls and asks the model again.
	TurnHandBack
)

func (p *Plane) Disposition(calls []ToolCall) TurnDisposition {
	if len(calls) == 0 {
		return TurnNotOurs
	}
	gateway, other := 0, 0
	for _, c := range calls {
		if IsGatewayTool(c.Name) {
			gateway++
		} else {
			other++
		}
	}
	switch {
	case gateway == 0:
		return TurnNotOurs
	case other == 0:
		return TurnAllOurs
	default:
		return TurnHandBack
	}
}

// Execute 执行一个网关工具调用。审批拒绝也是**一种结果**(denied=true),
// 不是错误:回填给模型,它就能改策略。
func (p *Plane) Execute(call ToolCall) ToolResult {
	return p.executeVisible(context.Background(), call, visibleNames(p.VisibleDefs(UnlockSet{})))
}

func (p *Plane) executeVisible(ctx context.Context, call ToolCall, visible map[string]bool) ToolResult {
	start := time.Now()
	if !p.cfg.Enabled {
		return ToolResult{ID: call.ID, Name: call.Name, Content: "gateway tools are disabled", IsError: true, Denied: true}
	}
	known := false
	for _, def := range p.defs {
		known = known || def.Name == call.Name
	}
	if !known {
		return ToolResult{ID: call.ID, Name: call.Name, Content: fmt.Sprintf("未知工具 `%s`(网关没有注册它)", call.Name), IsError: true, Denied: true}
	}
	if !visible[call.Name] {
		return ToolResult{ID: call.ID, Name: call.Name, Content: "tool is not available in this request; load its skill first", IsError: true, Denied: true}
	}
	if !p.guard.Allows(call.Name) {
		return ToolResult{
			ID: call.ID, Name: call.Name,
			Content: p.guard.DenialReason(call.Name), IsError: true, Denied: true,
		}
	}
	for _, e := range p.executors {
		if !e.Handles(call.Name) {
			continue
		}
		var content string
		var isError bool
		var err error
		if contextual, ok := e.(interface {
			CallContext(context.Context, string, any) (string, bool, error)
		}); ok {
			content, isError, err = contextual.CallContext(ctx, call.Name, call.Arguments)
		} else {
			content, isError, err = e.Call(call.Name, call.Arguments)
		}
		if err != nil {
			content = err.Error()
			isError = true
		}
		res := ToolResult{
			ID: call.ID, Name: call.Name, Content: content,
			IsError: isError, DurationMS: uint64(time.Since(start).Milliseconds()),
		}
		if loader, ok := e.(*SkillLoadExecutor); ok && !res.IsError {
			res.Unlocked, res.UnlockedBy = loader.UnlocksFrom(call)
		}
		return res
	}
	return ToolResult{
		ID: call.ID, Name: call.Name,
		Content: fmt.Sprintf("未知工具 `%s`(网关没有注册它)", call.Name),
		IsError: true, DurationMS: uint64(time.Since(start).Milliseconds()),
	}
}

func visibleNames(defs []ToolDef) map[string]bool {
	out := map[string]bool{}
	for _, def := range defs {
		out[def.Name] = true
	}
	return out
}

// RequestPlane snapshots the tools advertised in each round. Skill loads
// unlock tools for the next request, never for guessed calls in the same reply.
type RequestPlane struct {
	plane   *Plane
	unlock  UnlockSet
	visible map[string]bool
}

func (p *Plane) ForRequest(unlock UnlockSet) *RequestPlane {
	r := &RequestPlane{plane: p, unlock: UnlockSet{Skills: append([]string(nil), unlock.Skills...), Patterns: append([]string(nil), unlock.Patterns...)}}
	r.BeginRound()
	return r
}

func (r *RequestPlane) BeginRound()    { r.visible = visibleNames(r.plane.VisibleDefs(r.unlock)) }
func (r *RequestPlane) IsActive() bool { return r.plane.IsActive() }
func (r *RequestPlane) Disposition(calls []ToolCall) TurnDisposition {
	return r.plane.Disposition(calls)
}
func (r *RequestPlane) Truncate(content string) string { return r.plane.Truncate(content) }
func (r *RequestPlane) Unlocks() UnlockSet {
	return UnlockSet{Skills: append([]string(nil), r.unlock.Skills...), Patterns: append([]string(nil), r.unlock.Patterns...)}
}
func (r *RequestPlane) Execute(call ToolCall) ToolResult {
	return r.ExecuteContext(context.Background(), call)
}

func (r *RequestPlane) ExecuteContext(ctx context.Context, call ToolCall) ToolResult {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(r.plane.cfg.CallTimeoutMS)*time.Millisecond)
	defer cancel()
	result := r.plane.executeVisible(ctx, call, r.visible)
	if !result.IsError && !result.Denied {
		for _, pattern := range result.Unlocked {
			if !containsStr(r.unlock.Patterns, pattern) {
				r.unlock.Patterns = append(r.unlock.Patterns, pattern)
			}
		}
		if result.UnlockedBy != "" && !containsStr(r.unlock.Skills, result.UnlockedBy) {
			r.unlock.Skills = append(r.unlock.Skills, result.UnlockedBy)
		}
	}
	return result
}

// Truncate 按字符上限截断工具结果,并**明确标注**被截断。
func (p *Plane) Truncate(content string) string {
	max := p.cfg.MaxResultChars
	if max <= 0 {
		return content
	}
	r := []rune(content)
	if len(r) <= max {
		return content
	}
	return string(r[:max]) + fmt.Sprintf("\n…[truncated: %d of %d chars]", max, len(r))
}

// SkillSource 是工具平面需要的技能信息(由 skills.Set 满足)。
type SkillSource interface {
	Len() int
	Names() []string
	Unlocks() []string
	UnlocksFor(name string) []string
	Body(name string) (string, bool)
}

func containsStr(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// jsonString 是回填工具结果时的稳定序列化。
func jsonString(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return strings.TrimSpace(fmt.Sprint(v))
	}
	return string(b)
}
