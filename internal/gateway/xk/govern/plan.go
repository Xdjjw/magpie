package govern

import (
	"github.com/yetone/magpie/internal/gateway/xk/detect"
)

// DecisionKind 是决策的三种形状。
type DecisionKind string

const (
	DecisionAccept   DecisionKind = "accept"
	DecisionEscalate DecisionKind = "escalate"
	DecisionStop     DecisionKind = "stop"
)

// Decision:采纳 / 升级到某层 / 停止(带原因)。
type Decision struct {
	Kind  DecisionKind
	Layer LayerID    // escalate 时有效
	Stop  StopReason // stop 时有效
}

// Label 是稳定的短标签,直接进审计与解释表。
func (d Decision) Label() string {
	switch d.Kind {
	case DecisionAccept:
		return "accept"
	case DecisionEscalate:
		return "escalate:" + d.Layer.Label()
	default:
		return "stop:" + string(d.Stop)
	}
}

// MarshalJSON 与 Rust 的 `#[serde(tag="kind", content="layer")]` 形状一致:
// {"kind":"escalate","layer":"L3"} / {"kind":"stop","layer":"calls_exhausted"}。
func (d Decision) MarshalJSON() ([]byte, error) {
	switch d.Kind {
	case DecisionAccept:
		return []byte(`{"kind":"accept"}`), nil
	case DecisionEscalate:
		return []byte(`{"kind":"escalate","layer":"` + d.Layer.Label() + `"}`), nil
	default:
		return []byte(`{"kind":"stop","layer":"` + string(d.Stop) + `"}`), nil
	}
}

// Plan 是一次决策及其理由(理由直接进审计)。
type Plan struct {
	Decision Decision
	Reason   string
}

// PlanInput 刻意只给「决策需要知道的东西」。
type PlanInput struct {
	Verdict       *detect.Verdict
	CurrentLayer  LayerID
	MaxLayer      LayerID
	EnabledLayers []LayerID
	Budget        *Budget
	// 命中结构化/传输层证据时跳过 L2(词面层)。
	SkipLexicalOnStructured bool
}

// Plan 是决策入口:纯函数,升级策略可以被单测穷举——
// 这是「递进是决策而不是循环」的落点。
func Plan_(input *PlanInput) Plan {
	v := input.Verdict

	// 1) 判定通过且上游本身没报错 → 采纳。
	if v.Acceptable() {
		return Plan{Decision{Kind: DecisionAccept}, "verdict passed with healthy upstream"}
	}

	// 2) 上游故障(5xx / 连接问题)不是拒绝:升级重试毫无意义,原样透传。
	// 注意:4xx 策略错误(400 + content_policy_violation)若判定器明确要升级,
	// 则继续升写,不能当作传输故障直接透传。
	if !v.UpstreamOK && !v.Escalate {
		return Plan{Decision{Kind: DecisionStop, Stop: StopUpstreamError},
			"upstream transport failure, not a refusal"}
	}

	// 3) 判定不确定但策略说「不升级」→ 采纳。
	if !v.Escalate {
		return Plan{Decision{Kind: DecisionAccept},
			"uncertain verdict, escalation disabled by policy"}
	}

	// 4) 预算。
	if reason, ok := input.Budget.Check(); !ok {
		return Plan{Decision{Kind: DecisionStop, Stop: reason}, "budget exhausted"}
	}

	// 5) 找下一层。
	if next, ok := nextLayer(input, v); ok {
		// 只有真的「跳过了一层」才这么写理由——审计里的理由必须可核对。
		expectedNext, hasNext := input.CurrentLayer.Next()
		skipped := hasNext && expectedNext != next
		reason := "escalating to next rewrite layer"
		if skipped {
			reason = "structured evidence: skipping lexical layer"
		}
		return Plan{Decision{Kind: DecisionEscalate, Layer: next}, reason}
	}
	return Plan{Decision{Kind: DecisionStop, Stop: StopLayersExhausted},
		"no further layer available"}
}

func nextLayer(input *PlanInput, v *detect.Verdict) (LayerID, bool) {
	candidate, ok := input.CurrentLayer.Next()
	if !ok {
		return 0, false
	}
	for {
		if candidate > input.MaxLayer {
			return 0, false
		}
		if !containsLayer(input.EnabledLayers, candidate) {
			if candidate, ok = candidate.Next(); !ok {
				return 0, false
			}
			continue
		}
		if input.SkipLexicalOnStructured && candidate == L2 && v.HasStructuredEvidence() {
			if candidate, ok = candidate.Next(); !ok {
				return 0, false
			}
			continue
		}
		return candidate, true
	}
}

func containsLayer(layers []LayerID, l LayerID) bool {
	for _, x := range layers {
		if x == l {
			return true
		}
	}
	return false
}
