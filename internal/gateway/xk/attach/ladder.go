// Package attach is stage ③: the escalation loop and response-side
// restoration that make the semantic plane act on real traffic.
//
// 设计要点(与 xkeysmith 的引擎一致):
//   - **递进是决策,不是循环**:每一轮问 govern.Plan 该采纳 / 升层 / 停止;
//   - 每层保留原生请求的 stream 设置;缓冲判定后释放被采纳的响应,
//     不另发一次请求重放;
//   - **L4 还原在响应侧**:非流式整体还原;有占位符的流式响应由网关解码、
//     整体还原后重新编码,避免占位符跨 SSE 事件边界;
//   - **审计每 attempt 一条**,包括被跳过层与失败层的自述。
//
// 本包不依赖 magpie 内部:上游调用以 Forward 函数注入,因此整条阶梯
// 可以用假上游穷举测试(与 Rust 版把判定与传输解耦是同一个理由)。
package attach

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/yetone/magpie/internal/gateway/xk/audit"
	"github.com/yetone/magpie/internal/gateway/xk/detect"
	"github.com/yetone/magpie/internal/gateway/xk/govern"
	"github.com/yetone/magpie/internal/gateway/xk/rewrite"
)

// ProbeResult 是一次上游探测的原始观察(传输层事实,与判定解耦)。
type ProbeResult struct {
	Status       int
	Body         []byte
	Err          error
	InputTokens  uint64
	OutputTokens uint64
	Stream       bool
}

// Forward 发一次原生调用并缓冲响应。body 是已经改写好的一层请求体。
type Forward func(body []byte) ProbeResult

// Observation 把一次探测的原始响应翻译成判定器的输入。
//
// 方言差异都在这里吸收:OpenAI Chat 看 choices[].finish_reason / message.refusal,
// Anthropic 看 stop_reason,Responses 看 status/error;正文取文本槽拼接。
type Observation func(proto string, res ProbeResult) *detect.Observation

// LadderConfig 是一次阶梯运行的输入。
type LadderConfig struct {
	Pipeline    *rewrite.Pipeline
	Detector    *detect.Detector
	Limits      govern.Limits
	PipelineCfg govern.PipelineConfig
	EntryLayer  govern.LayerID
	MaxLayer    govern.LayerID
	Proto       string
	// SessionMemoryKey 非空时启用会话记忆(「上次成功的层」直接起步)。
	SessionKey string
	Sessions   *govern.SessionRegistry
	Budget     *govern.Budget
	Context    context.Context
}

// AttemptRecord 是一条审计记录(与 audit.TraceRecord 的字段对应)。
type AttemptRecord struct {
	Layer         govern.LayerID
	Decision      string
	Reason        string
	Verdict       audit.VerdictSummary
	Applied       []govern.LayerID
	Rules         []string
	Notes         []string
	RequestHash   string
	MaskedTargets []audit.MaskedTarget
	LatencyMS     uint64
	Outcome       string
	StopReason    string
	RequestBody   []byte
	ResponseBody  []byte
	Usage         audit.UsageSummary
	ToolCall      *audit.ToolCallRecord
}

// LadderResult 是整条阶梯的结局。
type LadderResult struct {
	// AcceptedLayer 是采纳的层;LayerNone 表示全层耗尽/停止。
	AcceptedLayer govern.LayerID
	Accepted      bool
	Attempt       *rewrite.Attempt
	Probe         ProbeResult
	Records       []AttemptRecord
	StopReason    govern.StopReason
	// Reason 是给用户/审计的一句话结论。
	Reason string
	Trace  audit.TraceID
}

// LayerNone 表示没有采纳任何层。
const LayerNone govern.LayerID = 255

// Run 跑完整条阶梯。
//
// 返回的 Attempt 是**被采纳的那一层**的改写结果(调用方用它做 L4 还原);
// 未采纳时返回最后一层的改写结果,以便如实回报。
func Run(cfg *LadderConfig, pristine *rewrite.Envelope, forward Forward, observe Observation) (*LadderResult, error) {
	budget := cfg.Budget
	if budget == nil {
		budget = govern.NewBudget(cfg.Limits)
	}
	entry := cfg.EntryLayer
	if cfg.Sessions != nil && cfg.SessionKey != "" {
		entry = cfg.Sessions.EntryLayer(cfg.SessionKey, &cfg.PipelineCfg, cfg.EntryLayer)
	}
	layer := entry
	var records []AttemptRecord
	escalated := false

	for {
		attempt, err := cfg.Pipeline.Attempt(pristine, layer)
		if err != nil {
			return nil, fmt.Errorf("rewrite at %s: %w", layer.Label(), err)
		}

		if cfg.Context != nil && cfg.Context.Err() != nil {
			reason := stopReasonForContext(cfg.Context.Err())
			return &LadderResult{Attempt: attempt, Records: records, StopReason: reason, Reason: cfg.Context.Err().Error()}, nil
		}
		if reason, ok := budget.Check(); !ok {
			return &LadderResult{Attempt: attempt, Records: records, StopReason: reason, Reason: string(reason)}, nil
		}
		request := mustJSON(attempt.Envelope.Body)
		started := time.Now()
		res := forward(request)
		budget.NoteCall(res.InputTokens + res.OutputTokens)
		if cfg.Context != nil && cfg.Context.Err() != nil {
			reason := stopReasonForContext(cfg.Context.Err())
			records = append(records, AttemptRecord{
				Layer: layer, Decision: "stop:" + string(reason), Reason: cfg.Context.Err().Error(),
				Applied: attempt.Applied(), RequestHash: attempt.RequestHash(),
				Outcome: string(reason), StopReason: string(reason),
				RequestBody: request, ResponseBody: res.Body,
				Usage:     audit.UsageSummary{InputTokens: res.InputTokens, OutputTokens: res.OutputTokens, TotalTokens: res.InputTokens + res.OutputTokens},
				LatencyMS: uint64(time.Since(started).Milliseconds()),
			})
			return &LadderResult{
				Attempt: attempt, Probe: res, Records: records,
				StopReason: reason, Reason: cfg.Context.Err().Error(),
			}, nil
		}

		obs := observe(cfg.Proto, res)
		verdict := cfg.Detector.Judge(obs)

		plan := govern.Plan_(&govern.PlanInput{
			Verdict:                 &verdict,
			CurrentLayer:            layer,
			MaxLayer:                cfg.MaxLayer,
			EnabledLayers:           cfg.PipelineCfg.EnabledLayers,
			Budget:                  budget,
			SkipLexicalOnStructured: cfg.Limits.SkipLexicalOnStructured,
		})

		rec := AttemptRecord{
			Layer:         layer,
			Decision:      plan.Decision.Label(),
			Reason:        plan.Reason,
			Verdict:       audit.Summarize(&verdict),
			Applied:       attempt.Applied(),
			RequestHash:   attempt.RequestHash(),
			MaskedTargets: maskedTargets(attempt),
			Outcome:       outcomeOf(verdict, plan.Decision),
			RequestBody:   request, ResponseBody: res.Body,
			Usage:     audit.UsageSummary{InputTokens: res.InputTokens, OutputTokens: res.OutputTokens, TotalTokens: res.InputTokens + res.OutputTokens},
			LatencyMS: uint64(time.Since(started).Milliseconds()),
		}
		for _, r := range attempt.Reports {
			rec.Rules = append(rec.Rules, r.Rules...)
			rec.Notes = append(rec.Notes, r.Notes...)
		}
		if plan.Decision.Kind == govern.DecisionStop {
			rec.StopReason = string(plan.Decision.Stop)
		}
		records = append(records, rec)

		switch plan.Decision.Kind {
		case govern.DecisionAccept:
			if cfg.Sessions != nil && cfg.SessionKey != "" {
				l := layer
				cfg.Sessions.Record(cfg.SessionKey, &cfg.PipelineCfg, &l, escalated)
			}
			return &LadderResult{
				AcceptedLayer: layer, Accepted: true, Attempt: attempt,
				Probe: res, Records: records,
				Reason: fmt.Sprintf("accepted at %s (%s)", layer.Label(), plan.Reason),
			}, nil
		case govern.DecisionStop:
			if cfg.Sessions != nil && cfg.SessionKey != "" {
				cfg.Sessions.Record(cfg.SessionKey, &cfg.PipelineCfg, nil, escalated)
			}
			return &LadderResult{
				Attempt: attempt, Probe: res, Records: records,
				StopReason: plan.Decision.Stop,
				Reason:     fmt.Sprintf("stopped at %s: %s", layer.Label(), plan.Reason),
			}, nil
		default: // escalate
			escalated = true
			layer = plan.Decision.Layer
		}
	}
}

func stopReasonForContext(err error) govern.StopReason {
	if err == context.DeadlineExceeded {
		return govern.StopWallClockExhausted
	}
	return govern.StopClientGone
}

func outcomeOf(v detect.Verdict, d govern.Decision) string {
	switch d.Kind {
	case govern.DecisionAccept:
		return "accepted"
	case govern.DecisionStop:
		if d.Stop == govern.StopUpstreamError {
			return "upstream_error"
		}
		return "exhausted"
	default:
		if v.Outcome == detect.Refused {
			return "refused"
		}
		return "uncertain"
	}
}

func maskedTargets(a *rewrite.Attempt) []audit.MaskedTarget {
	if a.Vault == nil {
		return nil
	}
	out := make([]audit.MaskedTarget, 0, a.Vault.Len())
	for _, e := range a.Vault.Entries() {
		out = append(out, audit.MaskedTarget{
			Kind: string(e.Kind), Placeholder: e.Placeholder, Original: e.Original,
		})
	}
	return out
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}

// ─────────────────────────── 响应侧还原 ───────────────────────────

// RestoreJSON 非流式:整体还原响应体里的所有字符串
// (包括 tool 参数——模型可能把抽象化的目标放进工具调用里)。
func RestoreJSON(attempt *rewrite.Attempt, body []byte) []byte {
	if attempt == nil || attempt.Vault == nil || attempt.Vault.IsEmpty() {
		return body
	}
	var doc any
	if err := json.Unmarshal(body, &doc); err != nil {
		// 不是 JSON(纯文本/SSE 片段):退化为全文替换。
		return []byte(attempt.Restore(string(body)))
	}
	restored := attempt.Vault.RestoreValue(doc)
	out, err := json.Marshal(restored)
	if err != nil {
		return body
	}
	return out
}

// RestoringWriter 流式:包一层 http.ResponseWriter,对 SSE 的 data 载荷
// 做增量还原。占位符可能被切在 chunk 边界上,交给 StreamRestorer 的
// 滑动窗口处理,绝不下发半个占位符。
type RestoringWriter struct {
	restorer *rewrite.StreamRestorer
	buf      bytes.Buffer
}

// NewRestoringWriter 为一个 attempt 构造流式还原器。
func NewRestoringWriter(attempt *rewrite.Attempt) *RestoringWriter {
	if attempt == nil || attempt.Vault == nil || attempt.Vault.IsEmpty() {
		return &RestoringWriter{}
	}
	return &RestoringWriter{restorer: attempt.Restorer()}
}

// sseDataPrefix 是 SSE 数据行前缀。
const sseDataPrefix = "data: "

// Push 处理一个响应 chunk:按行切分,对 data 载荷做还原。
// 返回应当下发的字节。
func (w *RestoringWriter) Push(chunk []byte) []byte {
	if w.restorer == nil || !w.restorer.HasMapping() {
		return chunk
	}
	w.buf.Write(chunk)
	var out bytes.Buffer
	for {
		line, err := w.buf.ReadString('\n')
		if err != nil {
			// 不完整的一行:留在缓冲里等下一个 chunk(避免切断占位符)。
			w.buf.WriteString(line)
			break
		}
		out.WriteString(w.rewriteLine(line))
	}
	return out.Bytes()
}

// Finish 流结束:把缓冲里最后一行与还原器的残留一起放出。
func (w *RestoringWriter) Finish() []byte {
	var out bytes.Buffer
	if w.buf.Len() > 0 {
		out.WriteString(w.rewriteLine(w.buf.String()))
		w.buf.Reset()
	}
	if w.restorer != nil {
		out.WriteString(w.restorer.Finish())
	}
	return out.Bytes()
}

func (w *RestoringWriter) rewriteLine(line string) string {
	if w.restorer == nil || !w.restorer.HasMapping() {
		return line
	}
	if !strings.HasPrefix(line, sseDataPrefix) {
		return line
	}
	payload := strings.TrimSuffix(line[len(sseDataPrefix):], "\n")
	if payload == "[DONE]" {
		return line
	}
	restored := w.restorer.Push(payload)
	return sseDataPrefix + restored + "\n"
}
