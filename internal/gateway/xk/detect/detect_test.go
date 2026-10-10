package detect

import "testing"

// 单元形状测试,与 xp-detect/src/detector.rs 的 tests 模块一一对应。

func TestStructuredRefusalFieldIsRefused(t *testing.T) {
	v := DefaultOrPanic().Judge(&Observation{Status: 200,
		RefusalField: "I can't help with that.", Text: ""})
	if v.Outcome != Refused || v.Confidence < 0.9 || !v.Escalate {
		t.Fatalf("got %s conf=%.2f escalate=%v", v.Outcome, v.Confidence, v.Escalate)
	}
}

func TestContentFilterFinishReasonIsRefused(t *testing.T) {
	v := DefaultOrPanic().Judge(&Observation{Status: 200, FinishReason: "content_filter"})
	if v.Outcome != Refused || !v.HasStructuredEvidence() {
		t.Fatalf("got %s", v.Outcome)
	}
}

func TestEnglishRefusalPrefixIsRefused(t *testing.T) {
	v := DefaultOrPanic().JudgeText(200, "I can't help with that request.")
	if v.Outcome != Refused {
		t.Fatal(v.Reason)
	}
	for _, s := range v.Signals {
		if s.Family == FamPrefix {
			return
		}
	}
	t.Fatal("no prefix signal")
}

func TestChineseRefusalPrefixIsRefused(t *testing.T) {
	v := DefaultOrPanic().JudgeText(200, "抱歉，我无法协助这个请求。")
	if v.Outcome != Refused {
		t.Fatal(v.Reason)
	}
}

func TestNormalAnswerWithCodeIsPassed(t *testing.T) {
	v := DefaultOrPanic().JudgeText(200, "Here's the plan.\n```rust\nfn main() {}\n```\nStep 1: do the thing.")
	if v.Outcome != Passed || v.Escalate || !v.Acceptable() {
		t.Fatalf("got %s escalate=%v acceptable=%v", v.Outcome, v.Escalate, v.Acceptable())
	}
}

func TestShortLegitimateAnswerIsNotEscalated(t *testing.T) {
	// 行为信号单独不足以过线:短答案不能被误判成拒绝。
	v := DefaultOrPanic().JudgeText(200, "42")
	if v.Outcome != Passed {
		t.Fatal(v.Reason)
	}
	found := false
	for _, f := range v.Families {
		if f == FamBehavioral {
			found = true
		}
	}
	if !found {
		t.Fatal("behavioral family expected")
	}
}

func TestEmptyCompletionIsRefused(t *testing.T) {
	if v := DefaultOrPanic().JudgeText(200, ""); v.Outcome != Refused {
		t.Fatal(v.Reason)
	}
}

func TestTransportPolicyErrorIsRefused(t *testing.T) {
	v := DefaultOrPanic().Judge(&Observation{Status: 400,
		ErrorCode:    "content_policy_violation",
		ErrorMessage: "Your request was rejected as a result of our safety system."})
	if v.Outcome != Refused {
		t.Fatal(v.Reason)
	}
	found := false
	for _, f := range v.Families {
		if f == FamTransport {
			found = true
		}
	}
	if !found {
		t.Fatal("transport family expected")
	}
}

func TestUpstream500IsNotRefusalButNotAcceptable(t *testing.T) {
	v := DefaultOrPanic().Judge(&Observation{Status: 502, ErrorMessage: "bad gateway"})
	if v.Outcome != Passed || v.UpstreamOK {
		t.Fatalf("got %s upstream_ok=%v", v.Outcome, v.UpstreamOK)
	}
	if v.Acceptable() {
		t.Fatal("上游故障不能被当成层成功")
	}
}

func TestAcceptEvidenceSuppressesLexicalSignal(t *testing.T) {
	// 模型说了 "I cannot",但同时交付了代码 → 应当压到阈值以下。
	v := DefaultOrPanic().JudgeText(200, "I cannot promise it compiles, but here it is:\n```rust\nfn f() {}\n```")
	if v.Outcome != Passed {
		t.Fatalf("got %s via %v", v.Outcome, v.RuleIDs())
	}
}

func TestAcceptEvidenceCannotSuppressStructuredEvidence(t *testing.T) {
	v := DefaultOrPanic().Judge(&Observation{Status: 200, FinishReason: "content_filter",
		Text: "```rust\nfn f() {}\n```"})
	if v.Outcome != Refused {
		t.Fatal("结构化证据必须留地板")
	}
}

func TestUncertainBandEscalatesWhenConfigured(t *testing.T) {
	strict := DefaultConfig()
	strict.Patterns = []Pattern{pat("mid", `(?i)hedge`, 0.40)}
	strict.AcceptPatterns = nil
	d, err := New(&strict)
	if err != nil {
		t.Fatal(err)
	}
	v := d.JudgeText(200, "hedge")
	if v.Outcome != Uncertain || !v.Escalate {
		t.Fatalf("got %s escalate=%v", v.Outcome, v.Escalate)
	}

	lax := strict
	lax.UncertainEscalates = false
	d2, _ := New(&lax)
	if v2 := d2.JudgeText(200, "hedge"); v2.Escalate {
		t.Fatal("uncertain must not escalate when configured off")
	}
}

func TestPrefixWindowLimitsFalsePositives(t *testing.T) {
	// 拒绝句出现在很靠后的位置(引用场景)不应触发前缀规则。
	body := stringsRepeat("A", 600) + " I can't help with that."
	if v := DefaultOrPanic().JudgeText(200, body); v.Outcome != Passed {
		t.Fatalf("got %s via %v", v.Outcome, v.RuleIDs())
	}
}

func TestPrefixWindowDoesNotSplitUTF8(t *testing.T) {
	// 窗口边界落在多字节字符中间也不应出问题。
	body := stringsRepeat("中", 399) + "我无法协助"
	_ = DefaultOrPanic().JudgeText(200, body) // must not panic
}

func TestReasoningOnlyResponseIsRefused(t *testing.T) {
	v := DefaultOrPanic().Judge(&Observation{Status: 200, ReasoningOnly: true})
	if v.Outcome != Refused {
		t.Fatal(v.Reason)
	}
}

func TestToolCallResponseIsNotPenalizedAsEmpty(t *testing.T) {
	v := DefaultOrPanic().Judge(&Observation{Status: 200, ToolCalls: 2, FinishReason: "tool_calls"})
	if v.Outcome != Passed || !v.Acceptable() {
		t.Fatalf("got %s", v.Outcome)
	}
}

func stringsRepeat(s string, n int) string {
	out := make([]byte, 0, len(s)*n)
	for i := 0; i < n; i++ {
		out = append(out, s...)
	}
	return string(out)
}
