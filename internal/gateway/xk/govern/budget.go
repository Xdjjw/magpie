package govern

import (
	"time"
)

// StopReason 是提前终止的原因。是否属于「正常耗尽」决定返回给客户端的
// 错误码(耗尽给 503 exhausted,上游故障透传原响应)。
type StopReason string

const (
	StopCallsExhausted       StopReason = "calls_exhausted"
	StopTokenBudgetExhausted StopReason = "token_budget_exhausted"
	StopWallClockExhausted   StopReason = "wall_clock_exhausted"
	StopLayersExhausted      StopReason = "layers_exhausted"
	StopUpstreamError        StopReason = "upstream_error"
	StopClientGone           StopReason = "client_gone"
)

func (s StopReason) IsExhaustion() bool {
	switch s {
	case StopCallsExhausted, StopTokenBudgetExhausted, StopWallClockExhausted, StopLayersExhausted:
		return true
	}
	return false
}

// Limits 与 xp-core GovernCfg 对齐。
type Limits struct {
	MaxUpstreamCalls        uint32
	TokenBudget             uint64
	WallClockMS             uint64
	SessionTTLSeconds       uint64
	MaxSessions             int
	MaxConcurrentRequests   int
	SkipLexicalOnStructured bool
}

func DefaultLimits() Limits {
	return Limits{
		MaxUpstreamCalls:        6,
		TokenBudget:             200_000,
		WallClockMS:             120_000,
		SessionTTLSeconds:       3_600,
		MaxSessions:             4_096,
		MaxConcurrentRequests:   64,
		SkipLexicalOnStructured: true,
	}
}

// Budget 是单次客户端请求的预算计数器(引擎内单协程使用,不加锁,
// 与 Rust 版一致)。
type Budget struct {
	limits     Limits
	started    time.Time
	callsUsed  uint32
	tokensUsed uint64
	toolRounds uint32
}

func NewBudget(l Limits) *Budget {
	return &Budget{limits: l, started: time.Now()}
}

func (b *Budget) Limits() *Limits        { return &b.limits }
func (b *Budget) CallsUsed() uint32      { return b.callsUsed }
func (b *Budget) TokensUsed() uint64     { return b.tokensUsed }
func (b *Budget) ToolRoundsUsed() uint32 { return b.toolRounds }

func (b *Budget) RemainingCalls() uint32 {
	if b.callsUsed >= b.limits.MaxUpstreamCalls {
		return 0
	}
	return b.limits.MaxUpstreamCalls - b.callsUsed
}

func (b *Budget) ElapsedMS() uint64 {
	return uint64(time.Since(b.started).Milliseconds())
}

func (b *Budget) RemainingMS() uint64 {
	e := b.ElapsedMS()
	if e >= b.limits.WallClockMS {
		return 0
	}
	return b.limits.WallClockMS - e
}

// NoteCall 记一次上游调用;可变参数带本次 total_tokens(与 Rust 的
// Option<u64> 对应:不传=没有用量信息)。
func (b *Budget) NoteCall(totalTokens ...uint64) {
	b.callsUsed++
	for _, t := range totalTokens {
		b.tokensUsed += t
	}
}

// NoteToolRound 记一次**工具轮**。
//
// 刻意不占用 MaxUpstreamCalls:那个上限描述「改写阶梯能爬几层」,
// 工具轮是另一条轴(由 tools.max_tool_rounds 管),两者混在一起会让
// 「满层阶梯 + 工具轮」直接没预算可用。
func (b *Budget) NoteToolRound(totalTokens ...uint64) {
	b.toolRounds++
	for _, t := range totalTokens {
		b.tokensUsed += t
	}
}

// CheckResources 是工具轮的资源闸门:只看 token 与墙钟,不看阶梯调用数。
func (b *Budget) CheckResources() (StopReason, bool) {
	if b.tokensUsed >= b.limits.TokenBudget {
		return StopTokenBudgetExhausted, false
	}
	if b.ElapsedMS() >= b.limits.WallClockMS {
		return StopWallClockExhausted, false
	}
	return "", true
}

// Check 报告还能不能再发一次上游调用。
func (b *Budget) Check() (StopReason, bool) {
	if b.callsUsed >= b.limits.MaxUpstreamCalls {
		return StopCallsExhausted, false
	}
	if reason, ok := b.CheckResources(); !ok {
		return reason, false
	}
	return "", true
}

// SimulateElapsed 把起始时间往前挪,用于测试与本地回放。
func (b *Budget) SimulateElapsed(ms uint64) {
	b.started = time.Now().Add(-time.Duration(ms) * time.Millisecond)
}

// BudgetSnapshot 是进审计的预算快照。
type BudgetSnapshot struct {
	CallsUsed      uint32 `json:"calls_used"`
	MaxCalls       uint32 `json:"max_calls"`
	TokensUsed     uint64 `json:"tokens_used"`
	TokenBudget    uint64 `json:"token_budget"`
	ElapsedMS      uint64 `json:"elapsed_ms"`
	WallClockMS    uint64 `json:"wall_clock_ms"`
	ToolRoundsUsed uint32 `json:"tool_rounds_used"`
}

func (b *Budget) Snapshot() BudgetSnapshot {
	return BudgetSnapshot{
		CallsUsed:      b.callsUsed,
		MaxCalls:       b.limits.MaxUpstreamCalls,
		TokensUsed:     b.tokensUsed,
		TokenBudget:    b.limits.TokenBudget,
		ElapsedMS:      b.ElapsedMS(),
		WallClockMS:    b.limits.WallClockMS,
		ToolRoundsUsed: b.toolRounds,
	}
}
