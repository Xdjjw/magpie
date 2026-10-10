package audit

import (
	"encoding/hex"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/yetone/magpie/internal/gateway/xk/govern"
)

// Wire 与 xp-core 的 WireFormat 线型一致("openai" / "anthropic")。
type Wire string

const (
	WireOpenAi    Wire = "openai"
	WireAnthropic Wire = "anthropic"
)

// TraceID 是请求级关联 id(UUID v7,时间有序)。
type TraceID uuid.UUID

func NewTraceID() TraceID { return TraceID(uuid.Must(uuid.NewV7())) }

// Short 返回 12 位短 id(audit explain 的输入)。
func (t TraceID) Short() string {
	return hex.EncodeToString(t[:])[:12]
}

func (t TraceID) String() string { return hex.EncodeToString(t[:]) }

func (t *TraceID) UnmarshalText(b []byte) error {
	id, err := uuid.ParseBytes(b)
	if err != nil {
		return err
	}
	*t = TraceID(id)
	return nil
}

func (t TraceID) MarshalText() ([]byte, error) { return []byte(t.String()), nil }

// VerdictSummary 是判定结果的审计投影(字段名与 Rust 线型一致)。
type VerdictSummary struct {
	Outcome    string   `json:"outcome"`
	Confidence float64  `json:"confidence"`
	Escalate   bool     `json:"escalate"`
	UpstreamOK bool     `json:"upstream_ok"`
	Families   []string `json:"families"`
	Signals    []string `json:"signals"`
	Reason     string   `json:"reason"`
}

// UsageSummary 是本次尝试的 token 用量。
type UsageSummary struct {
	InputTokens  uint64 `json:"input_tokens"`
	OutputTokens uint64 `json:"output_tokens"`
	TotalTokens  uint64 `json:"total_tokens"`
}

// MaskedTarget 记录一次 L4 抽象化:占位符 ↔ 原值(还原审计用)。
type MaskedTarget struct {
	Kind        string `json:"kind"`
	Placeholder string `json:"placeholder"`
	Original    string `json:"original"`
}

// ToolCallRecord 是一次**网关侧工具调用**的记录。与上游调用分开建模,
// 但共用同一份 JSONL:审计的第一原则是「按发生顺序排列」,把工具调用
// 插到同一时间线上才能看清因果。
type ToolCallRecord struct {
	Tool          string   `json:"tool"`
	Round         uint32   `json:"round"`
	ArgsHash      string   `json:"args_hash"`
	ArgsPreview   *string  `json:"args_preview"`
	ResultChars   int      `json:"result_chars"`
	ResultPreview *string  `json:"result_preview"`
	IsError       bool     `json:"is_error"`
	Denied        bool     `json:"denied"`
	DurationMS    uint64   `json:"duration_ms"`
	Unlocked      []string `json:"unlocked"`
	UnlockedBy    *string  `json:"unlocked_by"`
}

// TraceRecord 是一次尝试的完整记录——整个系统的「黑匣子」。
// 所有引用都用稳定 id(层号、规则 id、占位符),不用正则原文或内存地址。
type TraceRecord struct {
	TS      time.Time `json:"ts"`
	Trace   TraceID   `json:"trace"`
	Session string    `json:"session"`
	// 本次客户端请求内的尝试序号(从 0 开始)。
	Seq   uint32         `json:"seq"`
	Layer govern.LayerID `json:"layer"`
	// 累加到的层(与 Layer 相同,保留字段是为了将来支持「跳层」)。
	Attempt  uint8  `json:"attempt"`
	Wire     Wire   `json:"wire"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Stream   bool   `json:"stream"`

	Decision       string  `json:"decision"`
	StopReason     *string `json:"stop_reason"`
	DecisionReason *string `json:"decision_reason"`

	Verdict       VerdictSummary   `json:"verdict"`
	AppliedLayers []govern.LayerID `json:"applied_layers"`
	Rules         []string         `json:"rules"`
	// Notes 是改写层的**自述**(LayerReport::notes)。与 Rules 分开:
	// rules 是「命中了哪条规则」(稳定 id,可聚合),notes 是「这一层自己
	// 说它做了什么 / 为什么没做」。后者的价值在**静默失败**:例如
	// 「system 槽位不可写,跳过指令注入」——没有它,注入被跳过时审计里
	// 一个字都看不到。
	Notes []string `json:"notes,omitempty"`

	RequestHash   string `json:"request_hash"`
	ResponseHash  string `json:"response_hash"`
	RequestBytes  int    `json:"request_bytes"`
	ResponseBytes int    `json:"response_bytes"`

	// 脱敏后的预览(始终存在);未脱敏原文仅 RawCapture 开启时存在。
	RequestPreview  *string `json:"request_preview"`
	ResponsePreview *string `json:"response_preview"`
	RawRequest      *string `json:"raw_request"`
	RawResponse     *string `json:"raw_response"`

	MaskedTargets []MaskedTarget `json:"masked_targets"`
	Usage         UsageSummary   `json:"usage"`
	LatencyMS     uint64         `json:"latency_ms"`
	// accepted / refused / exhausted / upstream_error / invariant_violation
	Outcome string  `json:"outcome"`
	Error   *string `json:"error"`
	// 若这条记录是**工具调用**而不是上游调用,则非空。
	ToolCall *ToolCallRecord `json:"tool_call,omitempty"`
}

func (r *TraceRecord) IsAccepted() bool { return r.Outcome == "accepted" }

// SummaryLine 是单行摘要,供 tail 直接打印。
func (r *TraceRecord) SummaryLine() string {
	return fmt.Sprintf("%s %s %-3s %-9s conf=%.2f %5dms %s",
		r.TS.Format("15:04:05.000"), r.Trace.Short(), r.Layer.Label(),
		r.Outcome, r.Verdict.Confidence, r.LatencyMS, r.Verdict.Reason)
}
