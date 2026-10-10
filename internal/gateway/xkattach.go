package gateway

// Stage ③ wiring: when the model a client asks for is served by the
// xkeysmith provider (the local semantic gateway), the request runs the
// xk ladder — judge, escalate L0→L4, restore — instead of a plain relay.
//
// 为什么单独一条通路而不是把阶梯塞进 attempt 内部:magpie 的 attempt 里
// 有大量 vendor 兼容分支,语义阶梯需要的只是「发一次、判一次、升一层」。
// 把它做成一条独立通路,既不动那些分支,也让阶梯逻辑保持在
// internal/gateway/xk/attach 里可被假上游穷举测试。

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/yetone/magpie/internal/settings"

	"github.com/yetone/magpie/internal/gateway/xk/attach"
	"github.com/yetone/magpie/internal/gateway/xk/audit"
	"github.com/yetone/magpie/internal/gateway/xk/detect"
	"github.com/yetone/magpie/internal/gateway/xk/govern"
	"github.com/yetone/magpie/internal/gateway/xk/rewrite"
	"github.com/yetone/magpie/internal/gateway/xk/skills"
	"github.com/yetone/magpie/internal/gateway/xk/tools"
	"github.com/yetone/magpie/internal/provider"
)

// xkEnabled reports whether p is the local semantic gateway.
func xkEnabled(p provider.Provider) bool { return p.Preset == "xkeysmith" || p.ID == "xkeysmith" }

// xkSettings is the ladder's configuration, read from the server once.
type xkSettings struct {
	pipeline *rewrite.Pipeline
	detector *detect.Detector
	limits   govern.Limits
	pipeCfg  govern.PipelineConfig
	sessions *govern.SessionRegistry
	audit    *audit.Plane
	// 能力平面(A 档技能 + B/C 档工具),由环境变量/配置决定是否启用。
	skills *skills.Set
	tools  *tools.Plane
}

// xk returns the ladder settings, building them on first use.
func (s *Server) xk() *xkSettings {
	s.xkOnce.Do(func() {
		pipeline, err := rewrite.NewPipeline(rewrite.DefaultRewriteConfig(), govern.AllLayers)
		if err != nil {
			s.xkErr = err
			return
		}
		limits := govern.DefaultLimits()
		cfg := audit.DefaultConfig()
		cfg.Dir = s.xkAuditDir()
		plane, err := audit.Build(cfg)
		if err != nil {
			// 审计失败不该让语义通路整体不可用:退化为 NullSink。
			plane, _ = audit.Build(func() audit.Config { c := cfg; c.Enabled = false; return c }())
		}
		skillSet := xkLoadSkills()
		toolsPlane := xkLoadTools(skillSet)
		s.xkSettings = &xkSettings{
			pipeline: pipeline,
			detector: detect.DefaultOrPanic(),
			limits:   limits,
			pipeCfg:  govern.DefaultPipelineConfig(),
			sessions: govern.NewSessionRegistry(limits),
			audit:    plane,
			skills:   skillSet,
			tools:    toolsPlane,
		}
	})
	if s.xkErr != nil {
		return nil
	}
	return s.xkSettings
}

// serveXkeysmith runs the semantic ladder for one request and writes the
// reply. It returns (status, message) like attempt does; message is empty
// on success.
func (s *Server) serveXkeysmith(w http.ResponseWriter, r *http.Request, from provider.Protocol, p provider.Provider, model string, body []byte, u *Usage) (int, string) {
	settings := s.xk()
	if settings == nil {
		msg := "xkeysmith ladder unavailable"
		return writeError(w, from, 502, msg), msg
	}

	// 补丁 3:外部 xkeysmith(如果用户用官方二进制而不是本内置核心)。
	// 探活/拉起失败都不阻塞——内置核心仍能接管整条通路。
	if s.xkSide == nil {
		s.xkSide = newXkSidecar()
	}
	s.xkSide.Ensure(xkSidecarURL(p))

	env, err := xkEnvelope(from, body)
	if err != nil {
		msg := "xkeysmith: bad request body: " + err.Error()
		return writeError(w, from, 400, msg), msg
	}

	proto := xkProto(from)
	stream := streamOf(body)
	sessionKey := xkSessionKey(r, env, model)

	// A 档:技能索引/全文注入到 L1。注入发生在 L1,所以 L0 仍是 pristine。
	if settings.skills != nil && settings.skills.Len() > 0 {
		payload := settings.skills.Payload(skills.ModeIndex, 6000, 12000)
		if payload != "" {
			settings.pipeline.SetSkills(payload, settings.skills.Names(), "index")
		}
	}
	// B/C 档:把当前解锁状态下可见的工具 schema 交给调用方注入(见 xkInjectTools)。
	unlock := tools.UnlockSet{}
	if settings.tools != nil {
		if s2, p2 := settings.sessions.UnlockSet(sessionKey); len(s2) > 0 || len(p2) > 0 {
			unlock = tools.UnlockSet{Skills: s2, Patterns: p2}
		}
		env = xkInjectTools(env, settings.tools, unlock)
	}

	cfg := &attach.LadderConfig{
		Pipeline:    settings.pipeline,
		Detector:    settings.detector,
		Limits:      settings.limits,
		PipelineCfg: settings.pipeCfg,
		EntryLayer:  govern.L0,
		MaxLayer:    govern.L4,
		Proto:       proto,
		SessionKey:  sessionKey,
		Sessions:    settings.sessions,
	}

	forward := func(probe []byte) attach.ProbeResult {
		// 探测一律非流式:流式客户端走 probe_then_replay,采纳后再重放。
		res, err := s.forward(r.Context(), p, from, upstreamPath(p, from, model), xkNonStream(probe), r.Header)
		if err != nil {
			return attach.ProbeResult{Err: err}
		}
		defer res.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(res.Body, 8<<20))
		in, out := usageOf(raw)
		return attach.ProbeResult{Status: res.StatusCode, Body: raw, InputTokens: in, OutputTokens: out}
	}

	start := time.Now()
	result, err := attach.Run(cfg, env, forward, xkObservation)
	if err != nil {
		msg := "xkeysmith ladder: " + err.Error()
		return writeError(w, from, 502, msg), msg
	}
	latency := time.Since(start).Milliseconds()

	// 审计:每条 attempt 一条记录。
	s.xkRecord(settings, r, p, model, from, stream, latency, result)

	// trace 头透出:客户端据此把这次响应与审计记录对上
	// (xkeysmith 的 explain 用同一个 trace)。
	xkAttachTraceHeaders(w, result)

	if !result.Accepted {
		// 不伪造成功:全层耗尽给明确错误(503),上游故障透传原响应。
		if result.StopReason == govern.StopUpstreamError {
			return xkWriteProbe(w, result.Attempt, result.Probe, from)
		}
		msg := "xkeysmith exhausted its rewrite ladder"
		w.Header().Set("x-xkeysmith-error", string(result.StopReason))
		code := writeError(w, from, 503, msg)
		return code, msg
	}

	// 采纳层:先看模型是否发起了**网关工具调用**(B 档)。有则网关自己执行、
	// 回填、再调上游,直到模型给出最终答复;触到闸门给明确错误。
	if settings.tools != nil && settings.tools.IsActive() && !xkIsSSE(result.Probe) {
		final, err := s.xkRunToolRounds(settings, r, p, model, from, result, sessionKey)
		if err != nil {
			msg := "xkeysmith tool loop: " + err.Error()
			return writeError(w, from, 502, msg), msg
		}
		if final.StopReason != "" {
			// 闸门触顶 = 明确错误,绝不把客户端执行不了的 xk__ 调用交还。
			msg := "xkeysmith tool rounds exhausted"
			w.Header().Set("x-xkeysmith-error", string(final.StopReason))
			code := writeError(w, from, 503, msg)
			return code, msg
		}
		// 解锁集写回会话(技能门控的持久化)。
		if len(final.Unlocked) > 0 {
			settings.sessions.RecordUnlocks(sessionKey, &settings.pipeCfg, nil, final.Unlocked)
		}
		body, _ := json.Marshal(final.FinalBody)
		return xkWriteRestored(w, result.Attempt, body, from)
	}

	// 响应侧的 L4 还原。
	if stream && xkIsSSE(result.Probe) {
		return xkWriteStream(w, result, from)
	}
	return xkWriteProbe(w, result.Attempt, result.Probe, from)
}

// xkRunToolRounds 执行工具轮:模型发起 xk__ 调用 → 网关执行 → 回填 → 再调上游。
func (s *Server) xkRunToolRounds(settings *xkSettings, r *http.Request, p provider.Provider, model string, from provider.Protocol, result *attach.LadderResult, sessionKey string) (*attach.ToolLoopResult, error) {
	dialect := tools.DialectChat
	switch {
	case from == provider.Anthropic:
		dialect = tools.DialectAnthropic
	case from == provider.Responses:
		dialect = tools.DialectResponses
	}
	budget := govern.NewBudget(settings.limits)
	cfg := &attach.ToolLoopConfig{
		Plane:     settings.tools,
		Dialect:   dialect,
		MaxRounds: 4,
		Budget:    budget,
	}
	// 第一轮的响应已经在阶梯里拿到了,直接作为循环的起点。
	request, _ := json.Marshal(result.Attempt.Envelope.Body)
	var reqBody map[string]any
	_ = json.Unmarshal(request, &reqBody)

	first := true
	roundTrip := func(body map[string]any) (map[string]any, error) {
		if first {
			first = false
			// 复用阶梯的探测结果作为第一轮响应。
			var resp map[string]any
			if err := json.Unmarshal(result.Probe.Body, &resp); err != nil {
				return nil, err
			}
			return resp, nil
		}
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		res, err := s.forward(r.Context(), p, from, upstreamPath(p, from, model), raw, r.Header)
		if err != nil {
			return nil, err
		}
		defer res.Body.Close()
		payload, _ := io.ReadAll(io.LimitReader(res.Body, 8<<20))
		var resp map[string]any
		if err := json.Unmarshal(payload, &resp); err != nil {
			return nil, err
		}
		return resp, nil
	}
	return attach.RunToolLoop(cfg, reqBody, roundTrip)
}

// xkProto maps magpie's protocol to the ladder's dialect name.
func xkProto(from provider.Protocol) string {
	if from == provider.Anthropic {
		return "anthropic"
	}
	return "chat"
}

// xkEnvelope builds a rewrite envelope from the client's body.
func xkEnvelope(from provider.Protocol, body []byte) (*rewrite.Envelope, error) {
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, err
	}
	wire := rewrite.WireOpenAi
	if from == provider.Anthropic {
		wire = rewrite.WireAnthropic
	}
	return rewrite.NewEnvelope(doc, wire), nil
}

// xkSessionKey derives the session key the same way xkeysmith does:
// an explicit header wins, else wire+model+first user text.
func xkSessionKey(r *http.Request, env *rewrite.Envelope, model string) string {
	for _, h := range []string{"x-session-id", "x-xkeysmith-session", "x-conversation-id", "conversation_id"} {
		if v := strings.TrimSpace(r.Header.Get(h)); v != "" {
			return v
		}
	}
	first := ""
	env.ForEachTextRef(rewrite.ScopeUserTurns, func(_ rewrite.SlotCtx, text string) {
		if first == "" {
			first = text
		}
	})
	if len(first) > 512 {
		first = string([]rune(first)[:512])
	}
	return "auto:" + audit.ShortHash([]byte(model+"\x1f"+first))
}

// xkObservation translates a probe response into the judge's input,
// absorbing the dialect differences (Chat / Anthropic / Responses).
func xkObservation(proto string, res attach.ProbeResult) *detect.Observation {
	obs := &detect.Observation{Status: res.Status}
	if res.Err != nil {
		obs.ErrorMessage = res.Err.Error()
		return obs
	}
	if proto == "anthropic" {
		var doc struct {
			StopReason string `json:"stop_reason"`
			Content    []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			Error *struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(res.Body, &doc); err == nil {
			obs.FinishReason = doc.StopReason
			for _, c := range doc.Content {
				if c.Type == "text" {
					obs.Text += c.Text
				}
			}
			if doc.Error != nil {
				obs.ErrorCode = doc.Error.Type
				obs.ErrorMessage = doc.Error.Message
			}
		}
		return obs
	}
	// OpenAI Chat / Responses.
	var doc struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Content   any    `json:"content"`
				Refusal   string `json:"refusal"`
				ToolCalls []any  `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		Output []struct {
			Type    string `json:"type"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
		Status string `json:"status"`
		Error  *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(res.Body, &doc); err != nil {
		return obs
	}
	if len(doc.Choices) > 0 {
		c := doc.Choices[0]
		obs.FinishReason = c.FinishReason
		obs.RefusalField = c.Message.Refusal
		obs.Text = xkTextOf(c.Message.Content)
		obs.ToolCalls = len(c.Message.ToolCalls)
	}
	for _, item := range doc.Output {
		if item.Type == "message" {
			for _, c := range item.Content {
				if c.Type == "output_text" {
					obs.Text += c.Text
				}
			}
		}
	}
	if doc.Error != nil {
		obs.ErrorCode = doc.Error.Code
		obs.ErrorMessage = doc.Error.Message
	}
	if doc.Status == "incomplete" {
		obs.FinishReason = "incomplete"
	}
	return obs
}

// xkTextOf flattens a Chat content field (string or parts).
func xkTextOf(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []any:
		var b strings.Builder
		for _, part := range t {
			if m, ok := part.(map[string]any); ok {
				if s, ok := m["text"].(string); ok {
					b.WriteString(s)
				}
			}
		}
		return b.String()
	default:
		return ""
	}
}

// xkNonStream forces stream:false on a probe so the reply is one JSON body.
func xkNonStream(body []byte) []byte {
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		return body
	}
	if _, ok := doc["stream"]; ok {
		doc["stream"] = false
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return body
	}
	return out
}

// xkIsSSE reports whether a probe reply is a stream (some upstreams ignore
// stream:false).
func xkIsSSE(res attach.ProbeResult) bool {
	return strings.HasPrefix(strings.TrimSpace(string(res.Body)), "data:") ||
		strings.Contains(string(res.Body), "\ndata: ")
}

// usageOf reads token usage from a reply body, tolerating both dialects.
func usageOf(body []byte) (uint64, uint64) {
	var doc struct {
		Usage *struct {
			PromptTokens     uint64 `json:"prompt_tokens"`
			CompletionTokens uint64 `json:"completion_tokens"`
			InputTokens      uint64 `json:"input_tokens"`
			OutputTokens     uint64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &doc); err != nil || doc.Usage == nil {
		return 0, 0
	}
	in := doc.Usage.PromptTokens + doc.Usage.InputTokens
	out := doc.Usage.CompletionTokens + doc.Usage.OutputTokens
	return in, out
}

// ─────────────────────────── 能力平面装载 ───────────────────────────

// xkLoadSkills 从环境变量指定的目录加载技能(默认 ~/.magpie/xkeysmith/skills)。
// 技能正文是第三方文本,默认**不注入**——由配置显式打开。
func xkLoadSkills() *skills.Set {
	dir := os.Getenv("XKEYSMITH_SKILLS_DIR")
	if dir == "" {
		dir = filepath.Join(settings.Dir(), "xkeysmith", "skills")
	}
	set, err := skills.Load(dir)
	if err != nil {
		return skills.Empty()
	}
	return set
}

// xkLoadTools 构建工具平面(默认关闭;由 XKEYSMITH_TOOLS=1 打开)。
//
// 与 Rust 版一致的默认:approval 代码默认 allow(零配置即用),
// 范围控制的主力是**注入侧**(哪些工具会被下发)。
func xkLoadTools(skillSet *skills.Set) *tools.Plane {
	cfg := tools.DefaultToolsConfig()
	cfg.Enabled = os.Getenv("XKEYSMITH_TOOLS") == "1"
	if v := os.Getenv("XKEYSMITH_APPROVAL"); v == "deny" {
		cfg.Approval = tools.ApprovalDeny
	}
	if v := os.Getenv("XKEYSMITH_ALLOWLIST"); v != "" {
		for _, p := range strings.Split(v, ",") {
			if p = strings.TrimSpace(p); p != "" {
				cfg.Allowlist = append(cfg.Allowlist, p)
			}
		}
	}
	return tools.Build(cfg, skillSet, xkLoadMcpServers())
}

// xkLoadMcpServers 读 MCP 服务器配置(JSON,由桌面端或用户提供)。
func xkLoadMcpServers() []tools.McpServerCfg {
	raw := os.Getenv("XKEYSMITH_MCP_SERVERS")
	if raw == "" {
		return nil
	}
	var servers []tools.McpServerCfg
	if err := json.Unmarshal([]byte(raw), &servers); err != nil {
		return nil
	}
	// 展开 ${VAR} 与相对 cwd(与 xp-core 的 expand_env/绝对化同义)。
	base := settings.Dir()
	for i := range servers {
		servers[i].Command = tools.EnvExpand(servers[i].Command)
		for j := range servers[i].Args {
			servers[i].Args[j] = tools.EnvExpand(servers[i].Args[j])
		}
		servers[i].Cwd = tools.AbsCwd(tools.EnvExpand(servers[i].Cwd), base)
	}
	return servers
}

// xkInjectTools 把可见的 xk__ 工具 schema 注入请求体。
//
// 方言差异在这里吸收:Chat 走顶层 tools;Anthropic 走顶层 tools
// (input_schema 形状);Responses 走顶层 tools(扁平形状)。
// 渐进式披露:只注入**当前解锁状态下可见**的工具。
func xkInjectTools(env *rewrite.Envelope, plane *tools.Plane, unlock tools.UnlockSet) *rewrite.Envelope {
	if plane == nil || !plane.IsActive() {
		return env
	}
	visible := plane.VisibleDefs(unlock)
	if len(visible) == 0 {
		return env
	}
	switch env.Dialect {
	case rewrite.DialectResponses:
		var list []any
		for _, d := range visible {
			list = append(list, map[string]any{
				"type": "function", "name": d.Name,
				"description": d.Description, "parameters": d.Schema,
			})
		}
		env.Body["tools"] = appendTools(env.Body["tools"], list)
	default:
		if env.Wire == rewrite.WireAnthropic {
			var list []any
			for _, d := range visible {
				list = append(list, map[string]any{
					"name": d.Name, "description": d.Description, "input_schema": d.Schema,
				})
			}
			env.Body["tools"] = appendTools(env.Body["tools"], list)
		} else {
			var list []any
			for _, d := range visible {
				list = append(list, map[string]any{
					"type": "function",
					"function": map[string]any{
						"name": d.Name, "description": d.Description, "parameters": d.Schema,
					},
				})
			}
			env.Body["tools"] = appendTools(env.Body["tools"], list)
		}
	}
	return env
}

func appendTools(existing any, add []any) []any {
	list, _ := existing.([]any)
	return append(list, add...)
}
