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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
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
	skills     *skills.Set
	tools      *tools.Plane
	toolConfig tools.ToolsConfig
}

// xk returns the ladder settings, building them on first use.
func (s *Server) xk() *xkSettings {
	s.xkOnce.Do(func() {
		// 首次运行即有默认人格:总开关默认开,注入不该等到有人拨过开关。
		XkPersonaInit()
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
		skillMode := xkSkillMode()
		// `off` also removes the skill loader from the tool plane. This keeps
		// the advertised prompt and executable capability in lockstep.
		toolSkills := skillSet
		if skillMode == skills.ModeOff {
			toolSkills = skills.Empty()
		}
		toolsPlane := xkLoadTools(toolSkills)
		if skillSet.Len() > 0 {
			inject := skillMode == skills.ModeFull ||
				(skillMode == skills.ModeIndex && toolsPlane.IsActive())
			switch {
			case inject:
				pipeline.SetSkills(skillSet.Payload(skillMode, 6000, 12000), skillSet.Names(), string(skillMode))
			case skillMode == skills.ModeIndex:
				// index 是「先给索引,要正文就调 xk__skill_load」;工具平面
				// 没开时模型调不动它,所以不注入是对的 —— 但「技能目录非空
				// 却一个字都没进提示词」必须说出来,不能静默降级。
				log.Printf("xkeysmith: %d 个技能未注入: XKEYSMITH_SKILLS_MODE=index 需要工具平面(XKEYSMITH_TOOLS=1);"+
					" 不依赖工具的写法是 XKEYSMITH_SKILLS_MODE=full", skillSet.Len())
			}
		}
		s.xkSettings = &xkSettings{
			pipeline:   pipeline,
			detector:   detect.DefaultOrPanic(),
			limits:     limits,
			pipeCfg:    govern.DefaultPipelineConfig(),
			sessions:   govern.NewSessionRegistry(limits),
			audit:      plane,
			skills:     skillSet,
			tools:      toolsPlane,
			toolConfig: *toolsPlane.Config(),
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
func (s *Server) serveXkeysmith(w http.ResponseWriter, r *http.Request, from provider.Protocol, p provider.Provider, model string, body []byte, call *Call) (int, string) {
	settings := s.xk()
	if settings == nil {
		msg := "xkeysmith ladder unavailable"
		return writeError(w, from, 502, msg), msg
	}
	r, state, cancel := xkRequest(r, settings.limits)
	defer cancel()
	env, err := xkEnvelope(from, body)
	if err != nil {
		msg := "xkeysmith: bad request body: " + err.Error()
		return writeError(w, from, 400, msg), msg
	}
	stream := streamOf(body) || from == provider.Gemini && strings.Contains(r.URL.Path, "streamGenerateContent")
	sessionKey := xkSessionKey(r, env, model)
	// 入口人格在工具说明之前注入:它是基础框架,工具说明叠在上面。
	// L0 起就随请求上行——不等第一次被拒才由 L1 补上下文。
	if persona := xkPersonaInjection(); persona != "" {
		env.AppendSystemText(persona)
	}
	unlock := tools.UnlockSet{}
	if settings.tools != nil {
		names, patterns := settings.sessions.UnlockSet(sessionKey)
		unlock = tools.UnlockSet{Skills: names, Patterns: patterns}
	}
	var scoped *tools.RequestPlane
	if settings.tools != nil {
		scoped = settings.tools.ForRequest(unlock)
		env = xkInjectTools(env, scoped)
	}
	cfg := &attach.LadderConfig{
		Pipeline: settings.pipeline, Detector: settings.detector, Limits: settings.limits,
		PipelineCfg: settings.pipeCfg, EntryLayer: govern.L0, MaxLayer: govern.L4,
		Proto: xkProto(from), SessionKey: sessionKey, Sessions: settings.sessions,
		Budget: state.budget, Context: r.Context(),
	}
	var headers http.Header
	var last attach.ProbeResult
	forward := func(probe []byte) attach.ProbeResult {
		var native Call
		last, headers = s.xkNativeProbe(r, from, p, model, probe, &native)
		call.To = native.To
		xkSumUsage(&call.Usage, native.Usage)
		return last
	}
	result, err := attach.Run(cfg, env, forward, xkObservation)
	if err != nil {
		msg := "xkeysmith ladder: " + err.Error()
		return writeError(w, from, 502, msg), msg
	}
	result.Trace = state.trace
	defer func() { s.xkRecord(settings, r, p, model, sessionKey, from, stream, result) }()
	xkAttachTraceHeaders(w, result)
	if !result.Accepted {
		if result.StopReason == govern.StopUpstreamError {
			xkCopyHeaders(w, headers)
			if result.Probe.Err != nil {
				msg := result.Probe.Err.Error()
				return writeError(w, from, 502, msg), msg
			}
			return xkWriteProbe(w, result.Attempt, result.Probe, from)
		}
		return xkStop(w, from, state, result.StopReason, "xkeysmith exhausted its rewrite ladder")
	}
	if from != provider.Gemini && scoped != nil && scoped.IsActive() {
		reply, err := xkToolBody(from, result.Probe)
		if err != nil {
			return writeError(w, from, 502, err.Error()), err.Error()
		}
		calls := tools.ExtractToolCalls(xkToolDialect(from), reply)
		gatewayCalls := false
		for _, c := range calls {
			gatewayCalls = gatewayCalls || tools.IsGatewayTool(c.Name)
		}
		if gatewayCalls {
			first := true
			roundTrip := func(doc map[string]any) (map[string]any, error) {
				if first {
					first = false
					return reply, nil
				}
				envelope := rewrite.NewEnvelope(doc, env.Wire)
				envelope = xkInjectTools(envelope, scoped)
				request, err := json.Marshal(envelope.Body)
				if err != nil {
					return nil, err
				}
				began := time.Now()
				last = forward(request)
				result.Records = append(result.Records, attach.AttemptRecord{
					Layer: result.Attempt.UpTo, Decision: "tool:continue", Reason: "native reply after gateway tools",
					RequestBody: request, ResponseBody: last.Body, RequestHash: audit.ShortHash(request),
					Usage:     audit.UsageSummary{InputTokens: last.InputTokens, OutputTokens: last.OutputTokens, TotalTokens: last.InputTokens + last.OutputTokens},
					Outcome:   "tool_reply",
					LatencyMS: uint64(time.Since(began).Milliseconds()),
				})
				if last.Err != nil {
					return nil, last.Err
				}
				if last.Status >= 400 {
					return nil, fmt.Errorf("upstream returned %d: %s", last.Status, provider.APIError(last.Body, http.StatusText(last.Status)))
				}
				return xkToolBody(from, last)
			}
			final, err := attach.RunToolLoop(&attach.ToolLoopConfig{
				Plane: scoped, Dialect: xkToolDialect(from), MaxRounds: settings.toolConfig.MaxToolRounds,
				Budget: state.budget, Context: r.Context(), Tokens: func(map[string]any) uint64 { return last.InputTokens + last.OutputTokens },
				OnTool: func(round int, c tools.ToolCall, out tools.ToolResult) {
					if !out.Denied {
						state.acted.Store(true)
					}
					args, _ := json.Marshal(c.Arguments)
					result.Records = append(result.Records, attach.AttemptRecord{Layer: result.Attempt.UpTo, Decision: "tool:execute", Outcome: "tool_result", ToolCall: &audit.ToolCallRecord{
						Tool: c.Name, Round: uint32(round), ArgsHash: audit.ShortHash(args), ResultChars: len([]rune(out.Content)), IsError: out.IsError, Denied: out.Denied, DurationMS: out.DurationMS, Unlocked: out.Unlocked,
					}})
				},
			}, result.Attempt.Envelope.Body, roundTrip)
			if final != nil && len(final.Unlocked) > 0 {
				unlocked := scoped.Unlocks()
				settings.sessions.RecordUnlocks(sessionKey, &settings.pipeCfg, unlocked.Skills, unlocked.Patterns)
			}
			if err != nil {
				if errors.Is(err, context.DeadlineExceeded) {
					return xkStop(w, from, state, govern.StopWallClockExhausted, "xkeysmith tool loop exceeded its request deadline")
				}
				if errors.Is(err, context.Canceled) {
					return 499, "client gone"
				}
				if last.Status >= 400 {
					xkCopyHeaders(w, headers)
					return xkWriteProbe(w, result.Attempt, last, from)
				}
				msg := "xkeysmith tool loop: " + err.Error()
				return writeError(w, from, 502, msg), msg
			}
			if final.StopReason != "" {
				return xkStop(w, from, state, final.StopReason, "xkeysmith tool rounds exhausted")
			}
			result.Probe = last
		}
	}
	xkCopyHeaders(w, headers)
	if stream {
		if result.Probe.Stream || xkIsSSE(result.Probe) {
			return xkWriteStream(w, result, from)
		}
		return xkWriteSynthesizedStream(w, result, from)
	}
	if result.Probe.Stream || xkIsSSE(result.Probe) {
		reply, err := xkReply(from, result.Probe)
		if err != nil {
			return writeError(w, from, 502, err.Error()), err.Error()
		}
		request, _ := parse(from, body)
		result.Probe.Body = render(from, reply, request)
		result.Probe.Stream = false
	}
	return xkWriteProbe(w, result.Attempt, result.Probe, from)
}

func xkToolDialect(from provider.Protocol) tools.Dialect {
	switch from {
	case provider.Anthropic:
		return tools.DialectAnthropic
	case provider.Responses:
		return tools.DialectResponses
	default:
		return tools.DialectChat
	}
}

func xkToolBody(from provider.Protocol, probe attach.ProbeResult) (map[string]any, error) {
	payload := probe.Body
	if probe.Stream || xkIsSSE(probe) {
		reply, err := xkReply(from, probe)
		if err != nil {
			return nil, err
		}
		payload = render(from, reply, &Request{Model: reply.Model})
	}
	var doc map[string]any
	err := json.Unmarshal(payload, &doc)
	return doc, err
}

func xkCopyHeaders(w http.ResponseWriter, headers http.Header) {
	for key, values := range headers {
		switch http.CanonicalHeaderKey(key) {
		case "Content-Length", "Content-Encoding", "Transfer-Encoding":
			continue
		}
		if strings.HasPrefix(strings.ToLower(key), "x-xkeysmith-") {
			continue
		}
		w.Header()[key] = append([]string(nil), values...)
	}
}

// xkProto maps magpie's protocol to the ladder's dialect name.
func xkProto(from provider.Protocol) string {
	switch from {
	case provider.Anthropic:
		return "anthropic"
	case provider.Responses:
		return "responses"
	case provider.Gemini:
		return "gemini"
	default:
		return "chat"
	}
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
	} else if from == provider.Gemini {
		wire = rewrite.WireGemini
	}
	return rewrite.NewEnvelope(doc, wire), nil
}

// xkSessionKey derives the session key the same way xkeysmith does:
// an explicit header wins, else wire+model+first user text.
func xkSessionKey(r *http.Request, env *rewrite.Envelope, model string) string {
	for _, h := range []string{"x-magpie-session", "x-session-id", "x-xkeysmith-session", "x-conversation-id", "conversation_id"} {
		if v := strings.TrimSpace(r.Header.Get(h)); v != "" {
			return v
		}
	}
	first := ""
	if env == nil {
		return "auto:" + audit.ShortHash([]byte(model))
	}
	env.ForEachTextRef(rewrite.ScopeUserTurns, func(_ rewrite.SlotCtx, text string) {
		if first == "" {
			first = text
		}
	})
	if chars := []rune(first); len(chars) > 512 {
		first = string(chars[:512])
	}
	return "auto:" + audit.ShortHash([]byte(model+"\x1f"+first))
}

// xkObservation translates a probe response into the judge's input,
// absorbing the dialect differences (Chat / Anthropic / Responses).
func xkObservation(proto string, probe attach.ProbeResult) *detect.Observation {
	obs := &detect.Observation{Status: probe.Status}
	if probe.Err != nil {
		obs.ErrorMessage = probe.Err.Error()
		return obs
	}
	if probe.Status >= 400 {
		obs.ErrorCode = provider.ErrorType(probe.Body)
		obs.ErrorMessage = provider.APIError(probe.Body, http.StatusText(probe.Status))
		return obs
	}
	protocol := provider.Chat
	switch proto {
	case "anthropic":
		protocol = provider.Anthropic
	case "responses":
		protocol = provider.Responses
	case "gemini":
		protocol = provider.Gemini
	}
	reply, err := xkReply(protocol, probe)
	if err != nil {
		obs.ErrorMessage = err.Error()
		return obs
	}
	obs.FinishReason = reply.Stop
	obs.RefusalField = xkRefusal(protocol, probe)
	if reply.Stop == "filter" {
		obs.FinishReason = "content_filter"
	}
	for _, part := range reply.Parts {
		switch part.Kind {
		case Text:
			obs.Text += part.Text
		case ToolCall:
			obs.ToolCalls++
		}
	}
	return obs
}

// xkIsSSE reports whether a probe reply is a stream (some upstreams ignore
// stream:false).
func xkIsSSE(res attach.ProbeResult) bool {
	return strings.HasPrefix(strings.TrimSpace(string(res.Body)), "data:") ||
		strings.Contains(string(res.Body), "\ndata: ")
}

// ─────────────────────────── 能力平面装载 ───────────────────────────

// xkLoadSkills 从环境变量指定的目录加载技能(默认 ~/.magpie/xkeysmith/skills)。
// xk() 会把受限长度的技能索引注入 L1;完整正文仍通过 xk__skill_load
// 按需读取,并由工具平面的启用与解锁状态控制。
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

// xkSkillMode is deliberately explicit at the gateway boundary. `index`
// remains the compatibility default, but it is only injected when the skill
// loader is available; `full` is self-contained and works with tools off.
func xkSkillMode() skills.Mode {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("XKEYSMITH_SKILLS_MODE"))) {
	case "off", "none", "disabled":
		return skills.ModeOff
	case "full":
		return skills.ModeFull
	case "index", "":
		return skills.ModeIndex
	default:
		return skills.ModeIndex
	}
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
	servers, err := xkLoadMcpServers()
	if err != nil {
		// 不静默:一个写错的 MCP 配置此前表现成「没有任何服务器」。
		log.Printf("xkeysmith: %v", err)
	}
	return tools.BuildAsync(cfg, skillSet, servers)
}

// xkLoadMcpServers 读 MCP 服务器配置(JSON,由桌面端或用户提供)。
// xkLoadMcpServers 读 MCP 服务器配置(JSON,由桌面端或用户提供)。
//
// 解析失败如实返回错误:此前它静默返回空列表,于是一个写错的变量值或
// 少一个引号,表现成「一个 MCP 服务器都没配」,而没有任何提示。
func xkLoadMcpServers() ([]tools.McpServerCfg, error) {
	raw := os.Getenv("XKEYSMITH_MCP_SERVERS")
	if raw == "" {
		return nil, nil
	}
	var servers []tools.McpServerCfg
	if err := json.Unmarshal([]byte(raw), &servers); err != nil {
		return nil, fmt.Errorf("XKEYSMITH_MCP_SERVERS 不是合法的 JSON 数组: %w", err)
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
	return servers, nil
}

// xkInjectTools 把可见的 xk__ 工具 schema 注入请求体。
//
// 方言差异在这里吸收:Chat 走顶层 tools;Anthropic 走顶层 tools
// (input_schema 形状);Responses 走顶层 tools(扁平形状)。
// 渐进式披露:只注入**当前解锁状态下可见**的工具。
func xkInjectTools(env *rewrite.Envelope, plane *tools.RequestPlane) *rewrite.Envelope {
	if env.Wire == rewrite.WireGemini || plane == nil || !plane.IsActive() {
		return env
	}
	visible := plane.VisibleDefs()
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
	var kept []any
	for _, entry := range list {
		item, _ := entry.(map[string]any)
		name, _ := item["name"].(string)
		if function, ok := item["function"].(map[string]any); ok {
			name, _ = function["name"].(string)
		}
		if !tools.IsGatewayTool(name) {
			kept = append(kept, entry)
		}
	}
	return append(kept, add...)
}
