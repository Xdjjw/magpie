package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

// McpServerCfg 与 xp-core 对齐,支持 stdio 与 Streamable HTTP 传输。
type McpServerCfg struct {
	Name      string   `json:"name"`
	Enabled   bool     `json:"enabled"`
	Transport string   `json:"transport"` // stdio | http
	Command   string   `json:"command"`
	Args      []string `json:"args"`
	Cwd       string   `json:"cwd"`
	// http 传输:端点 URL 与额外请求头(例如 Authorization)。
	URL        string            `json:"url"`
	Headers    map[string]string `json:"headers"`
	AllowTools []string          `json:"allow_tools"` // 非空 = 只暴露列出的工具
	Env        map[string]string `json:"env"`
}

// UnmarshalJSON keeps the Rust policy's default for a server that omits
// `enabled`: configured MCP servers are enabled unless explicitly disabled.
// The alias avoids recursing into this method while preserving the snake_case
// wire names above.
func (c *McpServerCfg) UnmarshalJSON(data []byte) error {
	type plain McpServerCfg
	*c = McpServerCfg{Enabled: true}
	return json.Unmarshal(data, (*plain)(c))
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
	mu        sync.RWMutex
	closed    bool
	// conn is the context MCP handshakes run under: Close cancels it so a
	// connection still being made is given up rather than left to finish
	// against a plane that is gone.
	conn      context.Context
	connAbort context.CancelFunc
}

// connectable is the servers a plane may connect: the plane's own switch
// first, then each server's. Both entry points take their servers from
// here, so a server the configuration disabled is never connected,
// whichever one a caller picked.
func connectable(cfg ToolsConfig, servers []McpServerCfg) []McpServerCfg {
	if !cfg.Enabled {
		return nil
	}
	out := make([]McpServerCfg, 0, len(servers))
	for _, server := range servers {
		if server.Enabled {
			out = append(out, server)
		}
	}
	return out
}

// Build connects the configured MCP servers before it returns. **不会失败**:
// 某个 MCP 服务器连不上只记进 McpStatus, 因为一个本地 dev server 挂掉不该让整个代理起不来。
func Build(cfg ToolsConfig, skills SkillSource, servers []McpServerCfg) *Plane {
	p := newPlane(cfg, skills)
	for _, server := range connectable(cfg, servers) {
		p.connectMCP(server)
	}
	return p
}

// BuildAsync constructs the local executors immediately and connects MCP
// servers in background goroutines. MCP discovery is optional capability
// loading; it must not hold the first semantic request hostage to a dead or
// slow local process. Requests made before discovery completes simply omit
// those MCP tools and pick them up on a later round/request.
func BuildAsync(cfg ToolsConfig, skills SkillSource, servers []McpServerCfg) *Plane {
	p := newPlane(cfg, skills)
	for _, server := range connectable(cfg, servers) {
		go p.connectMCP(server)
	}
	return p
}

func newPlane(cfg ToolsConfig, skills SkillSource) *Plane {
	p := &Plane{cfg: cfg, guard: Guard{Approval: cfg.Approval, Allowlist: append([]string(nil), cfg.Allowlist...)}}
	p.conn, p.connAbort = context.WithCancel(context.Background())

	if cfg.Enabled && cfg.InjectSkillTool && skills.Len() > 0 {
		exec := &SkillLoadExecutor{Skills: skills, MaxResultChars: cfg.MaxResultChars}
		p.executors = append(p.executors, exec)
		p.defs = append(p.defs, exec.Defs()...)
	}

	// 门控模式 = 所有技能 unlocks 的并集。没有技能声明 unlocks 就全量注入。
	for _, pat := range skills.Unlocks() {
		if !containsStr(p.gating, pat) {
			p.gating = append(p.gating, pat)
		}
	}
	return p
}

func (p *Plane) connectMCP(server McpServerCfg) {
	client, err := ConnectMcpContext(p.conn, server, p.cfg.CallTimeoutMS)
	if err != nil {
		// 失败可见但不阻塞:如实记进状态,并**说出来** —— 状态没有任何
		// 生产读侧(McpStatus 只被测试调用),只记不报等于静默失败。
		log.Printf("xkeysmith: mcp %s (%s) 不可用: %v", server.Name, server.Transport, err)
		p.mu.Lock()
		if !p.closed {
			p.mcpStatus = append(p.mcpStatus, McpStatus{
				Name: server.Name, Transport: server.Transport, Tools: 0, Error: err.Error(),
			})
		}
		p.mu.Unlock()
		return
	}
	exec := &McpExecutor{Server: server, Client: client, MaxResultChars: p.cfg.MaxResultChars}
	defs := exec.Defs()
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		exec.Close()
		return
	}
	p.executors = append(p.executors, exec)
	p.defs = append(p.defs, defs...)
	p.mcpStatus = append(p.mcpStatus, McpStatus{
		Name: server.Name, Transport: server.Transport, Tools: len(defs),
	})
	p.mu.Unlock()
}

// Close is final and idempotent: connections already published are closed,
// and a handshake still in flight is aborted rather than left to finish
// against a plane that is gone.
func (p *Plane) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	executors := append([]Executor(nil), p.executors...)
	p.executors = nil
	p.mu.Unlock()
	if p.connAbort != nil {
		p.connAbort()
	}
	for _, e := range executors {
		e.Close()
	}
}

func (p *Plane) Config() *ToolsConfig { return &p.cfg }
func (p *Plane) AllDefs() []ToolDef {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return append([]ToolDef(nil), p.defs...)
}
func (p *Plane) GatingPatterns() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return append([]string(nil), p.gating...)
}
func (p *Plane) McpStatus() []McpStatus {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return append([]McpStatus(nil), p.mcpStatus...)
}

func (p *Plane) IsActive() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.cfg.Enabled && len(p.defs) > 0
}

// IsGated:这个工具是否需要先解锁。技能加载工具永远可用,
// 否则就没人能解锁任何东西了。
func (p *Plane) IsGated(tool string) bool {
	if tool == SkillLoad {
		return false
	}
	p.mu.RLock()
	gating := append([]string(nil), p.gating...)
	p.mu.RUnlock()
	for _, pat := range gating {
		if GlobMatch(pat, tool) {
			return true
		}
	}
	return false
}

// VisibleDefs 是当前解锁状态下应下发的工具定义。
func (p *Plane) VisibleDefs(unlock UnlockSet) []ToolDef {
	p.mu.RLock()
	defs := append([]ToolDef(nil), p.defs...)
	p.mu.RUnlock()
	return p.visibleDefs(defs, unlock)
}

func (p *Plane) visibleDefs(defs []ToolDef, unlock UnlockSet) []ToolDef {
	p.mu.RLock()
	gating := append([]string(nil), p.gating...)
	p.mu.RUnlock()
	var out []ToolDef
	for _, d := range defs {
		if d.Name == SkillLoad || !matchesAny(gating, d.Name) {
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

func matchesAny(patterns []string, tool string) bool {
	for _, pat := range patterns {
		if GlobMatch(pat, tool) {
			return true
		}
	}
	return false
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
	return p.ExecuteContext(context.Background(), call)
}

func (p *Plane) ExecuteContext(ctx context.Context, call ToolCall) ToolResult {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(p.cfg.CallTimeoutMS)*time.Millisecond)
	defer cancel()
	return p.executeVisible(ctx, call, visibleNames(p.VisibleDefs(UnlockSet{})))
}

func (p *Plane) executeVisible(ctx context.Context, call ToolCall, visible map[string]bool) ToolResult {
	start := time.Now()
	if !p.cfg.Enabled {
		return ToolResult{ID: call.ID, Name: call.Name, Content: "gateway tools are disabled", IsError: true, Denied: true}
	}
	p.mu.RLock()
	defs := append([]ToolDef(nil), p.defs...)
	executors := append([]Executor(nil), p.executors...)
	guard := p.guard
	p.mu.RUnlock()
	known := false
	for _, def := range defs {
		known = known || def.Name == call.Name
	}
	if !known {
		return ToolResult{ID: call.ID, Name: call.Name, Content: fmt.Sprintf("未知工具 `%s`(网关没有注册它)", call.Name), IsError: true, Denied: true}
	}
	if !visible[call.Name] {
		return ToolResult{ID: call.ID, Name: call.Name, Content: "tool is not available in this request; load its skill first", IsError: true, Denied: true}
	}
	if !guard.Allows(call.Name) {
		return ToolResult{
			ID: call.ID, Name: call.Name,
			Content: guard.DenialReason(call.Name), IsError: true, Denied: true,
		}
	}
	for _, e := range executors {
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
	plane     *Plane
	unlock    UnlockSet
	available []ToolDef
	active    bool
	visible   map[string]bool
}

func (p *Plane) ForRequest(unlock UnlockSet) *RequestPlane {
	p.mu.RLock()
	defs := append([]ToolDef(nil), p.defs...)
	active := p.cfg.Enabled && len(defs) > 0
	p.mu.RUnlock()
	r := &RequestPlane{
		plane: p, available: defs, active: active,
		unlock: UnlockSet{Skills: append([]string(nil), unlock.Skills...), Patterns: append([]string(nil), unlock.Patterns...)},
	}
	r.BeginRound()
	return r
}

func (r *RequestPlane) BeginRound() {
	r.visible = visibleNames(r.plane.visibleDefs(r.available, r.unlock))
}
func (r *RequestPlane) IsActive() bool { return r.active }
func (r *RequestPlane) VisibleDefs() []ToolDef {
	defs := r.plane.visibleDefs(r.available, r.unlock)
	return defs
}
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
